package webhook

import (
	"context"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
	"github.com/matryer/is"
	"golang.org/x/crypto/ssh"
)

// testEnv is a fresh, migrated SQLite database wired into a context the same
// way the server wires it, plus the rows the tests need.
type testEnv struct {
	ctx   context.Context
	dbx   *db.DB
	ds    store.Store
	cfg   *config.Config
	owner models.User
	repo  models.Repo
}

// newTestEnv builds a test environment. The owner user and a repository named
// "repo" owned by that user are created.
func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	is := is.New(t)

	dp := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.DataPath = dp
	cfg.DB.Driver = "sqlite"
	cfg.DB.DataSource = filepath.Join(dp, "test.db") + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	cfg.HTTP.PublicURL = "http://127.0.0.1:23232"
	cfg.SSH.PublicURL = "ssh://127.0.0.1:23231"
	cfg.Git.PublicURL = "git://127.0.0.1:9418"

	ctx := config.WithContext(context.Background(), cfg)
	dbx, err := db.Open(ctx, cfg.DB.Driver, cfg.DB.DataSource)
	is.NoErr(err)
	t.Cleanup(func() { dbx.Close() }) //nolint:errcheck

	is.NoErr(migrate.Migrate(ctx, dbx))
	ds := database.New(ctx, dbx)
	ctx = db.WithContext(ctx, dbx)
	ctx = store.WithContext(ctx, ds)

	is.NoErr(ds.CreateUser(ctx, dbx, "alice", false, nil))
	owner, err := ds.FindUserByUsername(ctx, dbx, "alice")
	is.NoErr(err)

	is.NoErr(ds.CreateRepo(ctx, dbx, "repo", owner.ID, "Project", "a description", false, false, false, false))
	repo, err := ds.GetRepoByName(ctx, dbx, "repo")
	is.NoErr(err)

	return &testEnv{ctx: ctx, dbx: dbx, ds: ds, cfg: cfg, owner: owner, repo: repo}
}

// newWebhook stores an active webhook on the test repository that receives the
// given events, and returns it as the models.Webhook the sender uses.
func (e *testEnv) newWebhook(t *testing.T, url, secret string, contentType ContentType, events ...Event) models.Webhook {
	t.Helper()
	is := is.New(t)
	ids := make([]int, len(events))
	for i, ev := range events {
		ids[i] = int(ev)
	}
	id, err := e.ds.CreateWebhook(e.ctx, e.dbx, e.repo.ID, url, secret, int(contentType), true)
	is.NoErr(err)
	is.NoErr(e.ds.CreateWebhookEvents(e.ctx, e.dbx, id, ids))
	w, err := e.ds.GetWebhookByID(e.ctx, e.dbx, e.repo.ID, id)
	is.NoErr(err)
	return w
}

// deliveries returns every recorded delivery for a webhook.
func (e *testEnv) deliveries(t *testing.T, webhookID int64) []models.WebhookDelivery {
	t.Helper()
	is := is.New(t)
	ds, err := e.ds.GetWebhookDeliveriesByWebhookID(e.ctx, e.dbx, webhookID)
	is.NoErr(err)
	return ds
}

// useHTTPClient replaces the SSRF-protected client for the duration of the
// test. The SSRF client refuses loopback targets, which httptest servers use.
// Only test code calls this; production always uses the protected client.
// It swaps a package variable, so tests that use it must not call t.Parallel.
func useHTTPClient(t *testing.T) {
	t.Helper()
	orig := secureHTTPClient
	secureHTTPClient = &http.Client{Timeout: 5 * time.Second}
	t.Cleanup(func() { secureHTTPClient = orig })
}

// fakeUser implements proto.User.
type fakeUser struct {
	id   int64
	name string
}

func (u fakeUser) ID() int64                   { return u.id }
func (u fakeUser) Username() string            { return u.name }
func (u fakeUser) IsAdmin() bool               { return false }
func (u fakeUser) PublicKeys() []ssh.PublicKey { return nil }
func (u fakeUser) Password() string            { return "" }

// fakeRepo implements proto.Repository. Open returns a real git repository at
// path, or openErr when set, so error paths can be driven.
type fakeRepo struct {
	id      int64
	userID  int64
	name    string
	path    string
	openErr error
}

func (r fakeRepo) ID() int64            { return r.id }
func (r fakeRepo) Name() string         { return r.name }
func (r fakeRepo) ProjectName() string  { return "Project" }
func (r fakeRepo) Description() string  { return "a description" }
func (r fakeRepo) IsPrivate() bool      { return false }
func (r fakeRepo) IsMirror() bool       { return false }
func (r fakeRepo) IsHidden() bool       { return false }
func (r fakeRepo) UserID() int64        { return r.userID }
func (r fakeRepo) CreatedAt() time.Time { return time.Unix(1700000000, 0) }
func (r fakeRepo) UpdatedAt() time.Time { return time.Unix(1700000100, 0) }

func (r fakeRepo) Open() (*git.Repository, error) {
	if r.openErr != nil {
		return nil, r.openErr
	}
	return git.Open(r.path)
}

var (
	_ proto.User       = fakeUser{}
	_ proto.Repository = fakeRepo{}
)

// gitRepo creates a bare git repository with the given files committed to
// main, and returns its path and the commit hash of the tip.
func gitRepo(t *testing.T) (string, string) {
	t.Helper()
	is := is.New(t)
	dir := filepath.Join(t.TempDir(), "repo.git")
	_, err := git.Init(dir, true)
	is.NoErr(err)
	work := t.TempDir()
	runGit(t, work, "init", "-q", "-b", "main")
	runGit(t, work, "-c", "user.name=Alice", "-c", "user.email=alice@example.com",
		"commit", "-q", "--allow-empty", "-m", "first commit\n\nbody of first")
	runGit(t, work, "-c", "user.name=Alice", "-c", "user.email=alice@example.com",
		"commit", "-q", "--allow-empty", "-m", "second commit")
	runGit(t, work, "push", "-q", dir, "main")
	runGit(t, dir, "symbolic-ref", "HEAD", "refs/heads/main")
	head := gitOutput(t, work, "rev-parse", "HEAD")
	return dir, head
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	_ = gitOutput(t, dir, args...)
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	is := is.New(t)
	cmd := exec.Command("git", args...) //nolint:noctx
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	is.NoErr(err)
	return strings.TrimSpace(string(out))
}
