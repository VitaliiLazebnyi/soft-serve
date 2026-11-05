package backend

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/webhook"
	"github.com/google/uuid"
	"github.com/matryer/is"
)

// Public addresses only: webhook URLs are validated against private ranges.
const (
	whURL      = "https://93.184.216.34/hook"
	whURLOther = "https://93.184.216.35/hook"
)

// syncBuffer is a log sink that background deliveries write to while a test
// may be reading it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// whEnv is a backend wired like a request: config, db and store in the
// context, an admin user, and one repository.
type whEnv struct {
	ctx   context.Context
	be    *Backend
	cfg   *config.Config
	admin proto.User
	repo  proto.Repository
	logs  *syncBuffer
}

func newWebhookEnv(t *testing.T) whEnv {
	t.Helper()
	is := is.New(t)
	logs := &syncBuffer{}
	be, cfg := newTestBackendWithLog(t, logs)
	ctx := config.WithContext(context.Background(), cfg)
	ctx = db.WithContext(ctx, be.db)
	ctx = store.WithContext(ctx, be.store)
	admin, err := be.User(ctx, "admin")
	is.NoErr(err)
	ctx = proto.WithUserContext(ctx, admin)
	repo, err := be.CreateRepository(ctx, "repo", admin, proto.RepositoryOptions{})
	is.NoErr(err)
	return whEnv{ctx: ctx, be: be, cfg: cfg, admin: admin, repo: repo, logs: logs}
}

// otherRepo creates a second repository with one webhook and returns the
// webhook ID, so tests can check that repo-scoped calls cannot reach it.
func (e whEnv) otherRepo(t *testing.T) (proto.Repository, int64) {
	t.Helper()
	is := is.New(t)
	other, err := e.be.CreateRepository(e.ctx, "other", e.admin, proto.RepositoryOptions{})
	is.NoErr(err)
	is.NoErr(e.be.CreateWebhook(e.ctx, other, whURLOther, webhook.ContentTypeJSON, "", []webhook.Event{webhook.EventPush}, true))
	hooks, err := e.be.ListWebhooks(e.ctx, other)
	is.NoErr(err)
	is.Equal(len(hooks), 1)
	return other, hooks[0].ID
}

func sortedEvents(evs []webhook.Event) []webhook.Event {
	out := slices.Clone(evs)
	slices.Sort(out)
	return out
}

func (e whEnv) createHook(t *testing.T, url string, events ...webhook.Event) int64 {
	t.Helper()
	is := is.New(t)
	is.NoErr(e.be.CreateWebhook(e.ctx, e.repo, url, webhook.ContentTypeJSON, "", events, true))
	hooks, err := e.be.ListWebhooks(e.ctx, e.repo)
	is.NoErr(err)
	for _, h := range hooks {
		if h.URL == url {
			return h.ID
		}
	}
	t.Fatalf("webhook %s not found", url)
	return 0
}

func TestCreateWebhookStoresFields(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)

	is.NoErr(e.be.CreateWebhook(e.ctx, e.repo, whURL, webhook.ContentTypeForm, "s3cret",
		[]webhook.Event{webhook.EventPush, webhook.EventBranchTagCreate}, true))

	hooks, err := e.be.ListWebhooks(e.ctx, e.repo)
	is.NoErr(err)
	is.Equal(len(hooks), 1)
	h := hooks[0]
	is.Equal(h.URL, whURL)
	is.Equal(h.ContentType, webhook.ContentTypeForm)
	is.Equal(h.Secret, "s3cret")
	is.True(h.Active)
	is.Equal(h.RepoID, e.repo.ID())
	is.Equal(sortedEvents(h.Events), []webhook.Event{webhook.EventBranchTagCreate, webhook.EventPush})

	got, err := e.be.Webhook(e.ctx, e.repo, h.ID)
	is.NoErr(err)
	is.Equal(got.URL, whURL)
	is.Equal(sortedEvents(got.Events), []webhook.Event{webhook.EventBranchTagCreate, webhook.EventPush})
}

func TestListWebhooksEmpty(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)

	hooks, err := e.be.ListWebhooks(e.ctx, e.repo)
	is.NoErr(err)
	is.Equal(len(hooks), 0)
}

