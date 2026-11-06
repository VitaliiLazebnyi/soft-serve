package backend

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	charmlog "charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/hooks"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/ssrf"
	"github.com/charmbracelet/soft-serve/pkg/webhook"
	"github.com/matryer/is"
	"golang.org/x/crypto/ssh"
)

const (
	zeroSha = "0000000000000000000000000000000000000000"
	someSha = "1111111111111111111111111111111111111111"
)

// seedHook stores a webhook directly, so its delivery attempts go to a
// loopback address that the SSRF-protected client refuses. The refusal is
// recorded as a delivery, which lets tests see which events were sent.
func (e whEnv) seedHook(t *testing.T, events ...webhook.Event) int64 {
	t.Helper()
	return e.seedHookOn(t, e.repo, events...)
}

// seedHookOn is seedHook for a given repository.
func (e whEnv) seedHookOn(t *testing.T, repo proto.Repository, events ...webhook.Event) int64 {
	t.Helper()
	is := is.New(t)
	var id int64
	is.NoErr(e.be.db.TransactionContext(e.ctx, func(tx *db.Tx) error {
		var err error
		id, err = e.be.store.CreateWebhook(e.ctx, tx, repo.ID(), "http://127.0.0.1:1/", "", int(webhook.ContentTypeJSON), true)
		if err != nil {
			return err
		}
		evs := make([]int, len(events))
		for i, ev := range events {
			evs[i] = int(ev)
		}
		return e.be.store.CreateWebhookEvents(e.ctx, tx, id, evs)
	}))
	return id
}

// deliveredEvents returns the events of every delivery recorded for a webhook.
func (e whEnv) deliveredEvents(t *testing.T, hookID int64) []webhook.Event {
	t.Helper()
	is := is.New(t)
	list, err := e.be.ListWebhookDeliveries(e.ctx, e.repo, hookID)
	is.NoErr(err)
	out := make([]webhook.Event, 0, len(list))
	for _, d := range list {
		out = append(out, d.Event)
	}
	return out
}

// waitFor polls cond until it holds. Changes made through the backend send
// their events in the background, so tests wait for those sends to finish.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// awaitDeliveries waits until the webhook has recorded exactly want.
func (e whEnv) awaitDeliveries(t *testing.T, hookID int64, want ...webhook.Event) {
	t.Helper()
	waitFor(t, "deliveries", func() bool {
		return slices.Equal(e.deliveredEvents(t, hookID), want)
	})
}

// awaitLog waits until the log contains text.
func (e whEnv) awaitLog(t *testing.T, text string) {
	t.Helper()
	waitFor(t, "log line "+text, func() bool {
		return strings.Contains(e.logs.String(), text)
	})
}

// A branch or tag creation sends branch_tag_create. The update hook is run
// as the admin named by SOFT_SERVE_USERNAME.
func TestUpdateHookSendsBranchTagCreate(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	t.Setenv("SOFT_SERVE_USERNAME", "admin")
	t.Setenv("SOFT_SERVE_PUBLIC_KEY", "")
	id := e.seedHook(t, webhook.EventBranchTagCreate)

	e.be.Update(e.ctx, io.Discard, io.Discard, e.repo.Name(), hooks.HookArg{
		OldSha: zeroSha, NewSha: someSha, RefName: "refs/heads/feature",
	})

	is.Equal(e.deliveredEvents(t, id), []webhook.Event{webhook.EventBranchTagCreate})
}

// A branch or tag deletion sends branch_tag_delete.
func TestUpdateHookSendsBranchTagDelete(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	t.Setenv("SOFT_SERVE_USERNAME", "admin")
	id := e.seedHook(t, webhook.EventBranchTagDelete)

	e.be.Update(e.ctx, io.Discard, io.Discard, e.repo.Name(), hooks.HookArg{
		OldSha: someSha, NewSha: zeroSha, RefName: "refs/heads/feature",
	})

	is.Equal(e.deliveredEvents(t, id), []webhook.Event{webhook.EventBranchTagDelete})
}

// A plain ref update sends no branch_tag event.
func TestUpdateHookPlainUpdateSendsNoBranchTag(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	t.Setenv("SOFT_SERVE_USERNAME", "admin")
	id := e.seedHook(t, webhook.EventBranchTagCreate, webhook.EventBranchTagDelete)

	e.be.Update(e.ctx, io.Discard, io.Discard, e.repo.Name(), hooks.HookArg{
		OldSha: someSha, NewSha: someSha, RefName: "refs/heads/feature",
	})

	is.Equal(len(e.deliveredEvents(t, id)), 0)
}

// Failing to send the event is logged, not returned, so the hook still
// completes. Dropping the webhooks table makes the lookup fail.
func TestUpdateHookSurvivesSendFailure(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	t.Setenv("SOFT_SERVE_USERNAME", "admin")
	e.seedHook(t, webhook.EventBranchTagCreate)
	_, err := e.be.db.ExecContext(e.ctx, "DROP TABLE webhooks")
	is.NoErr(err)

	e.be.Update(e.ctx, io.Discard, io.Discard, e.repo.Name(), hooks.HookArg{
		OldSha: zeroSha, NewSha: someSha, RefName: "refs/heads/feature",
	})

	is.True(strings.Contains(e.logs.String(), "error sending branch_tag webhook"))
}

