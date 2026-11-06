package testscript

import (
	"context"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"
)

// waitClosed returns once nothing listens on the address, and keeps waiting
// while something does, rather than taking any failed dial as closed.
func TestWaitClosed(t *testing.T) {
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	if err := waitClosed(addr, 500*time.Millisecond); err == nil {
		t.Fatalf("waitClosed returned nil while %s was listening", addr)
	}

	_ = l.Close()
	if err := waitClosed(addr, 30*time.Second); err != nil {
		t.Fatalf("waitClosed after the listener closed: %v", err)
	}
}

// waitExit returns once a process has exited and keeps waiting while it runs,
// without taking the exit status from the code that waits on it.
func TestWaitExit(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^$") //nolint:gosec,noctx
	cmd.Env = append(os.Environ(), "WAITEXIT_CHILD=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	if err := waitExit(cmd.Process, 300*time.Millisecond); err == nil {
		t.Fatal("waitExit returned nil while the process was running")
	}
	_ = stdin.Close() // the child exits when its stdin closes
	if err := waitExit(cmd.Process, 30*time.Second); err != nil {
		t.Fatalf("waitExit after the process exited: %v", err)
	}
	if err := <-waited; err != nil {
		t.Fatalf("exit status lost: %v", err)
	}
}

// stopserver waits only for the server its stop request went to, not for
// another one the script runs at the same time.
func TestServerProcesses(t *testing.T) {
	a, b := &os.Process{Pid: 1}, &os.Process{Pid: 2}
	cmds := []*exec.Cmd{
		{Args: []string{"soft", "serve"}, Process: a, Env: []string{
			"SOFT_SERVE_HTTP_PUBLIC_URL=http://localhost:2", // overridden below
			"SOFT_SERVE_HTTP_PUBLIC_URL=http://localhost:1",
		}},
		{Args: []string{"soft", "serve"}, Process: b, Env: []string{"SOFT_SERVE_HTTP_PUBLIC_URL=http://localhost:2"}},
		{Args: []string{"soft", "repo"}, Process: &os.Process{Pid: 3}, Env: []string{"SOFT_SERVE_HTTP_PUBLIC_URL=http://localhost:1"}},
	}
	got := serverProcesses(cmds, "http://localhost:1")
	if len(got) != 1 || got[0] != a {
		t.Fatalf("serverProcesses returned %v, want only the first server's process", got)
	}
}
