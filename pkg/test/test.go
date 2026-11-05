package test

import (
	"context"
	"math/rand/v2"
	"net"
	"strconv"
	"sync"
)

var (
	used = map[int]struct{}{}
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

// RandomPort returns a port that is free on IPv4 loopback and, where the
// machine has it, IPv6 loopback, and that this process has not returned
// before. A client dialing "localhost" may try either address, so a port in
// use on one of them would reach the wrong server.
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
		if !canListen("127.0.0.1", port) || hasIPv6 && !canListen("::1", port) {
			continue
		}
		used[port] = struct{}{}
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
