package migrate

import (
	"context"

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
		return migrateUp(ctx, tx, webhookDeliverySeqVersion, webhookDeliverySeqName)
	},
	Rollback: func(ctx context.Context, tx *db.Tx) error {
		return migrateDown(ctx, tx, webhookDeliverySeqVersion, webhookDeliverySeqName)
	},
}
