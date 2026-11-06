package backend

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	charmssh "charm.land/ssh"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/sshutils"
	"github.com/matryer/is"
	"golang.org/x/crypto/ssh"
)

// With max_repos_per_user set, a non-admin is refused once they have created
// that many repositories, by create and by import, and keeps the ones they
// have. Deleting one frees a slot.
func TestMaxReposPerUser(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	cfg.MaxReposPerUser = 2

	alice, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	is.NoErr(err)
	bob, err := be.CreateUser(ctx, "bob", proto.UserOptions{})
	is.NoErr(err)

	_, err = be.CreateRepository(ctx, "a1", alice, proto.RepositoryOptions{})
	is.NoErr(err)
	_, err = be.CreateRepository(ctx, "a2", alice, proto.RepositoryOptions{})
	is.NoErr(err)

	_, err = be.CreateRepository(ctx, "a3", alice, proto.RepositoryOptions{})
	is.True(errors.Is(err, proto.ErrRepoLimitReached))
	// Refused before the remote is touched, with the reason rather than
	// the fixed import failure.
	_, err = be.ImportRepository(ctx, "a4", alice, offlineRemote, proto.RepositoryOptions{})
	is.True(errors.Is(err, proto.ErrRepoLimitReached))
	for _, name := range []string{"a3", "a4"} {
		_, err = be.Repository(ctx, name)
		is.True(errors.Is(err, proto.ErrRepoNotFound))
		is.True(!be.repoPathExists(name))
	}

	// The existing repositories are kept.
	for _, name := range []string{"a1", "a2"} {
		_, err = be.Repository(ctx, name)
		is.NoErr(err)
	}

	// The limit is per user.
	_, err = be.CreateRepository(ctx, "b1", bob, proto.RepositoryOptions{})
	is.NoErr(err)

	is.NoErr(be.DeleteRepository(ctx, "a1"))
	_, err = be.CreateRepository(ctx, "a3", alice, proto.RepositoryOptions{})
	is.NoErr(err)

	// 0 means no limit.
	cfg.MaxReposPerUser = 0
	_, err = be.CreateRepository(ctx, "a5", alice, proto.RepositoryOptions{})
	is.NoErr(err)
}

// Server admins are exempt: admin accounts, and the holder of a key in
// initial_admin_keys whatever account the key is on. So is the server's own
// bootstrap create.
func TestMaxReposPerUserAdminsExempt(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	cfg.MaxReposPerUser = 1

	admin, err := be.User(ctx, "admin")
	is.NoErr(err)
	for _, name := range []string{"x1", "x2", "x3"} {
		_, err = be.CreateRepository(ctx, name, admin, proto.RepositoryOptions{})
		is.NoErr(err)
	}

	pub := newTestPublicKey(t)
	cfg.InitialAdminKeys = append(cfg.InitialAdminKeys, sshutils.MarshalAuthorizedKey(pub))
	alice, err := be.CreateUser(ctx, "alice", proto.UserOptions{PublicKeys: []ssh.PublicKey{pub}})
	is.NoErr(err)
	keyCtx := context.WithValue(proto.WithUserContext(ctx, alice), charmssh.ContextKeyPublicKey, pub)
	for _, name := range []string{"k1", "k2"} {
		_, err = be.CreateRepository(keyCtx, name, alice, proto.RepositoryOptions{})
		is.NoErr(err)
	}
	// Without the key on the context, alice is an ordinary user at the limit.
	_, err = be.CreateRepository(ctx, "k3", alice, proto.RepositoryOptions{})
	is.True(errors.Is(err, proto.ErrRepoLimitReached))

	bob, err := be.CreateUser(ctx, "bob", proto.UserOptions{})
	is.NoErr(err)
	for _, name := range []string{"s1", "s2"} {
		_, err = be.CreateSystemRepository(ctx, name, bob, proto.RepositoryOptions{})
		is.NoErr(err)
	}
}

// Repositories created without an account all count together as one user.
// They are owned by the default admin but are counted apart from the
// admin's own repositories.
func TestMaxReposPerUserAnonymous(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	cfg.MaxReposPerUser = 2
	rw := access.ReadWriteAccess
	cfg.AnonAccess = &rw

	admin, err := be.User(ctx, "admin")
	is.NoErr(err)
	for _, name := range []string{"x1", "x2", "x3"} {
		_, err = be.CreateRepository(ctx, name, admin, proto.RepositoryOptions{})
		is.NoErr(err)
	}

	_, err = be.CreateRepository(ctx, "anon1", nil, proto.RepositoryOptions{})
	is.NoErr(err)
	_, err = be.CreateRepository(ctx, "anon2", nil, proto.RepositoryOptions{})
	is.NoErr(err)
	_, err = be.CreateRepository(ctx, "anon3", nil, proto.RepositoryOptions{})
	is.True(errors.Is(err, proto.ErrRepoLimitReached))
	_, err = be.ImportRepository(ctx, "anon4", nil, offlineRemote, proto.RepositoryOptions{})
	is.True(errors.Is(err, proto.ErrRepoLimitReached))

	r, err := be.Repository(ctx, "anon1")
	is.NoErr(err)
	is.Equal(r.UserID(), admin.ID())
}

// Concurrent creates by one user cannot together go past the limit. On
// Postgres (SOFT_SERVE_DB_DRIVER=postgres) this relies on LockUserRepos:
// in READ COMMITTED each create would otherwise count without seeing the
// others' uncommitted rows.
func TestMaxReposPerUserConcurrent(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		be, cfg := newTestBackend(t)
		testMaxReposPerUserConcurrent(t, be, cfg, backendTestContext(be, cfg))
	})
	t.Run("postgres", func(t *testing.T) {
		e := newPostgresWebhookEnv(t)
		testMaxReposPerUserConcurrent(t, e.be, e.cfg, e.ctx)
	})
}

func testMaxReposPerUserConcurrent(t *testing.T, be *Backend, cfg *config.Config, ctx context.Context) {
	is := is.New(t)
	cfg.MaxReposPerUser = 3
	rw := access.ReadWriteAccess
	cfg.AnonAccess = &rw

	alice, err := be.CreateUser(ctx, "alice", proto.UserOptions{})
	is.NoErr(err)

	for _, user := range []proto.User{alice, nil} {
		const n = 10
		var wg sync.WaitGroup
		errs := make([]error, n)
		prefix := "anon"
		if user != nil {
			prefix = user.Username()
		}
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = be.CreateRepository(ctx, fmt.Sprintf("%s-%d", prefix, i), user, proto.RepositoryOptions{})
			}()
		}
		wg.Wait()

		var ok int
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case !errors.Is(err, proto.ErrRepoLimitReached):
				t.Fatalf("unexpected error: %v", err)
			}
		}
		is.Equal(ok, 3)
		got, err := be.countRepos(ctx, be.db, user)
		is.NoErr(err)
		is.Equal(got, int64(3))
	}
}
