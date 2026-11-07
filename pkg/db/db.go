package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"  // postgres driver
	_ "modernc.org/sqlite" // sqlite driver
)

// DB is the interface for a Soft Serve database.
type DB struct {
	*sqlx.DB
	logger *log.Logger
}

// Open opens a database connection.
func Open(ctx context.Context, driverName string, dsn string) (*DB, error) {
	if driverName == "sqlite" {
		dsn = sqliteImmediateTx(dsn)
	}
	db, err := sqlx.ConnectContext(ctx, driverName, dsn)
	if err != nil {
		return nil, err
	}

	d := &DB{
		DB: db,
	}

	if config.IsVerbose() {
		logger := log.FromContext(ctx).WithPrefix("db")
		d.logger = logger
	}

	return d, nil
}

// sqliteImmediateTx makes write transactions on SQLite start with BEGIN
// IMMEDIATE unless the DSN already sets _txlock. A deferred transaction that
// reads before it writes cannot upgrade to a write lock while another
// connection holds one: SQLite returns SQLITE_BUSY at once instead of waiting
// for busy_timeout. Taking the write lock at BEGIN makes it wait instead.
// Read-only transactions are still started as deferred by the driver.
func sqliteImmediateTx(dsn string) string {
	i := strings.IndexByte(dsn, '?')
	if i == 0 {
		// The driver treats the whole string as a file name.
		return dsn
	}
	if i > 0 {
		if q, err := url.ParseQuery(dsn[i+1:]); err != nil || q.Has("_txlock") {
			return dsn
		}
		return dsn + "&_txlock=immediate"
	}
	return dsn + "?_txlock=immediate"
}

// SetPoolLimits applies the connection pool settings in c. A zero value
// leaves the database/sql default for that setting: no limit on open
// connections, two idle connections, and no limit on a connection's
// lifetime or idle time.
func (d *DB) SetPoolLimits(c config.DBConfig) {
	if c.MaxOpenConns > 0 {
		d.SetMaxOpenConns(c.MaxOpenConns)
	}
	if c.MaxIdleConns > 0 {
		d.SetMaxIdleConns(c.MaxIdleConns)
	}
	if c.ConnMaxLifetime > 0 {
		d.SetConnMaxLifetime(time.Duration(c.ConnMaxLifetime) * time.Second)
	}
	if c.ConnMaxIdleTime > 0 {
		d.SetConnMaxIdleTime(time.Duration(c.ConnMaxIdleTime) * time.Second)
	}
}

// Close implements db.DB.
func (d *DB) Close() error {
	return d.DB.Close()
}

// Tx is a database transaction.
type Tx struct {
	*sqlx.Tx
	logger *log.Logger
}

// Transaction implements db.DB.
func (d *DB) Transaction(fn func(tx *Tx) error) error {
	return d.TransactionContext(context.Background(), fn)
}

// TransactionContext implements db.DB.
func (d *DB) TransactionContext(ctx context.Context, fn func(tx *Tx) error) error {
	txx, err := d.DB.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}

	tx := &Tx{txx, d.logger}
	if err := fn(tx); err != nil {
		return rollback(tx, err)
	}

	if err := tx.Commit(); err != nil {
		if errors.Is(err, sql.ErrTxDone) {
			// this is ok because whoever did finish the tx should have also written the error already.
			return nil
		}
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	return nil
}

func rollback(tx *Tx, err error) error {
	if rerr := tx.Rollback(); rerr != nil {
		if errors.Is(rerr, sql.ErrTxDone) {
			return err
		}
		return fmt.Errorf("failed to rollback: %s: %w", err.Error(), rerr)
	}

	return err
}