// A hook for an unknown user is logged and nothing is sent.
func TestUpdateHookUnknownUser(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	t.Setenv("SOFT_SERVE_USERNAME", "nobody")
	id := e.seedHook(t, webhook.EventBranchTagCreate)

	e.be.Update(e.ctx, io.Discard, io.Discard, e.repo.Name(), hooks.HookArg{
		OldSha: zeroSha, NewSha: someSha, RefName: "refs/heads/feature",
	})

	is.Equal(len(e.deliveredEvents(t, id)), 0)
}

func TestRenameRepositorySendsRepositoryEvent(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	id := e.seedHook(t, webhook.EventRepository)

	is.NoErr(e.be.RenameRepository(e.ctx, "repo", "renamed"))

	e.awaitDeliveries(t, id, webhook.EventRepository)
}

// A failed event does not undo a committed rename, and the caller sees success.
func TestRenameRepositoryKeepsChangeWhenEventFails(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	e.seedHook(t, webhook.EventRepository)
	_, err := e.be.db.ExecContext(e.ctx, "DROP TABLE webhook_deliveries")
	is.NoErr(err)

	is.NoErr(e.be.RenameRepository(e.ctx, "repo", "renamed"))
	_, err = e.be.Repository(e.ctx, "renamed")
	is.NoErr(err)
	_, err = e.be.Repository(e.ctx, "repo")
	is.True(errors.Is(err, proto.ErrRepoNotFound))
	e.awaitLog(t, "webhook event not delivered")
}

// A failed event does not undo a committed visibility change.
func TestSetPrivateKeepsChangeWhenEventFails(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	e.seedHook(t, webhook.EventRepositoryVisibilityChange)
	_, err := e.be.db.ExecContext(e.ctx, "DROP TABLE webhook_deliveries")
	is.NoErr(err)

	is.NoErr(e.be.SetPrivate(e.ctx, "repo", true))
	repo, err := e.be.Repository(e.ctx, "repo")
	is.NoErr(err)
	is.True(repo.IsPrivate())
	e.awaitLog(t, "webhook event not delivered")
}

// The repository-deleted event goes to the subscribers that were read before
// the rows were removed. The seeded receiver is a loopback address that the
// SSRF-protected client refuses, so the attempt shows up as a logged failure.
func TestDeleteRepositorySendsRepositoryEvent(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	e.seedHook(t, webhook.EventRepository)

	is.NoErr(e.be.DeleteRepository(e.ctx, "repo"))
	_, err := e.be.Repository(e.ctx, "repo")
	is.True(errors.Is(err, proto.ErrRepoNotFound))

	e.awaitLog(t, "webhook event not delivered")
}

// WaitForDeliveries returns only after the background sends have finished, so
// the failed attempt is already logged when it returns, with no polling.
func TestWaitForDeliveriesCompletesBackgroundSends(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	e.seedHook(t, webhook.EventRepository)

	is.NoErr(e.be.DeleteRepository(e.ctx, "repo"))
	e.be.WaitForDeliveries(e.ctx)

	is.True(strings.Contains(e.logs.String(), "webhook event not delivered"))
}

// A delivery that starts just before shutdown must be able to finish within
// the shutdown budget, so the budget must exceed the cap the SSRF-protected
// client puts on one request.
func TestShutdownDeliveryBudgetCoversSendTimeout(t *testing.T) {
	is := is.New(t)
	is.True(ShutdownDeliveryTimeout > ssrf.ClientTimeout)
}

// Once shutdown has started waiting, a change made by a request that is still
// running is committed, but its event is skipped rather than added to the
// deliveries being waited on.
func TestChangesAfterShutdownStartSkipDelivery(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	_, err := e.be.CreateUser(e.ctx, "bob", proto.UserOptions{})
	is.NoErr(err)
	id := e.seedHook(t, webhook.EventCollaborator)

	e.be.WaitForDeliveries(e.ctx)
	is.NoErr(e.be.AddCollaborator(e.ctx, "repo", "bob", access.ReadWriteAccess))

	is.True(strings.Contains(e.logs.String(), "webhook event skipped: server is shutting down"))
	is.Equal(len(e.deliveredEvents(t, id)), 0)
}

