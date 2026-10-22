package jobs

// FORK-IN-04, 57.6 D-22: success means a marker the engine rewrote during this
// run whose signature is the expected commit. Normalize's one-second allowance
// for r10k's whole-second marker times must not let a marker laid down by an
// earlier run, in the same second, pass as this run's work.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/puppet-stagehand/stagehand-deploy-agent/internal/jobs/fakeengine"
)

// earlyInSecond waits until the clock is in the first half of a second, so a
// marker written now and a job started a few milliseconds later share a
// second (the case the allowance exists for, and the one it gets wrong).
func earlyInSecond(t *testing.T) {
	t.Helper()
	if ns := time.Now().Nanosecond(); ns > 400_000_000 {
		time.Sleep(time.Second - time.Duration(ns)*time.Nanosecond + 20*time.Millisecond)
	}
}

// A marker for the expected commit, deploy_success true, finished this very
// second, that the engine did not touch: the run exits 0 and rewrites nothing.
func TestSameSecondStaleMarkerFails(t *testing.T) {
	fixture := "testdata/r10k-5.0.3/bad-module-ref.json" // marker_changed false, marker_before deploy_success true
	t.Setenv(fakeengine.ExitCodeEnv, "0")
	r, basedir := fakeEngine(t, "r10k", "", shaA, Options{})
	t.Setenv(fakeengine.FixtureEnv, fixture)

	earlyInSecond(t)
	if err := fakeengine.SeedBefore(fixture, basedir, shaA, time.Now()); err != nil {
		t.Fatal(err)
	}
	_, res := deployOneEnv(t, r, "production", shaA)

	if res.State != StateFailed || res.Error != "the engine did not rewrite the deploy marker" {
		t.Fatalf("result = %s %q, want failed with %q", res.State, res.Error, "the engine did not rewrite the deploy marker")
	}
	if res.DeployedSHA != "" {
		t.Fatalf("deployed_sha = %q: a marker this run did not write is never reported as what it deployed", res.DeployedSHA)
	}
}

// Two real deploys of one environment inside a second, each rewriting the
// marker (the second with the same bytes, as r10k's whole-second times make it),
// both succeed.
func TestBackToBackDeploysSameSecondSucceed(t *testing.T) {
	r, basedir := fakeEngine(t, "r10k", "testdata/r10k-5.0.3/deploy-success.json", shaA, Options{})
	earlyInSecond(t)
	for i := 1; i <= 2; i++ {
		_, res := deployOneEnv(t, r, "production", shaA)
		if res.State != StateDeployed || res.Error != "" || res.DeployedSHA != shaA {
			t.Fatalf("deploy %d = %s %q (deployed %q), want deployed", i, res.State, res.Error, res.DeployedSHA)
		}
	}
	if _, err := os.Stat(filepath.Join(basedir, "production", ".r10k-deploy.json")); err != nil {
		t.Fatal(err)
	}
}

func TestMarkerSnapshotRewritten(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".r10k-deploy.json")
	t0 := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	write := func(content string, mtime time.Time) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}

	absent := SnapshotMarker(path)
	if absent.Exists {
		t.Fatal("a missing file has a snapshot that exists")
	}
	write("a", t0)
	before := SnapshotMarker(path)
	if !before.Exists || before.ModTime.IsZero() || string(before.Bytes) != "a" {
		t.Fatalf("snapshot = %+v", before)
	}

	cases := []struct {
		name  string
		setup func()
		want  bool
	}{
		{"untouched", func() {}, false},
		{"same bytes, same mtime written again", func() { write("a", t0) }, false}, // indistinguishable from untouched
		{"same bytes, mtime one nanosecond later", func() { write("a", t0.Add(time.Nanosecond)) }, true},
		{"new bytes, same mtime", func() { write("b", t0) }, true},
		{"removed", func() { os.Remove(path) }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			write("a", t0)
			tc.setup()
			if got := before.Rewritten(SnapshotMarker(path)); got != tc.want {
				t.Fatalf("Rewritten = %v, want %v", got, tc.want)
			}
		})
	}
	// Nothing before the run: whatever is there afterwards was written by it.
	write("a", t0)
	if !absent.Rewritten(SnapshotMarker(path)) {
		t.Fatal("a marker that did not exist before the run reads as not rewritten")
	}
}
