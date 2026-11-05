package backend

import (
	charmlog "charm.land/log/v2"
	"context"
	"io"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
	"github.com/matryer/is"
	_ "modernc.org/sqlite"
)

// newTestBackend returns a Backend backed by a real, freshly migrated SQLite
// database, along with the *config.Config it was constructed with (so tests
// can mutate AnonAccess/AllowKeyless directly to simulate config overrides).
func newTestBackend(t *testing.T) (*Backend, *config.Config) {
	t.Helper()
	return newTestBackendWithLog(t, nil)
}

// newTestBackendWithLog is newTestBackend with the backend's logger writing to
// logs, so a test can assert on what the backend logged. A nil logs keeps the
// default logger.
func newTestBackendWithLog(t *testing.T, logs io.Writer) (*Backend, *config.Config) {
	t.Helper()
	is := is.New(t)
	ctx := context.Background()

	dp := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.DataPath = dp
	cfg.DB.Driver = "sqlite"
	cfg.DB.DataSource = dp + "/test.db?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	// Backend tests run with admin-only creation on; flag-off cases set it themselves.
	cfg.AdminOnlyCreate = true

	ctx = config.WithContext(ctx, cfg)
	dbx, err := db.Open(ctx, cfg.DB.Driver, cfg.DB.DataSource)
	is.NoErr(err)
	t.Cleanup(func() { dbx.Close() }) //nolint:errcheck

	is.NoErr(migrate.Migrate(ctx, dbx))
	dbstore := database.New(ctx, dbx)
	ctx = store.WithContext(ctx, dbstore)
	if logs != nil {
		ctx = charmlog.WithContext(ctx, charmlog.New(logs))
	}
	be := New(ctx, cfg, dbx, dbstore)

	return be, cfg
}

func TestAnonAccessConfigOverride(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := context.Background()

	// Seeded default from migration, no override set.
	is.Equal(be.AnonAccess(ctx), access.ReadOnlyAccess)

	// With no override, DB writes via SetAnonAccess are reflected.
	is.NoErr(be.SetAnonAccess(ctx, access.ReadWriteAccess))
	is.Equal(be.AnonAccess(ctx), access.ReadWriteAccess)

	// A config override takes precedence over whatever is in the DB.
	admin := access.AdminAccess
	cfg.AnonAccess = &admin
	is.Equal(be.AnonAccess(ctx), access.AdminAccess)

	// The DB value is unchanged underneath the override: once the override
	// is cleared, the last DB write is what's returned again.
	cfg.AnonAccess = nil
	is.Equal(be.AnonAccess(ctx), access.ReadWriteAccess)
}

func TestAllowKeylessConfigOverride(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := context.Background()

	// Seeded default from migration, no override set.
	is.Equal(be.AllowKeyless(ctx), true)

	// With no override, DB writes via SetAllowKeyless are reflected.
	is.NoErr(be.SetAllowKeyless(ctx, false))
	is.Equal(be.AllowKeyless(ctx), false)

	// A config override of true takes precedence over a DB value of false.
	allow := true
	cfg.AllowKeyless = &allow
	is.Equal(be.AllowKeyless(ctx), true)

	// A config override of false takes precedence over a DB value of true
	// too — this is a real tri-state override, not just a way to force
	// things open.
	is.NoErr(be.SetAllowKeyless(ctx, true))
	disallow := false
	cfg.AllowKeyless = &disallow
	is.Equal(be.AllowKeyless(ctx), false)

	// Clearing the override falls back to the DB value again.
	cfg.AllowKeyless = nil
	is.Equal(be.AllowKeyless(ctx), true)
}
