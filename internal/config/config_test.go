package config

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// load loads a testdata fixture through the native (strict) loader.
func load(t *testing.T, name string) (*Config, []Finding) {
	t.Helper()
	return Load(filepath.Join("testdata", name), false)
}

func errorsOf(fs []Finding) []Finding {
	var out []Finding
	for _, f := range fs {
		if f.Level == LevelError {
			out = append(out, f)
		}
	}
	return out
}

func warningsOf(fs []Finding) []Finding {
	var out []Finding
	for _, f := range fs {
		if f.Level == LevelWarning {
			out = append(out, f)
		}
	}
	return out
}

func TestLoadCompleteFileHasNoFindings(t *testing.T) {
	c, fs := load(t, "valid.yml")
	if len(fs) != 0 || c == nil {
		t.Fatalf("valid.yml: cfg=%v findings=%+v", c, fs)
	}
	if c.Server.Port != 8088 || c.Ops.Listen != "127.0.0.1:8089" || c.Engine.SSHConfig != "/etc/stagehand-deploy-agent/ssh_config" ||
		len(c.Server.TLS.AllowedCertnames) != 2 || len(c.Server.TLS.AllowedFingerprints) != 1 ||
		!reflect.DeepEqual(c.Deploys.ProtectedEnvironments, []string{"production"}) || c.Log.Level != "info" {
		t.Fatalf("valid.yml loaded as %+v", c)
	}
}

func TestLoadMinimalFileTakesTheDefaults(t *testing.T) {
	c, fs := load(t, "minimal.yml")
	if len(fs) != 0 || c == nil {
		t.Fatalf("minimal.yml: findings=%+v", fs)
	}
	if c.Server.Port != 8088 || c.Server.Address != "" {
		t.Fatalf("server = %+v, want port 8088 (the pack's DefaultWebhookPort) on all interfaces", c.Server)
	}
	if c.Ops.Listen != "127.0.0.1:8089" {
		t.Fatalf("ops.listen = %q, want 127.0.0.1:8089 (D-05)", c.Ops.Listen)
	}
	if c.Engine.Name != "r10k" || c.Engine.CommandPath != "/opt/puppetlabs/puppet/bin/r10k" || c.Engine.SSHConfig != "" ||
		!*c.Engine.Verbose || !*c.Engine.DeployModules || !*c.Engine.GenerateTypes ||
		c.Engine.EnvIncremental || c.Engine.UseLegacyPuppetfileFlag {
		t.Fatalf("engine = %+v", c.Engine)
	}
	d := c.Deploys
	if d.Basedir != "/etc/puppetlabs/code/environments" || d.StateDir != "/var/lib/stagehand-deploy-agent" ||
		d.Timeout.Std() != 15*time.Minute || d.OutputLimit != 262144 || d.MaxJobs != 500 {
		t.Fatalf("deploys = %+v", d)
	}
	if !reflect.DeepEqual(d.ProtectedEnvironments, []string{"production"}) {
		t.Fatalf("protected_environments = %q, want [production] (D-10)", d.ProtectedEnvironments)
	}
	if c.Log.Level != "info" {
		t.Fatalf("log.level = %q", c.Log.Level)
	}
}

// Each mistake is one ERROR that names the key, so an operator reading
// --check-config output knows which line to fix (FORK-IN-01).
func TestLoadRefusesEachMistakeNamingItsKey(t *testing.T) {
	cases := []struct{ file, key string }{
		{"unknown_key_top.yml", "surprise"},
		{"unknown_key_nested.yml", "server.tls.client_cert_authority"},
		{"second_document.yml", "document"},
		{"timeout_number.yml", "deploys.timeout"},
		{"timeout_subsecond.yml", "deploys.timeout"},
		{"port_zero.yml", "server.port"},
		{"port_too_big.yml", "server.port"},
		{"empty_certnames.yml", "server.tls.allowed_certnames"},
		{"bad_certname.yml", "server.tls.allowed_certnames[1]"},
		{"relative_environments_dir.yml", "deploys.environments_dir"},
		{"relative_certificate.yml", "server.tls.certificate"},
		{"bad_protected_env.yml", "deploys.protected_environments[0]"},
		{"bad_ssh_config.yml", "engine.ssh_config"},
		{"bad_log_level.yml", "log.level"},
		{"bad_listen.yml", "ops.listen"},
		{"bad_engine.yml", "engine.name"},
		{"bad_fingerprint.yml", "server.tls.allowed_fingerprints[0]"},
		{"output_limit_small.yml", "deploys.output_limit"},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			c, fs := load(t, tc.file)
			if c != nil {
				t.Fatalf("a mistaken file produced a Config: %+v", c)
			}
			errs := errorsOf(fs)
			if len(errs) != 1 || errs[0].Key != tc.key || errs[0].Message == "" {
				t.Fatalf("findings = %+v, want exactly one ERROR for key %q", fs, tc.key)
			}
		})
	}
}

