package store

import (
	"context"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/google/uuid"
)

// WebhookStore is an interface for managing webhooks.
type WebhookStore interface {
	// GetWebhookByID returns a webhook by its ID.
	GetWebhookByID(ctx context.Context, h db.Handler, repoID int64, id int64) (models.Webhook, error)
	// GetWebhooksByRepoID returns all webhooks for a repository.
	GetWebhooksByRepoID(ctx context.Context, h db.Handler, repoID int64) ([]models.Webhook, error)
	// LockAndCountWebhooks returns how many webhooks a repository has. On
	// Postgres it first locks the repository's row until the transaction
	// ends, so that concurrent callers that count and then insert in one
	// transaction take turns; SQLite already runs one writer at a time.
	LockAndCountWebhooks(ctx context.Context, h db.Handler, repoID int64) (int, error)
	// GetWebhooksByRepoIDWhereEvent returns all webhooks for a repository where event is in the events.
	GetWebhooksByRepoIDWhereEvent(ctx context.Context, h db.Handler, repoID int64, events []int) ([]models.Webhook, error)
	// CreateWebhook creates a webhook.
	CreateWebhook(ctx context.Context, h db.Handler, repoID int64, url string, secret string, contentType int, active bool) (int64, error)
	// UpdateWebhookByID updates a webhook by its ID.
	UpdateWebhookByID(ctx context.Context, h db.Handler, repoID int64, id int64, url string, secret string, contentType int, active bool) error
	// DeleteWebhookByID deletes a webhook by its ID.
	DeleteWebhookByID(ctx context.Context, h db.Handler, id int64) error
	// DeleteWebhookForRepoByID deletes a webhook for a repository by its ID.
	DeleteWebhookForRepoByID(ctx context.Context, h db.Handler, repoID int64, id int64) error
	// DeleteWebhookEventsByWebhookID deletes all events of a webhook.
	DeleteWebhookEventsByWebhookID(ctx context.Context, h db.Handler, webhookID int64) error
	// DeleteWebhookDeliveriesByWebhookID deletes all deliveries of a webhook.
	DeleteWebhookDeliveriesByWebhookID(ctx context.Context, h db.Handler, webhookID int64) error

	// GetWebhookEventByID returns a webhook event by its ID.
	GetWebhookEventByID(ctx context.Context, h db.Handler, id int64) (models.WebhookEvent, error)
	// GetWebhookEventsByWebhookID returns all webhook events for a webhook.
	GetWebhookEventsByWebhookID(ctx context.Context, h db.Handler, webhookID int64) ([]models.WebhookEvent, error)
	// CreateWebhookEvents creates webhook events for a webhook.
	CreateWebhookEvents(ctx context.Context, h db.Handler, webhookID int64, events []int) error
	// DeleteWebhookEventsByWebhookID deletes all webhook events for a webhook.
	DeleteWebhookEventsByID(ctx context.Context, h db.Handler, ids []int64) error

	// GetWebhookDeliveryByID returns a webhook delivery by its ID.
	GetWebhookDeliveryByID(ctx context.Context, h db.Handler, webhookID int64, id uuid.UUID) (models.WebhookDelivery, error)
	// GetWebhookDeliveriesByWebhookID returns all webhook deliveries for a
	// webhook, oldest first.
	GetWebhookDeliveriesByWebhookID(ctx context.Context, h db.Handler, webhookID int64) ([]models.WebhookDelivery, error)
	// ListWebhookDeliveriesByWebhookID returns all webhook deliveries for a
	// webhook, oldest first. This only returns the delivery ID, response
	// status, event, creation time, and sequence number.
	ListWebhookDeliveriesByWebhookID(ctx context.Context, h db.Handler, webhookID int64) ([]models.WebhookDelivery, error)
	// CreateWebhookDelivery creates a webhook delivery. It writes nothing if the
	// webhook no longer exists.
	CreateWebhookDelivery(ctx context.Context, h db.Handler, id uuid.UUID, webhookID int64, event int, url string, method string, requestError error, requestHeaders string, requestBody string, responseStatus int, responseHeaders string, responseBody string) error
	// DeleteWebhookDeliveryByID deletes a webhook delivery by its ID.
	DeleteWebhookDeliveryByID(ctx context.Context, h db.Handler, webhookID int64, id uuid.UUID) error
	// PruneWebhookDeliveries deletes all but the keep most recent deliveries
	// of a webhook, in the order they were recorded (by seq).
	PruneWebhookDeliveries(ctx context.Context, h db.Handler, webhookID int64, keep int) error
}
