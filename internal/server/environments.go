package server

// Stagehand: environment removal (D-09, D-10, D-20), new in the agent; the fork
// has no equivalent. Callers have already presented an allow-listed Puppet
// certificate in the TLS handshake; nothing here authorises anyone.
//
//	DELETE /api/v1/environments/{env}  202 with the queued removal; Location /api/v1/removals/{id}
//	                                   403 protected, 404 no such environment, 422 bad name, 503 busy
//	GET    /api/v1/removals/{id}       the removal record (the deploy record shape with "kind":"remove")
//
// A removal is polled here and never listed under /api/v1/deploys, so the
// pack's deploy calls and the golden corpus do not see it.

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/puppet-stagehand/stagehand-deploy-agent/internal/jobs"
)

// EnvironmentHandlers serves the environment-removal API.
type EnvironmentHandlers struct {
	runner    *jobs.Runner
	protected map[string]bool
	log       *slog.Logger
}

// NewEnvironmentHandlers builds the handlers around runner. Environments named
// in protected (deploys.protected_environments) are never removed. A nil log
// means slog.Default().
func NewEnvironmentHandlers(runner *jobs.Runner, protected []string, log *slog.Logger) *EnvironmentHandlers {
	if log == nil {
		log = slog.Default()
	}
	set := make(map[string]bool, len(protected))
	for _, name := range protected {
		set[name] = true
	}
	return &EnvironmentHandlers{runner: runner, protected: set, log: log.With("component", "environments")}
}

// remove queues the removal of one environment and answers 202 at once; the
// caller polls GET /api/v1/removals/{id}. Removal is always asynchronous.
func (h *EnvironmentHandlers) remove(w http.ResponseWriter, r *http.Request) {
	env := r.PathValue("env")
	if !jobs.ValidEnvironmentName(env) {
		unprocessable(w, "environment", "must match ^[a-z0-9_]+$")
		return
	}
	if h.protected[env] {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "environment " + env + " is protected", "field": "environment"})
		return
	}
	job, err := h.runner.NewRemoval(env)
	switch {
	case errors.Is(err, jobs.ErrEnvironmentNotFound):
		failure(w, http.StatusNotFound, "environment not found")
		return
	case errors.Is(err, jobs.ErrInvalidRequest):
		unprocessable(w, "environment", err.Error())
		return
	case err != nil:
		failure(w, http.StatusInternalServerError, "recording removal: "+err.Error())
		return
	}
	if err := h.runner.Start(job); err != nil {
		// The runner records it as failed so the id in the answer never reads
		// as queued (Runner.Refuse, PD-04).
		if refused, rerr := h.runner.Refuse(job, err); rerr != nil {
			h.log.Error("recording a refused removal", "job_id", refused.ID, "error", rerr.Error())
		}
		w.Header().Set("Retry-After", "30")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error(), "id": job.ID})
		return
	}
	h.log.Info("removal queued", "env", env, "job_id", job.ID)
	w.Header().Set("Location", "/api/v1/removals/"+job.ID)
	writeJSON(w, http.StatusAccepted, job)
}

// getRemoval answers one removal record by id. A deploy's id is not found
// here, as a removal's id is not found under /api/v1/deploys (PD-21).
func (h *EnvironmentHandlers) getRemoval(w http.ResponseWriter, r *http.Request) {
	job, err := h.runner.Store().Get(r.PathValue("id"))
	if errors.Is(err, jobs.ErrNotFound) || (err == nil && job.Kind != jobs.KindRemove) {
		failure(w, http.StatusNotFound, "removal not found")
		return
	}
	if err != nil {
		failure(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, job)
}
