package webhook

import (
	"errors"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/matryer/is"
)

const zeroHash = "0000000000000000000000000000000000000000"

// ownerLess is a repository whose owner does not exist, so the owner lookup
// in every builder fails.
func (e *testEnv) ownerLess() fakeRepo {
	return fakeRepo{id: e.repo.ID, name: e.repo.Name, userID: 999, path: ""}
}

func (e *testEnv) senderRepo(path string) fakeRepo {
	return fakeRepo{id: e.repo.ID, name: e.repo.Name, userID: e.owner.ID, path: path}
}

func (e *testEnv) sender() fakeUser {
	return fakeUser{id: e.owner.ID, name: e.owner.Username}
}

func TestNewBranchTagEventCreate(t *testing.T) {
	is := is.New(t)
	e := newTestEnv(t)

	ev, err := NewBranchTagEvent(e.ctx, e.sender(), e.senderRepo(""), "refs/heads/dev", zeroHash, "1111111111111111111111111111111111111111")
	is.NoErr(err)
	is.Equal(ev.EventType, EventBranchTagCreate)
	is.True(ev.Created)
	is.True(!ev.Deleted)
	is.Equal(ev.Ref, "refs/heads/dev")
	is.Equal(ev.Repository.Owner.Username, "alice")
	is.Equal(ev.Repository.HTTPURL, "http://127.0.0.1:23232/repo.git")
	is.Equal(ev.Repository.SSHURL, "ssh://127.0.0.1:23231/repo.git")
	is.Equal(ev.Repository.GitURL, "git://127.0.0.1:9418/repo.git")
	is.Equal(ev.Sender.Username, "alice")
}

func TestNewBranchTagEventDelete(t *testing.T) {
	is := is.New(t)
	e := newTestEnv(t)

	ev, err := NewBranchTagEvent(e.ctx, e.sender(), e.senderRepo(""), "refs/tags/v1", "1111111111111111111111111111111111111111", zeroHash)
	is.NoErr(err)
	is.Equal(ev.EventType, EventBranchTagDelete)
	is.True(ev.Deleted)
	is.True(!ev.Created)
}

func TestNewBranchTagEventRejectsUpdate(t *testing.T) {
	is := is.New(t)
	e := newTestEnv(t)

	// Neither side is zero: that is a plain update, not a branch or tag event.
	_, err := NewBranchTagEvent(e.ctx, e.sender(), e.senderRepo(""), "refs/heads/main",
		"1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222")
	is.True(err != nil)
	is.True(strings.Contains(err.Error(), "invalid branch or tag event"))
}

func TestNewBranchTagEventOwnerLookupFails(t *testing.T) {
	is := is.New(t)
	e := newTestEnv(t)

	_, err := NewBranchTagEvent(e.ctx, e.sender(), e.ownerLess(), "refs/heads/dev", zeroHash, "1111111111111111111111111111111111111111")
	is.True(err != nil)
}

func TestNewCollaboratorEvent(t *testing.T) {
	is := is.New(t)
	e := newTestEnv(t)
	is.NoErr(e.ds.CreateUser(e.ctx, e.dbx, "bob", false, nil))
	is.NoErr(e.ds.AddCollabByUsernameAndRepo(e.ctx, e.dbx, "bob", "repo", access.ReadWriteAccess))

	ev, err := NewCollaboratorEvent(e.ctx, e.sender(), e.senderRepo(""), "bob", CollaboratorEventAdded)
	is.NoErr(err)
	is.Equal(ev.EventType, EventCollaborator)
	is.Equal(ev.Action, CollaboratorEventAdded)
	is.Equal(ev.Collaborator.Username, "bob")
	is.True(ev.Collaborator.ID != 0)
	is.Equal(ev.AccessLevel, access.ReadWriteAccess)
	is.Equal(ev.Repository.Owner.Username, "alice")
}

func TestNewCollaboratorEventUnknownCollaborator(t *testing.T) {
	is := is.New(t)
	e := newTestEnv(t)

	_, err := NewCollaboratorEvent(e.ctx, e.sender(), e.senderRepo(""), "nobody", CollaboratorEventRemoved)
	is.True(err != nil)
}

func TestNewCollaboratorEventOwnerLookupFails(t *testing.T) {
	is := is.New(t)
	e := newTestEnv(t)

	_, err := NewCollaboratorEvent(e.ctx, e.sender(), e.ownerLess(), "alice", CollaboratorEventAdded)
	is.True(err != nil)
}

func TestNewRepositoryEventVisibilityChange(t *testing.T) {
	is := is.New(t)
	e := newTestEnv(t)

	ev, err := NewRepositoryEvent(e.ctx, e.sender(), e.senderRepo(""), RepositoryEventActionVisibilityChange)
	is.NoErr(err)
	is.Equal(ev.EventType, EventRepositoryVisibilityChange)
	is.Equal(ev.Action, RepositoryEventActionVisibilityChange)
}

