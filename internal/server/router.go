package server

import "net/http"

// NewMux registers the deploy routes on one ServeMux with Go 1.22 method
// patterns. A conflicting pattern panics here, at registration, so the tracer
// test (which builds the real mux) fails a conflict before it can reach a
// primary. Unknown paths and wrong methods fall to ServeMux's own 404 and 405.
func NewMux(h *DeployHandlers) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/deploys", h.create)
	mux.HandleFunc("GET /api/v1/deploys", h.list)
	mux.HandleFunc("GET /api/v1/deploys/{id}", h.get)
	return mux
}