func TestCreateWebhookRejectsUnsafeURLs(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)

	for _, u := range []string{
		"",
		"not a url",
		"ftp://93.184.216.34/hook",
		"http://127.0.0.1/hook",
		"http://10.0.0.1/hook",
		"http://169.254.169.254/latest/meta-data",
	} {
		err := e.be.CreateWebhook(e.ctx, e.repo, u, webhook.ContentTypeJSON, "", []webhook.Event{webhook.EventPush}, true)
		is.True(err != nil)
	}

	hooks, err := e.be.ListWebhooks(e.ctx, e.repo)
	is.NoErr(err)
	is.Equal(len(hooks), 0)
}

func TestCreateWebhookSanitizesURL(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)

	is.NoErr(e.be.CreateWebhook(e.ctx, e.repo, whURL+"\x1b[31m", webhook.ContentTypeJSON, "", []webhook.Event{webhook.EventPush}, true))

	hooks, err := e.be.ListWebhooks(e.ctx, e.repo)
	is.NoErr(err)
	is.Equal(len(hooks), 1)
	is.Equal(hooks[0].URL, whURL)
}

// Suspected bug: an unknown content type is accepted and stored, so every
// later delivery to the webhook fails.
func TestCreateWebhookRejectsInvalidContentType(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)

	err := e.be.CreateWebhook(e.ctx, e.repo, whURL, webhook.ContentType(99), "", []webhook.Event{webhook.EventPush}, true)
	is.True(err != nil)
}

// Suspected bug: an event value outside the known set is stored as-is.
func TestCreateWebhookRejectsUnknownEvent(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)

	err := e.be.CreateWebhook(e.ctx, e.repo, whURL, webhook.ContentTypeJSON, "", []webhook.Event{webhook.Event(99)}, true)
	is.True(err != nil)
}

// Suspected bug: a repeated event violates the unique (webhook_id, event)
// constraint, so the whole create fails with a raw database error instead of
// storing the event once.
func TestCreateWebhookAcceptsDuplicateEvents(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)

	is.NoErr(e.be.CreateWebhook(e.ctx, e.repo, whURL, webhook.ContentTypeJSON, "",
		[]webhook.Event{webhook.EventPush, webhook.EventPush}, true))

	hooks, err := e.be.ListWebhooks(e.ctx, e.repo)
	is.NoErr(err)
	is.Equal(len(hooks), 1)
	is.Equal(hooks[0].Events, []webhook.Event{webhook.EventPush})
}

func TestUpdateWebhookChangesEverything(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	id := e.createHook(t, whURL, webhook.EventPush, webhook.EventCollaborator)

	is.NoErr(e.be.UpdateWebhook(e.ctx, e.repo, id, whURLOther, webhook.ContentTypeForm, "new",
		[]webhook.Event{webhook.EventPush, webhook.EventRepository}, false))

	got, err := e.be.Webhook(e.ctx, e.repo, id)
	is.NoErr(err)
	is.Equal(got.URL, whURLOther)
	is.Equal(got.ContentType, webhook.ContentTypeForm)
	is.Equal(got.Secret, "new")
	is.True(!got.Active)
	is.Equal(sortedEvents(got.Events), []webhook.Event{webhook.EventPush, webhook.EventRepository})
}

// Suspected bug: when no event has to be removed, UpdateWebhook passes an
// empty list to DeleteWebhookEventsByID, and sqlx rejects an empty IN clause,
// so a plain update that keeps the same events fails.
func TestUpdateWebhookWithSameEvents(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	id := e.createHook(t, whURL, webhook.EventPush)

	is.NoErr(e.be.UpdateWebhook(e.ctx, e.repo, id, whURL, webhook.ContentTypeJSON, "",
		[]webhook.Event{webhook.EventPush}, true))
}

// Suspected bug: a plain add (nothing removed) hits the same empty IN clause.
func TestUpdateWebhookAddsEventOnly(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	id := e.createHook(t, whURL, webhook.EventPush)

	is.NoErr(e.be.UpdateWebhook(e.ctx, e.repo, id, whURL, webhook.ContentTypeJSON, "",
		[]webhook.Event{webhook.EventPush, webhook.EventRepository}, true))

	got, err := e.be.Webhook(e.ctx, e.repo, id)
	is.NoErr(err)
	is.Equal(sortedEvents(got.Events), []webhook.Event{webhook.EventPush, webhook.EventRepository})
}

