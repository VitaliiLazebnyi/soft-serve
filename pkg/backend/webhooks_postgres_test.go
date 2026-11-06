package backend

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
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
	name := fmt.Sprintf("softserve_test_%d", rand.Int64()) //nolint:gosec
	_, err = admin.ExecContext(ctx, "CREATE DATABASE "+name)
	is.NoErr(err)
	u, err := url.Parse(dsn)
	is.NoErr(err)
	u.Path = "/" + name

	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.DB.Driver = "postgres"
	cfg.DB.DataSource = u.String()
	ctx = config.WithContext(ctx, cfg)
	dbx, err := db.Open(ctx, cfg.DB.Driver, cfg.DB.DataSource)
	is.NoErr(err)
	t.Cleanup(func() {
		dbx.Close()                                                    //nolint:errcheck
		admin.ExecContext(context.Background(), "DROP DATABASE "+name) //nolint:errcheck
		admin.Close()                                                  //nolint:errcheck
	})

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
// transaction counts 19 webhooks and holds on; a CreateWebhook started
// meanwhile must wait for it, and once it commits its 20th webhook, must
// fail.
func TestCreateWebhookCapHoldsUnderConcurrencyPostgres(t *testing.T) {
	is := is.New(t)
	e := newPostgresWebhookEnv(t)
	for i := range webhook.MaxWebhooksPerRepo - 1 {
		e.createHook(t, fmt.Sprintf("%s/%d", whURL, i), webhook.EventPush)
	}

	result := make(chan error, 1)
	is.NoErr(e.be.db.TransactionContext(e.ctx, func(tx *db.Tx) error {
		n, err := e.be.store.LockAndCountWebhooks(e.ctx, tx, e.repo.ID())
		if err != nil {
			return err
		}
		is.Equal(n, webhook.MaxWebhooksPerRepo-1)

		go func() {
			result <- e.be.CreateWebhook(e.ctx, e.repo, whURL+"/racer",
				webhook.ContentTypeJSON, "", []webhook.Event{webhook.EventPush}, true)
		}()
		select {
		case err := <-result:
			t.Fatalf("CreateWebhook did not wait for the repository lock: %v", err)
		case <-time.After(300 * time.Millisecond):
		}

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
