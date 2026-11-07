package backend

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// TestOpenErrorLoggedNotLeaked checks that when git refuses a repository (here
// for "dubious ownership"), clients get the plain ErrNotAGitRepository with no
// server path, and git's reason is logged on the server.
func TestOpenErrorLoggedNotLeaked(t *testing.T) {
	logs := &syncBuffer{}
	be, cfg := newTestBackendWithLog(t, logs)
	ctx := context.Background()

	if _, err := be.CreateRepository(ctx, "existing", nil, proto.RepositoryOptions{}); err != nil {
		t.Fatal(err)
	}

	// Makes git treat repositories as owned by someone else, as it would on
	// a volume owned by another user.
	t.Setenv("GIT_TEST_ASSUME_DIFFERENT_OWNER", "1")

	check := func(t *testing.T, err error) {
		t.Helper()
		if !errors.Is(err, git.ErrNotAGitRepository) {
			t.Fatalf("error = %v, want ErrNotAGitRepository", err)
		}
		if err.Error() != git.ErrNotAGitRepository.Error() || strings.Contains(err.Error(), cfg.DataPath) {
			t.Fatalf("client error = %q, want only %q", err, git.ErrNotAGitRepository)
		}
		if !strings.Contains(logs.String(), "dubious ownership") {
			t.Fatalf("git's reason was not logged; logs:\n%s", logs)
		}
	}

	t.Run("Open", func(t *testing.T) {
		r, err := be.Repository(ctx, "existing")
		if err != nil {
			t.Fatal(err)
		}
		_, err = r.Open()
		check(t, err)
	})

	t.Run("CreateRepository", func(t *testing.T) {
		_, err := be.CreateRepository(ctx, "new", nil, proto.RepositoryOptions{})
		check(t, err)
	})
}