// Suspected bug: CreateWebhook sanitizes the URL but UpdateWebhook does not,
// so the stored URL depends on which call wrote it.
func TestUpdateWebhookSanitizesURL(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	id := e.createHook(t, whURL, webhook.EventPush)

	is.NoErr(e.be.UpdateWebhook(e.ctx, e.repo, id, whURLOther+"\x1b[31m", webhook.ContentTypeJSON, "",
		[]webhook.Event{webhook.EventPush}, true))

	got, err := e.be.Webhook(e.ctx, e.repo, id)
	is.NoErr(err)
	is.Equal(got.URL, whURLOther)
}

func TestUpdateWebhookRejectsUnsafeURL(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	id := e.createHook(t, whURL, webhook.EventPush)

	err := e.be.UpdateWebhook(e.ctx, e.repo, id, "http://127.0.0.1/hook", webhook.ContentTypeJSON, "",
		[]webhook.Event{webhook.EventPush}, true)
	is.True(err != nil)

	got, err := e.be.Webhook(e.ctx, e.repo, id)
	is.NoErr(err)
	is.Equal(got.URL, whURL)
}

// Suspected bug: updating a webhook id that does not belong to the repo
// succeeds silently, because the store's UPDATE matches zero rows and nothing
// checks that.
func TestUpdateWebhookUnknownIDFails(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)

	err := e.be.UpdateWebhook(e.ctx, e.repo, 424242, whURL, webhook.ContentTypeJSON, "",
		[]webhook.Event{webhook.EventPush}, true)
	is.True(err != nil)
}

// Suspected bug: the event rewrite uses the URL-scoped webhook ID after a
// zero-row update, so updating another repo's webhook through this repo
// rewrites that webhook's events.
func TestUpdateWebhookCannotTouchOtherRepo(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	other, otherID := e.otherRepo(t)

	_ = e.be.UpdateWebhook(e.ctx, e.repo, otherID, whURL, webhook.ContentTypeForm, "pwned",
		[]webhook.Event{webhook.EventRepository}, false)

	got, err := e.be.Webhook(e.ctx, other, otherID)
	is.NoErr(err)
	is.Equal(got.URL, whURLOther)
	is.Equal(got.ContentType, webhook.ContentTypeJSON)
	is.Equal(got.Secret, "")
	is.True(got.Active)
	is.Equal(got.Events, []webhook.Event{webhook.EventPush})
}

func TestWebhookGetScopedToRepo(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	other, otherID := e.otherRepo(t)
	_ = other

	_, err := e.be.Webhook(e.ctx, e.repo, otherID)
	is.True(err != nil)

	_, err = e.be.Webhook(e.ctx, e.repo, 424242)
	is.True(err != nil)
}

func TestDeleteWebhookScopedToRepo(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	other, otherID := e.otherRepo(t)

	is.True(e.be.DeleteWebhook(e.ctx, e.repo, otherID) != nil)
	is.True(e.be.DeleteWebhook(e.ctx, e.repo, 424242) != nil)

	hooks, err := e.be.ListWebhooks(e.ctx, other)
	is.NoErr(err)
	is.Equal(len(hooks), 1)
}

// Suspected bug: on SQLite the schema's ON DELETE CASCADE is not enforced
// because foreign keys are only switched on during migration 0001, so
// deleting a webhook leaves its events behind.
func TestDeleteWebhookRemovesEvents(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	id := e.createHook(t, whURL, webhook.EventPush, webhook.EventRepository)

	is.NoErr(e.be.DeleteWebhook(e.ctx, e.repo, id))

	hooks, err := e.be.ListWebhooks(e.ctx, e.repo)
	is.NoErr(err)
	is.Equal(len(hooks), 0)

	events, err := e.be.store.GetWebhookEventsByWebhookID(e.ctx, e.be.db, id)
	is.NoErr(err)
	is.Equal(len(events), 0)
}

// seedDelivery stores a delivery row directly, so the read paths can be
// tested without sending anything over the network.
func (e whEnv) seedDelivery(t *testing.T, webhookID int64, url string, body string) uuid.UUID {
	t.Helper()
	is := is.New(t)
	id := uuid.New()
	is.NoErr(e.be.db.TransactionContext(e.ctx, func(tx *db.Tx) error {
		return e.be.store.CreateWebhookDelivery(e.ctx, tx, id, webhookID, int(webhook.EventPush), url, "POST",
			nil, "X-Test: 1\n", body, 200, "", "ok")
	}))
	return id
}