func TestNewRepositoryEventOtherActions(t *testing.T) {
	is := is.New(t)
	e := newTestEnv(t)

	for _, action := range []RepositoryEventAction{
		RepositoryEventActionDelete,
		RepositoryEventActionRename,
		RepositoryEventActionDefaultBranchChange,
	} {
		ev, err := NewRepositoryEvent(e.ctx, e.sender(), e.senderRepo(""), action)
		is.NoErr(err)
		is.Equal(ev.EventType, EventRepository)
		is.Equal(ev.Action, action)
	}
}

func TestNewRepositoryEventOwnerLookupFails(t *testing.T) {
	is := is.New(t)
	e := newTestEnv(t)

	_, err := NewRepositoryEvent(e.ctx, e.sender(), e.ownerLess(), RepositoryEventActionDelete)
	is.True(err != nil)
}

func TestNewPushEventFirstPush(t *testing.T) {
	is := is.New(t)
	e := newTestEnv(t)
	path, head := gitRepo(t)

	ev, err := NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/main", zeroHash, head)
	is.NoErr(err)
	is.Equal(ev.EventType, EventPush)
	is.Equal(ev.Ref, "refs/heads/main")
	is.Equal(ev.After, head)
	is.Equal(ev.Repository.DefaultBranch, "main")
	is.Equal(ev.Repository.Owner.Username, "alice")

	// A first push with no earlier commits lists the whole history, newest first.
	is.Equal(len(ev.Commits), 2)
	is.Equal(ev.Commits[0].ID, head)
	is.Equal(ev.Commits[0].Title, "second commit")
	is.Equal(ev.Commits[1].Title, "first commit")
	is.Equal(strings.TrimSpace(ev.Commits[1].Message), "first commit\n\nbody of first")
	is.Equal(ev.Commits[1].Author.Name, "Alice")
	is.Equal(ev.Commits[1].Author.Email, "alice@example.com")
	is.True(!ev.Commits[1].Timestamp.IsZero())
}

func TestNewPushEventRangePush(t *testing.T) {
	is := is.New(t)
	e := newTestEnv(t)
	path, head := gitRepo(t)
	first := gitOutput(t, path, "rev-parse", "main~1")

	ev, err := NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/main", first, head)
	is.NoErr(err)
	// Only commits after `before` are listed.
	is.Equal(len(ev.Commits), 1)
	is.Equal(ev.Commits[0].Title, "second commit")
}

func TestNewPushEventUnknownAfterFails(t *testing.T) {
	is := is.New(t)
	e := newTestEnv(t)
	path, _ := gitRepo(t)

	_, err := NewPushEvent(e.ctx, e.sender(), e.senderRepo(path), "refs/heads/main", zeroHash, "3333333333333333333333333333333333333333")
	is.True(err != nil)
}

func TestNewPushEventOpenFails(t *testing.T) {
	is := is.New(t)
	e := newTestEnv(t)
	repo := e.senderRepo("")
	repo.openErr = errors.New("cannot open repository")

	_, err := NewPushEvent(e.ctx, e.sender(), repo, "refs/heads/main", zeroHash, "1111111111111111111111111111111111111111")
	is.True(err != nil)
}

func TestNewPushEventOwnerLookupFails(t *testing.T) {
	is := is.New(t)
	e := newTestEnv(t)

	_, err := NewPushEvent(e.ctx, e.sender(), e.ownerLess(), "refs/heads/main", zeroHash, "1111111111111111111111111111111111111111")
	is.True(err != nil)
}

func TestGetDefaultBranch(t *testing.T) {
	is := is.New(t)
	path, _ := gitRepo(t)

	branch, err := getDefaultBranch(fakeRepo{path: path})
	is.NoErr(err)
	is.Equal(branch, "main")
}

func TestGetDefaultBranchEmptyRepository(t *testing.T) {
	is := is.New(t)
	empty := t.TempDir() + "/empty.git"
	_ = gitOutput(t, t.TempDir(), "init", "-q", "--bare", empty)

	// An empty repository has no branch yet: that is not an error.
	branch, err := getDefaultBranch(fakeRepo{path: empty})
	is.NoErr(err)
	is.Equal(branch, "")
}

func TestGetDefaultBranchOpenErrorReturned(t *testing.T) {
	is := is.New(t)

	_, err := getDefaultBranch(fakeRepo{openErr: errors.New("boom")})
	is.True(err != nil)
	is.True(strings.Contains(err.Error(), "boom"))
}
