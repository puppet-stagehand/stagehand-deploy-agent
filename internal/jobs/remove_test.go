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
	"time"

	"github.com/puppet-stagehand/stagehand-deploy-agent/internal/jobs/fakeengine"
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

// writeFile writes content to path, creating its directory.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// waitState polls r's store until job id is in state want.
func waitState(t *testing.T, r *Runner, id string, want State) Job {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		got, err := r.Store().Get(id)
		if err == nil && got.State == want {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s never reached %s (last %+v, %v)", id, want, got, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRemovalWaitsForDeploy(t *testing.T) {
	t.Setenv(fakeengine.DelayEnv, "1s")
	r, basedir := fakeEngine(t, "r10k", "testdata/r10k-5.0.3/deploy-success.json", shaA,
		Options{LockPath: filepath.Join(t.TempDir(), "deploy.lock")})
	makeEnv(t, basedir, "feature_x")

	deploy, err := r.NewJob([]EnvRequest{{Name: "production", ExpectedSHA: shaA}})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(deploy); err != nil {
		t.Fatal(err)
	}
	waitState(t, r, deploy.ID, StateDeploying)

	removal, err := r.NewRemoval("feature_x")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(removal); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if got, _ := r.Store().Get(removal.ID); got.State != StateQueued {
		t.Fatalf("the removal ran during the deploy: %+v", got)
	}
	if _, err := os.Lstat(filepath.Join(basedir, "feature_x")); err != nil {
		t.Fatalf("environment removed while the deploy ran: %v", err)
	}

	r.wg.Wait()
	d, _ := r.Store().Get(deploy.ID)
	rm, _ := r.Store().Get(removal.ID)
	if d.State != StateDeployed || rm.State != StateDeployed {
		t.Fatalf("deploy %s, removal %s (%+v)", d.State, rm.State, rm.Results)
	}
	if rm.StartedAt.Before(*d.FinishedAt) {
		t.Fatalf("removal started %v, before the deploy finished %v", rm.StartedAt, d.FinishedAt)
	}
}

// Two runners with their own in-process slots stand for two agent processes on
// one state directory: only the flock on deploy.lock keeps the removal out.
func TestRemovalBlockedAcrossRunners(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "deploy.lock")
	// B is built first: the fake engine reads the basedir of the runner built
	// last, and that is A's, the one that deploys.
	b, bBase := fakeEngine(t, "r10k", "testdata/r10k-5.0.3/deploy-success.json", shaA, Options{LockPath: lockPath})
	makeEnv(t, bBase, "feature_x")
	t.Setenv(fakeengine.DelayEnv, "1s")
	a, _ := fakeEngine(t, "r10k", "testdata/r10k-5.0.3/deploy-success.json", shaA, Options{LockPath: lockPath})

	deploy, _ := a.NewJob([]EnvRequest{{Name: "production", ExpectedSHA: shaA}})
	if err := a.Start(deploy); err != nil {
		t.Fatal(err)
	}
	waitState(t, a, deploy.ID, StateDeploying)

	removal, err := b.NewRemoval("feature_x")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Start(removal); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	if got, _ := b.Store().Get(removal.ID); got.State != StateQueued {
		t.Fatalf("runner B removed while runner A held the lock: %+v", got)
	}
	if _, err := os.Lstat(filepath.Join(bBase, "feature_x")); err != nil {
		t.Fatalf("environment removed while another runner deployed: %v", err)
	}

	a.wg.Wait()
	done := waitState(t, b, removal.ID, StateDeployed)
	d, _ := a.Store().Get(deploy.ID)
	if done.StartedAt.Before(*d.FinishedAt) {
		t.Fatalf("removal started %v, before the other runner's deploy finished %v", done.StartedAt, d.FinishedAt)
	}
	b.wg.Wait()
}

func TestSweepRemovalsAfterCrash(t *testing.T) {
	r, basedir := removalRunner(t, Options{})
	id := newJobID()
	leftover := filepath.Join(basedir, ".removing-feature_x-"+id)
	writeFile(t, filepath.Join(leftover, "modules", "a", "init.pp"), "class a {}\n")
	staging := makeEnv(t, basedir, "staging")

	// Things that are not leftovers of this kind, or that must not be followed.
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "keep.txt"), "keep")
	if err := os.Symlink(outside, filepath.Join(basedir, ".removing-link-"+id)); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(basedir, ".removing-file-"+id), "not a directory")

	n, err := r.SweepRemovals()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("swept %d, want 1", n)
	}
	if _, err := os.Lstat(leftover); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("leftover not deleted: %v", err)
	}
	for _, keep := range []string{staging, filepath.Join(basedir, "feature_x"), filepath.Join(outside, "keep.txt"),
		filepath.Join(basedir, ".removing-link-"+id), filepath.Join(basedir, ".removing-file-"+id)} {
		if _, err := os.Lstat(keep); err != nil {
			t.Errorf("%s was removed by the sweep: %v", keep, err)
		}
	}
	// A second sweep has nothing to do.
	if n, err := r.SweepRemovals(); err != nil || n != 0 {
		t.Errorf("second sweep = %d, %v", n, err)
	}
}

