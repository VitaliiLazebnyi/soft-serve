package ssrf

import (
	"net"
	"net/netip"
	"slices"
)

// The rules below decide which private or internal destinations an operator's
// AllowedHosts lets an outbound HTTP request reach. Public addresses never
// need an entry. The trust model:
//
//   - An IP or CIDR entry allows the addresses it covers, whatever hostname
//     the request names. The check is on the address actually dialed, so DNS
//     cannot move a request past it.
//   - A hostname entry allows that name to resolve to private network
//     addresses (privateNetworks): loopback, RFC1918, CGNAT and IPv6 unique
//     local. The operator trusts the name, and with it whoever controls its
//     DNS: the name may resolve to a different private address at send time
//     than when the URL was saved. It never allows anything outside
//     privateNetworks.
//   - Link-local (including the 169.254.169.254 cloud metadata service), the
//     other cloud metadata addresses in metadataAddrs, and the rest of the
//     blocked ranges (multicast, reserved, IPv6 transition ranges that can
//     embed any IPv4 address, and so on) are reachable only through an IP or
//     CIDR entry that lies inside the blocked range it targets. A broad entry
//     such as 0.0.0.0/0 does not open them; 169.254.169.254 or
//     169.254.0.0/16 does.
//
// An entry with a port applies only to requests to that port; the port of a
// URL without one is 80 for http and 443 for https.

// privateNetworks are the blocked ranges a hostname entry may reach, and that
// any covering IP or CIDR entry may reach.
var privateNetworks = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("fc00::/7"),
}

// metadataAddrs are cloud metadata services that sit inside privateNetworks.
// They are reachable only through an entry naming the address itself.
// 169.254.169.254 needs no entry here: link-local is outside privateNetworks.
var metadataAddrs = []netip.Prefix{
	netip.MustParsePrefix("fd00:ec2::254/128"),  // AWS IMDS over IPv6
	netip.MustParsePrefix("100.100.100.200/32"), // Alibaba Cloud metadata, inside CGNAT
}

// permitsHTTP reports whether an HTTP request to host (as named in the URL)
// and port may connect to ip. Public addresses are always permitted; see the
// rules above for the rest. An empty AllowedHosts permits public addresses
// only.
func (a AllowedHosts) permitsHTTP(host, port string, ip net.IP) bool {
	if !isPrivateOrInternal(ip) {
		return true
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap()

	// required is the smallest range an IP or CIDR entry must stay inside to
	// open addr. For addresses in privateNetworks any covering entry does.
	required := 0
	if p, ok := mostSpecific(metadataAddrs, addr); ok {
		required = p.Bits()
	} else if !containsAny(privateNetworks, addr) {
		p, _ := mostSpecific(blockedPrefixes, addr)
		required = p.Bits()
	}
	for _, h := range a {
		if h.prefix.IsValid() && h.portMatches(port) && h.prefix.Contains(addr) && h.prefix.Bits() >= required {
			return true
		}
	}

	return required == 0 && a.namesHost(host, port)
}

// namesHost reports whether a hostname entry names host on port.
func (a AllowedHosts) namesHost(host, port string) bool {
	if net.ParseIP(host) != nil {
		return false
	}
	name := canonicalHostname(host)
	return slices.ContainsFunc(a, func(h allowedHost) bool {
		return h.name != "" && h.name == name && h.portMatches(port)
	})
}

// portMatches reports whether the entry applies to port. An entry without a
// port applies to every port.
func (h allowedHost) portMatches(port string) bool {
	return h.port == "" || h.port == port
}

// mostSpecific returns the longest prefix in prefixes that contains addr.
func mostSpecific(prefixes []netip.Prefix, addr netip.Addr) (netip.Prefix, bool) {
	var best netip.Prefix
	for _, p := range prefixes {
		if p.Contains(addr) && (!best.IsValid() || p.Bits() > best.Bits()) {
			best = p
		}
	}
	return best, best.IsValid()
}

func containsAny(prefixes []netip.Prefix, addr netip.Addr) bool {
	return slices.ContainsFunc(prefixes, func(p netip.Prefix) bool { return p.Contains(addr) })
}

// defaultHTTPPort is the port a URL with scheme and no port connects to.
func defaultHTTPPort(scheme string) string {
	if scheme == "https" {
		return "443"
	}
	return "80"
}
