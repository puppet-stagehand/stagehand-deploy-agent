package server

import (
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock is the clock the CRL reload interval is measured on.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// crlPEM is a CRL signed by ca listing the given certificates as revoked.
func (ca *testCA) crlPEM(t *testing.T, revoked ...*tls.Certificate) []byte {
	t.Helper()
	tmpl := &x509.RevocationList{
		Number:     big.NewInt(serial.Add(1)),
		ThisUpdate: time.Now().Add(-time.Minute),
		NextUpdate: time.Now().Add(24 * time.Hour),
	}
	for _, c := range revoked {
		tmpl.RevokedCertificateEntries = append(tmpl.RevokedCertificateEntries, x509.RevocationListEntry{
			SerialNumber: leafOf(t, c).SerialNumber, RevocationTime: time.Now().Add(-time.Minute),
		})
	}
	der, err := x509.CreateRevocationList(rand.Reader, tmpl, ca.cert, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "X509 CRL", Bytes: der})
}

func leafOf(t *testing.T, c *tls.Certificate) *x509.Certificate {
	t.Helper()
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return leaf
}

// serveWithClock starts the mTLS server for conf with an injected clock.
func serveWithClock(t *testing.T, f *mtlsFixture, clock func() time.Time) string {
	t.Helper()
	srv, err := newMTLSServerWithClock("127.0.0.1:0", f.tls, stubMux(), nil, clock)
	if err != nil {
		t.Fatal(err)
	}
	return serveOn(t, srv)
}

