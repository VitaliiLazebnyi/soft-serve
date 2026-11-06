package backend

import (
	"context"
	"encoding/json"
	"errors"
	neturl "net/url"
	"runtime/debug"
	"strings"
	"sync"
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
			b.logger.Error("error getting webhook", "webhook", id, "repo", repo.Name(), "err", err)
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

	// Log identifiers only: the stored body, headers and signature carry repo,
	// user and commit data, and must not end up in server logs.
	b.logger.Info("redelivering webhook delivery", "delivery", delID, "webhook", id, "event", webhook.Event(delivery.Event), "repo", repo.Name())

	// The stored body is in the content type the delivery was sent with, which
	// may differ from the webhook's current one if it was changed since. Use
	// the stored Content-Type header and fall back to the webhook's only when
	// that header is missing.
	// An unrecognized stored value is treated like a missing one.
	ct := webhook.ContentType(wh.ContentType) //nolint:gosec
	stored, found, err := storedContentType(delivery.RequestHeaders)
	switch {
	case err != nil:
		b.logger.Warn("unrecognized content type in webhook delivery, using the webhook's", "delivery", delID, "webhook", id, "content_type", ct, "err", err)
	case found:
		ct = stored
	}
	wh.ContentType = int(ct)

	var payload any
	if ct == webhook.ContentTypeForm {
		vals, err := neturl.ParseQuery(delivery.RequestBody)
		if err != nil {
			// The parse error quotes part of the body, so it is returned to
			// the caller but not logged.
			b.logger.Error("error parsing webhook form body", "delivery", delID, "webhook", id)
			return err
		}
		payload = vals
	} else {
		var raw json.RawMessage
		if err := json.Unmarshal([]byte(delivery.RequestBody), &raw); err != nil {
			// As above: the error can quote part of the body.
			b.logger.Error("error unmarshaling webhook payload", "delivery", delID, "webhook", id)
			return err
		}
		payload = raw
	}

	return webhook.SendWebhook(ctx, wh, webhook.Event(delivery.Event), payload)
}

// storedContentType returns the Content-Type of a delivery from its stored
// request headers, which hold one "Name: value" line per header. It reports
// false when there is no Content-Type line.
func storedContentType(headers string) (webhook.ContentType, bool, error) {
	for _, line := range strings.Split(headers, "\n") {
		name, value, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(name), "Content-Type") {
			continue
		}
		ct, err := webhook.ParseContentType(strings.TrimSpace(value))
		return ct, true, err
	}
	return 0, false, nil
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

// ShutdownDeliveryTimeout is how long shutdown waits for background
// deliveries. It is separate from the shorter HTTP and SSH drain, since a drain
// that stops early abandons the deliveries. It covers a request that started
// just before shutdown and runs to the client's cap (ssrf.ClientTimeout), with
// time left to record it and to send a few queued ones. A second signal stops
// the wait early.
const ShutdownDeliveryTimeout = time.Minute

// SendEventAfterChange sends the webhook event for a change that is already
// committed, the same way the backend's own changes do. See sendAfterChange.
func (d *Backend) SendEventAfterChange(ctx context.Context, ev webhook.EventPayload) {
	d.sendAfterChange(ctx, ev)
}

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

// maxBackgroundDeliveries caps the goroutines that send events in the
// background, and maxQueuedDeliveries caps the events waiting for one of them.
// Together they bound the memory and goroutines the deliveries use.
//
// One repository's events cannot take all of them: maxRepoRunningDeliveries
// caps the senders one repository uses at once, and maxRepoDeliveries caps its
// events queued plus running. A repository whose receivers are slow or never
// answer, each send taking up to the per-hook timeout, then only delays and
// drops its own events, and the others keep being sent.
const (
	maxBackgroundDeliveries  = 32
	maxQueuedDeliveries      = 1024
	maxRepoRunningDeliveries = 4
	maxRepoDeliveries        = 64
)

// errDeliveryQueueFull is recorded for an event that was dropped because the
// delivery queue was full.
var errDeliveryQueueFull = errors.New("not sent: webhook delivery queue is full")

