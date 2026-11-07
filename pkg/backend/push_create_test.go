package backend

import (
	"errors"
	"os/exec"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

// A repository created by an HTTP push is removed only if nothing was done
// with it since: no ref, no changed setting, no collaborator, webhook, LFS
// object or LFS lock.
func TestRemovePushCreatedRepositoryKeepsUsed(t *testing.T) {
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
	is.New(t).NoErr(err)
	_, err = be.CreateUser(ctx, "alice", proto.UserOptions{})
	is.New(t).NoErr(err)

	inTx := func(fn func(tx *db.Tx) error) func(t *testing.T, r proto.Repository) {
		return func(t *testing.T, _ proto.Repository) {
			is.New(t).NoErr(be.db.TransactionContext(ctx, fn))
		}
	}
	tests := map[string]func(t *testing.T, r proto.Repository){
		"untouched": nil,
		"ref": func(t *testing.T, r proto.Repository) {
			dir := be.repoPath(r.Name())
			tree, err := exec.CommandContext(t.Context(), "git", "-C", dir, "mktree").Output()
			is.New(t).NoErr(err)
			cmd := exec.CommandContext(t.Context(), "git", "-C", dir, "commit-tree", string(tree[:len(tree)-1]), "-m", "x")
			cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@b", "GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@b")
			commit, err := cmd.Output()
			is.New(t).NoErr(err)
			is.New(t).NoErr(exec.CommandContext(t.Context(), "git", "-C", dir, "update-ref", "refs/heads/main", string(commit[:len(commit)-1])).Run())
		},
		// The settings are changed in the store, as the backend's setters
		// do, without the webhook event those also deliver.
		"description": inTx(func(tx *db.Tx) error {
			return be.store.SetRepoDescriptionByName(ctx, tx, "repo-description", "about")
		}),
		"project-name": inTx(func(tx *db.Tx) error {
			return be.store.SetRepoProjectNameByName(ctx, tx, "repo-project-name", "Project")
		}),
		"private": inTx(func(tx *db.Tx) error {
			return be.store.SetRepoIsPrivateByName(ctx, tx, "repo-private", true)
		}),
		"hidden": inTx(func(tx *db.Tx) error {
			return be.store.SetRepoIsHiddenByName(ctx, tx, "repo-hidden", true)
		}),
		"collaborator": func(t *testing.T, r proto.Repository) {
			is.New(t).NoErr(be.AddCollaborator(ctx, r.Name(), "alice", access.ReadWriteAccess))
		},
		"webhook": func(t *testing.T, r proto.Repository) {
			inTx(func(tx *db.Tx) error {
				_, err := be.store.CreateWebhook(ctx, tx, r.ID(), "https://example.com/hook", "", 1, true)
				return err
			})(t, r)
		},
		"lfs-object": func(t *testing.T, r proto.Repository) {
			inTx(func(tx *db.Tx) error {
				return be.store.CreateLFSObject(ctx, tx, r.ID(), "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", 1)
			})(t, r)
		},
		"lfs-lock": func(t *testing.T, r proto.Repository) {
			inTx(func(tx *db.Tx) error {
				return be.store.CreateLFSLockForUser(ctx, tx, r.ID(), owner.ID(), "foo.png", "refs/heads/main")
			})(t, r)
		},
	}

	for name, use := range tests {
		t.Run(name, func(t *testing.T) {
			is := is.New(t)
			repoName := "repo-" + name
			r, err := be.CreateRepository(ctx, repoName, owner, proto.RepositoryOptions{})
			is.NoErr(err)
			if use != nil {
				use(t, r)
			}

			removed, err := be.RemovePushCreatedRepository(ctx, r)
			is.NoErr(err)
			_, err = be.Repository(ctx, repoName)
			if use == nil {
				is.True(removed)
				is.True(errors.Is(err, proto.ErrRepoNotFound))
			} else {
				is.True(!removed)
				is.NoErr(err) // the used repository is kept
			}
		})
	}
}

// A repository is not removed while a push to it runs, and a push that
// starts while it is being removed waits for that and then finds it gone,
// rather than writing refs into a directory about to be deleted.
func TestRemovePushCreatedRepositoryOrdersPushes(t *testing.T) {
	is := is.New(t)
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
	is.NoErr(err)
	r, err := be.CreateRepository(ctx, "repo", owner, proto.RepositoryOptions{})
	is.NoErr(err)

	// A push is running: the repository stays.
	done, err := be.BeginPush(ctx, r)
	is.NoErr(err)
	removed, err := be.RemovePushCreatedRepository(ctx, r)
	is.NoErr(err)
	is.True(!removed)
	done()

	// A push starts during the removal, after the last check for refs.
	pushErr := make(chan error, 1)
	testHookBeforePushCreatedDelete = func() {
		go func() {
			done, err := be.BeginPush(ctx, r)
			if err == nil {
				done()
			}
			pushErr <- err
		}()
		select {
		case err := <-pushErr:
			t.Errorf("push started during the removal: %v", err)
		case <-time.After(200 * time.Millisecond):
		}
	}
	t.Cleanup(func() { testHookBeforePushCreatedDelete = nil })

	removed, err = be.RemovePushCreatedRepository(ctx, r)
	is.NoErr(err)
	is.True(removed)
	is.True(errors.Is(<-pushErr, proto.ErrRepoNotFound)) // the push finds it gone
}

// A change that lands after the removal's checks for refs, and before the
// repository's row is deleted, still keeps it: the checks run in the
// transaction that deletes the row. A repository renamed away, with
// another created under its name, in that gap is not the one deleted.
func TestRemovePushCreatedRepositoryChecksWhenDeleting(t *testing.T) {
	be, cfg := newTestBackend(t)
	ctx := backendTestContext(be, cfg)
	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
	is.New(t).NoErr(err)
	_, err = be.CreateUser(ctx, "alice", proto.UserOptions{})
	is.New(t).NoErr(err)
	t.Cleanup(func() { testHookBeforePushCreatedDelete = nil })

	t.Run("collaborator", func(t *testing.T) {
		is := is.New(t)
		r, err := be.CreateRepository(ctx, "collab", owner, proto.RepositoryOptions{})
		is.NoErr(err)
		testHookBeforePushCreatedDelete = func() {
			is.NoErr(be.AddCollaborator(ctx, "collab", "alice", access.ReadWriteAccess))
		}

		removed, err := be.RemovePushCreatedRepository(ctx, r)
		is.NoErr(err)
		is.True(!removed)
		_, err = be.Repository(ctx, "collab")
		is.NoErr(err) // kept with its collaborator
	})

	t.Run("recreated", func(t *testing.T) {
		is := is.New(t)
		r, err := be.CreateRepository(ctx, "name", owner, proto.RepositoryOptions{})
		is.NoErr(err)
		var other proto.Repository
		testHookBeforePushCreatedDelete = func() {
			is.NoErr(be.RenameRepository(ctx, "name", "moved"))
			other, err = be.CreateRepository(ctx, "name", owner, proto.RepositoryOptions{})
			is.NoErr(err)
		}

		removed, err := be.RemovePushCreatedRepository(ctx, r)
		is.NoErr(err)
		is.True(!removed)
		cur, err := be.Repository(ctx, "name")
		is.NoErr(err) // the repository now under the name is kept
		is.Equal(cur.ID(), other.ID())
		_, err = be.Repository(ctx, "moved")
		is.NoErr(err)
	})
}
