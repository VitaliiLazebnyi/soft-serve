package migrate

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/internal/test"
	"github.com/charmbracelet/soft-serve/pkg/sshutils"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
	"golang.org/x/crypto/ssh"
)

// openDB opens an empty test database: postgres when SOFT_SERVE_DB_DRIVER is
// "postgres" (a disposable database next to SOFT_SERVE_DB_DATA_SOURCE, as the
// test_postgres CI job sets), sqlite otherwise.
func openDB(t *testing.T, ctx context.Context) *db.DB {
	t.Helper()
	if os.Getenv("SOFT_SERVE_DB_DRIVER") != "postgres" {
		dbx, err := test.OpenSqlite(ctx, t)
		if err != nil {
			t.Fatal(err)
		}
		return dbx
	}

	dsn := os.Getenv("SOFT_SERVE_DB_DATA_SOURCE")
	if dsn == "" {
		dsn = "postgres://postgres@localhost:5432/postgres?sslmode=disable"
	}
	admin, err := db.Open(ctx, "postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("softserve_migrate_test_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		admin.Close() //nolint:errcheck
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.ExecContext(context.TODO(), "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
		admin.Close() //nolint:errcheck
	})

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	dbx, err := db.Open(ctx, "postgres", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dbx.Close() }) //nolint:errcheck
	return dbx
}

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

type ownerRow struct {
	Username string `db:"username"`
	Admin    bool   `db:"admin"`
}

