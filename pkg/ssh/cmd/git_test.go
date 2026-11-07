package cmd

import (
	"bufio"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/matryer/is"
)

// A push that creates a repository and is then cancelled, because the client
// went away, must not leave the new repository behind.
func TestCancelledPushRemovesCreatedRepository(t *testing.T) {
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
	_, err := bufio.NewReader(stdout).ReadString('\n')
	is.NoErr(err)
	_, err = be.Repository(ctx, "fresh")
	is.NoErr(err)

	cancel()
	go io.Copy(io.Discard, stdout) //nolint: errcheck

	err = <-errc
	is.True(errors.Is(err, context.Canceled))

	_, err = be.Repository(ctx, "fresh")
	is.True(err != nil) // the created repository was removed
}
