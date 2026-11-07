package git

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"charm.land/log/v2"
	"github.com/charmbracelet/git-lfs-transfer/transfer"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/migrate"
	"github.com/charmbracelet/soft-serve/pkg/lfs"
	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/charmbracelet/soft-serve/pkg/storage"
	"github.com/charmbracelet/soft-serve/pkg/store"
	"github.com/charmbracelet/soft-serve/pkg/store/database"
	_ "modernc.org/sqlite"
)

// newLFSUploadTransfer returns an upload-session lfsTransfer backed by a
// migrated SQLite database holding repository 1 ("repo1"), and the context
// it was built from.
func newLFSUploadTransfer(t *testing.T) (*lfsTransfer, context.Context) {
	t.Helper()
	dp := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.DataPath = dp
	ctx := config.WithContext(context.Background(), cfg)
	dbx, err := db.Open(ctx, "sqlite", dp+"/test.db?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dbx.Close() }) //nolint:errcheck
	if err := migrate.Migrate(ctx, dbx); err != nil {
		t.Fatal(err)
	}
	datastore := database.New(ctx, dbx)
	if err := datastore.CreateUser(ctx, dbx, "owner", false, nil); err != nil {
		t.Fatal(err)
	}
	user, err := datastore.FindUserByUsername(ctx, dbx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := datastore.CreateRepo(ctx, dbx, "repo1", user.ID, "", "", false, false, false, false); err != nil {
		t.Fatal(err)
	}
	if r, err := datastore.GetRepoByName(ctx, dbx, "repo1"); err != nil || r.ID != (lfsTestRepo{}).ID() {
		t.Fatalf("repo1 has id %d (err %v), want %d", r.ID, err, (lfsTestRepo{}).ID())
	}
	ctx = db.WithContext(ctx, dbx)
	ctx = store.WithContext(ctx, datastore)

	return &lfsTransfer{
		ctx:     ctx,
		cfg:     cfg,
		dbx:     dbx,
		store:   datastore,
		logger:  log.New(&bytes.Buffer{}),
		storage: storage.NewLocalStorage(filepath.Join(dp, "lfs", "1")),
		repo:    lfsTestRepo{},
		op:      lfs.OperationUpload,
	}, ctx
}

func lfsObjectPath(cfg *config.Config, oid string) string {
	return filepath.Join(cfg.DataPath, "lfs", "1", "objects", oid[0:2], oid[2:4], oid)
}

// assertNoLFSTempFiles fails if an upload left anything in the incomplete
// directory.
func assertNoLFSTempFiles(t *testing.T, cfg *config.Config) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(cfg.DataPath, "lfs", "1", "incomplete"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, ent := range entries {
		t.Errorf("temp file left behind: %s", ent.Name())
	}
}

func oidOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// Upload must check the data against the oid and the declared size itself.
// It used to store and record whatever it was given, trusting its caller.
func TestLFSTransferUploadRejectsMismatchedContent(t *testing.T) {
	const content = "hello lfs"
	oid := oidOf(content)
	cases := []struct {
		name string
		oid  string
		size int64
		data string
	}{
		{"wrong hash", oidOf("something else"), int64(len(content)), content},
		{"data shorter than size", oid, int64(len(content)) + 1, content},
		{"data longer than size", oid, int64(len(content)) - 1, content},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr, _ := newLFSUploadTransfer(t)
			err := tr.Upload(tc.oid, tc.size, strings.NewReader(tc.data), nil)
			if !errors.Is(err, transfer.ErrCorruptData) {
				t.Fatalf("Upload: got %v, want ErrCorruptData", err)
			}
			if _, err := os.Stat(lfsObjectPath(tr.cfg, tc.oid)); !os.IsNotExist(err) {
				t.Errorf("object stored despite mismatch (stat err: %v)", err)
			}
			if _, err := tr.store.GetLFSObjectByOid(tr.ctx, tr.dbx, 1, tc.oid); err == nil {
				t.Error("object recorded despite mismatch")
			}
			assertNoLFSTempFiles(t, tr.cfg)
		})
	}
}

// Uploading an object that is already recorded succeeds. The insert used to
// fail on the unique constraint and the client got an internal error.
func TestLFSTransferUploadDuplicate(t *testing.T) {
	tr, _ := newLFSUploadTransfer(t)
	const content = "hello lfs"
	oid := oidOf(content)

	for i := range 2 {
		if err := tr.Upload(oid, int64(len(content)), strings.NewReader(content), nil); err != nil {
			t.Fatalf("upload %d: %v", i, err)
		}
	}

	got, err := os.ReadFile(lfsObjectPath(tr.cfg, oid))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Errorf("stored content %q, want %q", got, content)
	}
	obj, err := tr.store.GetLFSObjectByOid(tr.ctx, tr.dbx, 1, oid)
	if err != nil {
		t.Fatal(err)
	}
	if obj.Size != int64(len(content)) {
		t.Errorf("recorded size %d, want %d", obj.Size, len(content))
	}
	assertNoLFSTempFiles(t, tr.cfg)
}

