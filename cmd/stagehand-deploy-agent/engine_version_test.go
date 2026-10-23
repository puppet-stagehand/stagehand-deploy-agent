package main

// The engine's own version line, captured once at start for /api/v1/info
// (PD-35): argv only, first line as given, null (empty) on any failure.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/puppet-stagehand/stagehand-deploy-agent/internal/config"
)

func script(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "engine")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEngineVersionAsksEachToolItsOwnFlag(t *testing.T) {
	cmd := script(t, `echo "tool $*"; echo "second line"`)
	if got := engineVersion(config.Engine{Name: "r10k", CommandPath: cmd}, 5*time.Second); got != "tool version" {
		t.Errorf("r10k: %q, want %q", got, "tool version")
	}
	if got := engineVersion(config.Engine{Name: "g10k", CommandPath: cmd}, 5*time.Second); got != "tool -version" {
		t.Errorf("g10k: %q, want %q", got, "tool -version")
	}
}

func TestEngineVersionKeepsTheFirstLineAsGiven(t *testing.T) {
	cmd := script(t, `printf '  r10k 5.0.3 (build "x")  \nignored\n'`)
	if got := engineVersion(config.Engine{Name: "r10k", CommandPath: cmd}, 5*time.Second); got != `r10k 5.0.3 (build "x")` {
		t.Fatalf("got %q", got)
	}
}

func TestEngineVersionIsEmptyOnFailure(t *testing.T) {
	for name, e := range map[string]config.Engine{
		"exits 1":      {Name: "r10k", CommandPath: script(t, `echo "r10k 1"; exit 1`)},
		"prints none":  {Name: "r10k", CommandPath: script(t, `exit 0`)},
		"only blanks":  {Name: "r10k", CommandPath: script(t, `printf '\n\n'`)},
		"missing file": {Name: "r10k", CommandPath: "/nonexistent/r10k"},
	} {
		if got := engineVersion(e, 5*time.Second); got != "" {
			t.Errorf("%s: got %q, want empty", name, got)
		}
	}
}

func TestEngineVersionTimesOut(t *testing.T) {
	cmd := script(t, `sleep 30`)
	start := time.Now()
	if got := engineVersion(config.Engine{Name: "r10k", CommandPath: cmd}, 200*time.Millisecond); got != "" {
		t.Fatalf("got %q after a timeout, want empty", got)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %s to give up on a 200ms timeout", d)
	}
}

func TestEngineVersionIsBounded(t *testing.T) {
	cmd := script(t, `head -c 100000 /dev/zero | tr '\0' x; echo`)
	got := engineVersion(config.Engine{Name: "r10k", CommandPath: cmd}, 5*time.Second)
	if len(got) == 0 || len(got) > 256 {
		t.Fatalf("got %d bytes, want 1 to 256", len(got))
	}
	if strings.Trim(got, "x") != "" {
		t.Fatalf("unexpected content %q", got[:20])
	}
}
