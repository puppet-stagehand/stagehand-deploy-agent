package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MaxListLimit caps one List call, whatever limit the caller asks for.
const MaxListLimit = 100

// jobIDRE is the form of every job id: 128 random bits in lower-case hex. A
// file store never builds a path from anything else.
var jobIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

// MaxRefusedRecords is how many refused requests (the 503 answers) a store
// keeps, newest first, apart from the max_jobs deploy records (PD-17).
const MaxRefusedRecords = 50

// tempFileMaxAge is how old a temporary file in jobs/ must be before Reconcile
// removes it: older than any write that could still be in progress.
const tempFileMaxAge = 10 * time.Minute

// corruptDirName is the jobs/ subdirectory records that do not parse are moved to.
const corruptDirName = ".corrupt"

// errCorrupt marks a record whose file was read but does not parse.
var errCorrupt = errors.New("record does not parse")

// InterruptedReason is the error Reconcile gives a job a dead process left
// unfinished.
const InterruptedReason = "interrupted by a restart"

// FileStore keeps one JSON file per job in <state_dir>/jobs/<id>.json, so
// records survive a restart (D-12). Files are 0600 and the directory 0700:
// they hold repository URLs and engine output. Every write goes to a
// temporary file that is renamed over the record, so a crashed process never
// leaves a half-written record. A finished job's record is also synced to disk
// before the rename, so its verdict survives a power loss; progress writes
// (queued, deploying) are not, which keeps a wait:false answer immediate, and
// Reconcile fails such jobs after any restart anyway. Once a job finishes, the
// store deletes the oldest finished records beyond maxJobs.
//
// Each store is an instance with a random id, and holds an flock on
// <state_dir>/instances/<id>.lock while it is open. Records name the instance
// that wrote them ("owner", never shown by the API), so Reconcile can tell a
// job a dead process left behind from one another live process is running.
type FileStore struct {
	mu       sync.Mutex
	stateDir string
	dir      string // <state_dir>/jobs
	maxJobs  int
	instance string
	instLock *os.File // nil when flock is unavailable
	log      *slog.Logger
}

// storedJob is a record on disk: the job and the instance that wrote it.
type storedJob struct {
	Job
	Owner string `json:"owner,omitempty"`
	// Refused marks a request the runner refused (a 503). It is part of the
	// stored wrapper only, never of the API Job (PD-17).
	Refused bool `json:"refused,omitempty"`
}

// StoreOption configures a FileStore.
type StoreOption func(*FileStore)

// WithLogger sets the logger the store reports to (component=store). Without
// it the store logs to slog.Default().
func WithLogger(l *slog.Logger) StoreOption {
	return func(s *FileStore) {
		if l != nil {
			s.log = l.With("component", "store")
		}
	}
}

// NewFileStore opens (creating as needed) the job store under stateDir.
func NewFileStore(stateDir string, maxJobs int, opts ...StoreOption) (*FileStore, error) {
	if !filepath.IsAbs(stateDir) {
		return nil, fmt.Errorf("jobs: state_dir must be absolute, got %q", stateDir)
	}
	if maxJobs < 1 {
		return nil, fmt.Errorf("jobs: max_jobs must be at least 1, got %d", maxJobs)
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("jobs: state_dir: %w", err)
	}
	dir := filepath.Join(stateDir, "jobs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("jobs: %w", err)
	}
	// An existing directory may have been created wider; job records are
	// for this service only.
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("jobs: %w", err)
	}
	s := &FileStore{stateDir: stateDir, dir: dir, maxJobs: maxJobs, instance: newJobID(),
		log: slog.Default().With("component", "store")}
	for _, o := range opts {
		o(s)
	}
	if err := s.lockInstance(); err != nil {
		// Without it, other instances treat this one's unfinished jobs as
		// abandoned; deploys still work.
		s.log.Error("instance lock", "error", err.Error())
	}
	return s, nil
}

func (s *FileStore) instancesDir() string { return filepath.Join(s.stateDir, "instances") }

