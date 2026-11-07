package lfs

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/soft-serve/git"
)

// TestSearchPointerBlobsCancel verifies that the scan stops once its context
// is canceled, even when nobody reads the remaining pointers, and reports
// that it didn't finish.
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

	if err, ok := <-errChan; !ok || err == nil {
		t.Fatal("SearchPointerBlobs did not report the canceled scan")
	}
}

// TestSearchPointerBlobsBetween verifies that the scan only finds the
// pointers reachable from include but not from exclude.
func TestSearchPointerBlobsBetween(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}

	work := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = work
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
	var pointers []Pointer
	var tips []string
	run("init", "-q", "-b", "main")
	for i := 0; i < 2; i++ {
		p, err := GeneratePointer(bytes.NewReader([]byte(fmt.Sprintf("object %d", i))))
		if err != nil {
			t.Fatal(err)
		}
		pointers = append(pointers, p)
		if err := os.WriteFile(filepath.Join(work, fmt.Sprintf("file%d", i)), []byte(p.String()), 0o600); err != nil {
			t.Fatal(err)
		}
		run("add", ".")
		run("commit", "-q", "-m", fmt.Sprintf("add pointer %d", i))
		tips = append(tips, run("rev-parse", "HEAD"))
	}
	repo, err := git.Open(work)
	if err != nil {
		t.Fatal(err)
	}

	scan := func(include, exclude []string) ([]string, error) {
		pointerChan := make(chan PointerBlob)
		errChan := make(chan error, 1)
		go SearchPointerBlobsBetween(context.Background(), repo, include, exclude, pointerChan, errChan)
		var oids []string
		for p := range pointerChan {
			oids = append(oids, p.Oid)
		}
		return oids, <-errChan
	}

	for _, tc := range []struct {
		name             string
		include, exclude []string
		want             []string
	}{
		{"everything", tips[1:], nil, []string{pointers[0].Oid, pointers[1].Oid}},
		{"new commit", tips[1:], tips[:1], []string{pointers[1].Oid}},
		{"nothing new", tips[1:], tips[1:], nil},
		{"no include", nil, tips[:1], nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := scan(tc.include, tc.exclude)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			slices.Sort(got)
			want := slices.Clone(tc.want)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("oids = %v, want %v", got, want)
			}
		})
	}

	// An object that isn't in the repository fails the scan.
	if _, err := scan(tips[1:], []string{strings.Repeat("1", 40)}); err == nil {
		t.Error("scan with a missing object succeeded")
	}
}
