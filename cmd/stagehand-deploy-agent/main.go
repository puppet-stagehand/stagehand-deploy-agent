// Command stagehand-deploy-agent is the Stagehand deploy agent: a small mTLS
// service that runs r10k or g10k for the Code Manager pack and reports each
// deploy's outcome.
//
//	stagehand-deploy-agent serve [--config PATH]
//	stagehand-deploy-agent --check-config [--config PATH [--legacy]]
//	stagehand-deploy-agent import-history --from DIR [--config PATH]
//	stagehand-deploy-agent version
//
// Exit codes: 0 ok; 1 runtime error; 2 usage error; 77 serve run as root;
// 78 configuration ERROR.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/puppet-stagehand/stagehand-deploy-agent/internal/config"
	"github.com/puppet-stagehand/stagehand-deploy-agent/internal/jobs"
	"github.com/puppet-stagehand/stagehand-deploy-agent/internal/server"
)

// version is overridden at build time: -ldflags "-X main.version=1.0.0".
var version = "dev"

// Exit codes (PD-16).
const (
	exitOK        = 0
	exitRuntime   = 1
	exitUsage     = 2
	exitRoot      = 77 // serve refuses to run as root (D-20)
	exitBadConfig = 78 // EX_CONFIG: a configuration ERROR
)

// geteuid is a variable so a test can stand in for root.
var geteuid = os.Geteuid

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}
	switch {
	case args[0] == "serve":
		return serve(args[1:], stderr)
	case args[0] == "import-history":
		return importHistory(args[1:], stdout, stderr)
	case args[0] == "version":
		fmt.Fprintf(stdout, "stagehand-deploy-agent %s\n", version)
		return exitOK
	case strings.HasPrefix(args[0], "-"):
		return topLevel(args, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "stagehand-deploy-agent: unknown command %q\n", args[0])
		usage(stderr)
		return exitUsage
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, `usage: stagehand-deploy-agent serve [--config PATH]
       stagehand-deploy-agent --check-config [--config PATH [--legacy]]
       stagehand-deploy-agent import-history --from DIR [--config PATH]
       stagehand-deploy-agent version`)
}

// topLevel handles the flag forms: --check-config. --legacy exists only to let
// --check-config read a legacy file at another path; serve reaches the legacy
// reader through config.Locate alone.
func topLevel(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("stagehand-deploy-agent", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	check := fs.Bool("check-config", false, "check the configuration file and exit")
	explicit := fs.String("config", "", "path to the YAML configuration file")
	legacyFlag := fs.Bool("legacy", false, "read --config as a legacy /etc/voxpupuli/webhook.yml (with --check-config)")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || !*check || (*legacyFlag && *explicit == "") {
		usage(stderr)
		return exitUsage
	}
	path, legacy, err := config.Locate(*explicit, config.DefaultPath, config.LegacyPath)
	if err != nil {
		config.Report(stdout, "(none)", []config.Finding{{Level: config.LevelError, Key: "config", Message: err.Error()}})
		return exitBadConfig
	}
	legacy = legacy || *legacyFlag
	cfg, findings := config.Load(path, legacy)
	if cfg != nil {
		findings = append(findings, config.CheckFiles(cfg)...)
	}
	config.Report(stdout, path, findings)
	if config.HasErrors(findings) {
		return exitBadConfig
	}
	return exitOK
}

func serve(args []string, stderr io.Writer) int {
	// D-20: the agent has no reason to be root, and there is no flag,
	// variable or key that lets it. This is the first thing serve does.
	if geteuid() == 0 {
		fmt.Fprintln(stderr, "stagehand-deploy-agent: refusing to run as root (uid 0); run it as its own user, for example with the packaged systemd unit")
		return exitRoot
	}
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	explicit := fs.String("config", "", "path to the YAML configuration file")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		usage(stderr)
		return exitUsage
	}
	path, legacy, err := config.Locate(*explicit, config.DefaultPath, config.LegacyPath)
	if err != nil {
		fmt.Fprintf(stderr, "stagehand-deploy-agent: refusing to start: %v\n", err)
		return exitBadConfig
	}
	cfg, findings := config.Load(path, legacy)
	if cfg == nil {
		for _, f := range findings {
			fmt.Fprintln(stderr, f)
		}
		fmt.Fprintf(stderr, "stagehand-deploy-agent: refusing to start: config %s has errors\n", path)
		return exitBadConfig
	}
	log := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: logLevel(cfg.Log.Level)}))
	for _, f := range findings {
		log.Warn("config", "component", "config", "key", f.Key, "message", f.Message, "file", path)
	}
	if err := runServe(cfg, log); err != nil {
		fmt.Fprintf(stderr, "stagehand-deploy-agent: refusing to start: %v\n", err)
		return exitRuntime
	}
	return exitOK
}

