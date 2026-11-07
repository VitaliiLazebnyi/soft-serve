//go:build unix

package web

import (
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
// before it opens the file, and return the rename error promptly.
func TestRenameLFSObjectFIFODestination(t *testing.T) {
	const content = "hello lfs"
	oid := sha256Hex(content)
	objPath := path.Join("objects", oid[0:2], oid[2:4], oid)
	tempName := path.Join("incomplete", oid+"temp")

	root := t.TempDir()
	local := storage.NewLocalStorage(root)
	if _, err := local.Put(tempName, strings.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(root, filepath.FromSlash(objPath))
	if err := os.MkdirAll(filepath.Dir(fifo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		done <- renameLFSObject(failRenameStorage{local}, tempName, objPath, oid, int64(len(content)))
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("renameLFSObject accepted a FIFO as the object")
		}
	case <-time.After(5 * time.Second):
		// Open the FIFO for writing so the blocked open returns.
		if f, err := os.OpenFile(fifo, os.O_RDWR, 0); err == nil {
			f.Close() //nolint:errcheck
		}
		<-done
		t.Fatal("renameLFSObject blocked on a FIFO at the object path")
	}
}
