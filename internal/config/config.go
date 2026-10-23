// Derived in part from voxpupuli/webhook-go config/config.go (Apache-2.0,
// https://github.com/voxpupuli/webhook-go): the key names, the defaults and the
// r10k flag set. Modified for the Stagehand deploy agent.

// Package config loads the agent's strict YAML configuration (D-20): the file
// is decoded with go.yaml.in/yaml/v3 and KnownFields(true), so an unknown key
// stops the agent at start instead of being ignored. There is no viper.
//
// The key names are final (PD-06). Every mistake in a file is a Finding that
// names the dotted key, so serve and --check-config print the same answer. When
// the agent's own file is absent, /etc/voxpupuli/webhook.yml is read leniently
// and mapped onto this same schema for one release (legacy.go, D-15).
//
// Key names, defaults and the r10k/g10k flag set follow voxpupuli/webhook-go
// (Apache-2.0, https://github.com/voxpupuli/webhook-go) config/config.go; the
// strict loader, the schema layout and the Duration type are Stagehand's.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// DefaultPath is where the agent looks for its configuration.
const DefaultPath = "/etc/stagehand-deploy-agent/config.yml"

// DefaultPort is the mTLS port. It is the Code Manager pack's
// DefaultWebhookPort, so no estate setting changes (PD-03).
const DefaultPort = 8088

// DefaultR10kConfig is the tool's own configuration file when none is set.
const DefaultR10kConfig = "/etc/puppetlabs/r10k/r10k.yaml"

// DefaultOpsListen is where the plain-HTTP ops listener binds (D-05).
const DefaultOpsListen = "127.0.0.1:8089"

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
	Address string `yaml:"address,omitempty"`
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
	CRL string `yaml:"crl,omitempty"`
	// AllowedCertnames lists the Subject CNs (Puppet certnames) let in.
	AllowedCertnames []string `yaml:"allowed_certnames"`
	// AllowedFingerprints, when set, also pins the leaf's SHA-256
	// fingerprint (colon hex, as Puppet prints it).
	AllowedFingerprints []string `yaml:"allowed_fingerprints,omitempty"`
}

// Ops is the plain-HTTP ops listener (plan 08).
type Ops struct {
	// Listen is host:port. A non-loopback host is a WARNING: the listener
	// has no authentication (D-05).
	Listen string `yaml:"listen"`
}

// Engine is the deploy tool.
type Engine struct {
	// Name is "r10k" or "g10k".
	Name        string `yaml:"name"`
	CommandPath string `yaml:"command_path"`
	ConfigPath  string `yaml:"config_path,omitempty"`
	Verbose     *bool  `yaml:"verbose"`
	// DeployModules, GenerateTypes, EnvIncremental and UseLegacyPuppetfileFlag
	// are r10k's flags, named as upstream names them.
	DeployModules           *bool `yaml:"deploy_modules"`
	GenerateTypes           *bool `yaml:"generate_types"`
	EnvIncremental          bool  `yaml:"env_incremental"`
	UseLegacyPuppetfileFlag bool  `yaml:"use_legacy_puppetfile_flag"`
	// SSHConfig is the ssh_config passed to git with -F through
	// GIT_SSH_COMMAND (D-03). Optional.
	SSHConfig string `yaml:"ssh_config,omitempty"`
}

// Deploys is the job runner and its records.
type Deploys struct {
	// EnvironmentsDir is where environments, and so their deploy markers,
	// live. It matches the environments_dir field of GET /api/v1/info.
	EnvironmentsDir string   `yaml:"environments_dir"`
	StateDir        string   `yaml:"state_dir"`
	Timeout         Duration `yaml:"timeout"`
	OutputLimit     int      `yaml:"output_limit"`
	MaxJobs         int      `yaml:"max_jobs"`
	// ProtectedEnvironments are refused by DELETE (plan 05, D-10).
	ProtectedEnvironments []string `yaml:"protected_environments"`
}

// Log is the logger.
type Log struct {
	Level string `yaml:"level"`
}

var (
	certnameRE    = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	environmentRE = regexp.MustCompile(`^[a-z0-9_]+$`)
	sshConfigRE   = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)
)

// ValidSSHConfigPath reports whether p is safe to put in GIT_SSH_COMMAND, which
// git passes through a shell: an absolute path of letters, digits and . _ / -
// only (T-57.6.2-10).
func ValidSSHConfigPath(p string) bool { return sshConfigRE.MatchString(p) }

