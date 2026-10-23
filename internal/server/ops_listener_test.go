package server

// The ops listener beside the mTLS listener (D-05, D-07, PD-36): what it
// serves, what it refuses, the warning for a non-loopback bind, and that it
// outlives the runner's drain so /readyz can say 503 while the process stops.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/puppet-stagehand/stagehand-deploy-agent/internal/jobs"
)

// syncBuffer is a bytes.Buffer safe for a logger used from many goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// blockingStore holds the first terminal Save once armed, until released: a
// runner draining a job then stays in Shutdown for as long as the test wants.
type blockingStore struct {
	jobs.Store
	armed   atomic.Bool
	blocked chan struct{} // closed when a Save is held
	release chan struct{}
	once    sync.Once
}

func newBlockingStore(s jobs.Store) *blockingStore {
	return &blockingStore{Store: s, blocked: make(chan struct{}), release: make(chan struct{})}
}

func (b *blockingStore) Save(j jobs.Job) error {
	if b.armed.Load() && j.State.Terminal() {
		b.once.Do(func() { close(b.blocked) })
		<-b.release
	}
	return b.Store.Save(j)
}

// TestOpsRoutesNotOnMTLSMux: the deploy port never serves the ops routes
// (D-05): a probe on it is a 404, and the certificate does not change that.
func TestOpsRoutesNotOnMTLSMux(t *testing.T) {
	mux := NewMux(NewDeployHandlers(nil, nil), NewEnvironmentHandlers(nil, nil, nil))
	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s on the mTLS mux = %d, want 404", path, rec.Code)
		}
	}
}

func getOps(t *testing.T, addr, path string) (int, string) {
	t.Helper()
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + addr + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func withOps(addr string) rigMod {
	return func(rc *rigConfig) {
		prev := rc.options
		rc.options = func(o *Options) {
			if prev != nil {
				prev(o)
			}
			o.OpsListen = addr
		}
	}
}

// TestOpsListenerServesProbes: Serve opens the ops listener on ops.listen
// beside the mTLS one; it answers the three probe routes, refuses the API,
// and is closed when Serve returns.
func TestOpsListenerServesProbes(t *testing.T) {
	addr := "127.0.0.1:" + strconv.Itoa(freePort(t))
	rig := startServeRig(t, "", withOps(addr))
	if code, body := getOps(t, addr, "/healthz"); code != 200 || body != "ok" {
		t.Fatalf("/healthz = %d %q", code, body)
	}
	if code, body := getOps(t, addr, "/readyz"); code != 200 || body != `{"ready":true,"failing":[]}` {
		t.Fatalf("/readyz = %d %q", code, body)
	}
	for _, path := range []string{"/api/v1/deploys", "/api/v1/info", "/api/v1/environments/x"} {
		if code, _ := getOps(t, addr, path); code != 404 {
			t.Errorf("ops listener %s = %d, want 404", path, code)
		}
	}
	// And the mTLS port answers 404 for the ops routes, certificate or not.
	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		resp, err := rig.client.Get("https://" + rig.addr + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Errorf("mTLS port %s = %d, want 404", path, resp.StatusCode)
		}
	}
	rig.cancel()
	if err := rig.waitServed(t, 15*time.Second); err != nil {
		t.Fatalf("Serve = %v", err)
	}
	if _, err := (&http.Client{Timeout: time.Second}).Get("http://" + addr + "/healthz"); err == nil {
		t.Fatal("the ops listener still answers after Serve returned")
	}
}

// TestOpsListenerWarnsWhenNotLoopback: a bind beyond loopback is allowed but
// logs one WARNING that the listener is unauthenticated (D-05, T-57.6.2-33).
func TestOpsListenerWarnsWhenNotLoopback(t *testing.T) {
	logs := &syncBuffer{}
	rig := startServeRig(t, "", withOps("0.0.0.0:0"), func(rc *rigConfig) {
		prev := rc.options
		rc.options = func(o *Options) {
			prev(o)
			o.Logger = slog.New(slog.NewJSONHandler(logs, nil))
		}
	})
	rig.cancel()
	if err := rig.waitServed(t, 15*time.Second); err != nil {
		t.Fatalf("Serve = %v", err)
	}
	if n := strings.Count(logs.String(), "unauthenticated"); n != 1 {
		t.Fatalf("%d lines say the ops listener is unauthenticated, want exactly 1:\n%s", n, logs.String())
	}
	var warned bool
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil && strings.Contains(line, "unauthenticated") {
			warned = rec["level"] == "WARN"
		}
	}
	if !warned {
		t.Fatalf("the unauthenticated line is not at WARN level:\n%s", logs.String())
	}
}

