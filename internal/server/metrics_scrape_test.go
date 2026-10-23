package server

// The metrics seen through the real wiring: OptionsFromConfig builds the
// registry and hands the runner its observer, New puts the middleware in front
// of the mux and the registry's handler on the ops listener.

import (
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/puppet-stagehand/stagehand-deploy-agent/internal/jobs"
)

func scrapeOps(t *testing.T, addr string) string {
	t.Helper()
	code, body := getOps(t, addr, "/metrics")
	if code != 200 {
		t.Fatalf("/metrics = %d", code)
	}
	return body
}

func TestMetricsScrapeThroughTheOpsListener(t *testing.T) {
	opsAddr := "127.0.0.1:" + strconv.Itoa(freePort(t))
	tr := newTracerWith(t, "ops:\n  listen: "+opsAddr+"\n")
	if err := tr.srv.startOps(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.srv.stopOps() })

	// A deployed job, a removal, a protected removal, an invalid request, and a
	// handshake refused for its certname.
	if rec := tr.deployProduction(t); rec.State != "deployed" {
		t.Fatalf("deploy = %s", rec.State)
	}
	if err := os.MkdirAll(tr.basedir+"/feature_x", 0o755); err != nil {
		t.Fatal(err)
	}
	code, hdr, body := tr.do(t, http.MethodDelete, "/api/v1/environments/feature_x", "")
	if code != http.StatusAccepted {
		t.Fatalf("DELETE = %d %s", code, body)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		code, _, body = tr.do(t, http.MethodGet, hdr.Get("Location"), "")
		if code == 200 && strings.Contains(string(body), `"state":"deployed"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the removal never finished: %s", body)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if code, _, _ := tr.do(t, http.MethodDelete, "/api/v1/environments/production", ""); code != http.StatusForbidden {
		t.Fatalf("protected DELETE = %d", code)
	}
	if code, _, _ := tr.do(t, http.MethodPost, "/api/v1/deploys", `{"environments":[]}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid POST = %d", code)
	}
	intruder := tr.f.client(t, clientCert(t, tr.f.ca, "intruder"))
	if resp, err := intruder.Get(tr.url + "/api/v1/deploys"); err == nil {
		resp.Body.Close()
		t.Fatal("the intruder was let in")
	}

	out := scrapeOps(t, opsAddr)
	for _, want := range []string{
		`stagehand_deploy_agent_build_info{version="dev",engine="r10k"} 1`,
		"stagehand_deploy_agent_ready 1\n",
		`stagehand_deploy_agent_jobs_total{kind="deploy",outcome="deployed"} 1`,
		`stagehand_deploy_agent_jobs_total{kind="remove",outcome="deployed"} 1`,
		`stagehand_deploy_agent_http_requests_total{route="POST /api/v1/deploys",code="200"} 1`,
		`stagehand_deploy_agent_http_requests_total{route="DELETE /api/v1/environments/{env}",code="202"} 1`,
		`stagehand_deploy_agent_http_requests_total{route="GET /api/v1/removals/{id}",code="200"}`,
		`stagehand_deploy_agent_requests_refused_total{reason="protected"} 1`,
		`stagehand_deploy_agent_requests_refused_total{reason="invalid"} 1`,
		`stagehand_deploy_agent_tls_refusals_total{reason="certname"} 1`,
		`stagehand_deploy_agent_job_duration_seconds_count{kind="deploy"} 1`,
		"stagehand_deploy_agent_jobs_in_flight 0\n",
		"stagehand_deploy_agent_jobs_queued 0\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the scrape lacks %s", want)
		}
	}
	m := regexp.MustCompile(`(?m)^stagehand_deploy_agent_server_certificate_days_until_expiry (\S+)$`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no certificate expiry gauge:\n%s", out)
	}
	if days, err := strconv.ParseFloat(m[1], 64); err != nil || days <= 0 || days >= 1 {
		t.Fatalf("certificate days = %q, want a fraction of a day (the fixture certificate lasts an hour)", m[1])
	}
	if strings.Contains(out, "crl_days_until_expiry ") {
		t.Fatal("a CRL gauge is exposed although no CRL is configured")
	}
	for _, leak := range []string{"feature_x", "intruder", tracerSHA} {
		if strings.Contains(out, leak) {
			t.Errorf("%q reached the exposition", leak)
		}
	}
	// The mTLS port does not serve metrics.
	if code, _, _ := tr.do(t, http.MethodGet, "/metrics", ""); code != http.StatusNotFound {
		t.Errorf("/metrics on the mTLS port = %d, want 404", code)
	}
}

func TestMetricsCRLGaugeWithACRL(t *testing.T) {
	f := newMTLSFixture(t, "stagehand-code-manager")
	f.tls.CRL = writeFile(t, f.dir, "crl.pem", f.ca.crlPEM(t))
	m := newMetrics()
	srv, err := New(Options{Addr: "127.0.0.1:0", TLS: f.tls, Runner: mustRunner(t), Metrics: m})
	if err != nil {
		t.Fatal(err)
	}
	_ = srv
	out := scrape(t, m)
	mm := regexp.MustCompile(`(?m)^stagehand_deploy_agent_crl_days_until_expiry (\S+)$`).FindStringSubmatch(out)
	if mm == nil {
		t.Fatalf("no CRL gauge with a CRL configured:\n%s", out)
	}
	if days, _ := strconv.ParseFloat(mm[1], 64); days < 0.9 || days > 1.1 {
		t.Fatalf("CRL days = %q, want about 1 (the test CRL lasts 24h)", mm[1])
	}
}

// mustRunner is a runner for tests that only need New to accept the options.
func mustRunner(t *testing.T) *jobs.Runner {
	t.Helper()
	r, err := jobs.NewRunner(jobs.Options{
		Engine:      jobs.R10kEngine(func(env string) []string { return []string{"/bin/true", env} }),
		Basedir:     t.TempDir(),
		Timeout:     time.Minute,
		OutputLimit: 4096,
		Store:       jobs.NewMemoryStore(10),
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
