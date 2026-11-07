package git

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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
	// The original Wait error stays in the message.
	if runtime.GOOS != "windows" && !strings.Contains(err.Error(), "signal: killed") {
		t.Fatalf("expected the error to keep the Wait error, got %v", err)
	}
}

// TestUploadPackFailsAfterCancel covers git failing on its own after the
// context was cancelled but before it was killed: the real failure must be
// reported, not the cancellation.
func TestUploadPackFailsAfterCancel(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo.git")
	if out, err := exec.Command("git", "init", "--bare", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stdin, stdinW := io.Pipe()
	defer stdinW.Close() //nolint: errcheck
	stdout, stdoutW := io.Pipe()

	errc := make(chan error, 1)
	go func() {
		errc <- UploadPack(ctx, ServiceCommand{
			Stdin:  stdin,
			Stdout: stdoutW,
			Dir:    dir,
			// Do not kill git when the context is cancelled, so that it
			// exits on its own.
			CmdFunc: func(c *exec.Cmd) { c.Cancel = func() error { return nil } },
		})
		stdoutW.Close() //nolint: errcheck
	}()

	if _, err := bufio.NewReader(stdout).ReadString('\n'); err != nil {
		t.Fatalf("reading the advertisement: %v", err)
	}
	go io.Copy(io.Discard, stdout) //nolint: errcheck
	cancel()

	// A malformed packet line makes upload-pack fail with a protocol error.
	if _, err := stdinW.Write([]byte("zzzz")); err != nil {
		t.Fatalf("writing to upload-pack: %v", err)
	}
	stdinW.Close() //nolint: errcheck

	err := <-errc
	if err == nil {
		t.Fatal("expected upload-pack to fail")
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("expected the git failure, got the cancellation: %v", err)
	}
}

func TestStoppedByContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if stoppedByContext(context.Background(), context.Canceled) {
		t.Error("a context that is not done must not report a cancellation")
	}
	if !stoppedByContext(ctx, context.Canceled) {
		t.Error("the context error must report a cancellation")
	}
	if !stoppedByContext(ctx, fmt.Errorf("wait: %w", context.Canceled)) {
		t.Error("a wrapped context error must report a cancellation")
	}
	if stoppedByContext(ctx, errors.New("some other failure")) {
		t.Error("an unrelated error must not report a cancellation")
	}

	if runtime.GOOS == "windows" {
		t.Skip("exit by signal is not reported on windows")
	}

	exited := exec.Command("sh", "-c", "exit 1").Run()
	if exited == nil {
		t.Fatal("expected sh to exit with an error")
	}
	if stoppedByContext(ctx, exited) {
		t.Errorf("a process that exited on its own must not report a cancellation: %v", exited)
	}

	killed := exec.Command("sh", "-c", "kill -9 $$").Run()
	if killed == nil {
		t.Fatal("expected sh to be killed")
	}
	if !stoppedByContext(ctx, killed) {
		t.Errorf("a process killed by a signal must report a cancellation: %v", killed)
	}
}
