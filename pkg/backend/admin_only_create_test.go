package backend

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"testing"
	"time"

	charmssh "charm.land/ssh"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/sshutils"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/matryer/is"
	"golang.org/x/crypto/ssh"
)

// With AdminOnlyCreate on, a missing name is read-only for non-admins and
// anonymous users, so nothing can be created there.
func TestAdminOnlyCreateFlagOnCapsBaseAccess(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := context.Background()
	cfg.AdminOnlyCreate = true

	user, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	is.NoErr(err)
	admin, err := be.CreateUser(ctx, "root", proto.UserOptions{Admin: true})
	is.NoErr(err)

	is.Equal(be.AccessLevelForUser(ctx, "fresh", user), access.ReadOnlyAccess)
	is.Equal(be.AccessLevelForUser(ctx, "fresh", admin), access.AdminAccess)
	// Anonymous access is capped too: even when the server allows read-write
	// for anonymous users, a missing name is read-only.
	rw := access.ReadWriteAccess
	cfg.AnonAccess = &rw
	is.Equal(be.AccessLevelForUser(ctx, "fresh", nil), access.ReadOnlyAccess)
}

// With AdminOnlyCreate off (the default), the backend keeps upstream
// behaviour: a registered user gets read-write on a missing name, so can
// create it.
func TestAdminOnlyCreateFlagOffKeepsUpstreamBaseAccess(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := context.Background()
	is.True(!cfg.AdminOnlyCreate)
	rw := access.ReadWriteAccess
	cfg.AnonAccess = &rw

	user, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	is.NoErr(err)

	is.Equal(be.AccessLevelForUser(ctx, "fresh", user), access.ReadWriteAccess)
	is.Equal(be.AccessLevelForUser(ctx, "fresh", nil), access.ReadWriteAccess)
}

// A database admin keeps admin access on a missing name whatever the flag.
func TestDatabaseAdminBaseAccessIgnoresFlag(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := context.Background()

	pub := newTestPublicKey(t)
	_, err := be.CreateUser(ctx, "root", proto.UserOptions{Admin: true, PublicKeys: []ssh.PublicKey{pub}})
	is.NoErr(err)

	cfg.AdminOnlyCreate = true
	is.Equal(be.BaseAccessLevelByPublicKey(ctx, pub), access.AdminAccess)
	cfg.AdminOnlyCreate = false
	is.Equal(be.BaseAccessLevelByPublicKey(ctx, pub), access.AdminAccess)
}

// With the flag off, a registered user on a missing name gets the anonymous
// level when that is higher than read-write (upstream behaviour), and the
// flag on caps it at read-only.
func TestAnonAdminLevelIsCappedOnlyWhenFlagOn(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := context.Background()

	user, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	is.NoErr(err)
	anonAdmin := access.AdminAccess
	cfg.AnonAccess = &anonAdmin

	cfg.AdminOnlyCreate = false
	is.Equal(be.AccessLevelForUser(ctx, "fresh", user), access.AdminAccess)

	cfg.AdminOnlyCreate = true
	is.Equal(be.AccessLevelForUser(ctx, "fresh", user), access.ReadOnlyAccess)
}

// backendTestContext wires the config, database and store a backend call
// reads from its context (webhook delivery and rename need them).
func backendTestContext(be *Backend, cfg *config.Config) context.Context {
	ctx := context.Background()
	ctx = config.WithContext(ctx, cfg)
	ctx = db.WithContext(ctx, be.db)
	return store.WithContext(ctx, be.store)
}

// seedCreate creates a fixture repository while admin-only creation is off,
// so tests can set up owners without the create policy in the way. Tests that
// exercise the policy call be.CreateRepository directly.
func seedCreate(be *Backend, ctx context.Context, name string, user proto.User, opts proto.RepositoryOptions) (proto.Repository, error) {
	prev := be.cfg.AdminOnlyCreate
	be.cfg.AdminOnlyCreate = false
	defer func() { be.cfg.AdminOnlyCreate = prev }()
	return be.CreateRepository(ctx, name, user, opts)
}

// seedImport is seedCreate for ImportRepository.
func seedImport(be *Backend, ctx context.Context, name string, user proto.User, remote string, opts proto.RepositoryOptions) (proto.Repository, error) {
	prev := be.cfg.AdminOnlyCreate
	be.cfg.AdminOnlyCreate = false
	defer func() { be.cfg.AdminOnlyCreate = prev }()
	return be.ImportRepository(ctx, name, user, remote, opts)
}

