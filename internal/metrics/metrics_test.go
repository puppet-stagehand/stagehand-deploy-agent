package metrics

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)

func text(t *testing.T, r *Registry) string {
	t.Helper()
	var b bytes.Buffer
	if err := r.WriteText(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// TestWriteTextGolden pins the exposition bytes: HELP and TYPE once per
// family, escaping, cumulative buckets ending in +Inf, sum and count, and a
// deterministic order of families and series.
func TestWriteTextGolden(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("x_requests_total", "Requests with a \\ backslash\nand a newline.", "route", "code")
	c.Inc("POST /a", "202")
	c.Inc("GET /\"quoted\"\\path\nnl", "200")
	c.Inc("GET /\"quoted\"\\path\nnl", "200")
	g := r.Gauge("x_in_flight", "In flight.")
	g.Set(3)
	h := r.Histogram("x_duration_seconds", "Duration.", []float64{0.5, 1}, "kind")
	h.Observe(0.25, "deploy")
	h.Observe(0.75, "deploy")
	h.Observe(5, "deploy")
	r.GaugeFunc("x_absent", "Never exposed.", func() (float64, bool) { return 0, false })
	r.GaugeFunc("x_days", "Computed at scrape time.", func() (float64, bool) { return -1.5, true })
	r.Counter("x_unused_total", "No series yet.", "reason")

	want := `# HELP x_days Computed at scrape time.
# TYPE x_days gauge
x_days -1.5
# HELP x_duration_seconds Duration.
# TYPE x_duration_seconds histogram
x_duration_seconds_bucket{kind="deploy",le="0.5"} 1
x_duration_seconds_bucket{kind="deploy",le="1"} 2
x_duration_seconds_bucket{kind="deploy",le="+Inf"} 3
x_duration_seconds_sum{kind="deploy"} 6
x_duration_seconds_count{kind="deploy"} 3
# HELP x_in_flight In flight.
# TYPE x_in_flight gauge
x_in_flight 3
# HELP x_requests_total Requests with a \\ backslash\nand a newline.
# TYPE x_requests_total counter
x_requests_total{route="GET /\"quoted\"\\path\nnl",code="200"} 2
x_requests_total{route="POST /a",code="202"} 1
`
	if got := text(t, r); got != want {
		t.Fatalf("exposition differs.\n--- got ---\n%s--- want ---\n%s", got, want)
	}
	// Twice the same: the order does not depend on map iteration.
	if a, b := text(t, r), text(t, r); a != b {
		t.Fatal("two scrapes of an unchanged registry differ")
	}
}

func TestHandlerContentType(t *testing.T) {
	r := NewRegistry()
	r.Gauge("x_g", "g.").Set(1)
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if got := rec.Header().Get("Content-Type"); got != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "x_g 1\n") {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
}

func TestRegistrationRefusesBadNamesAndDuplicates(t *testing.T) {
	mustPanic := func(name string, f func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Errorf("%s did not panic", name)
			}
		}()
		f()
	}
	r := NewRegistry()
	r.Gauge("x_ok", "ok.")
	mustPanic("duplicate", func() { r.Gauge("x_ok", "again.") })
	mustPanic("bad metric name", func() { r.Gauge("1bad", "bad.") })
	mustPanic("bad label name", func() { r.Counter("x_c_total", "c.", "has space") })
	mustPanic("reserved le label", func() { r.Counter("x_d_total", "d.", "le") })
	mustPanic("wrong label count", func() { r.Counter("x_e_total", "e.", "a").Inc() })
}

func TestConcurrentUseIsSafe(t *testing.T) {
	r := New("1.0.0", "r10k", func() time.Time { return t0 })
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				r.ObserveRequest("GET /api/v1/deploys", 200, time.Millisecond)
				r.JobFinished("deploy", "deployed", time.Second)
				r.Queue(j%3, j%2)
				_ = text(t, r)
			}
		}()
	}
	wg.Wait()
}

// jobsSeries returns the jobs_total lines of an exposition.
func jobsSeries(exposition string) []string {
	var out []string
	for _, l := range strings.Split(exposition, "\n") {
		if strings.HasPrefix(l, "stagehand_deploy_agent_jobs_total{") {
			out = append(out, l)
		}
	}
	return out
}

