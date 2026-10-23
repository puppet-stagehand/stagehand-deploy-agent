// Package ops is the plain-HTTP operations listener: /healthz, /readyz and
// /metrics, and nothing else (D-05).
package ops

import (
	"net/http"
	"time"
)

// CRLSource is what readiness needs to know about the revocation list.
type CRLSource interface {
	NextUpdate() time.Time
	Readable() error
}

// Checks decides readiness (D-07).
type Checks struct {
	EnginePath      string
	EnvironmentsDir string
	CertificatePath string
	CRL             CRLSource
	Now             func() time.Time
}

func (c *Checks) Failing() []string { return nil }
func (c *Checks) Ready() bool       { return true }
func (c *Checks) SetDraining()      {}

// CertNotAfter returns the NotAfter of the first certificate in the PEM file.
func CertNotAfter(path string) (time.Time, error) { return time.Time{}, nil }

// NewMux serves the three ops routes.
func NewMux(checks *Checks, metrics http.Handler) *http.ServeMux { return http.NewServeMux() }

// Loopback reports whether addr binds only the loopback interface.
func Loopback(addr string) bool { return true }
