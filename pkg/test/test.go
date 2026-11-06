package test

import (
	"context"
	"math/rand/v2"
	"net"
	"strconv"
	"sync"
)

var (
	// used holds a reservation for every port this process has returned.
	used = map[int]net.PacketConn{}
	lock sync.Mutex

	hasIPv6     bool
	hasIPv6Once sync.Once
)

// Test ports come from below every common ephemeral range (Linux starts at
// 32768; macOS and Windows at 49152). A port the OS hands out for ":0" is
// always ephemeral, so no listener elsewhere in the tests can be given a port
// returned here in the gap before the server under test binds it.
const (
	minPort = 20000
	maxPort = 32000
)

// RandomPort returns a TCP port that is free on IPv4 loopback and, where the
// machine has it, IPv6 loopback, and that no other caller of RandomPort, in
// this process or another, has been given. A client dialing "localhost" may
// try either address, so a port in use on one of them would reach the wrong
// server.
//
// The TCP port itself cannot be held open: the server under test binds it
// later, often in another process. Checking that it is free is not enough on
// its own, since go test runs packages, and developers run worktrees, as
// separate processes that all pick from the same range, and two of them can
// pick the same free port before either server binds it. So each returned
// port is also reserved by binding the same port number on UDP loopback,
// which TCP listeners do not conflict with, and holding that socket for the
// life of the process. Another process's RandomPort cannot bind it, so it
// skips the port. The OS releases the reservation when the process exits.
// This is mainly used for testing.
func RandomPort() int {
	hasIPv6Once.Do(func() { hasIPv6 = canListen("::1", 0) })
	lock.Lock()
	defer lock.Unlock()
	for {
		port := minPort + rand.IntN(maxPort-minPort) //nolint:gosec
		if _, ok := used[port]; ok {
			continue
		}
		reservation, err := (&net.ListenConfig{}).ListenPacket(context.Background(), "udp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			continue // reserved by another process
		}
		if !canListen("127.0.0.1", port) || hasIPv6 && !canListen("::1", port) {
			_ = reservation.Close()
			continue
		}
		used[port] = reservation
		return port
	}
}

// canListen reports whether port can be bound on host right now.
func canListen(host string, port int) bool {
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}
