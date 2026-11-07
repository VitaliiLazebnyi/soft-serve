package cmd

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/matryer/is"
)

// gitTestTimeout bounds the waits in these tests, so a handler that never
// returns fails the test instead of hanging it.
const gitTestTimeout = 30 * time.Second

// cancelledPushToCreate runs a push to the missing repository "fresh" as an
// admin, which creates it, and cancels the session once receive-pack has sent
// its advertisement, as a client going away does. beforeCancel runs with the
// repository's path just before the cancellation. It returns the backend and
// a context to look the repository up with.
func cancelledPushToCreate(t *testing.T, beforeCancel func(dir string)) (context.Context, *backend.Backend) {
	t.Helper()
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	adminCtx := withUser(t, ctx, be, "boss", true)
	sessCtx, cancel := context.WithCancel(adminCtx)
	defer cancel()

	// Stdin stays open and empty, so receive-pack waits for the client after
	// the advertisement until it is killed.
	stdin, stdinW := io.Pipe()
	defer stdinW.Close() //nolint: errcheck
	stdout, stdoutW := io.Pipe()

	c := GitReceivePackCommand()
	c.SilenceUsage = true
	c.SilenceErrors = true
	c.SetIn(stdin)
	c.SetOut(stdoutW)
	c.SetErr(io.Discard)
	c.SetArgs([]string{"fresh"})

	errc := make(chan error, 1)
	go func() {
		errc <- c.ExecuteContext(sessCtx)
		stdoutW.Close() //nolint: errcheck
	}()

	// The repository exists once the advertisement starts.
	lc := make(chan error, 1)
	go func() {
		_, err := bufio.NewReader(stdout).ReadString('\n')
		lc <- err
	}()
	select {
	case err := <-lc:
		is.NoErr(err)
	case <-time.After(gitTestTimeout):
		t.Fatal("timed out reading the advertisement")
	}
	_, err := be.Repository(ctx, "fresh")
	is.NoErr(err)

	if beforeCancel != nil {
		beforeCancel(filepath.Join(config.FromContext(ctx).DataPath, "repos", "fresh.git"))
	}

	cancel()
	go io.Copy(io.Discard, stdout) //nolint: errcheck

	select {
	case err = <-errc:
	case <-time.After(gitTestTimeout):
		t.Fatal("timed out waiting for receive-pack to return")
	}
	is.True(errors.Is(err, context.Canceled))

	return ctx, be
}

// A push that creates a repository and is then cancelled, because the client
// went away, must not leave the new repository behind.
func TestCancelledPushRemovesCreatedRepository(t *testing.T) {
	is := is.New(t)
	var dir string
	ctx, be := cancelledPushToCreate(t, func(d string) { dir = d })

	_, err := be.Repository(ctx, "fresh")
	is.True(err != nil)      // the created repository was removed
	is.True(!dirExists(dir)) // and so was its directory
}

// receive-pack updates the refs and reports success to the client before the
// post-receive hooks run. A push cancelled after that, while the hooks run,
// must keep the repository it created: the client was told the push
// succeeded.
func TestCancelledPushKeepsCreatedRepositoryWithRefs(t *testing.T) {
	is := is.New(t)
	ctx, be := cancelledPushToCreate(t, func(dir string) {
		git := func(stdin string, args ...string) string {
			cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)
			cmd.Stdin = strings.NewReader(stdin)
			cmd.Env = append(os.Environ(),
				"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
				"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
			)
			out, err := cmd.Output()
			is.NoErr(err)
			return strings.TrimSpace(string(out))
		}
		tree := git("", "mktree")
		commit := git("", "commit-tree", tree, "-m", "pushed")
		git("", "update-ref", "refs/heads/main", commit)
	})

	_, err := be.Repository(ctx, "fresh")
	is.NoErr(err) // the repository with pushed refs was kept
}

// fastRetry keeps the retry tests quick.
var fastRetry = retryPolicy{first: time.Millisecond, total: 200 * time.Millisecond}

// emptyBareRepo creates an empty bare repository and returns its path.
func emptyBareRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "fresh.git")
	if out, err := exec.CommandContext(t.Context(), "git", "init", "--bare", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	return dir
}

// writeRef points refs/heads/main at a new empty commit in the bare
// repository dir.
func writeRef(t *testing.T, dir string) {
	t.Helper()
	git := func(args ...string) string {
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"--git-dir=" + dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		)
		cmd.Stdin = strings.NewReader("")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	commit := git("commit-tree", git("mktree"), "-m", "pushed")
	git("update-ref", "refs/heads/main", commit)
}

func dirExists(dir string) bool {
	_, err := os.Lstat(dir)
	return err == nil
}

// The delete is retried while it fails, until the directory is gone.
func TestRemoveFailedPushRepoRetries(t *testing.T) {
	is := is.New(t)
	dir := emptyBareRepo(t)
	calls := 0
	del := func(context.Context, string) error {
		calls++
		if calls < 3 {
			return errors.New("directory in use")
		}
		return os.RemoveAll(dir)
	}

	removeFailedPushRepo(t.Context(), log.New(io.Discard), "fresh", dir, del, fastRetry)
	is.Equal(calls, 3)
	is.True(!dirExists(dir))
}

// A delete that removes the row but leaves the directory behind is retried
// until the directory is gone, without checking refs on what is left of it.
func TestRemoveFailedPushRepoRetriesLeftoverDirectory(t *testing.T) {
	is := is.New(t)
	dir := emptyBareRepo(t)
	calls := 0
	del := func(context.Context, string) error {
		calls++
		if calls == 1 {
			// The row is gone and the directory was partly removed.
			return os.Remove(filepath.Join(dir, "HEAD"))
		}
		return os.RemoveAll(dir)
	}

	removeFailedPushRepo(t.Context(), log.New(io.Discard), "fresh", dir, del, fastRetry)
	is.Equal(calls, 2)
	is.True(!dirExists(dir))
}

// Refs written between attempts, while the repository still exists, keep it.
func TestRemoveFailedPushRepoKeepsRefsWrittenBetweenAttempts(t *testing.T) {
	is := is.New(t)
	dir := emptyBareRepo(t)
	calls := 0
	del := func(context.Context, string) error {
		calls++
		writeRef(t, dir)
		return errors.New("directory in use")
	}

	removeFailedPushRepo(t.Context(), log.New(io.Discard), "fresh", dir, del, fastRetry)
	is.Equal(calls, 1)
	is.True(dirExists(dir))
}

// A delete that keeps failing is given up after the retry policy's total.
func TestRemoveFailedPushRepoGivesUp(t *testing.T) {
	is := is.New(t)
	dir := emptyBareRepo(t)
	calls := 0
	del := func(context.Context, string) error {
		calls++
		return errors.New("directory in use")
	}

	start := time.Now()
	removeFailedPushRepo(t.Context(), log.New(io.Discard), "fresh", dir, del, fastRetry)
	is.True(calls > 1)
	is.True(time.Since(start) < 5*time.Second)
	is.True(dirExists(dir))
}
