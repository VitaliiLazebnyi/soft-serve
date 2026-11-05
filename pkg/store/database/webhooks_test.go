package database

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/google/uuid"
	"github.com/matryer/is"
)

// newWebhookTestDB opens a migrated SQLite database for one test.
func newWebhookTestDB(t *testing.T) (context.Context, *db.DB) {
	t.Helper()
	is := is.New(t)
	ctx := context.Background()
	dp := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.DataPath = dp
	ctx = config.WithContext(ctx, cfg)
	dbx, err := db.Open(ctx, "sqlite", dp+"/test.db?_pragma=foreign_keys(1)")
	is.NoErr(err)
	t.Cleanup(func() { dbx.Close() }) //nolint:errcheck
	is.NoErr(migrate.Migrate(ctx, dbx))

	// Foreign keys are enforced, so the webhook tables need real parents: one
	// user and the repos 7 and 8 that the tests use.
	_, err = dbx.ExecContext(ctx, `INSERT INTO users (id, username, admin, updated_at) VALUES (100, 'owner', 0, CURRENT_TIMESTAMP);`)
	is.NoErr(err)
	_, err = dbx.ExecContext(ctx, `INSERT INTO repos (id, name, project_name, description, private, mirror, hidden, user_id, updated_at)
		VALUES (7, 'repo7', 'repo7', '', 0, 0, 0, 100, CURRENT_TIMESTAMP),
		       (8, 'repo8', 'repo8', '', 0, 0, 0, 100, CURRENT_TIMESTAMP);`)
	is.NoErr(err)
	return ctx, dbx
}
func TestWebhookStoreRoundTrip(t *testing.T) {
	is := is.New(t)
	ctx, dbx := newWebhookTestDB(t)
	s := &webhookStore{}

	id, err := s.CreateWebhook(ctx, dbx, 7, "https://93.184.216.34/a", "sec", 1, true)
	is.NoErr(err)

	wh, err := s.GetWebhookByID(ctx, dbx, 7, id)
	is.NoErr(err)
	is.Equal(wh.URL, "https://93.184.216.34/a")
	is.Equal(wh.Secret, "sec")
	is.Equal(wh.ContentType, 1)
	is.True(wh.Active)
	is.Equal(wh.RepoID, int64(7))

	// Scoped by repo: the same ID under another repo is not found.
	_, err = s.GetWebhookByID(ctx, dbx, 8, id)
	is.True(errors.Is(err, sql.ErrNoRows))

	is.NoErr(s.UpdateWebhookByID(ctx, dbx, 7, id, "https://93.184.216.35/b", "new", 0, false))
	wh, err = s.GetWebhookByID(ctx, dbx, 7, id)
	is.NoErr(err)
	is.Equal(wh.URL, "https://93.184.216.35/b")
	is.Equal(wh.Secret, "new")
	is.Equal(wh.ContentType, 0)
	is.True(!wh.Active)

	// An update scoped to another repo changes nothing and reports no error.
	is.NoErr(s.UpdateWebhookByID(ctx, dbx, 8, id, "https://93.184.216.36/c", "x", 0, true))
	wh, err = s.GetWebhookByID(ctx, dbx, 7, id)
	is.NoErr(err)
	is.Equal(wh.URL, "https://93.184.216.35/b")

	list, err := s.GetWebhooksByRepoID(ctx, dbx, 7)
	is.NoErr(err)
	is.Equal(len(list), 1)
	none, err := s.GetWebhooksByRepoID(ctx, dbx, 99)
	is.NoErr(err)
	is.Equal(len(none), 0)

	is.NoErr(s.DeleteWebhookForRepoByID(ctx, dbx, 8, id)) // wrong repo: no-op
	_, err = s.GetWebhookByID(ctx, dbx, 7, id)
	is.NoErr(err)
	is.NoErr(s.DeleteWebhookForRepoByID(ctx, dbx, 7, id))
	_, err = s.GetWebhookByID(ctx, dbx, 7, id)
	is.True(errors.Is(err, sql.ErrNoRows))

	id2, err := s.CreateWebhook(ctx, dbx, 7, "https://93.184.216.37/d", "", 0, true)
	is.NoErr(err)
	is.NoErr(s.DeleteWebhookByID(ctx, dbx, id2))
	_, err = s.GetWebhookByID(ctx, dbx, 7, id2)
	is.True(errors.Is(err, sql.ErrNoRows))
}

