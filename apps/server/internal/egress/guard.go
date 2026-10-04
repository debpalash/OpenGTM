package egress

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// BlockedError reports a destination refused by the SSRF guard.
type BlockedError struct{ Reason string }

func (e *BlockedError) Error() string { return "blocked_url: " + e.Reason }

func blocked(format string, args ...any) error {
	return &BlockedError{Reason: fmt.Sprintf(format, args...)}
}

// Hostnames refused regardless of resolution (url_guard._BLOCKED_HOSTNAMES),
// plus the RFC 6761 .localhost suffix.
var blockedHostnames = map[string]bool{
	"localhost": true, "ip6-localhost": true, "ip6-loopback": true,
	"metadata.google.internal": true, "metadata": true,
}

// Ranges refused for any resolved or literal address. This is the union of
// Python's ipaddress is_private / is_loopback / is_link_local / is_reserved /
// is_multicast / is_unspecified used by url_guard.py, plus CGNAT
// (100.64.0.0/10) and IPv6 site-local (fec0::/10), which Python does not flag.
var blockedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		// IPv4
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
		"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16",
		"198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
		"255.255.255.255/32",
		// IPv6
		"::/8", "100::/8", "200::/7", "400::/6", "800::/5", "1000::/4", "4000::/3", "6000::/3",
		"8000::/3", "a000::/3", "c000::/3", "e000::/4", "f000::/5", "f800::/6", "fc00::/7",
		"fe00::/9", "fe80::/10", "fec0::/10", "ff00::/8",
		"2001::/23", "2001:db8::/32", "2001:10::/28", "64:ff9b:1::/48",
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// metadataIPs are cloud metadata endpoints (AWS/GCP/Azure, AWS IPv6).
var metadataIPs = map[netip.Addr]bool{
	netip.MustParseAddr("169.254.169.254"): true,
	netip.MustParseAddr("fd00:ec2::254"):   true,
}

var (
	nat64Prefix = netip.MustParsePrefix("64:ff9b::/96")
	sixToFour   = netip.MustParsePrefix("2002::/16")
)

// embeddedIPv4 extracts IPv4 addresses tunnelled inside IPv6 forms that
// route to them: IPv4-mapped, NAT64 well-known prefix and 6to4.
func embeddedIPv4(a netip.Addr) (netip.Addr, bool) {
	if a.Is4In6() {
		return a.Unmap(), true
	}
	if !a.Is6() {
		return netip.Addr{}, false
	}
	b := a.As16()
	if nat64Prefix.Contains(a) {
		return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}), true
	}
	if sixToFour.Contains(a) {
		return netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}), true
	}
	return netip.Addr{}, false
}

// IsBlockedIP reports whether an address must never be fetched.
func IsBlockedIP(a netip.Addr) bool {
	if !a.IsValid() {
		return true
	}
	a = a.WithZone("")
	if v4, ok := embeddedIPv4(a); ok {
		if a.Is4In6() {
			a = v4 // ::ffff:a.b.c.d is a.b.c.d
		} else if IsBlockedIP(v4) {
			return true
		}
	}
	if metadataIPs[a] {
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

var (
	numericToken = regexp.MustCompile(`^(0[xX][0-9a-fA-F]*|[0-9]+)$`)
)

// parseNumber parses one inet_aton style component (hex 0x.., octal 0.., or
// decimal). ok=false when the token is not numeric at all.
func parseNumber(tok string) (uint64, bool) {
	if !numericToken.MatchString(tok) {
		return 0, false
	}
	t := strings.ToLower(tok)
	var (
		n   uint64
		err error
	)
	switch {
	case strings.HasPrefix(t, "0x"):
		if len(t) == 2 {
			return 0, true
		}
		n, err = strconv.ParseUint(t[2:], 16, 64)
	case len(t) > 1 && t[0] == '0':
		n, err = strconv.ParseUint(t[1:], 8, 64)
		if err != nil {
			// "08" is not octal; inet_aton rejects it, Python falls through.
			return 0, false
		}
	default:
		n, err = strconv.ParseUint(t, 10, 64)
	}
	if err != nil {
		return 1 << 63, true // overflow: still an IP-looking token, treat as huge
	}
	return n, true
}

// ParseHostIP parses a host that is an IP in any encoding: standard IPv4/IPv6
// literals, a single integer (2130706433, 0x7f000001, 017700000001), or 1-4
// dotted components in decimal/octal/hex (0177.0.0.1, 127.1, 0x7f.1). It
// mirrors url_guard._parse_ip_any_encoding and extends it to inet_aton's
// short forms, which some resolvers accept.
func ParseHostIP(host string) (netip.Addr, bool) {
	h := strings.Trim(strings.TrimSpace(host), "[]")
	if a, err := netip.ParseAddr(h); err == nil {
		return a, true
	}
	parts := strings.Split(h, ".")
	if len(parts) == 0 || len(parts) > 4 {
		return netip.Addr{}, false
	}
	nums := make([]uint64, len(parts))
	for i, p := range parts {
		n, ok := parseNumber(p)
		if !ok {
			return netip.Addr{}, false
		}
		nums[i] = n
	}
	var v uint32
	switch len(nums) {
	case 1:
		v = uint32(nums[0] & 0xFFFFFFFF)
	case 2:
		v = uint32(nums[0]&0xFF)<<24 | uint32(nums[1]&0xFFFFFF)
	case 3:
		v = uint32(nums[0]&0xFF)<<24 | uint32(nums[1]&0xFF)<<16 | uint32(nums[2]&0xFFFF)
	case 4:
		v = uint32(nums[0]&0xFF)<<24 | uint32(nums[1]&0xFF)<<16 | uint32(nums[2]&0xFF)<<8 | uint32(nums[3]&0xFF)
	}
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}), true
}

