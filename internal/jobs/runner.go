package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// MaxEnvironments bounds one deploy request.
const MaxEnvironments = 20

// Engine is the deploy tool a runner drives.
type Engine struct {
	// Name is "r10k" or "g10k"; it is reported in every job.
	Name string
	// MarkerFile is the marker the engine writes into each environment.
	MarkerFile string
	// Argv returns the full argv (command path first) that deploys one
	// environment. It is executed directly, never through a shell.
	Argv func(env string) []string
	// Env is appended to the child's environment (KEY=value). The child
	// starts from os.Environ(); every inherited variable named in Unset, or
	// set again by Env, is dropped first (PD-05).
	Env []string
	// Unset names inherited variables the child must not see.
	Unset []string
}

// childEnv builds the engine child's environment: base with every variable
// named in unset, or set again by env, removed, then env appended. Names are
// compared up to the first "=".
func childEnv(base, env, unset []string) []string {
	drop := make(map[string]bool, len(env)+len(unset))
	for _, n := range unset {
		drop[n] = true
	}
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		drop[name] = true
	}
	out := make([]string, 0, len(base)+len(env))
	for _, kv := range base {
		name, _, _ := strings.Cut(kv, "=")
		if !drop[name] {
			out = append(out, kv)
		}
	}
	return append(out, env...)
}

// R10kEngine drives r10k; argv is normally the upstream r10k command helper.
func R10kEngine(argv func(env string) []string) Engine {
	return Engine{Name: "r10k", MarkerFile: ".r10k-deploy.json", Argv: argv}
}

// G10kEngine drives g10k; argv is normally the upstream g10k command helper
// ([command_path, -config=<cfg>, -branch=<env>, (-verbose)]). g10k is judged
// by its own marker, .g10k-deploy.json, by the same rule as r10k (D-29).
func G10kEngine(argv func(env string) []string) Engine {
	return Engine{Name: "g10k", MarkerFile: ".g10k-deploy.json", Argv: argv}
}

// DefaultMaxJobs is how many job records a FileStore keeps by default
// (deploys.max_jobs).
const DefaultMaxJobs = 500

// ErrInvalidRequest wraps every reason NewJob refuses a request.
var ErrInvalidRequest = errors.New("invalid deploy request")

// MaxPending bounds the jobs one runner holds at once, queued or running.
const MaxPending = 100

// ErrBusy is returned by Start when MaxPending jobs are already pending.
var ErrBusy = errors.New("too many deploys pending")

// ErrShutdown is returned by Start once the runner is shutting down.
var ErrShutdown = errors.New("the deploy service is shutting down")

// Options configure a Runner.
type Options struct {
	Engine      Engine
	Basedir     string        // where environments, and so markers, live
	Timeout     time.Duration // per engine invocation
	OutputLimit int           // bytes of combined output kept per environment
	Store       Store
	// Logger receives the runner's log lines (component=runner). Nil means
	// slog.Default().
	Logger *slog.Logger
	// LockPath is the cross-process deploy lock file, normally
	// <state_dir>/deploy.lock. Empty locks within this process only (tests).
	LockPath string
	// Observer is told what the runner does (metrics implement it). Nil
	// means nobody is told.
	Observer Observer
	// Context is the runner's base context, normally the server's: when it
	// is cancelled, running engines are stopped and queued jobs fail. Nil
	// means context.Background().
	Context context.Context
}

// Runner runs deploy jobs one at a time, holding the deploy lock.
type Runner struct {
	opts Options
	log  *slog.Logger
	// lock serialises jobs, in this process and across processes sharing the
	// lock file, so two deploys never run an engine at once (MH-03).
	lock *DeployLock

	ctx    context.Context // base context for every job
	cancel context.CancelFunc
	wg     sync.WaitGroup // admitted jobs, counted in reserve, released in release

	pendMu  sync.Mutex
	pending int // admitted and not yet released: queued or running
	running int // holding the deploy lock
	closed  bool
}

