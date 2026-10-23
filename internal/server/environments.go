package server

import (
	"log/slog"
	"net/http"

	"github.com/puppet-stagehand/stagehand-deploy-agent/internal/jobs"
)

// EnvironmentHandlers is a stub until the GREEN commit.
type EnvironmentHandlers struct{}

// NewEnvironmentHandlers is a stub until the GREEN commit.
func NewEnvironmentHandlers(runner *jobs.Runner, protected []string, log *slog.Logger) *EnvironmentHandlers {
	return &EnvironmentHandlers{}
}

func (h *EnvironmentHandlers) remove(w http.ResponseWriter, r *http.Request) {
	failure(w, http.StatusNotImplemented, "not implemented")
}

func (h *EnvironmentHandlers) getRemoval(w http.ResponseWriter, r *http.Request) {
	failure(w, http.StatusNotImplemented, "not implemented")
}
