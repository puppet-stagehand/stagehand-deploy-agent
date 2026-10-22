package config

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// LegacyPath is the fork's configuration file (D-15).
const LegacyPath = "/etc/voxpupuli/webhook.yml"

// LegacyFallbackRemovedIn is the agent release that stops reading LegacyPath.
const LegacyFallbackRemovedIn = "1.2.0"

// The legacy file is an input format only (assumption-delta decision). It is
// decoded leniently into legacyConfig, mapped into the one primary Config, and
// then run through the same Validate as a native file. Nothing downstream
// knows which file was read. It is NOT decoded with KnownFields: the fork's
// files hold chatops, mappings and queue keys the agent no longer has.
//
// Key names follow voxpupuli/webhook-go config/config.go (Apache-2.0,
// https://github.com/voxpupuli/webhook-go).
type legacyConfig struct {
	Server   legacyServer  `yaml:"server"`
	ChatOps  legacyChatOps `yaml:"chatops"`
	R10k     legacyR10k    `yaml:"r10k"`
	Deploys  legacyDeploys `yaml:"deploys"`
	Mappings yaml.Node     `yaml:"mappings"`
}

type legacyServer struct {
	Protected           bool        `yaml:"protected"`
	User                yaml.Node   `yaml:"user"`
	Password            yaml.Node   `yaml:"password"`
	Port                *int        `yaml:"port"`
	DeployOnSuccessOnly yaml.Node   `yaml:"deploy_on_success_only"`
	TLS                 legacyTLS   `yaml:"tls"`
	Queue               legacyQueue `yaml:"queue"`
}

type legacyTLS struct {
	Enabled             yaml.Node `yaml:"enabled"`
	Certificate         string    `yaml:"certificate"`
	Key                 string    `yaml:"key"`
	ClientCA            string    `yaml:"client_ca"`
	AllowedCertnames    []string  `yaml:"allowed_certnames"`
	CRL                 string    `yaml:"crl"`
	AllowedFingerprints []string  `yaml:"allowed_fingerprints"`
}

type legacyQueue struct {
	Enabled           yaml.Node `yaml:"enabled"`
	MaxConcurrentJobs yaml.Node `yaml:"max_concurrent_jobs"`
	MaxHistoryItems   yaml.Node `yaml:"max_history_items"`
}

type legacyChatOps struct {
	Enabled   bool      `yaml:"enabled"`
	Service   yaml.Node `yaml:"service"`
	Channel   yaml.Node `yaml:"channel"`
	User      yaml.Node `yaml:"user"`
	AuthToken yaml.Node `yaml:"auth_token"`
	ServerURI yaml.Node `yaml:"server_uri"`
}

type legacyR10k struct {
	CommandPath             string    `yaml:"command_path"`
	ConfigPath              string    `yaml:"config_path"`
	DefaultBranch           yaml.Node `yaml:"default_branch"`
	Prefix                  yaml.Node `yaml:"prefix"`
	AllowUppercase          yaml.Node `yaml:"allow_uppercase"`
	Verbose                 *bool     `yaml:"verbose"`
	DeployModules           *bool     `yaml:"deploy_modules"`
	UseLegacyPuppetfileFlag *bool     `yaml:"use_legacy_puppetfile_flag"`
	GenerateTypes           *bool     `yaml:"generate_types"`
	EnvIncremental          *bool     `yaml:"env_incremental"`
	BlockedBranches         yaml.Node `yaml:"blocked_branches"`
	IgnoreBranchPrefixes    yaml.Node `yaml:"ignore_branch_prefixes"`
	UseG10kCommands         bool      `yaml:"use_g10k_commands"`
}

type legacyDeploys struct {
	Basedir     string    `yaml:"basedir"`
	StateDir    string    `yaml:"state_dir"`
	Timeout     yaml.Node `yaml:"timeout"`
	OutputLimit *int      `yaml:"output_limit"`
	MaxJobs     *int      `yaml:"max_jobs"`
}

// The fork's own defaults (config/config.go setDefaults), reproduced when a
// legacy key is absent so an old file behaves as it did (PD-15).
const (
	legacyPort        = 4000
	legacyCommandPath = "/opt/puppetlabs/puppetserver/bin/r10k"
	legacyStateDir    = "/var/lib/webhook-go"
)

// ignoredLegacy lists the legacy keys that no longer do anything. A trailing
// dot is a prefix. Keys that refuse start-up (chatops.enabled: true,
// server.protected: true) are left out of the aggregated warning.
var ignoredLegacy = []string{
	"server.protected", "server.user", "server.password", "server.deploy_on_success_only",
	"server.tls.enabled", "server.queue.",
	"chatops.",
	"r10k.default_branch", "r10k.prefix", "r10k.allow_uppercase", "r10k.blocked_branches", "r10k.ignore_branch_prefixes",
	"mappings",
}

func isIgnoredLegacy(key string) bool {
	for _, k := range ignoredLegacy {
		if key == k || (strings.HasSuffix(k, ".") && strings.HasPrefix(key, k)) {
			return true
		}
	}
	return false
}