// Load reads the file at path and returns the configuration and every finding.
// legacy selects the /etc/voxpupuli/webhook.yml reader (legacy.go). The
// returned *Config is nil when any finding is an ERROR.
func Load(path string, legacy bool) (*Config, []Finding) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, []Finding{errorf("config", "%v", err)}
	}
	return Parse(b, path, legacy)
}

// Locate chooses the file to load (PD-11). An explicit path must exist and is
// never replaced by the legacy file. Otherwise the default path is used when
// present, then legacyPath (legacy is then true), and otherwise it is an error.
func Locate(explicit, defaultPath, legacyPath string) (path string, legacy bool, err error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", false, fmt.Errorf("config file not found: %w", err)
		}
		return explicit, false, nil
	}
	for i, p := range []string{defaultPath, legacyPath} {
		_, err := os.Stat(p)
		if err == nil {
			return p, i == 1, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", false, fmt.Errorf("config file %s: %w", p, err)
		}
	}
	return "", false, fmt.Errorf("no config: write %s", defaultPath)
}

// renamedKeys are keys the native schema dropped before 1.0.0 was frozen. They
// are refused like any unknown key (the agent was never released with them, so
// there is no alias), but the finding says what replaced them.
var renamedKeys = map[string]string{
	"deploys.basedir": "deploys.environments_dir",
}

// Parse is Load for bytes; name is used in messages.
func Parse(b []byte, name string, legacy bool) (*Config, []Finding) {
	if legacy {
		return parseLegacy(b, name)
	}
	return parseNative(b, name)
}

func parseNative(b []byte, name string) (*Config, []Finding) {
	var fs []Finding
	sc, syntaxErr := scanDocument(b)
	if syntaxErr != nil {
		return nil, []Finding{errorf("config", "%s: %v", name, syntaxErr)}
	}
	if sc.extraDocument {
		fs = append(fs, errorf("document", "more than one YAML document; keep exactly one"))
	}
	for _, u := range sc.unknown {
		if now, renamed := renamedKeys[u.key]; renamed {
			fs = append(fs, errorf(u.key, "unknown key (line %d); it is now called %s", u.line, now))
			continue
		}
		fs = append(fs, errorf(u.key, "unknown key (line %d); check the spelling against packaging/config.example.yml", u.line))
	}

	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		fs = append(fs, decodeFindings(err, sc, len(sc.unknown) > 0)...)
	}

	c.applyDefaults(sc.has)
	c.normalize()
	fs = append(fs, c.Validate()...)
	if sc.has("deploys.protected_environments") && len(c.Deploys.ProtectedEnvironments) == 0 {
		// An explicit empty list unprotects production; an absent one does not.
		c.Deploys.ProtectedEnvironments = nil
		fs = append(fs, warnf("deploys.protected_environments", "the list is empty, so no environment is protected from removal; the default is [production]"))
	}
	fs = dedupe(fs)
	if HasErrors(fs) {
		return nil, fs
	}
	return &c, fs
}

// applyDefaults fills what the file left out. has reports whether the file
// names a key, so an explicit port: 0 stays 0 and is then refused by Validate
// instead of silently becoming 8088.
func (c *Config) applyDefaults(has func(string) bool) {
	if !has("server.port") {
		c.Server.Port = DefaultPort
	}
	if c.Ops.Listen == "" {
		c.Ops.Listen = DefaultOpsListen
	}
	if c.Engine.Name == "" {
		c.Engine.Name = "r10k"
	}
	if c.Engine.CommandPath == "" {
		c.Engine.CommandPath = "/opt/puppetlabs/puppet/bin/r10k"
	}
	if c.Engine.ConfigPath == "" {
		c.Engine.ConfigPath = DefaultR10kConfig
	}
	// Upstream's defaults for these three are true.
	if c.Engine.Verbose == nil {
		c.Engine.Verbose = boolPtr(true)
	}
	if c.Engine.DeployModules == nil {
		c.Engine.DeployModules = boolPtr(true)
	}
	if c.Engine.GenerateTypes == nil {
		c.Engine.GenerateTypes = boolPtr(true)
	}
	if c.Deploys.EnvironmentsDir == "" {
		c.Deploys.EnvironmentsDir = "/etc/puppetlabs/code/environments"
	}
	if c.Deploys.StateDir == "" {
		c.Deploys.StateDir = "/var/lib/stagehand-deploy-agent"
	}
	if !has("deploys.timeout") {
		c.Deploys.Timeout = Duration(15 * time.Minute)
	}
	if !has("deploys.output_limit") {
		c.Deploys.OutputLimit = 262144
	}
	if !has("deploys.max_jobs") {
		c.Deploys.MaxJobs = 500
	}
	if !has("deploys.protected_environments") {
		c.Deploys.ProtectedEnvironments = []string{"production"}
	}
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
}

