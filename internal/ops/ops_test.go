package ops

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func do(h http.Handler, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func TestOpsHealthz(t *testing.T) {
	rec := do(NewMux(goodChecks(t), nil), http.MethodGet, "/healthz")
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("got %d %q, want 200 ok", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type = %q, want text/plain", ct)
	}
}

// TestOpsHealthzIgnoresFailingChecks: liveness answers while the process
// serves, even when readiness would not.
func TestOpsHealthzIgnoresFailingChecks(t *testing.T) {
	c := goodChecks(t)
	c.EnginePath += ".gone"
	if rec := do(NewMux(c, nil), http.MethodGet, "/healthz"); rec.Code != http.StatusOK {
		t.Fatalf("/healthz = %d with a failing engine check, want 200", rec.Code)
	}
}

func TestOpsMetricsRouteServesTheGivenHandler(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("m")) })
	rec := do(NewMux(goodChecks(t), h), http.MethodGet, "/metrics")
	if rec.Code != http.StatusOK || rec.Body.String() != "m" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
}

// TestAPIRoutesNotOnOpsMux: the ops listener is unauthenticated, so it must
// never serve the deploy API (D-05, T-57.6.2-34).
func TestAPIRoutesNotOnOpsMux(t *testing.T) {
	mux := NewMux(goodChecks(t), nil)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/deploys"},
		{http.MethodPost, "/api/v1/deploys"},
		{http.MethodGet, "/api/v1/deploys/abc"},
		{http.MethodGet, "/api/v1/info"},
		{http.MethodDelete, "/api/v1/environments/x"},
		{http.MethodGet, "/api/v1/removals/abc"},
		{http.MethodGet, "/"},
	} {
		if rec := do(mux, tc.method, tc.path); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s on the ops mux = %d, want 404", tc.method, tc.path, rec.Code)
		}
	}
}

func TestOpsLoopback(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8089": true, "127.9.9.9:1": true, "[::1]:8089": true, "localhost:8089": true,
		"0.0.0.0:0": false, ":8089": false, "10.0.0.5:8089": false, "[::]:8089": false, "example.test:8089": false,
	} {
		if got := Loopback(addr); got != want {
			t.Errorf("Loopback(%q) = %v, want %v", addr, got, want)
		}
	}
}
