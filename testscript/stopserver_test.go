package testscript

import (
	"context"
	"net"
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
