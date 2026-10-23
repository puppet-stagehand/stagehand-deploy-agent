package server

// GET /api/v1/info (D-06, D-19): the agent describes itself to a caller the
// handshake already let in. The seven key names and their order are a
// published contract once v1.0.0 ships, so they are pinned byte for byte.

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/puppet-stagehand/stagehand-deploy-agent/internal/ops"
)

var infoKeys = []string{"version", "engine", "engine_version", "environments_dir", "cert_not_after", "crl_next_update", "config_source"}

// objectKeys returns the keys of a JSON object in the order they appear.
func objectKeys(t *testing.T, raw []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("not a JSON object: %s", raw)
	}
	var keys []string
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

func getInfo(t *testing.T, client *http.Client, url string) (int, []byte) {
	t.Helper()
	resp, err := client.Get(url + "/api/v1/info")
	if err != nil {
		t.Fatalf("GET /api/v1/info: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// TestInfoKeysAndOrder: through the real wiring (config, OptionsFromConfigBuild,
// New), an allowed certificate gets 200 and the body is exactly the seven keys
// in D-19 order, with the values the process was built with.
func TestInfoKeysAndOrder(t *testing.T) {
	tr := newTracerBuild(t, "", BuildInfo{Version: "1.0.0", EngineVersion: "r10k 5.0.3", ConfigSource: "/etc/stagehand-deploy-agent/config.yml"})
	code, hdr, body := tr.do(t, http.MethodGet, "/api/v1/info", "")
	if code != http.StatusOK {
		t.Fatalf("GET /api/v1/info = %d %s", code, body)
	}
	if ct := hdr.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if got := objectKeys(t, body); strings.Join(got, ",") != strings.Join(infoKeys, ",") {
		t.Fatalf("keys = %v, want %v", got, infoKeys)
	}
	notAfter, err := ops.CertNotAfter(tr.f.tls.Certificate)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"version":"1.0.0","engine":"r10k","engine_version":"r10k 5.0.3","environments_dir":"` + tr.basedir +
		`","cert_not_after":"` + notAfter.UTC().Format(time.RFC3339) + `","crl_next_update":null,"config_source":"/etc/stagehand-deploy-agent/config.yml"}`
	if string(body) != want {
		t.Fatalf("body =\n%s\nwant\n%s", body, want)
	}
}

// infoRig serves a Server built from Options on a loopback port.
func infoRig(t *testing.T, f *mtlsFixture, build BuildInfo) (url string, hits *countingHandler) {
	t.Helper()
	srv, err := New(Options{Addr: "127.0.0.1:0", TLS: f.tls, Runner: mustRunner(t), EnvironmentsDir: "/srv/environments", Build: build})
	if err != nil {
		t.Fatal(err)
	}
	hits = &countingHandler{next: srv.HTTPServer().Handler}
	srv.HTTPServer().Handler = hits
	return serveOn(t, srv.HTTPServer()), hits
}

// TestInfoNullsAndCRL: no CRL gives null; a CRL gives its NextUpdate in
// RFC3339 UTC; an engine that did not answer gives a null engine_version.
func TestInfoNullsAndCRL(t *testing.T) {
	f := newMTLSFixture(t, "stagehand-code-manager")
	client := f.client(t, clientCert(t, f.ca, "stagehand-code-manager"))
	url, _ := infoRig(t, f, BuildInfo{Version: "1.0.0", ConfigSource: "/etc/voxpupuli/webhook.yml"})
	code, body := getInfo(t, client, url)
	if code != 200 {
		t.Fatalf("status %d: %s", code, body)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got["engine_version"] != nil || got["crl_next_update"] != nil {
		t.Fatalf("engine_version and crl_next_update must be null here: %s", body)
	}
	if got["config_source"] != "/etc/voxpupuli/webhook.yml" || got["environments_dir"] != "/srv/environments" {
		t.Fatalf("body = %s", body)
	}

	f2 := newMTLSFixture(t, "stagehand-code-manager")
	crlPath := writeFile(t, f2.dir, "crl.pem", f2.ca.crlPEM(t))
	f2.tls.CRL = crlPath
	raw, _ := os.ReadFile(crlPath)
	der, _ := pemDecodeCRL(raw)
	rl, err := x509.ParseRevocationList(der)
	if err != nil {
		t.Fatal(err)
	}
	url2, _ := infoRig(t, f2, BuildInfo{Version: "1.0.0"})
	code, body = getInfo(t, f2.client(t, clientCert(t, f2.ca, "stagehand-code-manager")), url2)
	if code != 200 {
		t.Fatalf("status %d: %s", code, body)
	}
	if !strings.Contains(string(body), `"crl_next_update":"`+rl.NextUpdate.UTC().Format(time.RFC3339)+`"`) {
		t.Fatalf("crl_next_update is not the CRL's NextUpdate %s: %s", rl.NextUpdate.UTC().Format(time.RFC3339), body)
	}
}

// TestInfoRefusesAnUnlistedCertificate: the allow-list is the handshake's, so
// a certificate not on it never reaches the handler.
func TestInfoRefusesAnUnlistedCertificate(t *testing.T) {
	f := newMTLSFixture(t, "stagehand-code-manager")
	url, hits := infoRig(t, f, BuildInfo{Version: "1.0.0"})
	resp, err := f.client(t, clientCert(t, f.ca, "intruder")).Get(url + "/api/v1/info")
	if err == nil {
		resp.Body.Close()
		t.Fatalf("an unlisted certificate got %d", resp.StatusCode)
	}
	if n := hits.n.Load(); n != 0 {
		t.Fatalf("%d requests reached the handler", n)
	}
	if resp, err := f.client(t, nil).Get(url + "/api/v1/info"); err == nil {
		resp.Body.Close()
		t.Fatal("a caller with no certificate got an answer")
	}
}

// TestInfoMethodsAndPath: only GET on exactly /api/v1/info.
func TestInfoMethodsAndPath(t *testing.T) {
	tr := newTracerBuild(t, "", BuildInfo{Version: "1.0.0"})
	if code, _, _ := tr.do(t, http.MethodPost, "/api/v1/info", "{}"); code != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/v1/info = %d, want 405", code)
	}
	if code, _, _ := tr.do(t, http.MethodGet, "/api/v1/info/extra", ""); code != http.StatusNotFound {
		t.Errorf("GET /api/v1/info/extra = %d, want 404", code)
	}
}

// TestInfoNotOnTheOpsMux: /api/v1/info is for authenticated callers only.
func TestInfoNotOnTheOpsMux(t *testing.T) {
	mux := ops.NewMux(&ops.Checks{}, nil)
	rec := httptestGet(mux, "/api/v1/info")
	if rec != http.StatusNotFound {
		t.Fatalf("ops mux /api/v1/info = %d, want 404", rec)
	}
}

func pemDecodeCRL(b []byte) ([]byte, bool) {
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, false
	}
	return block.Bytes, true
}

func httptestGet(h http.Handler, path string) int {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code
}
