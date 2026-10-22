package server

// The tracer for the deploy agent: a YAML config is loaded through
// config.Load, the real server is built by the same wiring `serve` uses
// (OptionsFromConfig + New), a client holding an allowed certificate POSTs a
// deploy over mTLS, and the fake r10k (internal/jobs/fakeengine) replays the
// SDK harness fixture internal/jobs/testdata/r10k-5.0.3/deploy-success.json
// (a verbatim copy of stagehand-sdk-public harness/fixtures/r10k-5.0.3/deploy-success.json).
// Ported from the fork's server/deploys_tracer_test.go.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/puppet-stagehand/stagehand-deploy-agent/internal/config"
	"github.com/puppet-stagehand/stagehand-deploy-agent/internal/jobs/fakeengine"
)

const tracerSHA = "0123456789abcdef0123456789abcdef01234567"

// deployRecord mirrors the JSON contract of /api/v1/deploys.
type deployRecord struct {
	ID      string `json:"id"`
	Engine  string `json:"engine"`
	State   string `json:"state"`
	Results []struct {
		Environment         string `json:"environment"`
		State               string `json:"state"`
		ExitCode            int    `json:"exit_code"`
		ExpectedSHA         string `json:"expected_sha"`
		DeployedSHA         string `json:"deployed_sha"`
		MarkerPresent       bool   `json:"marker_present"`
		MarkerDeploySuccess bool   `json:"marker_deploy_success"`
		StartedAt           string `json:"started_at"`
		FinishedAt          string `json:"finished_at"`
		DurationMS          *int64 `json:"duration_ms"`
		Output              string `json:"output"`
		OutputTruncated     bool   `json:"output_truncated"`
		Error               string `json:"error"`
	} `json:"results"`
}

type tracer struct {
	f       *mtlsFixture
	url     string
	client  *http.Client
	argvLog string
	basedir string
	hits    *countingHandler
}

// loadConfig writes the fixture's mTLS settings plus extra YAML to a file and
// loads it through config.Load, exactly as `serve` does at start.
func loadConfig(t *testing.T, f *mtlsFixture, extra string) *config.Config {
	t.Helper()
	allowed, _ := json.Marshal(f.tls.AllowedCertnames)
	y := fmt.Sprintf(`server:
  port: 8088
  tls:
    certificate: %q
    key: %q
    client_ca: %q
    allowed_certnames: %s
deploys:
  state_dir: %q
%s`, f.tls.Certificate, f.tls.Key, f.tls.ClientCA, allowed, f.stateDir, extra)
	p := writeFile(t, f.dir, "config.yml", []byte(y))
	cfg, findings := config.Load(p, false)
	if cfg == nil || config.HasErrors(findings) {
		t.Fatalf("config findings: %+v", findings)
	}
	return cfg
}

func newTracer(t *testing.T) *tracer {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f := newMTLSFixture(t, "stagehand-code-manager")
	basedir := t.TempDir()
	argvLog := filepath.Join(t.TempDir(), "argv.log")
	fixture, _ := filepath.Abs("../jobs/testdata/r10k-5.0.3/deploy-success.json")

	cfg := loadConfig(t, f, fmt.Sprintf(`  basedir: %q
  timeout: 1m
engine:
  command_path: %q
  config_path: /etc/puppetlabs/r10k/r10k.yaml
  verbose: false
  generate_types: false
`, basedir, exe))

	t.Setenv(fakeengine.FixtureEnv, fixture)
	t.Setenv(fakeengine.BasedirEnv, basedir)
	t.Setenv(fakeengine.SHAEnv, tracerSHA)
	t.Setenv(fakeengine.ArgvLogEnv, argvLog)

	ctx, cancel := context.WithCancel(context.Background())
	opts, err := OptionsFromConfig(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		_ = opts.Runner.Shutdown(10 * time.Second)
		_ = opts.Store.Close()
	})
	// Count what reaches the mux, to prove a refused caller never does.
	hits := &countingHandler{next: srv.HTTPServer().Handler}
	srv.HTTPServer().Handler = hits

	tr := &tracer{f: f, argvLog: argvLog, basedir: basedir, hits: hits}
	tr.url = serveOn(t, srv.HTTPServer())
	tr.client = f.client(t, clientCert(t, f.ca, "stagehand-code-manager"))
	return tr
}

func (tr *tracer) do(t *testing.T, method, path, body string) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, tr.url+path, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := tr.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, b
}

func (tr *tracer) deployProduction(t *testing.T) deployRecord {
	t.Helper()
	code, _, body := tr.do(t, http.MethodPost, "/api/v1/deploys",
		`{"environments":[{"name":"production","expected_sha":"`+tracerSHA+`"}],"wait":true}`)
	if code != http.StatusOK {
		t.Fatalf("POST status = %d, body %s", code, body)
	}
	var rec deployRecord
	if err := json.Unmarshal(body, &rec); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return rec
}