// NewRunner checks opts and returns a Runner.
func NewRunner(opts Options) (*Runner, error) {
	switch {
	case opts.Engine.Argv == nil || opts.Engine.Name == "" || opts.Engine.MarkerFile == "":
		return nil, errors.New("jobs: engine is not configured")
	case !filepath.IsAbs(opts.Basedir):
		return nil, fmt.Errorf("jobs: environments_dir must be absolute, got %q", opts.Basedir)
	case opts.Timeout <= 0:
		return nil, errors.New("jobs: timeout must be greater than zero")
	case opts.OutputLimit <= 0:
		return nil, errors.New("jobs: output limit must be greater than zero")
	case opts.Store == nil:
		return nil, errors.New("jobs: store is not configured")
	}
	base := opts.Context
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithCancel(base)
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Runner{opts: opts, log: logger.With("component", "runner"), lock: NewDeployLock(opts.LockPath), ctx: ctx, cancel: cancel}, nil
}

// reserve admits one more job, or refuses it. Admission and the wait-group
// count are one step under pendMu, the lock Shutdown takes to set closed, so a
// Shutdown that has begun to Wait can never miss a job that was let in, and a
// job refused as shutting down was never counted (FORK-WR-02). Whoever gets nil
// must call release exactly once when the job is on record as finished.
func (r *Runner) reserve() error {
	r.pendMu.Lock()
	defer r.pendMu.Unlock()
	switch {
	case r.closed || r.ctx.Err() != nil:
		return ErrShutdown
	case r.pending >= MaxPending:
		return ErrBusy
	}
	r.pending++
	r.wg.Add(1)
	r.notifyQueue()
	return nil
}

// release ends an admission made by reserve: the job no longer counts as
// pending, and Shutdown stops waiting for it.
func (r *Runner) release() {
	r.pendMu.Lock()
	r.pending--
	r.notifyQueue()
	r.pendMu.Unlock()
	r.wg.Done()
}

// setRunning records a job taking (+1) or giving back (-1) the deploy lock.
func (r *Runner) setRunning(delta int) {
	r.pendMu.Lock()
	r.running += delta
	r.notifyQueue()
	r.pendMu.Unlock()
}

// Start runs job in the background on the runner's base context and returns
// at once (MH-01). Its progress is read back from the store.
func (r *Runner) Start(job Job) error {
	if err := r.reserve(); err != nil {
		return err
	}
	// The goroutine owns its own copy: the caller still encodes job (the
	// 202 answer) while the runner rewrites its results.
	job = job.clone()
	go func() {
		defer r.release()
		if _, err := r.Run(r.ctx, job); err != nil {
			r.log.Error("deploy failed to record", "job_id", job.ID, "error", err.Error())
		}
	}()
	return nil
}

// RunSync runs job on the runner's base context and waits for it. It is not
// tied to any request: a client that hangs up must not stop an engine half way
// through an environment. deploys.timeout still bounds every invocation. It
// counts as a pending job, so Shutdown waits for its record (FORK-WR-01).
func (r *Runner) RunSync(job Job) (Job, error) {
	if err := r.reserve(); err != nil {
		return job, err
	}
	defer r.release()
	return r.Run(r.ctx, job)
}

// Shutdown stops accepting jobs, cancels the base context (stopping any
// running engine and failing queued jobs) and waits up to timeout for every
// admitted job, background (Start) or synchronous (RunSync), to record its
// outcome.
func (r *Runner) Shutdown(timeout time.Duration) error {
	r.pendMu.Lock()
	r.closed = true
	r.pendMu.Unlock()
	r.cancel()
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("jobs: background deploys still running after %s", timeout)
	}
}

// Store returns the runner's job store.
func (r *Runner) Store() Store { return r.opts.Store }

// Engine returns the name of the runner's engine.
func (r *Runner) Engine() string { return r.opts.Engine.Name }

