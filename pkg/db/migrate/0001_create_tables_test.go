package migrate

import (
	"context"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/internal/test"
)

// openLegacySqlite opens a sqlite database holding a Soft Serve v0.6 "user"
// table with the given users, which the first migration imports.
func openLegacySqlite(t *testing.T, ctx context.Context, users ...ownerRow) *db.DB {
	t.Helper()
	dbx, err := test.OpenSqlite(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, dbx, `CREATE TABLE user (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		username TEXT NOT NULL UNIQUE,
		admin BOOLEAN NOT NULL,
		updated_at DATETIME NOT NULL
	)`)
	for _, u := range users {
		mustExec(t, dbx, `INSERT INTO user (username, admin, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)`,
			u.Username, u.Admin)
	}
	return dbx
}

func TestFirstMigrationSeedsAdminKeysToAdminNotUserOne(t *testing.T) {
	ctx := context.TODO()
	// alice gets id 1 and is not an admin.
	dbx := openLegacySqlite(t, ctx,
		ownerRow{Username: "alice", Admin: false},
		ownerRow{Username: "admin", Admin: true},
	)

	ak := newAuthorizedKey(t)
	cfg := config.DefaultConfig()
	cfg.InitialAdminKeys = []string{ak}
	if err := Migrate(config.WithContext(ctx, cfg), dbx); err != nil {
		t.Fatal(err)
	}

	if o, ok := ownerOf(t, dbx, ak); !ok || o.Username != "admin" || !o.Admin {
		t.Fatalf("key owner = %+v, %v; want admin", o, ok)
	}
}

func TestFirstMigrationCreatesAdminForKeysWhenLegacyUsersHaveNone(t *testing.T) {
	ctx := context.TODO()
	dbx := openLegacySqlite(t, ctx, ownerRow{Username: "alice", Admin: false})

	ak := newAuthorizedKey(t)
	cfg := config.DefaultConfig()
	cfg.InitialAdminKeys = []string{ak}
	if err := Migrate(config.WithContext(ctx, cfg), dbx); err != nil {
		t.Fatal(err)
	}

	if o, ok := ownerOf(t, dbx, ak); !ok || o.Username != "admin" || !o.Admin {
		t.Fatalf("key owner = %+v, %v; want admin", o, ok)
	}
}

func TestFirstMigrationDoesNotSeedAdminKeysToNonAdminNamedAdmin(t *testing.T) {
	ctx := context.TODO()
	// "admin" is a regular account and another admin exists.
	dbx := openLegacySqlite(t, ctx,
		ownerRow{Username: "admin", Admin: false},
		ownerRow{Username: "bob", Admin: true},
	)

	ak := newAuthorizedKey(t)
	cfg := config.DefaultConfig()
	cfg.InitialAdminKeys = []string{ak}
	if err := Migrate(config.WithContext(ctx, cfg), dbx); err != nil {
		t.Fatal(err)
	}

	if o, ok := ownerOf(t, dbx, ak); ok {
		t.Fatalf("key attached to %+v; want it left out", o)
	}
	if n := count(t, dbx, "SELECT COUNT(*) FROM users WHERE username = 'admin' AND admin = false"); n != 1 {
		t.Errorf("admin was promoted")
	}
}