func TestWebhookStoreEvents(t *testing.T) {
	is := is.New(t)
	ctx, dbx := newWebhookTestDB(t)
	s := &webhookStore{}

	hook, err := s.CreateWebhook(ctx, dbx, 7, "https://93.184.216.34/a", "", 0, true)
	is.NoErr(err)
	other, err := s.CreateWebhook(ctx, dbx, 7, "https://93.184.216.35/b", "", 0, true)
	is.NoErr(err)

	is.NoErr(s.CreateWebhookEvents(ctx, dbx, hook, []int{4, 5}))
	is.NoErr(s.CreateWebhookEvents(ctx, dbx, other, []int{6}))
	is.NoErr(s.CreateWebhookEvents(ctx, dbx, hook, nil)) // nothing to add

	evs, err := s.GetWebhookEventsByWebhookID(ctx, dbx, hook)
	is.NoErr(err)
	is.Equal(len(evs), 2)

	ev, err := s.GetWebhookEventByID(ctx, dbx, evs[0].ID)
	is.NoErr(err)
	is.Equal(ev.WebhookID, hook)

	_, err = s.GetWebhookEventByID(ctx, dbx, 424242)
	is.True(errors.Is(err, sql.ErrNoRows))

	// Filter by event: push (4) matches hook only; an event with no match
	// returns nothing; an empty filter returns nothing and no error.
	whs, err := s.GetWebhooksByRepoIDWhereEvent(ctx, dbx, 7, []int{4})
	is.NoErr(err)
	is.Equal(len(whs), 1)
	is.Equal(whs[0].ID, hook)

	whs, err = s.GetWebhooksByRepoIDWhereEvent(ctx, dbx, 7, []int{99})
	is.NoErr(err)
	is.Equal(len(whs), 0)

	whs, err = s.GetWebhooksByRepoIDWhereEvent(ctx, dbx, 7, nil)
	is.NoErr(err)
	is.Equal(len(whs), 0)

	// Deleting no IDs is a no-op; deleting some removes only those.
	is.NoErr(s.DeleteWebhookEventsByID(ctx, dbx, nil))
	evs, err = s.GetWebhookEventsByWebhookID(ctx, dbx, hook)
	is.NoErr(err)
	is.Equal(len(evs), 2)

	is.NoErr(s.DeleteWebhookEventsByID(ctx, dbx, []int64{evs[0].ID}))
	evs, err = s.GetWebhookEventsByWebhookID(ctx, dbx, hook)
	is.NoErr(err)
	is.Equal(len(evs), 1)

	// Remove everything for a webhook; the other webhook's events stay.
	is.NoErr(s.DeleteWebhookEventsByWebhookID(ctx, dbx, hook))
	evs, err = s.GetWebhookEventsByWebhookID(ctx, dbx, hook)
	is.NoErr(err)
	is.Equal(len(evs), 0)
	evs, err = s.GetWebhookEventsByWebhookID(ctx, dbx, other)
	is.NoErr(err)
	is.Equal(len(evs), 1)
}

func TestWebhookStoreDeliveries(t *testing.T) {
	is := is.New(t)
	ctx, dbx := newWebhookTestDB(t)
	s := &webhookStore{}

	hook, err := s.CreateWebhook(ctx, dbx, 7, "https://93.184.216.34/a", "", 0, true)
	is.NoErr(err)
	other, err := s.CreateWebhook(ctx, dbx, 7, "https://93.184.216.35/b", "", 0, true)
	is.NoErr(err)

	okID := uuid.New()
	failID := uuid.New()
	is.NoErr(s.CreateWebhookDelivery(ctx, dbx, okID, hook, 4, "https://93.184.216.34/a", "POST", nil, "H: 1", `{"a":1}`, 200, "R: 1", "ok"))
	is.NoErr(s.CreateWebhookDelivery(ctx, dbx, failID, hook, 5, "https://93.184.216.34/a", "POST", errors.New("dial refused"), "", "{}", 0, "", ""))
	is.NoErr(s.CreateWebhookDelivery(ctx, dbx, uuid.New(), other, 6, "https://93.184.216.35/b", "POST", nil, "", "{}", 204, "", ""))

	d, err := s.GetWebhookDeliveryByID(ctx, dbx, hook, okID)
	is.NoErr(err)
	is.Equal(d.RequestBody, `{"a":1}`)
	is.Equal(d.Event, 4)
	is.True(!d.RequestError.Valid) // no error recorded

	f, err := s.GetWebhookDeliveryByID(ctx, dbx, hook, failID)
	is.NoErr(err)
	is.True(f.RequestError.Valid)
	is.Equal(f.RequestError.String, "dial refused")

	// Scoped by webhook: another webhook's delivery ID is not found.
	_, err = s.GetWebhookDeliveryByID(ctx, dbx, other, okID)
	is.True(errors.Is(err, sql.ErrNoRows))

	all, err := s.GetWebhookDeliveriesByWebhookID(ctx, dbx, hook)
	is.NoErr(err)
	is.Equal(len(all), 2)

	list, err := s.ListWebhookDeliveriesByWebhookID(ctx, dbx, hook)
	is.NoErr(err)
	is.Equal(len(list), 2)

	is.NoErr(s.DeleteWebhookDeliveryByID(ctx, dbx, other, okID)) // wrong webhook: no-op
	_, err = s.GetWebhookDeliveryByID(ctx, dbx, hook, okID)
	is.NoErr(err)
	is.NoErr(s.DeleteWebhookDeliveryByID(ctx, dbx, hook, okID))
	_, err = s.GetWebhookDeliveryByID(ctx, dbx, hook, okID)
	is.True(errors.Is(err, sql.ErrNoRows))

	is.NoErr(s.DeleteWebhookDeliveriesByWebhookID(ctx, dbx, hook))
	all, err = s.GetWebhookDeliveriesByWebhookID(ctx, dbx, hook)
	is.NoErr(err)
	is.Equal(len(all), 0)
	all, err = s.GetWebhookDeliveriesByWebhookID(ctx, dbx, other)
	is.NoErr(err)
	is.Equal(len(all), 1)
}

