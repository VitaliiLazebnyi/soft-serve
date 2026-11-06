package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/dbtest"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/matryer/is"
)

// newWebhookTestDB opens a migrated database for one test: Postgres when
// SOFT_SERVE_DB_DRIVER is "postgres", SQLite otherwise.
func newWebhookTestDB(t *testing.T) (context.Context, *db.DB) {
	t.Helper()
	is := is.New(t)
	ctx := context.Background()
	dbx, err := dbtest.Open(ctx, t)
	is.NoErr(err)
	cfg := config.DefaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.DB.Driver = dbx.DriverName()
	ctx = config.WithContext(ctx, cfg)
	if isSqlite(dbx) {
		// Enforce foreign keys, as the server's default data source does.
		// One connection, so the pragma holds for every statement.
		dbx.SetMaxOpenConns(1)
		_, err = dbx.ExecContext(ctx, `PRAGMA foreign_keys = ON;`)
		is.NoErr(err)
	}
	is.NoErr(migrate.Migrate(ctx, dbx))

	// Foreign keys are enforced, so the webhook tables need real parents: one
	// user and the repos 7 and 8 that the tests use.
	_, err = dbx.ExecContext(ctx, `INSERT INTO users (id, username, admin, updated_at) VALUES (100, 'owner', false, CURRENT_TIMESTAMP);`)
	is.NoErr(err)
	_, err = dbx.ExecContext(ctx, `INSERT INTO repos (id, name, project_name, description, private, mirror, hidden, user_id, updated_at)
		VALUES (7, 'repo7', 'repo7', '', false, false, false, 100, CURRENT_TIMESTAMP),
		       (8, 'repo8', 'repo8', '', false, false, false, 100, CURRENT_TIMESTAMP);`)
	is.NoErr(err)
	return ctx, dbx
}

func isSqlite(dbx *db.DB) bool {
	d := dbx.DriverName()
	return d == "sqlite" || d == "sqlite3"
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
	_, checks["LockAndCountWebhooks"] = s.LockAndCountWebhooks(ctx, dbx, 7)
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
	checks["PruneWebhookDeliveries"] = s.PruneWebhookDeliveries(ctx, dbx, id, 1)

	for name, err := range checks {
		if err == nil {
			t.Errorf("%s: no error on closed database", name)
		}
	}
}

// A webhook deleted after the existence check but before the insert makes the
// insert violate the foreign key. That is the same outcome as a webhook that
// is already gone: nothing is written and no error is returned. A trigger
// deletes the webhook in that window.
func TestCreateWebhookDeliveryForeignKeyViolationIsSkipped(t *testing.T) {
	is := is.New(t)
	ctx, dbx := newWebhookTestDB(t)
	s := &webhookStore{}
	id, err := s.CreateWebhook(ctx, dbx, 7, "https://93.184.216.34/a", "", 1, true)
	is.NoErr(err)
	triggers := []string{`CREATE TRIGGER vanish BEFORE INSERT ON webhook_deliveries
		BEGIN DELETE FROM webhooks WHERE id = NEW.webhook_id; END;`}
	if !isSqlite(dbx) {
		triggers = []string{
			`CREATE FUNCTION vanish() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN DELETE FROM webhooks WHERE id = NEW.webhook_id; RETURN NEW; END $$;`,
			`CREATE TRIGGER vanish BEFORE INSERT ON webhook_deliveries
			FOR EACH ROW EXECUTE FUNCTION vanish();`,
		}
	}
	for _, q := range triggers {
		_, err = dbx.ExecContext(ctx, q)
		is.NoErr(err)
	}

	is.NoErr(s.CreateWebhookDelivery(ctx, dbx, uuid.New(), id, 1, "https://93.184.216.34/a", "POST", nil, "", "{}", 200, "", ""))

	var n int
	is.NoErr(dbx.GetContext(ctx, &n, "SELECT COUNT(*) FROM webhook_deliveries"))
	is.Equal(n, 0)
}

// Both drivers' forms of a foreign key violation are recognized, and nothing
// else is.
func TestIsForeignKeyViolation(t *testing.T) {
	is := is.New(t)
	is.True(isForeignKeyViolation(fmt.Errorf("wrapped: %w", &pq.Error{Code: "23503"})))
	is.True(!isForeignKeyViolation(&pq.Error{Code: "23505"}))
	is.True(!isForeignKeyViolation(errors.New("FOREIGN KEY constraint failed")))
	is.True(!isForeignKeyViolation(nil))

	// A real foreign key violation from the driver under test.
	ctx, dbx := newWebhookTestDB(t)
	_, err := dbx.ExecContext(ctx, `INSERT INTO webhook_events (webhook_id, event) VALUES (424242, 1)`)
	is.True(err != nil)
	is.True(isForeignKeyViolation(err))
}

