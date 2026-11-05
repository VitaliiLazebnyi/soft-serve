package ssrf

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDialer records every address the secure client dials and connects
// only to the addresses in up, so failover runs without a network.
type fakeDialer struct {
	mu        sync.Mutex
	up        map[string]bool
	dialed    []string
	deadlines []time.Time
}

func newFakeDialer(t *testing.T, up ...string) *fakeDialer {
	t.Helper()
	f := &fakeDialer{up: map[string]bool{}}
	for _, a := range up {
		f.up[a] = true
	}
	orig := dialContext
	dialContext = func(ctx context.Context, _, addr string) (net.Conn, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.dialed = append(f.dialed, addr)
		d, _ := ctx.Deadline()
		f.deadlines = append(f.deadlines, d)
		if !f.up[addr] {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused: " + addr)}
		}
		client, server := net.Pipe()
		t.Cleanup(func() { server.Close() })
		return client, nil
	}
	t.Cleanup(func() { dialContext = orig })
	return f
}

// TestSecureDialRefusesMixedAnswers covers a name that answers with both
// public and internal addresses. The whole host is refused, whichever order
// the answers come in, and nothing is dialed.
func TestSecureDialRefusesMixedAnswers(t *testing.T) {
	fakeResolver(t, map[string][]string{
		"public-first.example":  {"140.82.112.3", "10.0.0.1"},
		"private-first.example": {"169.254.169.254", "140.82.112.3"},
		"v6-private.example":    {"140.82.112.3", "::1"},
	})
	f := newFakeDialer(t, "140.82.112.3:443")

	for _, host := range []string{"public-first.example", "private-first.example", "v6-private.example"} {
		t.Run(host, func(t *testing.T) {
			conn, err := secureDial(context.Background(), "tcp", net.JoinHostPort(host, "443"))
			if conn != nil {
				conn.Close()
			}
			if !errors.Is(err, ErrPrivateIP) {
				t.Fatalf("secureDial(%s) error = %v, want ErrPrivateIP", host, err)
			}
		})
	}
	if len(f.dialed) != 0 {
		t.Errorf("dialed %v for refused hosts, want nothing", f.dialed)
	}
}

// TestSecureDialFailsOverToNextAddress covers an unreachable first record:
// the next validated address is tried, as an IP literal.
func TestSecureDialFailsOverToNextAddress(t *testing.T) {
	fakeResolver(t, map[string][]string{
		"multi.example": {"140.82.112.3", "2606:4700::1111", "140.82.112.4"},
	})
	f := newFakeDialer(t, "[2606:4700::1111]:443")

	conn, err := secureDial(context.Background(), "tcp", "multi.example:443")
	if err != nil {
		t.Fatalf("secureDial() error = %v, want failover to the second address", err)
	}
	conn.Close()

	want := []string{"140.82.112.3:443", "[2606:4700::1111]:443"}
	if strings.Join(f.dialed, " ") != strings.Join(want, " ") {
		t.Errorf("dialed %v, want %v", f.dialed, want)
	}
}

// TestSecureDialAllAddressesFail checks that every address is tried and the
// error names each failure.
func TestSecureDialAllAddressesFail(t *testing.T) {
	fakeResolver(t, map[string][]string{
		"down.example": {"140.82.112.3", "140.82.112.4"},
	})
	f := newFakeDialer(t)

	_, err := secureDial(context.Background(), "tcp", "down.example:443")
	if err == nil {
		t.Fatal("secureDial() succeeded with every address down")
	}
	if len(f.dialed) != 2 {
		t.Errorf("dialed %v, want both addresses", f.dialed)
	}
	for _, addr := range []string{"140.82.112.3:443", "140.82.112.4:443", "all 2 addresses failed"} {
		if !strings.Contains(err.Error(), addr) {
			t.Errorf("error %q does not mention %q", err, addr)
		}
	}
	var opErr *net.OpError
	if !errors.As(err, &opErr) {
		t.Errorf("error %v does not wrap the dial errors", err)
	}
}

