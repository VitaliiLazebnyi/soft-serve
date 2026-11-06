package migrate

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/log/v2"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/internal/test"
	"github.com/google/uuid"
)

// The sequence migration applies to a database that already holds
// deliveries, numbers them by created_at and then insertion order (SQLite) or
// id (Postgres), and later inserts continue the sequence without reusing
// numbers. Rolling it back keeps the rows, and migrating up again renumbers
// them the same way. The test runs on Postgres when SOFT_SERVE_DB_DRIVER is
// "postgres".
func TestWebhookDeliverySeqBackfill(t *testing.T) {
	ctx := config.WithContext(context.TODO(), config.DefaultConfig())
	dbx, err := test.Open(ctx, t)
	if err != nil {
		t.Fatal(err)
	}

	// Migrate up to, but not including, the sequence migration. Later
	// migrations stay out of the test so that Rollback undoes this one.
	all := migrations
	t.Cleanup(func() { migrations = all })
	migrations = all[:webhookDeliverySeqVersion-1]
	if err := Migrate(ctx, dbx); err != nil {
		t.Fatalf("Migrate() before seq => %v", err)
	}
	migrations = all[:webhookDeliverySeqVersion]

	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := dbx.ExecContext(ctx, dbx.Rebind(q), args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustExec(`INSERT INTO users (id, username, admin, updated_at) VALUES (100, 'u', false, CURRENT_TIMESTAMP)`)
	mustExec(`INSERT INTO repos (id, name, project_name, description, private, mirror, hidden, user_id, updated_at)
		VALUES (1, 'r', 'r', '', false, false, false, 100, CURRENT_TIMESTAMP)`)
	mustExec(`INSERT INTO webhooks (id, repo_id, url, secret, content_type, active, updated_at)
		VALUES (1, 1, 'https://example.com', '', 0, true, CURRENT_TIMESTAMP)`)

	insert := func(id uuid.UUID, createdAt string) {
		t.Helper()
		if createdAt == "" {
			mustExec(`INSERT INTO webhook_deliveries (id, webhook_id, event, request_url, request_method, request_headers, request_body, response_status, response_headers, response_body)
				VALUES (?, 1, 1, 'https://example.com', 'POST', '', '{}', 200, '', '')`, id)
			return
		}
		mustExec(`INSERT INTO webhook_deliveries (id, webhook_id, event, request_url, request_method, request_headers, request_body, response_status, response_headers, response_body, created_at)
			VALUES (?, 1, 1, 'https://example.com', 'POST', '', '{}', 200, '', '', ?)`, id, createdAt)
	}

	// Rows in insertion order. The first two share a second, and the third
	// is older than both. The first id sorts after the second, so SQLite
	// (insertion order) and Postgres (id order) break the tie differently.
	rows := []struct {
		id        uuid.UUID
		createdAt string
	}{
		{uuid.MustParse("ffffffff-ffff-4fff-bfff-ffffffffffff"), "2024-01-01 00:00:05"},
		{uuid.MustParse("00000000-0000-4000-8000-000000000001"), "2024-01-01 00:00:05"},
		{uuid.New(), "2024-01-01 00:00:01"},
	}
	for _, r := range rows {
		insert(r.id, r.createdAt)
	}
	backfilled := []uuid.UUID{rows[2].id, rows[0].id, rows[1].id}
	if dbx.DriverName() == "postgres" {
		backfilled = []uuid.UUID{rows[2].id, rows[1].id, rows[0].id}
	}

	type delivery struct {
		ID  uuid.UUID `db:"id"`
		Seq int64     `db:"seq"`
	}
	check := func(want []delivery) {
		t.Helper()
		var got []delivery
		if err := dbx.SelectContext(ctx, &got, `SELECT id, seq FROM webhook_deliveries ORDER BY seq`); err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want) {
			t.Fatalf("got %d rows %v, want %d %v", len(got), got, len(want), want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("row %d = %v, want %v", i, got[i], want[i])
			}
		}
	}
	numbered := func(ids ...uuid.UUID) []delivery {
		out := make([]delivery, len(ids))
		for i, id := range ids {
			out[i] = delivery{id, int64(i + 1)}
		}
		return out
	}

	for _, round := range []string{"first", "after rollback"} {
		if err := Migrate(ctx, dbx); err != nil {
			t.Fatalf("%s: Migrate() seq => %v", round, err)
		}
		check(numbered(backfilled...))

		// A new row continues the sequence.
		next := uuid.New()
		insert(next, "")
		want := append(numbered(backfilled...), delivery{next, 4})
		check(want)

		// Deleting the newest row does not free its number.
		mustExec(`DELETE FROM webhook_deliveries WHERE id = ?`, next)
		after := uuid.New()
		insert(after, "")
		check(append(numbered(backfilled...), delivery{after, 5}))

		if err := Rollback(ctx, dbx); err != nil {
			t.Fatalf("%s: Rollback() => %v", round, err)
		}
		if hasColumn(ctx, t, dbx, "webhook_deliveries", "seq") {
			t.Fatalf("%s: seq column still there after rollback", round)
		}
		var n int
		if err := dbx.GetContext(ctx, &n, `SELECT COUNT(*) FROM webhook_deliveries`); err != nil {
			t.Fatal(err)
		}
		if n != 4 {
			t.Fatalf("%s: %d rows after rollback, want 4", round, n)
		}
		// Back to the original rows for the next round.
		mustExec(`DELETE FROM webhook_deliveries WHERE id = ?`, after)
	}
}

