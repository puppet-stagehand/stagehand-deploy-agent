package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"time"

	"github.com/puppet-stagehand/stagehand-deploy-agent/internal/config"
	"github.com/puppet-stagehand/stagehand-deploy-agent/internal/engine"
	"github.com/puppet-stagehand/stagehand-deploy-agent/internal/jobs"
	"github.com/puppet-stagehand/stagehand-deploy-agent/internal/metrics"
	"github.com/puppet-stagehand/stagehand-deploy-agent/internal/ops"
)

// shutdownGrace bounds how long a stopping server waits for requests to end
// and for deploy records to be written.
const shutdownGrace = 30 * time.Second

// opsShutdownGrace bounds how long the ops listener waits for a probe or a
// scrape in flight once everything else has stopped.
const opsShutdownGrace = 5 * time.Second

// Options assemble a Server.
type Options struct {
	// Addr is the listen address, "host:port" (host empty for all interfaces).
	Addr   string
	TLS    TLSConfig
	Runner *jobs.Runner
	// ProtectedEnvironments are never removed by DELETE (deploys.protected_environments, D-10).
	ProtectedEnvironments []string
	// Store is the runner's file store, closed by Serve as its last step so
	// the instance lock file does not outlive the process (FORK-WR-03). Nil
	// means nothing to close.
	Store  io.Closer
	Logger *slog.Logger
	// OpsListen is the plain-HTTP ops listener address (ops.listen). Empty
	// means no ops listener.
	OpsListen string
	// EnginePath (engine.command_path) and EnvironmentsDir (deploys.basedir)
	// are what /readyz checks the engine and the writable directory against.
	EnginePath      string
	EnvironmentsDir string
	// Metrics is the registry the middleware, the handshake and the ops
	// listener report to. Nil means a private one.
	Metrics *metrics.Registry
	// Build describes this process for build_info and /api/v1/info.
	Build BuildInfo
}

// BuildInfo is what main knows about the running process and the server does
// not: the version, the engine's own version line (empty when the engine did
// not answer) and the path of the configuration file that was loaded.
type BuildInfo struct {
	Version       string
	EngineVersion string
	ConfigSource  string
}

// Server is the mTLS deploy API.
type Server struct {
	srv    *http.Server
	runner *jobs.Runner
	store  io.Closer
	log    *slog.Logger

	// ops is the unauthenticated probe listener on opsAddr; nil when none is
	// configured. checks is its readiness state (D-05, D-07).
	ops     *http.Server
	opsAddr string
	checks  *ops.Checks
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
	reg := o.Metrics
	if reg == nil {
		reg = metrics.New(versionOrDev(o.Build.Version), o.Runner.Engine(), time.Now)
	}
	tlsCfg := o.TLS
	if tlsCfg.RefusalCounter == nil {
		tlsCfg.RefusalCounter = reg.TLSRefusal
	}
	certNotAfter, err := ops.CertNotAfter(o.TLS.Certificate)
	if err != nil {
		return nil, fmt.Errorf("server.tls.certificate: %w", err)
	}
	// The CRL set does not exist until the TLS server is built, and the mux is
	// needed to build it, so the info handler reads the CRL through a variable
	// filled in just after.
	var crl *crlSet
	info := NewInfoHandler(InfoOptions{
		Version:         versionOrDev(o.Build.Version),
		EngineName:      o.Runner.Engine(),
		EngineVersion:   o.Build.EngineVersion,
		EnvironmentsDir: o.EnvironmentsDir,
		CertNotAfter:    certNotAfter,
		CRLNextUpdate: func() time.Time {
			if crl == nil {
				return time.Time{}
			}
			return crl.NextUpdate()
		},
		ConfigSource: o.Build.ConfigSource,
	})
	mux := NewMux(NewDeployHandlers(o.Runner, log), NewEnvironmentHandlers(o.Runner, o.ProtectedEnvironments, log), info)
	srv, crl, err := newMTLSServer(o.Addr, tlsCfg, Wrap(mux, log, reg), log)
	if err != nil {
		return nil, err
	}
	s := &Server{srv: srv, runner: o.Runner, store: o.Store, log: log.With("component", "server")}
	s.checks = &ops.Checks{EnginePath: o.EnginePath, EnvironmentsDir: o.EnvironmentsDir, CertificatePath: o.TLS.Certificate}
	if crl != nil { // a nil *crlSet in the interface would not be a nil source
		s.checks.CRL = crl
	}
	certPath := o.TLS.Certificate
	var crlSource func() (time.Time, bool)
	if crl != nil {
		crlSource = func() (time.Time, bool) { n := crl.NextUpdate(); return n, !n.IsZero() }
	}
	reg.SetReadySource(s.checks.Ready)
	reg.SetExpirySources(func() (time.Time, bool) {
		t, err := ops.CertNotAfter(certPath)
		return t, err == nil
	}, crlSource)
	if o.OpsListen != "" {
		s.opsAddr = o.OpsListen
		s.ops = &http.Server{Handler: ops.NewMux(s.checks, reg.Handler()), ReadHeaderTimeout: 5 * time.Second}
	}
	return s, nil
}

func versionOrDev(v string) string {
	if v == "" {
		return "dev"
	}
	return v
}