// deploys.environments_dir was named deploys.basedir before 1.0.0 was frozen.
// The agent was never released under the old name, so there is no alias: the
// strict loader refuses it like any unknown key, and says what replaced it.
func TestDeploysBasedirIsRefusedAndNamesItsReplacement(t *testing.T) {
	body, err := os.ReadFile("testdata/valid.yml")
	if err != nil {
		t.Fatal(err)
	}
	old := bytes.Replace(body, []byte("  environments_dir:"), []byte("  basedir:"), 1)
	if bytes.Equal(old, body) {
		t.Fatal("valid.yml has no deploys.environments_dir line to rename")
	}
	c, fs := Parse(old, "t", false)
	if c != nil {
		t.Fatalf("a file with deploys.basedir produced a Config: %+v", c)
	}
	errs := errorsOf(fs)
	if len(errs) != 1 || errs[0].Key != "deploys.basedir" ||
		!strings.Contains(errs[0].Message, "unknown key") || !strings.Contains(errs[0].Message, "deploys.environments_dir") {
		t.Fatalf("findings = %+v, want one ERROR for deploys.basedir that names deploys.environments_dir", fs)
	}
}

func TestNumericTimeoutGetsAUnitHint(t *testing.T) {
	_, fs := load(t, "timeout_number.yml")
	if len(fs) != 1 || !strings.Contains(fs[0].Message, "use a unit, for example 15m") {
		t.Fatalf("findings = %+v", fs)
	}
	_, fs = load(t, "timeout_subsecond.yml")
	if len(fs) != 1 || !strings.Contains(fs[0].Message, "at least 1s") {
		t.Fatalf("findings = %+v", fs)
	}
}

func TestPublicOpsListenerIsAWarningNotAnError(t *testing.T) {
	c, fs := load(t, "listen_public.yml")
	if c == nil || len(errorsOf(fs)) != 0 {
		t.Fatalf("cfg=%v findings=%+v, want it to load", c, fs)
	}
	w := warningsOf(fs)
	if len(w) != 1 || w[0].Key != "ops.listen" || !strings.Contains(w[0].Message, "unauthenticated") {
		t.Fatalf("warnings = %+v", w)
	}
}

func TestLoopbackOpsListenersAreQuiet(t *testing.T) {
	for _, l := range []string{"127.0.0.1:9000", "[::1]:9000", "localhost:9000"} {
		body, _ := os.ReadFile("testdata/minimal.yml")
		c, fs := Parse(append(body, []byte("ops:\n  listen: \""+l+"\"\n")...), "t", false)
		if c == nil || len(fs) != 0 {
			t.Errorf("listen %s: cfg=%v findings=%+v", l, c, fs)
		}
	}
}

func TestProtectedEnvironmentsCanBeExtended(t *testing.T) {
	body, _ := os.ReadFile("testdata/minimal.yml")
	c, fs := Parse(append(body, []byte("deploys:\n  protected_environments: [production, staging]\n")...), "t", false)
	if c == nil || len(fs) != 0 || !reflect.DeepEqual(c.Deploys.ProtectedEnvironments, []string{"production", "staging"}) {
		t.Fatalf("cfg=%+v findings=%+v", c, fs)
	}
}

func TestLoadMissingFileIsAnErrorNamingThePath(t *testing.T) {
	c, fs := Load("testdata/nope.yml", false)
	if c != nil || len(errorsOf(fs)) != 1 || !strings.Contains(fs[0].Message, "nope.yml") {
		t.Fatalf("cfg=%v findings=%+v", c, fs)
	}
}

func TestEmptyFileNamesTheMissingKeys(t *testing.T) {
	c, fs := Parse(nil, "t", false)
	if c != nil || len(errorsOf(fs)) == 0 {
		t.Fatalf("an empty file loaded: cfg=%v findings=%+v", c, fs)
	}
	keys := map[string]bool{}
	for _, f := range fs {
		keys[f.Key] = true
	}
	for _, k := range []string{"server.tls.certificate", "server.tls.key", "server.tls.client_ca", "server.tls.allowed_certnames"} {
		if !keys[k] {
			t.Errorf("no finding for %s in %+v", k, fs)
		}
	}
}

// An unreadable credential file is only a WARNING under --check-config: the
// file may exist only inside the service (PD-13).
func TestCheckFilesWarnsInsteadOfFailing(t *testing.T) {
	c, _ := load(t, "valid.yml")
	// Paths that cannot exist on any host running the tests.
	gone := filepath.Join(t.TempDir(), "absent")
	c.Server.TLS.Certificate, c.Server.TLS.ClientCA, c.Server.TLS.CRL, c.Engine.SSHConfig = gone+"1", gone+"2", gone+"3", gone+"4"
	fs := CheckFiles(c)
	if len(errorsOf(fs)) != 0 {
		t.Fatalf("CheckFiles produced errors: %+v", fs)
	}
	var key *Finding
	for i := range fs {
		if fs[i].Key == "server.tls.key" {
			key = &fs[i]
		}
	}
	if key == nil || key.Level != LevelWarning || !strings.Contains(key.Message, "/run/credentials/stagehand-deploy-agent.service/server.key") {
		t.Fatalf("findings = %+v", fs)
	}
	// every checked key is named
	got := map[string]bool{}
	for _, f := range fs {
		got[f.Key] = true
	}
	for _, k := range []string{"server.tls.certificate", "server.tls.key", "server.tls.client_ca", "server.tls.crl", "engine.ssh_config"} {
		if !got[k] {
			t.Errorf("no warning for unreadable %s", k)
		}
	}
}