func (s *FileStore) lockInstance() error {
	if err := os.MkdirAll(s.instancesDir(), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(s.instancesDir(), s.instance+".lock"), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	ok, err := tryFlockExclusive(f)
	if err == nil && !ok {
		err = errors.New("lock held by another process")
	}
	if err != nil {
		f.Close()
		os.Remove(f.Name())
		return err
	}
	s.instLock = f
	return nil
}

// Close releases the store's instance lock. Records stay on disk.
func (s *FileStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.instLock == nil {
		return nil
	}
	name := s.instLock.Name()
	unflock(s.instLock)
	err := s.instLock.Close()
	s.instLock = nil
	os.Remove(name)
	return err
}

// ownerAlive reports whether the instance that wrote a record is still
// running: its instance lock file exists and is flocked by someone else. A
// dead instance's lock file is left for Reconcile's sweep to remove.
func (s *FileStore) ownerAlive(owner string) bool {
	if owner == s.instance && s.instLock != nil {
		return true
	}
	if !jobIDRE.MatchString(owner) {
		return false
	}
	path := filepath.Join(s.instancesDir(), owner+".lock")
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	ok, err := tryFlockExclusive(f)
	if err != nil {
		return false
	}
	if !ok {
		return true
	}
	unflock(f)
	return false
}

// ReconcileReport counts what one Reconcile did.
type ReconcileReport struct {
	Interrupted int // unfinished jobs of dead processes, marked failed
	StaleLocks  int // instance lock files of dead processes, removed
	TempFiles   int // temporary files a crash left in jobs/, removed
	Quarantined int // records that do not parse, moved to jobs/.corrupt/
}

// Reconcile fails every job a dead process left queued or deploying: its
// unfinished environments become failed "interrupted by a restart", so a
// client never polls a job forever (T-57.6-35). Finished jobs, and jobs an
// instance that is still running owns, are untouched. It also tidies the state
// directory so a long-lived install does not fill it (FORK-IN-02, FORK-WR-03):
// instance lock files whose flock can be taken (a dead owner, whatever state
// its records are in) are removed; jobs/.*.tmp-* files older than ten minutes
// (a write that was cut off) are removed; and records that do not parse are
// moved to jobs/.corrupt/, never deleted, and logged once. The server calls it
// once at start, before the deploy routes accept requests.
func (s *FileStore) Reconcile() (ReconcileReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rep ReconcileReport
	rep.TempFiles = s.sweepTempFilesLocked()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return rep, err
	}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !e.Type().IsRegular() || !jobIDRE.MatchString(id) {
			continue
		}
		rec, err := s.readStoredLocked(id)
		if errors.Is(err, errCorrupt) {
			if qerr := s.quarantineLocked(e.Name()); qerr != nil {
				s.log.Error("reconcile could not move a corrupt record aside", "file", e.Name(), "error", qerr.Error())
				continue
			}
			s.log.Warn("moved a record that does not parse to "+corruptDirName, "file", e.Name(), "error", err.Error())
			rep.Quarantined++
			continue
		}
		if err != nil {
			s.log.Error("reconcile skipped a record", "file", e.Name(), "error", err.Error())
			continue
		}
		if rec.State.Terminal() || s.ownerAlive(rec.Owner) {
			continue
		}
		job := FailUnstarted(rec.Job, InterruptedReason)
		if err := s.writeLocked(job, false); err != nil {
			return rep, err
		}
		s.log.Warn("deploy was unfinished when its process stopped; marked failed", "job_id", id, "state", string(rec.State))
		rep.Interrupted++
	}
	rep.StaleLocks = s.sweepInstanceLocksLocked()
	return rep, nil
}

// sweepTempFilesLocked removes jobs/.<name>.tmp-* files older than
// tempFileMaxAge and returns how many.
func (s *FileStore) sweepTempFilesLocked() int {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.HasPrefix(e.Name(), ".") || !strings.Contains(e.Name(), ".tmp-") {
			continue
		}
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < tempFileMaxAge {
			continue
		}
		if err := os.Remove(filepath.Join(s.dir, e.Name())); err != nil {
			s.log.Error("reconcile could not remove a temporary file", "file", e.Name(), "error", err.Error())
			continue
		}
		n++
	}
	return n
}

// quarantineLocked moves the record file name from jobs/ to jobs/.corrupt/.
func (s *FileStore) quarantineLocked(name string) error {
	dir := filepath.Join(s.dir, corruptDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	dst := filepath.Join(dir, name)
	if _, err := os.Lstat(dst); err == nil {
		dst += "." + strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return os.Rename(filepath.Join(s.dir, name), dst)
}

// sweepInstanceLocksLocked removes every instances/<id>.lock whose flock can be
// taken, so its owner is gone, other than this store's own, and returns how
// many. A process that is between creating its lock file and flocking it fails
// to lock and carries on without (lockInstance logs it): deploys still work.
func (s *FileStore) sweepInstanceLocksLocked() int {
	entries, err := os.ReadDir(s.instancesDir())
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".lock")
		if !ok || !e.Type().IsRegular() || !jobIDRE.MatchString(id) || id == s.instance {
			continue
		}
		path := filepath.Join(s.instancesDir(), e.Name())
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			continue
		}
		locked, err := tryFlockExclusive(f)
		if err == nil && locked {
			unflock(f)
			if err := os.Remove(path); err == nil {
				n++
			}
		}
		f.Close()
	}
	return n
}

func (s *FileStore) path(id string) string { return filepath.Join(s.dir, id+".json") }

