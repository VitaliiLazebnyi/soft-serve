package serve

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/cron"
	"github.com/charmbracelet/soft-serve/pkg/daemon"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	sshsrv "github.com/charmbracelet/soft-serve/pkg/ssh"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
	"github.com/charmbracelet/soft-serve/pkg/test"
	"github.com/matryer/is"
	_ "modernc.org/sqlite" // sqlite driver
)

func setupTestBackend(tb testing.TB) (context.Context, *config.Config, *backend.Backend) {
	tb.Helper()
	ctx, cfg, be, _ := setupTestBackendWithDB(tb)
	return ctx, cfg, be
}

func setupTestBackendWithDB(tb testing.TB) (context.Context, *config.Config, *backend.Backend, *db.DB) {
	tb.Helper()
	is := is.New(tb)

	cfg := config.DefaultConfig()
	cfg.DataPath = tb.TempDir()
	is.NoErr(cfg.Validate())

	ctx := context.Background()
	ctx = config.WithContext(ctx, cfg)

	is.NoErr(os.MkdirAll(cfg.DataPath, os.ModePerm))
	dbx, err := db.Open(ctx, cfg.DB.Driver, cfg.DB.DataSource)
	is.NoErr(err)
	tb.Cleanup(func() { dbx.Close() }) //nolint: errcheck

	is.NoErr(migrate.Migrate(ctx, dbx))

	dbstore := database.New(ctx, dbx)
	ctx = store.WithContext(ctx, dbstore)
	be := backend.New(ctx, cfg, dbx, dbstore)

	return ctx, cfg, be, dbx
}

func TestWarnIfAnonAdminAccess(t *testing.T) {
	cases := []struct {
		name         string
		allowKeyless bool
		anonAccess   access.AccessLevel
		wantWarning  bool
	}{
		{"defaults: keyless disabled, read-only anon", false, access.ReadOnlyAccess, false},
		{"keyless allowed but anon access below admin", true, access.ReadWriteAccess, false},
		{"admin anon access but keyless disallowed", false, access.AdminAccess, false},
		{"keyless allowed with admin anon access: the dangerous combo", true, access.AdminAccess, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			is := is.New(t)
			ctx, cfg, be := setupTestBackend(t)

			allow := c.allowKeyless
			cfg.AllowKeyless = &allow
			cfg.AnonAccess = &c.anonAccess

			var buf bytes.Buffer
			logger := log.New(&buf)

			warnIfAnonAdminAccess(ctx, be, logger)

			gotWarning := strings.Contains(buf.String(), "WARNING")
			is.Equal(gotWarning, c.wantWarning)
		})
	}
}

func TestEnsureDefaultRepoDisabled(t *testing.T) {
	is := is.New(t)
	ctx, cfg, be := setupTestBackend(t)

	cfg.DefaultRepo = ""
	ensureDefaultRepo(ctx, cfg, be, log.New(os.Stderr))

	repos, err := be.Repositories(ctx)
	is.NoErr(err)
	is.Equal(len(repos), 0)
}

// With admin-only creation on, the boot-time default repository is still
// created: it is attributed to the database admin (user 1), who may create.
func TestEnsureDefaultRepoCreatesWhenAdminOnly(t *testing.T) {
	is := is.New(t)
	ctx, cfg, be := setupTestBackend(t)

	cfg.AdminOnlyCreate = true
	cfg.DefaultRepo = "gitops"
	ensureDefaultRepo(ctx, cfg, be, log.New(os.Stderr))

	r, err := be.Repository(ctx, "gitops")
	is.NoErr(err)
	is.Equal(r.Name(), "gitops")
}

// The default repository is the server's own bootstrap, so admin-only
// creation does not apply to it even when user 1 is no longer an admin.
func TestEnsureDefaultRepoIgnoresAdminOnlyForDemotedOwner(t *testing.T) {
	is := is.New(t)
	ctx, cfg, be := setupTestBackend(t)

	owner, err := be.UserByID(ctx, 1)
	is.NoErr(err)
	// A second admin, so user 1 can be demoted.
	_, err = be.CreateUser(ctx, "root2", proto.UserOptions{Admin: true})
	is.NoErr(err)
	is.NoErr(be.SetAdmin(ctx, owner.Username(), false))

	cfg.AdminOnlyCreate = true
	cfg.DefaultRepo = "gitops"
	ensureDefaultRepo(ctx, cfg, be, log.New(os.Stderr))

	r, err := be.Repository(ctx, "gitops")
	is.NoErr(err)
	is.Equal(r.UserID(), owner.ID())
}

