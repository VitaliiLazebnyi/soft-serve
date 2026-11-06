package admin

import (
	"context"
	"io"
	"strings"
	"testing"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	_ "modernc.org/sqlite" // sqlite driver
)

// When an admin command fails, it still closes the database: cobra skips
// the command's PersistentPostRunE, which closes it otherwise.
func TestAdminErrorClosesDB(t *testing.T) {
	t.Setenv("SOFT_SERVE_DATA_PATH", t.TempDir())
	cfg := config.DefaultConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	ctx := config.WithContext(context.Background(), cfg)
	ctx = log.WithContext(ctx, log.New(io.Discard))

	// A fresh database has no migrations to roll back.
	Command.SetArgs([]string{"rollback"})
	Command.SetOut(io.Discard)
	Command.SetErr(io.Discard)
	err := Command.ExecuteContext(ctx)
	if err == nil || !strings.Contains(err.Error(), "rollback") {
		t.Fatalf("admin rollback returned %v, want a rollback error", err)
	}

	dbx := db.FromContext(rollbackCmd.Context())
	if dbx == nil {
		t.Fatal("no database in the command context")
	}
	if err := dbx.PingContext(context.Background()); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("database ping returned %v, want it closed", err)
	}
}
