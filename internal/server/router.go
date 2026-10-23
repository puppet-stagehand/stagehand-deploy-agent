package server

import "net/http"

// NewMux registers the deploy routes, and the environment routes when e is
// not nil, on one ServeMux with Go 1.22 method patterns. A conflicting pattern
// panics here, at registration, so the tracer test and the environments tests
// (which build the real mux) fail a conflict before it can reach a primary.
// Unknown paths and wrong methods fall to ServeMux's own 404 and 405.
func NewMux(h *DeployHandlers, e *EnvironmentHandlers) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/deploys", h.create)
	mux.HandleFunc("GET /api/v1/deploys", h.list)
	mux.HandleFunc("GET /api/v1/deploys/{id}", h.get)
	if e != nil {
		mux.HandleFunc("DELETE /api/v1/environments/{env}", e.remove)
		mux.HandleFunc("GET /api/v1/removals/{id}", e.getRemoval)
	}
	return mux
}
