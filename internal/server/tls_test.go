package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/puppet-stagehand/stagehand-deploy-agent/internal/jobs"
)

func TestMTLSAllowsListedCertname(t *testing.T) {
	f := newMTLSFixture(t, "stagehand-code-manager")
	h := &countingHandler{next: stubMux()}
	url := f.serve(t, h)

	resp, err := f.client(t, clientCert(t, f.ca, "stagehand-code-manager")).Get(url + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if h.n.Load() != 1 {
		t.Fatalf("handler ran %d times", h.n.Load())
	}
}

func TestMTLSRefusesUnlistedOrForeign(t *testing.T) {
	f := newMTLSFixture(t, "stagehand-code-manager")
	h := &countingHandler{next: stubMux()}
	url := f.serve(t, h)
	foreign := newTestCA(t, "Some other CA")

	cases := map[string]*tls.Certificate{
		"unlisted certname":     clientCert(t, f.ca, "intruder"),
		"listed CN, foreign CA": clientCert(t, foreign, "stagehand-code-manager"),
		"no client certificate": nil,
	}
	for name, cert := range cases {
		t.Run(name, func(t *testing.T) {
			resp, err := f.client(t, cert).Get(url + "/health")
			if err == nil {
				resp.Body.Close()
				t.Fatalf("request succeeded with status %d", resp.StatusCode)
			}
			if !strings.Contains(err.Error(), "tls") && !strings.Contains(err.Error(), "EOF") &&
				!strings.Contains(err.Error(), "reset") {
				t.Fatalf("unexpected error kind: %v", err)
			}
		})
	}
	if h.n.Load() != 0 {
		t.Fatalf("handler ran %d times, want 0", h.n.Load())
	}
}

func TestNewMTLSServerRejectsEmptyCAFile(t *testing.T) {
	f := newMTLSFixture(t, "stagehand-code-manager")
	f.tls.ClientCA = writeFile(t, f.dir, "empty.pem", []byte("not a certificate\n"))
	if _, err := NewMTLSServer("127.0.0.1:0", f.tls, stubMux(), nil); err == nil || !strings.Contains(err.Error(), "server.tls.client_ca") {
		t.Fatalf("err = %v, want one naming server.tls.client_ca", err)
	}
}

func TestStartRefusesClientCAWithoutAllowList(t *testing.T) {
	for name, allowed := range map[string][]string{"empty list": nil, "blank entry": {" "}} {
		t.Run(name, func(t *testing.T) {
			f := newMTLSFixture(t, allowed...)
			port := freePort(t)
			srv, err := NewMTLSServer(fmt.Sprintf(":%d", port), f.tls, stubMux(), nil)
			if err == nil || srv != nil || !strings.Contains(err.Error(), "server.tls.allowed_certnames") {
				t.Fatalf("NewMTLSServer = %v, %v; want an error naming server.tls.allowed_certnames", srv, err)
			}
			assertNotListening(t, port)
		})
	}
}

func TestStartRefusesClientCAWithoutServerCert(t *testing.T) {
	for name, clear := range map[string]func(*TLSConfig){
		"no certificate": func(c *TLSConfig) { c.Certificate = "" },
		"no key":         func(c *TLSConfig) { c.Key = "" },
	} {
		t.Run(name, func(t *testing.T) {
			f := newMTLSFixture(t, "stagehand-code-manager")
			port := freePort(t)
			clear(&f.tls)
			srv, err := NewMTLSServer(fmt.Sprintf(":%d", port), f.tls, stubMux(), nil)
			if err == nil || srv != nil ||
				!strings.Contains(err.Error(), "server.tls.certificate and server.tls.key are required when server.tls.client_ca is set") {
				t.Fatalf("NewMTLSServer = %v, %v; want the missing server certificate named", srv, err)
			}
			assertNotListening(t, port)
		})
	}
}

// The fork served upstream's webhook routes next to the deploy API, and
// switched them off in mTLS mode. The agent has no such routes at all: the
// mTLS port carries the deploy surface alone (D-05), so the legacy routes and
// the ops routes answer 404 for an allow-listed caller.
func TestLegacyRoutesAbsentInMTLSMode(t *testing.T) {
	f := newMTLSFixture(t, "stagehand-code-manager")
	runner, err := jobs.NewRunner(jobs.Options{
		Engine:      jobs.R10kEngine(func(env string) []string { return []string{"/nonexistent/r10k", env} }),
		Basedir:     t.TempDir(),
		Timeout:     time.Minute,
		OutputLimit: 1024,
		Store:       jobs.NewMemoryStore(10),
	})
	if err != nil {
		t.Fatal(err)
	}
	url := f.serve(t, NewMux(NewDeployHandlers(runner, nil)))
	c := f.client(t, clientCert(t, f.ca, "stagehand-code-manager"))

	body := `{"ref":"refs/heads/simple-tag","repository":{"name":"puppet-control"}}`
	for _, route := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/r10k/environment"},
		{http.MethodPost, "/api/v1/r10k/module"},
		{http.MethodGet, "/api/v1/queue"},
		{http.MethodGet, "/health"},
		{http.MethodGet, "/metrics"},
	} {
		req, _ := http.NewRequest(route.method, url+route.path, strings.NewReader(body))
		req.Header.Set("X-GitHub-Event", "push")
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s %s status = %d on the mTLS port, want 404", route.method, route.path, resp.StatusCode)
		}
	}
}

