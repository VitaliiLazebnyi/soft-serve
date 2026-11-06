package serve

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
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

// Start returns at once when one server's port is taken, and leaves none of
// the others listening, instead of running on with only some servers up.
func TestStartFailsWhenAListenerCannotBind(t *testing.T) {
	is := is.New(t)
	ctx, cfg, be, dbx := setupTestBackendWithDB(t)
	ctx = backend.WithContext(ctx, be)
	ctx = db.WithContext(ctx, dbx)
	ctx = log.WithContext(ctx, log.New(io.Discard))

	taken, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	is.NoErr(err)
	defer taken.Close() //nolint: errcheck

	// The SSH server binds before the HTTP server, whose port is taken.
	free, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	is.NoErr(err)
	sshAddr := free.Addr().String()
	is.NoErr(free.Close())

	cfg.SSH.ListenAddr = sshAddr
	cfg.Git.ListenAddr = "127.0.0.1:0"
	cfg.HTTP.ListenAddr = taken.Addr().String()
	cfg.Stats.ListenAddr = "127.0.0.1:0"

	s, err := NewServer(ctx)
	is.NoErr(err)

	errc := make(chan error, 1)
	go func() { errc <- s.Start() }()
	select {
	case err := <-errc:
		if err == nil || !strings.Contains(err.Error(), "HTTP server") {
			t.Fatalf("Start returned %v, want the HTTP server's listen error", err)
		}
	case <-time.After(10 * time.Second):
		_ = s.Close()
		t.Fatal("Start did not return although the HTTP server could not listen")
	}

	conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", sshAddr)
	if err == nil {
		_ = conn.Close()
		t.Fatalf("SSH server still listening on %s after Start failed", sshAddr)
	}
}
