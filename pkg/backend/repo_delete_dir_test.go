package backend

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// createDeleteFixture creates repository "delrepo" and returns its directory.
func createDeleteFixture(t *testing.T, be *Backend) string {
	t.Helper()
	ctx := backendTestContext(be, be.cfg)
	owner, err := be.CreateUser(ctx, "owner", proto.UserOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := be.CreateRepository(ctx, "delrepo", owner, proto.RepositoryOptions{}); err != nil {
		t.Fatal(err)
	}
	return be.repoPath("delrepo")
}

// A delete whose commit fails leaves the repository, so its directory must
// stay too. The directory used to be removed inside the transaction, so a
// failed commit left a row with no repository behind it.
func TestDeleteRepositoryCommitFailureKeepsDirectory(t *testing.T) {
	be, cfg := newTestBackend(t)
	rp := createDeleteFixture(t, be)
	ctx := backendTestContext(be, cfg)

	// A deferred foreign key is checked at commit, so the delete statements
	// succeed and the commit fails.
	for _, q := range []string{
		`CREATE TABLE pin (repo_id INTEGER NOT NULL REFERENCES repos(id) DEFERRABLE INITIALLY DEFERRED)`,
		`INSERT INTO pin (repo_id) SELECT id FROM repos WHERE name = 'delrepo'`,
	} {
		if _, err := be.db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	if err := be.DeleteRepository(ctx, "delrepo"); err == nil {
		t.Fatal("DeleteRepository succeeded although the commit was blocked")
	}
	if _, err := be.store.GetRepoByName(ctx, be.db, "delrepo"); err != nil {
		t.Fatalf("repository row gone after a failed delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rp, "HEAD")); err != nil {
		t.Errorf("repository directory damaged by a failed delete: %v", err)
	}
}

// Once the row is gone the delete has happened: a directory that cannot be
// removed is logged rather than reported as a failed delete, and nothing can
// take the leftover directory over. Deleting the repository again removes it.
func TestDeleteRepositoryDirectoryRemovalFailure(t *testing.T) {
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
	rp := createDeleteFixture(t, be)
	ctx := backendTestContext(be, cfg)

	// A directory without write permission keeps its entries.
	objects := filepath.Join(rp, "objects")
	if err := os.Chmod(objects, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(objects, 0o755) }) //nolint:errcheck

	if err := be.DeleteRepository(ctx, "delrepo"); err != nil {
		t.Fatalf("DeleteRepository => %v, want nil once the row is gone", err)
	}
	if _, err := be.store.GetRepoByName(ctx, be.db, "delrepo"); err == nil {
		t.Error("repository row still exists")
	}
	if !strings.Contains(logs.String(), "failed to delete repository directory") {
		t.Errorf("removal failure not logged; logs:\n%s", logs.String())
	}
	if _, err := os.Stat(rp); err != nil {
		t.Fatalf("expected the leftover directory to remain: %v", err)
	}

	// The leftover directory is not free for the taking.
	owner, err := be.User(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := be.CreateRepository(ctx, "delrepo", owner, proto.RepositoryOptions{}); !errors.Is(err, proto.ErrRepoExist) {
		t.Errorf("CreateRepository over the leftover directory => %v, want ErrRepoExist", err)
	}

	// Retrying the delete removes it, and the name is free again.
	if err := os.Chmod(objects, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := be.DeleteRepository(ctx, "delrepo"); err != nil {
		t.Fatalf("retry DeleteRepository => %v", err)
	}
	if _, err := os.Stat(rp); !os.IsNotExist(err) {
		t.Errorf("leftover directory still there after retry (stat err: %v)", err)
	}
	if _, err := be.CreateRepository(ctx, "delrepo", owner, proto.RepositoryOptions{}); err != nil {
		t.Errorf("CreateRepository after cleanup => %v", err)
	}
}
