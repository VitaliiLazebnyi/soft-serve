// Package dbtest opens disposable databases for tests outside pkg/db, which
// cannot import pkg/db/internal/test.
package dbtest

import (
	"context"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/internal/test"
)

// Open opens a new, empty test database: Postgres when SOFT_SERVE_DB_DRIVER
// is "postgres" (on the server named by SOFT_SERVE_DB_DATA_SOURCE), SQLite
// otherwise. The database is removed when the test is done. If ctx is nil,
// context.TODO() is used.
func Open(ctx context.Context, tb testing.TB) (*db.DB, error) {
	return test.Open(ctx, tb)
}