func TestEnsureDefaultRepoCreatesWhenMissing(t *testing.T) {
	is := is.New(t)
	ctx, cfg, be := setupTestBackend(t)

	cfg.DefaultRepo = "gitops"
	ensureDefaultRepo(ctx, cfg, be, log.New(os.Stderr))

	r, err := be.Repository(ctx, "gitops")
	is.NoErr(err)
	is.Equal(r.Name(), "gitops")
	is.Equal(r.IsPrivate(), false)
}

func TestEnsureDefaultRepoIsIdempotent(t *testing.T) {
	is := is.New(t)
	ctx, cfg, be := setupTestBackend(t)

	cfg.DefaultRepo = "gitops"
	logger := log.New(os.Stderr)

	ensureDefaultRepo(ctx, cfg, be, logger)
	r, err := be.Repository(ctx, "gitops")
	is.NoErr(err)
	createdAt := r.CreatedAt()

	// Restart-safety: a second boot against the same data must not error or
	// touch the existing repository.
	ensureDefaultRepo(ctx, cfg, be, logger)

	r, err = be.Repository(ctx, "gitops")
	is.NoErr(err)
	is.Equal(r.CreatedAt(), createdAt)

	repos, err := be.Repositories(ctx)
	is.NoErr(err)
	is.Equal(len(repos), 1)
}

func TestEnsureDefaultRepoInvalidNameSkipped(t *testing.T) {
	is := is.New(t)
	ctx, cfg, be := setupTestBackend(t)

	cfg.DefaultRepo = "../not valid!"
	ensureDefaultRepo(ctx, cfg, be, log.New(os.Stderr))

	repos, err := be.Repositories(ctx)
	is.NoErr(err)
	is.Equal(len(repos), 0)
}

func TestEnsureDefaultRepoLooksUpExisting(t *testing.T) {
	is := is.New(t)
	ctx, cfg, be := setupTestBackend(t)

	owner, err := be.UserByID(ctx, 1)
	is.NoErr(err)
	_, err = be.CreateRepository(ctx, "gitops", owner, proto.RepositoryOptions{
		Private: true,
	})
	is.NoErr(err)

	cfg.DefaultRepo = "gitops"
	ensureDefaultRepo(ctx, cfg, be, log.New(os.Stderr))

	r, err := be.Repository(ctx, "gitops")
	is.NoErr(err)
	is.Equal(r.IsPrivate(), true)
}

func TestEnsureDefaultRepoOwnerLookupFailsSkipped(t *testing.T) {
	is := is.New(t)
	ctx, cfg, be := setupTestBackend(t)

	// Delete the built-in admin account (ID 1) so UserByID cannot find it.
	// ensureDefaultRepo must log and return, not panic.
	is.NoErr(be.DeleteUser(ctx, "admin"))

	cfg.DefaultRepo = "gitops"
	ensureDefaultRepo(ctx, cfg, be, log.New(os.Stderr))

	repos, err := be.Repositories(ctx)
	is.NoErr(err)
	is.Equal(len(repos), 0)
}

func TestEnsureDefaultRepoLookupErrorSkipped(t *testing.T) {
	is := is.New(t)
	ctx, cfg, be, dbx := setupTestBackendWithDB(t)

	// Create the repo directory on disk so Repository() passes os.Stat,
	// then close the DB so the lookup behind it fails with a non-not-found error.
	repoDir := filepath.Join(cfg.DataPath, "repos", "gitops.git")
	is.NoErr(os.MkdirAll(repoDir, os.ModePerm))
	is.NoErr(dbx.Close())

	cfg.DefaultRepo = "gitops"
	ensureDefaultRepo(ctx, cfg, be, log.New(os.Stderr))

	_, err := be.UserByID(ctx, 1)
	is.True(err != nil) // db is closed, sanity check we broke it correctly
}

