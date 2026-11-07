package backend

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/utils"
)

// pushGates orders pushes to a repository against the removal of a
// repository created by an HTTP push that never sent refs. A push holds its
// repository's gate shared while it runs (BeginPush), and the removal holds
// it exclusively from its last check for refs until the repository is gone
// (RemovePushCreatedRepository). Without it, a push over SSH could write
// refs after the removal checked for them and before it deleted the
// directory, and the pushed refs would be lost.
type pushGates struct {
	mu    sync.Mutex
	gates map[string]*pushGate
}

// pushGate is one repository's gate. users counts the callers holding or
// waiting for it, so it can be dropped from the map when the last one is
// done.
type pushGate struct {
	rw    sync.RWMutex
	users int
}

// acquire returns the gate for name, creating it if needed, counted as used
// until release.
func (p *pushGates) acquire(name string) *pushGate {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.gates == nil {
		p.gates = make(map[string]*pushGate)
	}
	g, ok := p.gates[name]
	if !ok {
		g = &pushGate{}
		p.gates[name] = g
	}
	g.users++
	return g
}

// release undoes acquire.
func (p *pushGates) release(name string, g *pushGate) {
	p.mu.Lock()
	defer p.mu.Unlock()
	g.users--
	if g.users == 0 {
		delete(p.gates, name)
	}
}

// testHookBeforePushCreatedDelete, when set, runs in
// RemovePushCreatedRepository after the check for refs and before the
// transaction that checks the rest and deletes the repository, with the
// repository's gate held.
var testHookBeforePushCreatedDelete func()

// BeginPush marks a push to the existing repository repo as running until
// the returned function is called; RemovePushCreatedRepository does not
// remove a repository while a push to it runs. If a removal of the
// repository has started, BeginPush waits for it to end, and then returns
// proto.ErrRepoNotFound if the repository is gone or its name now belongs
// to another repository.
func (d *Backend) BeginPush(ctx context.Context, repo proto.Repository) (func(), error) {
	name := utils.SanitizeRepo(repo.Name())
	g := d.pushGates.acquire(name)
	g.rw.RLock()
	done := func() {
		g.rw.RUnlock()
		d.pushGates.release(name, g)
	}

	cur, err := d.Repository(ctx, name)
	if err != nil {
		done()
		return nil, err
	}
	if cur.ID() != repo.ID() {
		done()
		return nil, proto.ErrRepoNotFound
	}
	return done, nil
}

// RemovePushCreatedRepository deletes created, a repository an HTTP push
// created, if it was never used, and reports whether it did. It is kept if:
//
//   - a push to it is running (see BeginPush);
//   - it has a ref;
//   - its name now belongs to another repository;
//   - its project name, description, visibility, hidden or mirror flag or
//     owner differ from when it was created;
//   - it has a collaborator, a webhook, an LFS object or an LFS lock;
//   - any of this cannot be checked.
//
// The repository's settings are compared with created, rather than through
// repos.updated_at, because the setters do not update that column and its
// one-second resolution could not tell a change from the creation anyway.
func (d *Backend) RemovePushCreatedRepository(ctx context.Context, created proto.Repository) (bool, error) {
	name := utils.SanitizeRepo(created.Name())
	g := d.pushGates.acquire(name)
	defer d.pushGates.release(name, g)
	if !g.rw.TryLock() {
		return false, nil // a push is running; it may write refs
	}
	defer g.rw.Unlock()

	// Checked with the gate held: no push can start until the repository
	// is gone, so none can write a ref after this check.
	if hasRefs(d.repoPath(name)) {
		return false, nil
	}

	if testHookBeforePushCreatedDelete != nil {
		testHookBeforePushCreatedDelete()
	}

	// The rest is checked in the transaction that deletes the row, so a
	// setting, collaborator or webhook added meanwhile, or another
	// repository now under the name, is seen.
	err := d.deleteRepository(ctx, name, func(tx *db.Tx, m models.Repo) (bool, error) {
		return d.pushCreatedRepositoryUsed(ctx, tx, m, created)
	})
	switch {
	case errors.Is(err, errRepoKept):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

// pushCreatedRepositoryUsed reports whether m, the row now under created's
// name, is no longer created as it was made, or has anything recorded for
// it since.
func (d *Backend) pushCreatedRepositoryUsed(ctx context.Context, tx *db.Tx, m models.Repo, created proto.Repository) (bool, error) {
	if m.ID != created.ID() ||
		m.ProjectName != created.ProjectName() ||
		m.Description != created.Description() ||
		m.Private != created.IsPrivate() ||
		m.Hidden != created.IsHidden() ||
		m.Mirror != created.IsMirror() ||
		m.UserID.Int64 != created.UserID() {
		return true, nil
	}

	collabs, err := d.store.ListCollabsByRepo(ctx, tx, m.Name)
	if err != nil || len(collabs) > 0 {
		return true, err
	}
	hooks, err := d.store.GetWebhooksByRepoID(ctx, tx, m.ID)
	if err != nil || len(hooks) > 0 {
		return true, err
	}
	objs, err := d.store.GetLFSObjects(ctx, tx, m.ID)
	if err != nil || len(objs) > 0 {
		return true, err
	}
	locks, err := d.store.GetLFSLocks(ctx, tx, m.ID, 1, 1)
	if err != nil || len(locks) > 0 {
		return true, err
	}
	return false, nil
}

// hasRefs reports whether the repository at dir has any ref. A repository
// that cannot be read counts as having refs, so it is never removed by
// mistake.
func hasRefs(dir string) bool {
	out, err := git.NewCommand("for-each-ref", "--count=1", "--format=%(refname)").RunInDir(dir)
	if err != nil {
		return true
	}
	return strings.TrimSpace(string(out)) != ""
}
