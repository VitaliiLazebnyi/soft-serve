package git

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"charm.land/log/v2"
)

// failingWriter is a client connection that fails every write. When cancel
// is set it is called first, the way a client going away cancels the
// request context.
type failingWriter struct {
	err    error
	cancel context.CancelFunc
}

func (w failingWriter) Write([]byte) (int, error) {
	if w.cancel != nil {
		w.cancel()
	}
	return 0, w.err
}

// syncBuffer is a bytes.Buffer safe for concurrent use, for a logger used
// by goroutines the test does not wait for.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.buf.Bytes())
}

// captureLog swaps the default logger for one that records every level as
// JSON, and returns the recorded entries' levels by message.
func captureLog(t *testing.T) func() map[string]string {
	t.Helper()
	buf := &syncBuffer{}
	prev := log.Default()
	log.SetDefault(log.NewWithOptions(buf, log.Options{
		Level:     log.DebugLevel,
		Formatter: log.JSONFormatter,
	}))
	t.Cleanup(func() { log.SetDefault(prev) })
	return func() map[string]string {
		levels := map[string]string{}
		dec := json.NewDecoder(bytes.NewReader(buf.Bytes()))
		for {
			var entry struct{ Level, Msg string }
			if err := dec.Decode(&entry); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				t.Fatalf("decoding log: %v", err)
			}
			levels[entry.Msg] = entry.Level
		}
		return levels
	}
}

// A client that goes away makes copying git's output to it fail. That is
// expected and must not be logged as an error; any other copy failure still
// is.
func TestGitServiceOutputCopyErrorLevel(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo.git")
	if out, err := exec.CommandContext(t.Context(), "git", "init", "--bare", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}

	writeErr := errors.New("write failed")
	for _, tc := range []struct {
		name      string
		err       error
		cancelCtx bool
		want      string
	}{
		{"context cancelled", writeErr, true, "debug"},
		{"ssh channel closed", io.EOF, false, "debug"},
		{"closed pipe", io.ErrClosedPipe, false, "debug"},
		{"closed connection", fmt.Errorf("write: %w", net.ErrClosed), false, "debug"},
		{"broken pipe", &net.OpError{Op: "write", Net: "tcp", Err: os.NewSyscallError("write", syscall.EPIPE)}, false, "debug"},
		{"connection reset", &net.OpError{Op: "write", Net: "tcp", Err: os.NewSyscallError("write", syscall.ECONNRESET)}, false, "debug"},
		{"other failure", writeErr, false, "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			levels := captureLog(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			w := failingWriter{err: tc.err}
			if tc.cancelCtx {
				w.cancel = cancel
			}
			// The result does not matter here, only how the copy failure
			// is logged.
			_ = UploadPack(ctx, ServiceCommand{Stdout: w, Dir: dir})

			msg := "gitServiceHandler: failed to copy stdout: " + tc.err.Error()
			if got := levels()[msg]; got != tc.want {
				t.Errorf("%q logged at %q, want %q", msg, got, tc.want)
			}
		})
	}
}

// failingReader is a client connection that fails every read. When cancel
// is set it is called first, the way a client going away cancels the
// request context.
type failingReader struct {
	err    error
	cancel context.CancelFunc
}

func (r failingReader) Read([]byte) (int, error) {
	if r.cancel != nil {
		r.cancel()
	}
	return 0, r.err
}

// garbageReader is a client that sends bytes git does not accept, without
// end.
type garbageReader struct{}

func (garbageReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'z'
	}
	return len(p), nil
}

// waitForLog waits for the stdin copy, which the handler does not wait
// for, to log its failure, and returns the entry's message and level.
func waitForLog(t *testing.T, levels func() map[string]string, prefix string) (string, string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		for msg, level := range levels() {
			if strings.HasPrefix(msg, prefix) {
				return msg, level
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no log entry starting with %q", prefix)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Copying the client's input to git fails when the client goes away, or
// when git exits before reading all of it. Both are expected and must not
// be logged as an error; any other copy failure still is.
func TestGitServiceInputCopyErrorLevel(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo.git")
	if out, err := exec.Command("git", "init", "--bare", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}

	const prefix = "gitServiceHandler: failed to copy stdin: "
	readErr := errors.New("read failed")
	for _, tc := range []struct {
		name      string
		stdin     io.Reader
		cancelCtx bool
		want      string
	}{
		// git rejects the input and exits while the client keeps sending,
		// so writing to git fails with a broken pipe, or with
		// os.ErrClosed once the pipe was closed after git exited.
		{"git exited", garbageReader{}, false, "debug"},
		{"context cancelled", failingReader{err: readErr}, true, "debug"},
		{"closed pipe", failingReader{err: io.ErrClosedPipe}, false, "debug"},
		{"closed connection", failingReader{err: fmt.Errorf("read: %w", net.ErrClosed)}, false, "debug"},
		{"connection reset", failingReader{err: &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}}, false, "debug"},
		{"other failure", failingReader{err: readErr}, false, "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			levels := captureLog(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			stdin := tc.stdin
			if r, ok := stdin.(failingReader); ok && tc.cancelCtx {
				r.cancel = cancel
				stdin = r
			}
			// The result does not matter here, only how the copy failure
			// is logged.
			_ = UploadPack(ctx, ServiceCommand{Stdin: stdin, Dir: dir})

			if msg, level := waitForLog(t, levels, prefix); level != tc.want {
				t.Errorf("%q logged at %q, want %q", msg, level, tc.want)
			}
		})
	}
}

// The copy to git's input fails with os.ErrClosed once git exited and its
// input pipe was closed; that is expected.
func TestGitGone(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"file already closed", &os.PathError{Op: "write", Path: "|1", Err: os.ErrClosed}, true},
		{"other failure", errors.New("write failed"), false},
	} {
		if got := gitGone(tc.err); got != tc.want {
			t.Errorf("%s: gitGone(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}

// On Windows a reset or aborted connection and a closed pipe come as
// Windows error codes, not as syscall.ECONNRESET or syscall.EPIPE. They
// mean the client went away there, and only there: on other systems the
// same numbers are unrelated errors or none.
func TestClientGoneWindowsErrors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		errno syscall.Errno
	}{
		{"WSAECONNRESET", 10054},
		{"WSAECONNABORTED", 10053},
		{"ERROR_BROKEN_PIPE", 109},
		{"ERROR_NO_DATA", 232},
	} {
		for _, err := range []error{
			tc.errno,
			&net.OpError{Op: "write", Net: "tcp", Err: os.NewSyscallError("wsasend", tc.errno)},
			&os.PathError{Op: "write", Path: "|1", Err: tc.errno},
		} {
			if !clientGoneOn("windows", err) {
				t.Errorf("%s: clientGoneOn(windows, %v) = false, want true", tc.name, err)
			}
			for _, goos := range []string{"linux", "darwin"} {
				if clientGoneOn(goos, err) {
					t.Errorf("%s: clientGoneOn(%s, %v) = true, want false", tc.name, goos, err)
				}
			}
		}
	}
	if clientGoneOn("windows", syscall.Errno(10061)) { // WSAECONNREFUSED
		t.Error("clientGoneOn(windows, WSAECONNREFUSED) = true, want false")
	}
}
