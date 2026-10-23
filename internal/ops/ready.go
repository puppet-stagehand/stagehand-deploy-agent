package ops

// Readiness (D-07, PD-34). /readyz is strict about hard failures and silent
// about an expiry that is only coming up: that is the job of the
// *_days_until_expiry gauges, which an alert can watch. The check names are
// fixed and no path, error text or certname ever reaches the response, so an
// unauthenticated reader learns which check failed and nothing about the host.

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"os"
	"sync/atomic"
	"time"
)

// The names /readyz can report.
const (
	CheckEngine          = "engine"
	CheckEnvironmentsDir = "environments_dir"
	CheckCertificate     = "certificate"
	CheckCRL             = "crl"
	// CheckDraining is reported once shutdown has started (PD-36): the agent
	// is still answering, but a probe should stop sending it work.
	CheckDraining = "draining"
)

// CRLSource is what readiness needs to know about the revocation list the
// mTLS server enforces.
type CRLSource interface {
	// NextUpdate is when the list in force expires; zero if unknown.
	NextUpdate() time.Time
	// Readable reports whether the file can still be read as a CRL.
	Readable() error
}

// Checks decides readiness. Build it once with &Checks{...}; it must not be
// copied after first use.
type Checks struct {
	// EnginePath is engine.command_path: a regular file this process may run.
	EnginePath string
	// EnvironmentsDir is deploys.basedir: a directory this process may write.
	EnvironmentsDir string
	// CertificatePath is server.tls.certificate, read again on every check.
	CertificatePath string
	// CRL is nil when no CRL is configured.
	CRL CRLSource
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	draining atomic.Bool
}

func (c *Checks) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// SetDraining marks shutdown as started: from now on the agent is not ready.
func (c *Checks) SetDraining() { c.draining.Store(true) }

// Failing returns the names of the checks that fail now, in the fixed order
// engine, environments_dir, certificate, crl, then draining. It is never nil.
func (c *Checks) Failing() []string {
	failing := []string{}
	if executable(c.EnginePath) != nil {
		failing = append(failing, CheckEngine)
	}
	if writableDir(c.EnvironmentsDir) != nil {
		failing = append(failing, CheckEnvironmentsDir)
	}
	if notAfter, err := CertNotAfter(c.CertificatePath); err != nil || !notAfter.After(c.now()) {
		failing = append(failing, CheckCertificate)
	}
	if c.CRL != nil {
		next := c.CRL.NextUpdate()
		if c.CRL.Readable() != nil || (!next.IsZero() && !next.After(c.now())) {
			failing = append(failing, CheckCRL)
		}
	}
	if c.draining.Load() {
		failing = append(failing, CheckDraining)
	}
	return failing
}

// Ready reports whether no check fails.
func (c *Checks) Ready() bool { return len(c.Failing()) == 0 }

type readyResponse struct {
	Ready   bool     `json:"ready"`
	Failing []string `json:"failing"`
}

func (c *Checks) readyz(w http.ResponseWriter, _ *http.Request) {
	failing := c.Failing()
	code := http.StatusOK
	if len(failing) > 0 {
		code = http.StatusServiceUnavailable
	}
	b, err := json.Marshal(readyResponse{Ready: len(failing) == 0, Failing: failing})
	if err != nil { // cannot happen for this shape
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = w.Write(b)
}

// executable reports why path is not an engine this process can run.
func executable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	return access(path, modeExec)
}

// writableDir reports why dir is not a directory this process can write. On
// Linux access(2) reports EROFS for a read-only mount, which is what
// ProtectSystem=strict without a ReadWritePaths entry produces.
func writableDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("not a directory")
	}
	return access(dir, modeWrite)
}

// CertNotAfter returns the NotAfter of the first certificate in the PEM file at
// path, the leaf by the convention TLS chain files follow.
func CertNotAfter(path string) (time.Time, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, err
	}
	for rest := b; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return time.Time{}, errors.New("no certificate in the file")
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return time.Time{}, err
		}
		return cert.NotAfter, nil
	}
}