// TestSecureDialAddressFamily checks that a tcp4 or tcp6 dial only tries
// that family, while every answer is still validated.
func TestSecureDialAddressFamily(t *testing.T) {
	fakeResolver(t, map[string][]string{
		"dual.example":   {"2606:4700::1111", "140.82.112.3"},
		"v6only.example": {"2606:4700::1111"},
		"badv6.example":  {"140.82.112.3", "fc00::1"},
	})
	f := newFakeDialer(t, "140.82.112.3:443")

	conn, err := secureDial(context.Background(), "tcp4", "dual.example:443")
	if err != nil {
		t.Fatalf("tcp4 dial error = %v", err)
	}
	conn.Close()
	if len(f.dialed) != 1 || f.dialed[0] != "140.82.112.3:443" {
		t.Errorf("tcp4 dialed %v, want only the IPv4 address", f.dialed)
	}

	if _, err := secureDial(context.Background(), "tcp4", "v6only.example:443"); err == nil {
		t.Error("tcp4 dial to an IPv6-only host succeeded")
	}

	// The private IPv6 answer refuses the host even though tcp4 would never
	// have dialed it.
	if _, err := secureDial(context.Background(), "tcp4", "badv6.example:443"); !errors.Is(err, ErrPrivateIP) {
		t.Errorf("tcp4 dial with a private IPv6 answer error = %v, want ErrPrivateIP", err)
	}
}

// TestSecureDialDeadlines checks that attempts share the caller's deadline
// rather than each getting a full timeout, and that a dial without a
// deadline is still bounded.
func TestSecureDialDeadlines(t *testing.T) {
	fakeResolver(t, map[string][]string{
		"down.example": {"140.82.112.1", "140.82.112.2", "140.82.112.3", "140.82.112.4"},
	})

	t.Run("caller deadline", func(t *testing.T) {
		f := newFakeDialer(t)
		parent := time.Now().Add(20 * time.Second)
		ctx, cancel := context.WithDeadline(context.Background(), parent)
		defer cancel()

		_, _ = secureDial(ctx, "tcp", "down.example:443")
		if len(f.deadlines) != 4 {
			t.Fatalf("made %d attempts, want 4", len(f.deadlines))
		}
		for i, d := range f.deadlines {
			if d.IsZero() || d.After(parent) {
				t.Errorf("attempt %d deadline %v, want set and not after %v", i, d, parent)
			}
		}
		// Four attempts out of 20s: the first gets a share, not all of it.
		if first := time.Until(f.deadlines[0]); first > 6*time.Second {
			t.Errorf("first attempt given %v of 20s across 4 addresses", first)
		}
	})

	t.Run("no caller deadline", func(t *testing.T) {
		f := newFakeDialer(t)
		start := time.Now()
		_, _ = secureDial(context.Background(), "tcp", "down.example:443")
		for i, d := range f.deadlines {
			if d.IsZero() || d.Sub(start) > dialTimeout+time.Second {
				t.Errorf("attempt %d deadline %v, want within %v of start", i, d, dialTimeout)
			}
		}
	})

	t.Run("expired context", func(t *testing.T) {
		f := newFakeDialer(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := secureDial(ctx, "tcp", "down.example:443")
		if !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want context.Canceled", err)
		}
		if len(f.dialed) != 0 {
			t.Errorf("dialed %v after cancellation", f.dialed)
		}
	})
}

// TestSecureDialResolverErrorsHidden checks that DNS failures do not reach
// the webhook delivery log with resolver detail.
func TestSecureDialResolverErrorsHidden(t *testing.T) {
	fakeResolver(t, nil)
	newFakeDialer(t)

	_, err := secureDial(context.Background(), "tcp", "missing.example:443")
	if err == nil {
		t.Fatal("secureDial() succeeded for an unresolvable host")
	}
	if strings.Contains(err.Error(), "no such host") {
		t.Errorf("error %q leaks resolver detail", err)
	}
}

// TestValidateGitRemoteRefusesMixedAnswers is the import-side counterpart of
// TestSecureDialRefusesMixedAnswers.
func TestValidateGitRemoteRefusesMixedAnswers(t *testing.T) {
	fakeResolver(t, map[string][]string{
		"mixed.example": {"140.82.112.3", "10.0.0.1"},
	})
	for _, remote := range []string{"https://mixed.example/x.git", "git://mixed.example/x.git"} {
		if _, err := ValidateGitRemote(remote, anySSH); !errors.Is(err, ErrPrivateIP) {
			t.Errorf("ValidateGitRemote(%q) error = %v, want ErrPrivateIP", remote, err)
		}
	}
}