// errRepoDeliveriesFull is recorded for an event that was dropped because its
// repository already had maxRepoDeliveries events queued or running.
var errRepoDeliveriesFull = errors.New("not sent: too many webhook deliveries pending for this repository")

// deliverAfterChange sends ev to hooks in the background and never waits on
// receivers. The send is detached from the request's cancellation; each
// request is bounded by the webhook package's per-hook timeout.
//
// When every sender is busy, or the repository already uses its share of
// them, the event waits in a bounded queue. When the queue, or the
// repository's share of it, is full, the event is dropped: it is logged and,
// if record is set, recorded as a failed delivery so it can be redelivered.
func (d *Backend) deliverAfterChange(ctx context.Context, hooks []models.Webhook, ev webhook.EventPayload, record bool) {
	if len(hooks) == 0 {
		return
	}
	bg := context.WithoutCancel(ctx)
	send := func() {
		var err error
		if record {
			err = webhook.Deliver(bg, hooks, ev)
		} else {
			err = webhook.DeliverUnrecorded(bg, hooks, ev)
		}
		if err != nil {
			d.logEventNotDelivered(ev, err)
		}
	}

	var reason error
	switch d.deliveries.enqueue(ev.RepositoryID(), send) {
	case enqueueClosed:
		d.logger.Warn("webhook event skipped: server is shutting down", "event", ev.Event(), "repo_id", ev.RepositoryID())
	case enqueueFull:
		d.logger.Error("webhook event dropped: delivery queue is full", "event", ev.Event(), "repo_id", ev.RepositoryID(), "queued", maxQueuedDeliveries)
		reason = errDeliveryQueueFull
	case enqueueRepoFull:
		d.logger.Error("webhook event dropped: repository has too many pending deliveries", "event", ev.Event(), "repo_id", ev.RepositoryID(), "pending", maxRepoDeliveries)
		reason = errRepoDeliveriesFull
	}
	if reason != nil && record {
		if err := webhook.RecordUndelivered(bg, hooks, ev, reason); err != nil {
			d.logger.Error("dropped webhook event not recorded", "event", ev.Event(), "repo_id", ev.RepositoryID(), "err", err)
		}
	}
}

// enqueueResult is the outcome of deliveryQueue.enqueue.
type enqueueResult int

const (
	enqueued enqueueResult = iota
	enqueueFull
	enqueueRepoFull
	enqueueClosed
)

// deliveryQueue runs webhook deliveries on at most maxWorkers goroutines, with
// at most maxPending deliveries waiting. Each delivery belongs to a
// repository, and no repository has more than maxRepoRunning deliveries
// running or more than maxRepo queued plus running. Queued deliveries are
// taken from the repositories in turn, so one repository's backlog does not
// hold back the others'.
//
// It counts every delivery from enqueue until it has run, so shutdown can wait
// for them. Once closed, it refuses new ones, so a request still running
// during shutdown cannot add to a count that is already being waited on.
type deliveryQueue struct {
	maxWorkers     int
	maxPending     int
	maxRepoRunning int
	maxRepo        int
	logger         *log.Logger

	mu      sync.Mutex
	closed  bool
	workers int
	queued  int                       // waiting, over all repositories
	repos   map[int64]*repoDeliveries // repositories with deliveries queued or running
	backlog []int64                   // repositories with queued deliveries, in turn order
	count   int                       // queued plus running
	idle    chan struct{}
}

// repoDeliveries is one repository's share of a deliveryQueue.
type repoDeliveries struct {
	running int
	queued  []func()
}

func newDeliveryQueue(maxWorkers, maxPending int, logger *log.Logger) *deliveryQueue {
	return &deliveryQueue{
		maxWorkers:     maxWorkers,
		maxPending:     maxPending,
		maxRepoRunning: maxRepoRunningDeliveries,
		maxRepo:        maxRepoDeliveries,
		logger:         logger,
		repos:          map[int64]*repoDeliveries{},
	}
}

