package ops

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

var epoch = time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)

// writeCert writes a self-signed certificate that expires at notAfter.
func writeCert(t *testing.T, dir string, notAfter time.Time) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "agent.example.test"},
		NotBefore:    notAfter.Add(-90 * 24 * time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "server.pem")
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

type fakeCRL struct {
	next time.Time
	err  error
}

func (f fakeCRL) NextUpdate() time.Time { return f.next }
func (f fakeCRL) Readable() error       { return f.err }

// goodChecks is a Checks for which every hard check passes at epoch.
func goodChecks(t *testing.T) *Checks {
	t.Helper()
	dir := t.TempDir()
	engine := filepath.Join(dir, "r10k")
	if err := os.WriteFile(engine, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	envs := filepath.Join(dir, "environments")
	if err := os.Mkdir(envs, 0o755); err != nil {
		t.Fatal(err)
	}
	return &Checks{
		EnginePath:      engine,
		EnvironmentsDir: envs,
		CertificatePath: writeCert(t, dir, epoch.Add(365*24*time.Hour)),
		Now:             func() time.Time { return epoch },
	}
}

func getReadyz(c *Checks) (int, string) {
	rec := httptest.NewRecorder()
	NewMux(c, nil).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	return rec.Code, rec.Body.String()
}

// TestReadyz: each hard failure answers 503 naming exactly that check, and
// nothing else; with every check good it answers 200 {"ready":true,"failing":[]}.
func TestReadyz(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, c *Checks)
		want   []string
	}{
		{"all good", func(t *testing.T, c *Checks) {}, nil},
		{"engine missing", func(t *testing.T, c *Checks) { c.EnginePath += ".gone" }, []string{"engine"}},
		{"engine not executable", func(t *testing.T, c *Checks) {
			if err := os.Chmod(c.EnginePath, 0o644); err != nil {
				t.Fatal(err)
			}
		}, []string{"engine"}},
		{"engine is a directory", func(t *testing.T, c *Checks) { c.EnginePath = c.EnvironmentsDir }, []string{"engine"}},
		{"environments directory missing", func(t *testing.T, c *Checks) { c.EnvironmentsDir += ".gone" }, []string{"environments_dir"}},
		{"environments directory read-only", func(t *testing.T, c *Checks) {
			if os.Geteuid() == 0 {
				t.Skip("root can write a read-only directory")
			}
			if err := os.Chmod(c.EnvironmentsDir, 0o555); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(c.EnvironmentsDir, 0o755) })
		}, []string{"environments_dir"}},
		{"certificate expired", func(t *testing.T, c *Checks) {
			c.CertificatePath = writeCert(t, filepath.Dir(c.EnginePath), epoch.Add(-time.Minute))
		}, []string{"certificate"}},
		{"certificate unreadable", func(t *testing.T, c *Checks) { c.CertificatePath += ".gone" }, []string{"certificate"}},
		{"certificate not a certificate", func(t *testing.T, c *Checks) {
			if err := os.WriteFile(c.CertificatePath, []byte("not pem"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, []string{"certificate"}},
		{"certificate expiring in 3 days stays ready", func(t *testing.T, c *Checks) {
			c.CertificatePath = writeCert(t, filepath.Dir(c.EnginePath), epoch.Add(72*time.Hour))
		}, nil},
		{"crl next update passed", func(t *testing.T, c *Checks) {
			c.CRL = fakeCRL{next: epoch.Add(-time.Second)}
		}, []string{"crl"}},
		{"crl unreadable", func(t *testing.T, c *Checks) {
			c.CRL = fakeCRL{next: epoch.Add(time.Hour), err: errors.New("open /secret/path/crl.pem: permission denied")}
		}, []string{"crl"}},
		{"crl expiring in 3 days stays ready", func(t *testing.T, c *Checks) {
			c.CRL = fakeCRL{next: epoch.Add(72 * time.Hour)}
		}, nil},
		{"two failures are both named, in the fixed order", func(t *testing.T, c *Checks) {
			c.EnginePath += ".gone"
			c.CRL = fakeCRL{next: epoch.Add(-time.Second)}
		}, []string{"engine", "crl"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := goodChecks(t)
			tc.mutate(t, c)
			code, body := getReadyz(c)
			if len(tc.want) == 0 {
				if code != http.StatusOK || body != `{"ready":true,"failing":[]}` {
					t.Fatalf("got %d %s, want 200 {\"ready\":true,\"failing\":[]}", code, body)
				}
				return
			}
			if code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503 (body %s)", code, body)
			}
			if got := c.Failing(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Failing() = %v, want %v", got, tc.want)
			}
			for _, secret := range []string{"/", "secret", "permission"} {
				if strings.Contains(body, secret) {
					t.Fatalf("the body names %q, which must stay out of /readyz: %s", secret, body)
				}
			}
		})
	}
}

// TestReadyzBodyShape pins the failing body bytes.
func TestReadyzBodyShape(t *testing.T) {
	c := goodChecks(t)
	c.EnginePath += ".gone"
	code, body := getReadyz(c)
	if code != http.StatusServiceUnavailable || body != `{"ready":false,"failing":["engine"]}` {
		t.Fatalf("got %d %s", code, body)
	}
}

// TestReadyzDraining: once shutdown starts /readyz answers 503 (PD-36), and
// Ready() agrees, so the ready gauge does too.
func TestReadyzDraining(t *testing.T) {
	c := goodChecks(t)
	if !c.Ready() {
		t.Fatalf("a good Checks is not ready: %v", c.Failing())
	}
	c.SetDraining()
	code, body := getReadyz(c)
	if code != http.StatusServiceUnavailable || body != `{"ready":false,"failing":["draining"]}` {
		t.Fatalf("got %d %s", code, body)
	}
	if c.Ready() {
		t.Fatal("Ready() is true while draining")
	}
}

// TestReadyzDaysUntilExpiry: the helpers the gauges use.
func TestReadyzCertNotAfter(t *testing.T) {
	dir := t.TempDir()
	want := epoch.Add(10 * 24 * time.Hour).Truncate(time.Second)
	got, err := CertNotAfter(writeCert(t, dir, want))
	if err != nil || !got.Equal(want) {
		t.Fatalf("CertNotAfter = %v, %v; want %v", got, err, want)
	}
	if _, err := CertNotAfter(filepath.Join(dir, "missing.pem")); err == nil {
		t.Fatal("a missing file was read")
	}
}
