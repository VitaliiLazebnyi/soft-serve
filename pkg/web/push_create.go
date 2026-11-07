package web

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"charm.land/log/v2"
	gitb "github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// pushCreateTimeout is how long a repository created by an HTTP push may
// stay without refs, with no request to it in flight, before it is removed.
// git sends the push in a separate request after the ref advertisement that
// created the repository, and git-lfs uploads objects in between, so the
// repository cannot be removed when the advertisement ends.
var pushCreateTimeout = 10 * time.Minute

// pushCreates tracks repositories created by HTTP push-to-create that have
// no refs yet. The SSH push creates and pushes in one session and deletes
// the repository when the push fails; over HTTP the push may fail in a later
// request or never arrive. A listed repository is removed when a
// receive-pack request to it fails and leaves no ref, or when nothing has
// reached it for pushCreateTimeout and it still has no ref. It is kept
// instead if it was used meanwhile or a push over SSH to it is running; see
// backend.RemovePushCreatedRepository. A receive-pack request that succeeds
// without updating a ref, such as the probe git sends before a pack larger
// than http.postBuffer, only restarts the timeout. A repository leaves the
// list once a request to it ends and it has a ref.
//
// The list is kept in memory, so a repository still on it when the server
// stops is kept, empty, until someone deletes it. That errs on the side of
// keeping: nothing on disk tells such a repository apart from one created
// empty on purpose.
type pushCreates struct {
	// mu guards the map and the entries' fields. It is never held while
	// running git or deleting a repository, since every HTTP git request
	// takes it.
	mu    sync.Mutex
	repos map[string]*pushCreate
}

// pushCreate is a repository created by an HTTP push that has no refs yet.
type pushCreate struct {
	// ctx is the creating request's context without its cancellation, used
	// to delete the repository after that request has ended.
	ctx context.Context
	// repo is the created repository as it was created. It is only deleted
	// if the name still belongs to it, not to one renamed or created in its
	// place since, and nothing about it changed.
	repo     proto.Repository
	dir      string
	inflight int
	// gen changes whenever a request starts or a decision is taken, so a
	// decision made outside the lock, or a timer, that is out of date does
	// nothing.
	gen   int
	timer *time.Timer
	// removing is closed once a removal that has started is over. Requests
	// to the repository wait for it rather than run against a repository
	// being deleted.
	removing chan struct{}
}

func newPushCreates() *pushCreates {
	return &pushCreates{repos: make(map[string]*pushCreate)}
}

// pushOutcome records whether a receive-pack request failed. withAccess puts
// one in the context of every request to a listed repository, and
// serviceRpc marks it.
type pushOutcome struct {
	failed bool
}

type pushOutcomeKey struct{}

// markPushFailed records in ctx that the receive-pack request failed.
func markPushFailed(ctx context.Context) {
	if o, ok := ctx.Value(pushOutcomeKey{}).(*pushOutcome); ok {
		o.failed = true
	}
}

// add records that the request in ctx created repo, under name, at dir.
func (p *pushCreates) add(ctx context.Context, name string, repo proto.Repository, dir string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.repos[name] = &pushCreate{ctx: context.WithoutCancel(ctx), repo: repo, dir: dir}
}

// begin marks a request to the repository name as in flight, so it is not
// removed under the request. It returns the request's context, carrying a
// pushOutcome, and the function to call when the request ends, or ctx and
// nil when name is not on the list.
func (p *pushCreates) begin(ctx context.Context, name string) (context.Context, func()) {
	if p == nil {
		return ctx, nil
	}
	p.mu.Lock()
	pc, ok := p.repos[name]
	if !ok {
		p.mu.Unlock()
		return ctx, nil
	}
	if pc.removing != nil {
		// Wait for the removal to end; the request then finds the
		// repository gone, or kept and off the list.
		ch := pc.removing
		p.mu.Unlock()
		<-ch
		return ctx, nil
	}
	pc.inflight++
	pc.gen++
	if pc.timer != nil {
		pc.timer.Stop()
		pc.timer = nil
	}
	p.mu.Unlock()
	o := &pushOutcome{}
	return context.WithValue(ctx, pushOutcomeKey{}, o), func() { p.end(name, pc, o.failed) }
}

// end is called when a request to a listed repository ends. Once no
// request is in flight, a repository with refs leaves the list. One
// without refs is removed if the request was a failed push, and otherwise
// gets pushCreateTimeout for the push to arrive.
func (p *pushCreates) end(name string, pc *pushCreate, failed bool) {
	p.mu.Lock()
	pc.inflight--
	if p.repos[name] != pc || pc.inflight > 0 || pc.removing != nil {
		p.mu.Unlock()
		return
	}
	pc.gen++
	gen := pc.gen
	p.mu.Unlock()

	refs := hasRefs(pc.dir)
	if failed && !refs {
		p.remove(name, pc, gen)
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.repos[name] != pc || pc.gen != gen {
		return // a request started meanwhile; its end decides
	}
	if refs {
		delete(p.repos, name)
		return
	}
	pc.timer = time.AfterFunc(pushCreateTimeout, func() { p.remove(name, pc, gen) })
}

// remove takes the repository off the list and deletes it if it is still
// unused; see backend.RemovePushCreatedRepository. It does nothing if a
// request started since gen was taken.
func (p *pushCreates) remove(name string, pc *pushCreate, gen int) {
	p.mu.Lock()
	if p.repos[name] != pc || pc.gen != gen || pc.inflight > 0 || pc.removing != nil {
		p.mu.Unlock()
		return
	}
	pc.timer = nil
	pc.removing = make(chan struct{})
	p.mu.Unlock()

	defer func() {
		p.mu.Lock()
		if p.repos[name] == pc {
			delete(p.repos, name)
		}
		close(pc.removing)
		p.mu.Unlock()
	}()

	if hasRefs(pc.dir) {
		return
	}
	logger := log.FromContext(pc.ctx)
	be := backend.FromContext(pc.ctx)
	removed, err := be.RemovePushCreatedRepository(pc.ctx, pc.repo)
	switch {
	case errors.Is(err, proto.ErrRepoNotFound):
		// Deleted, or renamed, meanwhile.
	case err != nil:
		logger.Error("failed to remove repository created by a push", "repo", name, "err", err)
	case removed:
		logger.Info("removed repository created by a push that sent no refs", "repo", name)
	}
}

// hasRefs reports whether the repository at dir has any ref. A repository
// that cannot be read counts as having refs, so it is never removed by
// mistake.
func hasRefs(dir string) bool {
	out, err := gitb.NewCommand("for-each-ref", "--count=1", "--format=%(refname)").RunInDir(dir)
	if err != nil {
		return true
	}
	return strings.TrimSpace(string(out)) != ""
}
