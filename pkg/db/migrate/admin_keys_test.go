package migrate

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/internal/test"
	"github.com/charmbracelet/soft-serve/pkg/sshutils"
	"golang.org/x/crypto/ssh"
)

func newAuthorizedKey(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return sshutils.MarshalAuthorizedKey(pk)
}

type keyOwner struct {
	Username string `db:"username"`
	Admin    bool   `db:"admin"`
}

func ownerOf(t *testing.T, dbx *db.DB, ak string) (keyOwner, bool) {
	t.Helper()
	var owners []keyOwner
	if err := dbx.Select(&owners, dbx.Rebind(`SELECT users.username, users.admin
		FROM users INNER JOIN public_keys ON users.id = public_keys.user_id
		WHERE public_keys.public_key = ?`), ak); err != nil {
		t.Fatal(err)
	}
	if len(owners) > 1 {
		t.Fatalf("key %q has %d owners", ak, len(owners))
	}
	if len(owners) == 0 {
		return keyOwner{}, false
	}
	return owners[0], true
}

func count(t *testing.T, dbx *db.DB, query string) int {
	t.Helper()
	var n int
	if err := dbx.Get(&n, query); err != nil {
		t.Fatal(err)
	}
	return n
}

func mustExec(t *testing.T, dbx *db.DB, query string, args ...any) {
	t.Helper()
	if _, err := dbx.Exec(dbx.Rebind(query), args...); err != nil {
		t.Fatal(err)
	}
}

// boot opens the database and runs the migrations the way the server does
// on every start, with cfg in the context.
func boot(t *testing.T, ctx context.Context, cfg *config.Config, dbx *db.DB) {
	t.Helper()
	if err := Migrate(config.WithContext(ctx, cfg), dbx); err != nil {
		t.Fatalf("Migrate() => %v", err)
	}
}

func TestAdminKeyAddedAfterFirstBootBecomesAdminKey(t *testing.T) {
	ctx := context.TODO()
	dbx, err := test.OpenSqlite(ctx, t)
	if err != nil {
		t.Fatal(err)
	}

	first := newAuthorizedKey(t)
	cfg := config.DefaultConfig()
	cfg.InitialAdminKeys = []string{first}
	boot(t, ctx, cfg, dbx)

	if o, ok := ownerOf(t, dbx, first); !ok || o.Username != "admin" || !o.Admin {
		t.Fatalf("first key owner = %+v, %v; want admin", o, ok)
	}

	// A key added to the config after the first boot.
	later := newAuthorizedKey(t)
	cfg.InitialAdminKeys = []string{first, later}
	boot(t, ctx, cfg, dbx)

	if o, ok := ownerOf(t, dbx, later); !ok || o.Username != "admin" || !o.Admin {
		t.Fatalf("later key owner = %+v, %v; want admin", o, ok)
	}

	// Booting again changes nothing.
	keys := count(t, dbx, "SELECT COUNT(*) FROM public_keys")
	users := count(t, dbx, "SELECT COUNT(*) FROM users")
	boot(t, ctx, cfg, dbx)
	boot(t, ctx, cfg, dbx)
	if got := count(t, dbx, "SELECT COUNT(*) FROM public_keys"); got != keys {
		t.Errorf("public keys after re-boot = %d, want %d", got, keys)
	}
	if got := count(t, dbx, "SELECT COUNT(*) FROM users"); got != users {
		t.Errorf("users after re-boot = %d, want %d", got, users)
	}

	// Removing a key from the config does not remove it from the database.
	cfg.InitialAdminKeys = []string{first}
	boot(t, ctx, cfg, dbx)
	if o, ok := ownerOf(t, dbx, later); !ok || !o.Admin {
		t.Errorf("removed key owner = %+v, %v; want it kept on admin", o, ok)
	}
}

func TestAdminKeyOwnedByNonAdminIsNotPromoted(t *testing.T) {
	ctx := context.TODO()
	dbx, err := test.OpenSqlite(ctx, t)
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.DefaultConfig()
	boot(t, ctx, cfg, dbx)

	ak := newAuthorizedKey(t)
	mustExec(t, dbx, `INSERT INTO users (username, admin, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)`, "alice", false)
	mustExec(t, dbx, `INSERT INTO public_keys (user_id, public_key, updated_at)
		VALUES ((SELECT id FROM users WHERE username = ?), ?, CURRENT_TIMESTAMP)`, "alice", ak)

	cfg.InitialAdminKeys = []string{ak}
	boot(t, ctx, cfg, dbx)

	o, ok := ownerOf(t, dbx, ak)
	if !ok || o.Username != "alice" {
		t.Fatalf("key owner = %+v, %v; want it left on alice", o, ok)
	}
	if o.Admin {
		t.Fatal("alice was promoted to admin")
	}
}

func TestAdminKeyCreatesAdminWhenNoneExists(t *testing.T) {
	ctx := context.TODO()
	dbx, err := test.OpenSqlite(ctx, t)
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.DefaultConfig()
	boot(t, ctx, cfg, dbx)
	// The only admin was renamed and demoted.
	mustExec(t, dbx, `UPDATE users SET username = ?, admin = ? WHERE username = ?`, "bob", false, "admin")

	ak := newAuthorizedKey(t)
	cfg.InitialAdminKeys = []string{ak}
	boot(t, ctx, cfg, dbx)

	if o, ok := ownerOf(t, dbx, ak); !ok || o.Username != "admin" || !o.Admin {
		t.Fatalf("key owner = %+v, %v; want a new admin user", o, ok)
	}
	if n := count(t, dbx, "SELECT COUNT(*) FROM users WHERE username = 'bob' AND admin = false"); n != 1 {
		t.Errorf("bob was changed")
	}
}

func TestAdminKeyNotAttachedToNonAdminNamedAdmin(t *testing.T) {
	ctx := context.TODO()
	dbx, err := test.OpenSqlite(ctx, t)
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.DefaultConfig()
	boot(t, ctx, cfg, dbx)
	// No admin at all, and "admin" is a regular account.
	mustExec(t, dbx, `UPDATE users SET admin = ? WHERE username = ?`, false, "admin")

	ak := newAuthorizedKey(t)
	cfg.InitialAdminKeys = []string{ak}
	boot(t, ctx, cfg, dbx)

	if o, ok := ownerOf(t, dbx, ak); ok {
		t.Fatalf("key attached to %+v; want it left out", o)
	}
	if n := count(t, dbx, "SELECT COUNT(*) FROM users WHERE admin = true"); n != 0 {
		t.Errorf("admins = %d, want 0 (no promotion)", n)
	}
}
