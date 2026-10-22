package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const minimal = `server:
  tls:
    certificate: /etc/ssl/server.pem
    key: /etc/ssl/server.key
    client_ca: /etc/ssl/ca.pem
    allowed_certnames: [stagehand-code-manager]
`

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadValidFileAppliesDefaults(t *testing.T) {
	c, err := Load(write(t, minimal))
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.Port != 8088 {
		t.Fatalf("default port = %d, want 8088 (the pack's DefaultWebhookPort)", c.Server.Port)
	}
	if c.Engine.Name != "r10k" || c.Engine.CommandPath != "/opt/puppetlabs/puppet/bin/r10k" ||
		c.Deploys.Basedir != "/etc/puppetlabs/code/environments" ||
		c.Deploys.StateDir != "/var/lib/stagehand-deploy-agent" ||
		c.Deploys.Timeout.Std() != 15*time.Minute || c.Deploys.OutputLimit != 262144 || c.Deploys.MaxJobs != 500 {
		t.Fatalf("defaults = %+v", c)
	}
	if !*c.Engine.Verbose || !*c.Engine.DeployModules || !*c.Engine.GenerateTypes {
		t.Fatalf("engine flag defaults = %+v", c.Engine)
	}
}

func TestLoadExplicitValuesWin(t *testing.T) {
	c, err := Load(write(t, minimal+`engine:
  name: g10k
  verbose: false
deploys:
  timeout: 90s
  max_jobs: 7
server:
  port: 9000
`))
	if err == nil {
		t.Fatalf("a repeated top-level key must fail, got %+v", c)
	}
	c, err = Load(write(t, `server:
  port: 9000
  tls:
    certificate: /c
    key: /k
    client_ca: /ca
    allowed_certnames: [a]
engine:
  name: g10k
  verbose: false
deploys:
  timeout: 90s
  max_jobs: 7
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.Port != 9000 || c.Engine.Name != "g10k" || *c.Engine.Verbose || c.Deploys.Timeout.Std() != 90*time.Second || c.Deploys.MaxJobs != 7 {
		t.Fatalf("config = %+v", c)
	}
}

func TestLoadRefusesUnknownKeyNamingIt(t *testing.T) {
	for name, body := range map[string]string{
		"top level": minimal + "surprise: true\n",
		"nested":    strings.Replace(minimal, "client_ca:", "client_cert_authority:", 1),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(write(t, body))
			if err == nil {
				t.Fatal("an unknown key was accepted")
			}
			if !strings.Contains(err.Error(), "surprise") && !strings.Contains(err.Error(), "client_cert_authority") {
				t.Fatalf("error does not name the key: %v", err)
			}
		})
	}
}

func TestLoadRefusesSecondDocument(t *testing.T) {
	if _, err := Load(write(t, minimal+"---\nserver: {}\n")); err == nil || !strings.Contains(err.Error(), "more than one YAML document") {
		t.Fatalf("err = %v", err)
	}
}

func TestDurationNeedsAUnit(t *testing.T) {
	for _, d := range []string{"900", "15", "fast"} {
		body := minimal + "deploys:\n  timeout: " + d + "\n"
		if _, err := Load(write(t, body)); err == nil || !strings.Contains(err.Error(), "duration") {
			t.Fatalf("timeout %q: err = %v, want a duration error", d, err)
		}
	}
}

func TestValidateRefusesUnsafeMTLS(t *testing.T) {
	cases := map[string]string{
		"no client_ca":    strings.Replace(minimal, "    client_ca: /etc/ssl/ca.pem\n", "", 1),
		"no certnames":    strings.Replace(minimal, "[stagehand-code-manager]", "[]", 1),
		"blank certname":  strings.Replace(minimal, "[stagehand-code-manager]", `[" "]`, 1),
		"bad fingerprint": minimal + "    allowed_fingerprints: [AB:CD]\n",
		"no key":          strings.Replace(minimal, "    key: /etc/ssl/server.key\n", "", 1),
		"bad engine":      minimal + "engine:\n  name: puppet\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(write(t, body)); err == nil {
				t.Fatal("an unsafe configuration was accepted")
			}
		})
	}
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