// TestTracerDeployOverMTLS is the slice the whole phase rests on: an
// allow-listed client certificate POSTs wait:false over mTLS, gets 202 with an
// id and a Location header, and polls GET until the engine's marker signature
// equals the expected SHA and the record reads "deployed".
func TestTracerDeployOverMTLS(t *testing.T) {
	tr := newTracer(t)
	code, hdr, body := tr.do(t, http.MethodPost, "/api/v1/deploys",
		`{"environments":[{"name":"production","expected_sha":"`+tracerSHA+`"}],"wait":false}`)
	if code != http.StatusAccepted {
		t.Fatalf("POST status = %d, body %s", code, body)
	}
	var rec deployRecord
	if err := json.Unmarshal(body, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.ID == "" || rec.State != "queued" {
		t.Fatalf("202 record = %s", body)
	}
	if got, want := hdr.Get("Location"), "/api/v1/deploys/"+rec.ID; got != want {
		t.Fatalf("Location = %q, want %q", got, want)
	}
	if got := hdr.Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
	if bytes.HasSuffix(body, []byte("\n")) {
		t.Fatalf("contract body has a trailing newline: %q", body)
	}

	var got deployRecord
	deadline := time.Now().Add(30 * time.Second)
	for {
		code, _, body = tr.do(t, http.MethodGet, hdr.Get("Location"), "")
		if code != http.StatusOK {
			t.Fatalf("GET status = %d, body %s", code, body)
		}
		got = deployRecord{}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		if got.State == "deployed" || got.State == "failed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job never finished, last record %s", body)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got.State != "deployed" || len(got.Results) != 1 || got.Results[0].DeployedSHA != tracerSHA ||
		got.Results[0].ExpectedSHA != tracerSHA || !got.Results[0].MarkerPresent {
		t.Fatalf("finished record = %s", body)
	}
}

// TestTracerRefusesUnlistedCertnameInHandshake: a certificate signed by the
// estate CA whose certname is not allow-listed fails during the TLS handshake
// (an alert), never with an HTTP status, so the pack keeps classifying it as
// "credential refused".
func TestTracerRefusesUnlistedCertnameInHandshake(t *testing.T) {
	tr := newTracer(t)
	intruder := tr.f.client(t, clientCert(t, tr.f.ca, "intruder"))
	resp, err := intruder.Post(tr.url+"/api/v1/deploys", "application/json",
		strings.NewReader(`{"environments":[{"name":"production","expected_sha":"`+tracerSHA+`"}],"wait":false}`))
	if err == nil {
		resp.Body.Close()
		t.Fatalf("an unlisted certname got HTTP status %d", resp.StatusCode)
	}
	if !strings.Contains(err.Error(), "remote error: tls:") {
		t.Fatalf("error is not a TLS alert: %v", err)
	}
	if tr.hits.n.Load() != 0 {
		t.Fatalf("the mux saw %d requests from a refused certificate", tr.hits.n.Load())
	}
}

func TestDeployOneEnvironmentR10k(t *testing.T) {
	tr := newTracer(t)
	rec := tr.deployProduction(t)

	if rec.ID == "" || rec.Engine != "r10k" || rec.State != "deployed" || len(rec.Results) != 1 {
		t.Fatalf("record = %+v", rec)
	}
	r := rec.Results[0]
	if r.Environment != "production" || r.State != "deployed" || r.ExitCode != 0 ||
		r.ExpectedSHA != tracerSHA || r.DeployedSHA != tracerSHA ||
		!r.MarkerPresent || !r.MarkerDeploySuccess || r.Error != "" || r.OutputTruncated {
		t.Fatalf("result = %+v", r)
	}
	if r.StartedAt == "" || r.FinishedAt == "" || r.DurationMS == nil {
		t.Fatalf("timings missing: %+v", r)
	}
	if !strings.Contains(r.Output, "Environment production is now at "+tracerSHA) {
		t.Fatalf("output = %q", r.Output)
	}

	argvs, err := fakeengine.ReadArgvLog(tr.argvLog)
	if err != nil {
		t.Fatal(err)
	}
	// One invocation, started directly with an argv: a shell would have been
	// handed "-c" and a command string instead.
	if len(argvs) != 1 {
		t.Fatalf("engine ran %d times", len(argvs))
	}
	want := []string{"deploy", "environment", "production", "--config=/etc/puppetlabs/r10k/r10k.yaml"}
	if strings.Join(argvs[0][:4], "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv = %q, want prefix %q", argvs[0], want)
	}
}

func TestGetDeployByIDAndList(t *testing.T) {
	tr := newTracer(t)
	rec := tr.deployProduction(t)

	code, _, body := tr.do(t, http.MethodGet, "/api/v1/deploys/"+rec.ID, "")
	if code != http.StatusOK {
		t.Fatalf("GET by id status = %d, body %s", code, body)
	}
	var got deployRecord
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != rec.ID || got.State != rec.State || len(got.Results) != 1 ||
		got.Results[0].DeployedSHA != rec.Results[0].DeployedSHA || got.Results[0].Output != rec.Results[0].Output {
		t.Fatalf("GET by id = %+v, want %+v", got, rec)
	}

	code, _, body = tr.do(t, http.MethodGet, "/api/v1/deploys?limit=1", "")
	if code != http.StatusOK {
		t.Fatalf("list status = %d, body %s", code, body)
	}
	var list struct {
		Deploys []deployRecord `json:"deploys"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Deploys) != 1 || list.Deploys[0].ID != rec.ID {
		t.Fatalf("list = %s", body)
	}

	code, _, body = tr.do(t, http.MethodGet, "/api/v1/deploys/00000000-0000-0000-0000-000000000000", "")
	if code != http.StatusNotFound || string(body) != `{"error":"deploy not found"}` {
		t.Fatalf("unknown id status = %d, body %s; want 404 {\"error\":\"deploy not found\"}", code, body)
	}
}
