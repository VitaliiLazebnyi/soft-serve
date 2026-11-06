package test

import (
	"testing"

	"github.com/lib/pq"
)

func TestWithDatabase(t *testing.T) {
	for _, dsn := range []string{
		"postgres://postgres@localhost:5432/postgres?sslmode=disable",
		"postgresql://postgres:secret@localhost/other",
		"POSTGRES://postgres@localhost/postgres",
		"PostgreSQL://postgres@localhost/postgres?sslmode=disable",
		"postgres://postgres@localhost/postgres?dbname=zzz&sslmode=disable",
		"postgres://postgres@localhost?dbname=zzz",
		"host=localhost user=postgres dbname=postgres sslmode=disable",
		"host=localhost user=postgres sslmode=disable ",
	} {
		got, err := withDatabase(dsn, "softserve_test_1")
		if err != nil {
			t.Fatalf("withDatabase(%q) => %v", dsn, err)
		}
		cfg, err := pq.NewConfig(got)
		if err != nil {
			t.Fatalf("withDatabase(%q) = %q: %v", dsn, got, err)
		}
		if cfg.Database != "softserve_test_1" || cfg.Host != "localhost" || cfg.User != "postgres" {
			t.Errorf("withDatabase(%q) = %q: database %q, host %q, user %q", dsn, got, cfg.Database, cfg.Host, cfg.User)
		}
	}
}