// NewJob validates envs, records a queued job and returns it.
func (r *Runner) NewJob(envs []EnvRequest) (Job, error) {
	if len(envs) == 0 || len(envs) > MaxEnvironments {
		return Job{}, fmt.Errorf("%w: a deploy names 1 to %d environments, got %d", ErrInvalidRequest, MaxEnvironments, len(envs))
	}
	seen := map[string]bool{}
	job := Job{
		ID:        newJobID(),
		Engine:    r.opts.Engine.Name,
		State:     StateQueued,
		CreatedAt: time.Now().UTC(),
	}
	for _, e := range envs {
		if !ValidEnvironmentName(e.Name) {
			return Job{}, fmt.Errorf("%w: invalid environment name %q", ErrInvalidRequest, e.Name)
		}
		if !ValidSHA(e.ExpectedSHA) {
			return Job{}, fmt.Errorf("%w: invalid expected_sha for %s", ErrInvalidRequest, e.Name)
		}
		if seen[e.Name] {
			return Job{}, fmt.Errorf("%w: environment %s named twice", ErrInvalidRequest, e.Name)
		}
		seen[e.Name] = true
		job.Results = append(job.Results, EnvResult{Environment: e.Name, State: StateQueued, ExpectedSHA: e.ExpectedSHA})
	}
	if err := r.opts.Store.Save(job); err != nil {
		return Job{}, err
	}
	return job, nil
}

// FailUnstarted marks every unfinished environment of job, and the job, failed
// with reason. Nothing ran, so exit codes are -1.
func FailUnstarted(job Job, reason string) Job {
	now := time.Now().UTC()
	for i := range job.Results {
		if !job.Results[i].State.Terminal() {
			job.Results[i].State, job.Results[i].ExitCode, job.Results[i].Error = StateFailed, -1, reason
		}
	}
	job.State, job.FinishedAt = StateFailed, &now
	return job
}

// Refuse records job as failed because the runner would not take it (a busy
// or shutting-down runner), so its id never reads as queued, and returns the
// failed record (PD-04). The 503 answer rests on this method, so how refusals
// are stored can change without touching the handlers. The record is kept (the
// answer names its id) but through SaveRefused, which keeps refusals out of the
// history limit (PD-17).
func (r *Runner) Refuse(job Job, reason error) (Job, error) {
	r.ObserveRefusal(kindLabel(job.Kind), refusalReason(reason))
	job = FailUnstarted(job, reason.Error())
	return job, r.opts.Store.SaveRefused(job)
}

// Run deploys every environment of job in order, one engine invocation each,
// saving progress to the store as it goes, and returns the finished job. The
// job is failed unless every environment was deployed. A job of Kind
// KindRemove removes its environment instead of deploying it, under the same
// lock (D-09).
func (r *Runner) Run(ctx context.Context, job Job) (Job, error) {
	// The job stays queued while it waits for the lock. The lock is held
	// around every engine invocation of the job, environments in request
	// order, one at a time.
	release, err := r.lock.Acquire(ctx)
	if err != nil {
		reason := "not run: " + err.Error()
		if ctx.Err() != nil {
			reason = "not run: " + ErrShutdown.Error()
		}
		job = FailUnstarted(job, reason)
		err := r.opts.Store.Save(job)
		r.finished(job, 0)
		return job, err
	}
	defer release()

	started := time.Now().UTC()
	job.StartedAt = &started
	job.State = StateDeploying
	r.setRunning(1)
	defer r.setRunning(-1)
	if r.opts.Observer != nil {
		r.opts.Observer.JobStarted(kindLabel(job.Kind))
	}
	if err := r.opts.Store.Save(job); err != nil {
		r.finished(FailUnstarted(job, "not recorded"), time.Since(started))
		return job, err
	}

	all := true
	for i := range job.Results {
		if ctx.Err() != nil {
			// Shutting down: the remaining environments never start.
			job.Results[i].State, job.Results[i].ExitCode, job.Results[i].Error = StateFailed, -1, "not run: "+ErrShutdown.Error()
			all = false
			continue
		}
		job.Results[i].State = StateDeploying
		_ = r.opts.Store.Save(job)
		if job.Kind == KindRemove {
			job.Results[i] = r.removeOne(ctx, job.Results[i], job.ID)
		} else {
			job.Results[i] = r.deployOne(ctx, job.Results[i])
		}
		if job.Results[i].State != StateDeployed {
			all = false
		}
		if err := r.opts.Store.Save(job); err != nil {
			r.log.Error("saving deploy progress", "job_id", job.ID, "error", err.Error())
		}
	}

	finished := time.Now().UTC()
	job.FinishedAt = &finished
	job.State = StateDeployed
	if !all {
		job.State = StateFailed
	}
	err = r.opts.Store.Save(job)
	r.finished(job, finished.Sub(started))
	return job, err
}

