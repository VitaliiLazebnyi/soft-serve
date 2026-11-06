package test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/db"
)

// OpenSqlite opens a new temp SQLite database for testing.
// It removes the database file when the test is done using tb.Cleanup.
// If ctx is nil, context.TODO() is used.
func OpenSqlite(ctx context.Context, tb testing.TB) (*db.DB, error) {
	if ctx == nil {
		ctx = context.TODO()
	}
	dbpath := filepath.Join(tb.TempDir(), "test.db")
	dbx, err := db.Open(ctx, "sqlite", dbpath)
	if err != nil {
		return nil, err
	}
	tb.Cleanup(func() {
		if err := dbx.Close(); err != nil {
			tb.Error(err)
		}
	})
	return dbx, nil
}

// Open opens a new, empty test database. It uses Postgres when
// SOFT_SERVE_DB_DRIVER is "postgres", creating a disposable database on the
// server named by SOFT_SERVE_DB_DATA_SOURCE, and SQLite otherwise. The
// database is removed when the test is done.
func Open(ctx context.Context, tb testing.TB) (*db.DB, error) {
	if ctx == nil {
		ctx = context.TODO()
	}
	if os.Getenv("SOFT_SERVE_DB_DRIVER") != "postgres" {
		return OpenSqlite(ctx, tb)
	}
	return openPostgres(ctx, tb, os.Getenv("SOFT_SERVE_DB_DATA_SOURCE"))
}

func openPostgres(ctx context.Context, tb testing.TB, dsn string) (*db.DB, error) {
	if dsn == "" {
		dsn = "postgres://postgres@localhost:5432/postgres?sslmode=disable"
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return nil, err
	}

	admin, err := db.Open(ctx, "postgres", dsn)
	if err != nil {
		return nil, err
	}
	name := fmt.Sprintf("softserve_test_%d", rand.Int64()) //nolint:gosec
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		admin.Close() //nolint:errcheck
		return nil, err
	}

	u.Path = "/" + name
	dbx, err := db.Open(ctx, "postgres", u.String())
	if err != nil {
		admin.ExecContext(ctx, "DROP DATABASE "+name) //nolint:errcheck
		admin.Close()                                 //nolint:errcheck
		return nil, err
	}
	tb.Cleanup(func() {
		if err := dbx.Close(); err != nil {
			tb.Error(err)
		}
		if _, err := admin.ExecContext(context.TODO(), "DROP DATABASE "+name); err != nil {
			tb.Error(err)
		}
		if err := admin.Close(); err != nil {
			tb.Error(err)
		}
	})
	return dbx, nil
}