func ownerOf(t *testing.T, dbx *db.DB, ak string) (ownerRow, bool) {
	t.Helper()
	var owners []ownerRow
	if err := dbx.Select(&owners, dbx.Rebind(`SELECT users.username, users.admin
		FROM users INNER JOIN public_keys ON users.id = public_keys.user_id
		WHERE public_keys.public_key = ?`), ak); err != nil {
		t.Fatal(err)
	}
	if len(owners) > 1 {
		t.Fatalf("key %q has %d owners", ak, len(owners))
	}
	if len(owners) == 0 {
		return ownerRow{}, false
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
	dbx := openDB(t, ctx)

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

	// Removing a key the sync attached from the config removes it again,
	// as it stopped being an admin key before keys were synced.
	cfg.InitialAdminKeys = []string{first}
	boot(t, ctx, cfg, dbx)
	if o, ok := ownerOf(t, dbx, later); ok {
		t.Errorf("removed key owner = %+v; want it removed", o)
	}
	if n := count(t, dbx, `SELECT COUNT(*) FROM settings WHERE "key" LIKE 'initial_admin_key:%'`); n != 0 {
		t.Errorf("synced key records = %d, want 0", n)
	}

	// A key the first migration seeded is kept when it leaves the config,
	// as before.
	cfg.InitialAdminKeys = nil
	boot(t, ctx, cfg, dbx)
	if o, ok := ownerOf(t, dbx, first); !ok || o.Username != "admin" {
		t.Errorf("seeded key owner = %+v, %v; want it kept on admin", o, ok)
	}

	// Adding the key back attaches it again.
	cfg.InitialAdminKeys = []string{first, later}
	boot(t, ctx, cfg, dbx)
	if o, ok := ownerOf(t, dbx, later); !ok || o.Username != "admin" {
		t.Errorf("re-added key owner = %+v, %v; want admin", o, ok)
	}
}

func TestAdminKeyRemovedOnlyFromUserItWasAttachedTo(t *testing.T) {
	ctx := context.TODO()
	dbx := openDB(t, ctx)

	cfg := config.DefaultConfig()
	boot(t, ctx, cfg, dbx)

	ak := newAuthorizedKey(t)
	cfg.InitialAdminKeys = []string{ak}
	boot(t, ctx, cfg, dbx)

	// The key was moved to another account by hand.
	mustExec(t, dbx, `INSERT INTO users (username, admin, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)`, "erin", false)
	mustExec(t, dbx, `UPDATE public_keys SET user_id = (SELECT id FROM users WHERE username = ?) WHERE public_key = ?`, "erin", ak)

	cfg.InitialAdminKeys = nil
	boot(t, ctx, cfg, dbx)
	if o, ok := ownerOf(t, dbx, ak); !ok || o.Username != "erin" {
		t.Errorf("key owner = %+v, %v; want it left on erin", o, ok)
	}
}

func TestAdminKeyGoesToAdminOwningAnotherConfiguredKey(t *testing.T) {
	ctx := context.TODO()
	dbx := openDB(t, ctx)

	cfg := config.DefaultConfig()
	boot(t, ctx, cfg, dbx)

	// carol is a later admin who owns a configured key; admin has the
	// lower id.
	owned := newAuthorizedKey(t)
	mustExec(t, dbx, `INSERT INTO users (username, admin, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)`, "carol", true)
	mustExec(t, dbx, `INSERT INTO public_keys (user_id, public_key, updated_at)
		VALUES ((SELECT id FROM users WHERE username = ?), ?, CURRENT_TIMESTAMP)`, "carol", owned)

	ak := newAuthorizedKey(t)
	cfg.InitialAdminKeys = []string{owned, ak}
	boot(t, ctx, cfg, dbx)
	if o, ok := ownerOf(t, dbx, ak); !ok || o.Username != "carol" {
		t.Errorf("key owner = %+v, %v; want carol", o, ok)
	}
}

func TestAdminKeyPrefersAdminNamedAdmin(t *testing.T) {
	ctx := context.TODO()
	dbx := openDB(t, ctx)

	cfg := config.DefaultConfig()
	boot(t, ctx, cfg, dbx)

	// The original admin is renamed; carol is promoted; a new "admin"
	// account comes after both.
	mustExec(t, dbx, `UPDATE users SET username = ? WHERE username = ?`, "zed", "admin")
	for _, name := range []string{"carol", "admin"} {
		mustExec(t, dbx, `INSERT INTO users (username, admin, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)`, name, true)
	}

	ak := newAuthorizedKey(t)
	cfg.InitialAdminKeys = []string{ak}
	boot(t, ctx, cfg, dbx)
	if o, ok := ownerOf(t, dbx, ak); !ok || o.Username != "admin" {
		t.Errorf("key owner = %+v, %v; want admin", o, ok)
	}
}

func TestAdminKeyFallsBackToLowestIDAdmin(t *testing.T) {
	ctx := context.TODO()
	dbx := openDB(t, ctx)

	cfg := config.DefaultConfig()
	boot(t, ctx, cfg, dbx)

	// The original admin was deleted; two admins were promoted later.
	mustExec(t, dbx, `DELETE FROM users WHERE username = ?`, "admin")
	for _, name := range []string{"dave", "carol"} {
		mustExec(t, dbx, `INSERT INTO users (username, admin, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)`, name, true)
	}

	ak := newAuthorizedKey(t)
	cfg.InitialAdminKeys = []string{ak}
	boot(t, ctx, cfg, dbx)
	if o, ok := ownerOf(t, dbx, ak); !ok || o.Username != "dave" {
		t.Errorf("key owner = %+v, %v; want dave (lowest id)", o, ok)
	}
}

func TestAdminKeyConcurrentBoots(t *testing.T) {
	if os.Getenv("SOFT_SERVE_DB_DRIVER") != "postgres" {
		t.Skip("concurrent boots need a server database; set SOFT_SERVE_DB_DRIVER=postgres")
	}
	ctx := context.TODO()
	dbx := openDB(t, ctx)

	cfg := config.DefaultConfig()
	boot(t, ctx, cfg, dbx)
	// No admin left, so every boot also races to create one.
	mustExec(t, dbx, `UPDATE users SET username = ?, admin = ? WHERE username = ?`, "bob", false, "admin")

	keys := []string{newAuthorizedKey(t), newAuthorizedKey(t)}
	cfg.InitialAdminKeys = keys
	cctx := config.WithContext(ctx, cfg)

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- Migrate(cctx, dbx)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("Migrate() => %v", err)
		}
	}

	for _, ak := range keys {
		if o, ok := ownerOf(t, dbx, ak); !ok || o.Username != "admin" || !o.Admin {
			t.Errorf("key owner = %+v, %v; want admin", o, ok)
		}
	}
	if n := count(t, dbx, `SELECT COUNT(*) FROM settings WHERE "key" LIKE 'initial_admin_key:%'`); n != len(keys) {
		t.Errorf("synced key records = %d, want %d", n, len(keys))
	}
}

