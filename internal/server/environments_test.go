package server

// DELETE /api/v1/environments/{env} and GET /api/v1/removals/{id} (D-09, D-10,
// D-20). Every test goes through the real mux, so a route-pattern conflict
// panics here (route-safety).

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/puppet-stagehand/stagehand-deploy-agent/internal/jobs"
)

// envRig is the agent's real mux over a runner whose basedir holds the
// directories a test made.
type envRig struct {
	mux     http.Handler
	runner  *jobs.Runner
	basedir string
}

func newEnvRig(t *testing.T, protected []string) *envRig {
	t.Helper()
	basedir := t.TempDir()
	runner, err := jobs.NewRunner(jobs.Options{
		Engine:      jobs.R10kEngine(func(env string) []string { return []string{"/bin/false"} }),
		Basedir:     basedir,
		Timeout:     time.Minute,
		OutputLimit: 4096,
		Store:       jobs.NewMemoryStore(10),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runner.Shutdown(10 * time.Second) })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &envRig{
		mux:     NewMux(NewDeployHandlers(runner, log), NewEnvironmentHandlers(runner, protected, log)),
		runner:  runner,
		basedir: basedir,
	}
}

func (g *envRig) mkdir(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(g.basedir, name)
	if err := os.MkdirAll(filepath.Join(dir, "manifests"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func (g *envRig) do(method, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	g.mux.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	return w
}

func (g *envRig) waitFor(t *testing.T, id string, want jobs.State) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		w := g.do(http.MethodGet, "/api/v1/removals/"+id)
		var rec map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &rec); err != nil {
			t.Fatalf("removal body %q: %v", w.Body, err)
		}
		if rec["state"] == string(want) {
			return rec
		}
		if time.Now().After(deadline) {
			t.Fatalf("removal %s never reached %s: %s", id, want, w.Body)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDeleteEnvironmentQueuesARemovalAndItReachesDeployed(t *testing.T) {
	g := newEnvRig(t, []string{"production"})
	dir := g.mkdir(t, "feature_x")

	w := g.do(http.MethodDelete, "/api/v1/environments/feature_x")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body %s", w.Code, w.Body)
	}
	var rec struct {
		ID    string `json:"id"`
		Kind  string `json:"kind"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Kind != "remove" || rec.State != "queued" || len(rec.ID) != 32 {
		t.Fatalf("202 body = %s", w.Body)
	}
	if loc := w.Header().Get("Location"); loc != "/api/v1/removals/"+rec.ID {
		t.Fatalf("Location = %q", loc)
	}

	final := g.waitFor(t, rec.ID, jobs.StateDeployed)
	if final["kind"] != "remove" {
		t.Errorf("polled record = %v", final)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("directory still there: %v", err)
	}
}

func TestDeleteProtectedEnvironmentIsRefusedAndQueuesNothing(t *testing.T) {
	g := newEnvRig(t, []string{"production"})
	dir := g.mkdir(t, "production")

	w := g.do(http.MethodDelete, "/api/v1/environments/production")
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body %s", w.Code, w.Body)
	}
	if want := `{"error":"environment production is protected","field":"environment"}`; w.Body.String() != want {
		t.Fatalf("body = %s, want %s", w.Body, want)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("the protected directory was touched: %v", err)
	}
	if list, _ := g.runner.Store().List(10); len(list) != 0 {
		t.Fatalf("a refused removal was recorded: %+v", list)
	}
}

func TestDeleteHonoursAConfiguredProtectedList(t *testing.T) {
	g := newEnvRig(t, []string{"production", "staging"})
	g.mkdir(t, "staging")
	g.mkdir(t, "feature_x")

	w := g.do(http.MethodDelete, "/api/v1/environments/staging")
	if w.Code != http.StatusForbidden || w.Body.String() != `{"error":"environment staging is protected","field":"environment"}` {
		t.Fatalf("staging: status = %d, body %s", w.Code, w.Body)
	}
	if w := g.do(http.MethodDelete, "/api/v1/environments/feature_x"); w.Code != http.StatusAccepted {
		t.Fatalf("an unprotected name: status = %d, body %s", w.Code, w.Body)
	}
}

func TestDeleteRefusesABadName(t *testing.T) {
	g := newEnvRig(t, nil)
	for _, name := range []string{"Bad-Name", "has.dot", "with%20space", "x%3By"} {
		w := g.do(http.MethodDelete, "/api/v1/environments/"+name)
		if w.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, body %s", name, w.Code, w.Body)
			continue
		}
		if want := `{"error":"environment: must match ^[a-z0-9_]+$","field":"environment"}`; w.Body.String() != want {
			t.Errorf("%s: body = %s, want %s", name, w.Body, want)
		}
	}
}

func TestDeleteAMissingEnvironmentIs404(t *testing.T) {
	g := newEnvRig(t, []string{"production"})
	w := g.do(http.MethodDelete, "/api/v1/environments/missing")
	if w.Code != http.StatusNotFound || w.Body.String() != `{"error":"environment not found"}` {
		t.Fatalf("status = %d, body %s", w.Code, w.Body)
	}
}

func TestDeleteWhileShuttingDownAnswers503AndRecordsTheRefusal(t *testing.T) {
	g := newEnvRig(t, nil)
	dir := g.mkdir(t, "feature_x")
	if err := g.runner.Shutdown(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	w := g.do(http.MethodDelete, "/api/v1/environments/feature_x")
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "30" {
		t.Fatalf("status = %d, Retry-After %q, body %s", w.Code, w.Header().Get("Retry-After"), w.Body)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body["error"] != jobs.ErrShutdown.Error() || len(body["id"]) != 32 {
		t.Fatalf("body = %s", w.Body)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("a refused removal touched the directory: %v", err)
	}
	// The id in the 503 is on record as failed, never queued.
	rec := g.do(http.MethodGet, "/api/v1/removals/"+body["id"])
	if rec.Code != http.StatusOK {
		t.Fatalf("refused removal status = %d, body %s", rec.Code, rec.Body)
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["state"] != "failed" || got["kind"] != "remove" {
		t.Fatalf("refused removal = %s", rec.Body)
	}
}

func TestDeleteGetRemovalOfAnUnknownIdIs404(t *testing.T) {
	g := newEnvRig(t, nil)
	for _, id := range []string{"0123456789abcdef0123456789abcdef", "nope"} {
		w := g.do(http.MethodGet, "/api/v1/removals/"+id)
		if w.Code != http.StatusNotFound || w.Body.String() != `{"error":"removal not found"}` {
			t.Errorf("%s: status = %d, body %s", id, w.Code, w.Body)
		}
	}
}

// A removal is polled at /api/v1/removals and never appears under
// /api/v1/deploys: the pack's deploy calls and the golden corpus must not see
// it (D-20, PD-21).
func TestDeploysListExcludesRemovals(t *testing.T) {
	g := newEnvRig(t, nil)
	store := g.runner.Store()
	now := time.Now().UTC()
	mk := func(i int, kind string) jobs.Job {
		created := now.Add(time.Duration(i) * time.Second)
		return jobs.Job{
			ID: fmt.Sprintf("%032x", i+1), Engine: "r10k", Kind: kind, State: jobs.StateDeployed,
			CreatedAt: created, StartedAt: &created, FinishedAt: &created,
			Results: []jobs.EnvResult{{Environment: "production", State: jobs.StateDeployed}},
		}
	}
	var deployIDs, removalIDs []string
	for i := 0; i < 5; i++ {
		kind := ""
		if i == 1 || i == 2 || i == 4 {
			kind = jobs.KindRemove
		}
		j := mk(i, kind)
		if err := store.Save(j); err != nil {
			t.Fatal(err)
		}
		if kind == "" {
			deployIDs = append(deployIDs, j.ID)
		} else {
			removalIDs = append(removalIDs, j.ID)
		}
	}

	w := g.do(http.MethodGet, "/api/v1/deploys?limit=20")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body)
	}
	var body struct {
		Deploys []struct {
			ID   string `json:"id"`
			Kind string `json:"kind"`
		} `json:"deploys"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Deploys) != 2 {
		t.Fatalf("listed %d records, want exactly the 2 deploys: %s", len(body.Deploys), w.Body)
	}
	for _, d := range body.Deploys {
		if d.Kind != "" || (d.ID != deployIDs[0] && d.ID != deployIDs[1]) {
			t.Errorf("listed %+v, want only deploys %v", d, deployIDs)
		}
	}
	// limit counts deploys, not records of every kind.
	w = g.do(http.MethodGet, "/api/v1/deploys?limit=2")
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || len(body.Deploys) != 2 {
		t.Fatalf("limit=2 listed %d: %s", len(body.Deploys), w.Body)
	}

	for _, id := range removalIDs {
		w := g.do(http.MethodGet, "/api/v1/deploys/"+id)
		if w.Code != http.StatusNotFound || w.Body.String() != `{"error":"deploy not found"}` {
			t.Errorf("GET deploys/%s: status = %d, body %s", id, w.Code, w.Body)
		}
		if w := g.do(http.MethodGet, "/api/v1/removals/"+id); w.Code != http.StatusOK {
			t.Errorf("GET removals/%s: status = %d, body %s", id, w.Code, w.Body)
		}
	}
	// And the other way round: a deploy id is not a removal.
	if w := g.do(http.MethodGet, "/api/v1/removals/"+deployIDs[0]); w.Code != http.StatusNotFound || w.Body.String() != `{"error":"removal not found"}` {
		t.Errorf("GET removals/<deploy id>: status = %d, body %s", w.Code, w.Body)
	}
	if w := g.do(http.MethodGet, "/api/v1/deploys/"+deployIDs[0]); w.Code != http.StatusOK {
		t.Errorf("GET deploys/<deploy id>: status = %d", w.Code)
	}
}
