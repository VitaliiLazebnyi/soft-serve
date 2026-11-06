package serve

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/matryer/is"
)

// When the server fails to start, "soft serve" still shuts it down and
// closes the database before returning the error: cobra skips the command's
// PersistentPostRunE, which closes it otherwise.
func TestServeStartErrorShutsDown(t *testing.T) {
	is := is.New(t)

	taken, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	is.NoErr(err)
	defer taken.Close() //nolint: errcheck

	t.Setenv("SOFT_SERVE_DATA_PATH", t.TempDir())
	cfg := config.DefaultConfig()
	is.NoErr(cfg.Validate())
	cfg.SSH.ListenAddr = loopbackAddr()
	cfg.Git.ListenAddr = loopbackAddr()
	cfg.HTTP.ListenAddr = taken.Addr().String()
	cfg.Stats.ListenAddr = loopbackAddr()

	ctx := config.WithContext(context.Background(), cfg)
	ctx = log.WithContext(ctx, log.New(io.Discard))

	Command.SetArgs([]string{})
	Command.SetOut(io.Discard)
	Command.SetErr(io.Discard)
	err = Command.ExecuteContext(ctx)
	if err == nil || !strings.Contains(err.Error(), "HTTP server") {
		t.Fatalf("serve returned %v, want the HTTP server's listen error", err)
	}

	dbx := db.FromContext(Command.Context())
	is.True(dbx != nil)
	if err := dbx.PingContext(context.Background()); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("database ping returned %v, want it closed", err)
	}
}
