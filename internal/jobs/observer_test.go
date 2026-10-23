package jobs

// The runner reports what it does to an Observer (metrics implement it): job
// start and finish with the outcome, refusals, and the queue depth. A nil
// Observer is the default and changes nothing.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/puppet-stagehand/stagehand-deploy-agent/internal/jobs/fakeengine"
)

type recObserver struct {
	mu        sync.Mutex
	events    []string
	durations []time.Duration
	maxQueued int
	maxFlight int
	lastQ     [2]int
}

func (o *recObserver) add(e string) { o.mu.Lock(); o.events = append(o.events, e); o.mu.Unlock() }

func (o *recObserver) JobStarted(kind string) { o.add("start:" + kind) }
func (o *recObserver) JobFinished(kind, outcome string, d time.Duration) {
	o.mu.Lock()
	o.durations = append(o.durations, d)
	o.mu.Unlock()
	o.add("finish:" + kind + ":" + outcome)
}
func (o *recObserver) Refused(kind, reason string) { o.add("refused:" + kind + ":" + reason) }
func (o *recObserver) Queue(queued, inFlight int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.lastQ = [2]int{queued, inFlight}
	o.maxQueued, o.maxFlight = max(o.maxQueued, queued), max(o.maxFlight, inFlight)
}

func (o *recObserver) has(e string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, x := range o.events {
		if x == e {
			return true
		}
	}
	return false
}

func (o *recObserver) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return fmt.Sprint(o.events)
}

func observedRunner(t *testing.T, sha string, obs *recObserver) (*Runner, string) {
	t.Helper()
	return fakeEngine(t, "r10k", "testdata/r10k-5.0.3/deploy-success.json", sha, Options{Observer: obs})
}

func TestObserverSeesDeployedAndFailedJobs(t *testing.T) {
	obs := &recObserver{}
	r, _ := observedRunner(t, shaA, obs)
	job, err := r.NewJob([]EnvRequest{{Name: "production", ExpectedSHA: shaA}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.RunSync(job)
	if err != nil || got.State != StateDeployed {
		t.Fatalf("deploy = %s, %v", got.State, err)
	}
	if !obs.has("start:deploy") || !obs.has("finish:deploy:deployed") {
		t.Fatalf("events = %s", obs)
	}
	if len(obs.durations) != 1 || obs.durations[0] <= 0 {
		t.Fatalf("durations = %v, want one positive duration", obs.durations)
	}

	// The engine deploys another commit than the one expected: failed.
	obs2 := &recObserver{}
	r2, _ := observedRunner(t, shaB, obs2)
	job, _ = r2.NewJob([]EnvRequest{{Name: "production", ExpectedSHA: shaA}})
	got, _ = r2.RunSync(job)
	if got.State != StateFailed || !obs2.has("finish:deploy:failed") {
		t.Fatalf("state %s, events %s", got.State, obs2)
	}
}

func TestObserverSeesRemovals(t *testing.T) {
	obs := &recObserver{}
	r, basedir := observedRunner(t, shaA, obs)
	makeEnv(t, basedir, "feature_x")
	job, err := r.NewRemoval("feature_x")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := r.RunSync(job); err != nil || got.State != StateDeployed {
		t.Fatalf("removal = %s, %v", got.State, err)
	}
	if !obs.has("start:remove") || !obs.has("finish:remove:deployed") {
		t.Fatalf("events = %s", obs)
	}
}

func TestObserverSeesRefusals(t *testing.T) {
	obs := &recObserver{}
	r, basedir := observedRunner(t, shaA, obs)
	makeEnv(t, basedir, "feature_x")
	job, _ := r.NewJob([]EnvRequest{{Name: "production", ExpectedSHA: shaA}})
	if _, err := r.Refuse(job, ErrBusy); err != nil {
		t.Fatal(err)
	}
	job, _ = r.NewJob([]EnvRequest{{Name: "production", ExpectedSHA: shaA}})
	if _, err := r.Refuse(job, ErrShutdown); err != nil {
		t.Fatal(err)
	}
	rm, _ := r.NewRemoval("feature_x")
	if _, err := r.Refuse(rm, ErrBusy); err != nil {
		t.Fatal(err)
	}
	r.ObserveRefusal("remove", "protected")
	for _, want := range []string{"refused:deploy:busy", "refused:deploy:shutdown", "refused:remove:busy", "refused:remove:protected"} {
		if !obs.has(want) {
			t.Errorf("missing %s in %s", want, obs)
		}
	}
}

func TestObserverNotToldOfAJobThatNeverRan(t *testing.T) {
	obs := &recObserver{}
	r, _ := observedRunner(t, shaA, obs)
	job, _ := r.NewJob([]EnvRequest{{Name: "production", ExpectedSHA: shaA}})
	// Another holder has the deploy lock; this job gives up waiting for it.
	release, err := r.lock.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	got, _ := r.Run(ctx, job)
	if got.State != StateFailed {
		t.Fatalf("state = %s, want failed", got.State)
	}
	if obs.has("start:deploy") || !obs.has("finish:deploy:failed") {
		t.Fatalf("events = %s: a job that never held the lock starts nothing but still finishes failed", obs)
	}
	if obs.durations[0] != 0 {
		t.Fatalf("duration = %v, want 0 for a job that never ran", obs.durations[0])
	}
}

// TestObserverQueueDepth: with the lock held by a running deploy, a second
// admitted job counts as queued; when both are done the depth is back to 0.
func TestObserverQueueDepth(t *testing.T) {
	obs := &recObserver{}
	t.Setenv(fakeengine.DelayEnv, "300ms")
	r, _ := observedRunner(t, shaA, obs)
	for i := 0; i < 2; i++ {
		job, err := r.NewJob([]EnvRequest{{Name: "production", ExpectedSHA: shaA}})
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Start(job); err != nil {
			t.Fatal(err)
		}
	}
	r.wg.Wait()
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if obs.maxFlight != 1 {
		t.Errorf("most jobs in flight = %d, want 1 (the lock admits one)", obs.maxFlight)
	}
	if obs.maxQueued < 1 {
		t.Errorf("most jobs queued = %d, want at least 1 while the first ran", obs.maxQueued)
	}
	if obs.lastQ != [2]int{0, 0} {
		t.Errorf("final depth = %v, want 0 queued and 0 in flight", obs.lastQ)
	}
}

func TestRefusalReasonMapping(t *testing.T) {
	for err, want := range map[error]string{
		ErrBusy:                            "busy",
		ErrShutdown:                        "shutdown",
		fmt.Errorf("wrapped: %w", ErrBusy): "busy",
		errors.New("something else"):       "busy",
	} {
		if got := refusalReason(err); got != want {
			t.Errorf("refusalReason(%v) = %q, want %q", err, got, want)
		}
	}
}
