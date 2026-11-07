package serve

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/cmd"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/spf13/cobra"
)

var (
	syncHooks bool

	// Command is the serve command.
	Command = &cobra.Command{
		Use:                "serve",
		Short:              "Start the server",
		Args:               cobra.NoArgs,
		PersistentPreRunE:  cmd.InitBackendContext,
		PersistentPostRunE: cmd.CloseDBContext,
		RunE: cmd.CloseDBOnError(func(c *cobra.Command, _ []string) error {
			ctx := c.Context()
			// Use the config the database and backend were set up with,
			// so hooks and directories land under the same data path.
			cfg := config.FromContext(ctx)
			if !cfg.Exist() {
				// Write the defaults, not cfg, so environment overrides
				// are not saved to the file.
				def := config.DefaultConfig()
				def.DataPath = cfg.DataPath
				if err := def.WriteConfig(); err != nil {
					return fmt.Errorf("write config file: %w", err)
				}
			}

			// Create custom hooks directory if it doesn't exist
			customHooksPath := filepath.Join(cfg.DataPath, "hooks")
			if _, err := os.Stat(customHooksPath); err != nil && os.IsNotExist(err) {
				os.MkdirAll(customHooksPath, os.ModePerm) //nolint: errcheck
				// Generate update hook example without executable permissions
				hookPath := filepath.Join(customHooksPath, "update.sample")
				//nolint: gosec
				if err := os.WriteFile(hookPath, []byte(updateHookExample), 0o744); err != nil {
					return fmt.Errorf("failed to generate update hook example: %w", err)
				}
			}

			// Create log directory if it doesn't exist
			logPath := filepath.Join(cfg.DataPath, "log")
			if _, err := os.Stat(logPath); err != nil && os.IsNotExist(err) {
				os.MkdirAll(logPath, os.ModePerm) //nolint: errcheck
			}

			db := db.FromContext(ctx)
			if err := migrate.Migrate(ctx, db); err != nil {
				return fmt.Errorf("migration error: %w", err)
			}
			// Reconcile initial_admin_keys on every start, not only on the
			// first migration, so keys added to the config later become
			// admin keys. "soft admin migrate" does not do this.
			if err := migrate.SyncAdminKeys(ctx, db); err != nil {
				return fmt.Errorf("sync initial admin keys: %w", err)
			}

			s, err := NewServer(ctx)
			if err != nil {
				return fmt.Errorf("start server: %w", err)
			}

			if syncHooks {
				be := backend.FromContext(ctx)
				if err := cmd.InitializeHooks(ctx, cfg, be); err != nil {
					_ = s.Close()
					return fmt.Errorf("initialize hooks: %w", err)
				}
			}

			lch := make(chan error, 1)
			done := make(chan os.Signal, 1)
			testStop := make(chan struct{})

			signal.Notify(done, os.Interrupt, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

			// This endpoint is added for testing purposes
			// It allows us to stop the server from the test suite.
			// This is needed since Windows doesn't support signals.
			if testRun, _ := strconv.ParseBool(os.Getenv("SOFT_SERVE_TESTRUN")); testRun {
				h := s.HTTPServer.Server.Handler
				s.HTTPServer.Server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/__stop" && r.Method == http.MethodHead {
						close(testStop)
						return
					}
					h.ServeHTTP(w, r)
				})
			}

			go func() {
				lch <- s.Start()
			}()

			// serverErr is the error the server stopped with. It is returned
			// only after the shutdown, which runs either way.
			var serverErr error
			for {
				select {
				case err := <-lch:
					if err != nil {
						serverErr = fmt.Errorf("server error: %w", err)
					}
					// Server exited; shut down
					goto shutdown
				case sig := <-done:
					if sig == syscall.SIGHUP {
						s.logger.Info("received SIGHUP signal, reloading TLS certificates if enabled")
						if err := s.ReloadCertificates(); err != nil {
							s.logger.Error("failed to reload TLS certificates", "err", err)
						}
						continue
					}
					// Real signal received; shut down
					goto shutdown
				case <-testStop:
					// Test hook triggered; shut down
					goto shutdown
				}
			}

		shutdown:
			// Waiting for webhook deliveries can take up to a minute; a
			// second interrupt or terminate signal stops that wait.
			abort, stopWaiting := abortOnSignal(done, s.logger)
			defer stopWaiting()
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			return errors.Join(serverErr, s.Shutdown(ctx, abort))
		}),
	}
)

// abortOnSignal returns a context that is cancelled when sigs delivers an
// interrupt or terminate signal. SIGHUP keeps its reload meaning and is
// ignored here. Call the returned function to stop watching.
func abortOnSignal(sigs <-chan os.Signal, logger *log.Logger) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			select {
			case sig := <-sigs:
				if sig == syscall.SIGHUP {
					continue
				}
				logger.Warn("received second signal, not waiting for webhook deliveries", "signal", sig)
				cancel()
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	return ctx, cancel
}

func init() {
	Command.Flags().BoolVarP(&syncHooks, "sync-hooks", "", false, "synchronize hooks for all repositories before running the server")
}

const updateHookExample = `#!/bin/sh
#
# An example hook script to echo information about the push
# and send it to the client.
#
# To enable this hook, rename this file to "update" and make it executable.

refname="$1"
oldrev="$2"
newrev="$3"

# Safety check
if [ -z "$GIT_DIR" ]; then
        echo "Don't run this script from the command line." >&2
        echo " (if you want, you could supply GIT_DIR then run" >&2
        echo "  $0 <ref> <oldrev> <newrev>)" >&2
        exit 1
fi

if [ -z "$refname" -o -z "$oldrev" -o -z "$newrev" ]; then
        echo "usage: $0 <ref> <oldrev> <newrev>" >&2
        exit 1
fi

# Check types
# if $newrev is 0000...0000, it's a commit to delete a ref.
zero=$(git hash-object --stdin </dev/null | tr '[0-9a-f]' '0')
if [ "$newrev" = "$zero" ]; then
        newrev_type=delete
else
        newrev_type=$(git cat-file -t $newrev)
fi

echo "Hi from Soft Serve Plus update hook!"
echo
echo "Repository: $SOFT_SERVE_REPO_NAME"
echo "RefName: $refname"
echo "Change Type: $newrev_type"
echo "Old SHA1: $oldrev"
echo "New SHA1: $newrev"

exit 0
`
