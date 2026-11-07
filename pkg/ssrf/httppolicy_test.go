package ssrf

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
)

func TestAllowedHostsPermitsHTTP(t *testing.T) {
	t.Parallel()
	allowed := mustAllowedHosts(t,
		"10.0.0.0/8",
		"127.0.0.1:8080",
		"hooks.internal",
		"ci.internal:9000",
		"169.254.10.0/24",
		"fd00:ec2::254",
		"0.0.0.0/0:7000",
	)

	tests := []struct {
		name, host, port, ip string
		want                 bool
	}{
		{"public address needs no entry", "example.com", "443", "93.184.216.34", true},
		{"CIDR entry covers address for any name", "anything.example", "443", "10.1.2.3", true},
		{"CIDR entry covers IP literal", "10.9.9.9", "80", "10.9.9.9", true},
		{"private address outside every entry", "anything.example", "443", "192.168.1.1", false},
		{"IP entry with port, matching port", "127.0.0.1", "8080", "127.0.0.1", true},
		{"IP entry with port, other port", "127.0.0.1", "8081", "127.0.0.1", false},
		{"hostname entry reaches loopback", "hooks.internal", "80", "127.0.0.1", true},
		{"hostname entry reaches RFC1918 outside the CIDR entries", "hooks.internal", "443", "192.168.5.5", true},
		{"hostname entry is case and dot insensitive", "Hooks.Internal.", "443", "192.168.5.5", true},
		{"hostname entry reaches IPv6 unique local", "hooks.internal", "443", "fd12::1", true},
		{"hostname entry never reaches link-local metadata", "hooks.internal", "80", "169.254.169.254", false},
		{"hostname entry never reaches Alibaba metadata", "hooks.internal", "80", "100.100.100.200", false},
		{"hostname entry never reaches 6to4", "hooks.internal", "80", "2002:a9fe:a9fe::1", false},
		{"hostname entry with port, other port", "ci.internal", "443", "10.255.0.1", true}, // the CIDR entry
		{"hostname entry with port, other port and address", "ci.internal", "443", "192.168.0.1", false},
		{"hostname entry with port, matching port", "ci.internal", "9000", "192.168.0.1", true},
		{"other hostname gets nothing from a hostname entry", "evil.example", "80", "192.168.5.5", false},
		{"IP literal is not matched by hostname entries", "192.168.5.5", "80", "192.168.5.5", false},
		{"narrow link-local entry opens its range", "x.example", "80", "169.254.10.7", true},
		{"narrow link-local entry does not open metadata", "x.example", "80", "169.254.169.254", false},
		{"exact metadata entry opens it", "x.example", "80", "fd00:ec2::254", true},
		{"broad entry opens private networks", "x.example", "7000", "192.168.1.1", true},
		{"broad entry does not open metadata", "x.example", "7000", "169.254.169.254", false},
		{"broad entry does not open Alibaba metadata", "x.example", "7000", "100.100.100.200", false},
		{"IPv4-mapped IPv6 is checked as IPv4", "x.example", "80", "::ffff:10.0.0.1", true},
		{"IPv4-mapped metadata stays blocked", "hooks.internal", "80", "::ffff:169.254.169.254", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := allowed.permitsHTTP(tt.host, tt.port, net.ParseIP(tt.ip)); got != tt.want {
				t.Errorf("permitsHTTP(%q, %q, %s) = %v, want %v", tt.host, tt.port, tt.ip, got, tt.want)
			}
		})
	}

	// A hostname entry alone opens no metadata or link-local address.
	hostOnly := mustAllowedHosts(t, "hooks.internal")
	for _, ip := range []string{"169.254.169.254", "fd00:ec2::254", "100.100.100.200", "fe80::1", "64:ff9b::a9fe:a9fe"} {
		if hostOnly.permitsHTTP("hooks.internal", "80", net.ParseIP(ip)) {
			t.Errorf("hostname entry permits %s", ip)
		}
	}

	// An IPv4-mapped network entry opens the IPv4 addresses it maps, and is
	// held to the same rules for metadata.
	mapped := mustAllowedHosts(t, "::ffff:10.0.0.0/104", "[::ffff:0:0/96]:7000")
	for _, tt := range []struct {
		ip, port string
		want     bool
	}{
		{"10.1.2.3", "80", true},
		{"::ffff:10.1.2.3", "80", true},
		{"192.168.1.1", "80", false},
		{"192.168.1.1", "7000", true},
		{"169.254.169.254", "7000", false},
	} {
		if got := mapped.permitsHTTP("x.example", tt.port, net.ParseIP(tt.ip)); got != tt.want {
			t.Errorf("mapped entries: permitsHTTP(%s:%s) = %v, want %v", tt.ip, tt.port, got, tt.want)
		}
	}

	var empty AllowedHosts
	for _, ip := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "fd00::1"} {
		if empty.permitsHTTP("hooks.internal", "80", net.ParseIP(ip)) {
			t.Errorf("empty allowlist permits %s", ip)
		}
	}
	if !empty.permitsHTTP("example.com", "443", net.ParseIP("93.184.216.34")) {
		t.Error("empty allowlist refuses a public address")
	}
}

