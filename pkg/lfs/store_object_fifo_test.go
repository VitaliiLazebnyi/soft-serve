//go:build unix

package lfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/pkg/storage"
)

// A FIFO at the object path is not the object. Opening one blocks until a
// writer shows up, so the fallback after a failed rename must check the type
// before it opens the file, and StoreObject must fail promptly.
func TestStoreObjectRenameOntoFIFO(t *testing.T) {
	const content = "hello lfs"
	sum := sha256.Sum256([]byte(content))
	oid := hex.EncodeToString(sum[:])
	objPath := path.Join("objects", oid[0:2], oid[2:4], oid)

	root := t.TempDir()
	fifo := filepath.Join(root, filepath.FromSlash(objPath))
	if err := os.MkdirAll(filepath.Dir(fifo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	strg := failRenameStorage{storage.NewLocalStorage(root)}
	datastore := &memLFSStore{objects: map[string]int64{}}

	done := make(chan error, 1)
	go func() {
		done <- StoreObject(context.Background(), strg, nil, datastore, 1, oid, int64(len(content)), strings.NewReader(content))
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("StoreObject accepted a FIFO as the object")
		}
	case <-time.After(5 * time.Second):
		// Open the FIFO for writing so the blocked open returns.
		if f, err := os.OpenFile(fifo, os.O_RDWR, 0); err == nil {
			f.Close() //nolint:errcheck
		}
		<-done
		t.Fatal("StoreObject blocked on a FIFO at the object path")
	}
}
