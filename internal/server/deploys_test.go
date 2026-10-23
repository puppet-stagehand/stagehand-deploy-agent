package server

// The deploy API handler tests, ported from the fork's api/deploys_test.go with
// the gin plumbing replaced by NewMux and httptest. Every fork test keeps its
// name and its assertions, with one deliberate change: TestAsyncDeployG10k no
// longer asserts a wall-clock bound on the POST (FORK-WR-05); it proves "answers
// before the engine finishes" by state. The FORK-IN-06 request-hardening cases
// are new (TestDeployOversizedBody, TestDeployTrailingData,
// TestDeployUnknownFieldAccepted).
//
// The fake r10k replays internal/jobs/testdata/r10k-5.0.3/deploy-success.json, a
// verbatim copy of stagehand-sdk-public harness/fixtures/r10k-5.0.3/deploy-success.json.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/puppet-stagehand/stagehand-deploy-agent/internal/jobs"
	"github.com/puppet-stagehand/stagehand-deploy-agent/internal/jobs/fakeengine"
)

const goodSHA = "0123456789abcdef0123456789abcdef01234567"

// newTestDeploys wires the deploy handlers to the fake r10k and returns the
// agent's real mux plus the fake's argv log.
func newTestDeploys(t *testing.T) (http.Handler, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	basedir := t.TempDir()
	argvLog := filepath.Join(t.TempDir(), "argv.log")
	fixture, _ := filepath.Abs("../jobs/testdata/r10k-5.0.3/deploy-success.json")
	t.Setenv(fakeengine.FixtureEnv, fixture)
	t.Setenv(fakeengine.BasedirEnv, basedir)
	t.Setenv(fakeengine.SHAEnv, goodSHA)
	t.Setenv(fakeengine.ArgvLogEnv, argvLog)

	runner, err := jobs.NewRunner(jobs.Options{
		Engine: jobs.R10kEngine(func(env string) []string {
			return []string{exe, "deploy", "environment", env, "--config=/etc/puppetlabs/r10k/r10k.yaml"}
		}),
		Basedir:     basedir,
		Timeout:     time.Minute,
		OutputLimit: 4096,
		Store:       jobs.NewMemoryStore(10),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runner.Shutdown(10 * time.Second) })
	return NewMux(NewDeployHandlers(runner, slog.New(slog.NewTextHandler(io.Discard, nil))), nil), argvLog
}

func post(h http.Handler, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/deploys", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(w, req)
	return w
}