func TestListAndReadDeliveries(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	id := e.createHook(t, whURL, webhook.EventPush)
	delID := e.seedDelivery(t, id, whURL, `{"ref":"main"}`)

	list, err := e.be.ListWebhookDeliveries(e.ctx, e.repo, id)
	is.NoErr(err)
	is.Equal(len(list), 1)
	is.Equal(list[0].ID, delID)
	is.Equal(list[0].Event, webhook.EventPush)

	d, err := e.be.WebhookDelivery(e.ctx, e.repo, id, delID)
	is.NoErr(err)
	is.Equal(d.ID, delID)
	is.Equal(d.RequestBody, `{"ref":"main"}`)
	is.Equal(d.Event, webhook.EventPush)
}

func TestDeliveriesScopedToRepo(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	other, otherID := e.otherRepo(t)
	delID := e.seedDelivery(t, otherID, whURLOther, `{}`)

	_, err := e.be.ListWebhookDeliveries(e.ctx, e.repo, otherID)
	is.True(err != nil)

	_, err = e.be.WebhookDelivery(e.ctx, e.repo, otherID, delID)
	is.True(err != nil)

	// The owning repo still sees it.
	d, err := e.be.WebhookDelivery(e.ctx, other, otherID, delID)
	is.NoErr(err)
	is.Equal(d.ID, delID)
}

func TestDeliveriesUnknown(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	id := e.createHook(t, whURL, webhook.EventPush)

	_, err := e.be.ListWebhookDeliveries(e.ctx, e.repo, 424242)
	is.True(err != nil)

	_, err = e.be.WebhookDelivery(e.ctx, e.repo, id, uuid.New())
	is.True(err != nil)
}

// Redelivery to a private address fails at dial time. The failure is recorded
// as a new delivery, and the call itself returns nil.
func TestRedeliverRecordsNewDelivery(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	id := e.createHook(t, whURL, webhook.EventPush)
	// Point the stored hook at a private address without validation, so the
	// redelivery reaches the SSRF-protected client and fails there.
	is.NoErr(e.be.db.TransactionContext(e.ctx, func(tx *db.Tx) error {
		return e.be.store.UpdateWebhookByID(e.ctx, tx, e.repo.ID(), id, "http://127.0.0.1:1/", "", int(webhook.ContentTypeJSON), true)
	}))
	delID := e.seedDelivery(t, id, "http://127.0.0.1:1/", `{"ref":"main"}`)

	is.NoErr(e.be.RedeliverWebhookDelivery(e.ctx, e.repo, id, delID))

	list, err := e.be.ListWebhookDeliveries(e.ctx, e.repo, id)
	is.NoErr(err)
	is.Equal(len(list), 2)
}

// Suspected bug: a form-encoded webhook cannot be redelivered, because the
// stored JSON body is passed as a raw message and form encoding of it fails.
func TestRedeliverFormWebhook(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	id := e.createHook(t, whURL, webhook.EventPush)
	is.NoErr(e.be.db.TransactionContext(e.ctx, func(tx *db.Tx) error {
		return e.be.store.UpdateWebhookByID(e.ctx, tx, e.repo.ID(), id, "http://127.0.0.1:1/", "", int(webhook.ContentTypeForm), true)
	}))
	const body = "n=1&ref=main"
	delID := e.seedDelivery(t, id, "http://127.0.0.1:1/", body)

	is.NoErr(e.be.RedeliverWebhookDelivery(e.ctx, e.repo, id, delID))

	// The redelivery is recorded with the same form-encoded body.
	list, err := e.be.ListWebhookDeliveries(e.ctx, e.repo, id)
	is.NoErr(err)
	is.Equal(len(list), 2)
	for _, d := range list {
		if d.ID == delID {
			continue
		}
		rd, err := e.be.WebhookDelivery(e.ctx, e.repo, id, d.ID)
		is.NoErr(err)
		is.Equal(rd.RequestBody, body)
	}
}

