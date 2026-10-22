package jobs

// Refused requests (the 503 answers) keep their persisted record, but cannot
// push real deploy history out of the max_jobs window (FORK-IN-03, D-20, PD-17).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// keptRefused is how many refused records a store keeps (PD-17).
const keptRefused = 50

// refuseMany takes n requests through Runner.Refuse, as the 503 path does.
func refuseMany(t *testing.T, r *Runner, n int) []string {
	t.Helper()
	var ids []string
	for i := 0; i < n; i++ {
		job, err := r.NewJob([]EnvRequest{{Name: "production", ExpectedSHA: shaA}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.Refuse(job, ErrBusy); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, job.ID)
	}
	return ids
}

func TestRefusalsDoNotEvictHistory(t *testing.T) {
	stateDir := t.TempDir()
	s, err := NewFileStore(stateDir, 5)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	r, err := NewRunner(Options{
		Engine:      R10kEngine(func(env string) []string { return []string{"/nonexistent/stagehand-r10k", env} }),
		Basedir:     t.TempDir(),
		Timeout:     time.Minute,
		OutputLimit: 1024,
		Store:       s,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Shutdown(5 * time.Second) })

	// Five real deploys, all older than every refusal that follows.
	var deploys []string
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 5; i++ {
		j := finishedJob(t, base.Add(time.Duration(i)*time.Minute))
		deploys = append(deploys, j.ID)
		if err := s.Save(j); err != nil {
			t.Fatal(err)
		}
	}
	refused := refuseMany(t, r, 80)

	for _, id := range deploys {
		if _, err := s.Get(id); err != nil {
			t.Errorf("deploy record %s was evicted by refusals: %v", id, err)
		}
	}
	kept := 0
	for _, id := range refused {
		if _, err := s.Get(id); err == nil {
			kept++
		}
	}
	if kept != keptRefused {
		t.Errorf("%d refused records kept, want exactly the newest %d", kept, keptRefused)
	}
	for _, id := range refused[len(refused)-keptRefused:] {
		if _, err := s.Get(id); err != nil {
			t.Errorf("a refused record among the newest %d is gone: %v", keptRefused, err)
			break
		}
	}

	// The marker is on the stored wrapper only; the API record never has it.
	id := refused[len(refused)-1]
	onDisk, err := os.ReadFile(filepath.Join(stateDir, "jobs", id+".json"))
	if err != nil || !strings.Contains(string(onDisk), `"refused":true`) {
		t.Errorf("stored refused record = %s, %v; want it to carry \"refused\":true", onDisk, err)
	}
	job, err := s.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(job)
	if strings.Contains(string(b), "refused") {
		t.Errorf("the API record carries the refused flag: %s", b)
	}
	if job.State != StateFailed || job.Results[0].Error != ErrBusy.Error() {
		t.Errorf("refused record = %+v, want failed with the refusal reason", job)
	}
}

// The in-memory store (tests, and any caller without a file store) follows the
// same rule.
func TestMemoryStoreRefusalsDoNotEvictHistory(t *testing.T) {
	s := NewMemoryStore(5)
	r, err := NewRunner(Options{
		Engine:      R10kEngine(func(env string) []string { return []string{"/nonexistent/stagehand-r10k", env} }),
		Basedir:     t.TempDir(),
		Timeout:     time.Minute,
		OutputLimit: 1024,
		Store:       s,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Shutdown(5 * time.Second) })
	var deploys []string
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 5; i++ {
		j := finishedJob(t, base.Add(time.Duration(i)*time.Minute))
		deploys = append(deploys, j.ID)
		if err := s.Save(j); err != nil {
			t.Fatal(err)
		}
	}
	refused := refuseMany(t, r, 80)
	for _, id := range deploys {
		if _, err := s.Get(id); err != nil {
			t.Errorf("deploy record %s was evicted by refusals: %v", id, err)
		}
	}
	list, err := s.List(1000)
	if err != nil {
		t.Fatal(err)
	}
	if want := 5 + keptRefused; len(list) != want {
		t.Errorf("%d records kept, want %d (5 deploys + the newest %d refusals)", len(list), want, keptRefused)
	}
	if _, err := s.Get(refused[0]); err == nil {
		t.Error("the oldest refused record is still kept")
	}
}