// enqueue starts job for repository repoID on a new goroutine if one is free
// and the repository has not used up its share of them, or queues it. It never
// blocks on a running job.
func (q *deliveryQueue) enqueue(repoID int64, job func()) enqueueResult {
	q.mu.Lock()
	defer q.mu.Unlock()
	r := q.repos[repoID]
	if r == nil {
		r = &repoDeliveries{}
	}
	switch {
	case q.closed:
		return enqueueClosed
	case r.running+len(r.queued) >= q.maxRepo:
		return enqueueRepoFull
	case q.workers < q.maxWorkers && r.running < q.maxRepoRunning:
		q.workers++
		r.running++
		go q.work(repoID, job)
	case q.queued < q.maxPending:
		if len(r.queued) == 0 {
			q.backlog = append(q.backlog, repoID)
		}
		r.queued = append(r.queued, job)
		q.queued++
	default:
		return enqueueFull
	}
	q.repos[repoID] = r
	q.count++
	return enqueued
}

// work runs job for repository repoID, then queued jobs until none it may run
// are left.
func (q *deliveryQueue) work(repoID int64, job func()) {
	for {
		q.run(job)

		q.mu.Lock()
		r := q.repos[repoID]
		r.running--
		if r.running == 0 && len(r.queued) == 0 {
			delete(q.repos, repoID)
		}
		q.count--
		if q.count == 0 && q.idle != nil {
			close(q.idle)
			q.idle = nil
		}
		var ok bool
		repoID, job, ok = q.next()
		if !ok {
			q.workers--
			q.mu.Unlock()
			return
		}
		q.mu.Unlock()
	}
}

// next takes the first queued job of the first repository in the backlog that
// is below its running cap, and moves that repository to the back of the
// backlog if it has more queued. It reports false when there is none. The
// caller holds q.mu.
//
// A repository skipped for being at its cap has a job running, and the worker
// that finishes it calls next again, so its queued jobs are not stranded.
func (q *deliveryQueue) next() (int64, func(), bool) {
	for i, repoID := range q.backlog {
		r := q.repos[repoID]
		if r.running >= q.maxRepoRunning {
			continue
		}
		job := r.queued[0]
		r.queued[0] = nil
		r.queued = r.queued[1:]
		r.running++
		q.queued--
		q.backlog = append(q.backlog[:i], q.backlog[i+1:]...)
		if len(r.queued) > 0 {
			q.backlog = append(q.backlog, repoID)
		}
		return repoID, job, true
	}
	return 0, nil, false
}

// run runs one job. A panic in it is logged rather than allowed to crash the
// process, since no request handler is there to recover it.
func (q *deliveryQueue) run(job func()) {
	defer func() {
		if r := recover(); r != nil {
			q.logger.Error("webhook delivery panicked", "panic", r, "stack", string(debug.Stack()))
		}
	}()
	job()
}

// close refuses new deliveries. It returns how many are queued or running and
// a channel that is closed once they have all run.
func (q *deliveryQueue) close() (int, <-chan struct{}) {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	return q.pending()
}

// pending returns how many deliveries are queued or running and a channel that
// is closed once none are.
func (q *deliveryQueue) pending() (int, <-chan struct{}) {
	q.mu.Lock()
	defer q.mu.Unlock()
	finished := make(chan struct{})
	if q.count == 0 {
		close(finished)
	} else if q.idle == nil {
		q.idle = finished
	} else {
		finished = q.idle
	}
	return q.count, finished
}

// WaitForDeliveries stops new webhook deliveries from starting and waits for
// the queued and running ones to finish. It gives up when ctx is done and logs
// a warning, since the remaining deliveries are then abandoned. Call it after
// the servers have stopped accepting requests.
func (d *Backend) WaitForDeliveries(ctx context.Context) {
	pending, finished := d.deliveries.close()
	if pending == 0 {
		return
	}
	d.logger.Info("waiting for webhook deliveries", "pending", pending)
	select {
	case <-finished:
	case <-ctx.Done():
		left, _ := d.deliveries.pending()
		d.logger.Warn("shutting down with webhook deliveries still running", "pending", left, "err", ctx.Err())
	}
}

func (d *Backend) logEventNotDelivered(ev webhook.EventPayload, err error) {
	d.logger.Error("webhook event not delivered", "event", ev.Event(), "repo_id", ev.RepositoryID(), "err", err)
}
