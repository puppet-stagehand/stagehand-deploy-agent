// Command stagehand-deploy-agent is the Stagehand deploy agent: a small mTLS
// service that runs r10k or g10k for the Code Manager pack and reports each
// deploy's outcome. Subcommands: serve, version.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/puppet-stagehand/stagehand-deploy-agent/internal/config"
	"github.com/puppet-stagehand/stagehand-deploy-agent/internal/server"
)

// version is overridden at build time: -ldflags "-X main.version=1.0.0".
var version = "dev"

// geteuid is a variable so tests can stand in for root.
var geteuid = os.Geteuid

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	switch args[0] {
	case "serve":
		return serve(args[1:])
	case "version":
		fmt.Printf("stagehand-deploy-agent %s\n", version)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "stagehand-deploy-agent: unknown command %q\n", args[0])
		usage()
		return 2
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: stagehand-deploy-agent serve [--config PATH]\n       stagehand-deploy-agent version")
}

func serve(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	path := fs.String("config", config.DefaultPath, "path to the YAML configuration file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := runServe(*path, log); err != nil {
		fmt.Fprintf(os.Stderr, "stagehand-deploy-agent: refusing to start: %v\n", err)
		return 1
	}
	return 0
}

// runServe builds the store, runner, mux and mTLS server from the strict
// config and runs them until SIGTERM or SIGINT.
func runServe(path string, log *slog.Logger) error {
	cfg, findings := config.Load(path, false)
	if cfg == nil {
		return fmt.Errorf("%v", findings)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	opts, err := server.OptionsFromConfig(ctx, cfg, log)
	if err != nil {
		return err
	}
	srv, err := server.New(opts)
	if err != nil {
		_ = opts.Store.Close()
		return err
	}
	log.Info("listening", "component", "server", "addr", opts.Addr, "version", version, "engine", cfg.Engine.Name)
	return srv.Serve(ctx)
}
