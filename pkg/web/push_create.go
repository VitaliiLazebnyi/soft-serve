package web

import (
	"context"
	"strings"
	"sync"
	"time"

	"charm.land/log/v2"
	gitb "github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/backend"
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
// request or never arrive, so a repository is removed when a push to it
// ends with no refs, or when nothing has reached it for pushCreateTimeout.
// A repository leaves the list once it has refs.
//
// The list is kept in memory, so a repository still on it when the server
// stops is kept.
type pushCreates struct {
	mu    sync.Mutex
	repos map[string]*pushCreate
}

// pushCreate is a repository created by an HTTP push that has no refs yet.
type pushCreate struct {
	// ctx is the creating request's context without its cancellation, used
	// to delete the repository after that request has ended.
	ctx      context.Context
	dir      string
	inflight int
	// gen identifies the current expiry timer; a timer whose generation is
	// out of date does nothing.
	gen   int
	timer *time.Timer
}

func newPushCreates() *pushCreates {
	return &pushCreates{repos: make(map[string]*pushCreate)}
}

// add records that the request in ctx created the repository name at dir.
func (p *pushCreates) add(ctx context.Context, name, dir string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.repos[name] = &pushCreate{ctx: context.WithoutCancel(ctx), dir: dir}
}

// begin marks a request to the repository name as in flight, so it is not
// removed under the request. It returns the function to call when the
// request ends, with push set for a receive-pack RPC, or nil when name is
// not on the list.
func (p *pushCreates) begin(name string) func(push bool) {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	pc, ok := p.repos[name]
	if !ok {
		return nil
	}
	pc.inflight++
	pc.gen++
	if pc.timer != nil {
		pc.timer.Stop()
		pc.timer = nil
	}
	return func(push bool) { p.end(name, pc, push) }
}

// end is called when a request to a listed repository ends. Once no
// request is in flight, a repository with refs leaves the list; one without
// refs is removed after a push, and otherwise gets pushCreateTimeout for
// the push to arrive.
func (p *pushCreates) end(name string, pc *pushCreate, push bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pc.inflight--
	if p.repos[name] != pc || pc.inflight > 0 {
		return
	}
	if push || hasRefs(pc.dir) {
		p.remove(name, pc)
		return
	}
	pc.gen++
	gen := pc.gen
	pc.timer = time.AfterFunc(pushCreateTimeout, func() { p.expire(name, pc, gen) })
}

// expire removes a repository nothing has reached for pushCreateTimeout.
func (p *pushCreates) expire(name string, pc *pushCreate, gen int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.repos[name] != pc || pc.inflight > 0 || pc.gen != gen {
		return
	}
	p.remove(name, pc)
}

// remove takes the repository off the list and deletes it if it still has
// no refs. The lock is held so no request to it can start meanwhile.
func (p *pushCreates) remove(name string, pc *pushCreate) {
	delete(p.repos, name)
	if hasRefs(pc.dir) {
		return
	}
	logger := log.FromContext(pc.ctx)
	logger.Info("removing repository created by a push that sent no refs", "repo", name)
	if err := backend.FromContext(pc.ctx).DeleteRepository(pc.ctx, name); err != nil {
		logger.Error("failed to remove repository created by a push", "repo", name, "err", err)
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
