package migrate

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/internal/test"
)

func TestMigrate(t *testing.T) {
	// XXX: we need a config.Config in the context for the migrations to run
	// properly. Some migrations depend on the config being present.
	ctx := config.WithContext(context.TODO(), config.DefaultConfig())
	dbx, err := test.OpenSqlite(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, dbx); err != nil {
		t.Errorf("Migrate() => %v, want nil error", err)
	}
}

// An older binary must refuse to run against a database that a newer binary
// has already migrated past the newest migration it knows.
func TestMigrateRefusesNewerDatabase(t *testing.T) {
	ctx := config.WithContext(context.TODO(), config.DefaultConfig())
	dbx, err := test.Open(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, dbx); err != nil {
		t.Fatalf("Migrate() => %v, want nil error", err)
	}

	// Simulate an older binary that does not know the newest migration.
	all := migrations
	t.Cleanup(func() { migrations = all })
	migrations = all[:len(all)-1]

	err = Migrate(ctx, dbx)
	if err == nil {
		t.Fatal("Migrate() on a newer database => nil error, want an error")
	}
	if !strings.Contains(err.Error(), "newer than the latest version") {
		t.Errorf("Migrate() on a newer database => %v, want a version mismatch error", err)
	}

	// The binary that knows every migration still starts.
	migrations = all
	if err := Migrate(ctx, dbx); err != nil {
		t.Errorf("Migrate() with all migrations => %v, want nil error", err)
	}
}

// Rolling back a database that a newer binary migrated past the newest
// migration this binary knows must say so, and must leave the database alone.
func TestRollbackRefusesNewerDatabase(t *testing.T) {
	ctx := config.WithContext(context.TODO(), config.DefaultConfig())
	dbx, err := test.Open(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, dbx); err != nil {
		t.Fatalf("Migrate() => %v, want nil error", err)
	}

	// Simulate an older binary that does not know the newest migration.
	all := migrations
	t.Cleanup(func() { migrations = all })
	migrations = all[:len(all)-1]

	err = Rollback(ctx, dbx)
	if err == nil {
		t.Fatal("Rollback() on a newer database => nil error, want an error")
	}
	if !strings.Contains(err.Error(), "newer than the latest version") {
		t.Errorf("Rollback() on a newer database => %v, want a version mismatch error", err)
	}

	var version int64
	if err := dbx.Get(&version, "SELECT MAX(version) FROM migrations"); err != nil {
		t.Fatal(err)
	}
	if want := all[len(all)-1].Version; version != want {
		t.Errorf("migration version after refused Rollback() = %d, want %d", version, want)
	}
}

// Rollback must pick the migration by its version, not by its position in
// the list, so a gap in version numbers rolls back the right migration.
func TestRollbackByVersion(t *testing.T) {
	ctx := config.WithContext(context.TODO(), config.DefaultConfig())
	dbx, err := test.Open(ctx, t)
	if err != nil {
		t.Fatal(err)
	}

	all := migrations
	t.Cleanup(func() { migrations = all })

	var rolledBack []string
	mk := func(version int64, name string) Migration {
		return Migration{
			Version: version,
			Name:    name,
			Migrate: func(context.Context, *db.Tx) error { return nil },
			Rollback: func(context.Context, *db.Tx) error {
				rolledBack = append(rolledBack, name)
				return nil
			},
		}
	}
	migrations = []Migration{mk(1, "one"), mk(3, "three")}

	if err := Migrate(ctx, dbx); err != nil {
		t.Fatalf("Migrate() => %v, want nil error", err)
	}
	if err := Rollback(ctx, dbx); err != nil {
		t.Fatalf("Rollback() => %v, want nil error", err)
	}
	if len(rolledBack) != 1 || rolledBack[0] != "three" {
		t.Errorf("Rollback() rolled back %v, want [three]", rolledBack)
	}

	// A recorded version this binary has no migration for is an error, not a
	// rollback of some other migration.
	if _, err := dbx.Exec(dbx.Rebind("INSERT INTO migrations (name, version) VALUES (?, ?)"), "two", 2); err != nil {
		t.Fatal(err)
	}
	rolledBack = nil
	if err := Rollback(ctx, dbx); err == nil || !strings.Contains(err.Error(), "unknown to this binary") {
		t.Errorf("Rollback() at unknown version 2 => %v, want an unknown version error", err)
	}
	if len(rolledBack) != 0 {
		t.Errorf("Rollback() at unknown version 2 rolled back %v, want nothing", rolledBack)
	}
}