// With AdminOnlyCreate on, the backend itself refuses a non-admin, and an
// anonymous caller, creating, importing or renaming to a new name, whichever
// caller reaches it. Nothing is created.
func TestBackendRefusesNonAdminCreatesWhenFlagOn(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	cfg.AdminOnlyCreate = true

	user, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	is.NoErr(err)
	userCtx := proto.WithUserContext(ctx, user)

	_, err = be.CreateRepository(userCtx, "junk", user, proto.RepositoryOptions{})
	is.True(errors.Is(err, proto.ErrUnauthorized))
	_, err = be.CreateRepository(ctx, "anon-junk", nil, proto.RepositoryOptions{})
	is.True(errors.Is(err, proto.ErrUnauthorized))
	_, err = be.ImportRepository(userCtx, "junk-import", user, "https://example.invalid/x.git", proto.RepositoryOptions{})
	is.True(errors.Is(err, proto.ErrUnauthorized))

	_, err = be.Repository(ctx, "junk")
	is.True(err != nil)
	_, err = be.Repository(ctx, "anon-junk")
	is.True(err != nil)
	_, err = be.Repository(ctx, "junk-import")
	is.True(err != nil)
}

// With AdminOnlyCreate on, a non-admin's rename of an owned repository to a
// new name is refused and the repository stays where it was.
func TestBackendRefusesNonAdminRenameToNewNameWhenFlagOn(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	cfg.AdminOnlyCreate = true

	user, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	is.NoErr(err)
	_, err = seedCreate(be, ctx, "mine", user, proto.RepositoryOptions{})
	is.NoErr(err)

	userCtx := proto.WithUserContext(ctx, user)
	is.True(errors.Is(be.RenameRepository(userCtx, "mine", "renamed"), proto.ErrUnauthorized))
	_, err = be.Repository(ctx, "mine")
	is.NoErr(err)
	_, err = be.Repository(ctx, "renamed")
	is.True(err != nil)
}

// With AdminOnlyCreate on, a database admin may create, import and rename to
// a new name. The import may still fail for an unrelated reason (for example,
// a git version gate), so it only has to pass the policy.
func TestBackendAllowsAdminCreatesWhenFlagOn(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	cfg.AdminOnlyCreate = true

	admin, err := be.CreateUser(ctx, "root", proto.UserOptions{Admin: true})
	is.NoErr(err)
	adminCtx := proto.WithUserContext(ctx, admin)

	_, err = be.CreateRepository(adminCtx, "made", admin, proto.RepositoryOptions{})
	is.NoErr(err)
	_, err = be.ImportRepository(adminCtx, "imported", admin, "https://example.invalid/x.git", proto.RepositoryOptions{})
	is.True(!errors.Is(err, proto.ErrUnauthorized))
	is.NoErr(be.RenameRepository(adminCtx, "made", "made-again"))
	_, err = be.Repository(ctx, "made-again")
	is.NoErr(err)
}

// With AdminOnlyCreate off, the same non-admin calls are not refused by the
// policy: create and rename to a new name succeed.
func TestBackendAllowsNonAdminCreatesWhenFlagOff(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	cfg.AdminOnlyCreate = false

	user, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	is.NoErr(err)
	userCtx := proto.WithUserContext(ctx, user)

	_, err = be.CreateRepository(userCtx, "mine", user, proto.RepositoryOptions{})
	is.NoErr(err)
	is.NoErr(be.RenameRepository(userCtx, "mine", "renamed"))
	_, err = be.Repository(ctx, "renamed")
	is.NoErr(err)
	_, err = be.ImportRepository(userCtx, "imported", user, "https://example.invalid/x.git", proto.RepositoryOptions{})
	is.True(!errors.Is(err, proto.ErrUnauthorized))
}

func newTestPublicKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	is := is.New(t)
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	is.NoErr(err)
	signer, err := ssh.NewSignerFromKey(priv)
	is.NoErr(err)
	return signer.PublicKey()
}

// A key listed in initial_admin_keys is a server admin even when it was added
// after the first migration, so it never reached the database: the caller is
// anonymous or a non-admin account. With AdminOnlyCreate on, the backend must
// still let it create, import and rename to a new name, as the SSH layer's
// isServerAdmin does.
func TestBackendAllowsAdminKeyCreatesWhenFlagOn(t *testing.T) {
	for _, registered := range []bool{false, true} {
		name := "anonymous"
		if registered {
			name = "non-admin user"
		}
		t.Run(name, func(t *testing.T) {
			is := is.New(t)
			be, cfg := newTestBackend(t)
			ctx := backendTestContext(be, cfg)
			cfg.AdminOnlyCreate = true

			pub := newTestPublicKey(t)
			cfg.InitialAdminKeys = append(cfg.InitialAdminKeys, sshutils.MarshalAuthorizedKey(pub))

			var user proto.User
			if registered {
				var err error
				user, err = be.CreateUser(ctx, "alice", proto.UserOptions{PublicKeys: []ssh.PublicKey{pub}})
				is.NoErr(err)
				is.True(!user.IsAdmin())
				ctx = proto.WithUserContext(ctx, user)
			}
			keyCtx := context.WithValue(ctx, charmssh.ContextKeyPublicKey, pub)

			// Without the key on the context, the same user is refused.
			_, err := be.CreateRepository(ctx, "nokey", user, proto.RepositoryOptions{})
			is.True(errors.Is(err, proto.ErrUnauthorized))

			is.Equal(be.AccessLevelForUser(keyCtx, "fresh", user), access.AdminAccess)

			_, err = be.CreateRepository(keyCtx, "made", user, proto.RepositoryOptions{})
			is.NoErr(err)
			_, err = be.ImportRepository(keyCtx, "imported", user, "https://example.invalid/x.git", proto.RepositoryOptions{})
			is.True(!errors.Is(err, proto.ErrUnauthorized))
			if !registered {
				// The rename webhook needs a sender, so rename is only
				// exercised with a registered user.
				return
			}
			is.NoErr(be.RenameRepository(keyCtx, "made", "made-again"))
			_, err = be.Repository(ctx, "made-again")
			is.NoErr(err)
		})
	}
}

