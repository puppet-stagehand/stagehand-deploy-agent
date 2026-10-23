package contract_test

// TestGolden replays the webhook-go fork's own recorded responses
// (contract/golden, recorded by hack/record-golden.sh from the tag
// v2.15.1-stagehand.1) against the agent's real mux and holds the agent to
// them byte for byte. Cases listed in contract.Divergences must differ from the
// fork and equal the table entry; every other case must equal the fork.
//
// Normalisation is deliberately narrow (it mirrors the recorder): 32-hex job
// ids and non-zero RFC3339 values of created_at / started_at / finished_at are
// replaced by placeholders, and for cases flagged normalise_duration a
// duration_ms integer is replaced. The zero time 0001-01-01T00:00:00Z and every
// byte of whitespace stay literal, so tidying the record shape cannot pass.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/puppet-stagehand/stagehand-deploy-agent/contract"
	"github.com/puppet-stagehand/stagehand-deploy-agent/internal/jobs"
	"github.com/puppet-stagehand/stagehand-deploy-agent/internal/jobs/fakeengine"
	"github.com/puppet-stagehand/stagehand-deploy-agent/internal/server"
)

func TestMain(m *testing.M) {
	fakeengine.MaybeRun() // becomes the fake r10k when the fixture env var is set
	os.Exit(m.Run())
}

const (
	goldenDir = "golden"
	zeroTime  = "0001-01-01T00:00:00Z"
)

var (
	idRE   = regexp.MustCompile(`\b[0-9a-f]{32}\b`)
	timeRE = regexp.MustCompile(`"(created_at|started_at|finished_at)":"([^"]+)"`)
	durRE  = regexp.MustCompile(`"duration_ms":[0-9]+`)
)

// normalise replaces ids and non-zero RFC3339 times, and nothing else. The
// recorder (contract/recorder/record_test.go.txt) applies the same rules.
func normalise(b []byte, durations bool) []byte {
	b = idRE.ReplaceAll(b, []byte("<id>"))
	b = timeRE.ReplaceAllFunc(b, func(m []byte) []byte {
		sub := timeRE.FindSubmatch(m)
		if string(sub[2]) == zeroTime {
			return m
		}
		return []byte(`"` + string(sub[1]) + `":"<time>"`)
	})
	if durations {
		b = durRE.ReplaceAll(b, []byte(`"duration_ms":<ms>`))
	}
	return b
}

type goldenCase struct {
	Name              string `json:"name"`
	Method            string `json:"method"`
	Path              string `json:"path"`
	Body              string `json:"body"`
	Pad               int    `json:"pad"`
	Setup             string `json:"setup"`
	NormaliseDuration bool   `json:"normalise_duration"`
}

type goldenCases struct {
	SHA   string       `json:"sha"`
	Cases []goldenCase `json:"cases"`
}

type goldenMeta struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
}