// TestOpsListenerLoopbackDoesNotWarn: the default bind is quiet.
func TestOpsListenerLoopbackDoesNotWarn(t *testing.T) {
	logs := &syncBuffer{}
	addr := "127.0.0.1:" + strconv.Itoa(freePort(t))
	rig := startServeRig(t, "", withOps(addr), func(rc *rigConfig) {
		prev := rc.options
		rc.options = func(o *Options) {
			prev(o)
			o.Logger = slog.New(slog.NewJSONHandler(logs, nil))
		}
	})
	rig.cancel()
	_ = rig.waitServed(t, 15*time.Second)
	if strings.Contains(logs.String(), "unauthenticated") {
		t.Fatalf("a loopback ops listener was warned about:\n%s", logs.String())
	}
}

// TestOpsListenerOutlivesRunnerDrain (PD-36): once the stop signal arrives,
// /readyz answers 503 while the runner is still draining its last job, and
// the ops listener is closed only after that.
func TestOpsListenerOutlivesRunnerDrain(t *testing.T) {
	addr := "127.0.0.1:" + strconv.Itoa(freePort(t))
	var bs *blockingStore
	rig := startServeRig(t, "60s", withOps(addr), func(rc *rigConfig) {
		rc.wrapStore = func(s jobs.Store) jobs.Store { bs = newBlockingStore(s); return bs }
	})
	if code, _ := getOps(t, addr, "/readyz"); code != 200 {
		t.Fatalf("/readyz before the stop = %d, want 200", code)
	}
	resp, err := rig.client.Post("https://"+rig.addr+"/api/v1/deploys", "application/json",
		strings.NewReader(`{"environments":[{"name":"production","expected_sha":"`+tracerSHA+`"}],"wait":false}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	deadline := time.Now().Add(15 * time.Second)
	for {
		list, err := rig.store.List(1)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) == 1 && list[0].State == jobs.StateDeploying {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the deploy never reached deploying")
		}
		time.Sleep(10 * time.Millisecond)
	}
	bs.armed.Store(true)
	rig.cancel()
	select {
	case <-bs.blocked:
	case <-time.After(15 * time.Second):
		t.Fatal("the runner never reached its final save")
	}
	// Serve is now inside the runner drain; the ops listener must still answer.
	code, body := getOps(t, addr, "/readyz")
	if code != 503 || body != `{"ready":false,"failing":["draining"]}` {
		t.Fatalf("/readyz while draining = %d %s, want 503 draining", code, body)
	}
	if code, _ := getOps(t, addr, "/healthz"); code != 200 {
		t.Fatalf("/healthz while draining = %d, want 200", code)
	}
	close(bs.release)
	if err := rig.waitServed(t, 45*time.Second); err != nil {
		t.Fatalf("Serve = %v", err)
	}
	if _, err := (&http.Client{Timeout: time.Second}).Get("http://" + addr + "/healthz"); err == nil {
		t.Fatal("the ops listener still answers after Serve returned")
	}
}

// TestOpsListenerAddressInUseFailsServe: an ops.listen that cannot bind stops
// the start, and the mTLS listener is never opened.
func TestOpsListenerAddressInUseFailsServe(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	f := newMTLSFixture(t, "stagehand-code-manager")
	runner, err := jobs.NewRunner(jobs.Options{
		Engine:      jobs.R10kEngine(func(env string) []string { return []string{"/bin/true", env} }),
		Basedir:     t.TempDir(),
		Timeout:     time.Minute,
		OutputLimit: 4096,
		Store:       jobs.NewMemoryStore(10),
	})
	if err != nil {
		t.Fatal(err)
	}
	mtlsPort := freePort(t)
	srv, err := New(Options{
		Addr: "127.0.0.1:" + strconv.Itoa(mtlsPort), TLS: f.tls, Runner: runner,
		OpsListen: taken.Addr().String(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Serve(ctx); err == nil {
		t.Fatal("Serve returned nil although the ops address was taken")
	}
	assertNotListening(t, mtlsPort)
}
