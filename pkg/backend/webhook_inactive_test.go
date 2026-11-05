package backend

import (
	"errors"
	"io"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/hooks"
	"github.com/charmbracelet/soft-serve/pkg/webhook"
	"github.com/matryer/is"
)

// setHookActive switches a webhook on or off in the store, keeping its
// loopback URL, which the backend's validating update would refuse.
func (e whEnv) setHookActive(t *testing.T, id int64, active bool) {
	t.Helper()
	is := is.New(t)
	is.NoErr(e.be.db.TransactionContext(e.ctx, func(tx *db.Tx) error {
		h, err := e.be.store.GetWebhookByID(e.ctx, tx, e.repo.ID(), id)
		if err != nil {
			return err
		}
		return e.be.store.UpdateWebhookByID(e.ctx, tx, e.repo.ID(), id, h.URL, h.Secret, h.ContentType, active)
	}))
}

// seedInactiveHook stores a second, inactive webhook on another loopback
// address, since a repository's webhook URLs are unique.
func (e whEnv) seedInactiveHook(t *testing.T, events ...webhook.Event) int64 {
	t.Helper()
	is := is.New(t)
	var id int64
	is.NoErr(e.be.db.TransactionContext(e.ctx, func(tx *db.Tx) error {
		var err error
		id, err = e.be.store.CreateWebhook(e.ctx, tx, e.repo.ID(), "http://127.0.0.1:2/", "", int(webhook.ContentTypeJSON), false)
		if err != nil {
			return err
		}
		evs := make([]int, len(events))
		for i, ev := range events {
			evs[i] = int(ev)
		}
		return e.be.store.CreateWebhookEvents(e.ctx, tx, id, evs)
	}))
	return id
}

// An inactive webhook is sent nothing, on the push path that sends in the
// caller and on the background path, while an active one next to it is.
func TestInactiveWebhookGetsNoEvents(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	t.Setenv("SOFT_SERVE_USERNAME", "admin")
	t.Setenv("SOFT_SERVE_PUBLIC_KEY", "")
	active := e.seedHook(t, webhook.EventBranchTagCreate, webhook.EventRepository)
	inactive := e.seedInactiveHook(t, webhook.EventBranchTagCreate, webhook.EventRepository)

	e.be.Update(e.ctx, io.Discard, io.Discard, e.repo.Name(), hooks.HookArg{
		OldSha: zeroSha, NewSha: someSha, RefName: "refs/heads/feature",
	})
	is.NoErr(e.be.RenameRepository(e.ctx, "repo", "renamed"))
	e.be.WaitForDeliveries(e.ctx)

	is.Equal(sortedEvents(e.deliveredEvents(t, active)), sortedEvents([]webhook.Event{webhook.EventBranchTagCreate, webhook.EventRepository}))
	is.Equal(len(e.deliveredEvents(t, inactive)), 0)

	// Inactive webhooks are still listed, so they can be managed.
	list, err := e.be.ListWebhooks(e.ctx, e.repo)
	is.NoErr(err)
	is.Equal(len(list), 2)
}

// Reactivating a webhook resumes its deliveries.
func TestReactivatedWebhookResumesDeliveries(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	id := e.seedHook(t, webhook.EventRepositoryVisibilityChange)

	e.setHookActive(t, id, false)
	is.NoErr(e.be.SetPrivate(e.ctx, "repo", true))
	// Wait for the queue to drain without closing it, so the next change is
	// still delivered.
	_, idle := e.be.deliveries.pending()
	<-idle
	is.Equal(len(e.deliveredEvents(t, id)), 0)

	e.setHookActive(t, id, true)
	is.NoErr(e.be.SetPrivate(e.ctx, "repo", false))
	e.be.WaitForDeliveries(e.ctx)
	is.Equal(e.deliveredEvents(t, id), []webhook.Event{webhook.EventRepositoryVisibilityChange})
}

// A webhook deactivated while its event waits in the queue is skipped when
// the event is sent.
func TestWebhookDeactivatedWhileQueuedIsSkipped(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	id := e.seedHook(t, webhook.EventRepository)
	unblock := blockDeliveries(t, e, 10)

	ev, err := webhook.NewRepositoryEvent(e.ctx, e.admin, e.repo, webhook.RepositoryEventActionRename)
	is.NoErr(err)
	subs, err := webhook.Subscribers(e.ctx, ev)
	is.NoErr(err)
	is.Equal(len(subs), 1)
	e.be.deliverAfterChange(e.ctx, subs, ev, true)

	e.setHookActive(t, id, false)
	unblock()
	e.be.WaitForDeliveries(e.ctx)

	is.Equal(len(e.deliveredEvents(t, id)), 0)
}

// Redelivering to an inactive webhook is refused and sends nothing. Once the
// webhook is active again it can be redelivered.
func TestRedeliverInactiveWebhookRefused(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	id := e.seedHook(t, webhook.EventPush)
	delID := e.seedDelivery(t, id, "http://127.0.0.1:1/", `{"ref":"main"}`)
	e.setHookActive(t, id, false)

	err := e.be.RedeliverWebhookDelivery(e.ctx, e.repo, id, delID)
	is.True(errors.Is(err, webhook.ErrWebhookInactive))
	is.Equal(len(e.deliveredEvents(t, id)), 1)

	e.setHookActive(t, id, true)
	is.NoErr(e.be.RedeliverWebhookDelivery(e.ctx, e.repo, id, delID))
	is.Equal(len(e.deliveredEvents(t, id)), 2)
}
