package db

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenUnknownDriver(t *testing.T) {
	_, err := Open(context.TODO(), "invalid", "")
	if err == nil {
		t.Error("Open(invalid) => nil, want error")
	}
	if !strings.Contains(err.Error(), "unknown driver") {
		t.Errorf("Open(invalid) => %v, want error containing 'unknown driver'", err)
	}
}

func TestSqliteImmediateTx(t *testing.T) {
	for dsn, want := range map[string]string{
		"soft-serve.db": "soft-serve.db?_txlock=immediate",
		"soft-serve.db?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)": "soft-serve.db?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_txlock=immediate",
		"file:soft-serve.db?mode=rwc":                                      "file:soft-serve.db?mode=rwc&_txlock=immediate",
		"soft-serve.db?_txlock=deferred":                                   "soft-serve.db?_txlock=deferred",
		"soft-serve.db?_pragma=foreign_keys(1)&_txlock=exclusive":          "soft-serve.db?_pragma=foreign_keys(1)&_txlock=exclusive",
	} {
		if got := sqliteImmediateTx(dsn); got != want {
			t.Errorf("sqliteImmediateTx(%q) = %q, want %q", dsn, got, want)
		}
	}
}

// TestSqliteReadThenWriteWaitsForWriter checks that a transaction that reads
// before it writes waits (busy_timeout) for another connection's write
// transaction to commit instead of failing at once with "database is locked".
func TestSqliteReadThenWriteWaitsForWriter(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "test.db") + "?_pragma=busy_timeout(5000)"
	d, err := Open(ctx, "sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close() //nolint:errcheck
	if _, err := d.ExecContext(ctx, "CREATE TABLE t (n INTEGER)"); err != nil {
		t.Fatal(err)
	}

	written := make(chan struct{})
	release := make(chan struct{})
	writerErr := make(chan error, 1)
	go func() {
		writerErr <- d.TransactionContext(ctx, func(tx *Tx) error {
			if _, err := tx.ExecContext(ctx, "INSERT INTO t (n) VALUES (1)"); err != nil {
				return err
			}
			close(written)
			<-release
			return nil
		})
	}()

	select {
	case <-written:
	case err := <-writerErr:
		t.Fatalf("writer: %v", err)
	}

	readerErr := make(chan error, 1)
	go func() {
		readerErr <- d.TransactionContext(ctx, func(tx *Tx) error {
			var n int
			if err := tx.GetContext(ctx, &n, "SELECT COUNT(*) FROM t"); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, "INSERT INTO t (n) VALUES (?)", n+1)
			return err
		})
	}()

	// Give the second transaction time to hit the lock before releasing.
	time.Sleep(200 * time.Millisecond)
	close(release)

	if err := <-writerErr; err != nil {
		t.Fatalf("writer: %v", err)
	}
	if err := <-readerErr; err != nil {
		t.Fatalf("read-then-write transaction: %v", err)
	}

	var rows []int
	if err := d.SelectContext(ctx, &rows, "SELECT n FROM t ORDER BY n"); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0] != 1 || rows[1] != 2 {
		t.Errorf("rows = %v, want [1 2]", rows)
	}
}