func hasColumn(ctx context.Context, t *testing.T, dbx *db.DB, table, column string) bool {
	t.Helper()
	var n int
	q := `SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = ? AND column_name = ?`
	if dbx.DriverName() != "postgres" {
		q = `SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`
	}
	if err := dbx.GetContext(ctx, &n, dbx.Rebind(q), table, column); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

// On SQLite, the migration logs a VACUUM hint when it copied existing
// deliveries, and stays quiet on an empty table, such as a fresh install.
func TestWebhookDeliverySeqVacuumHint(t *testing.T) {
	all := migrations
	t.Cleanup(func() { migrations = all })
	for _, withRows := range []bool{false, true} {
		var buf bytes.Buffer
		ctx := config.WithContext(context.TODO(), config.DefaultConfig())
		ctx = log.WithContext(ctx, log.New(&buf))
		dbx, err := test.OpenSqlite(ctx, t)
		if err != nil {
			t.Fatal(err)
		}

		migrations = all[:webhookDeliverySeqVersion-1]
		if err := Migrate(ctx, dbx); err != nil {
			t.Fatalf("Migrate() before seq => %v", err)
		}
		if withRows {
			for _, q := range []string{
				`INSERT INTO users (id, username, admin, updated_at) VALUES (100, 'u', false, CURRENT_TIMESTAMP)`,
				`INSERT INTO repos (id, name, project_name, description, private, mirror, hidden, user_id, updated_at)
					VALUES (1, 'r', 'r', '', false, false, false, 100, CURRENT_TIMESTAMP)`,
				`INSERT INTO webhooks (id, repo_id, url, secret, content_type, active, updated_at)
					VALUES (1, 1, 'https://example.com', '', 0, true, CURRENT_TIMESTAMP)`,
				`INSERT INTO webhook_deliveries (id, webhook_id, event, request_url, request_method, request_headers, request_body, response_status, response_headers, response_body)
					VALUES ('` + uuid.NewString() + `', 1, 1, 'https://example.com', 'POST', '', '{}', 200, '', '')`,
			} {
				if _, err := dbx.ExecContext(ctx, q); err != nil {
					t.Fatalf("%s: %v", q, err)
				}
			}
		}
		migrations = all[:webhookDeliverySeqVersion]
		if err := Migrate(ctx, dbx); err != nil {
			t.Fatalf("Migrate() => %v", err)
		}
		if got := strings.Contains(buf.String(), "VACUUM"); got != withRows {
			t.Errorf("with rows %v: VACUUM hint logged = %v, want %v; log:\n%s", withRows, got, withRows, buf.String())
		}
		// The hint names the open database file, not the relative default.
		var file string
		if err := dbx.GetContext(ctx, &file, `SELECT file FROM pragma_database_list WHERE name = 'main'`); err != nil {
			t.Fatal(err)
		}
		if withRows && (!filepath.IsAbs(file) || !strings.Contains(buf.String(), file)) {
			t.Errorf("VACUUM hint does not name the database file %q; log:\n%s", file, buf.String())
		}
	}
}
