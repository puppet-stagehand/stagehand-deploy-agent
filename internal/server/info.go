package server

// Stagehand: GET /api/v1/info (D-06, D-19), new in the agent. It sits on the
// mTLS port behind the same handshake allow-list as every deploy route, so
// there is nothing to authorise here: a caller who can ask is a caller who can
// deploy (T-57.6.2-35). The seven field names and their order are a published
// contract from v1.0.0; renaming one is a 2.0.0 change.

import (
	"net/http"
	"time"
)

// InfoOptions are what the document reports.
type InfoOptions struct {
	// Version is the agent's version, EngineName "r10k" or "g10k".
	Version, EngineName string
	// EngineVersion is the engine's own version line, captured once at start;
	// empty when it did not answer, which the document reports as null.
	EngineVersion string
	// EnvironmentsDir is deploys.basedir.
	EnvironmentsDir string
	// CertNotAfter is the NotAfter of the server certificate being served.
	CertNotAfter time.Time
	// CRLNextUpdate reports the CRL's next update when one is configured; nil,
	// or a zero time, is reported as null. It is asked on every request, so a
	// reloaded CRL shows at once.
	CRLNextUpdate func() time.Time
	// ConfigSource is the configuration file that was loaded: the agent's own
	// path, or the legacy voxpupuli path when the fallback was used.
	ConfigSource string
}

// infoResponse is the document, fields in the order D-19 fixed.
type infoResponse struct {
	Version         string  `json:"version"`
	Engine          string  `json:"engine"`
	EngineVersion   *string `json:"engine_version"`
	EnvironmentsDir string  `json:"environments_dir"`
	CertNotAfter    string  `json:"cert_not_after"`
	CRLNextUpdate   *string `json:"crl_next_update"`
	ConfigSource    string  `json:"config_source"`
}

// InfoHandler serves GET /api/v1/info.
type InfoHandler struct {
	opts InfoOptions
	cert string // RFC 3339, UTC: fixed for the life of the process
	eng  *string
}

// NewInfoHandler builds the handler once, at start.
func NewInfoHandler(o InfoOptions) *InfoHandler {
	h := &InfoHandler{opts: o, cert: o.CertNotAfter.UTC().Format(time.RFC3339)}
	if o.EngineVersion != "" {
		v := o.EngineVersion
		h.eng = &v
	}
	return h
}

func (h *InfoHandler) get(w http.ResponseWriter, _ *http.Request) {
	doc := infoResponse{
		Version:         h.opts.Version,
		Engine:          h.opts.EngineName,
		EngineVersion:   h.eng,
		EnvironmentsDir: h.opts.EnvironmentsDir,
		CertNotAfter:    h.cert,
		ConfigSource:    h.opts.ConfigSource,
	}
	if h.opts.CRLNextUpdate != nil {
		if next := h.opts.CRLNextUpdate(); !next.IsZero() {
			s := next.UTC().Format(time.RFC3339)
			doc.CRLNextUpdate = &s
		}
	}
	writeJSON(w, http.StatusOK, doc)
}
