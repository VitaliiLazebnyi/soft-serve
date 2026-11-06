package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/google/uuid"
	"github.com/lib/pq"
	sqlite "modernc.org/sqlite"
	sqlitelib "modernc.org/sqlite/lib"
)

type webhookStore struct{}

var _ store.WebhookStore = (*webhookStore)(nil)

// CreateWebhook implements store.WebhookStore.
func (*webhookStore) CreateWebhook(ctx context.Context, h db.Handler, repoID int64, url string, secret string, contentType int, active bool) (int64, error) {
	var id int64
	query := h.Rebind(`INSERT INTO webhooks (repo_id, url, secret, content_type, active, updated_at)
			VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP) RETURNING id;`)
	err := h.GetContext(ctx, &id, query, repoID, url, secret, contentType, active)
	if err != nil {
		return 0, err
	}

	return id, nil
}

// CreateWebhookDelivery implements store.WebhookStore.
//
// The row is written only if the webhook still exists. A send that was in
// flight when its webhook was deleted therefore leaves no orphan row, even on
// a connection that does not enforce foreign keys. In that case nothing is
// written and no error is returned. The same holds when the webhook is
// deleted between the existence check and the insert, which a concurrent
// transaction can do on Postgres and which shows up as a foreign key
// violation.
func (*webhookStore) CreateWebhookDelivery(ctx context.Context, h db.Handler, id uuid.UUID, webhookID int64, event int, url string, method string, requestError error, requestHeaders string, requestBody string, responseStatus int, responseHeaders string, responseBody string) error {
	query := h.Rebind(`INSERT INTO webhook_deliveries (id, webhook_id, event, request_url, request_method, request_error, request_headers, request_body, response_status, response_headers, response_body)
			SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
			WHERE EXISTS (SELECT 1 FROM webhooks WHERE id = ?);`)
	// A successful delivery stores NULL, not an empty string.
	var reqErr sql.NullString
	if requestError != nil {
		reqErr = sql.NullString{String: requestError.Error(), Valid: true}
	}
	_, err := h.ExecContext(ctx, query, id, webhookID, event, url, method, reqErr, requestHeaders, requestBody, responseStatus, responseHeaders, responseBody, webhookID)
	if isForeignKeyViolation(err) {
		return nil
	}
	return err
}

// isForeignKeyViolation reports whether err is a foreign key violation from
// SQLite or Postgres.
func isForeignKeyViolation(err error) bool {
	var liteErr *sqlite.Error
	if errors.As(err, &liteErr) {
		return liteErr.Code() == sqlitelib.SQLITE_CONSTRAINT_FOREIGNKEY
	}
	var pgErr *pq.Error
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23503" // foreign_key_violation
	}
	return false
}

// CreateWebhookEvents implements store.WebhookStore.
func (*webhookStore) CreateWebhookEvents(ctx context.Context, h db.Handler, webhookID int64, events []int) error {
	query := h.Rebind(`INSERT INTO webhook_events (webhook_id, event)
			VALUES (?, ?);`)
	for _, event := range events {
		_, err := h.ExecContext(ctx, query, webhookID, event)
		if err != nil {
			return err
		}
	}
	return nil
}

// DeleteWebhookByID implements store.WebhookStore.
func (*webhookStore) DeleteWebhookByID(ctx context.Context, h db.Handler, id int64) error {
	query := h.Rebind(`DELETE FROM webhooks WHERE id = ?;`)
	_, err := h.ExecContext(ctx, query, id)
	return err
}

// DeleteWebhookForRepoByID implements store.WebhookStore.
func (*webhookStore) DeleteWebhookForRepoByID(ctx context.Context, h db.Handler, repoID int64, id int64) error {
	query := h.Rebind(`DELETE FROM webhooks WHERE repo_id = ? AND id = ?;`)
	_, err := h.ExecContext(ctx, query, repoID, id)
	return err
}

// DeleteWebhookEventsByWebhookID implements store.WebhookStore.
func (*webhookStore) DeleteWebhookEventsByWebhookID(ctx context.Context, h db.Handler, webhookID int64) error {
	query := h.Rebind(`DELETE FROM webhook_events WHERE webhook_id = ?;`)
	_, err := h.ExecContext(ctx, query, webhookID)
	return err
}

// DeleteWebhookDeliveriesByWebhookID implements store.WebhookStore.
func (*webhookStore) DeleteWebhookDeliveriesByWebhookID(ctx context.Context, h db.Handler, webhookID int64) error {
	query := h.Rebind(`DELETE FROM webhook_deliveries WHERE webhook_id = ?;`)
	_, err := h.ExecContext(ctx, query, webhookID)
	return err
}

