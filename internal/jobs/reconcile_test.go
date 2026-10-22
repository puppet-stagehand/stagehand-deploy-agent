package jobs

// What Reconcile cleans up besides interrupted jobs (FORK-IN-02, FORK-WR-03):
// instance lock files of dead processes, temporary files a crash left in jobs/,
// and records that no longer parse. Each test asserts on the files, so it reads
// the same before and after Reconcile learned to return a report.

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openStoreIn(t *testing.T, stateDir string, opts ...StoreOption) *FileStore {
	t.Helper()
	s, err := NewFileStore(stateDir, 500, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// A dead instance's lock file stays behind however cleanly its records ended:
// Reconcile used to look at it only for a record that was still unfinished.
func TestReconcileSweepsStaleInstanceLocks(t *testing.T) {
	stateDir := t.TempDir()

	// The dead owner: every record it wrote is finished, and its lock file is
	// still on disk (a killed process, or one that never closed its store).
	dead := openStoreIn(t, stateDir)
	if err := dead.Save(finishedJob(t, time.Now().Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	deadLock := filepath.Join(stateDir, "instances", dead.instance+".lock")
	if err := dead.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(deadLock, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	live := openStoreIn(t, stateDir) // another running process: its flock is held
	liveLock := filepath.Join(stateDir, "instances", live.instance+".lock")

	s := openStoreIn(t, stateDir)
	ownLock := filepath.Join(stateDir, "instances", s.instance+".lock")
	if _, err := s.Reconcile(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(deadLock); !os.IsNotExist(err) {
		t.Errorf("the dead instance's lock file survived Reconcile (stat err %v)", err)
	}
	for name, p := range map[string]string{"a live instance's": liveLock, "its own": ownLock} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s lock file was removed: %v", name, err)
		}
	}
}

func TestReconcileRemovesOldTempFiles(t *testing.T) {
	stateDir := t.TempDir()
	s := openStoreIn(t, stateDir)
	jobsDir := filepath.Join(stateDir, "jobs")
	old := filepath.Join(jobsDir, ".x.json.tmp-123")
	recent := filepath.Join(jobsDir, ".y.json.tmp-456")
	for _, p := range []string{old, recent} {
		if err := os.WriteFile(p, []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	elevenMinutesAgo := time.Now().Add(-11 * time.Minute)
	if err := os.Chtimes(old, elevenMinutesAgo, elevenMinutesAgo); err != nil {
		t.Fatal(err)
	}
	oneMinuteAgo := time.Now().Add(-time.Minute)
	if err := os.Chtimes(recent, oneMinuteAgo, oneMinuteAgo); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("the 11 minute old temp file survived (stat err %v)", err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Errorf("the 1 minute old temp file was removed (another writer may own it): %v", err)
	}
}

// A record that does not parse is moved aside, not deleted (it may be needed
// to see what went wrong), and stops being logged on every later List.
func TestReconcileQuarantinesCorruptRecords(t *testing.T) {
	stateDir := t.TempDir()
	var logs bytes.Buffer
	s := openStoreIn(t, stateDir, WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	good := finishedJob(t, time.Now().Add(-time.Minute))
	if err := s.Save(good); err != nil {
		t.Fatal(err)
	}
	badID := newJobID()
	bad := filepath.Join(stateDir, "jobs", badID+".json")
	if err := os.WriteFile(bad, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bad); !os.IsNotExist(err) {
		t.Errorf("the corrupt record is still in jobs/ (stat err %v)", err)
	}
	kept, err := os.ReadFile(filepath.Join(stateDir, "jobs", ".corrupt", badID+".json"))
	if err != nil || string(kept) != "{" {
		t.Errorf("quarantined copy = %q, %v; want the original bytes", kept, err)
	}
	if fi, err := os.Stat(filepath.Join(stateDir, "jobs", ".corrupt")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf(".corrupt dir = %v, %v; want mode 0700", fi, err)
	}

	for i := 0; i < 2; i++ {
		list, err := s.List(10)
		if err != nil || len(list) != 1 || list[0].ID != good.ID {
			t.Fatalf("List = %v, %v; want only the good record", list, err)
		}
	}
	lines := 0
	for _, l := range strings.Split(logs.String(), "\n") {
		if strings.Contains(l, badID) {
			lines++
		}
	}
	if lines != 1 {
		t.Errorf("the corrupt record is named on %d log lines, want exactly 1 (from Reconcile):\n%s", lines, logs.String())
	}
}

// Reconcile reports what it did, for the start-up log line.
func TestReconcileReportCounts(t *testing.T) {
	stateDir := t.TempDir()
	dead := openStoreIn(t, stateDir)
	interrupted := finishedJob(t, time.Now().Add(-time.Hour))
	interrupted.State, interrupted.FinishedAt = StateDeploying, nil
	interrupted.Results[0].State = StateDeploying
	if err := dead.Save(interrupted); err != nil {
		t.Fatal(err)
	}
	deadLock := filepath.Join(stateDir, "instances", dead.instance+".lock")
	if err := dead.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(deadLock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(stateDir, "jobs", ".z.json.tmp-9")
	if err := os.WriteFile(tmp, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	long := time.Now().Add(-time.Hour)
	if err := os.Chtimes(tmp, long, long); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "jobs", newJobID()+".json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	s := openStoreIn(t, stateDir)
	rep, err := s.Reconcile()
	if err != nil {
		t.Fatal(err)
	}
	if want := (ReconcileReport{Interrupted: 1, StaleLocks: 1, TempFiles: 1, Quarantined: 1}); rep != want {
		t.Fatalf("Reconcile = %+v, want %+v", rep, want)
	}
	// A second pass has nothing left to do.
	if rep, err := s.Reconcile(); err != nil || rep != (ReconcileReport{}) {
		t.Fatalf("second Reconcile = %+v, %v; want nothing", rep, err)
	}
}