// finished tells the Observer a job is over.
func (r *Runner) finished(job Job, d time.Duration) {
	if r.opts.Observer != nil {
		r.opts.Observer.JobFinished(kindLabel(job.Kind), string(job.State), d)
	}
}

func (r *Runner) deployOne(ctx context.Context, res EnvResult) EnvResult {
	// Defence in depth: the API validated these already.
	if !ValidEnvironmentName(res.Environment) || !ValidSHA(res.ExpectedSHA) {
		res.State, res.ExitCode, res.Error = StateFailed, -1, "invalid environment request"
		return res
	}

	argv := r.opts.Engine.Argv(res.Environment)
	if len(argv) == 0 {
		res.State, res.ExitCode, res.Error = StateFailed, -1, "engine command is empty"
		return res
	}
	markerPath := filepath.Join(r.opts.Basedir, res.Environment, r.opts.Engine.MarkerFile)
	// What the marker was before the engine ran, so a success can be tied to a
	// marker this run wrote (FORK-IN-04, PD-19).
	markerBefore := SnapshotMarker(markerPath)
	ctx, cancel := context.WithTimeout(ctx, r.opts.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	// The engine leads its own process group (SysProcAttr Setpgid, see
	// proc_unix.go) and a timeout kills the whole group: git children too.
	inProcessGroup(cmd)
	cmd.Env = childEnv(os.Environ(), r.opts.Engine.Env, r.opts.Engine.Unset)
	out := &tailBuffer{limit: r.opts.OutputLimit}
	errTail := &tailBuffer{limit: stderrTailLimit}
	cmd.Stdout = out
	cmd.Stderr = io.MultiWriter(out, errTail)
	// If a grandchild escaped the group and holds the output pipe, stop
	// waiting for it this long after the kill.
	cmd.WaitDelay = 10 * time.Second

	res.StartedAt = time.Now().UTC()
	r.log.Info("deploy starting", "env", res.Environment, "engine", r.opts.Engine.Name)
	runErr := cmd.Run()
	res.FinishedAt = time.Now().UTC()
	res.DurationMS = res.FinishedAt.Sub(res.StartedAt).Milliseconds()
	res.Output, res.OutputTruncated = out.snapshot()

	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
		res.ExitCode = 0
	case errors.As(runErr, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	case cmd.ProcessState != nil:
		// It ran, but Wait gave up on its output pipes (exec.ErrWaitDelay).
		res.ExitCode = cmd.ProcessState.ExitCode()
	default:
		res.State, res.ExitCode, res.Error = StateFailed, -1, "engine did not start: "+runErr.Error()
		return res
	}
	switch ctx.Err() {
	case context.DeadlineExceeded:
		res.State, res.Error = StateFailed, fmt.Sprintf("engine timed out after %s", r.opts.Timeout)
		return res
	case context.Canceled:
		res.State, res.Error = StateFailed, "engine stopped: "+ErrShutdown.Error()
		return res
	}

	markerAfter := SnapshotMarker(markerPath)
	marker, err := ReadMarker(markerPath)
	if err != nil {
		res.State, res.Error = StateFailed, "deploy marker unreadable: "+err.Error()
		return res
	}
	if marker != nil {
		res.MarkerPresent = true
		res.MarkerDeploySuccess = marker.DeploySuccess != nil && *marker.DeploySuccess
	}
	res.State, res.DeployedSHA, res.Error = Normalize(res.ExitCode, marker, res.StartedAt, res.ExpectedSHA)
	// Normalize lets a marker finished in the job's own second count as fresh
	// (r10k writes whole seconds). A marker an earlier run left, byte for byte
	// and to the nanosecond as it was before this engine started, is not this
	// run's work, whatever its finished_at says: never "deployed" from it.
	if res.State == StateDeployed && !markerBefore.Rewritten(markerAfter) {
		res.State, res.DeployedSHA, res.Error = StateFailed, "", "the engine did not rewrite the deploy marker"
	}
	if res.ExitCode != 0 {
		if tail := summariseStderr(errTail.snapshot()); tail != "" {
			res.Error += ": " + tail
		}
	}
	return res
}

// stderrTailLimit bounds the stderr appended to an "engine exited N" error.
const stderrTailLimit = 512

var ansiEscapeRE = regexp.MustCompile("\x1b\\[[0-9;]*[A-Za-z]")

// summariseStderr turns the end of an engine's stderr into one line: a line
// cut by the bound dropped (or marked "..." when it is the only one), colour
// codes removed, blank lines dropped, lines joined with " | ".
func summariseStderr(s string, truncated bool) string {
	if truncated {
		if i := strings.IndexByte(s, '\n'); i >= 0 && strings.TrimSpace(s[i+1:]) != "" {
			s = s[i+1:]
		} else {
			s = "..." + s
		}
	}
	var lines []string
	for _, l := range strings.Split(ansiEscapeRE.ReplaceAllString(s, ""), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return strings.Join(lines, " | ")
}

// tailBuffer keeps the last limit bytes written to it; the end of an engine's
// output is where its error is. stdout and stderr are copied by separate
// goroutines, so writes are locked.
type tailBuffer struct {
	mu        sync.Mutex
	limit     int
	buf       []byte
	truncated bool
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if len(p) > b.limit {
		p = p[len(p)-b.limit:]
		b.truncated = true
	}
	b.buf = append(b.buf, p...)
	if over := len(b.buf) - b.limit; over > 0 {
		b.buf = append(b.buf[:0], b.buf[over:]...)
		b.truncated = true
	}
	return n, nil
}

func (b *tailBuffer) String() string {
	s, _ := b.snapshot()
	return s
}

// snapshot returns what is kept and whether anything was cut.
func (b *tailBuffer) snapshot() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf), b.truncated
}

