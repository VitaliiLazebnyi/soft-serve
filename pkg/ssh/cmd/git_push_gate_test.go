package cmd

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/charmbracelet/soft-serve/pkg/proto"
	"github.com/matryer/is"
)

// A push over SSH counts as running for the removal of a repository an
// HTTP push created and left without refs, so that removal cannot delete
// the repository while the SSH push may write refs to it.
func TestSSHPushHoldsOffPushCreatedRemoval(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	adminCtx := withUser(t, ctx, be, "boss", true)
	user := proto.UserFromContext(adminCtx)
	repo, err := be.CreateRepository(ctx, "shared", user, proto.RepositoryOptions{})
	is.NoErr(err)

	// Stdin stays open and empty, so receive-pack waits for the client
	// after the advertisement.
	stdin, stdinW := io.Pipe()
	stdout, stdoutW := io.Pipe()

	c := GitReceivePackCommand()
	c.SilenceUsage = true
	c.SilenceErrors = true
	c.SetIn(stdin)
	c.SetOut(stdoutW)
	c.SetErr(io.Discard)
	c.SetArgs([]string{"shared"})

	errc := make(chan error, 1)
	go func() {
		errc <- c.ExecuteContext(adminCtx)
		stdoutW.Close() //nolint: errcheck
	}()
	_, err = bufio.NewReader(stdout).ReadString('\n')
	is.NoErr(err)
	go io.Copy(io.Discard, stdout) //nolint: errcheck

	removed, err := be.RemovePushCreatedRepository(ctx, repo)
	is.NoErr(err)
	is.True(!removed) // the SSH push is running

	// The client sends no commands, and the push ends.
	_, err = stdinW.Write([]byte("0000"))
	is.NoErr(err)
	is.NoErr(stdinW.Close())
	<-errc

	removed, err = be.RemovePushCreatedRepository(ctx, repo)
	is.NoErr(err)
	is.True(removed)
}

// An LFS upload over SSH counts as running for that removal too: removing
// the repository under it would fail the upload and leave its temporary
// file behind.
func TestSSHLFSUploadHoldsOffPushCreatedRemoval(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	adminCtx := withUser(t, ctx, be, "boss", true)
	user := proto.UserFromContext(adminCtx)
	repo, err := be.CreateRepository(ctx, "shared", user, proto.RepositoryOptions{})
	is.NoErr(err)

	stdin, stdinW := io.Pipe()
	stdout, stdoutW := io.Pipe()

	c := GitLFSTransfer()
	c.SilenceUsage = true
	c.SilenceErrors = true
	c.SetIn(stdin)
	c.SetOut(stdoutW)
	c.SetErr(io.Discard)
	c.SetArgs([]string{"shared", "upload"})

	errc := make(chan error, 1)
	go func() {
		errc <- c.ExecuteContext(adminCtx)
		stdoutW.Close() //nolint: errcheck
	}()
	// The transfer has started once it advertises its capabilities.
	_, err = bufio.NewReader(stdout).ReadString('\n')
	is.NoErr(err)
	go io.Copy(io.Discard, stdout) //nolint: errcheck

	removed, err := be.RemovePushCreatedRepository(ctx, repo)
	is.NoErr(err)
	is.True(!removed) // the LFS upload is running

	is.NoErr(stdinW.Close())
	<-errc

	removed, err = be.RemovePushCreatedRepository(ctx, repo)
	is.NoErr(err)
	is.True(removed)
}

// A push that finds the repository it was going to push to removed, as an
// unused HTTP push-created repository, creates it anew, as a push to a
// missing name does, rather than failing.
func TestSSHPushAfterPushCreatedRemovalCreates(t *testing.T) {
	is := is.New(t)
	ctx, be := newAuthTestContext(t)
	adminCtx := withUser(t, ctx, be, "boss", true)
	user := proto.UserFromContext(adminCtx)
	repo, err := be.CreateRepository(ctx, "shared", user, proto.RepositoryOptions{})
	is.NoErr(err)

	// The removal runs after this push looked the repository up and
	// before it starts.
	testHookBeforeBeginPush = func() {
		removed, err := be.RemovePushCreatedRepository(ctx, repo)
		is.NoErr(err)
		is.True(removed)
	}
	t.Cleanup(func() { testHookBeforeBeginPush = nil })

	// The repository's hooks run the server binary, which is the test
	// binary here; point git at no hooks instead.
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.hooksPath")
	t.Setenv("GIT_CONFIG_VALUE_0", t.TempDir())

	// A commit to push, and its pack.
	work := t.TempDir()
	gitCmd := func(stdin string, args ...string) []byte {
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", work}, args...)...)
		cmd.Stdin = strings.NewReader(stdin)
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		)
		out, err := cmd.Output()
		is.NoErr(err)
		return out
	}
	gitCmd("", "init", "-q", "--bare")
	tree := strings.TrimSpace(string(gitCmd("", "mktree")))
	commit := strings.TrimSpace(string(gitCmd("", "commit-tree", tree, "-m", "pushed")))
	pack := gitCmd(commit+"\n", "pack-objects", "--stdout", "--revs")
	line := fmt.Sprintf("%s %s refs/heads/main\x00report-status\n", strings.Repeat("0", 40), commit)
	req := fmt.Sprintf("%04x%s0000", len(line)+4, line) + string(pack)

	var out bytes.Buffer
	c := GitReceivePackCommand()
	c.SilenceUsage = true
	c.SilenceErrors = true
	c.SetIn(strings.NewReader(req))
	c.SetOut(&out)
	c.SetErr(io.Discard)
	c.SetArgs([]string{"shared"})
	is.NoErr(c.ExecuteContext(adminCtx))

	cur, err := be.Repository(ctx, "shared")
	is.NoErr(err) // created anew
	is.True(cur.ID() != repo.ID())
	r, err := cur.Open()
	is.NoErr(err)
	head, err := r.HEAD()
	is.NoErr(err)
	is.Equal(head.ID, commit) // with the pushed commit
}
