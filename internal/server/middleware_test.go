package server

// The request middleware (T-57.6.2-37, T-57.6.2-36): one JSON log line per
// mTLS request, the route taken from the matched ServeMux pattern, a recovered
// panic as a 500, and metrics that never see a path, id or body.

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/puppet-stagehand/stagehand-deploy-agent/internal/metrics"
)

func newMetrics() *metrics.Registry {
	return metrics.New("1.0.0", "r10k", time.Now)
}

func scrape(t *testing.T, m *metrics.Registry) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

func logLines(t *testing.T, b *syncBuffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(b.String()), "\n") {
		if l == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(l), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q", l)
		}
		out = append(out, rec)
	}
	return out
}

func withCertname(r *http.Request, cn string) *http.Request {
	r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{Subject: pkix.Name{CommonName: cn}}}}
	return r
}

func wrapMux(t *testing.T) (http.Handler, *syncBuffer, *metrics.Registry) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /things/{id}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"id": r.PathValue("id")})
	})
	mux.HandleFunc("POST /things", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("GET /boom", func(w http.ResponseWriter, r *http.Request) { panic("kaboom with a secret") })
	mux.HandleFunc("GET /late", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("partial"))
		panic("after the headers")
	})
	logs := &syncBuffer{}
	m := newMetrics()
	return Wrap(mux, slog.New(slog.NewJSONHandler(logs, nil)), m), logs, m
}

func TestRequestLogLine(t *testing.T) {
	h, logs, _ := wrapMux(t)
	req := withCertname(httptest.NewRequest(http.MethodPost, "/things", strings.NewReader("super-secret-body")), "stagehand-code-manager")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d", rec.Code)
	}
	lines := logLines(t, logs)
	if len(lines) != 1 {
		t.Fatalf("%d log lines, want exactly 1:\n%s", len(lines), logs.String())
	}
	l := lines[0]
	if l["component"] != "http" || l["route"] != "POST /things" || l["status"] != float64(202) || l["certname"] != "stagehand-code-manager" {
		t.Fatalf("log line = %v", l)
	}
	if _, ok := l["duration_ms"].(float64); !ok {
		t.Fatalf("duration_ms is missing or not a number: %v", l)
	}
	if strings.Contains(logs.String(), "super-secret-body") {
		t.Fatalf("the request body reached the log:\n%s", logs.String())
	}
}

// TestRouteIsThePatternNotThePath: the id never reaches a log route or a
// metric label (T-57.6.2-36).
func TestRouteIsThePatternNotThePath(t *testing.T) {
	h, logs, m := wrapMux(t)
	for _, id := range []string{"aaaa", "bbbb", "cccc"} {
		h.ServeHTTP(httptest.NewRecorder(), withCertname(httptest.NewRequest(http.MethodGet, "/things/"+id, nil), "x"))
	}
	for _, l := range logLines(t, logs) {
		if l["route"] != "GET /things/{id}" {
			t.Errorf("route = %v, want the pattern", l["route"])
		}
	}
	out := scrape(t, m)
	if !strings.Contains(out, `stagehand_deploy_agent_http_requests_total{route="GET /things/{id}",code="200"} 3`) {
		t.Fatalf("three requests are not one series:\n%s", out)
	}
	for _, id := range []string{"aaaa", "bbbb", "cccc"} {
		if strings.Contains(out, id) {
			t.Fatalf("path value %q reached the metrics", id)
		}
	}
}

func TestUnmatchedRoutes(t *testing.T) {
	h, logs, m := wrapMux(t)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/no/such/thing/at/all", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodDelete, "/things", nil)) // 405
	for _, l := range logLines(t, logs) {
		if l["route"] != "unmatched" {
			t.Errorf("route = %v, want unmatched", l["route"])
		}
	}
	out := scrape(t, m)
	for _, want := range []string{
		`stagehand_deploy_agent_http_requests_total{route="unmatched",code="404"} 1`,
		`stagehand_deploy_agent_http_requests_total{route="unmatched",code="405"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s\n%s", want, out)
		}
	}
	if strings.Contains(out, "no/such") {
		t.Fatal("an unmatched path reached the metrics")
	}
}

func TestPanicBecomes500AndIsLogged(t *testing.T) {
	h, logs, m := wrapMux(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withCertname(httptest.NewRequest(http.MethodGet, "/boom", nil), "stagehand-code-manager"))
	if rec.Code != http.StatusInternalServerError || rec.Body.String() != `{"error":"internal error"}` {
		t.Fatalf("got %d %q, want 500 {\"error\":\"internal error\"}", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q", ct)
	}
	var sawPanic bool
	for _, l := range logLines(t, logs) {
		if l["level"] == "ERROR" && strings.Contains(l["msg"].(string), "panic") {
			sawPanic = true
			if l["route"] != "GET /boom" || l["component"] != "http" {
				t.Errorf("panic line = %v", l)
			}
		}
	}
	if !sawPanic {
		t.Fatalf("the panic was not logged at ERROR:\n%s", logs.String())
	}
	if !strings.Contains(scrape(t, m), `route="GET /boom",code="500"} 1`) {
		t.Fatal("the 500 was not counted")
	}
	// The server carries on.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/things/ok", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("after a panic the next request = %d", rec.Code)
	}
}

// TestPanicAfterHeadersKeepsTheStatus: once the status is on the wire it
// cannot be changed to 500; the middleware must not write a second body.
func TestPanicAfterHeadersKeepsTheStatus(t *testing.T) {
	h, logs, _ := wrapMux(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/late", nil))
	if rec.Code != http.StatusTeapot || rec.Body.String() != "partial" {
		t.Fatalf("got %d %q, want the handler's own 418 partial", rec.Code, rec.Body.String())
	}
	if !strings.Contains(logs.String(), "panic") {
		t.Fatal("the late panic was not logged")
	}
}

func TestAbortHandlerPanicIsNotSwallowed(t *testing.T) {
	h := Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }),
		slog.New(slog.NewJSONHandler(io.Discard, nil)), nil)
	defer func() {
		if recover() != http.ErrAbortHandler {
			t.Fatal("http.ErrAbortHandler must propagate so net/http can drop the connection")
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}

func TestWrapWithoutMetricsOrCertificate(t *testing.T) {
	var buf bytes.Buffer
	h := Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }),
		slog.New(slog.NewJSONHandler(&buf, nil)), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != 204 {
		t.Fatalf("status = %d", rec.Code)
	}
	var l map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &l); err != nil || l["certname"] != "" {
		t.Fatalf("log line = %s (%v)", buf.String(), err)
	}
}
