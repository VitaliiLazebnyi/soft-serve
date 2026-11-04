package backend

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/proto"
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

func newTestPublicKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	is := is.New(t)
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	is.NoErr(err)
	signer, err := ssh.NewSignerFromKey(priv)
	is.NoErr(err)
	return signer.PublicKey()
}
