package server

// Stagehand: the mTLS-only server (D-10). The whole server requires a client
// certificate that chains to server.tls.client_ca (the estate's Puppet CA) and
// whose Subject CN, the Puppet certname, is listed in
// server.tls.allowed_certnames. Optionally the certificate must not be revoked
// by the CA's CRL (server.tls.crl, crl.go) and its SHA-256 fingerprint must be
// pinned (server.tls.allowed_fingerprints). Every check happens in the TLS
// handshake, before any handler runs, so a refused caller sees a TLS alert and
// never an HTTP status: the Code Manager pack classifies that alert as
// "credential refused".
//
// Ported from the webhook-go fork (server/tls.go); logrus became log/slog and
// config.Config became TLSConfig.

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/puppet-stagehand/stagehand-deploy-agent/internal/config"
)

// TLSConfig is the mTLS material and the caller allow-list.
type TLSConfig struct {
	Certificate, Key, ClientCA, CRL string
	AllowedCertnames                []string
	AllowedFingerprints             []string
}

var (
	errCertnameNotAllowed = errors.New("client certificate certname is not in server.tls.allowed_certnames")
	errCertRevoked        = errors.New("client certificate is revoked by server.tls.crl")
	errCertNotPinned      = errors.New("client certificate fingerprint is not in server.tls.allowed_fingerprints")
)

// NormalizeFingerprint returns a SHA-256 certificate fingerprint in the form
// Puppet prints it (upper-case hex pairs joined by colons) and whether s is
// one. Lower-case input is accepted. The definition is shared with
// configuration validation.
func NormalizeFingerprint(fp string) (string, bool) { return config.NormalizeFingerprint(fp) }

// NewMTLSServer returns an http.Server for handler that enforces mutual TLS.
// It loads and checks every file it needs, so a bad configuration fails here
// instead of after the listener opens. Serve it with ListenAndServeTLS("", "").
func NewMTLSServer(addr string, cfg TLSConfig, handler http.Handler, log *slog.Logger) (*http.Server, error) {
	return newMTLSServerWithClock(addr, cfg, handler, log, time.Now)
}

// fingerprint is the SHA-256 of a certificate's DER, as Puppet prints it.
func fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	var b strings.Builder
	for i, v := range sum {
		if i > 0 {
			b.WriteByte(':')
		}
		fmt.Fprintf(&b, "%02X", v)
	}
	return b.String()
}

// validate refuses a TLS configuration that would run the deploy server
// without its protections.
func (t TLSConfig) validate() error {
	if t.ClientCA == "" {
		return errors.New("server.tls.client_ca is not set")
	}
	if len(t.AllowedCertnames) == 0 {
		return fmt.Errorf("server.tls.allowed_certnames must list at least one certname when server.tls.client_ca is set")
	}
	for i, name := range t.AllowedCertnames {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("server.tls.allowed_certnames[%d] is empty", i)
		}
	}
	for i, fp := range t.AllowedFingerprints {
		if _, ok := NormalizeFingerprint(fp); !ok {
			return fmt.Errorf("server.tls.allowed_fingerprints[%d] must be a SHA-256 fingerprint as 32 colon-separated hex pairs (AB:CD:...), got %q", i, fp)
		}
	}
	if t.Certificate == "" || t.Key == "" {
		return fmt.Errorf("server.tls.certificate and server.tls.key are required when server.tls.client_ca is set")
	}
	return nil
}

// newMTLSServerWithClock is NewMTLSServer with the clock the CRL reload
// interval is measured on (tests inject one).
func newMTLSServerWithClock(addr string, t TLSConfig, handler http.Handler, log *slog.Logger, now func() time.Time) (*http.Server, error) {
	if log == nil {
		log = slog.Default()
	}
	if err := t.validate(); err != nil {
		return nil, err
	}

	caPEM, err := os.ReadFile(t.ClientCA)
	if err != nil {
		return nil, fmt.Errorf("server.tls.client_ca: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("server.tls.client_ca: no PEM certificates found in %s", t.ClientCA)
	}

	pair, err := tls.LoadX509KeyPair(t.Certificate, t.Key)
	if err != nil {
		return nil, fmt.Errorf("server.tls.certificate and server.tls.key: %w", err)
	}

	allowed := make(map[string]bool, len(t.AllowedCertnames))
	for _, name := range t.AllowedCertnames {
		allowed[name] = true
	}

	var crl *crlSet
	if t.CRL != "" {
		if crl, err = loadCRL(t.CRL, caPEM, now); err != nil {
			return nil, fmt.Errorf("server.tls.crl: %w", err)
		}
		crl.log = log.With("component", "crl")
	}
	pins := make(map[string]bool, len(t.AllowedFingerprints))
	for i, fp := range t.AllowedFingerprints {
		norm, ok := NormalizeFingerprint(fp)
		if !ok { // validate already refused it; kept for direct callers.
			return nil, fmt.Errorf("server.tls.allowed_fingerprints[%d] is not a SHA-256 fingerprint", i)
		}
		pins[norm] = true
	}
	tlog := log.With("component", "tls")

	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{pair},
			ClientAuth:   tls.RequireAndVerifyClientCert,
			ClientCAs:    pool,
			// VerifyConnection, not VerifyPeerCertificate: crypto/tls skips
			// VerifyPeerCertificate when a session is resumed, but calls
			// VerifyConnection on every handshake, resumed ones included, with
			// the chains verified against ClientCAs when the session began.
			// Every per-certificate check (the allow-list, the CRL and the
			// fingerprint pin) must live in this callback, or a resumed
			// session skips it (TestMTLSAllowListEnforcedOnResumedSession).
			VerifyConnection: func(cs tls.ConnectionState) error {
				if len(cs.VerifiedChains) == 0 || len(cs.VerifiedChains[0]) == 0 {
					return errCertnameNotAllowed
				}
				chain := cs.VerifiedChains[0]
				leaf := chain[0]
				cn := leaf.Subject.CommonName
				// Log the certname and serial only; never the certificate.
				if !allowed[cn] {
					tlog.Warn("refused client certificate: certname not allowed", "certname", cn)
					return errCertnameNotAllowed
				}
				if crl != nil {
					// The leaf and any intermediate CA; the root has no issuer CRL.
					for _, c := range chain[:max(1, len(chain)-1)] {
						if crl.Revoked(c) {
							tlog.Warn("refused client certificate: revoked", "certname", cn, "serial", c.SerialNumber.String())
							return errCertRevoked
						}
					}
				}
				if len(pins) > 0 && !pins[fingerprint(leaf)] {
					tlog.Warn("refused client certificate: fingerprint not pinned", "certname", cn)
					return errCertNotPinned
				}
				return nil
			},
		},
	}, nil
}