// Deleting a repository removes its webhooks with their events and deliveries,
// even on a connection that does not enforce foreign keys. No delivery row is
// written for the repository event, since its webhook is gone; the attempt is
// sent unrecorded, so its refusal is logged instead.
func TestDeleteRepositoryRemovesWebhookRows(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	foreignKeysOff(t, e)
	t.Setenv("SOFT_SERVE_USERNAME", "admin")
	t.Setenv("SOFT_SERVE_PUBLIC_KEY", "")
	id := e.seedHook(t, webhook.EventBranchTagCreate, webhook.EventRepository)
	e.be.Update(e.ctx, io.Discard, io.Discard, e.repo.Name(), hooks.HookArg{
		OldSha: zeroSha, NewSha: someSha, RefName: "refs/heads/feature",
	})
	is.Equal(e.deliveredEvents(t, id), []webhook.Event{webhook.EventBranchTagCreate})

	is.NoErr(e.be.DeleteRepository(e.ctx, "repo"))
	e.be.WaitForDeliveries(e.ctx)
	is.True(strings.Contains(e.logs.String(), "webhook event not delivered"))

	for _, q := range []string{
		"SELECT COUNT(*) FROM webhooks WHERE id = ?",
		"SELECT COUNT(*) FROM webhook_events WHERE webhook_id = ?",
		"SELECT COUNT(*) FROM webhook_deliveries WHERE webhook_id = ?",
	} {
		var n int
		is.NoErr(e.be.db.GetContext(e.ctx, &n, q, id))
		is.Equal(n, 0)
	}
}

func TestCollaboratorEventsAddAndRemove(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	_, err := e.be.CreateUser(e.ctx, "bob", proto.UserOptions{})
	is.NoErr(err)
	id := e.seedHook(t, webhook.EventCollaborator)

	is.NoErr(e.be.AddCollaborator(e.ctx, "repo", "bob", access.ReadWriteAccess))
	is.NoErr(e.be.RemoveCollaborator(e.ctx, "repo", "bob"))

	e.awaitDeliveries(t, id, webhook.EventCollaborator, webhook.EventCollaborator)
}

// A failed event does not undo committed collaborator changes.
func TestCollaboratorChangesKeepWhenEventFails(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	bob, err := e.be.CreateUser(e.ctx, "bob", proto.UserOptions{})
	is.NoErr(err)
	e.seedHook(t, webhook.EventCollaborator)
	_, err = e.be.db.ExecContext(e.ctx, "DROP TABLE webhook_deliveries")
	is.NoErr(err)

	is.NoErr(e.be.AddCollaborator(e.ctx, "repo", "bob", access.ReadWriteAccess))
	is.Equal(e.be.AccessLevelForUser(e.ctx, "repo", bob), access.ReadWriteAccess)

	is.NoErr(e.be.RemoveCollaborator(e.ctx, "repo", "bob"))
	// The repository is public, so a non-collaborator gets read-only.
	is.Equal(e.be.AccessLevelForUser(e.ctx, "repo", bob), access.ReadOnlyAccess)
	e.awaitLog(t, "webhook event not delivered")
}

func TestRemoveCollaboratorNotFound(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	_, err := e.be.CreateUser(e.ctx, "bob", proto.UserOptions{})
	is.NoErr(err)
	id := e.seedHook(t, webhook.EventCollaborator)

	is.True(errors.Is(e.be.RemoveCollaborator(e.ctx, "repo", "bob"), proto.ErrCollaboratorNotFound))
	e.be.WaitForDeliveries(e.ctx)
	is.Equal(len(e.deliveredEvents(t, id)), 0)
}

// makeCommit writes one commit into the bare repository at path and points
// refs/heads/main at it, so push events have real commits to describe.
func makeCommit(t *testing.T, path string) string {
	t.Helper()
	is := is.New(t)
	git := func(stdin string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"--git-dir", path}, args...)...) //nolint:gosec
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.Output()
		is.NoErr(err)
		return strings.TrimSpace(string(out))
	}
	blob := git("hello\n", "hash-object", "-w", "--stdin")
	tree := git("", "mktree")
	tree = git("100644 blob "+blob+"\tREADME.md\n", "mktree")
	commit := git("", "commit-tree", tree, "-m", "first")
	git("", "update-ref", "refs/heads/main", commit)
	return commit
}

// A push that moves a branch sends a push event describing its commits.
func TestUpdateHookSendsPushEvent(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	t.Setenv("SOFT_SERVE_USERNAME", "admin")
	id := e.seedHook(t, webhook.EventPush)
	commit := makeCommit(t, e.be.repoPath("repo"))

	e.be.Update(e.ctx, io.Discard, io.Discard, "repo", hooks.HookArg{
		OldSha: zeroSha, NewSha: commit, RefName: "refs/heads/main",
	})

	is.Equal(e.deliveredEvents(t, id), []webhook.Event{webhook.EventPush})
}

// A new branch on a repository with commits sends both branch_tag_create and
// push, in that order.
func TestUpdateHookSendsBranchTagThenPush(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	t.Setenv("SOFT_SERVE_USERNAME", "admin")
	id := e.seedHook(t, webhook.EventBranchTagCreate, webhook.EventPush)
	commit := makeCommit(t, e.be.repoPath("repo"))

	e.be.Update(e.ctx, io.Discard, io.Discard, "repo", hooks.HookArg{
		OldSha: zeroSha, NewSha: commit, RefName: "refs/heads/main",
	})

	is.Equal(e.deliveredEvents(t, id), []webhook.Event{webhook.EventBranchTagCreate, webhook.EventPush})
}

