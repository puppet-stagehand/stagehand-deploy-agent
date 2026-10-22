package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/puppet-stagehand/stagehand-deploy-agent/internal/config"
	"github.com/puppet-stagehand/stagehand-deploy-agent/internal/jobs"
	"github.com/puppet-stagehand/stagehand-deploy-agent/internal/jobs/fakeengine"
)

// TestMain lets this test binary stand in for r10k (see package fakeengine).
func TestMain(m *testing.M) {
	fakeengine.MaybeRun()
	os.Exit(m.Run())
}

// fork-argv.jsonl was recorded by running the fork's own helpers
// (webhook-go v2.15.1-stagehand.1, lib/helpers GetR10kDeployEnvironmentCommand
// (env, true) and GetG10kDeployEnvironmentCommand(env, env), configured through
// the fork's config.Init) for every combination of verbose, deploy_modules,
// generate_types, env_incremental and use_legacy_puppetfile_flag, at two
// config paths. Each line is one case. Do not edit it by hand.
type forkCase struct {
	Engine                  string   `json:"engine"`
	ConfigPath              string   `json:"config_path"`
	Verbose                 bool     `json:"verbose"`
	DeployModules           bool     `json:"deploy_modules"`
	GenerateTypes           bool     `json:"generate_types"`
	EnvIncremental          bool     `json:"env_incremental"`
	UseLegacyPuppetfileFlag bool     `json:"use_legacy_puppetfile_flag"`
	Argv                    []string `json:"argv"`
}

