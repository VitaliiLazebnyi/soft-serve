package backend

import (
	"context"
	"encoding/json"
	neturl "net/url"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/utils"
	"github.com/charmbracelet/soft-serve/pkg/webhook"
	"github.com/google/uuid"
)

// CreateWebhook creates a webhook for a repository.
func (b *Backend) CreateWebhook(ctx context.Context, repo proto.Repository, url string, contentType webhook.ContentType, secret string, events []webhook.Event, active bool) error {
	dbx := db.FromContext(ctx)
	datastore := store.FromContext(ctx)
	url = utils.Sanitize(url)
	if err := validateWebhookInput(url, contentType, events); err != nil {
		return err
	}
	events = dedupeEvents(events)

	return dbx.TransactionContext(ctx, func(tx *db.Tx) error {
		lastID, err := datastore.CreateWebhook(ctx, tx, repo.ID(), url, secret, int(contentType), active)
		if err != nil {
			return db.WrapError(err)
		}

		evs := make([]int, len(events))
		for i, e := range events {
			evs[i] = int(e)
		}
		if err := datastore.CreateWebhookEvents(ctx, tx, lastID, evs); err != nil {
			return db.WrapError(err)
		}

		return nil
	})
}

// Webhook returns a webhook for a repository.
func (b *Backend) Webhook(ctx context.Context, repo proto.Repository, id int64) (webhook.Hook, error) {
	dbx := db.FromContext(ctx)
	datastore := store.FromContext(ctx)

	var wh webhook.Hook
	if err := dbx.TransactionContext(ctx, func(tx *db.Tx) error {
		h, err := datastore.GetWebhookByID(ctx, tx, repo.ID(), id)
		if err != nil {
			return db.WrapError(err)
		}
		events, err := datastore.GetWebhookEventsByWebhookID(ctx, tx, id)
		if err != nil {
			return db.WrapError(err)
		}

		wh = webhook.Hook{
			Webhook:     h,
			ContentType: webhook.ContentType(h.ContentType), //nolint:gosec
			Events:      make([]webhook.Event, len(events)),
		}
		for i, e := range events {
			wh.Events[i] = webhook.Event(e.Event)
		}

		return nil
	}); err != nil {
		return webhook.Hook{}, db.WrapError(err)
	}

	return wh, nil
}

// ListWebhooks lists webhooks for a repository.
func (b *Backend) ListWebhooks(ctx context.Context, repo proto.Repository) ([]webhook.Hook, error) {
	dbx := db.FromContext(ctx)
	datastore := store.FromContext(ctx)

	var webhooks []models.Webhook
	webhookEvents := map[int64][]models.WebhookEvent{}
	if err := dbx.TransactionContext(ctx, func(tx *db.Tx) error {
		var err error
		webhooks, err = datastore.GetWebhooksByRepoID(ctx, tx, repo.ID())
		if err != nil {
			return err
		}

		for _, h := range webhooks {
			events, err := datastore.GetWebhookEventsByWebhookID(ctx, tx, h.ID)
			if err != nil {
				return err
			}
			webhookEvents[h.ID] = events
		}

		return nil
	}); err != nil {
		return nil, db.WrapError(err)
	}

	hooks := make([]webhook.Hook, len(webhooks))
	for i, h := range webhooks {
		events := make([]webhook.Event, len(webhookEvents[h.ID]))
		for i, e := range webhookEvents[h.ID] {
			events[i] = webhook.Event(e.Event)
		}

		hooks[i] = webhook.Hook{
			Webhook:     h,
			ContentType: webhook.ContentType(h.ContentType), //nolint:gosec
			Events:      events,
		}
	}

	return hooks, nil
}

// UpdateWebhook updates a webhook.
func (b *Backend) UpdateWebhook(ctx context.Context, repo proto.Repository, id int64, url string, contentType webhook.ContentType, secret string, updatedEvents []webhook.Event, active bool) error {
	dbx := db.FromContext(ctx)
	datastore := store.FromContext(ctx)

	url = utils.Sanitize(url)
	if err := validateWebhookInput(url, contentType, updatedEvents); err != nil {
		return err
	}
	updatedEvents = dedupeEvents(updatedEvents)

	return dbx.TransactionContext(ctx, func(tx *db.Tx) error {
		// The webhook must belong to this repository. Checking first keeps the
		// event rewrite below from touching another repository's webhook, and
		// makes an unknown ID an error instead of a silent no-op.
		if _, err := datastore.GetWebhookByID(ctx, tx, repo.ID(), id); err != nil {
			return db.WrapError(err)
		}

		if err := datastore.UpdateWebhookByID(ctx, tx, repo.ID(), id, url, secret, int(contentType), active); err != nil {
			return db.WrapError(err)
		}

		currentEvents, err := datastore.GetWebhookEventsByWebhookID(ctx, tx, id)
		if err != nil {
			return db.WrapError(err)
		}

		// Delete events that are no longer in the list.
		toBeDeleted := make([]int64, 0)
		for _, e := range currentEvents {
			found := false
			for _, ne := range updatedEvents {
				if int(ne) == e.Event {
					found = true
					break
				}
			}
			if !found {
				toBeDeleted = append(toBeDeleted, e.ID)
			}
		}

		if err := datastore.DeleteWebhookEventsByID(ctx, tx, toBeDeleted); err != nil {
			return db.WrapError(err)
		}

		// Prune events that are already in the list.
		newEvents := make([]int, 0)
		for _, e := range updatedEvents {
			found := false
			for _, ne := range currentEvents {
				if int(e) == ne.Event {
					found = true
					break
				}
			}
			if !found {
				newEvents = append(newEvents, int(e))
			}
		}

		if err := datastore.CreateWebhookEvents(ctx, tx, id, newEvents); err != nil {
			return db.WrapError(err)
		}

		return nil
	})
}

