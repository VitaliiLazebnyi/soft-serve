package git

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// testTimeout bounds every wait in these tests, so a handler that never
// returns fails the test instead of hanging it.
const testTimeout = 30 * time.Second

// initBare creates an empty bare repository and returns its path.
func initBare(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo.git")
	if out, err := exec.CommandContext(t.Context(), "git", "init", "--bare", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	return dir
}

// readLine reads one line from r, failing the test if none comes in time.
func readLine(t *testing.T, r io.Reader) string {
	t.Helper()
	type result struct {
		line string
		err  error
	}
	c := make(chan result, 1)
	go func() {
		line, err := bufio.NewReader(r).ReadString('\n')
		c <- result{line, err}
	}()
	select {
	case res := <-c:
		if res.err != nil {
			t.Fatalf("reading a line: %v", res.err)
		}
		return res.line
	case <-time.After(testTimeout):
		t.Fatal("timed out reading a line")
		return ""
	}
}

// waitErr waits for the handler's error, failing the test if the handler
// does not return in time.
func waitErr(t *testing.T, errc <-chan error) error {
	t.Helper()
	select {
	case err := <-errc:
		return err
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for the service handler to return")
		return nil
	}
}

// runCancelled runs upload-pack with scmd against an empty repository, reads
// the first line it writes, then calls atCancel with that line and cancels
// the context, and returns the handler's error. Stdin stays open and empty,
// so upload-pack waits for the client after the advertisement until it is
// killed.
func runCancelled(t *testing.T, scmd ServiceCommand, atCancel func(line string)) error {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	stdin, stdinW := io.Pipe()
	defer stdinW.Close() //nolint: errcheck
	stdout, stdoutW := io.Pipe()
	defer stdout.Close() //nolint: errcheck

	scmd.Stdin = stdin
	scmd.Stdout = stdoutW
	scmd.Dir = initBare(t)

	errc := make(chan error, 1)
	go func() {
		errc <- UploadPack(ctx, scmd)
		stdoutW.Close() //nolint: errcheck
	}()

	line := readLine(t, stdout)
	go io.Copy(io.Discard, stdout) //nolint: errcheck
	if atCancel != nil {
		atCancel(line)
	}
	cancel()

	return waitErr(t, errc)
}

// TestUploadPackClientGoesAway runs upload-pack the way a client that only
// wants the ref advertisement does: it reads the first line and then goes
// away, which cancels the context. The error must say the context was
// cancelled rather than report the killed git process.
func TestUploadPackClientGoesAway(t *testing.T) {
	err := runCancelled(t, ServiceCommand{}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	// The original Wait error stays in the message.
	if runtime.GOOS != "windows" && !strings.Contains(err.Error(), "signal: killed") {
		t.Fatalf("expected the error to keep the Wait error, got %v", err)
	}
}

// TestUploadPackChildHoldsOutputOpen covers a process started by git that
// keeps git's output open after git was killed, as the git wrapper on Windows
// does with the git it runs. The handler must still return once the context
// is cancelled instead of waiting for that process.
func TestUploadPackChildHoldsOutputOpen(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not found")
	}

	err = runCancelled(t, ServiceCommand{
		// Stand in for git: start a child that inherits stdout and outlives
		// the shell, print its pid, and wait.
		CmdFunc: func(c *exec.Cmd) {
			c.Path = sh
			c.Args = []string{"sh", "-c", "sleep 60 & echo $!; wait"}
		},
	}, func(line string) {
		pid, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil {
			t.Fatalf("reading the child pid: %v", err)
		}
		t.Cleanup(func() {
			if p, err := os.FindProcess(pid); err == nil {
				p.Kill() //nolint: errcheck
			}
		})
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

// TestUploadPackFailsAfterCancel covers git failing on its own after the
// context was cancelled but before it was killed: the real failure must be
// reported, not the cancellation.
func TestUploadPackFailsAfterCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	stdin, stdinW := io.Pipe()
	defer stdinW.Close() //nolint: errcheck
	stdout, stdoutW := io.Pipe()
	defer stdout.Close() //nolint: errcheck

	errc := make(chan error, 1)
	go func() {
		errc <- UploadPack(ctx, ServiceCommand{
			Stdin:  stdin,
			Stdout: stdoutW,
			Dir:    initBare(t),
			// Do not kill git when the context is cancelled, so that it
			// exits on its own.
			CmdFunc: func(c *exec.Cmd) { c.Cancel = func() error { return nil } },
		})
		stdoutW.Close() //nolint: errcheck
	}()

	readLine(t, stdout)
	go io.Copy(io.Discard, stdout) //nolint: errcheck
	cancel()

	// A malformed packet line makes upload-pack fail with a protocol error.
	if _, err := stdinW.Write([]byte("zzzz")); err != nil {
		t.Fatalf("writing to upload-pack: %v", err)
	}
	stdinW.Close() //nolint: errcheck

	err := waitErr(t, errc)
	if err == nil {
		t.Fatal("expected upload-pack to fail")
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("expected the git failure, got the cancellation: %v", err)
	}
}

func TestStoppedByContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if stoppedByContext(context.Background(), context.Canceled, true) {
		t.Error("a context that is not done must not report a cancellation")
	}
	if !stoppedByContext(ctx, context.Canceled, false) {
		t.Error("the context error must report a cancellation")
	}
	if !stoppedByContext(ctx, fmt.Errorf("wait: %w", context.Canceled), false) {
		t.Error("a wrapped context error must report a cancellation")
	}
	if stoppedByContext(ctx, errors.New("some other failure"), false) {
		t.Error("an unrelated error must not report a cancellation")
	}

	if runtime.GOOS == "windows" {
		if !stoppedByContext(ctx, errors.New("exit status 1"), true) {
			t.Error("a command killed for the context must report a cancellation")
		}
		t.Skip("uses sh")
	}

	exited := exec.CommandContext(t.Context(), "sh", "-c", "exit 1").Run()
	if exited == nil {
		t.Fatal("expected sh to exit with an error")
	}
	if stoppedByContext(ctx, exited, false) {
		t.Errorf("a process that exited on its own must not report a cancellation: %v", exited)
	}
	// Killing a process that already exited but was not yet reaped succeeds
	// on Unix, so the killed flag must not hide its exit status.
	if stoppedByContext(ctx, exited, true) {
		t.Errorf("a process that exited on its own must not report a cancellation even if killed: %v", exited)
	}

	killed := exec.CommandContext(t.Context(), "sh", "-c", "kill -9 $$").Run()
	if killed == nil {
		t.Fatal("expected sh to be killed")
	}
	if !stoppedByContext(ctx, killed, false) {
		t.Errorf("a process killed by a signal must report a cancellation: %v", killed)
	}
}
