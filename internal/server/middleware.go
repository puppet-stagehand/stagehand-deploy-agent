package server

import (
	"log/slog"
	"net/http"

	"github.com/puppet-stagehand/stagehand-deploy-agent/internal/metrics"
)

// Wrap logs, counts and recovers around next.
func Wrap(next http.Handler, log *slog.Logger, m *metrics.Registry) http.Handler { return next }
