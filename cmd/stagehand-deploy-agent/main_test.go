package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/puppet-stagehand/stagehand-deploy-agent/internal/jobs"
)

const testdata = "../../internal/config/testdata/"

// notRoot makes every test independent of who runs it: a CI container often
// runs as root, and serve refuses that (D-20).
func notRoot(t *testing.T) {
	t.Helper()
	old := geteuid
	geteuid = func() int { return 1000 }
	t.Cleanup(func() { geteuid = old })
}

func runCLI(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var o, e bytes.Buffer
	code = run(args, &o, &e)
	return code, o.String(), e.String()
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return lines[len(lines)-1]
}

// --check-config exits 78 (EX_CONFIG) on a refusal and names the key.
func TestCheckConfigExits78OnEachMistake(t *testing.T) {
	notRoot(t)
	for file, key := range map[string]string{
		"timeout_number.yml":    "deploys.timeout",
		"timeout_subsecond.yml": "deploys.timeout",
		"port_zero.yml":         "server.port",
		"bad_log_level.yml":     "log.level",
		"unknown_key_top.yml":   "surprise",
		"second_document.yml":   "document",
		"bad_ssh_config.yml":    "engine.ssh_config",
	} {
		t.Run(file, func(t *testing.T) {
			path := testdata + file
			code, out, _ := runCLI(t, "--check-config", "--config", path)
			if code != 78 {
				t.Fatalf("exit = %d, want 78\n%s", code, out)
			}
			if !strings.Contains(out, "ERROR "+key+": ") {
				t.Fatalf("output does not name %s:\n%s", key, out)
			}
			if !strings.Contains(lastLine(out), " 1 error(s), ") {
				t.Fatalf("last line = %q", lastLine(out))
			}
		})
	}
}

func TestCheckConfigExits0OnAValidFile(t *testing.T) {
	notRoot(t)
	code, out, _ := runCLI(t, "--check-config", "--config", testdata+"valid.yml")
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, out)
	}
	if !strings.Contains(lastLine(out), "0 error(s)") {
		t.Fatalf("last line = %q", lastLine(out))
	}
	// The key lives under /run/credentials, which exists only inside the unit.
	if !strings.Contains(out, "WARNING server.tls.key: ") || !strings.Contains(out, "/run/credentials/stagehand-deploy-agent.service/server.key") {
		t.Fatalf("no credential warning:\n%s", out)
	}
}

func TestCheckConfigPublicOpsListenerWarnsAndExits0(t *testing.T) {
	notRoot(t)
	code, out, _ := runCLI(t, "--check-config", "--config", testdata+"listen_public.yml")
	if code != 0 || !strings.Contains(out, "WARNING ops.listen: ") || !strings.Contains(out, "unauthenticated") {
		t.Fatalf("exit = %d\n%s", code, out)
	}
}

func TestCheckConfigExampleIsClean(t *testing.T) {
	notRoot(t)
	code, out, _ := runCLI(t, "--check-config", "--config", "../../packaging/config.example.yml")
	if code != 0 || !strings.Contains(lastLine(out), "0 error(s)") {
		t.Fatalf("exit = %d\n%s", code, out)
	}
}

func TestCheckConfigWithNoFileAnywhereIs78(t *testing.T) {
	notRoot(t)
	code, out, _ := runCLI(t, "--check-config", "--config", filepath.Join(t.TempDir(), "absent.yml"))
	if code != 78 || !strings.Contains(out, "ERROR config: ") {
		t.Fatalf("exit = %d\n%s", code, out)
	}
}

func TestUsageErrorsExit2(t *testing.T) {
	notRoot(t)
	for _, args := range [][]string{
		nil,
		{"frobnicate"},
		{"--legacy"},
		{"--check-config", "--legacy"},
		{"--config", "x.yml"},
	} {
		if code, _, _ := runCLI(t, args...); code != 2 {
			t.Errorf("run(%q) = %d, want 2", args, code)
		}
	}
}

func TestVersionPrints(t *testing.T) {
	notRoot(t)
	code, out, _ := runCLI(t, "version")
	if code != 0 || !strings.HasPrefix(out, "stagehand-deploy-agent ") {
		t.Fatalf("exit = %d out = %q", code, out)
	}
}

