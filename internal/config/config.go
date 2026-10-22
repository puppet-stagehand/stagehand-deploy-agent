// Package config loads the agent's strict YAML configuration (D-20): the file
// is decoded with go.yaml.in/yaml/v3 and KnownFields(true), so an unknown key
// stops the agent at start instead of being ignored. There is no viper.
//
// The key names are final (PD-06). This tracer slice loads and validates the
// subset a deploy needs; later plans add the rest of the validation, the
// legacy /etc/voxpupuli/webhook.yml fallback and --check-config.
//
// Key names, defaults and the r10k/g10k flag set follow voxpupuli/webhook-go
// (Apache-2.0, https://github.com/voxpupuli/webhook-go) config/config.go; the
// strict loader, the schema layout and the Duration type are Stagehand's.
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// DefaultPath is where the agent looks for its configuration.
const DefaultPath = "/etc/stagehand-deploy-agent/config.yml"

// DefaultPort is the mTLS port. It is the Code Manager pack's
// DefaultWebhookPort, so no estate setting changes (PD-03).
const DefaultPort = 8088

// Config is the whole configuration file.
type Config struct {
	Server  Server  `yaml:"server"`
	Ops     Ops     `yaml:"ops"`
	Engine  Engine  `yaml:"engine"`
	Deploys Deploys `yaml:"deploys"`
	Log     Log     `yaml:"log"`
}

// Server is the mTLS listener.
type Server struct {
	// Address is the interface to bind; empty means all interfaces.
	Address string `yaml:"address"`
	Port    int    `yaml:"port"`
	TLS     TLS    `yaml:"tls"`
}

// TLS is the mTLS material and the caller allow-list.
type TLS struct {
	Certificate string `yaml:"certificate"`
	Key         string `yaml:"key"`
	// ClientCA is the estate CA (PEM) client certificates must chain to.
	ClientCA string `yaml:"client_ca"`
	// CRL is the CA's revocation list (PEM); a listed certificate fails the
	// handshake. Reloaded when the file changes, at most once a minute.
	CRL string `yaml:"crl"`
	// AllowedCertnames lists the Subject CNs (Puppet certnames) let in.
	AllowedCertnames []string `yaml:"allowed_certnames"`
	// AllowedFingerprints, when set, also pins the leaf's SHA-256
	// fingerprint (colon hex, as Puppet prints it).
	AllowedFingerprints []string `yaml:"allowed_fingerprints"`
}

// Ops is the plain-HTTP ops listener (plan 03).
type Ops struct {
	Listen string `yaml:"listen"`
}

// Engine is the deploy tool.
type Engine struct {
	// Name is "r10k" or "g10k".
	Name        string `yaml:"name"`
	CommandPath string `yaml:"command_path"`
	ConfigPath  string `yaml:"config_path"`
	Verbose     *bool  `yaml:"verbose"`
	// DeployModules, GenerateTypes, EnvIncremental and UseLegacyPuppetfileFlag
	// are r10k's flags, named as upstream names them.
	DeployModules           *bool `yaml:"deploy_modules"`
	GenerateTypes           *bool `yaml:"generate_types"`
	EnvIncremental          bool  `yaml:"env_incremental"`
	UseLegacyPuppetfileFlag bool  `yaml:"use_legacy_puppetfile_flag"`
	// SSHConfig is the ssh_config passed to git with -F (plan 03, D-03).
	SSHConfig string `yaml:"ssh_config"`
}

// Deploys is the job runner and its records.
type Deploys struct {
	Basedir     string   `yaml:"basedir"`
	StateDir    string   `yaml:"state_dir"`
	Timeout     Duration `yaml:"timeout"`
	OutputLimit int      `yaml:"output_limit"`
	MaxJobs     int      `yaml:"max_jobs"`
	// ProtectedEnvironments are refused by DELETE (plan 05, D-10).
	ProtectedEnvironments []string `yaml:"protected_environments"`
}

// Log is the logger (plan 03).
type Log struct {
	Level string `yaml:"level"`
}

// Duration is a time.Duration written as a string with a unit ("15m"). A bare
// number is refused: upstream's viper read 900 as 900 nanoseconds (FORK-IN-01).
type Duration time.Duration

// UnmarshalYAML parses a quoted or plain scalar such as 15m or 90s.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: a duration must be a string with a unit, such as 15m", n.Line)
	}
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %q is not a duration; write a number with a unit, such as 15m or 90s", n.Line, n.Value)
	}
	*d = Duration(v)
	return nil
}

