package migrate

import (
	"context"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db/internal/test"
	"github.com/google/uuid"
)

// The retention migration trims each webhook to its 100 most recently
// recorded deliveries and leaves webhooks with fewer alone. Rolling it back
// keeps the remaining rows. The test runs on Postgres when
// SOFT_SERVE_DB_DRIVER is "postgres".
func TestWebhookDeliveryRetentionTrims(t *testing.T) {
	ctx := config.WithContext(context.TODO(), config.DefaultConfig())
	dbx, err := test.Open(ctx, t)
	if err != nil {
		t.Fatal(err)
	}

	// Migrate up to, but not including, the retention migration. Later
	// migrations stay out of the test so that Rollback undoes this one.
	all := migrations
	t.Cleanup(func() { migrations = all })
	migrations = all[:webhookDeliveryRetentionVersion-1]
	if err := Migrate(ctx, dbx); err != nil {
		t.Fatalf("Migrate() before retention => %v", err)
	}
	migrations = all[:webhookDeliveryRetentionVersion]

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
		VALUES (1, 1, 'https://example.com/a', '', 0, true, CURRENT_TIMESTAMP)`)
	mustExec(`INSERT INTO webhooks (id, repo_id, url, secret, content_type, active, updated_at)
		VALUES (2, 1, 'https://example.com/b', '', 0, true, CURRENT_TIMESTAMP)`)

	insert := func(webhookID int) uuid.UUID {
		id := uuid.New()
		mustExec(`INSERT INTO webhook_deliveries (id, webhook_id, event, request_url, request_method, request_headers, request_body, response_status, response_headers, response_body)
			VALUES (?, ?, 1, 'https://example.com', 'POST', '', '{}', 200, '', '')`, id, webhookID)
		return id
	}
	var hookA []uuid.UUID
	for i := range 130 {
		hookA = append(hookA, insert(1))
		if i < 3 {
			insert(2)
		}
	}

	if err := Migrate(ctx, dbx); err != nil {
		t.Fatalf("Migrate() retention => %v", err)
	}

	check := func(step string) {
		t.Helper()
		var got []uuid.UUID
		if err := dbx.SelectContext(ctx, &got, `SELECT id FROM webhook_deliveries WHERE webhook_id = 1 ORDER BY seq`); err != nil {
			t.Fatal(err)
		}
		want := hookA[30:]
		if len(got) != len(want) {
			t.Fatalf("%s: webhook 1 has %d deliveries, want %d", step, len(got), len(want))
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("%s: delivery %d = %s, want %s", step, i, got[i], want[i])
			}
		}
		var n int
		if err := dbx.GetContext(ctx, &n, `SELECT COUNT(*) FROM webhook_deliveries WHERE webhook_id = 2`); err != nil {
			t.Fatal(err)
		}
		if n != 3 {
			t.Errorf("%s: webhook 2 has %d deliveries, want 3", step, n)
		}
	}
	check("migrate")

	if err := Rollback(ctx, dbx); err != nil {
		t.Fatalf("Rollback() retention => %v", err)
	}
	check("rollback")
	if err := Migrate(ctx, dbx); err != nil {
		t.Fatalf("Migrate() retention again => %v", err)
	}
	check("migrate again")
}