func TestRemovalRefusesSymlink(t *testing.T) {
	r, basedir := removalRunner(t, Options{})
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "modules", "keep.pp"), "class keep {}\n")
	link := filepath.Join(basedir, "linked")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	job, err := r.NewRemoval("linked")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(job); err != nil {
		t.Fatal(err)
	}
	got := waitState(t, r, job.ID, StateFailed)
	if res := got.Results[0]; res.State != StateFailed || !strings.Contains(res.Error, "not a directory") {
		t.Fatalf("result = %+v", res)
	}
	if _, err := os.Stat(filepath.Join(outside, "modules", "keep.pp")); err != nil {
		t.Fatalf("the symlink target was touched: %v", err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the symlink itself was replaced: %v", err)
	}
	r.wg.Wait()

	// A plain file at the path is refused the same way.
	writeFile(t, filepath.Join(basedir, "afile"), "x")
	job, _ = r.NewRemoval("afile")
	_ = r.Start(job)
	got = waitState(t, r, job.ID, StateFailed)
	if !strings.Contains(got.Results[0].Error, "not a directory") {
		t.Fatalf("file result = %+v", got.Results[0])
	}
	if _, err := os.Stat(filepath.Join(basedir, "afile")); err != nil {
		t.Fatalf("a file was removed: %v", err)
	}
}

// ListKind returns one kind only, newest first, on both stores (PD-21).
func TestListKindSeparatesDeploysFromRemovals(t *testing.T) {
	fs, err := NewFileStore(t.TempDir(), 50)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fs.Close() })
	for name, s := range map[string]Store{"file": fs, "memory": NewMemoryStore(50)} {
		t.Run(name, func(t *testing.T) {
			base := time.Now().Add(-time.Hour)
			want := map[string][]string{}
			for i := 0; i < 5; i++ {
				j := finishedJob(t, base.Add(time.Duration(i)*time.Minute))
				if i%2 == 1 {
					j.Kind = KindRemove
				}
				if err := s.Save(j); err != nil {
					t.Fatal(err)
				}
				want[j.Kind] = append([]string{j.ID}, want[j.Kind]...) // newest first
			}
			for _, kind := range []string{"", KindRemove} {
				got, err := s.ListKind(kind, 100)
				if err != nil {
					t.Fatal(err)
				}
				var ids []string
				for _, j := range got {
					if j.Kind != kind {
						t.Errorf("ListKind(%q) returned a %q record", kind, j.Kind)
					}
					ids = append(ids, j.ID)
				}
				if strings.Join(ids, ",") != strings.Join(want[kind], ",") {
					t.Errorf("ListKind(%q) = %v, want %v", kind, ids, want[kind])
				}
			}
			if got, _ := s.ListKind("", 1); len(got) != 1 {
				t.Errorf("limit not applied: %d records", len(got))
			}
		})
	}
}

// Removals have a history window of their own: however many there are, they do
// not push deploy history out (the same rule as refusals, PD-17).
func TestRetentionCountsRemovalsApartFromDeploys(t *testing.T) {
	for name, s := range map[string]Store{"file": mustFileStore(t, 3), "memory": NewMemoryStore(3)} {
		t.Run(name, func(t *testing.T) {
			base := time.Now().Add(-time.Hour)
			var deploys []string
			for i := 0; i < 3; i++ {
				j := finishedJob(t, base.Add(time.Duration(i)*time.Minute))
				deploys = append(deploys, j.ID)
				if err := s.Save(j); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < 8; i++ {
				j := finishedJob(t, base.Add(time.Duration(10+i)*time.Minute))
				j.Kind = KindRemove
				if err := s.Save(j); err != nil {
					t.Fatal(err)
				}
			}
			for _, id := range deploys {
				if _, err := s.Get(id); err != nil {
					t.Errorf("deploy %s evicted by removals: %v", id, err)
				}
			}
			if got, _ := s.ListKind(KindRemove, 100); len(got) != 3 {
				t.Errorf("%d removal records kept, want 3", len(got))
			}
		})
	}
}

func mustFileStore(t *testing.T, max int) *FileStore {
	t.Helper()
	s, err := NewFileStore(t.TempDir(), max)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