func TestDeployRefusesBadInput(t *testing.T) {
	r, argvLog := newTestDeploys(t)

	many := make([]string, 21)
	for i := range many {
		many[i] = fmt.Sprintf(`{"name":"env%d","expected_sha":"%s"}`, i, goodSHA)
	}

	cases := []struct {
		name  string
		body  string
		field string
	}{
		{"shell metacharacters", `{"environments":[{"name":"prod;rm","expected_sha":"` + goodSHA + `"}],"wait":true}`, "environments[0].name"},
		{"path traversal", `{"environments":[{"name":"../x","expected_sha":"` + goodSHA + `"}],"wait":true}`, "environments[0].name"},
		{"empty name", `{"environments":[{"name":"","expected_sha":"` + goodSHA + `"}],"wait":true}`, "environments[0].name"},
		{"uppercase name", `{"environments":[{"name":"Production","expected_sha":"` + goodSHA + `"}],"wait":true}`, "environments[0].name"},
		{"option-looking name", `{"environments":[{"name":"--config=/x","expected_sha":"` + goodSHA + `"}],"wait":true}`, "environments[0].name"},
		{"short sha", `{"environments":[{"name":"production","expected_sha":"abc"}],"wait":true}`, "environments[0].expected_sha"},
		{"missing sha", `{"environments":[{"name":"production"}],"wait":true}`, "environments[0].expected_sha"},
		{"uppercase sha", `{"environments":[{"name":"production","expected_sha":"` + strings.ToUpper(goodSHA) + `"}],"wait":true}`, "environments[0].expected_sha"},
		{"second entry bad", `{"environments":[{"name":"production","expected_sha":"` + goodSHA + `"},{"name":"a b","expected_sha":"` + goodSHA + `"}],"wait":true}`, "environments[1].name"},
		{"duplicate", `{"environments":[{"name":"production","expected_sha":"` + goodSHA + `"},{"name":"production","expected_sha":"` + goodSHA + `"}],"wait":true}`, "environments[1].name"},
		{"no environments", `{"environments":[],"wait":true}`, "environments"},
		{"too many environments", `{"environments":[` + strings.Join(many, ",") + `],"wait":true}`, "environments"},
		{"wait missing", `{"environments":[{"name":"production","expected_sha":"` + goodSHA + `"}]}`, "wait"},
		{"wait not a boolean", `{"environments":[{"name":"production","expected_sha":"` + goodSHA + `"}],"wait":"no"}`, "wait"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := post(r, tc.body)
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, body %s", w.Code, w.Body)
			}
			var e struct {
				Error string `json:"error"`
				Field string `json:"field"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
				t.Fatal(err)
			}
			if e.Field != tc.field || !strings.HasPrefix(e.Error, tc.field+": ") {
				t.Fatalf("error = %+v, want field %q", e, tc.field)
			}
		})
	}

	if w := post(r, `{"environments":`); w.Code != http.StatusBadRequest {
		t.Fatalf("malformed JSON status = %d", w.Code)
	}

	argvs, err := fakeengine.ReadArgvLog(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	if len(argvs) != 0 {
		t.Fatalf("engine started %d times for bad input: %q", len(argvs), argvs)
	}
}

func TestDeployHandlerAnswersNormalisedRecord(t *testing.T) {
	r, argvLog := newTestDeploys(t)
	w := post(r, `{"environments":[{"name":"production","expected_sha":"`+goodSHA+`"}],"wait":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body)
	}
	var job jobs.Job
	if err := json.Unmarshal(w.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	if job.State != jobs.StateDeployed || job.Results[0].DeployedSHA != goodSHA {
		t.Fatalf("job = %+v", job)
	}
	argvs, _ := fakeengine.ReadArgvLog(argvLog)
	if len(argvs) != 1 {
		t.Fatalf("engine ran %d times", len(argvs))
	}
}

func TestDeployListLimitValidation(t *testing.T) {
	r, _ := newTestDeploys(t)
	for _, q := range []string{"limit=0", "limit=101", "limit=x"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/deploys?"+q, nil))
		if w.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d", q, w.Code)
		}
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/deploys", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"deploys":[]`) {
		t.Fatalf("empty list = %d %s", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/deploys/nope", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown id = %d", w.Code)
	}
}

// TestAsyncDeployG10k is the fork's plan 57.6-06 tracer. The configuration is
// loaded through config.Load with engine.name g10k, so the engine and its argv
// come from internal/engine exactly as in production. The fake g10k replays
// internal/jobs/testdata/g10k-0.10.0/deploy-success.json, a verbatim copy of
// stagehand-sdk-public harness/fixtures/g10k-0.10.0/deploy-success.json (a real
// g10k 0.10.0 run recorded by the SDK harness), after sleeping, so the test can
// watch the job move from queued to deployed.
//
// FORK-WR-05: the fork asserted that the POST returned within 100 ms, which
// fails on a busy runner. "Answers before the engine finishes" is proved by
// state instead: the 202 body itself says "queued" while the engine has not yet
// run, and a later GET sees the job move forward to deployed, never backward.
func TestAsyncDeployG10k(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f := newMTLSFixture(t, "stagehand-code-manager")
	basedir := t.TempDir()
	argvLog := filepath.Join(t.TempDir(), "argv.log")
	fixture, _ := filepath.Abs("../jobs/testdata/g10k-0.10.0/deploy-success.json")

	cfg := loadConfig(t, f, fmt.Sprintf(`  basedir: %q
  timeout: 1m
  output_limit: 4096
engine:
  name: g10k
  command_path: %q
  config_path: /etc/puppetlabs/r10k/g10k.yaml
  verbose: false
`, basedir, exe))

	t.Setenv(fakeengine.FixtureEnv, fixture)
	t.Setenv(fakeengine.BasedirEnv, basedir)
	t.Setenv(fakeengine.SHAEnv, goodSHA)
	t.Setenv(fakeengine.ArgvLogEnv, argvLog)
	t.Setenv(fakeengine.DelayEnv, "600ms")

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
	r := srv.Handler()

	w := post(r, `{"environments":[{"name":"production","expected_sha":"`+goodSHA+`"}],"wait":false}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body %s", w.Code, w.Body)
	}
	var accepted struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	// The answer was written while the engine was still asleep (600 ms): that
	// is what "answers at once" means, stated as state rather than as a clock reading.
	if accepted.ID == "" || accepted.State != "queued" {
		t.Fatalf("202 body = %s", w.Body)
	}
	if loc := w.Header().Get("Location"); loc != "/api/v1/deploys/"+accepted.ID {
		t.Fatalf("Location = %q", loc)
	}

	rank := map[jobs.State]int{jobs.StateQueued: 0, jobs.StateDeploying: 1, jobs.StateDeployed: 2, jobs.StateFailed: 2}
	var seen []jobs.State
	var job jobs.Job
	deadline := time.Now().Add(30 * time.Second)
	for {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/deploys/"+accepted.ID, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("GET status = %d, body %s", w.Code, w.Body)
		}
		job = jobs.Job{}
		if err := json.Unmarshal(w.Body.Bytes(), &job); err != nil {
			t.Fatal(err)
		}
		if _, ok := rank[job.State]; !ok {
			t.Fatalf("unexpected state %q", job.State)
		}
		if n := len(seen); n > 0 && rank[job.State] < rank[seen[n-1]] {
			t.Fatalf("state went backward: %v then %s", seen, job.State)
		}
		if len(seen) == 0 || seen[len(seen)-1] != job.State {
			seen = append(seen, job.State)
		}
		if job.State == jobs.StateDeployed || job.State == jobs.StateFailed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job still %s after 30s (states %v)", job.State, seen)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if seen[len(seen)-1] != jobs.StateDeployed {
		t.Fatalf("states %v, job %+v", seen, job)
	}
	res := job.Results[0]
	if job.Engine != "g10k" || res.State != jobs.StateDeployed || res.DeployedSHA != goodSHA ||
		!res.MarkerPresent || !res.MarkerDeploySuccess || res.ExitCode != 0 || res.Error != "" {
		t.Fatalf("job = %+v", job)
	}
	if !strings.Contains(res.Output, "Synced /srv/work/g10k/g10k.yaml with branch production") {
		t.Fatalf("output = %q", res.Output)
	}
	if _, err := os.Stat(filepath.Join(basedir, "production", ".g10k-deploy.json")); err != nil {
		t.Fatal(err)
	}

	argvs, err := fakeengine.ReadArgvLog(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-config=/etc/puppetlabs/r10k/g10k.yaml", "-branch=production"}
	if len(argvs) != 1 || strings.Join(argvs[0], "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv = %q, want exactly %q (an argv, no shell)", argvs, want)
	}

	// The record is a file under deploys.state_dir/jobs, so it outlives the process.
	if _, err := os.Stat(filepath.Join(f.stateDir, "jobs", accepted.ID+".json")); err != nil {
		t.Fatal(err)
	}
}

// Once the service is shutting down a deploy is refused with 503 and its
// record reads failed, never queued.
func TestDeployRefusedWhileShuttingDown(t *testing.T) {
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
	r := NewMux(NewDeployHandlers(runner, slog.New(slog.NewTextHandler(io.Discard, nil))), nil)
	if err := runner.Shutdown(time.Second); err != nil {
		t.Fatal(err)
	}
	for _, wait := range []string{"true", "false"} {
		w := post(r, `{"environments":[{"name":"production","expected_sha":"`+goodSHA+`"}],"wait":`+wait+`}`)
		if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
			t.Fatalf("wait %s: status = %d, body %s", wait, w.Code, w.Body)
		}
		var e struct{ ID string }
		_ = json.Unmarshal(w.Body.Bytes(), &e)
		job, err := runner.Store().Get(e.ID)
		if err != nil || job.State != jobs.StateFailed || job.Results[0].Error != jobs.ErrShutdown.Error() {
			t.Fatalf("wait %s: record = %+v, %v", wait, job, err)
		}
	}
}

// deployBody is a valid wait:false request whose total length is exactly n
// bytes, padded inside an unknown top-level field.
func deployBody(t *testing.T, n int) string {
	t.Helper()
	const head = `{"environments":[{"name":"production","expected_sha":"` + goodSHA + `"}],"wait":false,"pad":"`
	const tail = `"}`
	pad := n - len(head) - len(tail)
	if pad < 0 {
		t.Fatalf("cannot build a %d byte body", n)
	}
	return head + strings.Repeat("x", pad) + tail
}

// FORK-IN-06: a body over the 64 KiB cap is a size problem, not a syntax
// problem, so it answers 413 with its own message. The cap itself is still
// accepted.
func TestDeployOversizedBody(t *testing.T) {
	if want := fmt.Sprintf(`{"error":"request body exceeds %d bytes"}`, maxDeployBody); want != `{"error":"request body exceeds 65536 bytes"}` {
		t.Fatalf("maxDeployBody = %d no longer matches the contract text %s", maxDeployBody, want)
	}
	r, argvLog := newTestDeploys(t)

	for name, body := range map[string]string{
		"one byte over the cap":             deployBody(t, maxDeployBody+1),
		"far over the cap":                  deployBody(t, 4*maxDeployBody),
		"whitespace after a valid object":   deployBody(t, 200) + strings.Repeat(" ", maxDeployBody),
		"newlines after a valid object":     deployBody(t, 200) + strings.Repeat("\n", maxDeployBody),
		"cap exceeded by data after object": deployBody(t, maxDeployBody) + " x",
	} {
		t.Run(name, func(t *testing.T) {
			w := post(r, body)
			if w.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d, body %.200s", w.Code, w.Body)
			}
			if got, want := w.Body.String(), `{"error":"request body exceeds 65536 bytes"}`; got != want {
				t.Fatalf("body = %q, want %q", got, want)
			}
			if ct := w.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
				t.Fatalf("Content-Type = %q", ct)
			}
		})
	}

	if w := post(r, deployBody(t, maxDeployBody)); w.Code != http.StatusAccepted {
		t.Fatalf("a body of exactly %d bytes: status = %d, body %.200s", maxDeployBody, w.Code, w.Body)
	}
	// Only the exactly-at-the-cap request started an engine.
	deadline := time.Now().Add(15 * time.Second)
	for {
		argvs, err := fakeengine.ReadArgvLog(argvLog)
		if err == nil && len(argvs) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("engine runs = %d (%v), want exactly 1", len(argvs), err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// FORK-IN-06: data after the JSON object is refused rather than ignored.
func TestDeployTrailingData(t *testing.T) {
	r, argvLog := newTestDeploys(t)
	const obj = `{"environments":[{"name":"production","expected_sha":"` + goodSHA + `"}],"wait":false}`
	for name, body := range map[string]string{
		"text after the object":   obj + " garbage",
		"second object":           obj + obj,
		"a stray closing brace":   obj + "}",
		"a comma":                 obj + ",",
		"an unterminated value":   obj + ` {"a":`,
		"a number after a space":  obj + " 7",
		"a bare literal after nl": obj + "\nnull",
	} {
		t.Run(name, func(t *testing.T) {
			w := post(r, body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body %s", w.Code, w.Body)
			}
			if got, want := w.Body.String(), `{"error":"request body is not valid JSON: unexpected data after the JSON object"}`; got != want {
				t.Fatalf("body = %q, want %q", got, want)
			}
		})
	}
	// Whitespace after the object is not data.
	if w := post(r, obj+" \n\t "); w.Code != http.StatusAccepted {
		t.Fatalf("trailing whitespace: status = %d, body %s", w.Code, w.Body)
	}
	// Refused requests never created a job or started an engine.
	deadline := time.Now().Add(15 * time.Second)
	for {
		argvs, err := fakeengine.ReadArgvLog(argvLog)
		if err == nil && len(argvs) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("engine runs = %d (%v), want exactly 1 (the whitespace request)", len(argvs), err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/deploys", nil))
	var list struct {
		Deploys []jobs.Job `json:"deploys"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Deploys) != 1 {
		t.Fatalf("list = %s (%v), want exactly one job", w.Body, err)
	}
}

// FORK-IN-06 keeps unknown fields allowed: only trailing data is new, so a
// pack that adds a field in a later version is not turned away.
func TestDeployUnknownFieldAccepted(t *testing.T) {
	r, _ := newTestDeploys(t)
	w := post(r, `{"environments":[{"name":"production","expected_sha":"`+goodSHA+`","extra":1}],"wait":false,"unknown":{"a":[1,2]}}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body %s", w.Code, w.Body)
	}
}
