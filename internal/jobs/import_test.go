package jobs

// import-history (D-02): the fork's finished deploys are copied into the agent's
// store once, safely. The legacy format is the Job JSON plus "owner"; the agent
// keeps the layout, so an import is a validated copy.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

const legacyOwner = "ffffffffffffffffffffffffffffffff"

// writeLegacy writes job as the fork's FileStore did: <id>.json, 0600, the Job
// plus the owner of the writing instance.
func writeLegacy(t *testing.T, dir string, job Job) {
	t.Helper()
	b, err := json.Marshal(storedJob{Job: job, Owner: legacyOwner})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, job.ID+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func legacyJob(t *testing.T, created time.Time, state State) Job {
	t.Helper()
	j := finishedJob(t, created)
	j.State = state
	switch state {
	case StateFailed:
		j.Results[0].State, j.Results[0].Error = StateFailed, "engine exited 1"
	case StateQueued, StateDeploying:
		j.FinishedAt = nil
		j.Results[0].State = state
	}
	if state == StateQueued {
		j.StartedAt = nil
	}
	return j
}

// legacyDir builds the mixed directory of the plan: 3 deployed, 2 failed, 1
// deploying, 1 queued, 1 corrupt record, a notes.json, and a leftover temp file.
func legacyDir(t *testing.T) (dir string, finished []Job) {
	t.Helper()
	dir = t.TempDir()
	base := time.Now().Add(-24 * time.Hour)
	i := 0
	next := func() time.Time { i++; return base.Add(time.Duration(i) * time.Minute) }
	for _, st := range []State{StateDeployed, StateDeployed, StateDeployed, StateFailed, StateFailed} {
		j := legacyJob(t, next(), st)
		writeLegacy(t, dir, j)
		finished = append(finished, j)
	}
	writeLegacy(t, dir, legacyJob(t, next(), StateDeploying))
	writeLegacy(t, dir, legacyJob(t, next(), StateQueued))
	if err := os.WriteFile(filepath.Join(dir, newJobID()+".json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.json"), []byte(`{"hello":"world"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".x.json.tmp-1"), []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, finished
}

// snapshotDir records every entry's name, mode, mtime and content.
func snapshotDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		info, err := os.Lstat(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		out = append(out, e.Name()+"|"+info.Mode().String()+"|"+info.ModTime().String()+"|"+string(b))
	}
	sort.Strings(out)
	return out
}

func TestImportHistoryCopiesFinishedRecords(t *testing.T) {
	from, finished := legacyDir(t)
	before := snapshotDir(t, from)
	dst := mustFileStore(t, 500)

	rep, err := ImportHistory(from, dst)
	if err != nil {
		t.Fatal(err)
	}
	if want := (ImportReport{Imported: 5, SkippedUnfinished: 2, SkippedUnreadable: 2}); rep != want {
		t.Fatalf("report = %+v, want %+v", rep, want)
	}
	for _, j := range finished {
		got, err := dst.Get(j.ID)
		if err != nil {
			t.Fatalf("record %s not imported: %v", j.ID, err)
		}
		if g, w := mustJSON(t, got), mustJSON(t, j); g != w {
			t.Errorf("record %s differs:\n got %s\nwant %s", j.ID, g, w)
		}
		raw, err := os.ReadFile(filepath.Join(dst.dir, j.ID+".json"))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte(legacyOwner)) {
			t.Errorf("record %s still names the legacy owner: %s", j.ID, raw)
		}
		if fi, _ := os.Stat(filepath.Join(dst.dir, j.ID+".json")); fi.Mode().Perm() != 0o600 {
			t.Errorf("record %s mode = %v, want 0600", j.ID, fi.Mode().Perm())
		}
	}
	list, _ := dst.List(100)
	if len(list) != 5 {
		t.Errorf("%d records in the agent's store, want 5 (no unfinished one)", len(list))
	}
	if after := snapshotDir(t, from); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Errorf("the legacy directory changed:\nbefore %q\nafter  %q", before, after)
	}
}

func TestImportHistoryIsIdempotent(t *testing.T) {
	from, _ := legacyDir(t)
	dst := mustFileStore(t, 500)
	if _, err := ImportHistory(from, dst); err != nil {
		t.Fatal(err)
	}
	rep, err := ImportHistory(from, dst)
	if err != nil {
		t.Fatal(err)
	}
	if want := (ImportReport{SkippedExisting: 5, SkippedUnfinished: 2, SkippedUnreadable: 2}); rep != want {
		t.Fatalf("second run = %+v, want %+v", rep, want)
	}
}

func TestImportHistoryKeepsARecordThatIsAlreadyThere(t *testing.T) {
	from := t.TempDir()
	j := legacyJob(t, time.Now().Add(-time.Hour), StateDeployed)
	writeLegacy(t, from, j)
	dst := mustFileStore(t, 500)
	mine := j
	mine.Engine = "r10k"
	if err := dst.Save(mine); err != nil {
		t.Fatal(err)
	}
	rep, err := ImportHistory(from, dst)
	if err != nil || rep.Imported != 0 || rep.SkippedExisting != 1 {
		t.Fatalf("report = %+v, %v", rep, err)
	}
	if got, _ := dst.Get(j.ID); got.Engine != "r10k" {
		t.Fatalf("the agent's own record was overwritten: %+v", got)
	}
}

// A crafted file in the legacy directory (it is root's, but the import runs as
// root) copies nothing it should not (T-57.6.2-21).
func TestImportHistoryRefusesCraftedRecords(t *testing.T) {
	from := t.TempDir()
	good := legacyJob(t, time.Now().Add(-time.Hour), StateDeployed)
	writeLegacy(t, from, good)

	// A record filed under another record's id.
	other := legacyJob(t, time.Now().Add(-2*time.Hour), StateDeployed)
	b, _ := json.Marshal(storedJob{Job: other, Owner: legacyOwner})
	misnamed := newJobID()
	os.WriteFile(filepath.Join(from, misnamed+".json"), b, 0o600)

	mk := func(mutate func(m map[string]any)) {
		j := legacyJob(t, time.Now().Add(-3*time.Hour), StateDeployed)
		j.ID = newJobID()
		raw, _ := json.Marshal(storedJob{Job: j, Owner: legacyOwner})
		var m map[string]any
		json.Unmarshal(raw, &m)
		mutate(m)
		raw, _ = json.Marshal(m)
		os.WriteFile(filepath.Join(from, j.ID+".json"), raw, 0o600)
	}
	mk(func(m map[string]any) { m["state"] = "exploded" })
	mk(func(m map[string]any) { m["surprise"] = true })
	mk(func(m map[string]any) { m["refused"] = true })
	mk(func(m map[string]any) { m["results"].([]any)[0].(map[string]any)["environment"] = "../etc" })
	mk(func(m map[string]any) { m["results"].([]any)[0].(map[string]any)["state"] = "weird" })

	// A symlink with a valid name is not followed.
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	lj := legacyJob(t, time.Now().Add(-4*time.Hour), StateDeployed)
	lb, _ := json.Marshal(storedJob{Job: lj, Owner: legacyOwner})
	os.WriteFile(target, lb, 0o600)
	if err := os.Symlink(target, filepath.Join(from, lj.ID+".json")); err != nil {
		t.Fatal(err)
	}

	dst := mustFileStore(t, 500)
	rep, err := ImportHistory(from, dst)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Imported != 1 || rep.SkippedUnreadable != 7 {
		t.Fatalf("report = %+v, want 1 imported and 7 unreadable", rep)
	}
	if _, err := dst.Get(good.ID); err != nil {
		t.Errorf("the good record was not imported: %v", err)
	}
	if _, err := dst.Get(other.ID); err == nil {
		t.Errorf("a record filed under the wrong name was imported under its own id")
	}
}

// What the store's retention would delete at once is not copied, so a second
// run still imports nothing.
func TestImportHistoryCopiesOnlyWhatRetentionKeeps(t *testing.T) {
	from := t.TempDir()
	base := time.Now().Add(-24 * time.Hour)
	var jobs []Job
	for i := 0; i < 5; i++ {
		j := legacyJob(t, base.Add(time.Duration(i)*time.Minute), StateDeployed)
		writeLegacy(t, from, j)
		jobs = append(jobs, j)
	}
	dst := mustFileStore(t, 3)
	// One record the agent already holds, newer than all of them.
	mine := legacyJob(t, base.Add(time.Hour), StateDeployed)
	if err := dst.Save(mine); err != nil {
		t.Fatal(err)
	}

	rep, err := ImportHistory(from, dst)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Imported != 2 || rep.SkippedBeyondMax != 3 {
		t.Fatalf("report = %+v, want 2 imported and 3 beyond max_jobs", rep)
	}
	for _, j := range []Job{mine, jobs[4], jobs[3]} {
		if _, err := dst.Get(j.ID); err != nil {
			t.Errorf("newest record %s missing: %v", j.ID, err)
		}
	}
	rep, err = ImportHistory(from, dst)
	if err != nil || rep.Imported != 0 || rep.SkippedExisting != 2 || rep.SkippedBeyondMax != 3 {
		t.Fatalf("second run = %+v, %v", rep, err)
	}
}

func TestImportHistoryOfAnUnreadableDirectoryFails(t *testing.T) {
	dst := mustFileStore(t, 500)
	if _, err := ImportHistory(filepath.Join(t.TempDir(), "absent"), dst); err == nil {
		t.Fatal("no error for a missing directory")
	}
}
