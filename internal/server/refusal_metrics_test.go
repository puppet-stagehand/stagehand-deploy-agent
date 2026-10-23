package server

// The handshake reports why it refused a caller (tls_refusals_total{reason}).

import (
	"crypto/tls"
	"sync"
	"testing"
)

type refusalLog struct {
	mu      sync.Mutex
	reasons []string
}

func (l *refusalLog) add(r string) { l.mu.Lock(); l.reasons = append(l.reasons, r); l.mu.Unlock() }
func (l *refusalLog) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.reasons...)
}

func TestTLSRefusalCounterByReason(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(t *testing.T, f *mtlsFixture) *tls.Certificate
		reason string
	}{
		{"certname not listed", func(t *testing.T, f *mtlsFixture) *tls.Certificate {
			return clientCert(t, f.ca, "intruder")
		}, "certname"},
		{"revoked", func(t *testing.T, f *mtlsFixture) *tls.Certificate {
			c := clientCert(t, f.ca, "stagehand-code-manager")
			f.tls.CRL = writeFile(t, f.dir, "crl.pem", f.ca.crlPEM(t, c))
			return c
		}, "revoked"},
		{"fingerprint not pinned", func(t *testing.T, f *mtlsFixture) *tls.Certificate {
			c := clientCert(t, f.ca, "stagehand-code-manager")
			other := clientCert(t, f.ca, "stagehand-code-manager")
			f.tls.AllowedFingerprints = []string{fingerprint(leafOf(t, other))}
			return c
		}, "fingerprint"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newMTLSFixture(t, "stagehand-code-manager")
			cert := tc.setup(t, f)
			var got refusalLog
			f.tls.RefusalCounter = got.add
			url := f.serve(t, stubMux())
			if healthOK(t, f, url, cert) {
				t.Fatal("the certificate was accepted")
			}
			if r := got.get(); len(r) != 1 || r[0] != tc.reason {
				t.Fatalf("refusals = %v, want [%s]", r, tc.reason)
			}
		})
	}
}

// TestTLSAcceptedCertificateIsNotCounted: control.
func TestTLSAcceptedCertificateIsNotCounted(t *testing.T) {
	f := newMTLSFixture(t, "stagehand-code-manager")
	var got refusalLog
	f.tls.RefusalCounter = got.add
	url := f.serve(t, stubMux())
	if !healthOK(t, f, url, clientCert(t, f.ca, "stagehand-code-manager")) {
		t.Fatal("control: the allowed certificate was refused")
	}
	if r := got.get(); len(r) != 0 {
		t.Fatalf("refusals = %v, want none", r)
	}
}
