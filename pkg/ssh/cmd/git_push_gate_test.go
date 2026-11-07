package cmd

import (
	"bufio"
	"io"
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
