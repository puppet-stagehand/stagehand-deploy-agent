// Package ops is the plain-HTTP operations listener (D-05): /healthz, /readyz
// and /metrics, and nothing else. It has no authentication, so it binds
// loopback by default and carries no deploy route; the deploy API lives on the
// mTLS port and the mTLS port carries none of these.
package ops

import (
	"net"
	"net/http"
)

// NewMux serves exactly three routes: GET /healthz, GET /readyz and GET
// /metrics. metrics is the registry's handler; nil leaves /metrics answering
// 404. Anything else, including every /api path, falls to the mux's own 404.
func NewMux(checks *Checks, metrics http.Handler) *http.ServeMux {
	if metrics == nil {
		metrics = http.NotFoundHandler()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("GET /readyz", checks.readyz)
	mux.Handle("GET /metrics", metrics)
	return mux
}

// healthz answers whenever the process serves; it checks nothing else, so a
// supervisor can tell "hung" from "serving but not ready".
func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok"))
}

// Loopback reports whether addr (host:port) binds only a loopback interface:
// 127.0.0.0/8, ::1 or localhost. An empty host binds every interface and is
// not loopback.
func Loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