// A failed push event send is logged and does not stop the hook.
func TestUpdateHookSurvivesPushSendFailure(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	t.Setenv("SOFT_SERVE_USERNAME", "admin")
	e.seedHook(t, webhook.EventPush)
	commit := makeCommit(t, e.be.repoPath("repo"))
	_, err := e.be.db.ExecContext(e.ctx, "DROP TABLE webhooks")
	is.NoErr(err)

	e.be.Update(e.ctx, io.Discard, io.Discard, "repo", hooks.HookArg{
		OldSha: zeroSha, NewSha: commit, RefName: "refs/heads/main",
	})

	is.True(strings.Contains(e.logs.String(), "error sending push webhook"))
}

// A hook for a repository that does not exist is logged and does nothing.
func TestUpdateHookUnknownRepository(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	t.Setenv("SOFT_SERVE_USERNAME", "admin")
	id := e.seedHook(t, webhook.EventBranchTagCreate, webhook.EventPush)

	e.be.Update(e.ctx, io.Discard, io.Discard, "missing", hooks.HookArg{
		OldSha: zeroSha, NewSha: someSha, RefName: "refs/heads/feature",
	})

	is.True(strings.Contains(e.logs.String(), "error finding repository"))
	is.Equal(len(e.deliveredEvents(t, id)), 0)
}

// The hook identifies the user by public key when SOFT_SERVE_PUBLIC_KEY is
// set. A known key sends the event as that user.
func TestUpdateHookIdentifiesUserByPublicKey(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	pub := whTestKey(t)
	_, err := e.be.CreateUser(e.ctx, "keyed", proto.UserOptions{PublicKeys: []ssh.PublicKey{pub}})
	is.NoErr(err)
	t.Setenv("SOFT_SERVE_USERNAME", "")
	t.Setenv("SOFT_SERVE_PUBLIC_KEY", string(ssh.MarshalAuthorizedKey(pub)))
	id := e.seedHook(t, webhook.EventBranchTagCreate)

	e.be.Update(e.ctx, io.Discard, io.Discard, e.repo.Name(), hooks.HookArg{
		OldSha: zeroSha, NewSha: someSha, RefName: "refs/heads/feature",
	})

	is.Equal(e.deliveredEvents(t, id), []webhook.Event{webhook.EventBranchTagCreate})
}

// An unparseable public key stops the hook before any event is sent.
func TestUpdateHookInvalidPublicKey(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	t.Setenv("SOFT_SERVE_USERNAME", "")
	t.Setenv("SOFT_SERVE_PUBLIC_KEY", "not a key")
	id := e.seedHook(t, webhook.EventBranchTagCreate)

	e.be.Update(e.ctx, io.Discard, io.Discard, e.repo.Name(), hooks.HookArg{
		OldSha: zeroSha, NewSha: someSha, RefName: "refs/heads/feature",
	})

	is.Equal(len(e.deliveredEvents(t, id)), 0)
}

// A public key with no user row, such as an initial_admin_keys entry with no
// account, still sends the event: the SSH layer authorized the push.
func TestUpdateHookUnknownPublicKey(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	t.Setenv("SOFT_SERVE_USERNAME", "")
	t.Setenv("SOFT_SERVE_PUBLIC_KEY", string(ssh.MarshalAuthorizedKey(whTestKey(t))))
	id := e.seedHook(t, webhook.EventBranchTagCreate)

	e.be.Update(e.ctx, io.Discard, io.Discard, e.repo.Name(), hooks.HookArg{
		OldSha: zeroSha, NewSha: someSha, RefName: "refs/heads/feature",
	})

	is.Equal(e.deliveredEvents(t, id), []webhook.Event{webhook.EventBranchTagCreate})
}

// Collaborator changes validate the username before anything is written.
func TestAddCollaboratorRejectsInvalidUsername(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	is.True(e.be.AddCollaborator(e.ctx, "repo", "bad name!", access.ReadWriteAccess) != nil)
}

// Adding a collaborator twice is refused, and no second event is sent.
func TestAddCollaboratorTwice(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	_, err := e.be.CreateUser(e.ctx, "bob", proto.UserOptions{})
	is.NoErr(err)
	id := e.seedHook(t, webhook.EventCollaborator)
	is.NoErr(e.be.AddCollaborator(e.ctx, "repo", "bob", access.ReadWriteAccess))

	is.True(errors.Is(e.be.AddCollaborator(e.ctx, "repo", "bob", access.ReadWriteAccess), proto.ErrCollaboratorExist))
	e.awaitDeliveries(t, id, webhook.EventCollaborator)
}

// whTestKey returns a fresh ed25519 public key for a test user.
func whTestKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	is := is.New(t)
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	is.NoErr(err)
	signer, err := ssh.NewSignerFromKey(priv)
	is.NoErr(err)
	return signer.PublicKey()
}

// With both SHAs zero the event is classified as a creation, because the old
// SHA is checked first. This documents that classification.
func TestUpdateHookBothShasZeroIsCreate(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	t.Setenv("SOFT_SERVE_USERNAME", "admin")
	id := e.seedHook(t, webhook.EventBranchTagCreate, webhook.EventBranchTagDelete)

	e.be.Update(e.ctx, io.Discard, io.Discard, e.repo.Name(), hooks.HookArg{
		OldSha: zeroSha, NewSha: zeroSha, RefName: "refs/heads/feature",
	})

	is.Equal(e.deliveredEvents(t, id), []webhook.Event{webhook.EventBranchTagCreate})
}

