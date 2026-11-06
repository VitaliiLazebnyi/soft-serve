package serve

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
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

// When "soft serve" fails before the server starts, it still closes the
// database before returning the error: cobra skips the command's
// PersistentPostRunE, which closes it otherwise.
func TestServeEarlyErrorClosesDB(t *testing.T) {
	for _, tc := range []struct {
		name  string
		args  []string
		setup func(t *testing.T, cfg *config.Config)
		want  string
	}{
		{
			name: "parse config file",
			setup: func(t *testing.T, cfg *config.Config) {
				if err := os.WriteFile(cfg.ConfigPath(), []byte("ssh: ["), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "parse config file",
		},
		{
			name: "sync hooks",
			args: []string{"--sync-hooks"},
			setup: func(t *testing.T, cfg *config.Config) {
				// A repository whose directory is a file makes generating
				// its hooks fail.
				ctx := config.WithContext(context.Background(), cfg)
				ctx = log.WithContext(ctx, log.New(io.Discard))
				dbx, err := db.Open(ctx, cfg.DB.Driver, cfg.DB.DataSource)
				if err != nil {
					t.Fatal(err)
				}
				defer dbx.Close() //nolint: errcheck
				if err := migrate.Migrate(ctx, dbx); err != nil {
					t.Fatal(err)
				}
				if _, err := dbx.ExecContext(ctx, `INSERT INTO repos
					(name, project_name, description, private, mirror, hidden, user_id, updated_at)
					VALUES ('broken', '', '', false, false, false, 1, CURRENT_TIMESTAMP)`); err != nil {
					t.Fatal(err)
				}
				reposPath := filepath.Join(cfg.DataPath, "repos")
				if err := os.MkdirAll(reposPath, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(reposPath, "broken.git"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "initialize hooks",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SOFT_SERVE_DATA_PATH", t.TempDir())
			cfg := config.DefaultConfig()
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			cfg.SSH.ListenAddr = loopbackAddr()
			cfg.Git.ListenAddr = loopbackAddr()
			cfg.HTTP.ListenAddr = loopbackAddr()
			cfg.Stats.ListenAddr = loopbackAddr()
			tc.setup(t, cfg)

			ctx := config.WithContext(context.Background(), cfg)
			ctx = log.WithContext(ctx, log.New(io.Discard))

			t.Cleanup(func() { syncHooks = false })
			Command.SetArgs(append([]string{}, tc.args...))
			Command.SetOut(io.Discard)
			Command.SetErr(io.Discard)
			err := Command.ExecuteContext(ctx)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("serve returned %v, want a %q error", err, tc.want)
			}

			dbx := db.FromContext(Command.Context())
			if dbx == nil {
				t.Fatal("no database in the command context")
			}
			if err := dbx.PingContext(context.Background()); err == nil || !strings.Contains(err.Error(), "closed") {
				t.Fatalf("database ping returned %v, want it closed", err)
			}
		})
	}
}