func TestValidateURLAllowing(t *testing.T) {
	t.Parallel()
	lookup := fakeLookup(map[string][]string{
		"hooks.internal":    {"10.1.2.3", "fd12::1"},
		"rebind.internal":   {"169.254.169.254"},
		"mixed.internal":    {"10.1.2.3", "169.254.169.254"},
		"cidr.example":      {"172.16.0.5"},
		"public.example":    {"93.184.216.34"},
		"localhost":         {"127.0.0.1"},
		"elsewhere.example": {"192.168.0.1"},
	})
	allowed := mustAllowedHosts(t, "hooks.internal", "rebind.internal", "mixed.internal", "172.16.0.0/12", "127.0.0.1", "localhost:8080")

	tests := []struct {
		name    string
		allowed AllowedHosts
		url     string
		wantErr error
	}{
		{"empty list refuses loopback IP", nil, "http://127.0.0.1:8080/hook", ErrPrivateIP},
		{"empty list refuses listed hostname", nil, "http://hooks.internal/hook", ErrPrivateIP},
		{"empty list allows public", nil, "http://public.example/hook", nil},
		{"IP entry allows literal", allowed, "http://127.0.0.1:1234/hook", nil},
		{"hostname entry allows private answers", allowed, "https://hooks.internal/hook", nil},
		{"CIDR entry allows a name resolving inside it", allowed, "http://cidr.example/hook", nil},
		{"unlisted name resolving to private is refused", allowed, "http://elsewhere.example/hook", ErrPrivateIP},
		{"listed name resolving to metadata is refused", allowed, "http://rebind.internal/hook", ErrPrivateIP},
		{"listed name with one metadata answer is refused", allowed, "http://mixed.internal/hook", ErrPrivateIP},
		{"metadata literal is refused", allowed, "http://169.254.169.254/latest/meta-data/", ErrPrivateIP},
		{"localhost name refused on unlisted port", allowed, "http://localhost:9090/hook", ErrPrivateIP},
		{"localhost name allowed when listed", allowed, "http://localhost:8080/hook", nil},
		{"scheme still checked", allowed, "ftp://127.0.0.1/hook", ErrInvalidScheme},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateURL(tt.url, tt.allowed, lookup)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("validateURL(%q) = %v, want nil", tt.url, err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("validateURL(%q) = %v, want %v", tt.url, err, tt.wantErr)
			}
		})
	}

	// The exported wrapper with an empty list behaves as ValidateURL.
	if err := ValidateURLAllowing("http://127.0.0.1/hook", nil); !errors.Is(err, ErrPrivateIP) {
		t.Errorf("ValidateURLAllowing(loopback, nil) = %v, want ErrPrivateIP", err)
	}
	if err := ValidateURLAllowing("http://127.0.0.1/hook", mustAllowedHosts(t, "127.0.0.0/8")); err != nil {
		t.Errorf("ValidateURLAllowing(loopback, 127.0.0.0/8) = %v, want nil", err)
	}
}

// TestSecureClientAllowing sends real requests to a loopback listener, which
// only a client whose allowlist covers it may reach.
func TestSecureClientAllowing(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/target", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port := u.Port()

	fakeResolver(t, map[string][]string{
		"hooks.internal":  {"127.0.0.1"},
		"rebind.internal": {"169.254.169.254"},
	})

	get := func(c *http.Client, rawURL string) (*http.Response, error) {
		t.Helper()
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, rawURL, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := c.Do(req)
		if res != nil {
			res.Body.Close()
		}
		return res, err
	}

	tests := []struct {
		name    string
		allowed AllowedHosts
		url     string
		wantErr bool
	}{
		{"no allowlist refuses loopback", nil, srv.URL + "/hook", true},
		{"CIDR entry reaches loopback", mustAllowedHosts(t, "127.0.0.0/8"), srv.URL + "/hook", false},
		{"IP entry with the right port", mustAllowedHosts(t, "127.0.0.1:"+port), srv.URL + "/hook", false},
		{"IP entry with another port", mustAllowedHosts(t, "127.0.0.1:1"), srv.URL + "/hook", true},
		{"hostname entry resolving to loopback", mustAllowedHosts(t, "hooks.internal"), "http://hooks.internal:" + port + "/hook", false},
		{"unlisted hostname resolving to loopback", mustAllowedHosts(t, "other.internal"), "http://hooks.internal:" + port + "/hook", true},
		{"hostname entry resolving to metadata", mustAllowedHosts(t, "rebind.internal"), "http://rebind.internal:" + port + "/hook", true},
		{"metadata literal with a broad entry", mustAllowedHosts(t, "0.0.0.0/0"), "http://169.254.169.254:" + port + "/", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hits.Store(0)
			_, err := get(NewSecureClientAllowing(tt.allowed), tt.url)
			if tt.wantErr {
				if !errors.Is(err, ErrPrivateIP) {
					t.Fatalf("GET %s: err = %v, want ErrPrivateIP", tt.url, err)
				}
				if hits.Load() != 0 {
					t.Fatalf("GET %s reached the listener", tt.url)
				}
				return
			}
			if err != nil {
				t.Fatalf("GET %s: %v", tt.url, err)
			}
			if n := hits.Load(); n != 1 {
				t.Fatalf("GET %s: listener hit %d times, want 1", tt.url, n)
			}
		})
	}

	// Redirects are still not followed, so the allowlist only ever applies
	// to the host the request names.
	hits.Store(0)
	res, err := get(NewSecureClientAllowing(mustAllowedHosts(t, "127.0.0.1")), srv.URL+"/redirect")
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusFound || hits.Load() != 1 {
		t.Fatalf("redirect: status %d after %d hits, want 302 after 1", res.StatusCode, hits.Load())
	}

	// The default client is unaffected.
	if _, err := get(NewSecureClient(), srv.URL+"/hook"); !errors.Is(err, ErrPrivateIP) {
		t.Fatalf("NewSecureClient reached loopback: %v", err)
	}
}
