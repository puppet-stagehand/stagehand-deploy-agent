package server

// Test fixtures shared by the server tests: a throwaway CA standing in for the
// Puppet CA, a server certificate, client certificates, and an mTLS server on a
// loopback port. Ported from the fork's server/tls_test.go.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/puppet-stagehand/stagehand-deploy-agent/internal/jobs/fakeengine"
)

func TestMain(m *testing.M) {
	fakeengine.MaybeRun() // becomes the fake r10k when the fixture env var is set
	os.Exit(m.Run())
}

// testCA is a throwaway certificate authority standing in for a Puppet CA.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

var serial atomic.Int64

func newTestCA(t *testing.T, name string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(serial.Add(1)),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issue signs a leaf the way the Puppet CA does: CN = certname, and both
// serverAuth and clientAuth extended key usages.
func (ca *testCA) issue(t *testing.T, cn string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial.Add(1)),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder})
}

func writeFile(t *testing.T, dir, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// mtlsFixture is a CA, a server certificate and the TLS settings that use them.
type mtlsFixture struct {
	ca       *testCA
	dir      string
	tls      TLSConfig
	stateDir string
}

func newMTLSFixture(t *testing.T, allowed ...string) *mtlsFixture {
	t.Helper()
	dir := t.TempDir()
	ca := newTestCA(t, "Puppet CA: test")
	sc, sk := ca.issue(t, "puppet.example.test")
	return &mtlsFixture{
		ca:  ca,
		dir: dir,
		tls: TLSConfig{
			Certificate:      writeFile(t, dir, "server.pem", sc),
			Key:              writeFile(t, dir, "server.key", sk),
			ClientCA:         writeFile(t, dir, "ca.pem", ca.pem),
			AllowedCertnames: allowed,
		},
		stateDir: t.TempDir(),
	}
}

// serve starts NewMTLSServer on a loopback port and returns its base URL.
func (f *mtlsFixture) serve(t *testing.T, h http.Handler) string {
	t.Helper()
	srv, err := NewMTLSServer("127.0.0.1:0", f.tls, h, nil)
	if err != nil {
		t.Fatal(err)
	}
	return serveOn(t, srv)
}

// serveOn serves srv on a loopback listener and returns its base URL.
func serveOn(t *testing.T, srv *http.Server) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.ServeTLS(ln, "", "")
	t.Cleanup(func() { srv.Close() })
	return "https://" + ln.Addr().String()
}

// client returns an HTTPS client that trusts the fixture CA and presents the
// given client certificate (nil for none).
func (f *mtlsFixture) client(t *testing.T, cert *tls.Certificate) *http.Client {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(f.ca.pem)
	tc := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	if cert != nil {
		tc.Certificates = []tls.Certificate{*cert}
	}
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: tc}}
}

func clientCert(t *testing.T, ca *testCA, cn string) *tls.Certificate {
	t.Helper()
	c, k := ca.issue(t, cn)
	pair, err := tls.X509KeyPair(c, k)
	if err != nil {
		t.Fatal(err)
	}
	return &pair
}

// stubMux answers 200 {"message":"running"} on /health. The handshake tests
// only need some handler behind the TLS layer, and a counter in front of it
// proves a refused caller never reached it.
func stubMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"message": "running"})
	})
	return mux
}

// countingHandler counts how many requests reached a handler.
type countingHandler struct {
	n    atomic.Int64
	next http.Handler
}

func (h *countingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.n.Add(1)
	h.next.ServeHTTP(w, r)
}

// freePort returns a loopback port nothing is listening on.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

// assertNotListening proves a refused start never opened its listener.
func assertNotListening(t *testing.T, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		t.Fatalf("port %d is in use after a refused start: %v", port, err)
	}
	ln.Close()
}
