//go:build unix

package git

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A FIFO at the object path is not the object. Opening one blocks until a
// writer shows up, so the fallback after a failed rename must check the type
// before it opens the file, and the upload must fail promptly.
func TestLFSTransferUploadRenameOntoFIFO(t *testing.T) {
	const content = "hello lfs"
	oid := oidOf(content)
	tr, _ := newLFSUploadTransfer(t)
	fifo := lfsObjectPath(tr.cfg, oid)
	if err := os.MkdirAll(filepath.Dir(fifo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	tr.storage = failRenameStorage{tr.storage}

	done := make(chan error, 1)
	go func() {
		done <- tr.Upload(oid, int64(len(content)), strings.NewReader(content), nil)
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("Upload accepted a FIFO as the object")
		}
	case <-time.After(5 * time.Second):
		// Open the FIFO for writing so the blocked open returns.
		if f, err := os.OpenFile(fifo, os.O_RDWR, 0); err == nil {
			f.Close() //nolint:errcheck
		}
		<-done
		t.Fatal("Upload blocked on a FIFO at the object path")
	}
}
