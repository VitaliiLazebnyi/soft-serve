package backend

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
	"github.com/charmbracelet/soft-serve/pkg/webhook"
	"github.com/matryer/is"
)

// newPostgresWebhookEnv is newWebhookEnv on a disposable Postgres database on
// the server named by SOFT_SERVE_DB_DATA_SOURCE. The test is skipped unless
// SOFT_SERVE_DB_DRIVER is "postgres".
func newPostgresWebhookEnv(t *testing.T) whEnv {
	t.Helper()
	if os.Getenv("SOFT_SERVE_DB_DRIVER") != "postgres" {
		t.Skip("set SOFT_SERVE_DB_DRIVER=postgres to run")
	}
	is := is.New(t)
	ctx := context.Background()

	dsn := os.Getenv("SOFT_SERVE_DB_DATA_SOURCE")
	if dsn == "" {
		dsn = "postgres://postgres@localhost:5432/postgres?sslmode=disable"
	}
	admin, err := db.Open(ctx, "postgres", dsn)
	is.NoErr(err)
	t.Cleanup(func() { admin.Close() })                    //nolint:errcheck
	name := fmt.Sprintf("softserve_test_%d", rand.Int64()) //nolint:gosec
	_, err = admin.ExecContext(ctx, "CREATE DATABASE "+name)
	is.NoErr(err)
	t.Cleanup(func() { admin.ExecContext(context.Background(), "DROP DATABASE "+name) }) //nolint:errcheck

	// The data source is a URL or a key=value string, as in
	// pkg/db/internal/test, which this package cannot import.
	testDSN := strings.TrimSpace(dsn) + " dbname=" + name
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		is.NoErr(err)
		u.Path = "/" + name
		testDSN = u.String()
	}

	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.DB.Driver = "postgres"
	cfg.DB.DataSource = testDSN
	ctx = config.WithContext(ctx, cfg)
	dbx, err := db.Open(ctx, cfg.DB.Driver, cfg.DB.DataSource)
	is.NoErr(err)
	t.Cleanup(func() { dbx.Close() }) //nolint:errcheck

	is.NoErr(migrate.Migrate(ctx, dbx))
	dbstore := database.New(ctx, dbx)
	ctx = store.WithContext(ctx, dbstore)
	be := New(ctx, cfg, dbx, dbstore)
	ctx = db.WithContext(ctx, dbx)

	user, err := be.User(ctx, "admin")
	is.NoErr(err)
	ctx = proto.WithUserContext(ctx, user)
	repo, err := be.CreateRepository(ctx, "repo", user, proto.RepositoryOptions{})
	is.NoErr(err)
	return whEnv{ctx: ctx, be: be, cfg: cfg, admin: user, repo: repo, logs: &syncBuffer{}}
}

// Postgres runs transactions in READ COMMITTED, so without the repository
// lock a create could count the webhooks while another create, holding room
// for the last one, has not yet committed, and both would insert. Here one
// transaction locks the repository at 19 webhooks and holds on; a
// CreateWebhook started meanwhile must block on that transaction (checked
// with pg_blocking_pids, so the test fails if the lock is removed), and once
// it commits its 20th webhook, must fail.
func TestCreateWebhookCapHoldsUnderConcurrencyPostgres(t *testing.T) {
	is := is.New(t)
	e := newPostgresWebhookEnv(t)
	for i := range webhook.MaxWebhooksPerRepo - 1 {
		e.createHook(t, fmt.Sprintf("%s/%d", whURL, i), webhook.EventPush)
	}

	result := make(chan error, 1)
	is.NoErr(e.be.db.TransactionContext(e.ctx, func(tx *db.Tx) error {
		var holder int
		if err := tx.GetContext(e.ctx, &holder, `SELECT pg_backend_pid();`); err != nil {
			return err
		}
		n, err := e.be.store.LockAndCountWebhooks(e.ctx, tx, e.repo.ID())
		if err != nil {
			return err
		}
		is.Equal(n, webhook.MaxWebhooksPerRepo-1)

		go func() {
			result <- e.be.CreateWebhook(e.ctx, e.repo, whURL+"/racer",
				webhook.ContentTypeJSON, "", []webhook.Event{webhook.EventPush}, true)
		}()
		waitBlockedBy(t, e, holder, result)

		_, err = e.be.store.CreateWebhook(e.ctx, tx, e.repo.ID(), whURL+"/last", "", int(webhook.ContentTypeJSON), true)
		return err
	}))

	select {
	case err := <-result:
		is.True(errors.Is(err, webhook.ErrTooManyWebhooks))
	case <-time.After(10 * time.Second):
		t.Fatal("CreateWebhook still waiting after the lock was released")
	}
	hooks, err := e.be.ListWebhooks(e.ctx, e.repo)
	is.NoErr(err)
	is.Equal(len(hooks), webhook.MaxWebhooksPerRepo)
}

// waitBlockedBy waits until some backend is waiting for a lock held by the
// backend holder. It fails the test if result delivers first, meaning the
// operation it watches finished without waiting, or if nothing blocks within
// 10 seconds.
func waitBlockedBy(t *testing.T, e whEnv, holder int, result <-chan error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case err := <-result:
			t.Fatalf("CreateWebhook did not wait for the repository lock: %v", err)
		default:
		}
		var blocked bool
		if err := e.be.db.GetContext(e.ctx, &blocked,
			`SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid)));`, holder); err != nil {
			t.Fatalf("checking for blocked backends: %v", err)
		}
		if blocked {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("CreateWebhook did not block on the repository lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Many creates at once on Postgres: exactly the cap succeed, the rest get
// ErrTooManyWebhooks, and none fail with a deadlock or other error.
func TestCreateWebhookConcurrentPostgres(t *testing.T) {
	testConcurrentCreates(t, newPostgresWebhookEnv(t))
}