// OptionsFromConfig builds the store and the runner for cfg, in the order the
// fork used: open the FileStore, fail the jobs a dead process left unfinished
// (before any request is accepted, so no client polls one forever), then the
// runner with its cross-process lock at <state_dir>/deploy.lock. ctx is the
// runner's base context: cancelling it stops running engines and fails queued
// jobs.
func OptionsFromConfig(ctx context.Context, cfg *config.Config, log *slog.Logger) (Options, error) {
	return OptionsFromConfigBuild(ctx, cfg, log, BuildInfo{})
}

// OptionsFromConfigBuild is OptionsFromConfig for a process that knows its
// version, its engine's version and where its configuration came from. It
// also builds the metrics registry and hands the runner the observer that
// feeds it.
func OptionsFromConfigBuild(ctx context.Context, cfg *config.Config, log *slog.Logger, build BuildInfo) (Options, error) {
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
	rep, err := store.Reconcile()
	if err != nil {
		store.Close()
		return Options{}, fmt.Errorf("reconciling deploy records: %w", err)
	}
	log.Info("deploy store reconciled", "component", "server", "interrupted", rep.Interrupted,
		"stale_locks_removed", rep.StaleLocks, "temp_files_removed", rep.TempFiles, "records_quarantined", rep.Quarantined)
	if rep.Interrupted > 0 {
		log.Warn("deploys interrupted by a restart were marked failed", "component", "server", "count", rep.Interrupted)
	}
	reg := metrics.New(versionOrDev(build.Version), cfg.Engine.Name, time.Now)
	runner, err := jobs.NewRunner(jobs.Options{
		Observer:    reg,
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
		TLS:    TLSConfig{Certificate: t.Certificate, Key: t.Key, ClientCA: t.ClientCA, CRL: t.CRL, AllowedCertnames: t.AllowedCertnames, AllowedFingerprints: t.AllowedFingerprints, RefusalCounter: reg.TLSRefusal},
		Runner: runner,
		Store:  store,
		Logger: log,

		ProtectedEnvironments: cfg.Deploys.ProtectedEnvironments,
		OpsListen:             cfg.Ops.Listen,
		EnginePath:            cfg.Engine.CommandPath,
		EnvironmentsDir:       cfg.Deploys.Basedir,
		Metrics:               reg,
		Build:                 build,
	}, nil
}

// Handler returns the server's mux, for tests that serve it themselves.
func (s *Server) Handler() http.Handler { return s.srv.Handler }

// HTTPServer returns the underlying http.Server (tests serve it on a listener
// of their own).
func (s *Server) HTTPServer() *http.Server { return s.srv }

// Serve listens and serves until ctx is cancelled, then shuts down in an order
// that leaves every admitted deploy on record (PD-18, FORK-WR-01) and keeps
// the ops listener answering until the last of it is done (PD-36):
//
//  1. /readyz starts to answer 503 (draining), and the HTTP server starts to
//     shut down in the background: listeners close at once, requests in
//     flight (a wait:true deploy among them) carry on;
//  2. the runner shuts down: it refuses new jobs, cancels its context, which
//     stops a running engine (its whole process group) and fails queued jobs,
//     and waits until every admitted job, wait:true included, has written its
//     final record;
//  3. Serve waits for the HTTP shutdown, so each answer has been written;
//  4. the ops listener stops, so a probe sees 503 for the whole drain;
//  5. the store is closed, which removes the instance lock file (FORK-WR-03).
//
// Under systemd TimeoutStopSec=60 this fits: steps 1 to 3 are bounded by
// shutdownGrace together, step 4 by opsShutdownGrace.
//
// The ops address is bound first: if it cannot be, Serve returns that error
// and the mTLS listener is never opened.
func (s *Server) Serve(ctx context.Context) error {
	var first error
	keep := func(err error) {
		if first == nil && err != nil {
			first = err
		}
	}
	if err := s.startOps(); err != nil {
		keep(err)
		keep(s.runner.Shutdown(shutdownGrace))
		keep(s.closeStore())
		return first
	}
	errc := make(chan error, 1)
	go func() { errc <- s.srv.ListenAndServeTLS("", "") }()
	select {
	case err := <-errc: // the listener failed (address in use, ...)
		keep(err)
		keep(s.runner.Shutdown(shutdownGrace))
		keep(s.stopOps())
		keep(s.closeStore())
		return first
	case <-ctx.Done():
	}
	s.log.Info("shutting down")
	s.checks.SetDraining()
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
	keep(s.stopOps())
	keep(s.closeStore())
	return first
}

// startOps binds the ops listener and serves it in the background. A bind
// beyond loopback is allowed but warned about once: the listener has no
// authentication (D-05, T-57.6.2-33).
func (s *Server) startOps() error {
	if s.ops == nil {
		return nil
	}
	ln, err := net.Listen("tcp", s.opsAddr)
	if err != nil {
		return fmt.Errorf("ops listener on %s: %w", s.opsAddr, err)
	}
	if !ops.Loopback(s.opsAddr) {
		s.log.Warn("the ops listener is not on a loopback address: /healthz, /readyz and /metrics are unauthenticated; firewall it or set ops.listen to 127.0.0.1",
			"addr", s.opsAddr)
	}
	s.log.Info("ops listening", "addr", ln.Addr().String())
	go func() {
		if err := s.ops.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("ops listener stopped", "error", err.Error())
		}
	}()
	return nil
}

func (s *Server) stopOps() error {
	if s.ops == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), opsShutdownGrace)
	defer cancel()
	if err := s.ops.Shutdown(ctx); err != nil {
		_ = s.ops.Close()
		s.log.Error("ops listener shutdown", "error", err.Error())
		return err
	}
	return nil
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