func TestRedeliverFailures(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	id := e.createHook(t, whURL, webhook.EventPush)
	bad := e.seedDelivery(t, id, whURL, `not json`)
	good := e.seedDelivery(t, id, whURL, `{}`)
	other, otherID := e.otherRepo(t)
	otherDel := e.seedDelivery(t, otherID, whURLOther, `{}`)
	_ = other

	// Unknown delivery and unknown webhook are both reported as not found.
	is.True(errors.Is(e.be.RedeliverWebhookDelivery(e.ctx, e.repo, id, uuid.New()), db.ErrRecordNotFound))
	is.True(errors.Is(e.be.RedeliverWebhookDelivery(e.ctx, e.repo, 424242, good), db.ErrRecordNotFound))
	// Delivery exists, but its body is not valid JSON.
	is.True(e.be.RedeliverWebhookDelivery(e.ctx, e.repo, id, bad) != nil)
	// A webhook owned by another repo cannot be redelivered through this one.
	is.True(e.be.RedeliverWebhookDelivery(e.ctx, e.repo, otherID, otherDel) != nil)
}

// Error paths inside transactions are reached by removing the table the
// operation reads or writes. The database is per test, so this is safe.
func TestErrorPathsWhenStoreFails(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	id := e.createHook(t, whURL, webhook.EventPush)
	delID := e.seedDelivery(t, id, whURL, `{}`)

	drop := func(table string) {
		t.Helper()
		_, err := e.be.db.ExecContext(e.ctx, "DROP TABLE "+table)
		is.NoErr(err)
	}

	// Events table gone: reads and writes of events fail.
	drop("webhook_events")
	_, err := e.be.ListWebhooks(e.ctx, e.repo)
	is.True(err != nil)
	_, err = e.be.Webhook(e.ctx, e.repo, id)
	is.True(err != nil)
	is.True(e.be.UpdateWebhook(e.ctx, e.repo, id, whURL, webhook.ContentTypeJSON, "", []webhook.Event{webhook.EventRepository}, true) != nil)
	is.True(e.be.CreateWebhook(e.ctx, e.repo, whURLOther, webhook.ContentTypeJSON, "", []webhook.Event{webhook.EventPush}, true) != nil)

	// Deliveries table gone.
	drop("webhook_deliveries")
	_, err = e.be.ListWebhookDeliveries(e.ctx, e.repo, id)
	is.True(err != nil)
	_, err = e.be.WebhookDelivery(e.ctx, e.repo, id, delID)
	is.True(err != nil)
	is.True(e.be.RedeliverWebhookDelivery(e.ctx, e.repo, id, delID) != nil)

	// Webhooks table gone: every entry point fails.
	drop("webhooks")
	is.True(e.be.CreateWebhook(e.ctx, e.repo, whURL, webhook.ContentTypeJSON, "", []webhook.Event{webhook.EventPush}, true) != nil)
	_, err = e.be.ListWebhooks(e.ctx, e.repo)
	is.True(err != nil)
	is.True(e.be.UpdateWebhook(e.ctx, e.repo, id, whURL, webhook.ContentTypeJSON, "", []webhook.Event{webhook.EventPush}, true) != nil)
	is.True(e.be.DeleteWebhook(e.ctx, e.repo, id) != nil)
}

// A URL already used by another webhook of the same repository violates the
// unique (repo_id, url) constraint, so the update fails and changes nothing.
func TestUpdateWebhookFailsOnDuplicateURL(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	a := e.createHook(t, whURL, webhook.EventPush)
	b := e.createHook(t, whURLOther, webhook.EventPush)

	is.True(e.be.UpdateWebhook(e.ctx, e.repo, b, whURL, webhook.ContentTypeJSON, "",
		[]webhook.Event{webhook.EventRepository}, true) != nil)

	gotB, err := e.be.Webhook(e.ctx, e.repo, b)
	is.NoErr(err)
	is.Equal(gotB.URL, whURLOther)
	is.Equal(gotB.Events, []webhook.Event{webhook.EventPush})
	gotA, err := e.be.Webhook(e.ctx, e.repo, a)
	is.NoErr(err)
	is.Equal(gotA.URL, whURL)
}

// execTrigger makes writes to table fail with a database error. Triggers live
// only in the per-test database.
func (e whEnv) execTrigger(t *testing.T, name, timing, op, table string) {
	t.Helper()
	is := is.New(t)
	_, err := e.be.db.ExecContext(e.ctx,
		"CREATE TRIGGER "+name+" "+timing+" "+op+" ON "+table+" BEGIN SELECT RAISE(ABORT, 'forced failure'); END;")
	is.NoErr(err)
}

