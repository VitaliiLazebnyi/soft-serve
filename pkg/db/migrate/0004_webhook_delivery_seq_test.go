package migrate

import (
	"context"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db/internal/test"
	"github.com/google/uuid"
)

// The sequence migration applies to a database that already holds
// deliveries, numbers them by created_at and then insertion order, and later
// inserts continue the sequence.
func TestWebhookDeliverySeqBackfill(t *testing.T) {
	ctx := config.WithContext(context.TODO(), config.DefaultConfig())
	dbx, err := test.OpenSqlite(ctx, t)
	if err != nil {
		t.Fatal(err)
	}

	// Migrate up to, but not including, the sequence migration.
	all := migrations
	t.Cleanup(func() { migrations = all })
	migrations = all[:webhookDeliverySeqVersion-1]
	if err := Migrate(ctx, dbx); err != nil {
		t.Fatalf("Migrate() before seq => %v", err)
	}

	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := dbx.ExecContext(ctx, dbx.Rebind(q), args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustExec(`INSERT INTO users (id, username, admin, updated_at) VALUES (100, 'u', 0, CURRENT_TIMESTAMP)`)
	mustExec(`INSERT INTO repos (id, name, project_name, description, private, mirror, hidden, user_id, updated_at)
		VALUES (1, 'r', 'r', '', 0, 0, 0, 100, CURRENT_TIMESTAMP)`)
	mustExec(`INSERT INTO webhooks (id, repo_id, url, secret, content_type, active, updated_at)
		VALUES (1, 1, 'https://example.com', '', 0, 1, CURRENT_TIMESTAMP)`)

	// Rows in insertion order. The first two share a second, and the third
	// is older than both.
	rows := []struct {
		id        uuid.UUID
		createdAt string
	}{
		{uuid.New(), "2024-01-01 00:00:05"},
		{uuid.New(), "2024-01-01 00:00:05"},
		{uuid.New(), "2024-01-01 00:00:01"},
	}
	for _, r := range rows {
		mustExec(`INSERT INTO webhook_deliveries (id, webhook_id, event, request_url, request_method, request_headers, request_body, response_status, response_headers, response_body, created_at)
			VALUES (?, 1, 1, 'https://example.com', 'POST', '', '{}', 200, '', '', ?)`, r.id, r.createdAt)
	}

	migrations = all
	if err := Migrate(ctx, dbx); err != nil {
		t.Fatalf("Migrate() seq => %v", err)
	}

	newID := uuid.New()
	mustExec(`INSERT INTO webhook_deliveries (id, webhook_id, event, request_url, request_method, request_headers, request_body, response_status, response_headers, response_body)
		VALUES (?, 1, 1, 'https://example.com', 'POST', '', '{}', 200, '', '')`, newID)

	var got []struct {
		ID  uuid.UUID `db:"id"`
		Seq int64     `db:"seq"`
	}
	if err := dbx.SelectContext(ctx, &got, `SELECT id, seq FROM webhook_deliveries ORDER BY seq`); err != nil {
		t.Fatal(err)
	}
	want := []uuid.UUID{rows[2].id, rows[0].id, rows[1].id, newID}
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d", len(got), len(want))
	}
	for i, g := range got {
		if g.ID != want[i] || g.Seq != int64(i+1) {
			t.Errorf("row %d = (%s, %d), want (%s, %d)", i, g.ID, g.Seq, want[i], i+1)
		}
	}
}
