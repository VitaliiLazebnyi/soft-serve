//go:build linux || darwin

package test

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestRandomPortPanicsWhenItCannotBind checks that RandomPort gives up with a
// clear error when binding fails for a reason other than the port being in
// use, here running out of file descriptors, instead of retrying forever
// while holding its lock.
func TestRandomPortPanicsWhenItCannotBind(t *testing.T) {
	if os.Getenv("RANDOMPORT_RLIMIT_CHILD") == "1" {
		lim := syscall.Rlimit{Cur: 32, Max: 32}
		if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
			t.Fatal(err)
		}
		for {
			RandomPort() // each port keeps a descriptor open until they run out
		}
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestRandomPortPanicsWhenItCannotBind$") //nolint:gosec,noctx
	cmd.Env = append(os.Environ(), "RANDOMPORT_RLIMIT_CHILD=1")
	done := make(chan struct{})
	var out []byte
	var err error
	go func() {
		out, err = cmd.CombinedOutput()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatal("RandomPort kept retrying after it ran out of file descriptors")
	}
	if err == nil {
		t.Fatalf("child exited successfully; want a panic\n%s", out)
	}
	if !strings.Contains(string(out), "panic: test.RandomPort:") || !strings.Contains(string(out), "too many open files") {
		t.Fatalf("child did not panic with the bind error:\n%s", out)
	}
}
