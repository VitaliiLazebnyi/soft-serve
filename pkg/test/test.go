package test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"runtime"
	"strconv"
	"sync"
	"syscall"
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

	// maxAttempts bounds how many ports RandomPort tries before giving up.
	// It is a few times the size of the range, so that a range that is
	// mostly but not entirely taken still yields a port.
	maxAttempts = 4 * (maxPort - minPort)
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
//
// Only a port that is in use is skipped. Any other error, such as running out
// of file descriptors or a sandbox that forbids binding, would fail for every
// port, so RandomPort panics with it instead of retrying forever. It also
// panics when it finds no free port after maxAttempts tries, for example
// because concurrent test processes hold most of the range.
// This is mainly used for testing.
func RandomPort() int {
	hasIPv6Once.Do(func() { hasIPv6 = canListen("::1", 0) == nil })
	lock.Lock()
	defer lock.Unlock()
	for range maxAttempts {
		port := minPort + rand.IntN(maxPort-minPort) //nolint:gosec
		if _, ok := used[port]; ok {
			continue
		}
		reservation, err := (&net.ListenConfig{}).ListenPacket(context.Background(), "udp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			skipIfInUse(err) // reserved by another process
			continue
		}
		err = canListen("127.0.0.1", port)
		if err == nil && hasIPv6 {
			err = canListen("::1", port)
		}
		if err != nil {
			_ = reservation.Close()
			skipIfInUse(err)
			continue
		}
		used[port] = reservation
		return port
	}
	panic(fmt.Sprintf("test.RandomPort: no free port in [%d, %d) after %d attempts", minPort, maxPort, maxAttempts))
}

// skipIfInUse returns if err means the port is in use, and panics otherwise.
func skipIfInUse(err error) {
	if !inUse(err) {
		panic(fmt.Sprintf("test.RandomPort: %v", err))
	}
}

// inUse reports whether err from binding a port means that the port is taken,
// as opposed to binding failing for a reason that no other port would avoid.
func inUse(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	if errno == syscall.EADDRINUSE {
		return true
	}
	// On Windows, syscall.EADDRINUSE is not the code Winsock returns. A port
	// in a range the OS excludes, such as one Hyper-V reserved, fails with
	// WSAEACCES instead.
	const wsaeacces, wsaeaddrinuse = 10013, 10048
	return runtime.GOOS == "windows" && (errno == wsaeaddrinuse || errno == wsaeacces)
}

// canListen returns the error from binding port on host right now, if any.
func canListen(host string, port int) error {
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return err
	}
	_ = l.Close()
	return nil
}