func TestEnsureDefaultRepoCreateErrorSkipped(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires enforceable directory permissions as a non-root user")
	}

	is := is.New(t)
	ctx, cfg, be := setupTestBackend(t)

	// Make the "repos" directory read-only so CreateRepository's git.Init
	// fails for a reason other than the repo already existing.
	reposDir := filepath.Join(cfg.DataPath, "repos")
	is.NoErr(os.MkdirAll(reposDir, os.ModePerm))
	is.NoErr(os.Chmod(reposDir, 0o555))
	t.Cleanup(func() { os.Chmod(reposDir, 0o755) }) //nolint: errcheck

	cfg.DefaultRepo = "gitops"
	ensureDefaultRepo(ctx, cfg, be, log.New(os.Stderr))

	repos, err := be.Repositories(ctx)
	is.NoErr(err)
	is.Equal(len(repos), 0)
}

// During shutdown, a second interrupt or terminate signal ends the wait for
// webhook deliveries. SIGHUP keeps meaning "reload" and does not.
func TestAbortOnSignal(t *testing.T) {
	is := is.New(t)
	var logs bytes.Buffer
	sigs := make(chan os.Signal, 1)
	abort, stop := abortOnSignal(sigs, log.New(&logs))
	defer stop()

	sigs <- syscall.SIGHUP
	select {
	case <-abort.Done():
		t.Fatal("SIGHUP aborted the wait")
	case <-time.After(50 * time.Millisecond):
	}

	sigs <- syscall.SIGTERM
	select {
	case <-abort.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("SIGTERM did not abort the wait")
	}
	is.True(strings.Contains(logs.String(), "not waiting for webhook deliveries"))
}

// newStartTestServer returns a server whose four listeners are on the given
// loopback addresses. It is closed when the test ends.
func newStartTestServer(t *testing.T, sshAddr, gitAddr, httpAddr, statsAddr string) *Server {
	t.Helper()
	is := is.New(t)
	ctx, cfg, be, dbx := setupTestBackendWithDB(t)
	ctx = backend.WithContext(ctx, be)
	ctx = db.WithContext(ctx, dbx)
	ctx = log.WithContext(ctx, log.New(io.Discard))

	cfg.SSH.ListenAddr = sshAddr
	cfg.Git.ListenAddr = gitAddr
	cfg.HTTP.ListenAddr = httpAddr
	cfg.Stats.ListenAddr = statsAddr

	s, err := NewServer(ctx)
	is.NoErr(err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func loopbackAddr() string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(test.RandomPort()))
}

// startAsync runs s.Start and returns a channel that receives its result.
func startAsync(s *Server) <-chan error {
	errc := make(chan error, 1)
	go func() { errc <- s.Start() }()
	return errc
}

func waitStart(t *testing.T, errc <-chan error, why string) error {
	t.Helper()
	select {
	case err := <-errc:
		return err
	case <-time.After(10 * time.Second):
		t.Fatalf("Start did not return %s", why)
		return nil
	}
}

// requireBindable fails the test unless addr can be bound, that is, unless
// nothing listens on it any more.
func requireBindable(t *testing.T, addr string) {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", addr)
	if err != nil {
		t.Fatalf("%s is still in use: %v", addr, err)
	}
	_ = l.Close()
}

// Start returns at once when one server's port is taken, and leaves none of
// the others listening, instead of running on with only some servers up.
func TestStartFailsWhenAListenerCannotBind(t *testing.T) {
	taken, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close() //nolint: errcheck

	// The SSH and Git servers bind before the HTTP server, whose port is
	// taken.
	sshAddr, gitAddr := loopbackAddr(), loopbackAddr()
	s := newStartTestServer(t, sshAddr, gitAddr, taken.Addr().String(), loopbackAddr())

	err = waitStart(t, startAsync(s), "although the HTTP server could not listen")
	if err == nil || !strings.Contains(err.Error(), "HTTP server") {
		t.Fatalf("Start returned %v, want the HTTP server's listen error", err)
	}
	requireBindable(t, sshAddr)
	requireBindable(t, gitAddr)
}