// Webhooks and their events are listed in creation order. Without ORDER BY,
// SQLite returns webhooks in URL order through the unique (repo_id, url)
// index, so "n=10" came before "n=9".
func TestWebhookListsAreOrderedByID(t *testing.T) {
	is := is.New(t)
	ctx, dbx := newWebhookTestDB(t)
	s := &webhookStore{}
	var ids []int64
	for _, n := range []int{9, 10, 1} {
		id, err := s.CreateWebhook(ctx, dbx, 7, fmt.Sprintf("https://93.184.216.34/?n=%d", n), "", 1, true)
		is.NoErr(err)
		is.NoErr(s.CreateWebhookEvents(ctx, dbx, id, []int{3, 1, 2}))
		ids = append(ids, id)
	}

	list, err := s.GetWebhooksByRepoID(ctx, dbx, 7)
	is.NoErr(err)
	byEvent, err := s.GetWebhooksByRepoIDWhereEvent(ctx, dbx, 7, []int{1})
	is.NoErr(err)
	for _, got := range [][]int64{idsOf(list), idsOf(byEvent)} {
		is.Equal(got, ids)
	}

	events, err := s.GetWebhookEventsByWebhookID(ctx, dbx, ids[0])
	is.NoErr(err)
	for i := 1; i < len(events); i++ {
		is.True(events[i-1].ID < events[i].ID)
	}
}

// Deliveries recorded within the same second list in the order they were
// inserted, oldest first. created_at has one-second resolution and IDs are
// random UUIDs, so neither can order them.
func TestWebhookStoreDeliveriesOrder(t *testing.T) {
	is := is.New(t)
	ctx, dbx := newWebhookTestDB(t)
	s := &webhookStore{}

	hook, err := s.CreateWebhook(ctx, dbx, 7, "https://93.184.216.34/a", "", 0, true)
	is.NoErr(err)
	other, err := s.CreateWebhook(ctx, dbx, 7, "https://93.184.216.35/b", "", 0, true)
	is.NoErr(err)

	insert := func(webhookID int64) uuid.UUID {
		id := uuid.New()
		is.NoErr(s.CreateWebhookDelivery(ctx, dbx, id, webhookID, 1, "https://93.184.216.34/a", "POST", nil, "", "{}", 200, "", ""))
		return id
	}

	var want []uuid.UUID
	for range 20 {
		want = append(want, insert(hook))
		insert(other) // interleaved deliveries of another webhook
	}
	// Pin every row to the same second.
	_, err = dbx.ExecContext(ctx, `UPDATE webhook_deliveries SET created_at = '2024-01-01 00:00:00';`)
	is.NoErr(err)

	check := func() {
		t.Helper()
		list, err := s.ListWebhookDeliveriesByWebhookID(ctx, dbx, hook)
		is.NoErr(err)
		all, err := s.GetWebhookDeliveriesByWebhookID(ctx, dbx, hook)
		is.NoErr(err)
		for _, got := range [][]models.WebhookDelivery{list, all} {
			ids := make([]uuid.UUID, len(got))
			for i, d := range got {
				ids[i] = d.ID
			}
			is.Equal(ids, want)
		}
	}
	check()

	// Deleting the newest delivery does not let a later one sort before
	// older ones.
	is.NoErr(s.DeleteWebhookDeliveryByID(ctx, dbx, hook, want[len(want)-1]))
	want = want[:len(want)-1]
	want = append(want, insert(hook))
	check()

	// The listing follows seq, not the physical row order: a row stored last
	// with the lowest seq lists first.
	first := uuid.New()
	_, err = dbx.ExecContext(ctx, dbx.Rebind(`INSERT INTO webhook_deliveries (id, webhook_id, event, request_url, request_method, request_headers, request_body, response_status, response_headers, response_body, seq)
		VALUES (?, ?, 1, '', 'POST', '', '{}', 200, '', '', 0);`), first, hook)
	is.NoErr(err)
	want = append([]uuid.UUID{first}, want...)
	check()
}

