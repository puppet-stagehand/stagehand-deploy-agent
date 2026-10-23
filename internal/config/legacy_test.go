package config

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

func loadLegacy(t *testing.T, name string) (*Config, []Finding) {
	t.Helper()
	return Load("testdata/"+name, true)
}

// fallbackWarnings returns the one WARNING that names the legacy file.
func fallbackWarnings(fs []Finding) []Finding {
	var out []Finding
	for _, f := range warningsOf(fs) {
		if strings.Contains(f.Message, "/etc/voxpupuli/webhook.yml") {
			out = append(out, f)
		}
	}
	return out
}

func findKey(fs []Finding, level, key string) *Finding {
	for i := range fs {
		if fs[i].Level == level && fs[i].Key == key {
			return &fs[i]
		}
	}
	return nil
}

// D-15: a primary upgraded by hand starts on the file the module wrote for the
// fork, with the same settings and one WARNING naming the file and the release
// that removes the fallback.
func TestLegacyModuleFileMapsSettingForSetting(t *testing.T) {
	c, fs := loadLegacy(t, "legacy_module.yml")
	if c == nil || HasErrors(fs) {
		t.Fatalf("legacy_module.yml: cfg=%v findings=%+v", c, fs)
	}
	if c.Server.Port != 8088 ||
		c.Server.TLS.Certificate != "/etc/puppetlabs/puppet/ssl/certs/primary.example.com.pem" ||
		c.Server.TLS.Key != "/etc/puppetlabs/puppet/ssl/private_keys/primary.example.com.pem" ||
		c.Server.TLS.ClientCA != "/etc/puppetlabs/puppet/ssl/certs/ca.pem" ||
		c.Server.TLS.CRL != "/etc/puppetlabs/puppetserver/ca/ca_crl.pem" ||
		!reflect.DeepEqual(c.Server.TLS.AllowedCertnames, []string{"stagehand-code-manager"}) ||
		len(c.Server.TLS.AllowedFingerprints) != 0 {
		t.Fatalf("server = %+v", c.Server)
	}
	if c.Engine.Name != "r10k" || c.Engine.CommandPath != "/opt/puppetlabs/puppet/bin/r10k" || c.Engine.ConfigPath != "/etc/puppetlabs/r10k/r10k.yaml" {
		t.Fatalf("engine = %+v", c.Engine)
	}
	d := c.Deploys
	if d.EnvironmentsDir != "/etc/puppetlabs/code/environments" || d.StateDir != "/var/lib/webhook-go" ||
		d.Timeout.Std() != 15*time.Minute || d.OutputLimit != 262144 || d.MaxJobs != 500 {
		t.Fatalf("deploys = %+v", d)
	}
	if !reflect.DeepEqual(d.ProtectedEnvironments, []string{"production"}) || c.Ops.Listen != DefaultOpsListen || c.Log.Level != "info" {
		t.Fatalf("new-schema defaults: deploys=%+v ops=%+v log=%+v", d, c.Ops, c.Log)
	}

	fb := fallbackWarnings(fs)
	if len(fb) != 1 || !strings.Contains(fb[0].Message, LegacyFallbackRemovedIn) || !strings.Contains(fb[0].Message, "/etc/stagehand-deploy-agent/config.yml") {
		t.Fatalf("fallback warnings = %+v, want exactly one naming the file, the new path and %s", fb, LegacyFallbackRemovedIn)
	}
	if LegacyFallbackRemovedIn != "1.2.0" {
		t.Fatalf("LegacyFallbackRemovedIn = %q", LegacyFallbackRemovedIn)
	}
}

func TestLegacyG10kFlagBecomesEngineName(t *testing.T) {
	c, fs := loadLegacy(t, "legacy_g10k.yml")
	if c == nil || HasErrors(fs) {
		t.Fatalf("cfg=%v findings=%+v", c, fs)
	}
	if c.Engine.Name != "g10k" || c.Engine.CommandPath != "/usr/local/bin/g10k" {
		t.Fatalf("engine = %+v", c.Engine)
	}
}

// An enabled removed feature, or the plain-webhook mode, cannot be silently
// dropped: the agent refuses and names the key (T-57.6.2-11).
func TestLegacyRefusesWhatIsSwitchedOn(t *testing.T) {
	cases := []struct {
		file string
		keys []string
	}{
		{"legacy_chatops_on.yml", []string{"chatops.enabled"}},
		{"legacy_no_client_ca.yml", []string{"server.tls.client_ca"}},
		{"legacy_shipped.yml", []string{"server.protected", "server.tls.client_ca"}},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			c, fs := loadLegacy(t, tc.file)
			if c != nil {
				t.Fatalf("a refused legacy file produced a Config: %+v", c)
			}
			for _, k := range tc.keys {
				f := findKey(fs, LevelError, k)
				if f == nil || f.Message == "" {
					t.Errorf("no ERROR for %s in %+v", k, fs)
				}
			}
			if len(fallbackWarnings(fs)) != 1 {
				t.Errorf("the fallback warning is missing or repeated: %+v", fs)
			}
		})
	}
}

// The fork's own build/webhook.yml sets server.protected: true with
// puppet:puppet, so it must never start unedited.
func TestLegacyShippedFileNamesBasicAuth(t *testing.T) {
	_, fs := loadLegacy(t, "legacy_shipped.yml")
	f := findKey(fs, LevelError, "server.protected")
	if f == nil || !strings.Contains(f.Message, "client certificate") {
		t.Fatalf("findings = %+v", fs)
	}
}