// DeleteWebhookDeliveryByID implements store.WebhookStore.
func (*webhookStore) DeleteWebhookDeliveryByID(ctx context.Context, h db.Handler, webhookID int64, id uuid.UUID) error {
	query := h.Rebind(`DELETE FROM webhook_deliveries WHERE webhook_id = ? AND id = ?;`)
	_, err := h.ExecContext(ctx, query, webhookID, id)
	return err
}

// PruneWebhookDeliveries implements store.WebhookStore.
//
// Deliveries are ordered by seq, which increases with every delivery
// recorded. The subquery finds the seq of the newest delivery past the keep
// most recent, and everything up to it is deleted. Both steps walk the
// (webhook_id, seq) index, so the cost depends on keep and on the rows
// deleted, not on the size of the table. With keep or fewer deliveries the
// subquery is NULL and nothing is deleted. The same statement works on SQLite
// and Postgres.
func (*webhookStore) PruneWebhookDeliveries(ctx context.Context, h db.Handler, webhookID int64, keep int) error {
	if keep < 0 {
		keep = 0
	}
	_, err := h.ExecContext(ctx, h.Rebind(pruneWebhookDeliveriesQuery), webhookID, webhookID, keep)
	return err
}

// pruneWebhookDeliveriesQuery takes the webhook ID twice and then keep.
const pruneWebhookDeliveriesQuery = `DELETE FROM webhook_deliveries
	WHERE webhook_id = ?
	AND seq <= (
		SELECT seq FROM webhook_deliveries
		WHERE webhook_id = ?
		ORDER BY seq DESC
		LIMIT 1 OFFSET ?
	);`

// DeleteWebhookEventsByID implements store.WebhookStore.
func (*webhookStore) DeleteWebhookEventsByID(ctx context.Context, h db.Handler, ids []int64) error {
	// sqlx rejects an empty IN clause, and there is nothing to delete.
	// Updating a webhook without changing its events reaches here with no IDs.
	if len(ids) == 0 {
		return nil
	}

	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	query := h.Rebind(`DELETE FROM webhook_events WHERE id IN (` + placeholders(len(ids)) + `);`)
	_, err := h.ExecContext(ctx, query, args...)
	return err
}

// GetWebhookByID implements store.WebhookStore.
func (*webhookStore) GetWebhookByID(ctx context.Context, h db.Handler, repoID int64, id int64) (models.Webhook, error) {
	query := h.Rebind(`SELECT * FROM webhooks WHERE repo_id = ? AND id = ?;`)
	var wh models.Webhook
	err := h.GetContext(ctx, &wh, query, repoID, id)
	return wh, err
}

// GetWebhookDeliveriesByWebhookID implements store.WebhookStore.
// Deliveries are returned oldest first, in the order they were recorded.
func (*webhookStore) GetWebhookDeliveriesByWebhookID(ctx context.Context, h db.Handler, webhookID int64) ([]models.WebhookDelivery, error) {
	query := h.Rebind(`SELECT * FROM webhook_deliveries WHERE webhook_id = ? ORDER BY seq;`)
	var whds []models.WebhookDelivery
	err := h.SelectContext(ctx, &whds, query, webhookID)
	return whds, err
}

// GetWebhookDeliveryByID implements store.WebhookStore.
func (*webhookStore) GetWebhookDeliveryByID(ctx context.Context, h db.Handler, webhookID int64, id uuid.UUID) (models.WebhookDelivery, error) {
	query := h.Rebind(`SELECT * FROM webhook_deliveries WHERE webhook_id = ? AND id = ?;`)
	var whd models.WebhookDelivery
	err := h.GetContext(ctx, &whd, query, webhookID, id)
	return whd, err
}

// GetWebhookEventByID implements store.WebhookStore.
func (*webhookStore) GetWebhookEventByID(ctx context.Context, h db.Handler, id int64) (models.WebhookEvent, error) {
	query := h.Rebind(`SELECT * FROM webhook_events WHERE id = ?;`)
	var whe models.WebhookEvent
	err := h.GetContext(ctx, &whe, query, id)
	return whe, err
}

// GetWebhookEventsByWebhookID implements store.WebhookStore.
func (*webhookStore) GetWebhookEventsByWebhookID(ctx context.Context, h db.Handler, webhookID int64) ([]models.WebhookEvent, error) {
	query := h.Rebind(`SELECT * FROM webhook_events WHERE webhook_id = ? ORDER BY id;`)
	var whes []models.WebhookEvent
	err := h.SelectContext(ctx, &whes, query, webhookID)
	return whes, err
}