// The fork's unedited build/webhook.yml sets server.protected: true, so it
// refuses; --legacy lets --check-config read a legacy file at another path.
func TestCheckConfigLegacyShippedFileRefuses(t *testing.T) {
	notRoot(t)
	code, out, _ := runCLI(t, "--check-config", "--config", testdata+"legacy_shipped.yml", "--legacy")
	if code != 78 || !strings.Contains(out, "ERROR server.protected: ") || !strings.Contains(out, "ERROR server.tls.client_ca: ") {
		t.Fatalf("exit = %d\n%s", code, out)
	}
	if !strings.Contains(out, "WARNING config: using legacy config /etc/voxpupuli/webhook.yml") {
		t.Fatalf("no fallback warning:\n%s", out)
	}
}

func TestCheckConfigLegacyModuleFileIsAccepted(t *testing.T) {
	notRoot(t)
	code, out, _ := runCLI(t, "--check-config", "--config", testdata+"legacy_module.yml", "--legacy")
	if code != 0 || !strings.Contains(out, "1.2.0") || !strings.Contains(lastLine(out), "0 error(s)") {
		t.Fatalf("exit = %d\n%s", code, out)
	}
}

// Without --legacy the same file is read as a native one, and is refused.
func TestCheckConfigReadsAnExplicitFileAsNative(t *testing.T) {
	notRoot(t)
	code, out, _ := runCLI(t, "--check-config", "--config", testdata+"legacy_module.yml")
	if code != 78 || !strings.Contains(out, "ERROR r10k: unknown key") {
		t.Fatalf("exit = %d\n%s", code, out)
	}
}

// --legacy exists for --check-config only: serve reaches the legacy reader
// through config.Locate alone.
func TestServeLegacyFlagIsAUsageError(t *testing.T) {
	notRoot(t)
	if code, _, _ := runCLI(t, "serve", "--legacy", "--config", testdata+"legacy_module.yml"); code != 2 {
		t.Fatalf("serve --legacy exit = %d, want 2", code)
	}
}

// D-20: serve run as uid 0 exits 77 and says what to do instead. There is no
// override: check-config and version still run as root.
func TestServeRefusesRoot(t *testing.T) {
	old := geteuid
	geteuid = func() int { return 0 }
	t.Cleanup(func() { geteuid = old })

	code, _, errOut := runCLI(t, "serve", "--config", testdata+"valid.yml")
	if code != 77 || !strings.Contains(errOut, "refusing to run as root") || !strings.Contains(errOut, "its own user") {
		t.Fatalf("serve as root: exit = %d stderr = %q", code, errOut)
	}
	if code, _, _ := runCLI(t, "version"); code != 0 {
		t.Errorf("version as root = %d, want 0", code)
	}
	if code, _, _ := runCLI(t, "--check-config", "--config", testdata+"valid.yml"); code != 0 {
		t.Errorf("--check-config as root = %d, want 0", code)
	}
	// No flag turns the refusal off.
	if code, _, _ := runCLI(t, "serve", "--run-as-root", "--config", testdata+"valid.yml"); code != 77 {
		t.Errorf("serve --run-as-root as root = %d, want 77", code)
	}
}

// As any other user serve goes on to load its configuration.
func TestServeAsAnotherUserLoadsItsConfig(t *testing.T) {
	notRoot(t)
	code, _, errOut := runCLI(t, "serve", "--config", filepath.Join(t.TempDir(), "absent.yml"))
	if code != 78 || strings.Contains(errOut, "root") {
		t.Fatalf("exit = %d stderr = %q, want 78 from the config step", code, errOut)
	}
	if code, _, _ := runCLI(t, "serve", "--run-as-root"); code != 2 {
		t.Errorf("serve --run-as-root = %d, want 2 (no such flag)", code)
	}
}

