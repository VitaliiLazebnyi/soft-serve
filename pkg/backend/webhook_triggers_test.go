package backend

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/hooks"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/webhook"
	"github.com/matryer/is"
	"golang.org/x/crypto/ssh"
)

const zeroSha = "0000000000000000000000000000000000000000"
const someSha = "1111111111111111111111111111111111111111"

// seedHook stores a webhook directly, so its delivery attempts go to a
// loopback address that the SSRF-protected client refuses. The refusal is
// recorded as a delivery, which lets tests see which events were sent.
func (e whEnv) seedHook(t *testing.T, events ...webhook.Event) int64 {
	t.Helper()
	is := is.New(t)
	var id int64
	is.NoErr(e.be.db.TransactionContext(e.ctx, func(tx *db.Tx) error {
		var err error
		id, err = e.be.store.CreateWebhook(e.ctx, tx, e.repo.ID(), "http://127.0.0.1:1/", "", int(webhook.ContentTypeJSON), true)
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

// A delivery that starts before shutdown must be able to finish within the
// shutdown budget. The per-request HTTP timeout is 30s (pkg/ssrf), and one
// event may take up to webhookSendTimeout across its hooks.
func TestShutdownDeliveryBudgetCoversSendTimeout(t *testing.T) {
	is := is.New(t)
	is.True(webhookSendTimeout >= 30*time.Second)
	is.True(ShutdownDeliveryTimeout >= webhookSendTimeout)
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
// written for the repository event, since its webhook is gone.
func TestDeleteRepositoryRemovesWebhookRows(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	t.Setenv("SOFT_SERVE_USERNAME", "admin")
	t.Setenv("SOFT_SERVE_PUBLIC_KEY", "")
	id := e.seedHook(t, webhook.EventBranchTagCreate, webhook.EventRepository)
	e.be.Update(e.ctx, io.Discard, io.Discard, e.repo.Name(), hooks.HookArg{
		OldSha: zeroSha, NewSha: someSha, RefName: "refs/heads/feature",
	})
	is.Equal(e.deliveredEvents(t, id), []webhook.Event{webhook.EventBranchTagCreate})

	is.NoErr(e.be.DeleteRepository(e.ctx, "repo"))

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
	is.Equal(len(e.deliveredEvents(t, id)), 0)
}

// makeCommit writes one commit into the bare repository at path and points
// refs/heads/main at it, so push events have real commits to describe.
func makeCommit(t *testing.T, path string) string {
	t.Helper()
	is := is.New(t)
	git := func(stdin string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"--git-dir", path}, args...)...) //nolint:gosec
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
}

// A hook for a repository that does not exist is logged and does nothing.
func TestUpdateHookUnknownRepository(t *testing.T) {
	e := newWebhookEnv(t)
	t.Setenv("SOFT_SERVE_USERNAME", "admin")

	e.be.Update(e.ctx, io.Discard, io.Discard, "missing", hooks.HookArg{
		OldSha: zeroSha, NewSha: someSha, RefName: "refs/heads/feature",
	})
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

// An unknown public key stops the hook before any event is sent.
func TestUpdateHookUnknownPublicKey(t *testing.T) {
	is := is.New(t)
	e := newWebhookEnv(t)
	t.Setenv("SOFT_SERVE_USERNAME", "")
	t.Setenv("SOFT_SERVE_PUBLIC_KEY", string(ssh.MarshalAuthorizedKey(whTestKey(t))))
	id := e.seedHook(t, webhook.EventBranchTagCreate)

	e.be.Update(e.ctx, io.Discard, io.Discard, e.repo.Name(), hooks.HookArg{
		OldSha: zeroSha, NewSha: someSha, RefName: "refs/heads/feature",
	})

	is.Equal(len(e.deliveredEvents(t, id)), 0)
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