// Go's crypto/tls skips VerifyPeerCertificate when a session is resumed, so a
// check placed there would let a resumed session through. Two servers share
// session-ticket keys; the client gets a ticket from the first, then resumes
// against the second. The positive control proves resumption really happens;
// the negative case proves the allow-list is enforced on a resumed session.
func TestMTLSAllowListEnforcedOnResumedSession(t *testing.T) {
	f := newMTLSFixture(t, "stagehand-code-manager")
	cert := clientCert(t, f.ca, "stagehand-code-manager")
	var ticketKey [32]byte
	if _, err := rand.Read(ticketKey[:]); err != nil {
		t.Fatal(err)
	}

	serveWith := func(t *testing.T, mutate func(*TLSConfig)) string {
		conf := f.tls
		if mutate != nil {
			mutate(&conf)
		}
		srv, err := NewMTLSServer("127.0.0.1:0", conf, stubMux(), nil)
		if err != nil {
			t.Fatal(err)
		}
		srv.TLSConfig.SetSessionTicketKeys([][32]byte{ticketKey})
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go srv.ServeTLS(ln, "", "")
		t.Cleanup(func() { srv.Close() })
		return "https://" + ln.Addr().String()
	}
	listing := func(names ...string) func(*TLSConfig) {
		return func(c *TLSConfig) { c.AllowedCertnames = names }
	}

	newClient := func(cache tls.ClientSessionCache) *http.Client {
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(f.ca.pem)
		return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
			DisableKeepAlives: true,
			TLSClientConfig: &tls.Config{
				RootCAs: pool, MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{*cert},
				ServerName:         "localhost", // one session-cache key across both servers
				ClientSessionCache: cache,
			},
		}}
	}

	get := func(c *http.Client, url string) (*http.Response, error) {
		resp, err := c.Get(url + "/health")
		if err == nil {
			io.Copy(io.Discard, resp.Body) // reading lets the client store the TLS 1.3 ticket
			resp.Body.Close()
		}
		return resp, err
	}

	t.Run("control: a resumed session is accepted when listed", func(t *testing.T) {
		cache := tls.NewLRUClientSessionCache(4)
		c := newClient(cache)
		if _, err := get(c, serveWith(t, nil)); err != nil {
			t.Fatal(err)
		}
		resp, err := get(c, serveWith(t, nil))
		if err != nil {
			t.Fatal(err)
		}
		if !resp.TLS.DidResume {
			t.Fatal("second connection did not resume; the negative case would prove nothing")
		}
	})

	t.Run("a resumed session is refused when no longer listed", func(t *testing.T) {
		cache := tls.NewLRUClientSessionCache(4)
		c := newClient(cache)
		if _, err := get(c, serveWith(t, nil)); err != nil {
			t.Fatal(err)
		}
		resp, err := get(c, serveWith(t, listing("someone-else")))
		if err == nil {
			t.Fatalf("resumed session accepted (DidResume=%v, status %d)", resp.TLS.DidResume, resp.StatusCode)
		}
	})

	// Plan 57.6-06: the CRL and the fingerprint pin are per-certificate
	// checks too, so they must hold on a resumed session.
	t.Run("a resumed session is refused once the certificate is revoked", func(t *testing.T) {
		crl := writeFile(t, t.TempDir(), "crl.pem", f.ca.crlPEM(t, cert))
		c := newClient(tls.NewLRUClientSessionCache(4))
		if _, err := get(c, serveWith(t, nil)); err != nil {
			t.Fatal(err)
		}
		resp, err := get(c, serveWith(t, func(conf *TLSConfig) { conf.CRL = crl }))
		if err == nil {
			t.Fatalf("resumed session accepted with a revoked certificate (DidResume=%v, status %d)", resp.TLS.DidResume, resp.StatusCode)
		}
	})

	t.Run("a resumed session is refused when the certificate is not pinned", func(t *testing.T) {
		other := fingerprintOf(t, clientCert(t, f.ca, "stagehand-code-manager"))
		c := newClient(tls.NewLRUClientSessionCache(4))
		if _, err := get(c, serveWith(t, nil)); err != nil {
			t.Fatal(err)
		}
		resp, err := get(c, serveWith(t, func(conf *TLSConfig) { conf.AllowedFingerprints = []string{other} }))
		if err == nil {
			t.Fatalf("resumed session accepted with an unpinned certificate (DidResume=%v, status %d)", resp.TLS.DidResume, resp.StatusCode)
		}
	})
}