// Building an event looks up the repository owner. Foreign keys stop the owner
// row being removed normally, so corruptOwner switches them off for the delete.
// A rename whose event cannot be built is still committed.
func TestRenameRepositoryKeepsChangeWhenEventBuildFails(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	corruptOwner(t, e)

	is.NoErr(e.be.RenameRepository(e.ctx, "repo", "renamed"))
	_, err := e.be.Repository(e.ctx, "renamed")
	is.NoErr(err)
	is.True(strings.Contains(e.logs.String(), "rename committed but webhook event skipped"))
}

// Building the delete event fails before anything is removed, so the delete
// returns the error and the repository is kept.
func TestDeleteRepositoryReturnsEventBuildFailure(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	corruptOwner(t, e)

	is.True(e.be.DeleteRepository(e.ctx, "repo") != nil)
	_, err := e.be.Repository(e.ctx, "repo")
	is.NoErr(err)
}

// corruptOwner removes the repository owner's row with foreign keys switched
// off, which is the only way to make event building fail. The pool is pinned
// to one connection so the pragma applies to the statements that follow it.
func corruptOwner(t *testing.T, e whEnv) {
	t.Helper()
	is := is.New(t)
	e.be.db.SetMaxOpenConns(1)
	_, err := e.be.db.ExecContext(e.ctx, "PRAGMA foreign_keys = OFF")
	is.NoErr(err)
	_, err = e.be.db.ExecContext(e.ctx, "DELETE FROM users WHERE username = 'admin'")
	is.NoErr(err)
	_, err = e.be.db.ExecContext(e.ctx, "PRAGMA foreign_keys = ON")
	is.NoErr(err)
}

// Adding a collaborator whose event cannot be built still commits and returns
// nil. Removing one fails before any change, so it returns the error.
func TestCollaboratorEventBuildFailure(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	bob, err := e.be.CreateUser(e.ctx, "bob", proto.UserOptions{})
	is.NoErr(err)
	corruptOwner(t, e)

	is.NoErr(e.be.AddCollaborator(e.ctx, "repo", "bob", access.ReadWriteAccess))
	is.Equal(e.be.AccessLevelForUser(e.ctx, "repo", bob), access.ReadWriteAccess)

	is.True(e.be.RemoveCollaborator(e.ctx, "repo", "bob") != nil)
	is.Equal(e.be.AccessLevelForUser(e.ctx, "repo", bob), access.ReadWriteAccess)
	is.True(strings.Contains(e.logs.String(), "collaborator added but webhook event skipped"))
}

// A failing collaborator table makes both operations return the error.
func TestCollaboratorStoreFailure(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	_, err := e.be.CreateUser(e.ctx, "bob", proto.UserOptions{})
	is.NoErr(err)
	_, err = e.be.db.ExecContext(e.ctx, "DROP TABLE collabs")
	is.NoErr(err)

	is.True(e.be.AddCollaborator(e.ctx, "repo", "bob", access.ReadWriteAccess) != nil)
	is.True(e.be.RemoveCollaborator(e.ctx, "repo", "bob") != nil)
}

// Removing a row that a trigger refuses to delete returns the database error.
func TestRemoveCollaboratorDeleteFailure(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	_, err := e.be.CreateUser(e.ctx, "bob", proto.UserOptions{})
	is.NoErr(err)
	is.NoErr(e.be.AddCollaborator(e.ctx, "repo", "bob", access.ReadWriteAccess))
	e.execTrigger(t, "fail_collab_delete", "BEFORE", "DELETE", "collabs")

	is.True(e.be.RemoveCollaborator(e.ctx, "repo", "bob") != nil)
}

// Changing visibility builds the event first; a failed owner lookup returns
// the error.
func TestSetPrivateReturnsEventBuildFailure(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	_, err := e.be.db.ExecContext(e.ctx, "DELETE FROM users WHERE username = 'admin'")
	is.NoErr(err)

	is.True(e.be.SetPrivate(e.ctx, "repo", true) != nil)
}

// If the collaborator row vanishes between the lookup and the delete, the
// removal reports not found. A trigger deletes the row first, so the outer
// delete matches nothing.
func TestRemoveCollaboratorRowVanishes(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	_, err := e.be.CreateUser(e.ctx, "bob", proto.UserOptions{})
	is.NoErr(err)
	is.NoErr(e.be.AddCollaborator(e.ctx, "repo", "bob", access.ReadWriteAccess))
	_, err = e.be.db.ExecContext(e.ctx, "CREATE TRIGGER vanish BEFORE DELETE ON collabs BEGIN DELETE FROM collabs WHERE rowid = OLD.rowid; END;")
	is.NoErr(err)

	is.True(errors.Is(e.be.RemoveCollaborator(e.ctx, "repo", "bob"), proto.ErrCollaboratorNotFound))
}

// A visibility change whose event cannot be built still commits and returns nil.
func TestSetPrivateKeepsChangeWhenEventBuildFails(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	corruptOwner(t, e)

	is.NoErr(e.be.SetPrivate(e.ctx, "repo", true))
	repo, err := e.be.Repository(e.ctx, "repo")
	is.NoErr(err)
	is.True(repo.IsPrivate())
	is.True(strings.Contains(e.logs.String(), "visibility change committed but webhook event skipped"))
}

