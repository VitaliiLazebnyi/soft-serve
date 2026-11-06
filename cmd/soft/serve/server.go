package serve

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"

	"charm.land/log/v2"

	"charm.land/ssh"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/cron"
	"github.com/charmbracelet/soft-serve/pkg/daemon"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/jobs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	sshsrv "github.com/charmbracelet/soft-serve/pkg/ssh"
	"github.com/charmbracelet/soft-serve/pkg/stats"
	"github.com/charmbracelet/soft-serve/pkg/utils"
	"github.com/charmbracelet/soft-serve/pkg/web"
	"golang.org/x/sync/errgroup"
)

// Server is the Soft Serve server.
type Server struct {
	SSHServer   *sshsrv.SSHServer
	GitDaemon   *daemon.GitDaemon
	HTTPServer  *web.HTTPServer
	StatsServer *stats.StatsServer
	CertLoader  *CertReloader
	Cron        *cron.Scheduler
	Config      *config.Config
	Backend     *backend.Backend
	DB          *db.DB

	logger *log.Logger
	ctx    context.Context
}

// NewServer returns a new *Server configured to serve Soft Serve. The SSH
// server key-pair will be created if none exists.
// It expects a context with *backend.Backend, *db.DB, *log.Logger, and
// *config.Config attached.
func NewServer(ctx context.Context) (*Server, error) {
	var err error
	cfg := config.FromContext(ctx)
	be := backend.FromContext(ctx)
	db := db.FromContext(ctx)
	logger := log.FromContext(ctx).WithPrefix("server")
	srv := &Server{
		Config:  cfg,
		Backend: be,
		DB:      db,
		logger:  log.FromContext(ctx).WithPrefix("server"),
		ctx:     ctx,
	}

	// Add cron jobs.
	sched := cron.NewScheduler(ctx)
	for n, j := range jobs.List() {
		id, err := sched.AddFunc(j.Runner.Spec(ctx), j.Runner.Func(ctx))
		if err != nil {
			logger.Warn("error adding cron job", "job", n, "err", err)
		}

		j.ID = id
	}

	srv.Cron = sched

	ensureDefaultRepo(ctx, cfg, be, logger)

	srv.SSHServer, err = sshsrv.NewSSHServer(ctx)
	if err != nil {
		return nil, fmt.Errorf("create ssh server: %w", err)
	}

	srv.GitDaemon, err = daemon.NewGitDaemon(ctx)
	if err != nil {
		return nil, fmt.Errorf("create git daemon: %w", err)
	}

	srv.HTTPServer, err = web.NewHTTPServer(ctx)
	if err != nil {
		return nil, fmt.Errorf("create http server: %w", err)
	}

	srv.StatsServer, err = stats.NewStatsServer(ctx)
	if err != nil {
		return nil, fmt.Errorf("create stats server: %w", err)
	}

	if cfg.HTTP.TLSKeyPath != "" && cfg.HTTP.TLSCertPath != "" {
		srv.CertLoader, err = NewCertReloader(cfg.HTTP.TLSCertPath, cfg.HTTP.TLSKeyPath, logger)
		if err != nil {
			return nil, fmt.Errorf("create cert reloader: %w", err)
		}

		srv.HTTPServer.SetTLSConfig(&tls.Config{
			GetCertificate: srv.CertLoader.GetCertificateFunc(),
		})
	}

	warnIfAnonAdminAccess(ctx, be, logger)

	// The backend decides this on its own; surface it once at startup so the
	// operator does not learn about it from the first failed import.
	if reason := be.ImportsDisabledReason(); reason != "" {
		logger.Error("imports and mirror syncs are disabled", "reason", reason)
	}

	return srv, nil
}

// warnIfAnonAdminAccess logs a loud warning if the server's effective,
// post-override settings grant unauthenticated (keyless) connections admin
// access. This checks effective runtime state via the backend, not just the
// new config-override fields, since the same risk exists whether the
// dangerous combination came from config or was set via `ssh soft settings`
// on a previous run.
func warnIfAnonAdminAccess(ctx context.Context, be *backend.Backend, logger *log.Logger) {
	if !be.AllowKeyless(ctx) || be.AnonAccess(ctx) < access.AdminAccess {
		return
	}

	logger.Warn("################################################################")
	logger.Warn("# WARNING: anonymous keyless connections have ADMIN access.    #")
	logger.Warn("# Anyone who can reach this server has full control, no auth.  #")
	logger.Warn("# This is intended for local/dev use only. Do not expose this  #")
	logger.Warn("# server to an untrusted network.                              #")
	logger.Warn("################################################################")
}

