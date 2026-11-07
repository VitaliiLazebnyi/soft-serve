package backend

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// lfsDeleteFixture creates repository "lfsrepo" with a recorded LFS object,
// an object file with no row, and the temporary file of an unfinished
// upload, and returns the repository's LFS directory and those three paths.
func lfsDeleteFixture(t *testing.T, be *Backend) (dir string, files []string) {
	t.Helper()
	ctx := backendTestContext(be, be.cfg)
	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := be.CreateRepository(ctx, "lfsrepo", owner, proto.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}

	dir = filepath.Join(be.cfg.DataPath, "lfs", strconv.FormatInt(repo.ID(), 10))
	recorded := strings.Repeat("a", 64)
	unrecorded := strings.Repeat("b", 64)
	if err := be.store.CreateLFSObject(ctx, be.db, repo.ID(), recorded, 4); err != nil {
		t.Fatal(err)
	}
	files = []string{
		filepath.Join(dir, "objects", "aa", "aa", recorded),
		filepath.Join(dir, "objects", "bb", "bb", unrecorded),
		filepath.Join(dir, "incomplete", unrecorded+"0123456789abcdef01234567"),
	}
	for _, f := range files {
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir, files
}

// Deleting a repository removes its whole LFS directory. It used to delete
// only the object files that had a row, so objects without one and the
// temporary files of unfinished uploads outlived the repository.
func TestDeleteRepositoryRemovesLFSDirectory(t *testing.T) {
	be, cfg := newTestBackend(t)
	dir, _ := lfsDeleteFixture(t, be)

	if err := be.DeleteRepository(backendTestContext(be, cfg), "lfsrepo"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		var left []string
		filepath.WalkDir(dir, func(p string, d os.DirEntry, _ error) error { //nolint:errcheck
			if d != nil && !d.IsDir() {
				left = append(left, p)
			}
			return nil
		})
		t.Errorf("LFS directory %s still exists (stat err: %v), files left: %v", dir, err, left)
	}
}

// A delete that fails in the database leaves the repository, so its LFS
// objects must stay too. They used to be removed inside the transaction,
// before the row, and were lost when the transaction rolled back.
func TestDeleteRepositoryFailureKeepsLFSObjects(t *testing.T) {
	be, cfg := newTestBackend(t)
	_, files := lfsDeleteFixture(t, be)
	ctx := backendTestContext(be, cfg)

	if _, err := be.db.ExecContext(ctx, `CREATE TRIGGER block_repo_delete BEFORE DELETE ON repos
		BEGIN SELECT RAISE(ABORT, 'blocked'); END;`); err != nil {
		t.Fatal(err)
	}
	if err := be.DeleteRepository(ctx, "lfsrepo"); err == nil {
		t.Fatal("DeleteRepository succeeded although the row delete was blocked")
	}
	if _, err := be.store.GetRepoByName(ctx, be.db, "lfsrepo"); err != nil {
		t.Fatalf("repository row gone after a failed delete: %v", err)
	}
	for _, f := range files {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("%s removed by a failed delete: %v", f, err)
		}
	}
}

// Once the row is gone the delete has happened, so a directory that cannot be
// removed is logged rather than reported as a failed delete.
func TestDeleteRepositoryLogsLFSRemovalFailure(t *testing.T) {
	// As in os/removeall_test.go: on Windows a mode only sets the read-only
	// attribute, which does not stop removal, and root ignores the mode.
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions do not prevent removal on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root can remove the directory regardless of its mode")
	}
	var logs bytes.Buffer
	be, cfg := newTestBackendWithLog(t, &logs)
	dir, _ := lfsDeleteFixture(t, be)

	// A directory without write permission keeps its entries.
	objects := filepath.Join(dir, "objects")
	if err := os.Chmod(objects, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(objects, 0o755) }) //nolint:errcheck

	if err := be.DeleteRepository(backendTestContext(be, cfg), "lfsrepo"); err != nil {
		t.Fatalf("DeleteRepository => %v, want nil once the row is gone", err)
	}
	if _, err := be.store.GetRepoByName(backendTestContext(be, cfg), be.db, "lfsrepo"); err == nil {
		t.Error("repository row still exists")
	}
	if !strings.Contains(logs.String(), "failed to delete lfs objects") {
		t.Errorf("removal failure not logged; logs:\n%s", logs.String())
	}
}