// Std returns the value as a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// Load reads, strictly decodes, defaults and validates the file at path.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	defer f.Close()
	return Decode(f, path)
}

// Decode is Load for a reader; name is used in messages.
func Decode(r io.Reader, name string) (*Config, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	var c Config
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("config: %s: %w", name, err)
	}
	// One document only: a second one would be silently ignored otherwise.
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return nil, fmt.Errorf("config: %s: more than one YAML document", name)
	} else if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("config: %s: %w", name, err)
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("config: %s: %w", name, err)
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Server.Port == 0 {
		c.Server.Port = DefaultPort
	}
	if c.Engine.Name == "" {
		c.Engine.Name = "r10k"
	}
	if c.Engine.CommandPath == "" {
		c.Engine.CommandPath = "/opt/puppetlabs/puppet/bin/r10k"
	}
	if c.Engine.ConfigPath == "" {
		c.Engine.ConfigPath = "/etc/puppetlabs/r10k/r10k.yaml"
	}
	// Upstream's defaults for these three are true.
	t := true
	if c.Engine.Verbose == nil {
		c.Engine.Verbose = &t
	}
	if c.Engine.DeployModules == nil {
		c.Engine.DeployModules = &t
	}
	if c.Engine.GenerateTypes == nil {
		c.Engine.GenerateTypes = &t
	}
	if c.Deploys.Basedir == "" {
		c.Deploys.Basedir = "/etc/puppetlabs/code/environments"
	}
	if c.Deploys.StateDir == "" {
		c.Deploys.StateDir = "/var/lib/stagehand-deploy-agent"
	}
	if c.Deploys.Timeout == 0 {
		c.Deploys.Timeout = Duration(15 * time.Minute)
	}
	if c.Deploys.OutputLimit == 0 {
		c.Deploys.OutputLimit = 262144
	}
	if c.Deploys.MaxJobs == 0 {
		c.Deploys.MaxJobs = 500
	}
}

// Validate refuses a configuration that would run the deploy API without its
// protections. It checks the tracer subset; plan 03 completes it.
func (c *Config) Validate() error {
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("server.port must be 1 to 65535, got %d", c.Server.Port)
	}
	t := c.Server.TLS
	if t.ClientCA == "" {
		return errors.New("server.tls.client_ca is required")
	}
	if t.Certificate == "" || t.Key == "" {
		return errors.New("server.tls.certificate and server.tls.key are required")
	}
	if len(t.AllowedCertnames) == 0 {
		return errors.New("server.tls.allowed_certnames must list at least one certname")
	}
	for i, name := range t.AllowedCertnames {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("server.tls.allowed_certnames[%d] is empty", i)
		}
	}
	for i, fp := range t.AllowedFingerprints {
		if _, ok := NormalizeFingerprint(fp); !ok {
			return fmt.Errorf("server.tls.allowed_fingerprints[%d] must be a SHA-256 fingerprint as 32 colon-separated hex pairs (AB:CD:...), got %q", i, fp)
		}
	}
	if c.Engine.Name != "r10k" && c.Engine.Name != "g10k" {
		return fmt.Errorf("engine.name must be r10k or g10k, got %q", c.Engine.Name)
	}
	if !filepath.IsAbs(c.Deploys.Basedir) {
		return fmt.Errorf("deploys.basedir must be an absolute path, got %q", c.Deploys.Basedir)
	}
	if !filepath.IsAbs(c.Deploys.StateDir) {
		return fmt.Errorf("deploys.state_dir must be an absolute path, got %q", c.Deploys.StateDir)
	}
	if c.Deploys.Timeout.Std() <= 0 {
		return errors.New("deploys.timeout must be greater than zero")
	}
	if c.Deploys.OutputLimit <= 0 {
		return errors.New("deploys.output_limit must be greater than zero")
	}
	if c.Deploys.MaxJobs < 1 {
		return errors.New("deploys.max_jobs must be at least 1")
	}
	return nil
}

var fingerprintRE = regexp.MustCompile(`^([0-9A-F]{2}:){31}[0-9A-F]{2}$`)

// NormalizeFingerprint returns a SHA-256 certificate fingerprint in the form
// Puppet prints it (upper-case hex pairs joined by colons) and whether s is
// one. Lower-case input is accepted.
//
// It lives here, and server.NormalizeFingerprint wraps it, so configuration
// validation and the TLS server share one definition without config importing
// server.
func NormalizeFingerprint(s string) (string, bool) {
	up := strings.ToUpper(strings.TrimSpace(s))
	return up, fingerprintRE.MatchString(up)
}
