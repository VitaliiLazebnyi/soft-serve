package backend

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/proto"
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
	is.True(cfg.AdminOnlyCreate)

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

// With AdminOnlyCreate off, the backend keeps upstream behaviour: a registered
// user gets read-write on a missing name, so can create it.
func TestAdminOnlyCreateFlagOffKeepsUpstreamBaseAccess(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := context.Background()
	cfg.AdminOnlyCreate = false
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
	is.True(cfg.AdminOnlyCreate)

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
	is.True(cfg.AdminOnlyCreate)

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