// Start serves on every enabled listener, and Shutdown makes it return and
// releases all of them.
func TestStartServesAllListenersUntilShutdown(t *testing.T) {
	addrs := []string{loopbackAddr(), loopbackAddr(), loopbackAddr(), loopbackAddr()}
	s := newStartTestServer(t, addrs[0], addrs[1], addrs[2], addrs[3])
	errc := startAsync(s)

	for _, addr := range addrs {
		deadline := time.Now().Add(10 * time.Second)
		for {
			conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(context.Background(), "tcp", addr)
			if err == nil {
				_ = conn.Close()
				break
			}
			select {
			case err := <-errc:
				t.Fatalf("Start returned %v before %s accepted connections", err, addr)
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("nothing accepts connections on %s: %v", addr, err)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx, ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := waitStart(t, errc, "after Shutdown"); err != nil {
		t.Fatalf("Start returned %v after Shutdown, want nil", err)
	}
	for _, addr := range addrs {
		requireBindable(t, addr)
	}
}

// A Close that comes before the servers start serving, here even before
// Start, still stops them. Otherwise the SSH server went on to serve after
// Close and Start never returned, and the Git daemon's listener stayed open.
func TestStartAfterCloseDoesNotServe(t *testing.T) {
	addrs := []string{loopbackAddr(), loopbackAddr(), loopbackAddr(), loopbackAddr()}
	s := newStartTestServer(t, addrs[0], addrs[1], addrs[2], addrs[3])
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := waitStart(t, startAsync(s), "after Close"); err != nil {
		t.Fatalf("Start returned %v after Close, want nil", err)
	}
	for _, addr := range addrs {
		requireBindable(t, addr)
	}
}

// An enabled server with no listen address is an error rather than a random
// port on every interface.
func TestStartRejectsEmptyListenAddr(t *testing.T) {
	sshAddr := loopbackAddr()
	s := newStartTestServer(t, sshAddr, "", loopbackAddr(), loopbackAddr())
	err := waitStart(t, startAsync(s), "with an empty Git listen address")
	if err == nil || !strings.Contains(err.Error(), "Git daemon: listen address is empty") {
		t.Fatalf("Start returned %v, want an empty listen address error", err)
	}
	requireBindable(t, sshAddr)
}

// A Shutdown that comes right after Start bound its listeners, while the
// servers are only starting to serve on them, still stops them all within
// its deadline. The SSH server used to wait for a listener it started serving
// on after its Shutdown ran, and that listener was closed only after every
// server's Shutdown returned, so Shutdown ran into its deadline.
func TestShutdownRightAfterStart(t *testing.T) {
	for i := range 50 {
		addrs := []string{loopbackAddr(), loopbackAddr(), loopbackAddr(), loopbackAddr()}
		s := newStartTestServer(t, addrs[0], addrs[1], addrs[2], addrs[3])
		errc := startAsync(s)

		deadline := time.Now().Add(10 * time.Second)
		for {
			s.mu.Lock()
			bound := s.listeners != nil
			s.mu.Unlock()
			if bound {
				break
			}
			select {
			case err := <-errc:
				t.Fatalf("run %d: Start returned %v before binding its listeners", i, err)
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("run %d: Start did not bind its listeners", i)
			}
			runtime.Gosched()
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := s.Shutdown(ctx, ctx)
		cancel()
		if err != nil {
			t.Fatalf("run %d: Shutdown: %v", i, err)
		}
		if err := waitStart(t, errc, "after Shutdown"); err != nil {
			t.Fatalf("run %d: Start returned %v after Shutdown, want nil", i, err)
		}
		for _, addr := range addrs {
			requireBindable(t, addr)
		}
	}
}

// A server that fails while serving makes Start return its error at once.
// Start used to hold the error until every other server had stopped too, so
// the process kept running with only some of its servers up.
func TestStartReturnsWhenAServerFails(t *testing.T) {
	addrs := []string{loopbackAddr(), loopbackAddr(), loopbackAddr(), loopbackAddr()}
	s := newStartTestServer(t, addrs[0], addrs[1], addrs[2], addrs[3])
	errc := startAsync(s)

	var httpL *serveListener
	deadline := time.Now().Add(10 * time.Second)
	for {
		s.mu.Lock()
		if s.listeners != nil {
			httpL = s.listeners[2] // SSH, Git, HTTP, Stats
		}
		s.mu.Unlock()
		if httpL != nil {
			break
		}
		select {
		case err := <-errc:
			t.Fatalf("Start returned %v before binding its listeners", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("Start did not bind its listeners")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-httpL.serving:
	case <-time.After(10 * time.Second):
		t.Fatal("the HTTP server did not start serving")
	}

	// Close the HTTP server's listener behind its back, so that its Serve
	// fails while the other servers keep running.
	_ = httpL.Listener.Close()

	err := waitStart(t, errc, "although the HTTP server failed")
	if err == nil || !strings.Contains(err.Error(), "HTTP server") {
		t.Fatalf("Start returned %v, want the HTTP server's error", err)
	}
}

// requireServerClosed fails the test unless serve, run on a listener that is
// already closed, reports want, which each server returns only once it has
// been closed itself. A server that is still open reports the listener's
// error instead.
func requireServerClosed(t *testing.T, name string, serve func(net.Listener) error, want error) {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_ = l.Close()
	if err := serve(l); !errors.Is(err, want) {
		t.Errorf("%s: Serve returned %v, want %v: it was not closed", name, err, want)
	}
}

// When NewServer fails, here because the TLS certificate cannot be loaded, it
// closes the servers it already built instead of dropping them.
func TestNewServerClosesWhatItBuiltOnError(t *testing.T) {
	ctx, cfg, be, dbx := setupTestBackendWithDB(t)
	ctx = backend.WithContext(ctx, be)
	ctx = db.WithContext(ctx, dbx)
	ctx = log.WithContext(ctx, log.New(io.Discard))
	cfg.HTTP.TLSCertPath = filepath.Join(t.TempDir(), "missing.crt")
	cfg.HTTP.TLSKeyPath = filepath.Join(t.TempDir(), "missing.key")

	var closed []*Server
	orig := closeFailedServer
	closeFailedServer = func(s *Server) {
		closed = append(closed, s)
		orig(s)
	}
	t.Cleanup(func() { closeFailedServer = orig })

	s, err := NewServer(ctx)
	if err == nil || !strings.Contains(err.Error(), "create cert reloader") {
		t.Fatalf("NewServer returned %v, want a cert reloader error", err)
	}
	if s != nil {
		t.Fatalf("NewServer returned a server along with its error")
	}
	if len(closed) != 1 {
		t.Fatalf("NewServer closed %d servers, want 1", len(closed))
	}
	// The SSH server is left out: it can serve again after Close, so serving
	// on a closed listener does not tell whether it was closed.
	srv := closed[0]
	requireServerClosed(t, "Git daemon", srv.GitDaemon.Serve, daemon.ErrServerClosed)
	requireServerClosed(t, "HTTP server", srv.HTTPServer.Serve, http.ErrServerClosed)
	requireServerClosed(t, "Stats server", srv.StatsServer.Serve, http.ErrServerClosed)
}

// Close skips the servers that were never built, so that it can close a
// server NewServer failed to finish, here one that got no further than its
// Git daemon.
func TestCloseSkipsServersNotBuilt(t *testing.T) {
	ctx, _, be, dbx := setupTestBackendWithDB(t)
	ctx = backend.WithContext(ctx, be)
	ctx = db.WithContext(ctx, dbx)
	ctx = log.WithContext(ctx, log.New(io.Discard))

	sshSrv, err := sshsrv.NewSSHServer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	gitDaemon, err := daemon.NewGitDaemon(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Cron: cron.NewScheduler(ctx), SSHServer: sshSrv, GitDaemon: gitDaemon}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	requireServerClosed(t, "Git daemon", gitDaemon.Serve, daemon.ErrServerClosed)
}