// TestJobsTotalSeries: after a deployed job, a failed job, a refusal and a
// removal, jobs_total has the four expected {kind,outcome} series, and no
// environment name or job id is in any label (T-57.6.2-36).
func TestJobsTotalSeries(t *testing.T) {
	r := New("1.0.0", "r10k", func() time.Time { return t0 })
	r.JobStarted("deploy")
	r.JobFinished("deploy", "deployed", 2*time.Second)
	r.JobStarted("deploy")
	r.JobFinished("deploy", "failed", 3*time.Second)
	r.Refused("deploy", "busy")
	r.JobStarted("remove")
	r.JobFinished("remove", "deployed", 40*time.Millisecond)
	out := text(t, r)
	want := []string{
		`stagehand_deploy_agent_jobs_total{kind="deploy",outcome="deployed"} 1`,
		`stagehand_deploy_agent_jobs_total{kind="deploy",outcome="failed"} 1`,
		`stagehand_deploy_agent_jobs_total{kind="deploy",outcome="refused"} 1`,
		`stagehand_deploy_agent_jobs_total{kind="remove",outcome="deployed"} 1`,
	}
	if got := jobsSeries(out); !reflect.DeepEqual(got, want) {
		t.Fatalf("jobs_total series:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if !strings.Contains(out, `stagehand_deploy_agent_requests_refused_total{reason="busy"} 1`) {
		t.Fatalf("a busy refusal is not in requests_refused_total:\n%s", out)
	}
	if !strings.Contains(out, `stagehand_deploy_agent_job_duration_seconds_count{kind="deploy"} 2`) {
		t.Fatalf("job_duration_seconds is missing the two deploys:\n%s", out)
	}
}

// TestRefusedWithoutAJob: a protected or invalid request refused by a handler
// counts as a refused request but is not a job.
func TestRefusedWithoutAJob(t *testing.T) {
	r := New("1.0.0", "r10k", func() time.Time { return t0 })
	r.Refused("remove", "protected")
	r.Refused("deploy", "invalid")
	out := text(t, r)
	if len(jobsSeries(out)) != 0 {
		t.Fatalf("a refusal that created no job is in jobs_total:\n%s", out)
	}
	for _, reason := range []string{"protected", "invalid"} {
		if !strings.Contains(out, `stagehand_deploy_agent_requests_refused_total{reason="`+reason+`"} 1`) {
			t.Errorf("missing requests_refused_total reason=%s:\n%s", reason, out)
		}
	}
}

// TestUnknownLabelValuesAreFolded: a value outside the fixed set can never
// become a new series.
func TestUnknownLabelValuesAreFolded(t *testing.T) {
	r := New("1.0.0", "r10k", func() time.Time { return t0 })
	r.JobFinished("feature_x", "weird", time.Second)
	r.Refused("deploy", "the environment feature_x")
	r.TLSRefusal("someone.example.test")
	out := text(t, r)
	for _, bad := range []string{"feature_x", "weird", "someone"} {
		if strings.Contains(out, bad) {
			t.Fatalf("%q reached the exposition:\n%s", bad, out)
		}
	}
	if !strings.Contains(out, `kind="other",outcome="other"`) {
		t.Fatalf("an unknown kind and outcome were not folded to other:\n%s", out)
	}
}

func TestTLSRefusalsAndRequests(t *testing.T) {
	r := New("1.0.0", "r10k", func() time.Time { return t0 })
	r.TLSRefusal("certname")
	r.TLSRefusal("certname")
	r.TLSRefusal("revoked")
	r.ObserveRequest("GET /api/v1/deploys/{id}", 404, 3*time.Millisecond)
	r.ObserveRequest("unmatched", 404, 10*time.Millisecond)
	out := text(t, r)
	for _, want := range []string{
		`stagehand_deploy_agent_tls_refusals_total{reason="certname"} 2`,
		`stagehand_deploy_agent_tls_refusals_total{reason="revoked"} 1`,
		`stagehand_deploy_agent_http_requests_total{route="GET /api/v1/deploys/{id}",code="404"} 1`,
		`stagehand_deploy_agent_http_requests_total{route="unmatched",code="404"} 1`,
		`stagehand_deploy_agent_http_request_duration_seconds_bucket{route="unmatched",le="0.005"} 0`,
		`stagehand_deploy_agent_http_request_duration_seconds_bucket{route="unmatched",le="0.01"} 1`,
		`stagehand_deploy_agent_http_request_duration_seconds_bucket{route="unmatched",le="+Inf"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s\n%s", want, out)
		}
	}
}

func TestBuildInfoAndReady(t *testing.T) {
	r := New("1.2.3", "g10k", func() time.Time { return t0 })
	if out := text(t, r); !strings.Contains(out, `stagehand_deploy_agent_build_info{version="1.2.3",engine="g10k"} 1`) ||
		!strings.Contains(out, "stagehand_deploy_agent_ready 0\n") {
		t.Fatalf("build_info or ready (no source yet) wrong:\n%s", out)
	}
	ready := false
	r.SetReadySource(func() bool { return ready })
	if !strings.Contains(text(t, r), "stagehand_deploy_agent_ready 0\n") {
		t.Fatal("ready is not 0 while the source says not ready")
	}
	ready = true
	if !strings.Contains(text(t, r), "stagehand_deploy_agent_ready 1\n") {
		t.Fatal("ready is not 1 while the source says ready")
	}
}

func TestQueueGauges(t *testing.T) {
	r := New("1.0.0", "r10k", func() time.Time { return t0 })
	r.Queue(4, 1)
	out := text(t, r)
	if !strings.Contains(out, "stagehand_deploy_agent_jobs_queued 4\n") || !strings.Contains(out, "stagehand_deploy_agent_jobs_in_flight 1\n") {
		t.Fatalf("queue gauges wrong:\n%s", out)
	}
}

// TestExpiryGaugesAreComputedAtScrapeTime: days until expiry follow the clock,
// go negative once expired, and the CRL gauge is absent without a CRL.
func TestExpiryGaugesAreComputedAtScrapeTime(t *testing.T) {
	now := t0
	r := New("1.0.0", "r10k", func() time.Time { return now })
	if out := text(t, r); strings.Contains(out, "days_until_expiry ") {
		t.Fatalf("an expiry gauge is exposed before any source is set:\n%s", out)
	}
	certEnd := t0.Add(10 * 24 * time.Hour)
	r.SetExpirySources(func() (time.Time, bool) { return certEnd, true }, nil)
	out := text(t, r)
	if !strings.Contains(out, "stagehand_deploy_agent_server_certificate_days_until_expiry 10\n") {
		t.Fatalf("10 days left not shown:\n%s", out)
	}
	if strings.Contains(out, "stagehand_deploy_agent_crl_days_until_expiry ") {
		t.Fatalf("the CRL gauge is exposed without a CRL:\n%s", out)
	}
	now = t0.Add(12*24*time.Hour + 12*time.Hour)
	if out = text(t, r); !strings.Contains(out, "stagehand_deploy_agent_server_certificate_days_until_expiry -2.5\n") {
		t.Fatalf("expired certificate not negative:\n%s", out)
	}
	r.SetExpirySources(func() (time.Time, bool) { return certEnd, true }, func() (time.Time, bool) { return now.Add(36 * time.Hour), true })
	if out = text(t, r); !strings.Contains(out, "stagehand_deploy_agent_crl_days_until_expiry 1.5\n") {
		t.Fatalf("CRL gauge wrong:\n%s", out)
	}
}

// TestNoUnboundedLabels pins every label name the registry declares to the
// PD-33 set: no environment, job id, certname or path can be a label, and a
// new label has to change this test (T-57.6.2-36).
func TestNoUnboundedLabels(t *testing.T) {
	r := New("1.0.0", "r10k", func() time.Time { return t0 })
	want := map[string][]string{
		"stagehand_deploy_agent_build_info":                           {"version", "engine"},
		"stagehand_deploy_agent_ready":                                nil,
		"stagehand_deploy_agent_http_requests_total":                  {"route", "code"},
		"stagehand_deploy_agent_http_request_duration_seconds":        {"route"},
		"stagehand_deploy_agent_jobs_total":                           {"kind", "outcome"},
		"stagehand_deploy_agent_job_duration_seconds":                 {"kind"},
		"stagehand_deploy_agent_jobs_in_flight":                       nil,
		"stagehand_deploy_agent_jobs_queued":                          nil,
		"stagehand_deploy_agent_requests_refused_total":               {"reason"},
		"stagehand_deploy_agent_tls_refusals_total":                   {"reason"},
		"stagehand_deploy_agent_server_certificate_days_until_expiry": nil,
		"stagehand_deploy_agent_crl_days_until_expiry":                nil,
	}
	got := r.Labels()
	if len(got) != len(want) {
		t.Fatalf("%d families, want %d: %v", len(got), len(want), got)
	}
	for name, labels := range want {
		if !reflect.DeepEqual(got[name], labels) {
			t.Errorf("%s labels = %v, want %v", name, got[name], labels)
		}
	}
}