// healthOK makes one fresh connection (no keep-alive, no session resumption)
// and reports whether the handshake and request succeeded.
func healthOK(t *testing.T, f *mtlsFixture, url string, cert *tls.Certificate) bool {
	t.Helper()
	resp, err := f.client(t, cert).Get(url + "/health") // a new transport: a new handshake
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

func TestRevokedCertificateRefused(t *testing.T) {
	f := newMTLSFixture(t, "stagehand-code-manager")
	revoked := clientCert(t, f.ca, "stagehand-code-manager")
	other := clientCert(t, f.ca, "stagehand-code-manager") // same certname, a re-issued certificate
	crlPath := writeFile(t, f.dir, "crl.pem", f.ca.crlPEM(t, revoked))
	f.tls.CRL = crlPath
	clock := &fakeClock{t: time.Now()}
	url := serveWithClock(t, f, clock.Now)

	if healthOK(t, f, url, revoked) {
		t.Fatal("a revoked certificate completed the handshake")
	}
	if !healthOK(t, f, url, other) {
		t.Fatal("control: an unrevoked certificate was refused")
	}

	// The CA publishes a CRL without the serial (newer mtime). Within the
	// reload interval the old list still applies; after it, the new one does.
	if err := os.WriteFile(crlPath, f.ca.crlPEM(t), 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(crlPath, later, later); err != nil {
		t.Fatal(err)
	}
	clock.Advance(30 * time.Second)
	if healthOK(t, f, url, revoked) {
		t.Fatal("the CRL was reloaded before the reload interval")
	}
	clock.Advance(31 * time.Second)
	if !healthOK(t, f, url, revoked) {
		t.Fatal("the replaced CRL was not reloaded after the interval")
	}

	// Revoking again takes effect the same way.
	if err := os.WriteFile(crlPath, f.ca.crlPEM(t, revoked), 0o600); err != nil {
		t.Fatal(err)
	}
	later = later.Add(time.Minute)
	if err := os.Chtimes(crlPath, later, later); err != nil {
		t.Fatal(err)
	}
	clock.Advance(61 * time.Second)
	if healthOK(t, f, url, revoked) {
		t.Fatal("a re-revoked certificate completed the handshake")
	}
}

// A CRL that stops parsing on reload keeps the last good list in force.
func TestCRLReloadKeepsLastGoodList(t *testing.T) {
	f := newMTLSFixture(t, "stagehand-code-manager")
	revoked := clientCert(t, f.ca, "stagehand-code-manager")
	crlPath := writeFile(t, f.dir, "crl.pem", f.ca.crlPEM(t, revoked))
	f.tls.CRL = crlPath
	clock := &fakeClock{t: time.Now()}
	url := serveWithClock(t, f, clock.Now)

	if err := os.WriteFile(crlPath, []byte("-----BEGIN X509 CRL-----\ngarbage\n-----END X509 CRL-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	os.Chtimes(crlPath, later, later)
	clock.Advance(2 * time.Minute)
	if healthOK(t, f, url, revoked) {
		t.Fatal("an unparseable CRL on reload dropped the revocations")
	}
	if err := os.Remove(crlPath); err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Minute)
	if healthOK(t, f, url, revoked) {
		t.Fatal("a deleted CRL on reload dropped the revocations")
	}
}

func TestStartRefusesUnparseableCRL(t *testing.T) {
	foreign := newTestCA(t, "Some other CA")
	for name, content := range map[string][]byte{
		"garbage":        []byte("this is not a CRL\n"),
		"garbage PEM":    []byte("-----BEGIN X509 CRL-----\nAAAA\n-----END X509 CRL-----\n"),
		"foreign issuer": foreign.crlPEM(t),
		"empty":          {},
	} {
		t.Run(name, func(t *testing.T) {
			f := newMTLSFixture(t, "stagehand-code-manager")
			port := freePort(t)
			f.tls.CRL = writeFile(t, f.dir, "crl.pem", content)
			srv, err := NewMTLSServer(fmt.Sprintf(":%d", port), f.tls, stubMux(), nil)
			if err == nil || srv != nil || !strings.Contains(err.Error(), "server.tls.crl") {
				t.Fatalf("NewMTLSServer = %v, %v; want an error naming server.tls.crl", srv, err)
			}
			assertNotListening(t, port)
		})
	}
	t.Run("missing file", func(t *testing.T) {
		f := newMTLSFixture(t, "stagehand-code-manager")
		f.tls.CRL = f.dir + "/no-such-crl.pem"
		if _, err := NewMTLSServer("127.0.0.1:0", f.tls, stubMux(), nil); err == nil || !strings.Contains(err.Error(), "server.tls.crl") {
			t.Fatalf("err = %v, want one naming server.tls.crl", err)
		}
	})
}

// TestCRLNextUpdateAndReadable: readiness asks the CRL set when its list
// expires and whether the file can still be read (D-07).
func TestCRLNextUpdateAndReadable(t *testing.T) {
	f := newMTLSFixture(t, "stagehand-code-manager")
	crlPath := writeFile(t, f.dir, "crl.pem", f.ca.crlPEM(t))
	caPEM := f.ca.pem
	clock := &fakeClock{t: time.Now()}
	c, err := loadCRL(crlPath, caPEM, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Until(c.NextUpdate()); d < 23*time.Hour || d > 25*time.Hour {
		t.Fatalf("NextUpdate is %v from now, want about 24h", d)
	}
	if err := c.Readable(); err != nil {
		t.Fatalf("Readable = %v for a good file", err)
	}

	// A broken replacement: the last good list stays in force, but the file is
	// not readable as a CRL, and readiness must say so.
	if err := os.WriteFile(crlPath, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(crlPath, later, later); err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Minute)
	if err := c.Readable(); err == nil {
		t.Fatal("Readable = nil for a file that no longer parses")
	}
	if d := time.Until(c.NextUpdate()); d < 23*time.Hour {
		t.Fatalf("NextUpdate lost the last good list: %v", d)
	}

	// A good file again clears it.
	if err := os.WriteFile(crlPath, f.ca.crlPEM(t), 0o600); err != nil {
		t.Fatal(err)
	}
	later = later.Add(time.Hour)
	if err := os.Chtimes(crlPath, later, later); err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Minute)
	if err := c.Readable(); err != nil {
		t.Fatalf("Readable = %v after a good file replaced the bad one", err)
	}

	// A deleted file is unreadable.
	if err := os.Remove(crlPath); err != nil {
		t.Fatal(err)
	}
	if err := c.Readable(); err == nil {
		t.Fatal("Readable = nil for a deleted file")
	}
}