// blockDeliveries replaces the backend's delivery queue with one of one
// worker and maxPending queued deliveries, and occupies the worker until the
// returned function is called.
func blockDeliveries(t *testing.T, e whEnv, maxPending int) func() {
	t.Helper()
	e.be.deliveries = newDeliveryQueue(1, maxPending, e.be.logger)
	release := make(chan struct{})
	started := make(chan struct{})
	e.be.deliveries.enqueue(0, func() {
		close(started)
		<-release
	})
	<-started
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	return unblock
}

// When every sender is busy, the event waits in the queue: the caller returns
// at once, and the event is sent once a sender is free.
func TestDeliveryWithBusySendersIsQueuedNotRunInCaller(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	id := e.seedHook(t, webhook.EventRepository)
	hooks, err := e.be.store.GetWebhooksByRepoID(e.ctx, e.be.db, e.repo.ID())
	is.NoErr(err)
	unblock := blockDeliveries(t, e, 10)

	const n = 3
	for i := 0; i < n; i++ {
		ev, err := webhook.NewRepositoryEvent(e.ctx, e.admin, e.repo, webhook.RepositoryEventActionRename)
		is.NoErr(err)
		e.be.deliverAfterChange(e.ctx, hooks, ev, true)
	}
	is.Equal(len(e.deliveredEvents(t, id)), 0) // queued, not run in the caller

	unblock()
	e.be.WaitForDeliveries(e.ctx)
	is.Equal(len(e.deliveredEvents(t, id)), n)
}

// When the queue is full too, the event is dropped without blocking the
// caller. The drop is logged and recorded as a failed delivery.
func TestDeliveryWithFullQueueIsDroppedAndRecorded(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	id := e.seedHook(t, webhook.EventRepository)
	hooks, err := e.be.store.GetWebhooksByRepoID(e.ctx, e.be.db, e.repo.ID())
	is.NoErr(err)
	blockDeliveries(t, e, 0)

	ev, err := webhook.NewRepositoryEvent(e.ctx, e.admin, e.repo, webhook.RepositoryEventActionRename)
	is.NoErr(err)
	e.be.deliverAfterChange(e.ctx, hooks, ev, true)

	is.True(strings.Contains(e.logs.String(), "webhook event dropped: delivery queue is full"))
	list, err := e.be.ListWebhookDeliveries(e.ctx, e.repo, id)
	is.NoErr(err)
	is.Equal(len(list), 1)
	d, err := e.be.WebhookDelivery(e.ctx, e.repo, id, list[0].ID)
	is.NoErr(err)
	is.Equal(d.RequestError.String, errDeliveryQueueFull.Error())
	is.Equal(d.Event, webhook.EventRepository)
}

// The queue never runs more than its workers at once, and runs every job.
func TestDeliveryQueueBoundsWorkers(t *testing.T) {
	is := is.New(t)
	q := newDeliveryQueue(2, 100, charmlog.New(io.Discard))
	var mu sync.Mutex
	running, peak, ran := 0, 0, 0
	for i := 0; i < 50; i++ {
		is.Equal(q.enqueue(int64(i), func() {
			mu.Lock()
			running++
			peak = max(peak, running)
			mu.Unlock()
			time.Sleep(time.Millisecond)
			mu.Lock()
			running--
			ran++
			mu.Unlock()
		}), enqueued)
	}
	_, finished := q.close()
	<-finished
	is.Equal(ran, 50)
	is.True(peak <= 2)
	is.Equal(q.enqueue(1, func() {}), enqueueClosed)
}

// blocker returns a job that signals started and then waits for release.
func blocker(started chan<- struct{}, release <-chan struct{}) func() {
	return func() {
		started <- struct{}{}
		<-release
	}
}

// A repository never has more than its share of the senders running, so
// another repository's delivery starts at once even while the first one has
// a backlog of deliveries that do not return.
func TestDeliveryQueueCapsRunningPerRepo(t *testing.T) {
	is := is.New(t)
	q := newDeliveryQueue(8, 100, charmlog.New(io.Discard))
	q.maxRepoRunning = 2
	started := make(chan struct{}, 5)
	release := make(chan struct{})
	for i := 0; i < 5; i++ {
		is.Equal(q.enqueue(1, blocker(started, release)), enqueued)
	}
	<-started
	<-started

	ran := make(chan struct{})
	is.Equal(q.enqueue(2, func() { close(ran) }), enqueued)
	<-ran

	q.mu.Lock()
	is.Equal(q.repos[1].running, 2)
	is.Equal(len(q.repos[1].queued), 3)
	q.mu.Unlock()
	is.Equal(len(started), 0)

	close(release)
	_, finished := q.close()
	<-finished
	is.Equal(len(started), 3) // the queued ones ran once a share was free
}

