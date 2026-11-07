package serve

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"

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

	// mu guards listeners and closed. Start keeps the listeners it bound
	// so that Shutdown and Close can close them even if they run before the
	// servers have started serving on them.
	mu        sync.Mutex
	listeners []*serveListener
	closed    bool
}

// NewServer returns a new *Server configured to serve Soft Serve. The SSH
// server key-pair will be created if none exists.
// It expects a context with *backend.Backend, *db.DB, *log.Logger, and
// *config.Config attached.
func NewServer(ctx context.Context) (_ *Server, err error) {
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

	// If a later step fails, close what was already built rather than leave
	// it to the caller, which only gets the error.
	defer func() {
		if err != nil {
			closeFailedServer(srv)
		}
	}()

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

	switch _, err := be.CreateSystemRepository(ctx, name, owner, proto.RepositoryOptions{Private: cfg.DefaultPrivate}); {
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
//
// Likewise, if a server fails while serving, Start returns its error at once,
// without waiting for the others to stop. The caller should then shut the
// server down.
//
// Start returns nil without serving if Shutdown or Close has been called.
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

	listeners := make([]*serveListener, 0, len(services))
	closeAll := func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}
	for _, svc := range services {
		// An empty address would make net.Listen pick a random port on
		// every interface, where the servers used to fall back to their
		// library's default port. Neither is what an operator would expect
		// from a missing setting.
		if svc.addr == "" {
			closeAll()
			return fmt.Errorf("start %s: listen address is empty", svc.name)
		}
		l, err := (&net.ListenConfig{}).Listen(s.ctx, "tcp", svc.addr)
		if err != nil {
			closeAll()
			return fmt.Errorf("start %s: %w", svc.name, err)
		}
		listeners = append(listeners, newServeListener(l))
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		closeAll()
		return nil
	}
	s.listeners = listeners
	s.mu.Unlock()

	// Each server reports here when it stops. The first one to fail ends
	// Start at once, rather than once every other server has stopped too,
	// so that the process does not keep running with only some of its
	// servers up.
	errc := make(chan error, len(services))
	for i, svc := range services {
		l := listeners[i]
		go func() {
			defer l.markServing()
			s.logger.Print("Starting "+svc.name, "addr", l.Addr().String())
			err := svc.serve(l)
			if errors.Is(err, svc.closed) || errors.Is(err, net.ErrClosed) && s.isClosed() {
				// A server that was shut down before it started serving,
				// or before its own Shutdown or Close ran, reports the
				// listener closeListeners closed rather than its
				// ErrServerClosed.
				err = nil
			} else if err != nil {
				err = fmt.Errorf("%s: %w", svc.name, err)
			}
			errc <- err
		}()
	}

	s.Cron.Start()
	for range services {
		if err := <-errc; err != nil {
			s.logger.Error("server stopped", "err", err)
			return err
		}
	}
	return nil
}

// Shutdown lets the server gracefully shutdown. The servers drain within ctx.
// The background webhook deliveries are then waited for within their own
// budget, backend.ShutdownDeliveryTimeout, or until abort is done, whichever
// comes first.
func (s *Server) Shutdown(ctx context.Context, abort context.Context) error {
	s.markClosed()
	// Close the listeners first, and wait until every server serves on its
	// listener, which then fails at once, or has given up on it. A server's
	// Shutdown only closes the listeners it already serves on, and the SSH
	// server's then waits for every listener it serves on to be released, so
	// one that started serving just after its Shutdown ran held it until
	// the deadline. The SSH server and the Git daemon also count a listener
	// they start serving on with a WaitGroup their Shutdown waits on, which
	// must not start counting while it is being waited on.
	for _, l := range s.closeListeners() {
		select {
		case <-l.serving:
		case <-ctx.Done():
		}
	}
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

// closeFailedServer closes a server NewServer could not finish building.
// Tests replace it to see which server NewServer closed.
var closeFailedServer = func(s *Server) { _ = s.Close() }

// Close closes the servers. It skips the ones that were never built, so it
// also closes a server NewServer failed to finish.
func (s *Server) Close() error {
	s.markClosed()
	s.closeListeners()
	var errg errgroup.Group
	if s.GitDaemon != nil {
		errg.Go(s.GitDaemon.Close)
	}
	if s.HTTPServer != nil {
		errg.Go(s.HTTPServer.Close)
	}
	if s.SSHServer != nil {
		errg.Go(s.SSHServer.Close)
	}
	if s.StatsServer != nil {
		errg.Go(s.StatsServer.Close)
	}
	if s.Cron != nil {
		errg.Go(func() error {
			s.Cron.Stop()
			return nil
		})
	}
	// defer s.DB.Close() // nolint: errcheck
	return errg.Wait()
}

// markClosed makes a Start that has not yet stored its listeners close them
// and return instead of serving.
func (s *Server) markClosed() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

func (s *Server) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// closeListeners closes the listeners Start bound, and returns them. A server
// that was shut down before it started serving on its listener does not know
// about it, so would otherwise leave it open, and the SSH server would even go
// on to serve on it. It runs before the servers' own Shutdown or Close, which
// close the listeners they serve on again; serveListener makes that a no-op.
func (s *Server) closeListeners() []*serveListener {
	s.mu.Lock()
	listeners := s.listeners
	s.listeners = nil
	s.mu.Unlock()
	for _, l := range listeners {
		_ = l.Close()
	}
	return listeners
}

// serveListener is a listener Start bound. It closes the listener only once,
// and returns nil from every later Close: both closeListeners and the server
// serving on the listener close it, and the server would otherwise report the
// second Close as an error. Its serving channel is closed once the server
// first accepts on it, which every server does only after it has registered
// the listener, or once the server has returned without doing so.
type serveListener struct {
	net.Listener
	closeOnce   sync.Once
	servingOnce sync.Once
	serving     chan struct{}
}

func newServeListener(l net.Listener) *serveListener {
	return &serveListener{Listener: l, serving: make(chan struct{})}
}

func (l *serveListener) Accept() (net.Conn, error) {
	l.markServing()
	return l.Listener.Accept()
}

func (l *serveListener) Close() error {
	var err error
	l.closeOnce.Do(func() { err = l.Listener.Close() })
	return err
}

func (l *serveListener) markServing() {
	l.servingOnce.Do(func() { close(l.serving) })
}
