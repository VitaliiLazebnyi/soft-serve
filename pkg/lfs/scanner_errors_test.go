package lfs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/git"
)

// newPointerRepo returns a repository with one commit holding an LFS
// pointer file.
func newPointerRepo(t *testing.T) *git.Repository {
	t.Helper()
	dir := t.TempDir()
	pointer := "version https://git-lfs.github.com/spec/v1\n" +
		"oid sha256:4d7a214614ab2935c943f9e0ff69d22eadbb8f32b1258daaa5e2ca24d17e2393\n" +
		"size 12345\n"
	if err := os.WriteFile(filepath.Join(dir, "file.bin"), []byte(pointer), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"add", "file.bin"},
		{"-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-q", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	repo, err := git.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

// failGit puts a git first on PATH that fails, writing "injected failure"
// to stderr, when its arguments include arg, and runs the real git
// otherwise.
func failGit(t *testing.T, arg string) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not found")
	}
	bin := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
for a in "$@"; do
	if [ "$a" = %q ]; then
		echo "injected failure" >&2
		exit 1
	fi
done
exec %q "$@"
`, arg, realGit)
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func searchPointers(repo *git.Repository) ([]PointerBlob, error) {
	pointerChan := make(chan PointerBlob)
	errChan := make(chan error, 1)
	go SearchPointerBlobs(context.Background(), repo, pointerChan, errChan)
	var pointers []PointerBlob
	for p := range pointerChan {
		pointers = append(pointers, p)
	}
	return pointers, <-errChan
}

func TestSearchPointerBlobs(t *testing.T) {
	pointers, err := searchPointers(newPointerRepo(t))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(pointers) != 1 || pointers[0].Size != 12345 {
		t.Errorf("pointers = %+v, want the one in file.bin", pointers)
	}
}

// Every git command of the scan must report its failure. Only git rev-list
// used to: a failing cat-file ended the scan early with no error, so it
// passed for a complete scan that found fewer pointers.
func TestSearchPointerBlobsReportsGitFailures(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as git")
	}
	for _, tc := range []struct{ arg, label string }{
		{"rev-list", "git rev-list"},
		{"--batch-check", "git cat-file --batch-check"},
		{"--batch", "git cat-file --batch "},
	} {
		t.Run(tc.arg, func(t *testing.T) {
			repo := newPointerRepo(t)
			failGit(t, tc.arg)
			_, err := searchPointers(repo)
			if err == nil {
				t.Fatal("scan reported no error")
			}
			if !strings.Contains(err.Error(), tc.label) {
				t.Errorf("error %q does not name %q", err, tc.label)
			}
			if !strings.Contains(err.Error(), "injected failure") {
				t.Errorf("error %q does not include git's stderr", err)
			}
		})
	}
}