func parseLegacy(b []byte, name string) (*Config, []Finding) {
	fs := []Finding{warnf("config", "using legacy config %s; write %s, this fallback is removed in %s", LegacyPath, DefaultPath, LegacyFallbackRemovedIn)}

	sc, err := scanAgainst(b, reflect.TypeOf(legacyConfig{}))
	if err != nil {
		return nil, append(fs, errorf("config", "%s: %v", name, err))
	}
	for _, u := range sc.unknown {
		fs = append(fs, warnf(u.key, "unknown key in the legacy file (line %d); ignored", u.line))
	}

	var l legacyConfig
	if err := yaml.NewDecoder(bytes.NewReader(b)).Decode(&l); err != nil && !errors.Is(err, io.EOF) {
		fs = append(fs, decodeFindings(err, sc, false)...)
	}

	c, mapped := mapLegacy(&l, sc)
	fs = append(fs, mapped...)
	c.normalize()
	fs = append(fs, c.Validate()...)
	fs = dedupe(fs)
	if HasErrors(fs) {
		return nil, fs
	}
	return c, fs
}

// mapLegacy maps the legacy file onto the primary schema (RESEARCH section 5).
func mapLegacy(l *legacyConfig, sc *scan) (*Config, []Finding) {
	var fs []Finding
	c := &Config{}

	c.Server.Port = legacyPort
	if l.Server.Port != nil {
		c.Server.Port = *l.Server.Port
	} else {
		fs = append(fs, warnf("server.port", "legacy default port %d in use; the pack expects %d", legacyPort, DefaultPort))
	}
	c.Server.TLS = TLS{
		Certificate:         l.Server.TLS.Certificate,
		Key:                 l.Server.TLS.Key,
		ClientCA:            l.Server.TLS.ClientCA,
		CRL:                 l.Server.TLS.CRL,
		AllowedCertnames:    l.Server.TLS.AllowedCertnames,
		AllowedFingerprints: l.Server.TLS.AllowedFingerprints,
	}
	c.Ops.Listen = DefaultOpsListen

	r := l.R10k
	c.Engine = Engine{
		Name:                    "r10k",
		CommandPath:             orString(r.CommandPath, legacyCommandPath),
		ConfigPath:              orString(r.ConfigPath, DefaultR10kConfig),
		Verbose:                 boolPtr(orBool(r.Verbose, true)),
		DeployModules:           boolPtr(orBool(r.DeployModules, true)),
		GenerateTypes:           boolPtr(orBool(r.GenerateTypes, true)),
		EnvIncremental:          orBool(r.EnvIncremental, false),
		UseLegacyPuppetfileFlag: orBool(r.UseLegacyPuppetfileFlag, false),
	}
	if r.UseG10kCommands {
		c.Engine.Name = "g10k"
	}

	d := l.Deploys
	c.Deploys = Deploys{
		Basedir:               orString(d.Basedir, "/etc/puppetlabs/code/environments"),
		StateDir:              orString(d.StateDir, legacyStateDir),
		Timeout:               Duration(15 * time.Minute),
		OutputLimit:           262144,
		MaxJobs:               500,
		ProtectedEnvironments: []string{"production"},
	}
	if d.OutputLimit != nil {
		c.Deploys.OutputLimit = *d.OutputLimit
	}
	if d.MaxJobs != nil {
		c.Deploys.MaxJobs = *d.MaxJobs
	}
	if d.Timeout.Kind != 0 {
		if v, err := ParseDurationNode(&d.Timeout); err != nil {
			fs = append(fs, errorf("deploys.timeout", "%v (line %d)", err, d.Timeout.Line))
		} else {
			c.Deploys.Timeout = v
		}
	}
	c.Log.Level = "info"

	// Removed features that are switched on, and the plain-webhook mode, stop
	// the agent rather than being dropped silently (D-15, T-57.6.2-11).
	refused := map[string]bool{}
	if l.ChatOps.Enabled {
		refused["chatops.enabled"] = true
		fs = append(fs, errorf("chatops.enabled", "chatops was removed from the agent; set it to false or delete the chatops section"))
	}
	if l.Server.Protected {
		refused["server.protected"] = true
		fs = append(fs, errorf("server.protected", "basic authentication was removed; the agent authenticates callers by client certificate. Set it to false and list the callers in server.tls.allowed_certnames"))
	}
	if l.Server.TLS.ClientCA == "" {
		fs = append(fs, errorf("server.tls.client_ca", "is empty: the legacy plain-webhook mode was removed and the agent serves mutual TLS only; set the CA that client certificates must chain to"))
	}

	// One aggregated warning for what is ignored, in file order.
	type kl struct {
		key  string
		line int
	}
	var ignored []kl
	for key, line := range sc.present {
		if isIgnoredLegacy(key) && !refused[key] {
			ignored = append(ignored, kl{key, line})
		}
	}
	sort.Slice(ignored, func(i, j int) bool { return ignored[i].line < ignored[j].line })
	if len(ignored) > 0 {
		names := make([]string, len(ignored))
		for i, k := range ignored {
			names[i] = k.key
		}
		fs = append(fs, warnf("config", "ignored, because the agent no longer uses them: %s", strings.Join(names, ", ")))
	}
	return c, fs
}

func orString(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func orBool(v *bool, def bool) bool {
	if v == nil {
		return def
	}
	return *v
}
