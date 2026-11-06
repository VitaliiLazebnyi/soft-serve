package ssrf

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"
)

var (
	// ErrPrivateIP is returned when a connection to a private or internal IP is blocked.
	ErrPrivateIP = errors.New("connection to private or internal IP address is not allowed")
	// ErrInvalidScheme is returned when a URL scheme is not http or https.
	ErrInvalidScheme = errors.New("URL must use http or https scheme")
	// ErrInvalidURL is returned when a URL is invalid.
	ErrInvalidURL = errors.New("invalid URL")
)

// ClientTimeout caps each request made by the client from NewSecureClient,
// including reading the response body.
const ClientTimeout = 30 * time.Second

// dialTimeout bounds one dial made by the secure client, from DNS resolution
// through the last connection attempt. It is a fixed budget: net/http dials
// with a context detached from the request (context.WithoutCancel), so the
// request deadline and ClientTimeout never reach secureDial. A shorter
// deadline already on the dial context still applies.
const dialTimeout = 10 * time.Second

// maxDialAddrs caps how many of a name's validated addresses one dial tries.
// Every answer is still validated; the cap only stops a name with a long
// answer list from turning one request into many outbound connection
// attempts to addresses its owner picked. Four matches common client
// practice and leaves each attempt a useful share of dialTimeout.
const maxDialAddrs = 4

// minDialAttempt is the least time an attempt is given when the budget is
// split between several addresses, so later attempts are not squeezed to
// nothing. It mirrors the standard library's choice.
const minDialAttempt = 2 * time.Second

// dialContext dials a single, already validated address. Tests replace it so
// failover can be exercised without a network.
var dialContext = (&net.Dialer{KeepAlive: 30 * time.Second}).DialContext

// NewSecureClient returns an HTTP client with SSRF protection.
// It validates resolved IPs at dial time to block connections to private
// and internal networks; see secureDial. Redirects are disabled to match the
// webhook client convention and prevent redirect-based SSRF.
func NewSecureClient() *http.Client {
	return &http.Client{
		Timeout: ClientTimeout,
		Transport: &http.Transport{
			DialContext:           secureDial,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// secureDial resolves addr's host once, validates every address it resolves
// to, and dials up to maxDialAddrs of them in resolver order until one
// connects, all within dialTimeout.
//
// A host is refused outright if any of its addresses is private or internal,
// even when others are public. A name that answers with a mix is a DNS
// rebinding setup, not a legitimate endpoint, and this matches ValidateURL
// and ValidateGitRemote, which refuse such a name at configuration time.
//
// The name is never resolved again after validation: each attempt dials an
// IP literal, so the address connected to is always one that was checked.
// TLS still verifies against the original hostname, which net/http takes
// from the request URL rather than from the dialed address.
func secureDial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err //nolint:wrapcheck
	}

	// Bound the whole dial, resolution included, so neither a slow resolver
	// nor a long answer list can stretch it past dialTimeout.
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	ips, err := resolveDialTargets(ctx, network, host)
	if err != nil {
		return nil, err
	}
	skipped := 0
	if len(ips) > maxDialAddrs {
		skipped = len(ips) - maxDialAddrs
		ips = ips[:maxDialAddrs]
	}

	var firstErr error
	failed := 0
	for i, ip := range ips {
		if ctx.Err() != nil {
			break
		}
		attemptCtx, cancel := context.WithDeadline(ctx, attemptDeadline(ctx, len(ips)-i))
		conn, err := dialContext(attemptCtx, network, net.JoinHostPort(ip.String(), port))
		cancel()
		if err == nil {
			return conn, nil
		}
		if firstErr == nil {
			firstErr = err
		}
		failed++
	}

	// Only the first failure is kept: the message is stored with every
	// webhook delivery, so it must not grow with the number of addresses.
	switch {
	case failed == 0:
		return nil, fmt.Errorf("connect to %s: %w", host, ctx.Err())
	case failed == 1 && skipped == 0:
		return nil, firstErr
	case skipped == 0:
		return nil, fmt.Errorf("connect to %s: %d addresses failed; first: %w", host, failed, firstErr)
	default:
		return nil, fmt.Errorf("connect to %s: %d addresses failed, %d more not tried; first: %w", host, failed, skipped, firstErr)
	}
}

// resolveDialTargets returns the addresses secureDial may dial for host, in
// the order the resolver returned them. Every address the name resolves to
// is checked, and any private or internal one refuses the host as a whole.
func resolveDialTargets(ctx context.Context, network, host string) ([]net.IPAddr, error) {
	if ip := net.ParseIP(host); ip != nil {
		if isPrivateOrInternal(ip) {
			return nil, ErrPrivateIP
		}
		return []net.IPAddr{{IP: ip}}, nil
	}

	ips, err := lookupIPAddr(ctx, host)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			// Cancelled or out of time; say so rather than blame DNS.
			return nil, fmt.Errorf("resolve %s: %w", host, ctxErr)
		}
		// Not wrapped: the resolver error names the server's DNS resolver,
		// and this message is stored with the webhook delivery where the
		// repo admin reads it.
		return nil, fmt.Errorf("DNS resolution failed for host %s", host)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("no IP addresses found for host: %s", host)
	}
	for _, ip := range ips {
		if isPrivateOrInternal(ip.IP) {
			return nil, ErrPrivateIP
		}
	}

	// Every answer is validated above; only the dial is limited to the
	// address family asked for. http.Transport always asks for "tcp", so
	// this only matters to a caller that names a family, but dialing a
	// literal of the wrong family would fail anyway.
	var targets []net.IPAddr
	for _, ip := range ips {
		is4 := ip.IP.To4() != nil
		if (network == "tcp4" && !is4) || (network == "tcp6" && is4) {
			continue
		}
		targets = append(targets, ip)
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("no %s addresses found for host: %s", network, host)
	}
	return targets, nil
}