// importHistory copies the fork's finished deploy records from DIR (its
// <state_dir>/jobs) into the agent's store (D-02, PD-23). Unlike serve it may
// run as root: the cutover script does, because the legacy directory is root's.
// The source is only read. Exit 0 on success (records skipped are not errors),
// 1 if DIR cannot be read or the import fails, 78 on a configuration ERROR.
func importHistory(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("import-history", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	from := fs.String("from", "", "the legacy jobs directory to read (<old state_dir>/jobs)")
	explicit := fs.String("config", "", "path to the YAML configuration file")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *from == "" {
		usage(stderr)
		return exitUsage
	}
	path, legacy, err := config.Locate(*explicit, config.DefaultPath, config.LegacyPath)
	if err != nil {
		fmt.Fprintf(stderr, "stagehand-deploy-agent: import-history: %v\n", err)
		return exitBadConfig
	}
	cfg, findings := config.Load(path, legacy)
	if cfg == nil {
		for _, f := range findings {
			fmt.Fprintln(stderr, f)
		}
		fmt.Fprintf(stderr, "stagehand-deploy-agent: import-history: config %s has errors\n", path)
		return exitBadConfig
	}
	// Before anything is created: the source must be a readable directory.
	if info, err := os.Stat(*from); err != nil || !info.IsDir() {
		if err == nil {
			err = fmt.Errorf("not a directory")
		}
		fmt.Fprintf(stderr, "stagehand-deploy-agent: import-history: cannot read --from %s: %v\n", *from, err)
		return exitRuntime
	}
	if _, err := os.ReadDir(*from); err != nil {
		fmt.Fprintf(stderr, "stagehand-deploy-agent: import-history: cannot read --from %s: %v\n", *from, err)
		return exitRuntime
	}
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	store, err := jobs.NewFileStore(cfg.Deploys.StateDir, cfg.Deploys.MaxJobs, jobs.WithLogger(log))
	if err != nil {
		fmt.Fprintf(stderr, "stagehand-deploy-agent: import-history: %v\n", err)
		return exitRuntime
	}
	rep, err := jobs.ImportHistory(*from, store)
	if cerr := store.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		fmt.Fprintf(stderr, "stagehand-deploy-agent: import-history: %v\n", err)
		return exitRuntime
	}
	fmt.Fprintf(stdout, "imported %d; skipped %d already present, %d unfinished, %d unreadable\n",
		rep.Imported, rep.SkippedExisting, rep.SkippedUnfinished, rep.SkippedUnreadable)
	if rep.SkippedBeyondMax > 0 {
		fmt.Fprintf(stderr, "stagehand-deploy-agent: import-history: %d older record(s) were not copied: deploys.max_jobs (%d) keeps only the newest\n",
			rep.SkippedBeyondMax, cfg.Deploys.MaxJobs)
	}
	return exitOK
}

func logLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// runServe builds the store, runner, mux and mTLS server from the checked
// config and runs them until SIGTERM or SIGINT.
func runServe(cfg *config.Config, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	opts, err := server.OptionsFromConfig(ctx, cfg, log)
	if err != nil {
		return err
	}
	// OptionsFromConfig has reconciled the store; now delete what a crash in the
	// middle of an environment removal left behind, before any request is
	// accepted (D-09).
	if n, err := opts.Runner.SweepRemovals(); err != nil {
		log.Warn("removal leftovers were not swept; they stay until the next start", "component", "server", "error", err.Error())
	} else {
		log.Info("removal leftovers swept", "component", "server", "deleted", n)
	}
	srv, err := server.New(opts)
	if err != nil {
		_ = opts.Store.Close()
		return err
	}
	log.Info("listening", "component", "server", "addr", opts.Addr, "version", version, "engine", cfg.Engine.Name)
	return srv.Serve(ctx)
}

// engineVersion returns the first line of the engine's own version output.
func engineVersion(e config.Engine, timeout time.Duration) string { return "" }
