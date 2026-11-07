package backend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/matryer/is"
)

// lfsScan is one run of the pointer scan.
type lfsScan struct {
	include, exclude []string
	// oids holds the oids of the pointers it found.
	oids []string
}

// recordLFSScans makes the pointer scan record each of its runs.
func recordLFSScans(t *testing.T) func() []lfsScan {
	t.Helper()
	var mu sync.Mutex
	var scans []lfsScan
	orig := searchPointerBlobs
	t.Cleanup(func() { searchPointerBlobs = orig })
	searchPointerBlobs = func(ctx context.Context, r *git.Repository, include, exclude []string, pointerChan chan<- lfs.PointerBlob, errChan chan<- error) {
		mu.Lock()
		scans = append(scans, lfsScan{include: include, exclude: exclude})
		i := len(scans) - 1
		mu.Unlock()

		found := make(chan lfs.PointerBlob)
		go orig(ctx, r, include, exclude, found, errChan)
		defer close(pointerChan)
		for p := range found {
			mu.Lock()
			scans[i].oids = append(scans[i].oids, p.Oid)
			mu.Unlock()
			select {
			case pointerChan <- p:
			case <-ctx.Done():
			}
		}
	}
	return func() []lfsScan {
		mu.Lock()
		defer mu.Unlock()
		s := scans
		scans = nil
		return s
	}
}

// sync runs SyncRepoLFSObjects with the test's client.
func (lt *lfsTest) sync() error {
	lt.client.requested = nil
	return SyncRepoLFSObjects(lt.ctx, lt.repo, lt.be.db, lt.be.store, lt.client)
}

// addObject commits a pointer to a new LFS object, which the client serves,
// and pushes it to main.
func (lt *lfsTest) addObject(t *testing.T) lfs.Pointer {
	t.Helper()
	p := lt.commitObject(t)
	lt.git(t, "push", "-q", lt.be.repoPath("lfs-repo"), "HEAD:main")
	return p
}

