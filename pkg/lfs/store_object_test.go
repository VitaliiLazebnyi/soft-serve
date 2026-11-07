package lfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/db"
	"github.com/charmbracelet/soft-serve/pkg/db/models"
	"github.com/charmbracelet/soft-serve/pkg/storage"
	"github.com/charmbracelet/soft-serve/pkg/store"
)

// failRenameStorage is a storage whose Rename always fails, as renaming onto
// a file another upload is renaming or a download has open does on Windows.
type failRenameStorage struct{ storage.Storage }

func (failRenameStorage) Rename(string, string) error {
	return errors.New("rename: access is denied")
}

// memLFSStore records LFS objects in memory. Only the two methods
// StoreObject uses are implemented.
type memLFSStore struct {
	store.LFSStore
	objects map[string]int64
}

func (m *memLFSStore) CreateLFSObject(_ context.Context, _ db.Handler, _ int64, oid string, size int64) error {
	if _, ok := m.objects[oid]; ok {
		return db.ErrDuplicateKey
	}
	m.objects[oid] = size
	return nil
}

func (m *memLFSStore) GetLFSObjectByOid(_ context.Context, _ db.Handler, _ int64, oid string) (models.LFSObject, error) {
	size, ok := m.objects[oid]
	if !ok {
		return models.LFSObject{}, db.ErrRecordNotFound
	}
	return models.LFSObject{Oid: oid, Size: size}, nil
}

// When the rename into objects/ fails, StoreObject accepts a file already
// there only if it is the object: the right size and hashing to the oid.
// Not everything that writes objects/ verifies the content, so a file of the
// right size alone is not enough.
func TestStoreObjectRenameFailure(t *testing.T) {
	const content = "hello lfs"
	sum := sha256.Sum256([]byte(content))
	oid := hex.EncodeToString(sum[:])
	objPath := path.Join("objects", oid[0:2], oid[2:4], oid)

	cases := []struct {
		name    string
		dest    string // content already at objPath, "" for none
		wantErr bool
	}{
		{"destination holds the object", content, false},
		{"no destination", "", true},
		{"destination of another size", content + "!", true},
		{"destination of the same size but other content", "jello lfs", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			local := storage.NewLocalStorage(root)
			if tc.dest != "" {
				if _, err := local.Put(objPath, strings.NewReader(tc.dest)); err != nil {
					t.Fatal(err)
				}
			}
			datastore := &memLFSStore{objects: map[string]int64{}}

			err := StoreObject(context.Background(), failRenameStorage{local}, nil, datastore, 1, oid, int64(len(content)), strings.NewReader(content))
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Fatalf("StoreObject => %v, want error %v", err, tc.wantErr)
			}
			if _, recorded := datastore.objects[oid]; recorded == tc.wantErr {
				t.Errorf("object recorded = %v, want %v", recorded, !tc.wantErr)
			}
			if tc.dest != "" {
				got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(objPath)))
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != tc.dest {
					t.Errorf("destination changed to %q", got)
				}
			}
			entries, err := os.ReadDir(filepath.Join(root, "incomplete"))
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			for _, ent := range entries {
				t.Errorf("temp file left behind: %s", ent.Name())
			}
		})
	}
}