// writeImportConfig writes a valid config whose state_dir is stateDir.
func writeImportConfig(t *testing.T, stateDir string, maxJobs int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.yml")
	body := "server:\n  tls:\n    certificate: /etc/ssl/server.pem\n    key: /etc/ssl/server.key\n    client_ca: /etc/ssl/ca.pem\n    allowed_certnames: [stagehand-code-manager]\n" +
		"deploys:\n  state_dir: " + stateDir + "\n  max_jobs: " + strconv.Itoa(maxJobs) + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeLegacyRecords writes n finished and k unfinished fork records.
func writeLegacyRecords(t *testing.T, dir string, finished, unfinished int) {
	t.Helper()
	base := time.Now().Add(-time.Hour)
	write := func(i int, state jobs.State) {
		created := base.Add(time.Duration(i) * time.Minute)
		j := jobs.Job{ID: fmt.Sprintf("%032x", i+1), Engine: "r10k", State: state, CreatedAt: created,
			Results: []jobs.EnvResult{{Environment: "production", State: state, ExpectedSHA: strings.Repeat("a", 40)}}}
		b, _ := json.Marshal(j)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		m["owner"] = strings.Repeat("e", 32)
		b, _ = json.Marshal(m)
		if err := os.WriteFile(filepath.Join(dir, j.ID+".json"), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < finished; i++ {
		write(i, jobs.StateDeployed)
	}
	for i := 0; i < unfinished; i++ {
		write(finished+i, jobs.StateDeploying)
	}
}

// The output line is what plan 09's cutover script parses; its wording is fixed.
func TestImportHistoryCLIPrintsOneLineAndIsIdempotent(t *testing.T) {
	notRoot(t)
	from := t.TempDir()
	writeLegacyRecords(t, from, 3, 1)
	cfg := writeImportConfig(t, filepath.Join(t.TempDir(), "state"), 500)

	code, out, errOut := runCLI(t, "import-history", "--from", from, "--config", cfg)
	if code != 0 || out != "imported 3; skipped 0 already present, 1 unfinished, 0 unreadable\n" {
		t.Fatalf("exit = %d, stdout %q, stderr %q", code, out, errOut)
	}
	code, out, _ = runCLI(t, "import-history", "--from", from, "--config", cfg)
	if code != 0 || out != "imported 0; skipped 3 already present, 1 unfinished, 0 unreadable\n" {
		t.Fatalf("second run: exit = %d, stdout %q", code, out)
	}
}

func TestImportHistoryCLIExitCodes(t *testing.T) {
	notRoot(t)
	good := writeImportConfig(t, filepath.Join(t.TempDir(), "state"), 500)

	// A missing source directory: 1, and the state directory is not even created.
	stateDir := filepath.Join(t.TempDir(), "state")
	cfg := writeImportConfig(t, stateDir, 500)
	if code, _, errOut := runCLI(t, "import-history", "--from", "/nonexistent", "--config", cfg); code != 1 || !strings.Contains(errOut, "/nonexistent") {
		t.Errorf("missing --from: exit = %d, stderr %q, want 1 naming the path", code, errOut)
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Errorf("the state directory was created before --from was checked: %v", err)
	}
	// A file is not a directory.
	file := filepath.Join(t.TempDir(), "afile")
	os.WriteFile(file, []byte("x"), 0o600)
	if code, _, _ := runCLI(t, "import-history", "--from", file, "--config", good); code != 1 {
		t.Errorf("--from a file: exit = %d, want 1", code)
	}
	// A config ERROR: 78, whatever --from is.
	if code, _, errOut := runCLI(t, "import-history", "--from", t.TempDir(), "--config", testdata+"timeout_number.yml"); code != 78 || !strings.Contains(errOut, "deploys.timeout") {
		t.Errorf("config error: exit = %d, stderr %q, want 78 naming the key", code, errOut)
	}
	if code, _, _ := runCLI(t, "import-history", "--from", t.TempDir(), "--config", filepath.Join(t.TempDir(), "absent.yml")); code != 78 {
		t.Errorf("no config: exit = %d, want 78", code)
	}
	// The packaged example config, a missing source: 1 (the plan's own check).
	if code, _, _ := runCLI(t, "import-history", "--from", "/nonexistent", "--config", "../../packaging/config.example.yml"); code != 1 {
		t.Errorf("example config: exit = %d, want 1", code)
	}
	// Usage.
	for _, args := range [][]string{
		{"import-history"},
		{"import-history", "--config", good},
		{"import-history", "--from"},
		{"import-history", "--from", t.TempDir(), "extra"},
	} {
		if code, _, _ := runCLI(t, args...); code != 2 {
			t.Errorf("run(%q) = %d, want 2", args, code)
		}
	}
}

// Only serve refuses uid 0 (D-20): the cutover script runs the import as root,
// because the legacy directory is root's.
func TestImportHistoryCLIRunsAsRoot(t *testing.T) {
	old := geteuid
	geteuid = func() int { return 0 }
	t.Cleanup(func() { geteuid = old })
	from := t.TempDir()
	writeLegacyRecords(t, from, 2, 0)
	cfg := writeImportConfig(t, filepath.Join(t.TempDir(), "state"), 500)
	code, out, errOut := runCLI(t, "import-history", "--from", from, "--config", cfg)
	if code != 0 || out != "imported 2; skipped 0 already present, 0 unfinished, 0 unreadable\n" {
		t.Fatalf("as root: exit = %d, stdout %q, stderr %q", code, out, errOut)
	}
}
