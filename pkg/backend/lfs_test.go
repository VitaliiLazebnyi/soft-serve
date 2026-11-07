package backend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

// fakeLFSClient serves every requested object from memory and records which
// objects were asked for.
type fakeLFSClient struct {
	objects map[string][]byte
	// errs holds the per-object errors the remote reports.
	errs map[string]error
	// err, if set, fails the whole download, like a failed batch request.
	err error
	// omit holds requested objects the remote leaves out of its response.
	omit map[string]bool
	// extra holds objects the remote sends without being asked for them,
	// with their content.
	extra     map[lfs.Pointer][]byte
	requested []string
	// onRead, if set, runs when an object's content is first read, standing
	// in for the time spent receiving it from the network.
	onRead func()
}

func (c *fakeLFSClient) Download(_ context.Context, pointers []lfs.Pointer, cb lfs.DownloadCallback) error {
	if c.err != nil {
		return c.err
	}
	for _, p := range pointers {
		c.requested = append(c.requested, p.Oid)
		if err := c.errs[p.Oid]; err != nil {
			if err := cb(p, nil, err); err != nil {
				return err
			}
			continue
		}
		if c.omit[p.Oid] {
			continue
		}
		var r io.Reader = bytes.NewReader(c.objects[p.Oid])
		if c.onRead != nil {
			r = &hookReader{r: r, hook: c.onRead}
		}
		if err := cb(p, io.NopCloser(r), nil); err != nil {
			return err
		}
	}
	for p, content := range c.extra {
		if err := cb(p, io.NopCloser(bytes.NewReader(content)), nil); err != nil {
			return err
		}
	}
	return nil
}

func (c *fakeLFSClient) Upload(context.Context, []lfs.Pointer, lfs.UploadCallback) error {
	return nil
}

// hookReader calls hook before the first Read.
type hookReader struct {
	r    io.Reader
	hook func()
	done bool
}

func (h *hookReader) Read(p []byte) (int, error) {
	if !h.done {
		h.done = true
		h.hook()
	}
	return h.r.Read(p)
}

// lfsTest is a repository whose history holds LFS pointers, and a client
// that serves their content.
type lfsTest struct {
	be       *Backend
	ctx      context.Context
	repo     proto.Repository
	pointers []lfs.Pointer
	client   *fakeLFSClient
	// work is the work tree the repository is pushed from.
	work string
}

// newLFSTest creates the repository "lfs-repo" with one LFS pointer file per
// object, numbered "lfs object 0" and up.
func newLFSTest(t *testing.T, objects int) *lfsTest {
	t.Helper()
	return newLFSTestWith(t, objects, nil)
}