// ensureDefaultRepo creates the repo named by cfg.DefaultRepo if it does not
// already exist. It never fails startup: it logs invalid names and creation
// errors, then returns.
func ensureDefaultRepo(ctx context.Context, cfg *config.Config, be *backend.Backend, logger *log.Logger) {
	if cfg.DefaultRepo == "" {
		return
	}

	name := utils.SanitizeRepo(cfg.DefaultRepo)
	if err := utils.ValidateRepo(name); err != nil {
		logger.Warn("invalid default_repo, skipping", "name", cfg.DefaultRepo, "err", err)
		return
	}

	if _, err := be.Repository(ctx, name); err == nil {
		return
	} else if !errors.Is(err, proto.ErrRepoNotFound) {
		logger.Warn("failed to look up default repo", "name", name, "err", err)
		return
	}

	// The migration always inserts a user at ID 1. The repos table requires
	// a non-null owner, so we attribute the repo to that account. This is the
	// server's own bootstrap, not a user request, so it is not subject to
	// admin_only_create even if that account is no longer an admin.
	owner, err := be.UserByID(ctx, 1)
	if err != nil {
		logger.Warn("failed to look up default repo owner, skipping", "name", name, "err", err)
		return
	}

	switch _, err := be.CreateSystemRepository(ctx, name, owner, proto.RepositoryOptions{}); {
	case errors.Is(err, proto.ErrRepoExist):
		logger.Warn("default repo directory exists without a database row, skipping", "name", name)
	case err != nil:
		logger.Warn("failed to create default repo", "name", name, "err", err)
	default:
		logger.Info("created default repo", "name", name)
	}
}

// ReloadCertificates reloads the TLS certificates for the HTTP server.
func (s *Server) ReloadCertificates() error {
	if s.CertLoader == nil {
		return nil
	}
	return s.CertLoader.Reload()
}

// Start binds the listener of every enabled server and then serves them all
// until they stop.
//
// All listeners are bound before any server starts serving. If one cannot be
// bound, for example because its port is in use, Start closes the ones
// already bound and returns that error at once. Otherwise the process would
// keep running with only some of its servers up, and the failed server's
// error would surface only once all the others had stopped. It also means
// that once any of the servers accepts connections, all of them do.
func (s *Server) Start() error {
	type service struct {
		name   string
		addr   string
		serve  func(net.Listener) error
		closed error // the error serve returns after a shutdown
	}
	var services []service
	if s.Config.SSH.Enabled {
		services = append(services, service{"SSH server", s.Config.SSH.ListenAddr, s.SSHServer.Serve, ssh.ErrServerClosed})
	}
	if s.Config.Git.Enabled {
		services = append(services, service{"Git daemon", s.Config.Git.ListenAddr, s.GitDaemon.Serve, daemon.ErrServerClosed})
	}
	if s.Config.HTTP.Enabled {
		services = append(services, service{"HTTP server", s.Config.HTTP.ListenAddr, s.HTTPServer.Serve, http.ErrServerClosed})
	}
	if s.Config.Stats.Enabled {
		services = append(services, service{"Stats server", s.Config.Stats.ListenAddr, s.StatsServer.Serve, http.ErrServerClosed})
	}

	listeners := make([]net.Listener, 0, len(services))
	for _, svc := range services {
		l, err := (&net.ListenConfig{}).Listen(s.ctx, "tcp", svc.addr)
		if err != nil {
			for _, l := range listeners {
				_ = l.Close()
			}
			return fmt.Errorf("start %s: %w", svc.name, err)
		}
		listeners = append(listeners, l)
	}

	errg, _ := errgroup.WithContext(s.ctx)
	for i, svc := range services {
		l := listeners[i]
		errg.Go(func() error {
			s.logger.Print("Starting "+svc.name, "addr", svc.addr)
			if err := svc.serve(l); !errors.Is(err, svc.closed) {
				return err
			}
			return nil
		})
	}

	errg.Go(func() error {
		s.Cron.Start()
		return nil
	})
	return errg.Wait()
}

// Shutdown lets the server gracefully shutdown. The servers drain within ctx.
// The background webhook deliveries are then waited for within their own
// budget, backend.ShutdownDeliveryTimeout, or until abort is done, whichever
// comes first.
func (s *Server) Shutdown(ctx context.Context, abort context.Context) error {
	errg, ctx := errgroup.WithContext(ctx)
	errg.Go(func() error {
		return s.GitDaemon.Shutdown(ctx)
	})
	errg.Go(func() error {
		return s.HTTPServer.Shutdown(ctx)
	})
	errg.Go(func() error {
		return s.SSHServer.Shutdown(ctx)
	})
	errg.Go(func() error {
		return s.StatsServer.Shutdown(ctx)
	})
	errg.Go(func() error {
		for _, j := range jobs.List() {
			s.Cron.Remove(j.ID)
		}
		s.Cron.Stop()
		return nil
	})
	// defer s.DB.Close() // nolint: errcheck
	err := errg.Wait()

	// Always wait for the deliveries, even when a server failed to stop:
	// the queue is closed here so no new delivery starts, and the ones
	// already queued or running get their own budget, which is separate from
	// the caller's drain deadline for the HTTP and SSH servers.
	dctx, cancel := context.WithTimeout(abort, backend.ShutdownDeliveryTimeout)
	defer cancel()
	s.Backend.WaitForDeliveries(dctx)
	return err
}

// Close closes the SSH server.
func (s *Server) Close() error {
	var errg errgroup.Group
	errg.Go(s.GitDaemon.Close)
	errg.Go(s.HTTPServer.Close)
	errg.Go(s.SSHServer.Close)
	errg.Go(s.StatsServer.Close)
	errg.Go(func() error {
		s.Cron.Stop()
		return nil
	})
	// defer s.DB.Close() // nolint: errcheck
	return errg.Wait()
}
