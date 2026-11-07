package repo

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/soft-serve/git"
	"github.com/charmbracelet/soft-serve/pkg/proto"
)

// openRepo is a proto.Repository that only opens a repository on disk.
type openRepo struct {
	proto.Repository
	path string
}

func (r openRepo) Open() (*git.Repository, error) { return git.Open(r.path) }

// gitRun runs git in dir and fails the test on error.
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(cmd.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@b",
		"GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@b")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// A HEAD that names a branch that does not exist, as in a mirror whose
// remote renamed its default branch, falls back to an existing branch
// rather than an error.
func TestDefaultRef(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	gitRun(t, filepath.Dir(dir), "init", "-q", "-b", "trunk", dir)
	gitRun(t, dir, "commit", "-q", "--allow-empty", "-m", "first")
	gitRun(t, dir, "branch", "dev")

	r, err := git.Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	ref, err := defaultRef(r)
	if err != nil || ref == nil || ref.Name().Short() != "trunk" {
		t.Fatalf("defaultRef with a valid HEAD = %v, %v; want trunk", ref, err)
	}

	gitRun(t, dir, "symbolic-ref", "HEAD", "refs/heads/gone")
	switch msg := UpdateRefCmd(openRepo{path: dir})().(type) {
	case RefMsg:
		if ref := (*git.Reference)(msg); ref == nil || ref.Name().Short() != "dev" {
			t.Fatalf("UpdateRefCmd with a dangling HEAD = %v; want dev", msg)
		}
	default:
		t.Fatalf("UpdateRefCmd with a dangling HEAD = %T %v; want a RefMsg", msg, msg)
	}
	ref, err = defaultRef(r)
	if err != nil || ref == nil || ref.Name().Short() != "dev" {
		t.Fatalf("defaultRef with a dangling HEAD = %v, %v; want dev", ref, err)
	}
	if ref.ID == "" {
		t.Fatal("defaultRef with a dangling HEAD returned a reference without a commit")
	}

	gitRun(t, dir, "branch", "main", "trunk")
	ref, err = defaultRef(r)
	if err != nil || ref == nil || ref.Name().Short() != "main" {
		t.Fatalf("defaultRef with a dangling HEAD and a main branch = %v, %v; want main", ref, err)
	}
}