// commitObject commits a pointer to a new LFS object, which the client
// serves, without pushing it.
func (lt *lfsTest) commitObject(t *testing.T) lfs.Pointer {
	t.Helper()
	i := len(lt.pointers)
	content := []byte(fmt.Sprintf("lfs object %d", i))
	p, err := lfs.GeneratePointer(bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	lt.client.objects[p.Oid] = content
	lt.pointers = append(lt.pointers, p)
	if err := os.WriteFile(filepath.Join(lt.work, fmt.Sprintf("file%d.bin", i)), []byte(p.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	lt.git(t, "add", ".")
	lt.git(t, "commit", "-q", "-m", fmt.Sprintf("add lfs object %d", i))
	return p
}

// detachHead commits a pointer to a new LFS object and points the
// repository's HEAD straight at that commit, so no ref reaches it.
func (lt *lfsTest) detachHead(t *testing.T) lfs.Pointer {
	t.Helper()
	p := lt.commitObject(t)
	lt.git(t, "push", "-q", lt.be.repoPath("lfs-repo"), "HEAD:refs/tmp/detached")
	lt.bareGit(t, "update-ref", "--no-deref", "HEAD", "refs/tmp/detached")
	lt.bareGit(t, "update-ref", "-d", "refs/tmp/detached")
	return p
}

// git runs git in the work tree and returns its output.
func (lt *lfsTest) git(t *testing.T, args ...string) string {
	t.Helper()
	return lt.gitIn(t, lt.work, args...)
}

// bareGit runs git in the repository and returns its output.
func (lt *lfsTest) bareGit(t *testing.T, args ...string) string {
	t.Helper()
	return lt.gitIn(t, lt.be.repoPath("lfs-repo"), args...)
}

// gitIn runs git in dir and returns its output.
func (lt *lfsTest) gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
	)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// lfsDir returns the LFS directory of the test's repository.
func (lt *lfsTest) lfsDir() string {
	return filepath.Join(lt.be.cfg.DataPath, "lfs", strconv.FormatInt(lt.repo.ID(), 10))
}

func oids(pointers ...lfs.Pointer) []string {
	var s []string
	for _, p := range pointers {
		s = append(s, p.Oid)
	}
	return s
}

// TestSyncRepoLFSObjectsIncremental verifies that a mirror sync only scans
// the history added since the last one, and stores the objects in it.
func TestSyncRepoLFSObjectsIncremental(t *testing.T) {
	is := is.New(t)
	lt := newLFSTest(t, 2)
	scans := recordLFSScans(t)

	// The first sync scans everything.
	is.NoErr(lt.sync())
	got := scans()
	is.Equal(len(got), 1)
	is.Equal(got[0].exclude, nil)
	is.Equal(len(got[0].oids), 2)
	tip1 := lt.git(t, "rev-parse", "HEAD")
	is.Equal(got[0].include, []string{tip1})

	// A sync that fetched nothing scans nothing.
	is.NoErr(lt.sync())
	is.Equal(len(scans()), 0)
	is.Equal(len(lt.client.requested), 0)

	// A sync after a new commit scans only that commit's objects.
	p := lt.addObject(t)
	tip2 := lt.git(t, "rev-parse", "HEAD")
	is.NoErr(lt.sync())
	got = scans()
	is.Equal(len(got), 1)
	is.Equal(got[0].include, []string{tip2})
	is.Equal(got[0].exclude, []string{tip1})
	is.Equal(got[0].oids, oids(p))         // only the new object is scanned
	is.Equal(lt.client.requested, oids(p)) // and fetched
	content, err := os.ReadFile(lt.path(p))
	is.NoErr(err)
	is.Equal(content, lt.client.objects[p.Oid]) // and stored
	_, err = lt.be.store.GetLFSObjectByOid(lt.ctx, lt.be.db, lt.repo.ID(), p.Oid)
	is.NoErr(err) // and recorded
}

// TestSyncRepoLFSObjectsFullScan verifies the cases in which a mirror sync
// scans the whole history again.
func TestSyncRepoLFSObjectsFullScan(t *testing.T) {
	t.Run("import", func(t *testing.T) {
		is := is.New(t)
		lt := newLFSTest(t, 1)
		scans := recordLFSScans(t)
		is.NoErr(lt.store())
		is.NoErr(lt.store())
		got := scans()
		is.Equal(len(got), 2) // an import always scans
		is.Equal(got[1].exclude, nil)
		is.Equal(len(got[1].oids), 1) // everything

		// It leaves the tips it scanned for the first mirror sync.
		is.NoErr(lt.sync())
		is.Equal(len(scans()), 0)
	})

	t.Run("old tip gone", func(t *testing.T) {
		is := is.New(t)
		lt := newLFSTest(t, 1)
		scans := recordLFSScans(t)
		is.NoErr(lt.sync())
		scans()

		// A tip of the last sync that's no longer in the repository makes
		// the shorter scan fail.
		missing := strings.Repeat("1", 40)
		is.NoErr(os.WriteFile(filepath.Join(lt.lfsDir(), lfsScannedRefsFile), []byte(missing+"\n"), 0o600))
		is.NoErr(os.Remove(lt.path(lt.pointers[0])))
		is.NoErr(lt.sync())
		got := scans()
		is.Equal(len(got), 2)
		is.Equal(got[0].exclude, []string{missing})
		is.Equal(got[1].exclude, nil) // so it scans everything
		is.Equal(len(got[1].oids), 1)
		is.Equal(lt.client.requested, oids(lt.pointers[0])) // and fetches what's missing

		// And the next sync starts from the tips it scanned.
		is.NoErr(lt.sync())
		is.Equal(len(scans()), 0)
	})

	t.Run("unreadable tips", func(t *testing.T) {
		is := is.New(t)
		lt := newLFSTest(t, 1)
		scans := recordLFSScans(t)
		is.NoErr(lt.sync())
		scans()

		is.NoErr(os.WriteFile(filepath.Join(lt.lfsDir(), lfsScannedRefsFile), []byte("not an object id\n"), 0o600))
		is.NoErr(lt.sync())
		got := scans()
		is.Equal(len(got), 1)
		is.Equal(got[0].exclude, nil)
	})

	t.Run("lfs directory removed", func(t *testing.T) {
		is := is.New(t)
		lt := newLFSTest(t, 2)
		scans := recordLFSScans(t)
		is.NoErr(lt.sync())
		scans()

		// The objects are gone, and the record of what was scanned with
		// them, so everything is scanned and fetched again.
		is.NoErr(os.RemoveAll(lt.lfsDir()))
		is.NoErr(lt.sync())
		got := scans()
		is.Equal(len(got), 1)
		is.Equal(got[0].exclude, nil)
		is.Equal(len(lt.client.requested), 2)
		for _, p := range lt.pointers {
			_, err := os.Stat(lt.path(p))
			is.NoErr(err)
		}
	})
}

// TestSyncRepoLFSObjectsRetry verifies that objects a sync couldn't store
// are tried again by later syncs, even when nothing new was fetched.
func TestSyncRepoLFSObjectsRetry(t *testing.T) {
	t.Run("first sync", func(t *testing.T) {
		is := is.New(t)
		lt := newLFSTest(t, 2)
		scans := recordLFSScans(t)
		p := lt.pointers[1]
		lt.client.errs = map[string]error{p.Oid: errors.New("not there")}
		is.True(errors.Is(lt.sync(), ErrLFSObjectsUnavailable))
		scans()

		lt.client.errs = nil
		is.NoErr(lt.sync())
		got := scans()
		is.Equal(len(got), 1)
		is.Equal(got[0].exclude, nil)          // still everything
		is.Equal(lt.client.requested, oids(p)) // and the object is fetched
		_, err := os.Stat(lt.path(p))
		is.NoErr(err)
	})

	t.Run("later sync", func(t *testing.T) {
		is := is.New(t)
		lt := newLFSTest(t, 1)
		scans := recordLFSScans(t)
		is.NoErr(lt.sync())
		tip1 := lt.git(t, "rev-parse", "HEAD")
		scans()

		p := lt.addObject(t)
		lt.client.errs = map[string]error{p.Oid: errors.New("not there")}
		is.True(errors.Is(lt.sync(), ErrLFSObjectsUnavailable))
		scans()

		// Nothing new was fetched, but the commit with the object that
		// was unavailable is scanned again.
		lt.client.errs = nil
		is.NoErr(lt.sync())
		got := scans()
		is.Equal(len(got), 1)
		is.Equal(got[0].exclude, []string{tip1})
		is.Equal(got[0].oids, oids(p))
		is.Equal(lt.client.requested, oids(p))
		_, err := os.Stat(lt.path(p))
		is.NoErr(err)

		is.NoErr(lt.sync())
		is.Equal(len(scans()), 0)
	})
}

// TestSyncRepoLFSObjectsDetachedHead verifies that the history only a
// detached HEAD reaches is scanned, like git rev-list --all does, in a full
// scan and in a scan of the new history.
func TestSyncRepoLFSObjectsDetachedHead(t *testing.T) {
	t.Run("full scan", func(t *testing.T) {
		is := is.New(t)
		lt := newLFSTest(t, 1)
		p := lt.detachHead(t)
		is.NoErr(lt.sync())
		is.Equal(lt.client.requested, oids(lt.pointers...))
		_, err := os.Stat(lt.path(p))
		is.NoErr(err)
	})

	t.Run("new history", func(t *testing.T) {
		is := is.New(t)
		lt := newLFSTest(t, 1)
		scans := recordLFSScans(t)
		is.NoErr(lt.sync())
		scans()

		p := lt.detachHead(t)
		is.NoErr(lt.sync())
		got := scans()
		is.Equal(len(got), 1)
		is.True(got[0].exclude != nil) // only the new history
		is.Equal(lt.client.requested, oids(p))
		_, err := os.Stat(lt.path(p))
		is.NoErr(err)
	})
}
