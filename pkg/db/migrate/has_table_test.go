package migrate

import (
	"context"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/internal/test"
)

// hasTable looks for a table in the schema that unqualified statements use.
// On Postgres that is the first schema on search_path, which need not be
// "public". The test runs on Postgres when SOFT_SERVE_DB_DRIVER is
// "postgres".
func TestHasTable(t *testing.T) {
	ctx := context.TODO()
	dbx, err := test.Open(ctx, t)
	if err != nil {
		t.Fatal(err)
	}

	if err := dbx.TransactionContext(ctx, func(tx *db.Tx) error {
		if dbx.DriverName() == "postgres" {
			for _, q := range []string{
				`CREATE TABLE public.only_in_public (id INTEGER)`,
				`CREATE SCHEMA app`,
				`SET LOCAL search_path TO app`,
			} {
				if _, err := tx.ExecContext(ctx, q); err != nil {
					t.Fatalf("%s: %v", q, err)
				}
			}
			if hasTable(tx, "only_in_public") {
				t.Errorf("hasTable(only_in_public) = true with search_path=app, want false")
			}
		}

		if hasTable(tx, "widgets") {
			t.Errorf("hasTable(widgets) before CREATE = true, want false")
		}
		if _, err := tx.ExecContext(ctx, `CREATE TABLE widgets (id INTEGER)`); err != nil {
			t.Fatal(err)
		}
		if !hasTable(tx, "widgets") {
			t.Errorf("hasTable(widgets) after CREATE = false, want true")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
