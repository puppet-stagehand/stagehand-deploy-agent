package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
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