// Over HTTP (token, password or JWT) there is no public key on the context,
// so the only server-admin signal is the account's admin flag. Owning a key
// listed in initial_admin_keys must not count: `pubkey add` attaches a key
// without proof of holding its private half, so a user could claim a key
// before an operator lists it and become an admin over HTTP.
func TestHTTPAdminOnlyByAdminFlag(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	cfg.AdminOnlyCreate = true

	claimed := newTestPublicKey(t)
	_, err := be.CreateUser(ctx, "mallory", proto.UserOptions{PublicKeys: []ssh.PublicKey{claimed}})
	is.NoErr(err)
	_, err = be.CreateUser(ctx, "boss", proto.UserOptions{Admin: true, PublicKeys: []ssh.PublicKey{newTestPublicKey(t)}})
	is.NoErr(err)

	// The operator lists the key after mallory attached it.
	cfg.InitialAdminKeys = append(cfg.InitialAdminKeys, sshutils.MarshalAuthorizedKey(claimed))

	// Load the users the way the HTTP layer does: by name (password, JWT)
	// and by access token.
	load := func(name string) []proto.User {
		u, err := be.User(ctx, name)
		is.NoErr(err)
		tok, err := be.CreateAccessToken(ctx, u, "t", time.Time{})
		is.NoErr(err)
		byToken, err := be.UserByAccessToken(ctx, tok)
		is.NoErr(err)
		return []proto.User{u, byToken}
	}

	for i, u := range load("mallory") {
		is.True(!u.IsAdmin())
		is.Equal(be.AccessLevelForUser(ctx, "fresh", u), access.ReadOnlyAccess)
		_, err := be.CreateRepository(ctx, fmt.Sprintf("mallory-%d", i), u, proto.RepositoryOptions{})
		is.True(errors.Is(err, proto.ErrUnauthorized))
	}
	for i, u := range load("boss") {
		is.Equal(be.AccessLevelForUser(ctx, "fresh", u), access.AdminAccess)
		_, err := be.CreateRepository(ctx, fmt.Sprintf("boss-%d", i), u, proto.RepositoryOptions{})
		is.NoErr(err)
	}
}

// A key listed in initial_admin_keys that a non-admin account owns does not
// resolve to that account: the owner may have claimed it with `pubkey add`
// before it was listed, and the key holder's session would otherwise act as
// them (and own what it creates). The key keeps admin access with no
// account. A listed key owned by an admin account resolves as before, and
// unlisting the key restores the ordinary lookup.
func TestListedAdminKeyOwnedByNonAdminHasNoAccount(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)

	claimed := newTestPublicKey(t)
	_, err := be.CreateUser(ctx, "mallory", proto.UserOptions{PublicKeys: []ssh.PublicKey{claimed}})
	is.NoErr(err)
	bossKey := newTestPublicKey(t)
	_, err = be.CreateUser(ctx, "boss", proto.UserOptions{Admin: true, PublicKeys: []ssh.PublicKey{bossKey}})
	is.NoErr(err)

	u, err := be.UserByPublicKey(ctx, claimed)
	is.NoErr(err)
	is.Equal(u.Username(), "mallory")

	orig := cfg.InitialAdminKeys
	cfg.InitialAdminKeys = append(append([]string{}, orig...),
		sshutils.MarshalAuthorizedKey(claimed), sshutils.MarshalAuthorizedKey(bossKey))

	_, err = be.UserByPublicKey(ctx, claimed)
	is.True(errors.Is(err, proto.ErrUserNotFound))
	is.Equal(be.BaseAccessLevelByPublicKey(ctx, claimed), access.AdminAccess)

	// A private repository made with the key goes to the default admin,
	// and mallory gets nothing on it.
	r, err := be.CreateRepository(ctx, "secret", nil, proto.RepositoryOptions{Private: true})
	is.NoErr(err)
	mallory, err := be.User(ctx, "mallory")
	is.NoErr(err)
	is.True(r.UserID() != mallory.ID())
	is.Equal(be.AccessLevelForUser(ctx, "secret", mallory), access.NoAccess)

	u, err = be.UserByPublicKey(ctx, bossKey)
	is.NoErr(err)
	is.Equal(u.Username(), "boss")

	cfg.InitialAdminKeys = orig
	u, err = be.UserByPublicKey(ctx, claimed)
	is.NoErr(err)
	is.Equal(u.Username(), "mallory")
}