// attemptDeadline gives the next of the remaining attempts an equal share of
// the time left on ctx, at least minDialAttempt, but never past ctx's own
// deadline.
func attemptDeadline(ctx context.Context, remaining int) time.Time {
	now := time.Now()
	deadline, _ := ctx.Deadline() // secureDial always sets one
	share := max(deadline.Sub(now)/time.Duration(remaining), minDialAttempt)
	if d := now.Add(share); d.Before(deadline) {
		return d
	}
	return deadline
}

// blockedPrefixes lists every network an outbound request must not reach.
//
// Prefixes are used rather than byte comparisons so the ranges stay readable
// and auditable against the RFCs that define them. This is also how the
// standard library models the same address space internally.
//
// The IPv6 transition ranges need explanation. Each embeds an arbitrary IPv4
// address inside an IPv6 one, so an address that looks public to the IPv6
// checks can name a loopback, RFC1918, or cloud metadata host once a relay
// decodes it. They are blocked in full rather than decoded and re-checked,
// because nothing here has a legitimate reason to reach a host through one of
// these encodings. Blocking the range removes the whole class of bypass
// instead of depending on getting each decoding exactly right.
var blockedPrefixes = []netip.Prefix{
	// IPv4.
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network"
	netip.MustParsePrefix("10.0.0.0/8"),      // RFC1918 private
	netip.MustParsePrefix("100.64.0.0/10"),   // RFC6598 shared address space (CGNAT)
	netip.MustParsePrefix("127.0.0.0/8"),     // loopback
	netip.MustParsePrefix("169.254.0.0/16"),  // link-local, includes cloud metadata
	netip.MustParsePrefix("172.16.0.0/12"),   // RFC1918 private
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // TEST-NET-1
	netip.MustParsePrefix("192.88.99.0/24"),  // RFC7526 6to4 relay anycast
	netip.MustParsePrefix("192.168.0.0/16"),  // RFC1918 private
	netip.MustParsePrefix("198.18.0.0/15"),   // RFC2544 benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // TEST-NET-2
	netip.MustParsePrefix("203.0.113.0/24"),  // TEST-NET-3
	netip.MustParsePrefix("224.0.0.0/4"),     // multicast
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved, includes broadcast

	// IPv6.
	netip.MustParsePrefix("::1/128"),        // loopback
	netip.MustParsePrefix("64:ff9b::/96"),   // RFC6052 NAT64 well-known prefix
	netip.MustParsePrefix("64:ff9b:1::/48"), // RFC8215 NAT64 local-use prefix
	netip.MustParsePrefix("100::/64"),       // RFC6666 discard-only
	netip.MustParsePrefix("2001::/32"),      // RFC4380 Teredo
	netip.MustParsePrefix("2001:db8::/32"),  // documentation
	netip.MustParsePrefix("2002::/16"),      // RFC3056 6to4
	netip.MustParsePrefix("fc00::/7"),       // unique local
	netip.MustParsePrefix("fe80::/10"),      // link-local
	netip.MustParsePrefix("ff00::/8"),       // multicast

	// IPv4-compatible IPv6 (::x.x.x.x), deprecated by RFC4291 but still
	// parsed. Unmap leaves this form alone, so the IPv4 prefixes above do not
	// apply to it. Covers the unspecified address too.
	netip.MustParsePrefix("::/96"),
}

// isPrivateOrInternal reports whether an IP address is private, internal, or
// otherwise unsafe to send an outbound request to.
func isPrivateOrInternal(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		// Not an address we can reason about, so refuse rather than allow.
		return true
	}

	// Treat an IPv4 address written in IPv6 form (::ffff:127.0.0.1) as the
	// IPv4 address it names, so the IPv4 prefixes apply to it.
	addr = addr.Unmap()

	return slices.ContainsFunc(blockedPrefixes, func(p netip.Prefix) bool {
		return p.Contains(addr)
	})
}

// ValidateURL validates that a URL is safe to make requests to.
// It checks that the scheme is http/https, the hostname is not localhost,
// and all resolved IPs are public.
func ValidateURL(rawURL string) error {
	if rawURL == "" {
		return ErrInvalidURL
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("%w: cannot parse URL", ErrInvalidURL)
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		return ErrInvalidScheme
	}

	hostname := u.Hostname()
	if hostname == "" {
		return fmt.Errorf("%w: missing hostname", ErrInvalidURL)
	}

	if isLocalhost(hostname) {
		return ErrPrivateIP
	}

	if ip := net.ParseIP(hostname); ip != nil {
		if isPrivateOrInternal(ip) {
			return ErrPrivateIP
		}
		return nil
	}

	ips, err := net.DefaultResolver.LookupIPAddr(context.Background(), hostname)
	if err != nil {
		// See validateRemoteHost: the resolver error names the resolver.
		return fmt.Errorf("%w: cannot resolve hostname", ErrInvalidURL)
	}

	if slices.ContainsFunc(ips, func(addr net.IPAddr) bool {
		return isPrivateOrInternal(addr.IP)
	}) {
		return ErrPrivateIP
	}

	return nil
}

// ValidateIPBeforeDial validates an IP address before establishing a connection.
// This prevents DNS rebinding attacks by checking the resolved IP at dial time.
func ValidateIPBeforeDial(ip net.IP) error {
	if isPrivateOrInternal(ip) {
		return ErrPrivateIP
	}
	return nil
}

// isLocalhost checks if the hostname is localhost or similar.
func isLocalhost(hostname string) bool {
	hostname = strings.ToLower(hostname)
	return hostname == "localhost" ||
		hostname == "localhost.localdomain" ||
		strings.HasSuffix(hostname, ".localhost")
}