// Observer is told what the runner does. Metrics implement it. Calls are
// made while the runner holds a lock for the queue depth, so an Observer must
// return quickly and must not call back into the runner.
type Observer interface {
	// JobStarted is called when a job takes the deploy lock.
	JobStarted(kind string)
	// JobFinished is called once the job's final record is written. outcome is
	// "deployed" or "failed"; d is how long the job held the lock, zero if it
	// never ran.
	JobFinished(kind, outcome string, d time.Duration)
	// Refused is called when a request was turned away: busy and shutdown by
	// the runner, protected and invalid by the handlers (via ObserveRefusal).
	Refused(kind, reason string)
	// Queue reports the jobs admitted and waiting, and the jobs holding the lock.
	Queue(queued, inFlight int)
}

// kindLabel is the metric and log name of a job's kind: "deploy" or "remove".
func kindLabel(k string) string {
	if k == KindRemove {
		return "remove"
	}
	return "deploy"
}

// refusalReason names why the runner turned a job away.
func refusalReason(err error) string {
	if errors.Is(err, ErrShutdown) {
		return "shutdown"
	}
	return "busy"
}

// ObserveRefusal reports a request refused before it became a job, such as a
// protected environment or an invalid request. kind is "deploy" or "remove".
// It is safe on a nil Runner and with no Observer.
func (r *Runner) ObserveRefusal(kind, reason string) {
	if r == nil || r.opts.Observer == nil {
		return
	}
	r.opts.Observer.Refused(kind, reason)
}

// notifyQueue tells the Observer the queue depth. The caller holds pendMu.
func (r *Runner) notifyQueue() {
	if r.opts.Observer == nil {
		return
	}
	queued := r.pending - r.running
	if queued < 0 { // Run called directly, without an admission
		queued = 0
	}
	r.opts.Observer.Queue(queued, r.running)
}
