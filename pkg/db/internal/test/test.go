package test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"path/filepath"
	"strings"
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
// server named by SOFT_SERVE_DB_DATA_SOURCE (a postgres:// URL or a key=value
// connection string), and SQLite otherwise. The
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
	admin, err := db.Open(ctx, "postgres", dsn)
	if err != nil {
		return nil, err
	}
	name := fmt.Sprintf("softserve_test_%d", rand.Int64()) //nolint:gosec
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		admin.Close() //nolint:errcheck
		return nil, err
	}

	testDSN, err := withDatabase(dsn, name)
	if err != nil {
		admin.ExecContext(ctx, "DROP DATABASE "+name) //nolint:errcheck
		admin.Close()                                 //nolint:errcheck
		return nil, err
	}
	dbx, err := db.Open(ctx, "postgres", testDSN)
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

// withDatabase returns dsn with its database set to name. dsn is either a
// URL (postgres://... or postgresql://...) or a key=value connection string
// such as "host=localhost user=postgres sslmode=disable". In the key=value
// form a later dbname overrides an earlier one, so it is appended.
func withDatabase(dsn, name string) (string, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", err
		}
		u.Path = "/" + name
		return u.String(), nil
	}
	return strings.TrimSpace(dsn) + " dbname=" + name, nil
}
