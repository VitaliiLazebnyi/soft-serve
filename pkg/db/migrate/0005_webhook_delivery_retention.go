package migrate

import (
	"context"

	"github.com/charmbracelet/soft-serve/pkg/db"
)

const (
	webhookDeliveryRetentionName    = "webhook_delivery_retention"
	webhookDeliveryRetentionVersion = 5
)

// Trim each webhook to its 100 most recent deliveries, the number recording a
// delivery keeps from now on (maxDeliveries in pkg/webhook). Deliveries had
// no retention before, so a table can hold far more. Trimming it here, once,
// keeps that large delete out of recording a delivery, which runs under a
// short timeout. Rolling back cannot restore the deleted rows, so it does
// nothing.
var webhookDeliveryRetention = Migration{
	Name:    webhookDeliveryRetentionName,
	Version: webhookDeliveryRetentionVersion,
	Migrate: func(ctx context.Context, tx *db.Tx) error {
		return migrateUp(ctx, tx, webhookDeliveryRetentionVersion, webhookDeliveryRetentionName)
	},
	Rollback: func(ctx context.Context, tx *db.Tx) error {
		return migrateDown(ctx, tx, webhookDeliveryRetentionVersion, webhookDeliveryRetentionName)
	},
}