// normalize makes an empty list nil, so a Config compares equal however it was
// written.
func (c *Config) normalize() {
	if len(c.Server.TLS.AllowedCertnames) == 0 {
		c.Server.TLS.AllowedCertnames = nil
	}
	if len(c.Server.TLS.AllowedFingerprints) == 0 {
		c.Server.TLS.AllowedFingerprints = nil
	}
	if len(c.Deploys.ProtectedEnvironments) == 0 {
		c.Deploys.ProtectedEnvironments = nil
	}
}

func boolPtr(b bool) *bool { return &b }

func isAbs(p string) bool { return filepath.IsAbs(p) }

// Validate checks every rule in the schema and returns one Finding per
// mistake. A Config that returns no ERROR is safe to serve.
func (c *Config) Validate() []Finding {
	var fs []Finding
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		fs = append(fs, errorf("server.port", "must be 1 to 65535, got %d", c.Server.Port))
	}
	t := c.Server.TLS
	for _, f := range []struct{ key, v string }{
		{"server.tls.certificate", t.Certificate},
		{"server.tls.key", t.Key},
		{"server.tls.client_ca", t.ClientCA},
	} {
		switch {
		case f.v == "":
			msg := "is required"
			if f.key == "server.tls.client_ca" {
				msg = "is required: the agent serves mutual TLS only"
			}
			fs = append(fs, errorf(f.key, "%s", msg))
		case !isAbs(f.v):
			fs = append(fs, errorf(f.key, "must be an absolute path, got %q", f.v))
		}
	}
	if t.CRL != "" && !isAbs(t.CRL) {
		fs = append(fs, errorf("server.tls.crl", "must be an absolute path, got %q", t.CRL))
	}
	if len(t.AllowedCertnames) == 0 {
		fs = append(fs, errorf("server.tls.allowed_certnames", "must list at least one certname; nobody could call the API otherwise"))
	}
	for i, name := range t.AllowedCertnames {
		if !certnameRE.MatchString(name) {
			fs = append(fs, errorf(fmt.Sprintf("server.tls.allowed_certnames[%d]", i), "%q is not a certname; use letters, digits, '.', '_' and '-' only", name))
		}
	}
	for i, fp := range t.AllowedFingerprints {
		if _, ok := NormalizeFingerprint(fp); !ok {
			fs = append(fs, errorf(fmt.Sprintf("server.tls.allowed_fingerprints[%d]", i), "must be a SHA-256 fingerprint as 32 colon-separated hex pairs (AB:CD:...), got %q", fp))
		}
	}

	fs = append(fs, c.validateOps()...)

	e := c.Engine
	if e.Name != "r10k" && e.Name != "g10k" {
		fs = append(fs, errorf("engine.name", "must be r10k or g10k, got %q", e.Name))
	}
	if e.CommandPath != "" && !isAbs(e.CommandPath) {
		fs = append(fs, errorf("engine.command_path", "must be an absolute path, got %q", e.CommandPath))
	}
	if e.ConfigPath != "" && !isAbs(e.ConfigPath) {
		fs = append(fs, errorf("engine.config_path", "must be an absolute path, got %q", e.ConfigPath))
	}
	if e.SSHConfig != "" && !ValidSSHConfigPath(e.SSHConfig) {
		fs = append(fs, errorf("engine.ssh_config", "must be an absolute path of letters, digits, '.', '_', '/' and '-' only, got %q (git passes it through a shell)", e.SSHConfig))
	}

	d := c.Deploys
	if !isAbs(d.EnvironmentsDir) {
		fs = append(fs, errorf("deploys.environments_dir", "must be an absolute path, got %q", d.EnvironmentsDir))
	}
	if !isAbs(d.StateDir) {
		fs = append(fs, errorf("deploys.state_dir", "must be an absolute path, got %q", d.StateDir))
	}
	if d.Timeout.Std() < MinDuration {
		fs = append(fs, errorf("deploys.timeout", "must be at least 1s, got %s", d.Timeout.Std()))
	}
	if d.OutputLimit < 1024 {
		fs = append(fs, errorf("deploys.output_limit", "must be at least 1024 bytes, got %d", d.OutputLimit))
	}
	if d.MaxJobs < 1 {
		fs = append(fs, errorf("deploys.max_jobs", "must be at least 1, got %d", d.MaxJobs))
	}
	for i, env := range d.ProtectedEnvironments {
		if !environmentRE.MatchString(env) {
			fs = append(fs, errorf(fmt.Sprintf("deploys.protected_environments[%d]", i), "%q is not an environment name; use lower-case letters, digits and '_' only", env))
		}
	}

	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		fs = append(fs, errorf("log.level", "must be debug, info, warn or error, got %q", c.Log.Level))
	}
	return fs
}