func TestCheckFilesIsQuietWhenFilesAreReadable(t *testing.T) {
	dir := t.TempDir()
	p := func(n string) string {
		f := filepath.Join(dir, n)
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		return f
	}
	c, _ := load(t, "valid.yml")
	c.Server.TLS.Certificate, c.Server.TLS.Key, c.Server.TLS.ClientCA, c.Server.TLS.CRL, c.Engine.SSHConfig = p("c"), p("k"), p("ca"), p("crl"), p("ssh")
	if fs := CheckFiles(c); len(fs) != 0 {
		t.Fatalf("findings = %+v", fs)
	}
}

func TestReportFormat(t *testing.T) {
	var b bytes.Buffer
	Report(&b, "/x/c.yml", nil)
	if b.String() != "config /x/c.yml: OK\n" {
		t.Fatalf("clean report = %q", b.String())
	}
	b.Reset()
	Report(&b, "/x/c.yml", []Finding{
		{Level: LevelWarning, Key: "ops.listen", Message: "w"},
		{Level: LevelError, Key: "server.port", Message: "e"},
	})
	want := "WARNING ops.listen: w\nERROR server.port: e\nconfig /x/c.yml: 1 error(s), 1 warning(s)\n"
	if b.String() != want {
		t.Fatalf("report = %q, want %q", b.String(), want)
	}
	if !HasErrors([]Finding{{Level: LevelError}}) || HasErrors([]Finding{{Level: LevelWarning}}) {
		t.Fatal("HasErrors is wrong")
	}
}

// packaging/config.example.yml is what an operator copies. It must load under
// the strict loader (FORK-WR-04: the fork's example could not be enabled as
// written).
func TestExampleConfigLoads(t *testing.T) {
	c, fs := Load("../../packaging/config.example.yml", false)
	if c == nil || len(errorsOf(fs)) != 0 {
		t.Fatalf("example config: cfg=%v findings=%+v", c, fs)
	}
}

// Every key in the schema appears in the example, so none is undocumented.
func TestExampleConfigDocumentsEveryKey(t *testing.T) {
	b, err := os.ReadFile("../../packaging/config.example.yml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for _, key := range SchemaKeys() {
		leaf := key[strings.LastIndex(key, ".")+1:]
		if !strings.Contains(text, leaf+":") {
			t.Errorf("config.example.yml does not mention %s", key)
		}
	}
}

func TestLocate(t *testing.T) {
	dir := t.TempDir()
	touch := func(n string) string {
		p := filepath.Join(dir, n)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	def, leg, missing := touch("default.yml"), touch("legacy.yml"), filepath.Join(dir, "absent.yml")

	t.Run("explicit path wins and is not legacy", func(t *testing.T) {
		p, legacy, err := Locate(def, missing, leg)
		if err != nil || p != def || legacy {
			t.Fatalf("got %q %v %v", p, legacy, err)
		}
	})
	t.Run("a missing explicit path is an error and never falls back", func(t *testing.T) {
		_, _, err := Locate(missing, def, leg)
		if err == nil || !strings.Contains(err.Error(), missing) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("the default path when present", func(t *testing.T) {
		p, legacy, err := Locate("", def, leg)
		if err != nil || p != def || legacy {
			t.Fatalf("got %q %v %v", p, legacy, err)
		}
	})
	t.Run("the legacy file only when the default is absent", func(t *testing.T) {
		p, legacy, err := Locate("", missing, leg)
		if err != nil || p != leg || !legacy {
			t.Fatalf("got %q %v %v", p, legacy, err)
		}
	})
	t.Run("neither file", func(t *testing.T) {
		_, _, err := Locate("", missing, missing)
		if err == nil || !strings.Contains(err.Error(), "no config: write "+missing) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestNormalizeFingerprint(t *testing.T) {
	good := strings.Repeat("ab:", 31) + "ab"
	if got, ok := NormalizeFingerprint(good); !ok || got != strings.ToUpper(good) {
		t.Fatalf("NormalizeFingerprint = %q, %v", got, ok)
	}
	for _, bad := range []string{"", "AB:CD", "SHA256:" + good, strings.ReplaceAll(good, ":", "")} {
		if _, ok := NormalizeFingerprint(bad); ok {
			t.Fatalf("%q accepted", bad)
		}
	}
}