// Pruning keeps the most recently recorded deliveries of one webhook and
// leaves the deliveries of other webhooks alone. Recording order is what
// counts: created_at and the IDs are set to run the other way.
func TestPruneWebhookDeliveriesKeepsNewest(t *testing.T) {
	is := is.New(t)
	ctx, dbx := newWebhookTestDB(t)
	s := &webhookStore{}
	hook, err := s.CreateWebhook(ctx, dbx, 7, "https://93.184.216.34/a", "", 1, true)
	is.NoErr(err)
	other, err := s.CreateWebhook(ctx, dbx, 7, "https://93.184.216.35/b", "", 1, true)
	is.NoErr(err)

	// Eight deliveries, recorded with descending IDs and created_at, and
	// interleaved with deliveries of another webhook.
	ids := make([]uuid.UUID, 8)
	for i := range ids {
		ids[i] = uuid.MustParse(fmt.Sprintf("%08d-0000-1000-8000-000000000000", 99-i))
		is.NoErr(s.CreateWebhookDelivery(ctx, dbx, ids[i], hook, 1, "u", "POST", nil, "", "{}", 200, "", ""))
		_, err := dbx.ExecContext(ctx, `UPDATE webhook_deliveries SET created_at = ? WHERE id = ?;`,
			fmt.Sprintf("2024-01-01 00:00:%02d", 59-i), ids[i])
		is.NoErr(err)
		if i < 3 {
			is.NoErr(s.CreateWebhookDelivery(ctx, dbx, uuid.New(), other, 1, "u", "POST", nil, "", "{}", 200, "", ""))
		}
	}

	keptIDs := func() []uuid.UUID {
		t.Helper()
		all, err := s.GetWebhookDeliveriesByWebhookID(ctx, dbx, hook)
		is.NoErr(err)
		out := []uuid.UUID{}
		for _, d := range all {
			out = append(out, d.ID)
		}
		return out
	}

	is.NoErr(s.PruneWebhookDeliveries(ctx, dbx, hook, 5))
	is.Equal(keptIDs(), ids[3:]) // the three recorded first are gone

	others, err := s.GetWebhookDeliveriesByWebhookID(ctx, dbx, other)
	is.NoErr(err)
	is.Equal(len(others), 3)

	// Exactly as many deliveries as the limit, or fewer: nothing is deleted.
	is.NoErr(s.PruneWebhookDeliveries(ctx, dbx, hook, 5))
	is.Equal(keptIDs(), ids[3:])
	is.NoErr(s.PruneWebhookDeliveries(ctx, dbx, hook, 100))
	is.Equal(keptIDs(), ids[3:])

	is.NoErr(s.PruneWebhookDeliveries(ctx, dbx, hook, 0))
	is.Equal(keptIDs(), []uuid.UUID{})
	others, err = s.GetWebhookDeliveriesByWebhookID(ctx, dbx, other)
	is.NoErr(err)
	is.Equal(len(others), 3)
}

// The prune runs on every recorded delivery, so it must not scan the
// deliveries table. On SQLite both its lookups use the (webhook_id, seq)
// index.
func TestPruneWebhookDeliveriesUsesIndex(t *testing.T) {
	is := is.New(t)
	ctx, dbx := newWebhookTestDB(t)
	var plan []struct {
		ID      int    `db:"id"`
		Parent  int    `db:"parent"`
		NotUsed int    `db:"notused"`
		Detail  string `db:"detail"`
	}
	is.NoErr(dbx.SelectContext(ctx, &plan, "EXPLAIN QUERY PLAN "+pruneWebhookDeliveriesQuery, 1, 1, 100))
	is.True(len(plan) > 0)
	for _, p := range plan {
		t.Log(p.Detail)
		is.True(!strings.HasPrefix(p.Detail, "SCAN"))       // no full scan
		is.True(!strings.Contains(p.Detail, "TEMP B-TREE")) // no sort
	}
}

func idsOf(whs []models.Webhook) []int64 {
	out := make([]int64, len(whs))
	for i, w := range whs {
		out[i] = w.ID
	}
	return out
}

// LockAndCountWebhooks counts only the given repository's webhooks.
func TestLockAndCountWebhooks(t *testing.T) {
	is := is.New(t)
	ctx, dbx := newWebhookTestDB(t)
	s := &webhookStore{}
	n, err := s.LockAndCountWebhooks(ctx, dbx, 7)
	is.NoErr(err)
	is.Equal(n, 0)
	for i := range 3 {
		_, err := s.CreateWebhook(ctx, dbx, 7, fmt.Sprintf("https://93.184.216.34/%d", i), "", 1, true)
		is.NoErr(err)
	}
	_, err = s.CreateWebhook(ctx, dbx, 8, "https://93.184.216.35/x", "", 1, true)
	is.NoErr(err)
	n, err = s.LockAndCountWebhooks(ctx, dbx, 7)
	is.NoErr(err)
	is.Equal(n, 3)
}
