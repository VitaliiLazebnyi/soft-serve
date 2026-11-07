package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/db"
)

// MigrateFunc is a function that executes a migration.
type MigrateFunc func(ctx context.Context, tx *db.Tx) error //nolint:revive

// Migration is a struct that contains the name of the migration and the
// function to execute it.
type Migration struct {
	Version  int64
	Name     string
	Migrate  MigrateFunc
	Rollback MigrateFunc
}

// Migrations is a database model to store migrations.
type Migrations struct {
	ID      int64  `db:"id"`
	Name    string `db:"name"`
	Version int64  `db:"version"`
}

func (Migrations) schema(driverName string) string {
	switch driverName {
	case "sqlite3", "sqlite":
		return `CREATE TABLE IF NOT EXISTS migrations (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				name TEXT NOT NULL,
				version INTEGER NOT NULL UNIQUE
			);
		`
	case "postgres":
		return `CREATE TABLE IF NOT EXISTS migrations (
			id SERIAL PRIMARY KEY,
			name TEXT NOT NULL,
			version INTEGER NOT NULL UNIQUE
		);
	`
	case "mysql":
		return `CREATE TABLE IF NOT EXISTS migrations (
			id INT NOT NULL AUTO_INCREMENT,
			name TEXT NOT NULL,
			version INT NOT NULL,
			UNIQUE (version),
			PRIMARY KEY (id)
		);
	`
	default:
		panic("unknown driver")
	}
}

// Migrate runs the migrations.
func Migrate(ctx context.Context, dbx *db.DB) error {
	logger := log.FromContext(ctx).WithPrefix("migrate")
	return dbx.TransactionContext(ctx, func(tx *db.Tx) error {
		ok, err := tableExists(tx, "migrations")
		if err != nil {
			return fmt.Errorf("look up migrations table: %w", err)
		}
		if !ok {
			if _, err := tx.ExecContext(ctx, Migrations{}.schema(tx.DriverName())); err != nil {
				return err
			}
		}

		var migrs Migrations
		if err := tx.Get(&migrs, tx.Rebind("SELECT * FROM migrations ORDER BY version DESC LIMIT 1")); err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}

		// Refuse to run against a database migrated by a newer binary: its
		// schema may not match what this binary expects.
		if n := len(migrations); n > 0 && migrs.Version > migrations[n-1].Version {
			return fmt.Errorf("database is at migration version %d, newer than the latest version %d known to this binary; upgrade soft-serve", migrs.Version, migrations[n-1].Version)
		}

		for _, m := range migrations {
			if m.Version <= migrs.Version {
				continue
			}

			logger.Infof("running migration %d. %s", m.Version, m.Name)
			if err := m.Migrate(ctx, tx); err != nil {
				return err
			}

			if _, err := tx.ExecContext(ctx, tx.Rebind("INSERT INTO migrations (name, version) VALUES (?, ?)"), m.Name, m.Version); err != nil {
				return err
			}
		}

		return nil
	})
}

// Rollback rolls back a migration.
func Rollback(ctx context.Context, dbx *db.DB) error {
	logger := log.FromContext(ctx).WithPrefix("migrate")
	return dbx.TransactionContext(ctx, func(tx *db.Tx) error {
		ok, err := tableExists(tx, "migrations")
		if err != nil {
			return fmt.Errorf("look up migrations table: %w", err)
		}
		if !ok {
			return fmt.Errorf("there are no migrations to rollback")
		}

		var migrs Migrations
		if err := tx.Get(&migrs, tx.Rebind("SELECT * FROM migrations ORDER BY version DESC LIMIT 1")); err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("get latest migration: %w", err)
			}
		}

		if migrs.Version == 0 {
			return fmt.Errorf("there are no migrations to rollback")
		}

		// Look the migration up by version rather than by position, and
		// refuse to roll back one this binary does not know.
		var m *Migration
		for i := range migrations {
			if migrations[i].Version == migrs.Version {
				m = &migrations[i]
				break
			}
		}
		if m == nil {
			if n := len(migrations); n > 0 && migrs.Version > migrations[n-1].Version {
				return fmt.Errorf("database is at migration version %d, newer than the latest version %d known to this binary; upgrade soft-serve", migrs.Version, migrations[n-1].Version)
			}
			return fmt.Errorf("database is at migration version %d, which is unknown to this binary", migrs.Version)
		}

		logger.Infof("rolling back migration %d. %s", m.Version, m.Name)
		if err := m.Rollback(ctx, tx); err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx, tx.Rebind("DELETE FROM migrations WHERE version = ?"), migrs.Version); err != nil {
			return err
		}

		return nil
	})
}

// tableExists reports whether the table exists, returning any error from the
// lookup other than the table not being found.
func tableExists(tx *db.Tx, tableName string) (bool, error) {
	var query string
	switch tx.DriverName() {
	case "sqlite3", "sqlite":
		query = "SELECT name FROM sqlite_master WHERE type='table' AND name=?"
	case "postgres":
		// Look in the schema unqualified CREATE TABLE statements write to,
		// which follows search_path and is not necessarily "public".
		query = "SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema() AND table_name = ?"
	case "mysql":
		// A MySQL schema is a database; look in the connection's current one.
		query = "SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?"
	}

	query = tx.Rebind(query)
	var name string
	if err := tx.Get(&name, query, tableName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