// Guard validates destinations. The zero value blocks private targets.
type Guard struct {
	// AllowPrivateForTesting disables address and hostname blocking so tests
	// can reach httptest servers on 127.0.0.1. Never set it from config.
	AllowPrivateForTesting bool
	// Resolver resolves hostnames (default net.DefaultResolver).
	Resolver interface {
		LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
	}
}

// CheckURL mirrors url_guard.check_url(url, allow_http=True) without DNS:
// scheme, host presence, blocked names and IP literals in any encoding.
func (g *Guard) CheckURL(u *url.URL) error {
	if u == nil {
		return blocked("empty url")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && scheme != "http" {
		return blocked("scheme '%s' not allowed", scheme)
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return blocked("no host")
	}
	if g.AllowPrivateForTesting {
		return nil
	}
	if blockedHostnames[host] || strings.HasSuffix(host, ".localhost") {
		return blocked("host '%s' is blocked", host)
	}
	if ip, ok := ParseHostIP(host); ok && IsBlockedIP(ip) {
		return blocked("ip '%s' is private/blocked", ip)
	}
	return nil
}

// CheckString parses then checks a URL string.
func (g *Guard) CheckString(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return blocked("empty url")
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return blocked("unparseable url")
	}
	return g.CheckURL(u)
}

func (g *Guard) resolver() interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
} {
	if g.Resolver != nil {
		return g.Resolver
	}
	return net.DefaultResolver
}

// Resolve returns the validated addresses for host. Every address must pass
// (one private answer blocks the host, like Python's resolve=True), and the
// caller dials only these addresses, so DNS rebinding cannot swap targets.
func (g *Guard) Resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	host = strings.TrimSuffix(strings.ToLower(strings.Trim(host, "[]")), ".")
	if host == "" {
		return nil, blocked("no host")
	}
	if !g.AllowPrivateForTesting && (blockedHostnames[host] || strings.HasSuffix(host, ".localhost")) {
		return nil, blocked("host '%s' is blocked", host)
	}
	if ip, ok := ParseHostIP(host); ok {
		if !g.AllowPrivateForTesting && IsBlockedIP(ip) {
			return nil, blocked("ip '%s' is private/blocked", ip)
		}
		return []netip.Addr{ip.Unmap()}, nil
	}
	addrs, err := g.resolver().LookupIPAddr(ctx, host)
	if err != nil {
		return nil, blocked("cannot resolve host: %v", err)
	}
	if len(addrs) == 0 {
		return nil, blocked("cannot resolve host: no addresses")
	}
	out := make([]netip.Addr, 0, len(addrs))
	for _, a := range addrs {
		ip, ok := netip.AddrFromSlice(a.IP)
		if !ok {
			continue
		}
		ip = ip.Unmap()
		if !g.AllowPrivateForTesting && IsBlockedIP(ip) {
			return nil, blocked("host resolves to private ip %s", ip)
		}
		out = append(out, ip)
	}
	if len(out) == 0 {
		return nil, blocked("cannot resolve host: no usable addresses")
	}
	return out, nil
}
