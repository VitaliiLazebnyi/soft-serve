package test

import (
	"context"
	"net"
	"strconv"
	"testing"
)

// TestRandomPortReservesAcrossProcesses checks that a returned port stays
// reserved, which is what keeps another process's RandomPort from returning
// it, while the TCP port itself stays free for the server under test.
func TestRandomPortReservesAcrossProcesses(t *testing.T) {
	port := RandomPort()
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))

	if c, err := (&net.ListenConfig{}).ListenPacket(context.Background(), "udp4", addr); err == nil {
		_ = c.Close()
		t.Fatalf("UDP %s could be bound; the port is not reserved", addr)
	}

	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", addr)
	if err != nil {
		t.Fatalf("TCP %s cannot be bound by the server under test: %v", addr, err)
	}
	_ = l.Close()
}

func TestRandomPortUnique(t *testing.T) {
	seen := map[int]bool{}
	for range 200 {
		p := RandomPort()
		if seen[p] {
			t.Fatalf("port %d returned twice", p)
		}
		if p < minPort || p >= maxPort {
			t.Fatalf("port %d outside [%d, %d)", p, minPort, maxPort)
		}
		seen[p] = true
	}
}
