package cmd

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/spf13/cobra"
)

// CloseDBOnError closes the database when the wrapped RunE fails, since
// cobra skips PersistentPostRunE then, and leaves it open otherwise.
func TestCloseDBOnError(t *testing.T) {
	for _, tc := range []struct {
		name       string
		runErr     error
		wantClosed bool
	}{
		{name: "error", runErr: errors.New("boom"), wantClosed: true},
		{name: "success", runErr: nil, wantClosed: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			dbx, err := db.Open(ctx, "sqlite", filepath.Join(t.TempDir(), "test.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = dbx.Close() })

			c := &cobra.Command{}
			c.SetContext(db.WithContext(ctx, dbx))
			run := CloseDBOnError(func(*cobra.Command, []string) error { return tc.runErr })
			if err := run(c, nil); !errors.Is(err, tc.runErr) {
				t.Fatalf("run returned %v, want %v", err, tc.runErr)
			}

			err = dbx.PingContext(ctx)
			closed := err != nil && strings.Contains(err.Error(), "closed")
			if closed != tc.wantClosed {
				t.Fatalf("database ping returned %v, want closed=%v", err, tc.wantClosed)
			}
		})
	}
}
