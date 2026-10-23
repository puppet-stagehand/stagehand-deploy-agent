package server

// Stagehand: the request middleware for the mTLS port. It replaces gin.Logger
// and gin.Recovery from the fork. For every request it writes one structured
// log line (component=http, route, status, duration_ms, certname), counts the
// request in the metrics, and turns a handler panic into the fork's 500
// answer. It never logs a body, a query string or a path value: the route is
// the pattern the mux matched ("GET /api/v1/deploys/{id}"), or "unmatched".

import (
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/puppet-stagehand/stagehand-deploy-agent/internal/metrics"
)

// unmatchedRoute labels a request no pattern matched (a 404 or a 405 from the
// mux itself), so a scanner cannot mint a label per path.
const unmatchedRoute = "unmatched"

// statusWriter records the status code and whether the header was written.
type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status, w.wrote = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.status, w.wrote = http.StatusOK, true
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the real writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Flush passes through to the real writer when it can flush.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Wrap returns next with logging, metrics (when m is not nil) and panic
// recovery around it. next should be the ServeMux itself: the matched pattern
// is read from the request after the mux has served it, which is why Wrap
// must sit outside the mux, not inside a handler.
func Wrap(next http.Handler, log *slog.Logger, m *metrics.Registry) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "http")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		certname := peerCertname(r)
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec) // net/http's own way to drop a connection
				}
				log.Error("handler panic", "route", routeOf(r), "certname", certname,
					"panic", describePanic(rec), "stack", string(debug.Stack()))
				if !sw.wrote {
					writeJSON(sw, http.StatusInternalServerError, map[string]string{"error": "internal error"})
				}
			}
			d := time.Since(start)
			route := routeOf(r)
			if m != nil {
				m.ObserveRequest(route, sw.status, d)
			}
			log.Info("request", "route", route, "status", sw.status,
				"duration_ms", float64(d.Microseconds())/1000, "certname", certname)
		}()
		next.ServeHTTP(sw, r)
	})
}

// routeOf is the pattern the mux matched for r, or "unmatched". ServeMux sets
// r.Pattern on the request it was given, which is the one Wrap holds.
func routeOf(r *http.Request) string {
	if r.Pattern == "" {
		return unmatchedRoute
	}
	return r.Pattern
}

// peerCertname is the Subject CN of the client certificate the handshake
// accepted, or "" without one. The handshake already vetted it.
func peerCertname(r *http.Request) string {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return ""
	}
	return r.TLS.PeerCertificates[0].Subject.CommonName
}

func describePanic(v any) string {
	switch x := v.(type) {
	case error:
		return x.Error()
	case string:
		return x
	default:
		return fmt.Sprintf("panic value of type %T", v)
	}
}