// Once a repository has its cap of deliveries queued or running, its next
// one is refused, while other repositories can still queue theirs.
func TestDeliveryQueueCapsPendingPerRepo(t *testing.T) {
	is := is.New(t)
	q := newDeliveryQueue(1, 100, charmlog.New(io.Discard))
	q.maxRepo = 3
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	is.Equal(q.enqueue(1, blocker(started, release)), enqueued)
	<-started

	is.Equal(q.enqueue(1, func() {}), enqueued)
	is.Equal(q.enqueue(1, func() {}), enqueued)
	is.Equal(q.enqueue(1, func() {}), enqueueRepoFull)
	ran := make(chan struct{})
	is.Equal(q.enqueue(2, func() { close(ran) }), enqueued)

	close(release)
	<-ran
	_, finished := q.close()
	<-finished
	is.Equal(len(q.repos), 0)
}

// Queued deliveries are taken from the repositories in turn, so a repository
// with a long backlog does not hold back one that queued later.
func TestDeliveryQueueTakesReposInTurn(t *testing.T) {
	is := is.New(t)
	q := newDeliveryQueue(1, 100, charmlog.New(io.Discard))
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	is.Equal(q.enqueue(0, blocker(started, release)), enqueued)
	<-started

	var order []int64 // written by the single worker only
	for _, repo := range []int64{1, 1, 1, 2} {
		is.Equal(q.enqueue(repo, func() { order = append(order, repo) }), enqueued)
	}
	close(release)
	_, finished := q.close()
	<-finished
	is.Equal(order, []int64{1, 2, 1, 1})
}

// An event of a repository that already has its cap of deliveries pending is
// dropped without blocking the caller, logged, and recorded as a failed
// delivery so it can be redelivered. Another repository's event is still
// queued and sent.
func TestDeliveryForBusyRepoIsDroppedAndRecorded(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	id := e.seedHook(t, webhook.EventRepository)
	hooks, err := e.be.store.GetWebhooksByRepoID(e.ctx, e.be.db, e.repo.ID())
	is.NoErr(err)
	other, err := e.be.CreateRepository(e.ctx, "other", e.admin, proto.RepositoryOptions{})
	is.NoErr(err)
	otherID := e.seedHookOn(t, other, webhook.EventRepository)
	otherHooks, err := e.be.store.GetWebhooksByRepoID(e.ctx, e.be.db, other.ID())
	is.NoErr(err)

	// The repository's one allowed delivery is running and does not return
	// until released.
	e.be.deliveries = newDeliveryQueue(4, 100, e.be.logger)
	e.be.deliveries.maxRepo = 1
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	is.Equal(e.be.deliveries.enqueue(e.repo.ID(), blocker(started, release)), enqueued)
	<-started

	ev, err := webhook.NewRepositoryEvent(e.ctx, e.admin, e.repo, webhook.RepositoryEventActionRename)
	is.NoErr(err)
	e.be.deliverAfterChange(e.ctx, hooks, ev, true)

	is.True(strings.Contains(e.logs.String(), "webhook event dropped: repository has too many pending deliveries"))
	list, err := e.be.ListWebhookDeliveries(e.ctx, e.repo, id)
	is.NoErr(err)
	is.Equal(len(list), 1)
	d, err := e.be.WebhookDelivery(e.ctx, e.repo, id, list[0].ID)
	is.NoErr(err)
	is.Equal(d.RequestError.String, errRepoDeliveriesFull.Error())
	is.Equal(d.Event, webhook.EventRepository)

	otherEv, err := webhook.NewRepositoryEvent(e.ctx, e.admin, other, webhook.RepositoryEventActionRename)
	is.NoErr(err)
	e.be.deliverAfterChange(e.ctx, otherHooks, otherEv, true)

	unblock()
	e.be.WaitForDeliveries(e.ctx)
	list, err = e.be.ListWebhookDeliveries(e.ctx, other, otherID)
	is.NoErr(err)
	is.Equal(len(list), 1)
	d, err = e.be.WebhookDelivery(e.ctx, other, otherID, list[0].ID)
	is.NoErr(err)
	// Sent, and refused by the SSRF guard, rather than dropped.
	is.True(d.RequestError.String != errRepoDeliveriesFull.Error())
	is.True(d.RequestError.String != errDeliveryQueueFull.Error())
	is.Equal(len(e.deliveredEvents(t, id)), 1) // the dropped one only
}

// A panicking delivery is logged and does not take the process down; the
// queue keeps running later deliveries.
func TestDeliveryPanicIsRecovered(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	is.Equal(e.be.deliveries.enqueue(1, func() { panic("boom") }), enqueued)
	ran := make(chan struct{})
	is.Equal(e.be.deliveries.enqueue(1, func() { close(ran) }), enqueued)
	<-ran
	e.be.WaitForDeliveries(e.ctx)
	is.True(strings.Contains(e.logs.String(), "webhook delivery panicked"))
}

