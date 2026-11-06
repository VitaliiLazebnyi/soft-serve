package ssrf

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// slack absorbs the time between a test noting its start and secureDial
// setting its deadline, so deadline checks stay stable on a loaded machine.
const slack = time.Second

// fakeDialer records every address the secure client dials and connects
// only to the addresses in up, so failover runs without a network.
type fakeDialer struct {
	mu        sync.Mutex
	up        map[string]bool
	route     map[string]string // address -> real listener to connect to
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
		if real, ok := f.route[addr]; ok {
			return (&net.Dialer{}).DialContext(ctx, "tcp", real)
		}
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
// error reports the count and the first failure only.
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
	if want := "2 addresses failed; first:"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not mention %q", err, want)
	}
	if !strings.Contains(err.Error(), "140.82.112.3:443") || strings.Contains(err.Error(), "140.82.112.4") {
		t.Errorf("error %q should name only the first failure", err)
	}
	var opErr *net.OpError
	if !errors.As(err, &opErr) {
		t.Errorf("error %v does not wrap the dial error", err)
	}
}

// TestSecureDialCapsAddresses checks that a long answer list is validated in
// full but only maxDialAddrs addresses are dialed, and that the error stays
// short.
func TestSecureDialCapsAddresses(t *testing.T) {
	var many []string
	for i := 1; i <= 200; i++ {
		many = append(many, fmt.Sprintf("140.82.%d.%d", i/250, i%250))
	}
	fakeResolver(t, map[string][]string{
		"many.example":    many,
		"manybad.example": append(slices.Clone(many), "10.0.0.1"),
	})
	f := newFakeDialer(t)

	_, err := secureDial(context.Background(), "tcp", "many.example:443")
	if len(f.dialed) != maxDialAddrs {
		t.Errorf("dialed %d addresses, want %d", len(f.dialed), maxDialAddrs)
	}
	if want := fmt.Sprintf("%d addresses failed, %d more not tried", maxDialAddrs, len(many)-maxDialAddrs); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("error = %v, want it to mention %q", err, want)
	}
	if err != nil && len(err.Error()) > 300 {
		t.Errorf("error is %d bytes, want it bounded", len(err.Error()))
	}

	// The private answer past the cap still refuses the host.
	f.dialed = nil
	if _, err := secureDial(context.Background(), "tcp", "manybad.example:443"); !errors.Is(err, ErrPrivateIP) {
		t.Errorf("error = %v, want ErrPrivateIP", err)
	}
	if len(f.dialed) != 0 {
		t.Errorf("dialed %v for a refused host", f.dialed)
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

// TestSecureDialDeadlines checks that attempts share one fixed budget rather
// than each getting a full timeout, that a shorter caller deadline still
// applies, and that cancellation is reported as such.
func TestSecureDialDeadlines(t *testing.T) {
	fakeResolver(t, map[string][]string{
		"down.example": {"140.82.112.1", "140.82.112.2", "140.82.112.3", "140.82.112.4"},
	})

	t.Run("no caller deadline", func(t *testing.T) {
		f := newFakeDialer(t)
		start := time.Now()
		_, _ = secureDial(context.Background(), "tcp", "down.example:443")
		if len(f.deadlines) != 4 {
			t.Fatalf("made %d attempts, want 4", len(f.deadlines))
		}
		for i, d := range f.deadlines {
			if d.IsZero() || d.Sub(start) > dialTimeout+slack {
				t.Errorf("attempt %d deadline %v, want within %v of start", i, d, dialTimeout)
			}
		}
		// Four attempts out of 10s: the first gets a share, not all of it.
		if first := f.deadlines[0].Sub(start); first > dialTimeout/4+time.Second {
			t.Errorf("first attempt given %v of %v across 4 addresses", first, dialTimeout)
		}
	})

	t.Run("longer caller deadline", func(t *testing.T) {
		f := newFakeDialer(t)
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		_, _ = secureDial(ctx, "tcp", "down.example:443")
		for i, d := range f.deadlines {
			if d.Sub(start) > dialTimeout+slack {
				t.Errorf("attempt %d deadline %v past the %v budget", i, d.Sub(start), dialTimeout)
			}
		}
	})

	t.Run("shorter caller deadline", func(t *testing.T) {
		f := newFakeDialer(t)
		parent := time.Now().Add(3 * time.Second)
		ctx, cancel := context.WithDeadline(context.Background(), parent)
		defer cancel()
		_, _ = secureDial(ctx, "tcp", "down.example:443")
		for i, d := range f.deadlines {
			if d.IsZero() || d.After(parent) {
				t.Errorf("attempt %d deadline %v, want set and not after %v", i, d, parent)
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

	t.Run("resolution is bounded", func(t *testing.T) {
		var lookupDeadline time.Time
		orig := lookupIPAddr
		lookupIPAddr = func(ctx context.Context, _ string) ([]net.IPAddr, error) {
			lookupDeadline, _ = ctx.Deadline()
			return []net.IPAddr{{IP: net.ParseIP("140.82.112.1")}}, nil
		}
		t.Cleanup(func() { lookupIPAddr = orig })
		newFakeDialer(t)
		start := time.Now()
		_, _ = secureDial(context.Background(), "tcp", "slow.example:443")
		if lookupDeadline.IsZero() || lookupDeadline.Sub(start) > dialTimeout+slack {
			t.Errorf("lookup deadline %v, want within %v of start", lookupDeadline, dialTimeout)
		}
	})
}

// TestSecureClientFailover drives secureDial through NewSecureClient, the
// way webhooks use it: the first address refuses, the second serves.
func TestSecureClientFailover(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	fakeResolver(t, map[string][]string{
		"multi.example": {"140.82.112.3", "140.82.112.4"},
	})
	f := newFakeDialer(t)
	f.route = map[string]string{"140.82.112.4:80": srv.Listener.Addr().String()}

	resp, err := NewSecureClient().Get("http://multi.example/hook")
	if err != nil {
		t.Fatalf("Get() error = %v, want failover to the second address", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
	if want := "140.82.112.3:80 140.82.112.4:80"; strings.Join(f.dialed, " ") != want {
		t.Errorf("dialed %v, want %s", f.dialed, want)
	}
}

// TestSecureClientDialBudget checks the budget as the real client sees it:
// net/http does not pass the request deadline to the dial, so attempts fit
// in dialTimeout whatever the request allows, and the address cap holds.
func TestSecureClientDialBudget(t *testing.T) {
	fakeResolver(t, map[string][]string{
		"down.example": {
			"140.82.112.1", "140.82.112.2", "140.82.112.3", "140.82.112.4",
			"140.82.112.5", "140.82.112.6", "140.82.112.7", "140.82.112.8",
		},
	})
	f := newFakeDialer(t)

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://down.example/hook", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := NewSecureClient().Do(req)
	if err == nil {
		resp.Body.Close()
		t.Fatal("Do() succeeded with every address down")
	}
	if len(f.dialed) != maxDialAddrs {
		t.Errorf("dialed %d addresses, want %d", len(f.dialed), maxDialAddrs)
	}
	for i, d := range f.deadlines {
		if d.IsZero() || d.Sub(start) > dialTimeout+slack {
			t.Errorf("attempt %d deadline %v after start, want within %v", i, d.Sub(start), dialTimeout)
		}
	}
	if !strings.Contains(err.Error(), "4 more not tried") {
		t.Errorf("error %q does not report the skipped addresses", err)
	}
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