// GetWebhooksByRepoID implements store.WebhookStore.
func (*webhookStore) GetWebhooksByRepoID(ctx context.Context, h db.Handler, repoID int64) ([]models.Webhook, error) {
	query := h.Rebind(`SELECT * FROM webhooks WHERE repo_id = ? ORDER BY id;`)
	var whs []models.Webhook
	err := h.SelectContext(ctx, &whs, query, repoID)
	return whs, err
}

// LockAndCountWebhooks implements store.WebhookStore.
//
// Postgres runs transactions in READ COMMITTED, where two transactions can
// each insert a webhook and count without seeing the other's. Locking the
// repository row before the COUNT makes the second wait until the first
// commits; its COUNT then runs with a fresh snapshot and sees the first one's
// webhook. FOR NO KEY UPDATE conflicts only with itself and FOR UPDATE, so
// unlike FOR UPDATE it does not block the key-share locks that inserts into
// tables referencing the repository take, including the caller's own insert
// in another transaction.
//
// SQLite has no row locks and needs none here: a transaction that has
// already written holds the database's write lock until it ends, so callers
// that insert before they count take turns.
func (*webhookStore) LockAndCountWebhooks(ctx context.Context, h db.Handler, repoID int64) (int, error) {
	d, ok := h.(interface{ DriverName() string })
	if !ok {
		return 0, fmt.Errorf("lock and count webhooks: handler %T does not report its driver", h)
	}
	switch driver := d.DriverName(); driver {
	case "postgres":
		if _, err := h.ExecContext(ctx, `SELECT 1 FROM repos WHERE id = $1 FOR NO KEY UPDATE;`, repoID); err != nil {
			return 0, err
		}
	case "sqlite", "sqlite3":
	default:
		return 0, fmt.Errorf("lock and count webhooks: unsupported database driver %q", driver)
	}
	var n int
	err := h.GetContext(ctx, &n, h.Rebind(`SELECT COUNT(*) FROM webhooks WHERE repo_id = ?;`), repoID)
	return n, err
}

// GetWebhooksByRepoIDWhereEvent implements store.WebhookStore. Only active
// webhooks are returned, since this is what events are sent to; use
// GetWebhooksByRepoID to list every webhook.
func (*webhookStore) GetWebhooksByRepoIDWhereEvent(ctx context.Context, h db.Handler, repoID int64, events []int) ([]models.Webhook, error) {
	if len(events) == 0 {
		return nil, nil
	}

	args := []any{repoID, true}
	for _, e := range events {
		args = append(args, e)
	}
	query := h.Rebind(`SELECT webhooks.*
			FROM webhooks
			INNER JOIN webhook_events ON webhooks.id = webhook_events.webhook_id
			WHERE webhooks.repo_id = ? AND webhooks.active = ? AND webhook_events.event IN (` + placeholders(len(events)) + `)
			ORDER BY webhooks.id;`)
	var whs []models.Webhook
	err := h.SelectContext(ctx, &whs, query, args...)
	return whs, err
}

// ListWebhookDeliveriesByWebhookID implements store.WebhookStore.
// Deliveries are returned oldest first, in the order they were recorded.
func (*webhookStore) ListWebhookDeliveriesByWebhookID(ctx context.Context, h db.Handler, webhookID int64) ([]models.WebhookDelivery, error) {
	query := h.Rebind(`SELECT id, response_status, event, created_at, seq FROM webhook_deliveries WHERE webhook_id = ? ORDER BY seq;`)
	var whds []models.WebhookDelivery
	err := h.SelectContext(ctx, &whds, query, webhookID)
	return whds, err
}

// UpdateWebhookByID implements store.WebhookStore.
func (*webhookStore) UpdateWebhookByID(ctx context.Context, h db.Handler, repoID int64, id int64, url string, secret string, contentType int, active bool) error {
	query := h.Rebind(`UPDATE webhooks SET url = ?, secret = ?, content_type = ?, active = ?, updated_at = CURRENT_TIMESTAMP WHERE repo_id = ? AND id = ?;`)
	_, err := h.ExecContext(ctx, query, url, secret, contentType, active, repoID, id)
	return err
}

// placeholders returns n comma-separated "?" placeholders for an IN clause.
// The callers never pass n == 0; they return early for an empty list.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
