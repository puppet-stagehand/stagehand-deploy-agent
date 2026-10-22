// Derived from voxpupuli/webhook-go lib/helpers/r10k-command.go and
// g10k-command.go (Apache-2.0, https://github.com/voxpupuli/webhook-go).
// Modified for the Stagehand deploy agent.
//
// Package engine builds the argv the deploy runner executes for r10k and g10k.
// The argv is a plain slice handed to exec directly, never a shell string
// (T-57.6.2-02), so an environment name can never become shell syntax; the
// runner has already restricted names to ^[a-z0-9_]+$.
package engine

import (
	"fmt"

	"github.com/puppet-stagehand/stagehand-deploy-agent/internal/config"
	"github.com/puppet-stagehand/stagehand-deploy-agent/internal/jobs"
)

const (
	r10kCommand = "r10k"
	g10kCommand = "g10k"
	// DefaultR10kConfig is the r10k configuration file when none is set.
	DefaultR10kConfig = "/etc/puppetlabs/r10k/r10k.yaml"
)

// Build returns the runner engine for cfg. The child is started with
// GIT_TERMINAL_PROMPT=0, so a git that needs a password fails at once instead
// of waiting on a terminal nobody is at.
//
// ssh (D-03): with engine.ssh_config set, git is told to use that file and no
// other (ssh -F skips ~/.ssh/config and /etc/ssh/ssh_config, so a Host * block
// elsewhere cannot override the pinned host keys). Without it the agent sets no
// ssh command, and one inherited from the agent's own environment is removed so
// a stray value cannot weaken pinning. git passes GIT_SSH_COMMAND through a
// shell, so a path with anything outside [A-Za-z0-9._/-] is refused here as
// well as by config validation (T-57.6.2-10).
func Build(cfg config.Engine) (jobs.Engine, error) {
	var e jobs.Engine
	switch cfg.Name {
	case "r10k":
		e = jobs.R10kEngine(func(env string) []string { return R10kDeployArgv(cfg, env) })
	case "g10k":
		e = jobs.G10kEngine(func(env string) []string { return G10kDeployArgv(cfg, env) })
	default:
		return jobs.Engine{}, fmt.Errorf("engine.name must be r10k or g10k, got %q", cfg.Name)
	}
	e.Env = []string{"GIT_TERMINAL_PROMPT=0"}
	if cfg.SSHConfig == "" {
		e.Unset = []string{"GIT_SSH_COMMAND"}
		return e, nil
	}
	if !config.ValidSSHConfigPath(cfg.SSHConfig) {
		return jobs.Engine{}, fmt.Errorf("engine.ssh_config must be an absolute path of letters, digits, '.', '_', '/' and '-' only, got %q", cfg.SSHConfig)
	}
	e.Env = append(e.Env, fmt.Sprintf("GIT_SSH_COMMAND=ssh -F %s", cfg.SSHConfig))
	return e, nil
}

func commandPath(cfg config.Engine, fallback string) string {
	if cfg.CommandPath == "" {
		return fallback
	}
	return cfg.CommandPath
}

func configPath(cfg config.Engine) string {
	if cfg.ConfigPath == "" {
		return DefaultR10kConfig
	}
	return cfg.ConfigPath
}

func set(b *bool) bool { return b != nil && *b }

// R10kDeployArgv is upstream's GetR10kDeployEnvironmentCommand(env, true): it
// deploys the environment and, when engine.deploy_modules is on, its modules.
func R10kDeployArgv(cfg config.Engine, env string) []string {
	cmd := []string{commandPath(cfg, r10kCommand), "deploy", "environment", env,
		fmt.Sprintf("--config=%s", configPath(cfg))}
	if set(cfg.Verbose) {
		cmd = append(cmd, "--verbose")
	}
	if set(cfg.GenerateTypes) {
		cmd = append(cmd, "--generate-types")
	}
	if set(cfg.DeployModules) {
		if cfg.UseLegacyPuppetfileFlag {
			cmd = append(cmd, "--puppetfile")
		} else {
			cmd = append(cmd, "--modules")
			if cfg.EnvIncremental {
				cmd = append(cmd, "--incremental")
			}
		}
	}
	return cmd
}

// G10kDeployArgv is upstream's GetG10kDeployEnvironmentCommand(env, env): the
// branch equals the environment, which selects -branch=<env>.
func G10kDeployArgv(cfg config.Engine, env string) []string {
	cmd := []string{commandPath(cfg, g10kCommand), fmt.Sprintf("-config=%s", configPath(cfg)),
		fmt.Sprintf("-branch=%s", env)}
	if set(cfg.Verbose) {
		cmd = append(cmd, "-verbose")
	}
	return cmd
}
