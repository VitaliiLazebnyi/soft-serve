package migrate

import (
	"context"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/db"
)

const (
	webhookDeliverySeqName    = "webhook_delivery_seq"
	webhookDeliverySeqVersion = 4
)

// Give webhook deliveries a monotonic sequence number. created_at has
// one-second resolution and delivery IDs are random UUIDs, so deliveries
// recorded within the same second had no stable order. Existing rows are
// numbered by created_at, then by insertion order (SQLite rowid) or ID
// (Postgres).
var webhookDeliverySeq = Migration{
	Name:    webhookDeliverySeqName,
	Version: webhookDeliverySeqVersion,
	Migrate: func(ctx context.Context, tx *db.Tx) error {
		if err := migrateUp(ctx, tx, webhookDeliverySeqVersion, webhookDeliverySeqName); err != nil {
			return err
		}
		suggestSqliteVacuum(ctx, tx)
		return nil
	},
	Rollback: func(ctx context.Context, tx *db.Tx) error {
		return migrateDown(ctx, tx, webhookDeliverySeqVersion, webhookDeliverySeqName)
	},
}

// suggestSqliteVacuum logs a hint to run VACUUM after the SQLite table
// rebuild copied existing deliveries. SQLite keeps the pages of the dropped
// table in the file as free pages, so the file stays about one deliveries
// table larger. VACUUM cannot run inside the migration transaction, so it is
// left to the operator. Nothing is logged when there were no deliveries to
// copy, such as on a fresh install.
func suggestSqliteVacuum(ctx context.Context, tx *db.Tx) {
	if d := tx.DriverName(); d != "sqlite" && d != "sqlite3" {
		return
	}
	var hasRows bool
	if err := tx.GetContext(ctx, &hasRows, `SELECT EXISTS (SELECT 1 FROM webhook_deliveries)`); err != nil || !hasRows {
		return
	}
	log.FromContext(ctx).WithPrefix("migrate").Info(
		"webhook deliveries were copied into a rebuilt table; the space of the old table stays in the database file until it is vacuumed. " +
			"To reclaim it, stop the server and run VACUUM on the database file, for example: sqlite3 soft-serve.db VACUUM")
}