// newLFSTestWith is newLFSTest, with extra, if set, called to add more files
// to the work tree before it is committed.
func newLFSTestWith(t *testing.T, objects int, extra func(work string, pointers []lfs.Pointer)) *lfsTest {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}

	be, cfg := newTestBackend(t)
	ctx := config.WithContext(context.Background(), cfg)
	repo, err := be.CreateRepository(ctx, "lfs-repo", nil, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	work := t.TempDir()
	lt := &lfsTest{be: be, ctx: ctx, repo: repo, client: &fakeLFSClient{objects: map[string][]byte{}}, work: work}
	for i := 0; i < objects; i++ {
		content := []byte(fmt.Sprintf("lfs object %d", i))
		p, err := lfs.GeneratePointer(bytes.NewReader(content))
		if err != nil {
			t.Fatal(err)
		}
		lt.client.objects[p.Oid] = content
		lt.pointers = append(lt.pointers, p)
		if err := os.WriteFile(filepath.Join(work, fmt.Sprintf("file%d.bin", i)), []byte(p.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if extra != nil {
		extra(work, lt.pointers)
	}

	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"add", "."},
		{"commit", "-q", "-m", "add lfs pointers"},
		{"push", "-q", be.repoPath("lfs-repo"), "main"},
	} {
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = work
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	return lt
}

// store runs StoreRepoMissingLFSObjects with the test's client.
func (lt *lfsTest) store() error {
	return StoreRepoMissingLFSObjects(lt.ctx, lt.repo, lt.be.db, lt.be.store, lt.client)
}

// path returns where the object of p is stored.
func (lt *lfsTest) path(p lfs.Pointer) string {
	return filepath.Join(lt.be.cfg.DataPath, "lfs", strconv.FormatInt(lt.repo.ID(), 10), "objects", p.RelativePath())
}

// TestStoreRepoMissingLFSObjects verifies that every LFS object is downloaded
// and recorded, including the final partial batch, and that a later sync only
// fetches objects that are missing on disk.
func TestStoreRepoMissingLFSObjects(t *testing.T) {
	for _, n := range []int{1, 21} {
		t.Run(fmt.Sprintf("%d objects", n), func(t *testing.T) {
			is := is.New(t)
			lt := newLFSTest(t, n)

			is.NoErr(lt.store())
			is.Equal(len(lt.client.requested), n) // every LFS object is downloaded

			for _, p := range lt.pointers {
				_, err := lt.be.store.GetLFSObjectByOid(lt.ctx, lt.be.db, lt.repo.ID(), p.Oid)
				is.NoErr(err) // every LFS object is recorded
				got, err := os.ReadFile(lt.path(p))
				is.NoErr(err)
				is.Equal(got, lt.client.objects[p.Oid]) // every LFS object is stored
			}

			// A second sync finds everything on disk and recorded.
			lt.client.requested = nil
			is.NoErr(lt.store())
			is.Equal(len(lt.client.requested), 0) // nothing is downloaded again

			// An object that is recorded but gone from disk is fetched again.
			p := lt.pointers[0]
			is.NoErr(os.Remove(lt.path(p)))
			is.NoErr(lt.store())
			is.Equal(lt.client.requested, []string{p.Oid})
			got, err := os.ReadFile(lt.path(p))
			is.NoErr(err)
			is.Equal(got, lt.client.objects[p.Oid])
		})
	}
}

// TestStoreRepoMissingLFSObjectsNoTxDuringDownload verifies that receiving an
// object's content doesn't hold a database write transaction, so other
// writers aren't blocked for as long as the download takes.
func TestStoreRepoMissingLFSObjectsNoTxDuringDownload(t *testing.T) {
	is := is.New(t)
	lt := newLFSTest(t, 1)

	var called bool
	var writeErr error
	lt.client.onRead = func() {
		called = true
		wctx, cancel := context.WithTimeout(lt.ctx, time.Second)
		defer cancel()
		writeErr = lt.be.store.SetAllowKeylessAccess(wctx, lt.be.db, false)
	}

	is.NoErr(lt.store())
	is.True(called)    // the content was read
	is.NoErr(writeErr) // another writer isn't blocked while the object downloads
	_, err := lt.be.store.GetLFSObjectByOid(lt.ctx, lt.be.db, lt.repo.ID(), lt.pointers[0].Oid)
	is.NoErr(err)
}

// TestStoreRepoMissingLFSObjectsRepoDeleted verifies that a sync fails, rather
// than keep storing objects, once the repository's database row is gone.
// Recording an object then violates the foreign key, which db.WrapError
// reports as a duplicate key.
func TestStoreRepoMissingLFSObjectsRepoDeleted(t *testing.T) {
	for _, onDisk := range []bool{false, true} {
		t.Run(fmt.Sprintf("on disk %v", onDisk), func(t *testing.T) {
			is := is.New(t)
			lt := newLFSTest(t, 1)

			if onDisk {
				// The object is on disk but not recorded, so it is only recorded.
				p := lt.pointers[0]
				is.NoErr(os.MkdirAll(filepath.Dir(lt.path(p)), 0o755))
				is.NoErr(os.WriteFile(lt.path(p), lt.client.objects[p.Oid], 0o600))
			}

			is.NoErr(lt.be.store.DeleteRepoByName(lt.ctx, lt.be.db, lt.repo.Name()))
			is.True(lt.store() != nil) // the sync fails
		})
	}
}

// TestStoreRepoMissingLFSObjectsObjectError verifies that an object the
// remote reports an error for is skipped, the other objects are still
// stored, and the sync reports ErrLFSObjectsUnavailable.
func TestStoreRepoMissingLFSObjectsObjectError(t *testing.T) {
	is := is.New(t)
	lt := newLFSTest(t, 2)
	missing, ok := lt.pointers[0], lt.pointers[1]
	lt.client.errs = map[string]error{missing.Oid: errors.New("object does not exist")}

	err := lt.store()
	is.True(errors.Is(err, ErrLFSObjectsUnavailable))            // the missing object is reported
	is.Equal(err.Error(), "some lfs objects are unavailable: 1") // as a count; each oid is logged on its own
	is.Equal(len(lt.client.requested), 2)                        // every object is attempted

	_, err = lt.be.store.GetLFSObjectByOid(lt.ctx, lt.be.db, lt.repo.ID(), ok.Oid)
	is.NoErr(err) // the available object is recorded
	_, err = os.Stat(lt.path(ok))
	is.NoErr(err) // and stored
	_, err = lt.be.store.GetLFSObjectByOid(lt.ctx, lt.be.db, lt.repo.ID(), missing.Oid)
	is.True(err != nil) // the missing object is not recorded
}

// TestFetchImportLFSObjects verifies that an import succeeds when the remote
// lacks some LFS objects, and fails when fetching them fails otherwise.
func TestFetchImportLFSObjects(t *testing.T) {
	is := is.New(t)
	lt := newLFSTest(t, 2)

	var logs bytes.Buffer
	lt.be.logger = log.New(&logs)
	lt.client.errs = map[string]error{lt.pointers[0].Oid: errors.New("object does not exist")}
	is.NoErr(lt.be.fetchImportLFSObjects(lt.ctx, lt.repo, lt.client))     // missing objects are only logged
	is.True(strings.Contains(logs.String(), "will not be fetched again")) // a plain import doesn't retry them
	is.True(strings.Contains(logs.String(), "missing=1"))                 // the warning counts them

	lt.client.err = errors.New("batch request failed")
	is.True(lt.be.fetchImportLFSObjects(lt.ctx, lt.repo, lt.client) != nil) // other errors fail the import
}

// TestStoreRepoMissingLFSObjectsBadContent verifies that content which
// doesn't match the requested pointer is neither stored nor recorded.
func TestStoreRepoMissingLFSObjectsBadContent(t *testing.T) {
	for name, content := range map[string]func([]byte) []byte{
		"wrong hash": func(b []byte) []byte { return bytes.ToUpper(b) },
		"too short":  func(b []byte) []byte { return b[:len(b)-1] },
		"too long":   func(b []byte) []byte { return append(b, '!') },
	} {
		t.Run(name, func(t *testing.T) {
			is := is.New(t)
			lt := newLFSTest(t, 1)
			p := lt.pointers[0]
			lt.client.objects[p.Oid] = content(lt.client.objects[p.Oid])

			is.True(errors.Is(lt.store(), ErrLFSObjectsUnavailable)) // the object is reported
			_, err := os.Stat(lt.path(p))
			is.True(os.IsNotExist(err)) // the content is not stored
			_, err = lt.be.store.GetLFSObjectByOid(lt.ctx, lt.be.db, lt.repo.ID(), p.Oid)
			is.True(err != nil) // nor recorded
			tmp, _ := os.ReadDir(filepath.Join(filepath.Dir(lt.path(p)), "..", "..", "..", "incomplete"))
			is.Equal(len(tmp), 0) // no temporary file is left behind
		})
	}
}

// TestStoreRepoMissingLFSObjectsOmitted verifies that a requested object the
// remote leaves out of its response is reported.
func TestStoreRepoMissingLFSObjectsOmitted(t *testing.T) {
	is := is.New(t)
	lt := newLFSTest(t, 1)
	lt.client.omit = map[string]bool{lt.pointers[0].Oid: true}
	is.True(errors.Is(lt.store(), ErrLFSObjectsUnavailable))
}

// TestStoreRepoMissingLFSObjectsUnrequested verifies that objects the remote
// sends without being asked for, valid or not, are not stored.
func TestStoreRepoMissingLFSObjectsUnrequested(t *testing.T) {
	is := is.New(t)
	lt := newLFSTest(t, 1)

	unasked := []byte("not in the repository")
	up, err := lfs.GeneratePointer(bytes.NewReader(unasked))
	is.NoErr(err)
	invalid := lfs.Pointer{Oid: "../../../../escaped", Size: 1}
	lt.client.extra = map[lfs.Pointer][]byte{up: unasked, invalid: []byte("x")}

	is.NoErr(lt.store())
	_, err = os.Stat(lt.path(lt.pointers[0]))
	is.NoErr(err) // the requested object is stored
	_, err = os.Stat(lt.path(up))
	is.True(os.IsNotExist(err)) // the unrequested one is not
	_, err = lt.be.store.GetLFSObjectByOid(lt.ctx, lt.be.db, lt.repo.ID(), up.Oid)
	is.True(err != nil) // nor recorded
	var found []string
	is.NoErr(filepath.Walk(lt.be.cfg.DataPath, func(path string, _ os.FileInfo, err error) error {
		if filepath.Base(path) == "escaped" || filepath.Base(filepath.Dir(path)) == "escaped" {
			found = append(found, path)
		}
		return err
	}))
	is.Equal(len(found), 0) // the invalid pointer wrote nothing
}

// TestStoreRepoMissingLFSObjectsTruncated verifies that a file on disk whose
// size doesn't match its pointer, such as one an interrupted download left
// behind, is downloaded again rather than recorded as it is.
func TestStoreRepoMissingLFSObjectsTruncated(t *testing.T) {
	for _, recorded := range []bool{false, true} {
		t.Run(fmt.Sprintf("recorded %v", recorded), func(t *testing.T) {
			is := is.New(t)
			lt := newLFSTest(t, 1)
			p := lt.pointers[0]
			content := lt.client.objects[p.Oid]

			if recorded {
				is.NoErr(lt.be.store.CreateLFSObject(lt.ctx, lt.be.db, lt.repo.ID(), p.Oid, p.Size))
			}
			is.NoErr(os.MkdirAll(filepath.Dir(lt.path(p)), 0o755))
			is.NoErr(os.WriteFile(lt.path(p), content[:len(content)/2], 0o600))

			is.NoErr(lt.store())
			is.Equal(lt.client.requested, []string{p.Oid}) // the object is downloaded again
			got, err := os.ReadFile(lt.path(p))
			is.NoErr(err)
			is.Equal(got, content) // and stored in full
			_, err = lt.be.store.GetLFSObjectByOid(lt.ctx, lt.be.db, lt.repo.ID(), p.Oid)
			is.NoErr(err) // and recorded
		})
	}
}

// TestStoreRepoMissingLFSObjectsDuplicateOid verifies that an oid two
// different pointer blobs share is requested only once.
func TestStoreRepoMissingLFSObjectsDuplicateOid(t *testing.T) {
	is := is.New(t)
	lt := newLFSTestWith(t, 1, func(work string, pointers []lfs.Pointer) {
		// A trailing line makes another blob that parses to the same pointer.
		is.NoErr(os.WriteFile(filepath.Join(work, "copy.bin"), []byte(pointers[0].String()+"\n"), 0o600))
	})

	is.NoErr(lt.store())
	is.Equal(lt.client.requested, []string{lt.pointers[0].Oid}) // requested once
}
