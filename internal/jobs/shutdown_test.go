package jobs

// Shutdown behaviour of the runner (FORK-WR-01, FORK-WR-02): a job the runner
// accepted is always on record as finished by the time Shutdown returns, for
// the background path (Start) and the synchronous path (RunSync, wait:true).

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/puppet-stagehand/stagehand-deploy-agent/internal/jobs/fakeengine"
)

// instantFailRunner is a runner whose engine never starts (a missing command),
// so every job ends at once, with no child process, and a stress test can run
// hundreds of them.
func instantFailRunner(t *testing.T) *Runner {
	t.Helper()
	r, err := NewRunner(Options{
		Engine:      R10kEngine(func(env string) []string { return []string{"/nonexistent/stagehand-r10k", env} }),
		Basedir:     t.TempDir(),
		Timeout:     time.Minute,
		OutputLimit: 1024,
		Store:       NewMemoryStore(1000),
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestStartShutdownRace: Start racing Shutdown either refuses with ErrShutdown
// or is waited for. No accepted job may be left queued when Shutdown returns
// (FORK-WR-02: wg.Add used to happen outside the lock that checks "closed", so
// Wait could return on a zero counter while a Start was between the two).
func TestStartShutdownRace(t *testing.T) {
	for i := 0; i < 200; i++ {
		r := instantFailRunner(t)
		const starters = 16
		var wg sync.WaitGroup
		var mu sync.Mutex
		var accepted []string
		begin := make(chan struct{})
		for s := 0; s < starters; s++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-begin
				job, err := r.NewJob([]EnvRequest{{Name: "production", ExpectedSHA: shaA}})
				if err != nil {
					t.Error(err)
					return
				}
				switch err := r.Start(job); {
				case err == nil:
					mu.Lock()
					accepted = append(accepted, job.ID)
					mu.Unlock()
				case errors.Is(err, ErrShutdown):
				default:
					t.Errorf("Start = %v, want nil or ErrShutdown", err)
				}
			}()
		}
		close(begin)
		if err := r.Shutdown(10 * time.Second); err != nil {
			t.Fatalf("iteration %d: Shutdown = %v", i, err)
		}
		// Shutdown has returned: nothing it was racing may still be in flight,
		// so no wait here. Starters that lose the race return ErrShutdown.
		mu.Lock()
		ids := append([]string(nil), accepted...)
		mu.Unlock()
		for _, id := range ids {
			got, err := r.Store().Get(id)
			if err != nil || !got.State.Terminal() {
				t.Fatalf("iteration %d: job %s accepted by Start is %q (%v) after Shutdown returned", i, id, got.State, err)
			}
		}
		wg.Wait()
		// A Start that was admitted after Shutdown returned would be a leak.
		mu.Lock()
		late := len(accepted) - len(ids)
		mu.Unlock()
		if late != 0 {
			t.Fatalf("iteration %d: %d jobs accepted after Shutdown returned", i, late)
		}
	}
}

// TestRunSyncJoinsShutdown: a wait:true deploy in flight when Shutdown is
// called has its final record on disk when Shutdown returns (FORK-WR-01:
// RunSync never joined the wait group, so Shutdown returned straight after
// cancelling and the record stayed "deploying").
func TestRunSyncJoinsShutdown(t *testing.T) {
	t.Setenv(fakeengine.DelayEnv, "2s")
	r, _ := fakeEngine(t, "r10k", "testdata/r10k-5.0.3/deploy-success.json", shaA, Options{})
	job, err := r.NewJob([]EnvRequest{{Name: "production", ExpectedSHA: shaA}})
	if err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		job Job
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		j, err := r.RunSync(job)
		done <- outcome{j, err}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if got, err := r.Store().Get(job.ID); err == nil && got.State == StateDeploying {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the job never reached deploying")
		}
		time.Sleep(5 * time.Millisecond)
	}

	if err := r.Shutdown(5 * time.Second); err != nil {
		t.Fatalf("Shutdown = %v", err)
	}
	got, err := r.Store().Get(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateFailed || got.FinishedAt == nil {
		t.Fatalf("record after Shutdown returned = %s (finished %v), want a finished failed record", got.State, got.FinishedAt)
	}
	if e := got.Results[0].Error; e == "" || !containsAny(e, "shutting down", "stopped") {
		t.Fatalf("result error = %q, want it to name the shutdown", e)
	}
	select {
	case o := <-done:
		if o.err != nil {
			t.Fatalf("RunSync = %v", o.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunSync did not return")
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// TestAdmissionIsCountedBeforeShutdownCanWait pins the ordering the stress test
// above can only hit by luck: a job that reserve() admitted is already counted
// in the wait group, so a Shutdown that runs between admission and the job's
// goroutine starting waits for it instead of returning on a zero counter
// (FORK-WR-02).
func TestAdmissionIsCountedBeforeShutdownCanWait(t *testing.T) {
	r := instantFailRunner(t)
	if err := r.reserve(); err != nil {
		t.Fatal(err)
	}
	if err := r.Shutdown(200 * time.Millisecond); err == nil {
		t.Fatal("Shutdown returned nil while an admitted job had not finished")
	}
	r.release()
	if err := r.Shutdown(5 * time.Second); err != nil {
		t.Fatalf("Shutdown after the job was released = %v", err)
	}
	if err := r.reserve(); !errors.Is(err, ErrShutdown) {
		t.Fatalf("reserve after Shutdown = %v, want ErrShutdown", err)
	}
}