// Disabled removed features and the legacy-only keys are ignored with one
// aggregated warning, not one per key.
func TestLegacyIgnoredKeysAreOneWarning(t *testing.T) {
	c, fs := loadLegacy(t, "legacy_ignored.yml")
	if c == nil || HasErrors(fs) {
		t.Fatalf("cfg=%v findings=%+v", c, fs)
	}
	var agg []Finding
	for _, f := range warningsOf(fs) {
		if strings.Contains(f.Message, "ignored") && f.Key == "config" && !strings.Contains(f.Message, "/etc/voxpupuli/webhook.yml") {
			agg = append(agg, f)
		}
	}
	if len(agg) != 1 {
		t.Fatalf("aggregated warnings = %+v in %+v", agg, fs)
	}
	for _, k := range []string{"server.queue.enabled", "server.queue.max_concurrent_jobs", "server.protected", "server.user", "server.tls.enabled",
		"r10k.prefix", "r10k.default_branch", "r10k.blocked_branches", "chatops.enabled", "chatops.service", "mappings"} {
		if !strings.Contains(agg[0].Message, k) {
			t.Errorf("the aggregated warning does not list %s: %q", k, agg[0].Message)
		}
	}
	// Total: the fallback line, the aggregated line, one unknown key.
	if w := warningsOf(fs); len(w) != 3 {
		t.Fatalf("warnings = %+v, want 3", w)
	}
}

func TestLegacyUnknownKeyWarnsNamingIt(t *testing.T) {
	_, fs := loadLegacy(t, "legacy_ignored.yml")
	f := findKey(fs, LevelWarning, "whatever")
	if f == nil || !strings.Contains(f.Message, "unknown") {
		t.Fatalf("findings = %+v", fs)
	}
}

// PD-15: a key the legacy file leaves out takes the fork's default, not the
// agent's.
func TestLegacyDefaultsAreTheForksOwn(t *testing.T) {
	c, fs := loadLegacy(t, "legacy_defaults.yml")
	if c == nil || HasErrors(fs) {
		t.Fatalf("cfg=%v findings=%+v", c, fs)
	}
	if c.Server.Port != 4000 {
		t.Fatalf("port = %d, want the fork's 4000", c.Server.Port)
	}
	pw := findKey(fs, LevelWarning, "server.port")
	if pw == nil || !strings.Contains(pw.Message, "legacy default port 4000") || !strings.Contains(pw.Message, "8088") {
		t.Fatalf("no port warning: %+v", fs)
	}
	e, d := c.Engine, c.Deploys
	if e.Name != "r10k" || e.CommandPath != "/opt/puppetlabs/puppetserver/bin/r10k" || e.ConfigPath != "/etc/puppetlabs/r10k/r10k.yaml" ||
		!*e.Verbose || !*e.DeployModules || !*e.GenerateTypes || e.EnvIncremental || e.UseLegacyPuppetfileFlag {
		t.Fatalf("engine = %+v", e)
	}
	if d.EnvironmentsDir != "/etc/puppetlabs/code/environments" || d.StateDir != "/var/lib/webhook-go" ||
		d.Timeout.Std() != 15*time.Minute || d.OutputLimit != 262144 || d.MaxJobs != 500 {
		t.Fatalf("deploys = %+v", d)
	}
}

func TestLegacyNumericTimeoutIsRefusedWithTheUnitHint(t *testing.T) {
	c, fs := loadLegacy(t, "legacy_timeout_number.yml")
	f := findKey(fs, LevelError, "deploys.timeout")
	if c != nil || f == nil || !strings.Contains(f.Message, "use a unit, for example 15m") {
		t.Fatalf("cfg=%v findings=%+v", c, fs)
	}
}

// The mapped Config goes through the same Validate as a native file, so a
// legacy file with a bad path is refused by the same rule.
func TestLegacyMappedConfigIsValidatedLikeANativeOne(t *testing.T) {
	body := "server:\n  tls:\n    certificate: cert.pem\n    key: /k\n    client_ca: /ca\n    allowed_certnames: [a]\n"
	c, fs := Parse([]byte(body), "t", true)
	if c != nil || findKey(fs, LevelError, "server.tls.certificate") == nil {
		t.Fatalf("cfg=%v findings=%+v", c, fs)
	}
}

// Assumption-delta invariant: the legacy file is only an input format. Whatever
// it maps to must be expressible in the primary schema, so writing the mapped
// Config out and loading it with the strict native loader gives the same Config
// with no ERRORs. A mapper that produced a value the primary schema cannot
// hold goes red here.
func TestLegacyMapsIntoPrimarySchema(t *testing.T) {
	for _, file := range []string{"legacy_module.yml", "legacy_g10k.yml", "legacy_ignored.yml", "legacy_defaults.yml"} {
		t.Run(file, func(t *testing.T) {
			c, fs := loadLegacy(t, file)
			if c == nil || HasErrors(fs) {
				t.Fatalf("cfg=%v findings=%+v", c, fs)
			}
			out, err := yaml.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			back, fs2 := Parse(out, "roundtrip", false)
			if back == nil || HasErrors(fs2) {
				t.Fatalf("the mapped Config does not load natively: %+v\n%s", fs2, out)
			}
			if !reflect.DeepEqual(c, back) {
				t.Fatalf("mapped and re-loaded Configs differ:\n mapped %+v\n loaded %+v\n%s", c, back, out)
			}
		})
	}
}