func loadCases(t *testing.T) goldenCases {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(goldenDir, "cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cs goldenCases
	if err := json.Unmarshal(raw, &cs); err != nil {
		t.Fatal(err)
	}
	return cs
}

// rig is the agent's deploy API over a real mux and a plain-HTTP listener (the
// handshake is not part of the byte contract), with the fake r10k behind it.
type rig struct {
	url    string
	runner *jobs.Runner
	client *http.Client
}

func newRig(t *testing.T, sha string) *rig {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	basedir, stateDir := t.TempDir(), t.TempDir()
	fixture, _ := filepath.Abs("../internal/jobs/testdata/r10k-5.0.3/deploy-success.json")
	t.Setenv(fakeengine.FixtureEnv, fixture)
	t.Setenv(fakeengine.BasedirEnv, basedir)
	t.Setenv(fakeengine.SHAEnv, sha)

	store, err := jobs.NewFileStore(stateDir, 10)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := jobs.NewRunner(jobs.Options{
		Engine: jobs.R10kEngine(func(env string) []string {
			return []string{exe, "deploy", "environment", env, "--config=/etc/puppetlabs/r10k/r10k.yaml"}
		}),
		Basedir:     basedir,
		Timeout:     time.Minute,
		OutputLimit: 4096,
		Store:       store,
		LockPath:    filepath.Join(stateDir, "deploy.lock"),
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.NewMux(server.NewDeployHandlers(runner, nil), nil))
	t.Cleanup(func() {
		srv.Close()
		_ = runner.Shutdown(10 * time.Second)
		_ = store.Close()
	})
	return &rig{
		url:    srv.URL,
		runner: runner,
		client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}
}

func (r *rig) do(t *testing.T, method, path, body string) (*http.Response, []byte) {
	t.Helper()
	var rdr io.Reader
	if method == http.MethodPost {
		rdr = bytes.NewBufferString(body)
	}
	req, err := http.NewRequest(method, r.url+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := r.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, b
}

func TestGolden(t *testing.T) {
	cs := loadCases(t)
	if len(cs.Cases) < 17 {
		t.Fatalf("corpus has %d cases, want at least 17", len(cs.Cases))
	}
	for name := range contract.Divergences {
		found := false
		for _, c := range cs.Cases {
			found = found || c.Name == name
		}
		if !found {
			t.Errorf("Divergences names %q, which is not a recorded case", name)
		}
	}

	for _, c := range cs.Cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			rawMeta, err := os.ReadFile(filepath.Join(goldenDir, c.Name+".meta.json"))
			if err != nil {
				t.Fatal(err)
			}
			var meta goldenMeta
			if err := json.Unmarshal(rawMeta, &meta); err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(filepath.Join(goldenDir, c.Name+".body"))
			if err != nil {
				t.Fatal(err)
			}

			r := newRig(t, cs.SHA)
			switch c.Setup {
			case "":
			case "finished_job":
				resp, body := r.do(t, http.MethodPost, "/api/v1/deploys",
					`{"environments":[{"name":"production","expected_sha":"`+cs.SHA+`"}],"wait":true}`)
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("setup deploy status = %d, body %s", resp.StatusCode, body)
				}
			case "shutdown":
				if err := r.runner.Shutdown(time.Second); err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatalf("unknown setup %q", c.Setup)
			}

			body := c.Body
			if c.Pad > 0 {
				body = strings.Replace(body, "{{PAD}}", strings.Repeat("x", c.Pad), 1)
			}
			resp, raw := r.do(t, c.Method, c.Path, body)
			got := normalise(raw, c.NormaliseDuration)

			if d, ok := contract.Divergences[c.Name]; ok {
				if resp.StatusCode == meta.Status && bytes.Equal(got, want) {
					t.Fatalf("%s is listed as a divergence but the agent answers exactly as the fork did", c.Name)
				}
				if resp.StatusCode != d.Status {
					t.Errorf("status = %d, divergence table says %d (body %q)", resp.StatusCode, d.Status, raw)
				}
				if ct := resp.Header.Get("Content-Type"); ct != d.ContentType {
					t.Errorf("Content-Type = %q, divergence table says %q", ct, d.ContentType)
				}
				if string(got) != d.Body {
					t.Errorf("body = %q, divergence table says %q", got, d.Body)
				}
				if d.Reason == "" {
					t.Error("divergence has no reason")
				}
				return
			}

			if resp.StatusCode != meta.Status {
				t.Errorf("status = %d, fork answered %d (body %q)", resp.StatusCode, meta.Status, raw)
			}
			for _, h := range []string{"Content-Type", "Location", "Retry-After"} {
				gotH := string(idRE.ReplaceAll([]byte(resp.Header.Get(h)), []byte("<id>")))
				if wantH := meta.Headers[h]; gotH != wantH {
					t.Errorf("header %s = %q, fork answered %q", h, gotH, wantH)
				}
			}
			if !bytes.Equal(got, want) {
				t.Errorf("body differs from the fork's\n got: %q\nwant: %q", got, want)
			}
		})
	}
}

// TestGoldenIsNormalisedOnly guards the corpus itself: a recorded body may hold
// placeholders but no raw job id or non-zero timestamp, and the zero time of a
// queued result must still be there, literally.
func TestGoldenIsNormalisedOnly(t *testing.T) {
	cs := loadCases(t)
	rawID := regexp.MustCompile(`"id":"[0-9a-f]`)
	for _, c := range cs.Cases {
		b, err := os.ReadFile(filepath.Join(goldenDir, c.Name+".body"))
		if err != nil {
			t.Fatal(err)
		}
		if rawID.Match(b) {
			t.Errorf("%s: raw job id left in the golden body", c.Name)
		}
		for _, m := range timeRE.FindAllSubmatch(b, -1) {
			if v := string(m[2]); v != zeroTime && v != "<time>" {
				t.Errorf("%s: raw time %q left in the golden body", c.Name, v)
			}
		}
	}
	b, err := os.ReadFile(filepath.Join(goldenDir, "post_wait_false.body"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(`"started_at":"`+zeroTime+`"`)) {
		t.Error("post_wait_false.body lost the literal zero time")
	}
	if bytes.HasSuffix(b, []byte("\n")) {
		t.Error("post_wait_false.body ends with a newline; gin writes none")
	}
}