// Save writes job atomically, then prunes old finished records.
func (s *FileStore) Save(job Job) error {
	if !jobIDRE.MatchString(job.ID) {
		return fmt.Errorf("jobs: invalid job id %q", job.ID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeLocked(job, false); err != nil {
		return err
	}
	if job.State.Terminal() {
		s.pruneLocked()
	}
	return nil
}

// SaveRefused writes a refused request's record (see Store.SaveRefused): marked
// refused in the stored wrapper, then retention runs, which keeps only the
// newest MaxRefusedRecords such records and leaves the deploy history alone.
func (s *FileStore) SaveRefused(job Job) error {
	if !jobIDRE.MatchString(job.ID) {
		return fmt.Errorf("jobs: invalid job id %q", job.ID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeLocked(job, true); err != nil {
		return err
	}
	s.pruneLocked()
	return nil
}

func (s *FileStore) writeLocked(job Job, refused bool) error {
	b, err := json.Marshal(storedJob{Job: job, Owner: s.instance, Refused: refused})
	if err != nil {
		return err
	}
	if err := writeFileAtomic(s.dir, job.ID+".json", b, job.State.Terminal()); err != nil {
		return fmt.Errorf("jobs: saving %s: %w", job.ID, err)
	}
	return nil
}

// writeFileAtomic writes b to dir/name with mode 0600 through a temporary
// file in the same directory and a rename; durable syncs the data first.
func writeFileAtomic(dir, name string, b []byte, durable bool) error {
	tmp, err := os.CreateTemp(dir, "."+name+".tmp-*") // CreateTemp uses mode 0600
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(b); err != nil {
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if durable {
		if err := tmp.Sync(); err != nil {
			return err
		}
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, filepath.Join(dir, name)); err != nil {
		return err
	}
	ok = true
	return nil
}

// Get returns the job with id, or ErrNotFound (also for an id that is not a
// job id at all).
func (s *FileStore) Get(id string) (Job, error) {
	if !jobIDRE.MatchString(id) {
		return Job{}, ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readLocked(id)
}

func (s *FileStore) readLocked(id string) (Job, error) {
	rec, err := s.readStoredLocked(id)
	return rec.Job, err
}

func (s *FileStore) readStoredLocked(id string) (storedJob, error) {
	b, err := os.ReadFile(s.path(id))
	if errors.Is(err, fs.ErrNotExist) {
		return storedJob{}, ErrNotFound
	}
	if err != nil {
		return storedJob{}, err
	}
	var rec storedJob
	if err := json.Unmarshal(b, &rec); err != nil {
		return storedJob{}, fmt.Errorf("jobs: record %s is corrupt: %w: %w", id, errCorrupt, err)
	}
	return rec, nil
}

// List returns up to limit jobs of every kind (at most MaxListLimit), newest first.
func (s *FileStore) List(limit int) ([]Job, error) {
	return s.list(limit, func(Job) bool { return true })
}

// ListKind returns up to limit jobs whose Kind is kind (at most MaxListLimit),
// newest first. The limit applies after the kind is chosen.
func (s *FileStore) ListKind(kind string, limit int) ([]Job, error) {
	return s.list(limit, func(j Job) bool { return j.Kind == kind })
}

func (s *FileStore) list(limit int, keep func(Job) bool) ([]Job, error) {
	if limit > MaxListLimit {
		limit = MaxListLimit
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.allLocked()
	if err != nil {
		return nil, err
	}
	if limit < 0 {
		limit = 0
	}
	out := []Job{}
	for _, j := range all {
		if len(out) == limit {
			break
		}
		if keep(j) {
			out = append(out, j)
		}
	}
	return out, nil
}

// allLocked reads every record, newest first. A corrupt record is logged and
// skipped so one bad file cannot hide the rest of the history.
func (s *FileStore) allLocked() ([]Job, error) {
	recs, err := s.allStoredLocked()
	if err != nil {
		return nil, err
	}
	out := make([]Job, len(recs))
	for i, r := range recs {
		out[i] = r.Job
	}
	return out, nil
}

func (s *FileStore) allStoredLocked() ([]storedJob, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	out := []storedJob{}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !e.Type().IsRegular() || !jobIDRE.MatchString(id) {
			continue
		}
		rec, err := s.readStoredLocked(id)
		if err != nil {
			s.log.Error("skipped an unreadable record", "file", e.Name(), "error", err.Error())
			continue
		}
		out = append(out, rec)
	}
	sort.SliceStable(out, func(a, b int) bool {
		if !out[a].CreatedAt.Equal(out[b].CreatedAt) {
			return out[a].CreatedAt.After(out[b].CreatedAt)
		}
		return out[a].ID > out[b].ID
	})
	return out, nil
}

// pruneLocked deletes the oldest finished records beyond maxJobs of their kind,
// and the oldest refused records beyond MaxRefusedRecords. Deploys and removals
// are counted apart, so a run of removals cannot push deploy history out, and
// refused records count toward neither. Unfinished jobs are never deleted.
func (s *FileStore) pruneLocked() {
	all, err := s.allStoredLocked()
	if err != nil {
		s.log.Error("retention", "error", err.Error())
		return
	}
	kept, refused := map[string]int{}, 0
	for _, rec := range all { // newest first
		if rec.Refused {
			if refused++; refused <= MaxRefusedRecords {
				continue
			}
		} else {
			if kept[rec.Kind]++; kept[rec.Kind] <= s.maxJobs || !rec.State.Terminal() {
				continue
			}
		}
		if err := os.Remove(s.path(rec.ID)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			s.log.Error("retention", "error", err.Error())
		}
	}
}
