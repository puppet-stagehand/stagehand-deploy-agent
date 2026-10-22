package server

// Shutdown ordering of Serve (FORK-WR-01, FORK-WR-03): when the process is told
// to stop, a deploy a client is waiting on gets its final record written, and
// the store's instance lock file is removed, before Serve returns.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/puppet-stagehand/stagehand-deploy-agent/internal/jobs"
	"github.com/puppet-stagehand/stagehand-deploy-agent/internal/jobs/fakeengine"
)

// serveRig is a real Serve on a loopback port: a FileStore, a runner with a
// context of its own (not the serve context, the strictest case: only Serve's
// own ordering can stop the engine in time) and the fake r10k.
type serveRig struct {
	f       *mtlsFixture
	store   *jobs.FileStore
	runner  *jobs.Runner
	addr    string
	client  *http.Client
	cancel  context.CancelFunc
	served  chan error
	started time.Time
}

func startServeRig(t *testing.T, engineDelay string) *serveRig {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f := newMTLSFixture(t, "stagehand-code-manager")
	basedir := t.TempDir()
	fixture, _ := filepath.Abs("../jobs/testdata/r10k-5.0.3/deploy-success.json")
	t.Setenv(fakeengine.FixtureEnv, fixture)
	t.Setenv(fakeengine.BasedirEnv, basedir)
	t.Setenv(fakeengine.SHAEnv, tracerSHA)
	if engineDelay != "" {
		t.Setenv(fakeengine.DelayEnv, engineDelay)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, err := jobs.NewFileStore(f.stateDir, 50, jobs.WithLogger(log))
	if err != nil {
		t.Fatal(err)
	}
	runner, err := jobs.NewRunner(jobs.Options{
		Engine: jobs.R10kEngine(func(env string) []string {
			return []string{exe, "deploy", "environment", env, "--config=/etc/puppetlabs/r10k/r10k.yaml"}
		}),
		Basedir:     basedir,
		Timeout:     5 * time.Minute,
		OutputLimit: 4096,
		Store:       store,
		Logger:      log,
		LockPath:    filepath.Join(f.stateDir, "deploy.lock"),
	})
	if err != nil {
		t.Fatal(err)
	}
	addr := "127.0.0.1:" + strconv.Itoa(freePort(t))
	srv, err := New(Options{Addr: addr, TLS: f.tls, Runner: runner, Store: store, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	rig := &serveRig{f: f, store: store, runner: runner, addr: addr, cancel: cancel,
		served: make(chan error, 1), client: f.client(t, clientCert(t, f.ca, "stagehand-code-manager"))}
	t.Cleanup(func() {
		cancel()
		_ = runner.Shutdown(10 * time.Second)
		_ = store.Close()
	})
	go func() { rig.served <- srv.Serve(ctx) }()

	deadline := time.Now().Add(10 * time.Second)
	for {
		c, err := net.Dial("tcp", addr)
		if err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Serve never listened on %s: %v", addr, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return rig
}

// waitServed returns Serve's result, failing the test when it does not return
// within limit.
func (rig *serveRig) waitServed(t *testing.T, limit time.Duration) error {
	t.Helper()
	select {
	case err := <-rig.served:
		return err
	case <-time.After(limit):
		t.Fatalf("Serve did not return within %s of the stop signal", limit)
		return nil
	}
}

func lockFiles(t *testing.T, stateDir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(stateDir, "instances", "*.lock"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestServeWritesWaitTrueRecordBeforeReturn: a wait:true deploy is in flight
// when the stop signal arrives. When Serve returns its record is finished (the
// engine was stopped, the failure written), and the client got an answer or a
// closed connection, never a hang.
func TestServeWritesWaitTrueRecordBeforeReturn(t *testing.T) {
	rig := startServeRig(t, "60s") // the engine sleeps far longer than the test
	type answer struct {
		code int
		body []byte
		err  error
	}
	got := make(chan answer, 1)
	go func() {
		resp, err := rig.client.Post("https://"+rig.addr+"/api/v1/deploys", "application/json",
			strings.NewReader(`{"environments":[{"name":"production","expected_sha":"`+tracerSHA+`"}],"wait":true}`))
		if err != nil {
			got <- answer{err: err}
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		got <- answer{code: resp.StatusCode, body: b}
	}()

	var id string
	deadline := time.Now().Add(15 * time.Second)
	for id == "" {
		list, err := rig.store.List(1)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) == 1 && list[0].State == jobs.StateDeploying {
			id = list[0].ID
		}
		if time.Now().After(deadline) {
			t.Fatal("the wait:true deploy never reached deploying")
		}
		time.Sleep(10 * time.Millisecond)
	}

	rig.cancel()
	// Old ordering waited out the whole 30 s HTTP grace before it gave up on the
	// handler; allow it, so a failure is the record assertion below.
	if err := rig.waitServed(t, 45*time.Second); err != nil {
		t.Fatalf("Serve = %v", err)
	}
	rec, err := rig.store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != jobs.StateFailed || rec.FinishedAt == nil {
		t.Fatalf("record when Serve returned = %s (finished %v), want a finished failed record", rec.State, rec.FinishedAt)
	}
	if e := rec.Results[0].Error; !strings.Contains(e, "shutting down") {
		t.Fatalf("result error = %q, want it to say the service was shutting down", e)
	}

	select {
	case a := <-got:
		if a.err == nil {
			var j jobs.Job
			if a.code != http.StatusOK || json.Unmarshal(a.body, &j) != nil || j.State != jobs.StateFailed {
				t.Fatalf("client answer = %d %s, want 200 with the failed record", a.code, a.body)
			}
		} // a closed connection is acceptable too
	case <-time.After(10 * time.Second):
		t.Fatal("the wait:true client was left hanging")
	}
}

// TestServeClosesStore: Serve closes the FileStore as its last step, so the
// instance lock file does not outlive the process (one file per restart used
// to accumulate, FORK-WR-03).
func TestServeClosesStore(t *testing.T) {
	rig := startServeRig(t, "")
	if n := len(lockFiles(t, rig.f.stateDir)); n != 1 {
		t.Fatalf("%d instance lock files while serving, want 1", n)
	}
	rig.cancel()
	if err := rig.waitServed(t, 15*time.Second); err != nil {
		t.Fatalf("Serve = %v", err)
	}
	if left := lockFiles(t, rig.f.stateDir); len(left) != 0 {
		t.Fatalf("instance lock files left after Serve returned: %v", left)
	}
}
