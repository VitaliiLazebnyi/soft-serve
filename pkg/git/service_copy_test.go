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
	"syscall"
	"testing"

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

// captureLog swaps the default logger for one that records every level as
// JSON, and returns the recorded entries' levels by message.
func captureLog(t *testing.T) func() map[string]string {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Default()
	log.SetDefault(log.NewWithOptions(&buf, log.Options{
		Level:     log.DebugLevel,
		Formatter: log.JSONFormatter,
	}))
	t.Cleanup(func() { log.SetDefault(prev) })
	return func() map[string]string {
		levels := map[string]string{}
		dec := json.NewDecoder(&buf)
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
