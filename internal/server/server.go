package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"time"

	"github.com/puppet-stagehand/stagehand-deploy-agent/internal/config"
	"github.com/puppet-stagehand/stagehand-deploy-agent/internal/engine"
	"github.com/puppet-stagehand/stagehand-deploy-agent/internal/jobs"
)

// shutdownGrace bounds how long a stopping server waits for requests to end
// and for deploy records to be written.
const shutdownGrace = 30 * time.Second

// Options assemble a Server.
type Options struct {
	// Addr is the listen address, "host:port" (host empty for all interfaces).
	Addr   string
	TLS    TLSConfig
	Runner *jobs.Runner
	// Store is the runner's file store, closed by Serve as its last step so
	// the instance lock file does not outlive the process (FORK-WR-03). Nil
	// means nothing to close.
	Store  io.Closer
	Logger *slog.Logger
}

// Server is the mTLS deploy API.
type Server struct {
	srv    *http.Server
	runner *jobs.Runner
	store  io.Closer
	log    *slog.Logger
}

// New builds the mux and the mTLS server. It loads and checks every file the
// TLS configuration names, so a bad configuration fails here, before a
// listener opens.
func New(o Options) (*Server, error) {
	if o.Runner == nil {
		return nil, errors.New("server: no runner")
	}
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	srv, err := NewMTLSServer(o.Addr, o.TLS, NewMux(NewDeployHandlers(o.Runner, log)), log)
	if err != nil {
		return nil, err
	}
	return &Server{srv: srv, runner: o.Runner, store: o.Store, log: log.With("component", "server")}, nil
}

// OptionsFromConfig builds the store and the runner for cfg, in the order the
// fork used: open the FileStore, fail the jobs a dead process left unfinished
// (before any request is accepted, so no client polls one forever), then the
// runner with its cross-process lock at <state_dir>/deploy.lock. ctx is the
// runner's base context: cancelling it stops running engines and fails queued
// jobs.
func OptionsFromConfig(ctx context.Context, cfg *config.Config, log *slog.Logger) (Options, error) {
	if log == nil {
		log = slog.Default()
	}
	eng, err := engine.Build(cfg.Engine)
	if err != nil {
		return Options{}, err
	}
	store, err := jobs.NewFileStore(cfg.Deploys.StateDir, cfg.Deploys.MaxJobs, jobs.WithLogger(log))
	if err != nil {
		return Options{}, err
	}
	if n, err := store.Reconcile(); err != nil {
		store.Close()
		return Options{}, fmt.Errorf("reconciling deploy records: %w", err)
	} else if n > 0 {
		log.Warn("deploys interrupted by a restart were marked failed", "component", "server", "count", n)
	}
	runner, err := jobs.NewRunner(jobs.Options{
		Engine:      eng,
		Basedir:     cfg.Deploys.Basedir,
		Timeout:     cfg.Deploys.Timeout.Std(),
		OutputLimit: cfg.Deploys.OutputLimit,
		Store:       store,
		Logger:      log,
		LockPath:    filepath.Join(cfg.Deploys.StateDir, "deploy.lock"),
		Context:     ctx,
	})
	if err != nil {
		store.Close()
		return Options{}, err
	}
	t := cfg.Server.TLS
	return Options{
		Addr:   fmt.Sprintf("%s:%d", cfg.Server.Address, cfg.Server.Port),
		TLS:    TLSConfig{Certificate: t.Certificate, Key: t.Key, ClientCA: t.ClientCA, CRL: t.CRL, AllowedCertnames: t.AllowedCertnames, AllowedFingerprints: t.AllowedFingerprints},
		Runner: runner,
		Store:  store,
		Logger: log,
	}, nil
}

// Handler returns the server's mux, for tests that serve it themselves.
func (s *Server) Handler() http.Handler { return s.srv.Handler }

// HTTPServer returns the underlying http.Server (tests serve it on a listener
// of their own).
func (s *Server) HTTPServer() *http.Server { return s.srv }

// Serve listens and serves until ctx is cancelled, then shuts down in an order
// that leaves every admitted deploy on record (PD-18, FORK-WR-01):
//
//  1. the HTTP server starts to shut down in the background: listeners close at
//     once, requests in flight (a wait:true deploy among them) carry on;
//  2. the runner shuts down: it refuses new jobs, cancels its context, which
//     stops a running engine (its whole process group) and fails queued jobs,
//     and waits until every admitted job, wait:true included, has written its
//     final record;
//  3. Serve waits for the HTTP shutdown, so each answer has been written;
//  4. the store is closed, which removes the instance lock file (FORK-WR-03).
//
// Under systemd TimeoutStopSec=60 this fits: steps 1 to 3 are bounded by
// shutdownGrace together.
func (s *Server) Serve(ctx context.Context) error {
	errc := make(chan error, 1)
	go func() { errc <- s.srv.ListenAndServeTLS("", "") }()
	var first error
	keep := func(err error) {
		if first == nil && err != nil {
			first = err
		}
	}
	select {
	case err := <-errc: // the listener failed (address in use, ...)
		keep(err)
		keep(s.runner.Shutdown(shutdownGrace))
		keep(s.closeStore())
		return first
	case <-ctx.Done():
	}
	s.log.Info("shutting down")
	httpDone := make(chan error, 1)
	go func() {
		sctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		httpDone <- s.srv.Shutdown(sctx)
	}()
	if err := s.runner.Shutdown(shutdownGrace); err != nil {
		s.log.Error("runner shutdown", "error", err.Error())
		keep(err)
	}
	if err := <-httpDone; err != nil {
		s.log.Error("shutdown", "error", err.Error())
		keep(err)
		_ = s.srv.Close() // drop what the grace period could not drain
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		keep(err)
	}
	keep(s.closeStore())
	return first
}

func (s *Server) closeStore() error {
	if s.store == nil {
		return nil
	}
	if err := s.store.Close(); err != nil {
		s.log.Error("closing the job store", "error", err.Error())
		return err
	}
	return nil
}
