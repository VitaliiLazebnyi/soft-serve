package git

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestUploadPackClientGoesAway runs upload-pack the way a client that only
// wants the ref advertisement does: it reads the first line and then goes
// away, which cancels the context. The error must say the context was
// cancelled rather than report the killed git process.
func TestUploadPackClientGoesAway(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo.git")
	if out, err := exec.Command("git", "init", "--bare", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Stdin stays open and empty, so upload-pack waits for the client after
	// the advertisement until it is killed.
	stdin, stdinW := io.Pipe()
	defer stdinW.Close() //nolint: errcheck
	stdout, stdoutW := io.Pipe()

	errc := make(chan error, 1)
	go func() {
		errc <- UploadPack(ctx, ServiceCommand{
			Stdin:  stdin,
			Stdout: stdoutW,
			Dir:    dir,
		})
		stdoutW.Close() //nolint: errcheck
	}()

	if _, err := bufio.NewReader(stdout).ReadString('\n'); err != nil {
		t.Fatalf("reading the advertisement: %v", err)
	}
	cancel()
	go io.Copy(io.Discard, stdout) //nolint: errcheck

	err := <-errc
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}
