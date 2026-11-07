package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/storage"
	"github.com/charmbracelet/soft-serve/pkg/store"
)

type lfsUploadEnv struct {
	ctx       context.Context
	datastore store.Store
	repo      proto.Repository
	token     string
	router    http.Handler
	lfsDir    string
}

func newLFSUploadEnv(t *testing.T) *lfsUploadEnv {
	t.Helper()
	ctx, be, datastore := newLFSTestContext(t)
	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := be.CreateRepository(ctx, "repo", owner, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	token, err := be.CreateAccessToken(ctx, owner, "test", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.FromContext(ctx)
	return &lfsUploadEnv{
		ctx:       ctx,
		datastore: datastore,
		repo:      repo,
		token:     token,
		router:    lfsTestRouter(ctx),
		lfsDir:    filepath.Join(cfg.DataPath, "lfs", strconv.FormatInt(repo.ID(), 10)),
	}
}

// upload PUTs body to the basic upload route for oid, declaring size as the
// content length.
func (e *lfsUploadEnv) upload(oid string, size int64, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(e.ctx, http.MethodPut,
		"/repo.git/info/lfs/objects/basic/"+oid, body)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Length", strconv.FormatInt(size, 10))
	req.ContentLength = size
	req.Header.Set("Authorization", "token "+e.token)
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	return w
}

func (e *lfsUploadEnv) objectPath(oid string) string {
	return filepath.Join(e.lfsDir, "objects", oid[0:2], oid[2:4], oid)
}

// assertNoTempFiles fails if an upload left anything in the incomplete
// directory.
func (e *lfsUploadEnv) assertNoTempFiles(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(e.lfsDir, "incomplete"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, ent := range entries {
		t.Errorf("temp file left behind: %s", ent.Name())
	}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// TestLFSUploadRejectsMismatchedContent checks that the basic upload handler
// verifies the body against the oid and the declared size. It used to write
// the body straight to the object path and record it, so a client could
// store arbitrary content under any oid.
func TestLFSUploadRejectsMismatchedContent(t *testing.T) {
	const content = "hello lfs"
	oid := sha256Hex(content)
	cases := []struct {
		name string
		oid  string
		size int64
		body string
	}{
		{"wrong hash", sha256Hex("something else"), int64(len(content)), content},
		{"body shorter than size", oid, int64(len(content)) + 1, content},
		{"body longer than size", oid, int64(len(content)) - 1, content},
		{"truncated body", oid, int64(len(content)), content[:len(content)-1]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newLFSUploadEnv(t)
			w := e.upload(tc.oid, tc.size, strings.NewReader(tc.body))
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("got status %d, want 422: %s", w.Code, w.Body.String())
			}
			if _, err := os.Stat(e.objectPath(tc.oid)); !os.IsNotExist(err) {
				t.Errorf("object stored despite mismatch (stat err: %v)", err)
			}
			if _, err := e.datastore.GetLFSObjectByOid(e.ctx, db.FromContext(e.ctx), e.repo.ID(), tc.oid); err == nil {
				t.Error("object recorded despite mismatch")
			}
			e.assertNoTempFiles(t)
		})
	}
}

// TestLFSUploadRequiresContentLength checks an upload whose length is unknown,
// such as a chunked body. net/http drops the Content-Length header then, so
// the handler must not rely on the header: it answers 411, since the object
// cannot be checked against a declared size, and stores nothing.
func TestLFSUploadRequiresContentLength(t *testing.T) {
	e := newLFSUploadEnv(t)
	const content = "hello lfs"
	oid := sha256Hex(content)

	req := httptest.NewRequestWithContext(e.ctx, http.MethodPut,
		"/repo.git/info/lfs/objects/basic/"+oid, strings.NewReader(content))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Authorization", "token "+e.token)
	req.ContentLength = -1
	req.TransferEncoding = []string{"chunked"}
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)

	if w.Code != http.StatusLengthRequired {
		t.Fatalf("got status %d, want 411: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, lfs.MediaType) {
		t.Errorf("Content-Type = %q, want an LFS JSON error", ct)
	}
	if _, err := os.Stat(e.objectPath(oid)); !os.IsNotExist(err) {
		t.Errorf("object stored without a content length (stat err: %v)", err)
	}
	if _, err := e.datastore.GetLFSObjectByOid(e.ctx, db.FromContext(e.ctx), e.repo.ID(), oid); err == nil {
		t.Error("object recorded without a content length")
	}
	e.assertNoTempFiles(t)
}

// TestLFSUploadStoresVerifiedObject checks the happy path, including a
// repeated upload of an object that is already stored.
func TestLFSUploadStoresVerifiedObject(t *testing.T) {
	e := newLFSUploadEnv(t)
	const content = "hello lfs"
	oid := sha256Hex(content)

	for i := range 2 {
		w := e.upload(oid, int64(len(content)), strings.NewReader(content))
		if w.Code != http.StatusOK {
			t.Fatalf("upload %d: got status %d, want 200: %s", i, w.Code, w.Body.String())
		}
	}

	got, err := os.ReadFile(e.objectPath(oid))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Errorf("stored content %q, want %q", got, content)
	}
	obj, err := e.datastore.GetLFSObjectByOid(e.ctx, db.FromContext(e.ctx), e.repo.ID(), oid)
	if err != nil {
		t.Fatal(err)
	}
	if obj.Size != int64(len(content)) {
		t.Errorf("recorded size %d, want %d", obj.Size, len(content))
	}
	e.assertNoTempFiles(t)
}

// hookReader runs hook once, on the first Read, before reading from r. The
// handler only reads the body after its "already stored?" check, so the hook
// runs between that check and the database insert.
type hookReader struct {
	r    io.Reader
	once sync.Once
	hook func()
}

func (h *hookReader) Read(p []byte) (int, error) {
	h.once.Do(h.hook)
	return h.r.Read(p)
}

// TestLFSUploadConcurrentDuplicate checks that two uploads of the same object
// that both get past the existence check both succeed. The second insert hits
// the unique constraint, which used to be answered with a 500.
func TestLFSUploadConcurrentDuplicate(t *testing.T) {
	e := newLFSUploadEnv(t)
	const content = "concurrent object"
	oid := sha256Hex(content)

	const n = 2
	var started sync.WaitGroup
	started.Add(n)
	codes := make([]int, n)
	var done sync.WaitGroup
	for i := range n {
		done.Add(1)
		go func() {
			defer done.Done()
			body := &hookReader{r: strings.NewReader(content), hook: func() {
				// Wait until every request is past the existence check.
				started.Done()
				started.Wait()
			}}
			w := e.upload(oid, int64(len(content)), body)
			codes[i] = w.Code
			if w.Code != http.StatusOK {
				t.Errorf("upload %d: got status %d, want 200: %s", i, w.Code, w.Body.String())
			}
		}()
	}
	done.Wait()

	if _, err := e.datastore.GetLFSObjectByOid(e.ctx, db.FromContext(e.ctx), e.repo.ID(), oid); err != nil {
		t.Errorf("object not recorded: %v", err)
	}
	e.assertNoTempFiles(t)
}

// TestLFSUploadRepoDeletedMidUpload checks that a foreign key violation is
// not mistaken for a duplicate upload. db.WrapError maps both to
// ErrDuplicateKey, so the handler must confirm the row exists before it
// reports success.
func TestLFSUploadRepoDeletedMidUpload(t *testing.T) {
	e := newLFSUploadEnv(t)
	const content = "orphan object"
	oid := sha256Hex(content)

	body := &hookReader{r: strings.NewReader(content), hook: func() {
		if err := e.datastore.DeleteRepoByName(e.ctx, db.FromContext(e.ctx), "repo"); err != nil {
			t.Errorf("delete repo: %v", err)
		}
	}}
	w := e.upload(oid, int64(len(content)), body)
	if w.Code == http.StatusOK {
		t.Fatalf("got 200 for an object whose repository was deleted")
	}
	e.assertNoTempFiles(t)
}

// failRenameStorage is a storage whose Rename always fails, as renaming onto
// a file another upload is renaming or has open does on Windows.
type failRenameStorage struct{ storage.Storage }

func (failRenameStorage) Rename(string, string) error {
	return errors.New("rename: access is denied")
}

// TestRenameLFSObjectExistingDestination checks the rename into objects/
// when it fails. If a concurrent upload of the same object has already put
// a file of the expected size there, the object is stored and the upload
// succeeds; on Windows, one of two identical concurrent uploads used to get
// a 500. Otherwise the rename error is returned.
func TestRenameLFSObjectExistingDestination(t *testing.T) {
	const content = "hello lfs"
	oid := sha256Hex(content)
	objPath := path.Join("objects", oid[0:2], oid[2:4], oid)
	tempName := path.Join("incomplete", oid+"temp")

	cases := []struct {
		name    string
		dest    *string // content already at objPath, nil for none
		wantErr bool
	}{
		{"destination holds the object", ptr(content), false},
		{"no destination", nil, true},
		{"destination of another size", ptr(content + "!"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			local := storage.NewLocalStorage(t.TempDir())
			if _, err := local.Put(tempName, strings.NewReader(content)); err != nil {
				t.Fatal(err)
			}
			if tc.dest != nil {
				if _, err := local.Put(objPath, strings.NewReader(*tc.dest)); err != nil {
					t.Fatal(err)
				}
			}
			err := renameLFSObject(failRenameStorage{local}, tempName, objPath, int64(len(content)))
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Errorf("renameLFSObject => %v, want error %v", err, tc.wantErr)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }
