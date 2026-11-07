package lfs

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/git"
)

// TestSearchPointerBlobsCancel verifies that the scan stops once its context
// is canceled, even when nobody reads the remaining pointers.
func TestSearchPointerBlobsCancel(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}

	work := t.TempDir()
	for i := 0; i < 5; i++ {
		p, err := GeneratePointer(bytes.NewReader([]byte(fmt.Sprintf("object %d", i))))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, fmt.Sprintf("file%d", i)), []byte(p.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"add", "."},
		{"commit", "-q", "-m", "add lfs pointers"},
	} {
		cmd := exec.Command("git", args...)
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

	repo, err := git.Open(work)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pointerChan := make(chan PointerBlob)
	errChan := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		SearchPointerBlobs(ctx, repo, pointerChan, errChan)
		close(done)
	}()

	// Read one pointer, let the scanner block on the next one, then stop
	// reading and cancel.
	<-pointerChan
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("SearchPointerBlobs did not return after the context was canceled")
	}
}