func TestAdminKeyOwnedByNonAdminIsNotPromoted(t *testing.T) {
	ctx := context.TODO()
	dbx := openDB(t, ctx)

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
	dbx := openDB(t, ctx)

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
	dbx := openDB(t, ctx)

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

func TestAdminKeyNotRemovedWhileAnEntryCannotBeRead(t *testing.T) {
	ctx := context.TODO()
	dbx := openDB(t, ctx)

	cfg := config.DefaultConfig()
	boot(t, ctx, cfg, dbx)

	// The key is configured as a file, like a mounted secret.
	ak := newAuthorizedKey(t)
	path := filepath.Join(t.TempDir(), "admin.pub")
	if err := os.WriteFile(path, []byte(ak+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.InitialAdminKeys = []string{path}
	boot(t, ctx, cfg, dbx)
	if o, ok := ownerOf(t, dbx, ak); !ok || o.Username != "admin" {
		t.Fatalf("key owner = %+v, %v; want admin", o, ok)
	}

	// The file is not there on the next start.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	boot(t, ctx, cfg, dbx)
	if o, ok := ownerOf(t, dbx, ak); !ok || o.Username != "admin" {
		t.Errorf("key owner with the file missing = %+v, %v; want it kept on admin", o, ok)
	}

	// The same when Validate already dropped the entry, as on a real start.
	vcfg := config.DefaultConfig()
	vcfg.DataPath = t.TempDir()
	vcfg.InitialAdminKeys = []string{path}
	if err := vcfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(vcfg.InitialAdminKeys) != 0 {
		t.Fatalf("validated keys = %v, want none", vcfg.InitialAdminKeys)
	}
	boot(t, ctx, vcfg, dbx)
	if o, ok := ownerOf(t, dbx, ak); !ok || o.Username != "admin" {
		t.Errorf("key owner after Validate = %+v, %v; want it kept on admin", o, ok)
	}

	// Once every entry parses again, the key that left the config goes.
	cfg.InitialAdminKeys = nil
	boot(t, ctx, cfg, dbx)
	if o, ok := ownerOf(t, dbx, ak); ok {
		t.Errorf("key owner = %+v; want it removed", o)
	}
}

func TestAdminKeyAddedByHandIsKept(t *testing.T) {
	ctx := context.TODO()
	dbx := openDB(t, ctx)
	st := database.New(ctx, dbx)

	cfg := config.DefaultConfig()
	boot(t, ctx, cfg, dbx)
	mustExec(t, dbx, `INSERT INTO users (username, admin, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)`, "erin", false)

	readd, moved, other := newAuthorizedKey(t), newAuthorizedKey(t), newAuthorizedKey(t)
	cfg.InitialAdminKeys = []string{readd, moved, other}
	boot(t, ctx, cfg, dbx)

	pk := func(ak string) ssh.PublicKey {
		t.Helper()
		k, _, err := sshutils.ParseAuthorizedKey(ak)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	tx := func(fn func(tx *db.Tx) error) {
		t.Helper()
		if err := dbx.TransactionContext(ctx, fn); err != nil {
			t.Fatal(err)
		}
	}
	tx(func(tx *db.Tx) error {
		// readd is removed and added back to admin with the user commands
		// to keep it without the config.
		if err := st.RemovePublicKeyByUsername(ctx, tx, "admin", pk(readd)); err != nil {
			return err
		}
		if err := st.AddPublicKeyByUsername(ctx, tx, "admin", pk(readd)); err != nil {
			return err
		}
		// moved goes to erin.
		if err := st.RemovePublicKeyByUsername(ctx, tx, "admin", pk(moved)); err != nil {
			return err
		}
		if err := st.AddPublicKeyByUsername(ctx, tx, "erin", pk(moved)); err != nil {
			return err
		}
		// Removing other from a user that does not own it changes nothing.
		return st.RemovePublicKeyByUsername(ctx, tx, "erin", pk(other))
	})

	cfg.InitialAdminKeys = nil
	boot(t, ctx, cfg, dbx)
	if o, ok := ownerOf(t, dbx, readd); !ok || o.Username != "admin" {
		t.Errorf("re-added key owner = %+v, %v; want it kept on admin", o, ok)
	}
	if o, ok := ownerOf(t, dbx, moved); !ok || o.Username != "erin" {
		t.Errorf("moved key owner = %+v, %v; want it kept on erin", o, ok)
	}
	if o, ok := ownerOf(t, dbx, other); ok {
		t.Errorf("synced key owner = %+v; want it removed", o)
	}
}

func TestAdminKeysInsertedInSortedOrder(t *testing.T) {
	ctx := context.TODO()
	dbx := openDB(t, ctx)

	cfg := config.DefaultConfig()
	boot(t, ctx, cfg, dbx)

	keys := make([]string, 6)
	for i := range keys {
		keys[i] = newAuthorizedKey(t)
	}
	sorted := slices.Sorted(slices.Values(keys))
	cfg.InitialAdminKeys = slices.Clone(sorted)
	slices.Reverse(cfg.InitialAdminKeys)
	boot(t, ctx, cfg, dbx)

	var got []string
	if err := dbx.Select(&got, `SELECT public_key FROM public_keys ORDER BY id`); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, sorted) {
		t.Errorf("insert order = %v, want sorted %v", got, sorted)
	}
}

func TestInsertIgnoreOnlyIgnoresNamedColumn(t *testing.T) {
	ctx := context.TODO()
	dbx := openDB(t, ctx)
	boot(t, ctx, config.DefaultConfig(), dbx)
	mustExec(t, dbx, `INSERT INTO settings (id, "key", value, updated_at) VALUES (?, ?, ?, CURRENT_TIMESTAMP)`, 1000, "a", "1")

	insert := func(id int, key string) (int64, error) {
		var n int64
		err := dbx.TransactionContext(ctx, func(tx *db.Tx) error {
			q := insertIgnore(tx, `INTO settings (id, "key", value, updated_at) VALUES (?, ?, ?, CURRENT_TIMESTAMP)`, `"key"`)
			res, err := tx.ExecContext(ctx, q, id, key, "2")
			if err != nil {
				return err
			}
			n, err = res.RowsAffected()
			return err
		})
		return n, err
	}

	// A conflict on the named column is ignored.
	if n, err := insert(1001, "a"); err != nil || n != 0 {
		t.Errorf("same key: rows = %d, err = %v; want 0, nil", n, err)
	}
	// Any other conflict is an error.
	if _, err := insert(1000, "b"); err == nil {
		t.Error("primary key clash: err = nil, want an error")
	}
}

func TestAdminKeyConcurrentBootsDifferentOrder(t *testing.T) {
	if os.Getenv("SOFT_SERVE_DB_DRIVER") != "postgres" {
		t.Skip("concurrent boots need a server database; set SOFT_SERVE_DB_DRIVER=postgres")
	}
	ctx := context.TODO()
	dbx := openDB(t, ctx)
	boot(t, ctx, config.DefaultConfig(), dbx)

	keys := make([]string, 6)
	for i := range keys {
		keys[i] = newAuthorizedKey(t)
	}
	reversed := slices.Clone(keys)
	slices.Reverse(reversed)

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := range 8 {
		cfg := config.DefaultConfig()
		cfg.InitialAdminKeys = keys
		if i%2 == 1 {
			cfg.InitialAdminKeys = reversed
		}
		cctx := config.WithContext(ctx, cfg)
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- Migrate(cctx, dbx)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("Migrate() => %v", err)
		}
	}
	for _, ak := range keys {
		if o, ok := ownerOf(t, dbx, ak); !ok || o.Username != "admin" {
			t.Errorf("key owner = %+v, %v; want admin", o, ok)
		}
	}
}