// A foreign key violation is not a duplicate upload. db.WrapError maps both
// to ErrDuplicateKey, so Upload must confirm the row exists before it
// reports success.
func TestLFSTransferUploadRepoDeleted(t *testing.T) {
	tr, _ := newLFSUploadTransfer(t)
	if err := tr.store.DeleteRepoByName(tr.ctx, tr.dbx, "repo1"); err != nil {
		t.Fatal(err)
	}
	const content = "orphan object"
	if err := tr.Upload(oidOf(content), int64(len(content)), strings.NewReader(content), nil); err == nil {
		t.Fatal("Upload succeeded for a repository that no longer exists")
	}
	assertNoLFSTempFiles(t, tr.cfg)
}

// Through the transfer protocol, an object whose data is longer than its
// declared size is refused with a 400, nothing is left on disk, and the
// next command is still read from the right place in the stream.
func TestLFSTransferPutObjectOversized(t *testing.T) {
	tr, ctx := newLFSUploadTransfer(t)
	data := "hello"
	oid := oidOf(data)

	var in strings.Builder
	in.WriteString(pkt("version 1\n") + "0000")
	in.WriteString(pkt("put-object "+oid+"\n") + pkt(fmt.Sprintf("size=%d\n", len(data)-2)) + "0001")
	in.WriteString(pkt(data) + "0000")
	in.WriteString(pkt("quit\n") + "0000")

	ctx = proto.WithRepositoryContext(ctx, lfsTestRepo{})
	ctx = proto.WithUserContext(ctx, lfsTestUser{})
	var out bytes.Buffer
	if err := LFSTransfer(ctx, ServiceCommand{
		Stdin:  strings.NewReader(in.String()),
		Stdout: &out,
		Args:   []string{"repo1", lfs.OperationUpload},
	}); err != nil {
		t.Fatalf("LFSTransfer: %v", err)
	}

	var got []string
	for _, m := range regexp.MustCompile(`status (\d{3})`).FindAllStringSubmatch(out.String(), -1) {
		got = append(got, m[1])
	}
	want := []string{"200", "400", "200"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("statuses = %v, want %v\noutput: %q", got, want, out.String())
	}
	if _, err := os.Stat(lfsObjectPath(tr.cfg, oid)); !os.IsNotExist(err) {
		t.Errorf("object stored despite size mismatch (stat err: %v)", err)
	}
	assertNoLFSTempFiles(t, tr.cfg)
}

// failRenameStorage is a storage whose Rename always fails, as renaming onto
// a file another upload is renaming or has open does on Windows.
type failRenameStorage struct{ storage.Storage }

func (failRenameStorage) Rename(string, string) error {
	return errors.New("rename: access is denied")
}

// When the rename into objects/ fails but a concurrent upload of the same
// object has already put a file of the expected size there, the object is
// stored and the upload succeeds. On Windows, one of two identical
// concurrent uploads used to fail this way. Otherwise the rename error is
// returned.
func TestLFSTransferUploadRenameOntoExistingObject(t *testing.T) {
	const content = "hello lfs"
	oid := oidOf(content)
	objPath := path.Join("objects", oid[0:2], oid[2:4], oid)

	cases := []struct {
		name    string
		dest    string // content already at objPath, "" for none
		wantErr bool
	}{
		{"destination holds the object", content, false},
		{"no destination", "", true},
		{"destination of another size", content + "!", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr, _ := newLFSUploadTransfer(t)
			if tc.dest != "" {
				if _, err := tr.storage.Put(objPath, strings.NewReader(tc.dest)); err != nil {
					t.Fatal(err)
				}
			}
			tr.storage = failRenameStorage{tr.storage}

			err := tr.Upload(oid, int64(len(content)), strings.NewReader(content), nil)
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Fatalf("Upload => %v, want error %v", err, tc.wantErr)
			}
			_, gerr := tr.store.GetLFSObjectByOid(tr.ctx, tr.dbx, 1, oid)
			if recorded := gerr == nil; recorded == tc.wantErr {
				t.Errorf("object recorded = %v, want %v", recorded, !tc.wantErr)
			}
			assertNoLFSTempFiles(t, tr.cfg)
		})
	}
}