// DeleteWebhook deletes a webhook for a repository.
func (b *Backend) DeleteWebhook(ctx context.Context, repo proto.Repository, id int64) error {
	dbx := db.FromContext(ctx)
	datastore := store.FromContext(ctx)

	return dbx.TransactionContext(ctx, func(tx *db.Tx) error {
		_, err := datastore.GetWebhookByID(ctx, tx, repo.ID(), id)
		if err != nil {
			return db.WrapError(err)
		}
		return db.WrapError(b.deleteWebhookRows(ctx, tx, repo.ID(), id))
	})
}

// deleteWebhookRows removes one webhook of a repository together with its
// events and deliveries. The rows are removed explicitly rather than through
// ON DELETE CASCADE, which only applies when the connection enables foreign
// keys; the default SQLite data source does, but a custom one may not.
func (d *Backend) deleteWebhookRows(ctx context.Context, tx *db.Tx, repoID int64, id int64) error {
	if err := d.store.DeleteWebhookDeliveriesByWebhookID(ctx, tx, id); err != nil {
		return err
	}
	if err := d.store.DeleteWebhookEventsByWebhookID(ctx, tx, id); err != nil {
		return err
	}
	return d.store.DeleteWebhookForRepoByID(ctx, tx, repoID, id)
}

// deleteRepoWebhookRows removes every webhook of a repository with its rows.
func (d *Backend) deleteRepoWebhookRows(ctx context.Context, tx *db.Tx, repoID int64) error {
	whs, err := d.store.GetWebhooksByRepoID(ctx, tx, repoID)
	if err != nil {
		return err
	}
	for _, w := range whs {
		if err := d.deleteWebhookRows(ctx, tx, repoID, w.ID); err != nil {
			return err
		}
	}
	return nil
}

// ListWebhookDeliveries lists webhook deliveries for a webhook belonging to
// the given repository.
//
// The webhook is resolved scoped to the repository, so a caller authorized
// only for repo A cannot read deliveries of a webhook owned by repo B.
func (b *Backend) ListWebhookDeliveries(ctx context.Context, repo proto.Repository, id int64) ([]webhook.Delivery, error) {
	dbx := db.FromContext(ctx)
	datastore := store.FromContext(ctx)

	var deliveries []models.WebhookDelivery
	if err := dbx.TransactionContext(ctx, func(tx *db.Tx) error {
		if _, err := datastore.GetWebhookByID(ctx, tx, repo.ID(), id); err != nil {
			return db.WrapError(err)
		}

		var err error
		deliveries, err = datastore.ListWebhookDeliveriesByWebhookID(ctx, tx, id)
		if err != nil {
			return db.WrapError(err)
		}

		return nil
	}); err != nil {
		return nil, db.WrapError(err)
	}

	ds := make([]webhook.Delivery, len(deliveries))
	for i, d := range deliveries {
		ds[i] = webhook.Delivery{
			WebhookDelivery: d,
			Event:           webhook.Event(d.Event),
		}
	}

	return ds, nil
}

// RedeliverWebhookDelivery redelivers a webhook delivery.
func (b *Backend) RedeliverWebhookDelivery(ctx context.Context, repo proto.Repository, id int64, delID uuid.UUID) error {
	dbx := db.FromContext(ctx)
	datastore := store.FromContext(ctx)

	var delivery models.WebhookDelivery
	var wh models.Webhook
	if err := dbx.TransactionContext(ctx, func(tx *db.Tx) error {
		var err error
		wh, err = datastore.GetWebhookByID(ctx, tx, repo.ID(), id)
		if err != nil {
			log.Errorf("error getting webhook: %v", err)
			return db.WrapError(err)
		}

		delivery, err = datastore.GetWebhookDeliveryByID(ctx, tx, id, delID)
		if err != nil {
			return db.WrapError(err)
		}

		return nil
	}); err != nil {
		return db.WrapError(err)
	}

	log.Infof("redelivering webhook delivery %s for webhook %d\n\n%s\n\n", delID, id, delivery.RequestBody)

	// Stored bodies are in the webhook's own content type: form-encoded for
	// form webhooks, JSON otherwise.
	var payload any
	if webhook.ContentType(wh.ContentType) == webhook.ContentTypeForm { //nolint:gosec
		vals, err := neturl.ParseQuery(delivery.RequestBody)
		if err != nil {
			log.Errorf("error parsing webhook form body: %v", err)
			return err
		}
		payload = vals
	} else {
		var raw json.RawMessage
		if err := json.Unmarshal([]byte(delivery.RequestBody), &raw); err != nil {
			log.Errorf("error unmarshaling webhook payload: %v", err)
			return err
		}
		payload = raw
	}

	return webhook.SendWebhook(ctx, wh, webhook.Event(delivery.Event), payload)
}

// WebhookDelivery returns a webhook delivery for a webhook belonging to the
// given repository.
//
// The webhook is resolved scoped to the repository, so a caller authorized
// only for repo A cannot read a delivery of a webhook owned by repo B.
// Deliveries expose the full request URL, headers, and body, so this scoping
// is what keeps them from leaking across repositories.
func (b *Backend) WebhookDelivery(ctx context.Context, repo proto.Repository, webhookID int64, id uuid.UUID) (webhook.Delivery, error) {
	dbx := db.FromContext(ctx)
	datastore := store.FromContext(ctx)

	var delivery webhook.Delivery
	if err := dbx.TransactionContext(ctx, func(tx *db.Tx) error {
		if _, err := datastore.GetWebhookByID(ctx, tx, repo.ID(), webhookID); err != nil {
			return db.WrapError(err)
		}

		d, err := datastore.GetWebhookDeliveryByID(ctx, tx, webhookID, id)
		if err != nil {
			return db.WrapError(err)
		}

		delivery = webhook.Delivery{
			WebhookDelivery: d,
			Event:           webhook.Event(d.Event),
		}

		return nil
	}); err != nil {
		return webhook.Delivery{}, db.WrapError(err)
	}

	return delivery, nil
}

// validateWebhookInput checks a webhook's URL, content type and events before
// anything is stored.
func validateWebhookInput(url string, contentType webhook.ContentType, events []webhook.Event) error {
	// Validate webhook URL to prevent SSRF attacks.
	if err := webhook.ValidateWebhookURL(url); err != nil {
		return err //nolint:wrapcheck
	}
	if contentType.String() == "" {
		return webhook.ErrInvalidContentType
	}
	for _, e := range events {
		if e.String() == "" {
			return webhook.ErrInvalidEvent
		}
	}
	return nil
}

// dedupeEvents returns events without repeats, keeping the first occurrence.
// A webhook can subscribe to an event only once.
func dedupeEvents(events []webhook.Event) []webhook.Event {
	seen := make(map[webhook.Event]bool, len(events))
	out := make([]webhook.Event, 0, len(events))
	for _, e := range events {
		if !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	return out
}

// webhookSendTimeout bounds how long the deliveries of one event may take,
// including waiting on slow receivers.
const webhookSendTimeout = time.Minute

// sendAfterChange sends the webhook event for a change that is already
// committed. The subscribers are read now and the deliveries run in the
// background, so the caller does not wait on receivers. The change has
// happened, so a failure is logged rather than returned.
func (d *Backend) sendAfterChange(ctx context.Context, ev webhook.EventPayload) {
	hooks, err := webhook.Subscribers(ctx, ev)
	if err != nil {
		d.logEventNotDelivered(ev, err)
		return
	}
	d.deliverAfterChange(ctx, hooks, ev, true)
}

// deliverAfterChange sends ev to hooks in the background. It is detached from
// the request's cancellation but still bounded by webhookSendTimeout.
func (d *Backend) deliverAfterChange(ctx context.Context, hooks []models.Webhook, ev webhook.EventPayload, record bool) {
	if len(hooks) == 0 {
		return
	}
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), webhookSendTimeout)
	d.deliveries.Add(1)
	go func() {
		defer d.deliveries.Done()
		defer cancel()
		var err error
		if record {
			err = webhook.Deliver(bg, hooks, ev)
		} else {
			err = webhook.DeliverUnrecorded(bg, hooks, ev)
		}
		if err != nil {
			d.logEventNotDelivered(ev, err)
		}
	}()
}

// WaitForDeliveries waits for the background webhook deliveries that have
// started to finish. It gives up when ctx is done and logs a warning, since
// the remaining deliveries are then abandoned. Call it after the servers have
// stopped accepting requests, so no new deliveries start.
func (d *Backend) WaitForDeliveries(ctx context.Context) {
	done := make(chan struct{})
	go func() {
		d.deliveries.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		d.logger.Warn("shutting down with webhook deliveries still running", "err", ctx.Err())
	}
}

func (d *Backend) logEventNotDelivered(ev webhook.EventPayload, err error) {
	d.logger.Error("webhook event not delivered", "event", ev.Event(), "repo_id", ev.RepositoryID(), "err", err)
}