// fingerprintOf is the leaf's SHA-256 fingerprint as Puppet prints it:
// upper-case hex pairs joined by colons.
func fingerprintOf(t *testing.T, c *tls.Certificate) string {
	t.Helper()
	sum := sha256.Sum256(c.Certificate[0])
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}

func TestFingerprintPin(t *testing.T) {
	f := newMTLSFixture(t, "stagehand-code-manager")
	pinned := clientCert(t, f.ca, "stagehand-code-manager")
	unpinned := clientCert(t, f.ca, "stagehand-code-manager") // same certname, another key

	t.Run("another certificate's fingerprint refuses the listed certname", func(t *testing.T) {
		f.tls.AllowedFingerprints = []string{fingerprintOf(t, unpinned)}
		url := f.serve(t, stubMux())
		if healthOK(t, f, url, pinned) {
			t.Fatal("an unpinned certificate completed the handshake")
		}
	})
	t.Run("its own fingerprint admits it", func(t *testing.T) {
		f.tls.AllowedFingerprints = []string{fingerprintOf(t, pinned)}
		url := f.serve(t, stubMux())
		if !healthOK(t, f, url, pinned) {
			t.Fatal("the pinned certificate was refused")
		}
		if healthOK(t, f, url, unpinned) {
			t.Fatal("a second certificate with the same certname got in")
		}
	})
	t.Run("lower-case input is accepted", func(t *testing.T) {
		f.tls.AllowedFingerprints = []string{strings.ToLower(fingerprintOf(t, pinned))}
		url := f.serve(t, stubMux())
		if !healthOK(t, f, url, pinned) {
			t.Fatal("the pinned certificate was refused")
		}
	})
	t.Run("a malformed fingerprint refuses to start", func(t *testing.T) {
		for _, bad := range []string{"", "AB:CD", "SHA256:" + fingerprintOf(t, pinned), strings.ReplaceAll(fingerprintOf(t, pinned), ":", "")} {
			f.tls.AllowedFingerprints = []string{bad}
			if _, err := NewMTLSServer("127.0.0.1:0", f.tls, stubMux(), nil); err == nil || !strings.Contains(err.Error(), "server.tls.allowed_fingerprints") {
				t.Fatalf("%q: err = %v, want one naming server.tls.allowed_fingerprints", bad, err)
			}
		}
	})
}
