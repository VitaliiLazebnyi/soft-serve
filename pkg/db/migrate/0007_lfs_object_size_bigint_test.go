package migrate

import (
	"context"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db/internal/test"
)

// LFS objects of 2 GiB or more must be recordable. Postgres stored
// lfs_objects.size as a 32-bit INTEGER, so their insert failed. Rolling the
// migration back works once no such object is recorded. The test runs on
// Postgres when SOFT_SERVE_DB_DRIVER is "postgres".
func TestLFSObjectSizeBigint(t *testing.T) {
	ctx := config.WithContext(context.TODO(), config.DefaultConfig())
	dbx, err := test.Open(ctx, t)
	if err != nil {
		t.Fatal(err)
	}

	// Later migrations stay out of the test so that Rollback undoes this one.
	all := migrations
	t.Cleanup(func() { migrations = all })
	migrations = all[:lfsObjectSizeBigintVersion]
	if err := Migrate(ctx, dbx); err != nil {
		t.Fatalf("Migrate() => %v", err)
	}

	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := dbx.ExecContext(ctx, dbx.Rebind(q), args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustExec(`INSERT INTO users (id, username, admin, updated_at) VALUES (100, 'u', false, CURRENT_TIMESTAMP)`)
	mustExec(`INSERT INTO repos (id, name, project_name, description, private, mirror, hidden, user_id, updated_at)
		VALUES (1, 'r', 'r', '', false, false, false, 100, CURRENT_TIMESTAMP)`)

	const big = int64(5) << 30 // 5 GiB
	mustExec(`INSERT INTO lfs_objects (repo_id, oid, size, updated_at) VALUES (1, 'big', ?, CURRENT_TIMESTAMP)`, big)
	var got int64
	if err := dbx.GetContext(ctx, &got, `SELECT size FROM lfs_objects WHERE oid = 'big'`); err != nil {
		t.Fatal(err)
	}
	if got != big {
		t.Fatalf("size = %d, want %d", got, big)
	}

	mustExec(`DELETE FROM lfs_objects WHERE oid = 'big'`)
	mustExec(`INSERT INTO lfs_objects (repo_id, oid, size, updated_at) VALUES (1, 'small', 42, CURRENT_TIMESTAMP)`)
	if err := Rollback(ctx, dbx); err != nil {
		t.Fatalf("Rollback() => %v", err)
	}
	if err := Migrate(ctx, dbx); err != nil {
		t.Fatalf("Migrate() again => %v", err)
	}
	if err := dbx.GetContext(ctx, &got, `SELECT size FROM lfs_objects WHERE oid = 'small'`); err != nil {
		t.Fatal(err)
	}
	if got != 42 {
		t.Errorf("size after rollback and migrate = %d, want 42", got)
	}
}