func (e whEnv) dropTrigger(t *testing.T, name string) {
	t.Helper()
	is := is.New(t)
	_, err := e.be.db.ExecContext(e.ctx, "DROP TRIGGER "+name)
	is.NoErr(err)
}

// A failure while removing an event must roll back the whole update, so the
// URL and the remaining events stay as they were.
func TestUpdateWebhookRollsBackOnEventDeleteFailure(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	id := e.createHook(t, whURL, webhook.EventPush, webhook.EventCollaborator)

	e.execTrigger(t, "fail_event_delete", "BEFORE", "DELETE", "webhook_events")
	is.True(e.be.UpdateWebhook(e.ctx, e.repo, id, whURLOther, webhook.ContentTypeForm, "x",
		[]webhook.Event{webhook.EventPush}, false) != nil)
	e.dropTrigger(t, "fail_event_delete")

	got, err := e.be.Webhook(e.ctx, e.repo, id)
	is.NoErr(err)
	is.Equal(got.URL, whURL)
	is.Equal(got.ContentType, webhook.ContentTypeJSON)
	is.Equal(sortedEvents(got.Events), []webhook.Event{webhook.EventCollaborator, webhook.EventPush})
}

// A failure while adding an event must roll back the whole update.
func TestUpdateWebhookRollsBackOnEventInsertFailure(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	id := e.createHook(t, whURL, webhook.EventPush)

	e.execTrigger(t, "fail_event_insert", "BEFORE", "INSERT", "webhook_events")
	is.True(e.be.UpdateWebhook(e.ctx, e.repo, id, whURLOther, webhook.ContentTypeJSON, "",
		[]webhook.Event{webhook.EventPush, webhook.EventRepository}, true) != nil)
	e.dropTrigger(t, "fail_event_insert")

	got, err := e.be.Webhook(e.ctx, e.repo, id)
	is.NoErr(err)
	is.Equal(got.URL, whURL)
	is.Equal(got.Events, []webhook.Event{webhook.EventPush})
}

// Each dependent-row delete in DeleteWebhook can fail. A failure must leave
// the webhook, its events and its deliveries in place.
func TestDeleteWebhookRollsBackOnStoreFailure(t *testing.T) {
	for _, table := range []string{"webhook_deliveries", "webhook_events", "webhooks"} {
		t.Run(table, func(t *testing.T) {
			is := is.New(t)
			e := newWebhookEnv(t)
			id := e.createHook(t, whURL, webhook.EventPush)
			delID := e.seedDelivery(t, id, whURL, `{}`)

			e.execTrigger(t, "fail_delete", "BEFORE", "DELETE", table)
			is.True(e.be.DeleteWebhook(e.ctx, e.repo, id) != nil)
			e.dropTrigger(t, "fail_delete")

			_, err := e.be.Webhook(e.ctx, e.repo, id)
			is.NoErr(err)
			events, err := e.be.store.GetWebhookEventsByWebhookID(e.ctx, e.be.db, id)
			is.NoErr(err)
			is.Equal(len(events), 1)
			_, err = e.be.WebhookDelivery(e.ctx, e.repo, id, delID)
			is.NoErr(err)
		})
	}
}

// A failure while creating events fails the whole create.
func TestCreateWebhookRollsBackOnEventInsertFailure(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)

	e.execTrigger(t, "fail_create_event", "BEFORE", "INSERT", "webhook_events")
	is.True(e.be.CreateWebhook(e.ctx, e.repo, whURL, webhook.ContentTypeJSON, "", []webhook.Event{webhook.EventPush}, true) != nil)
	e.dropTrigger(t, "fail_create_event")

	hooks, err := e.be.ListWebhooks(e.ctx, e.repo)
	is.NoErr(err)
	is.Equal(len(hooks), 0)
}

// A stored form body that is not a valid query string cannot be redelivered.
func TestRedeliverMalformedFormBody(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	id := e.createHook(t, whURL, webhook.EventPush)
	is.NoErr(e.be.db.TransactionContext(e.ctx, func(tx *db.Tx) error {
		return e.be.store.UpdateWebhookByID(e.ctx, tx, e.repo.ID(), id, "http://127.0.0.1:1/", "", int(webhook.ContentTypeForm), true)
	}))
	delID := e.seedDelivery(t, id, "http://127.0.0.1:1/", "%zz=1")

	is.True(e.be.RedeliverWebhookDelivery(e.ctx, e.repo, id, delID) != nil)
}