func (c *Config) validateOps() []Finding {
	host, port, err := net.SplitHostPort(c.Ops.Listen)
	if err != nil {
		return []Finding{errorf("ops.listen", "must be host:port, for example %s, got %q", DefaultOpsListen, c.Ops.Listen)}
	}
	if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		return []Finding{errorf("ops.listen", "port must be 1 to 65535, got %q", port)}
	}
	if !isLoopback(host) {
		return []Finding{warnf("ops.listen", "%q is not a loopback address: /healthz, /readyz and /metrics are unauthenticated and will be reachable from that network; keep %s unless a scraper needs more", c.Ops.Listen, DefaultOpsListen)}
	}
	return nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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

// ---- schema walking --------------------------------------------------------

var nodeType = reflect.TypeOf(yaml.Node{})

type unknownKey struct {
	key  string
	line int
}

// scan is what a walk over a YAML document learns about it: which dotted keys
// it names, on which lines, and which of them the schema does not know. The
// strict decoder reports problems by line number only; scan turns a line into
// the key an operator has to fix.
type scan struct {
	present       map[string]int
	lineKey       map[int]string
	unknown       []unknownKey
	extraDocument bool
}

func (s *scan) has(key string) bool { _, ok := s.present[key]; return ok }

// scanDocument parses b and walks its first document against Config. A second
// document is noted, never walked. A syntax error is returned.
func scanDocument(b []byte) (*scan, error) { return scanAgainst(b, reflect.TypeOf(Config{})) }

func scanAgainst(b []byte, t reflect.Type) (*scan, error) {
	s := &scan{present: map[string]int{}, lineKey: map[int]string{}}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	var root yaml.Node
	if err := dec.Decode(&root); err != nil {
		if errors.Is(err, io.EOF) {
			return s, nil
		}
		return nil, err
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		s.extraDocument = true
	} else if !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(root.Content) == 1 {
		s.walk(root.Content[0], t, "")
	}
	return s, nil
}

func (s *scan) walk(n *yaml.Node, t reflect.Type, prefix string) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if n.Kind != yaml.MappingNode || t.Kind() != reflect.Struct || t == nodeType {
		return
	}
	fields := yamlFields(t)
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		key := prefix + k.Value
		f, ok := fields[k.Value]
		if !ok {
			s.unknown = append(s.unknown, unknownKey{key, k.Line})
			continue
		}
		s.present[key] = k.Line
		s.lineKey[k.Line] = key
		if _, taken := s.lineKey[v.Line]; !taken {
			s.lineKey[v.Line] = key
		}
		s.walk(v, f.Type, key+".")
	}
}

// yamlFields maps each YAML key of struct type t to its field.
func yamlFields(t reflect.Type) map[string]reflect.StructField {
	out := map[string]reflect.StructField{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = strings.ToLower(f.Name)
		}
		out[name] = f
	}
	return out
}

// SchemaKeys lists every dotted leaf key of the native schema, in file order.
func SchemaKeys() []string { return schemaKeys(reflect.TypeOf(Config{}), "") }

func schemaKeys(t reflect.Type, prefix string) []string {
	var out []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct && ft != nodeType {
			out = append(out, schemaKeys(ft, prefix+name+".")...)
			continue
		}
		out = append(out, prefix+name)
	}
	return out
}

var typeErrLine = regexp.MustCompile(`^line (\d+): (.*)$`)

// decodeFindings turns a yaml.TypeError into one Finding per problem, keyed by
// the dotted key found on the line it names. skipUnknown drops the strict
// decoder's "field x not found" lines when the scan has already reported them
// with their full dotted keys.
func decodeFindings(err error, sc *scan, skipUnknown bool) []Finding {
	var te *yaml.TypeError
	if !errors.As(err, &te) {
		return []Finding{errorf("config", "%v", err)}
	}
	var out []Finding
	for _, msg := range te.Errors {
		line, text := 0, msg
		if m := typeErrLine.FindStringSubmatch(msg); m != nil {
			line, _ = strconv.Atoi(m[1])
			text = m[2]
		}
		if skipUnknown && strings.Contains(text, "not found in type") {
			continue
		}
		key := sc.lineKey[line]
		if key == "" {
			key = "config"
		}
		out = append(out, errorf(key, "%s (line %d)", text, line))
	}
	return out
}
