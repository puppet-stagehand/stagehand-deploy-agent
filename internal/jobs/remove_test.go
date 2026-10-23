package jobs

// Environment removal (D-09, D-10, D-20). The runner is built with the same
// helper the deploy tests use; a removal never starts the engine, so the fake
// engine is only there to satisfy Options.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// removalRunner returns a runner with an environment directory feature_x
// (holding a file and a nested directory) under its basedir.
func removalRunner(t *testing.T, opts Options) (*Runner, string) {
	t.Helper()
	r, basedir := fakeEngine(t, "r10k", "testdata/r10k-5.0.3/deploy-success.json", shaA, opts)
	makeEnv(t, basedir, "feature_x")
	return r, basedir
}

func makeEnv(t *testing.T, basedir, name string) string {
	t.Helper()
	dir := filepath.Join(basedir, name)
	if err := os.MkdirAll(filepath.Join(dir, "modules", "apache"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "modules", "apache", "init.pp"), []byte("class apache {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRemovalDeletesTheEnvironment(t *testing.T) {
	r, basedir := removalRunner(t, Options{})
	job, err := r.NewRemoval("feature_x")
	if err != nil {
		t.Fatal(err)
	}
	if job.Kind != KindRemove || job.State != StateQueued || len(job.Results) != 1 || job.Results[0].Environment != "feature_x" {
		t.Fatalf("queued removal = %+v", job)
	}
	if job.Engine != "r10k" {
		t.Errorf("engine = %q, want the configured engine name", job.Engine)
	}
	if err := r.Start(job); err != nil {
		t.Fatal(err)
	}
	r.wg.Wait()

	got, err := r.Store().Get(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateDeployed || got.Results[0].State != StateDeployed || got.FinishedAt == nil {
		t.Fatalf("finished removal = %+v", got)
	}
	if res := got.Results[0]; res.ExpectedSHA != "" || res.DeployedSHA != "" || res.Error != "" {
		t.Errorf("a removal carries no sha and no error, got %+v", res)
	}
	if _, err := os.Lstat(filepath.Join(basedir, "feature_x")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("environment directory still there: %v", err)
	}
	left, _ := filepath.Glob(filepath.Join(basedir, ".removing-*"))
	if len(left) != 0 {
		t.Fatalf("aside directory left behind: %v", left)
	}
}

func TestRemovalOfAMissingEnvironmentIsNotFound(t *testing.T) {
	r, _ := removalRunner(t, Options{})
	if _, err := r.NewRemoval("missing"); !errors.Is(err, ErrEnvironmentNotFound) {
		t.Fatalf("err = %v, want ErrEnvironmentNotFound", err)
	}
	if list, _ := r.Store().List(10); len(list) != 0 {
		t.Fatalf("a refused removal was recorded: %+v", list)
	}
}

func TestRemovalRefusesBadNames(t *testing.T) {
	r, basedir := removalRunner(t, Options{})
	if err := os.MkdirAll(filepath.Join(basedir, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "Bad-Name", "../x", "a/b", "feature_x/..", ".", "..", "--config=/x", "prod;rm"} {
		if _, err := r.NewRemoval(name); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("NewRemoval(%q) err = %v, want ErrInvalidRequest", name, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(basedir, "x")); err != nil {
		t.Fatalf("a sibling directory was touched: %v", err)
	}
}

// Only removal records carry a kind; a deploy record's JSON has no such key, so
// the contract the pack and the golden corpus pin does not move (PD-20).
func TestRemovalRecordHasAKindAndADeployRecordDoesNot(t *testing.T) {
	r, _ := removalRunner(t, Options{})
	removal, err := r.NewRemoval("feature_x")
	if err != nil {
		t.Fatal(err)
	}
	deploy, err := r.NewJob([]EnvRequest{{Name: "production", ExpectedSHA: shaA}})
	if err != nil {
		t.Fatal(err)
	}
	rb, _ := json.Marshal(removal)
	db, _ := json.Marshal(deploy)
	if !strings.Contains(string(rb), `"kind":"remove"`) {
		t.Errorf("removal JSON lacks the kind: %s", rb)
	}
	if strings.Contains(string(db), `"kind"`) {
		t.Errorf("deploy JSON gained a kind key: %s", db)
	}
}

func TestRemovalRunsInsideTheSharedLock(t *testing.T) {
	// A removal and a deploy queued together never run side by side: the
	// removal waits on the same slot (the full cross-process case is in
	// TestRemovalBlockedAcrossRunners).
	r, _ := removalRunner(t, Options{})
	release, err := r.lock.Acquire(r.ctx)
	if err != nil {
		t.Fatal(err)
	}
	job, err := r.NewRemoval("feature_x")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(job); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Store().Get(job.ID)
	if got.State != StateQueued {
		t.Fatalf("a removal started while the deploy lock was held: %+v", got)
	}
	release()
	r.wg.Wait()
	got, _ = r.Store().Get(job.ID)
	if got.State != StateDeployed {
		t.Fatalf("removal after the lock was released = %+v", got)
	}
}
