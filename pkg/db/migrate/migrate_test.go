package migrate

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
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
