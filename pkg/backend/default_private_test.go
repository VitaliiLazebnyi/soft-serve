package backend

import (
	"context"
	"errors"
	"testing"

	charmssh "charm.land/ssh"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/sshutils"
	"github.com/matryer/is"
)

// DefaultPrivate is the visibility of a repository created without an
// explicit one. With default_private set, it is private for users and
// server admins, and refused for anonymous callers, who could not reach a
// private repository.
func TestDefaultPrivate(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)

	alice, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	is.NoErr(err)
	admin, err := be.User(ctx, "admin")
	is.NoErr(err)

	// Off (the default): public for everyone, anonymous callers included.
	for _, u := range []proto.User{alice, admin, nil} {
		private, err := be.DefaultPrivate(ctx, u)
		is.NoErr(err)
		is.True(!private)
	}

	cfg.DefaultPrivate = true
	for _, u := range []proto.User{alice, admin} {
		private, err := be.DefaultPrivate(ctx, u)
		is.NoErr(err)
		is.True(private)
	}

	_, err = be.DefaultPrivate(ctx, nil)
	is.True(errors.Is(err, proto.ErrAnonymousPrivate))

	// A key in initial_admin_keys that no account holds is a server admin
	// and has access to every repository, so it gets the private default.
	pub := newTestPublicKey(t)
	cfg.InitialAdminKeys = append(cfg.InitialAdminKeys, sshutils.MarshalAuthorizedKey(pub))
	keyCtx := context.WithValue(ctx, charmssh.ContextKeyPublicKey, pub)
	private, err := be.DefaultPrivate(keyCtx, nil)
	is.NoErr(err)
	is.True(private)
}