func readForkCases(t *testing.T) []forkCase {
	t.Helper()
	f, err := os.Open("testdata/fork-argv.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var cases []forkCase
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var c forkCase
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		cases = append(cases, c)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return cases
}

func cfgFor(c forkCase) config.Engine {
	return config.Engine{
		Name:                    c.Engine,
		CommandPath:             "/opt/bin/tool",
		ConfigPath:              c.ConfigPath,
		Verbose:                 &c.Verbose,
		DeployModules:           &c.DeployModules,
		GenerateTypes:           &c.GenerateTypes,
		EnvIncremental:          c.EnvIncremental,
		UseLegacyPuppetfileFlag: c.UseLegacyPuppetfileFlag,
	}
}

// The r10k and g10k argv equal the fork's for the same inputs, for all 128
// recorded combinations.
func TestEngineArgvEqualsForkArgv(t *testing.T) {
	cases := readForkCases(t)
	if len(cases) != 128 {
		t.Fatalf("recorded %d cases, want 128 (2 engines x 32 flag combinations x 2 config paths)", len(cases))
	}
	for _, c := range cases {
		eng, err := Build(cfgFor(c))
		if err != nil {
			t.Fatal(err)
		}
		if got := eng.Argv("production"); !slices.Equal(got, c.Argv) {
			t.Errorf("%s verbose=%v modules=%v types=%v incremental=%v legacy=%v config=%s:\n got  %q\n want %q",
				c.Engine, c.Verbose, c.DeployModules, c.GenerateTypes, c.EnvIncremental, c.UseLegacyPuppetfileFlag, c.ConfigPath, got, c.Argv)
		}
	}
}

// The argv is a slice executed directly: the command comes first, nothing goes
// through a shell, and a hostile environment name stays one element (the
// runner restricts names to ^[a-z0-9_]+$ before a job exists; this is the
// second wall).
func TestEngineArgvIsNeverAShellString(t *testing.T) {
	for _, c := range readForkCases(t) {
		argv := cfgFor(c)
		eng, _ := Build(argv)
		got := eng.Argv("x; rm -rf /")
		if got[0] != "/opt/bin/tool" {
			t.Fatalf("argv[0] = %q", got[0])
		}
		for _, a := range got {
			if a == "-c" || a == "sh" || a == "bash" || a == "/bin/sh" {
				t.Fatalf("argv hands the engine to a shell: %q", got)
			}
		}
		n := 0
		for _, a := range got {
			if strings.Contains(a, "rm -rf /") {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("the environment name is not exactly one argv element: %q", got)
		}
	}
}

// Build names the engine, its marker, and starts the child with
// GIT_TERMINAL_PROMPT=0.
func TestEngineBuildNamesMarkerAndEnv(t *testing.T) {
	for name, marker := range map[string]string{"r10k": ".r10k-deploy.json", "g10k": ".g10k-deploy.json"} {
		eng, err := Build(config.Engine{Name: name, CommandPath: "/x"})
		if err != nil {
			t.Fatal(err)
		}
		if eng.Name != name || eng.MarkerFile != marker {
			t.Fatalf("%s: engine = %+v", name, eng)
		}
		if !slices.Equal(eng.Env, []string{"GIT_TERMINAL_PROMPT=0"}) {
			t.Fatalf("%s: Env = %q", name, eng.Env)
		}
	}
}

func TestEngineBuildRefusesUnknownEngine(t *testing.T) {
	for _, name := range []string{"", "puppet", "R10K"} {
		if _, err := Build(config.Engine{Name: name}); err == nil || !strings.Contains(err.Error(), "engine.name") {
			t.Fatalf("Build(%q) err = %v, want one naming engine.name", name, err)
		}
	}
}

// Empty paths fall back as the fork's helpers did: the bare tool name, and
// r10k's stock config file.
func TestEngineFallbackPaths(t *testing.T) {
	if got := R10kDeployArgv(config.Engine{}, "production"); got[0] != "r10k" || got[4] != "--config=/etc/puppetlabs/r10k/r10k.yaml" {
		t.Fatalf("r10k fallback argv = %q", got)
	}
	if got := G10kDeployArgv(config.Engine{}, "production"); got[0] != "g10k" || got[1] != "-config=/etc/puppetlabs/r10k/r10k.yaml" {
		t.Fatalf("g10k fallback argv = %q", got)
	}
}

const sshSHA = "0123456789abcdef0123456789abcdef01234567"

// deployThroughRunner runs one real deploy through a real jobs.Runner and the
// fake engine, and returns the git variables the engine child was started with.
func deployThroughRunner(t *testing.T, cfg config.Engine) []string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Name, cfg.CommandPath = "r10k", exe
	eng, err := Build(cfg)
	if err != nil {
		t.Fatal(err)
	}
	fixture, _ := filepath.Abs("../jobs/testdata/r10k-5.0.3/deploy-success.json")
	basedir := t.TempDir()
	envLog := filepath.Join(t.TempDir(), "env.log")
	t.Setenv(fakeengine.FixtureEnv, fixture)
	t.Setenv(fakeengine.BasedirEnv, basedir)
	t.Setenv(fakeengine.SHAEnv, sshSHA)
	t.Setenv(fakeengine.EnvLogEnv, envLog)

	r, err := jobs.NewRunner(jobs.Options{Engine: eng, Basedir: basedir, Timeout: time.Minute, OutputLimit: 4096, Store: jobs.NewMemoryStore(10)})
	if err != nil {
		t.Fatal(err)
	}
	job, err := r.NewJob([]jobs.EnvRequest{{Name: "production", ExpectedSHA: sshSHA}})
	if err != nil {
		t.Fatal(err)
	}
	job, err = r.Run(context.Background(), job)
	if err != nil || job.State != jobs.StateDeployed {
		t.Fatalf("deploy: state=%s err=%v job=%+v", job.State, err, job)
	}
	b, err := os.ReadFile(envLog)
	if err != nil {
		t.Fatalf("the fake engine recorded no environment: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// D-03: with engine.ssh_config set, r10k's git is told to use the agent's own
// ssh_config, and an ssh command inherited from the agent's environment is
// replaced, not appended to (T-57.6.2-10).
func TestEngineSSHConfigReachesTheChild(t *testing.T) {
	t.Setenv("GIT_SSH_COMMAND", "ssh -o StrictHostKeyChecking=no")
	got := deployThroughRunner(t, config.Engine{SSHConfig: "/etc/stagehand-deploy-agent/ssh_config"})
	want := []string{"GIT_SSH_COMMAND=ssh -F /etc/stagehand-deploy-agent/ssh_config", "GIT_TERMINAL_PROMPT=0"}
	if !slices.Equal(got, want) {
		t.Fatalf("child environment = %q, want %q", got, want)
	}
}

// Without engine.ssh_config the agent sets no ssh command of its own and an
// inherited one is removed, so a stray environment cannot weaken pinning.
func TestEngineSSHAbsentUnsetsAnInheritedCommand(t *testing.T) {
	t.Setenv("GIT_SSH_COMMAND", "ssh -o StrictHostKeyChecking=no")
	got := deployThroughRunner(t, config.Engine{})
	want := []string{"GIT_SSH_COMMAND=unset", "GIT_TERMINAL_PROMPT=0"}
	if !slices.Equal(got, want) {
		t.Fatalf("child environment = %q, want %q", got, want)
	}
}

// git passes GIT_SSH_COMMAND through a shell, so a path with anything outside
// [A-Za-z0-9._/-] never reaches it.
func TestEngineSSHRefusesAPathGitWouldPassToAShell(t *testing.T) {
	for _, p := range []string{"/etc/x y", "/etc/x;id", "/etc/$(id)", "/etc/`id`", "relative/ssh_config", "/etc/x'y", "/etc/x\ny"} {
		if _, err := Build(config.Engine{Name: "r10k", SSHConfig: p}); err == nil || !strings.Contains(err.Error(), "engine.ssh_config") {
			t.Errorf("Build with ssh_config %q: err = %v, want one naming engine.ssh_config", p, err)
		}
	}
}