// Once the database is closed every store call must return an error rather
// than succeed silently. This covers the error return of each method.
func TestWebhookStoreErrorsOnClosedDB(t *testing.T) {
	is := is.New(t)
	ctx, dbx := newWebhookTestDB(t)
	s := &webhookStore{}
	id, err := s.CreateWebhook(ctx, dbx, 7, "https://93.184.216.34/a", "", 0, true)
	is.NoErr(err)
	did := uuid.New()
	is.NoErr(s.CreateWebhookDelivery(ctx, dbx, did, id, 4, "u", "POST", nil, "", "", 0, "", ""))
	is.NoErr(dbx.Close())

	checks := map[string]error{}
	_, checks["GetWebhookByID"] = s.GetWebhookByID(ctx, dbx, 7, id)
	_, checks["GetWebhooksByRepoID"] = s.GetWebhooksByRepoID(ctx, dbx, 7)
	_, checks["GetWebhooksByRepoIDWhereEvent"] = s.GetWebhooksByRepoIDWhereEvent(ctx, dbx, 7, []int{4})
	_, checks["CreateWebhook"] = s.CreateWebhook(ctx, dbx, 7, "u", "", 0, true)
	checks["UpdateWebhookByID"] = s.UpdateWebhookByID(ctx, dbx, 7, id, "u", "", 0, true)
	checks["DeleteWebhookByID"] = s.DeleteWebhookByID(ctx, dbx, id)
	checks["DeleteWebhookForRepoByID"] = s.DeleteWebhookForRepoByID(ctx, dbx, 7, id)
	checks["DeleteWebhookEventsByWebhookID"] = s.DeleteWebhookEventsByWebhookID(ctx, dbx, id)
	checks["DeleteWebhookDeliveriesByWebhookID"] = s.DeleteWebhookDeliveriesByWebhookID(ctx, dbx, id)
	_, checks["GetWebhookEventByID"] = s.GetWebhookEventByID(ctx, dbx, 7)
	_, checks["GetWebhookEventsByWebhookID"] = s.GetWebhookEventsByWebhookID(ctx, dbx, id)
	checks["CreateWebhookEvents"] = s.CreateWebhookEvents(ctx, dbx, id, []int{4})
	checks["DeleteWebhookEventsByID"] = s.DeleteWebhookEventsByID(ctx, dbx, []int64{1})
	_, checks["GetWebhookDeliveryByID"] = s.GetWebhookDeliveryByID(ctx, dbx, id, did)
	_, checks["GetWebhookDeliveriesByWebhookID"] = s.GetWebhookDeliveriesByWebhookID(ctx, dbx, id)
	_, checks["ListWebhookDeliveriesByWebhookID"] = s.ListWebhookDeliveriesByWebhookID(ctx, dbx, id)
	checks["CreateWebhookDelivery"] = s.CreateWebhookDelivery(ctx, dbx, uuid.New(), id, 4, "u", "POST", nil, "", "", 0, "", "")
	checks["DeleteWebhookDeliveryByID"] = s.DeleteWebhookDeliveryByID(ctx, dbx, id, did)

	for name, err := range checks {
		if err == nil {
			t.Errorf("%s: no error on closed database", name)
		}
	}
}