// Shutdown logs how many deliveries it waits for. When its context ends first,
// as on a second signal, it returns at once and warns that some are left.
func TestWaitForDeliveriesLogsPendingAndStopsOnCancel(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	blockDeliveries(t, e, 1)
	is.Equal(e.be.deliveries.enqueue(1, func() {}), enqueued)

	ctx, cancel := context.WithCancel(e.ctx)
	cancel()
	start := time.Now()
	e.be.WaitForDeliveries(ctx)
	is.True(time.Since(start) < time.Second)

	logs := e.logs.String()
	is.True(strings.Contains(logs, "waiting for webhook deliveries"))
	is.True(strings.Contains(logs, "pending=2"))
	is.True(strings.Contains(logs, "shutting down with webhook deliveries still running"))
}

// A change made without a user in the context, as by an initial admin key
// that has no user row, still sends its event, with an empty sender.
func TestEventsWithoutUserHaveEmptySender(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	id := e.seedHook(t, webhook.EventRepository, webhook.EventRepositoryVisibilityChange)
	ctx := proto.WithUserContext(e.ctx, nil)

	is.NoErr(e.be.SetPrivate(ctx, "repo", true))
	is.NoErr(e.be.RenameRepository(ctx, "repo", "renamed"))
	e.be.WaitForDeliveries(e.ctx)

	// The two events may be sent by different senders, so in either order.
	is.Equal(sortedEvents(e.deliveredEvents(t, id)), sortedEvents([]webhook.Event{webhook.EventRepositoryVisibilityChange, webhook.EventRepository}))
	var bodies []string
	is.NoErr(e.be.db.SelectContext(e.ctx, &bodies, "SELECT request_body FROM webhook_deliveries WHERE webhook_id = ?", id))
	for _, b := range bodies {
		is.True(strings.Contains(b, `"sender":{"id":0,"username":""}`))
	}
	is.True(!strings.Contains(e.logs.String(), "panicked"))
}

// foreignKeysOff pins the pool to one connection and switches foreign keys off
// on it, so ON DELETE CASCADE does not remove dependent rows and only explicit
// deletes do.
func foreignKeysOff(t *testing.T, e whEnv) {
	t.Helper()
	is := is.New(t)
	e.be.db.SetMaxOpenConns(1)
	_, err := e.be.db.ExecContext(e.ctx, "PRAGMA foreign_keys = OFF")
	is.NoErr(err)
	var on int
	is.NoErr(e.be.db.GetContext(e.ctx, &on, "PRAGMA foreign_keys"))
	is.Equal(on, 0)
}

// A send whose webhook is deleted before it records must not leave a delivery
// row behind for the removed webhook.
func TestDeliveryRecordedAfterWebhookDeletedWritesNoRow(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	id := e.seedHook(t, webhook.EventRepository)
	hooks, err := e.be.store.GetWebhooksByRepoID(e.ctx, e.be.db, e.repo.ID())
	is.NoErr(err)
	ev, err := webhook.NewRepositoryEvent(e.ctx, e.admin, e.repo, webhook.RepositoryEventActionRename)
	is.NoErr(err)

	is.NoErr(e.be.DeleteWebhook(e.ctx, e.repo, id))
	is.NoErr(webhook.Deliver(e.ctx, hooks, ev))

	var n int
	is.NoErr(e.be.db.GetContext(e.ctx, &n, "SELECT COUNT(*) FROM webhook_deliveries WHERE webhook_id = ?", id))
	is.Equal(n, 0)
}

// With AdminOnlyCreate on, a non-admin's create and rename to a new name are
// refused before any change. The rename half is the meaningful one: the
// non-admin owns "mine", which has a repository hook, and a policy check
// that ran after the commit would leave "renamed" behind and deliver an
// event to that hook. (CreateRepository sends no event either way.)
// TestRenameRepositorySendsRepositoryEvent shows the same hook does receive
// the event when a rename really happens.
func TestRefusedCreateAndRenameSendNoRepositoryEvent(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	e.cfg.AdminOnlyCreate = true

	alice, err := e.be.CreateUser(e.ctx, "alice", proto.UserOptions{})
	is.NoErr(err)
	aliceCtx := proto.WithUserContext(e.ctx, alice)
	mine, err := seedCreate(e.be, e.ctx, "mine", alice, proto.RepositoryOptions{})
	is.NoErr(err)
	mineHook := e.seedHookOn(t, mine, webhook.EventRepository)

	before := e.logs.String()

	is.True(errors.Is(func() error {
		_, err := e.be.CreateRepository(aliceCtx, "junk", alice, proto.RepositoryOptions{})
		return err
	}(), proto.ErrUnauthorized))
	is.True(errors.Is(e.be.RenameRepository(aliceCtx, "mine", "renamed"), proto.ErrUnauthorized))
	e.be.WaitForDeliveries(e.ctx)

	// Nothing was created or renamed.
	_, err = e.be.Repository(e.ctx, "junk")
	is.True(err != nil)
	_, err = e.be.Repository(e.ctx, "mine")
	is.NoErr(err)
	_, err = e.be.Repository(e.ctx, "renamed")
	is.True(err != nil)

	// No delivery was recorded on the hook, and no webhook event was logged.
	mineDeliveries, err := e.be.ListWebhookDeliveries(e.ctx, mine, mineHook)
	is.NoErr(err)
	is.Equal(len(mineDeliveries), 0)
	is.True(!strings.Contains(e.logs.String()[len(before):], "webhook event"))
}
