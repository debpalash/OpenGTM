// Package egress is the SSRF guard and pooled HTTP client for all outbound
// requests. CheckURL is a port of legacy/python/apps/api/core/url_guard.py
// (check_url with resolve=False); the Python and Go verdicts are pinned to the
// same vector file, testdata/url_guard_vectors.json.
package egress

import (
	"errors"
	"fmt"
	"math/big"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
)

// ErrBlocked is wrapped by every rejection so callers can errors.Is it.
var ErrBlocked = errors.New("egress: blocked")

// BlockedError is the typed rejection. Reason is byte-identical to the
// message of Python's BlockedUrlError (callers surface it verbatim, e.g.
// "blocked_url: <Reason[:60]>"); Error() adds a prefix for logs only.
type BlockedError struct{ Reason string }

func (e *BlockedError) Error() string        { return ErrBlocked.Error() + ": " + e.Reason }
func (e *BlockedError) Is(target error) bool { return target == ErrBlocked }

func blocked(format string, a ...any) error {
	return &BlockedError{Reason: fmt.Sprintf(format, a...)}
}

var blockedHostnames = map[string]struct{}{
	"localhost": {}, "ip6-localhost": {}, "ip6-loopback": {},
	"metadata.google.internal": {}, "metadata": {},
}

var metadataIPs = map[netip.Addr]struct{}{
	netip.MustParseAddr("169.254.169.254"): {},
	netip.MustParseAddr("fd00:ec2::254"):   {},
}

func prefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

// Ranges mirror CPython 3.13 ipaddress (is_private, is_reserved, is_multicast,
// is_link_local, is_loopback, is_unspecified), which url_guard relies on.
var (
	v4Private = prefixes("0.0.0.0/8", "10.0.0.0/8", "127.0.0.0/8", "169.254.0.0/16",
		"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24",
		"192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24",
		"240.0.0.0/4", "255.255.255.255/32")
	v4PrivateExceptions = prefixes("192.0.0.9/32", "192.0.0.10/32")
	v4Multicast         = prefixes("224.0.0.0/4")

	v6Private = prefixes("::1/128", "::/128", "::ffff:0:0/96", "64:ff9b:1::/48",
		"100::/64", "2001::/23", "2001:db8::/32", "2002::/16", "3fff::/20",
		"fc00::/7", "fe80::/10")
	v6PrivateExceptions = prefixes("2001:1::1/128", "2001:1::2/128", "2001:3::/32",
		"2001:4:112::/48", "2001:20::/28", "2001:30::/28")
	v6Reserved = prefixes("::/8", "100::/8", "200::/7", "400::/6", "800::/5",
		"1000::/4", "4000::/3", "6000::/3", "8000::/3", "a000::/3", "c000::/3",
		"e000::/4", "f000::/5", "f800::/6", "fe00::/9")
	v6Multicast = prefixes("ff00::/8")
)

func inAny(ps []netip.Prefix, a netip.Addr) bool {
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// IsBlockedIP reports whether ip is not safe to dial (url_guard._ip_is_blocked).
func IsBlockedIP(ip netip.Addr) bool {
	ip = ip.WithZone("")
	if ip.Is4In6() {
		ip = ip.Unmap()
	}
	if _, ok := metadataIPs[ip]; ok {
		return true
	}
	if ip.Is4() {
		return (inAny(v4Private, ip) && !inAny(v4PrivateExceptions, ip)) || inAny(v4Multicast, ip)
	}
	return (inAny(v6Private, ip) && !inAny(v6PrivateExceptions, ip)) ||
		inAny(v6Reserved, ip) || inAny(v6Multicast, ip)
}

var (
	reSingleInt = regexp.MustCompile(`^(?:0[xX][0-9a-fA-F]+|0[0-7]+|[0-9]+)$`)
	reDotPart   = regexp.MustCompile(`^(?:0[xX][0-9a-fA-F]+|[0-9]+)$`)
)

// intAny parses hex (0x..), classic octal (leading 0) or decimal; ok=false on
// a digit invalid for the base (e.g. "08"), where Python raises ValueError.
func intAny(tok string) (*big.Int, bool) {
	t := strings.ToLower(tok)
	switch {
	case strings.HasPrefix(t, "0x"):
		return new(big.Int).SetString(t[2:], 16)
	case strings.HasPrefix(t, "0") && len(t) > 1:
		return new(big.Int).SetString(t[1:], 8)
	}
	return new(big.Int).SetString(t, 10)
}

// parseIPAnyEncoding is url_guard._parse_ip_any_encoding. ok=false means the
// host is a domain name (or an unparseable numeric form Python also lets pass).
func parseIPAnyEncoding(host string) (netip.Addr, bool) {
	h := strings.Trim(strings.TrimSpace(host), "[]")
	if a, err := netip.ParseAddr(h); err == nil {
		return a, true
	}
	if reSingleInt.MatchString(h) {
		n, ok := intAny(h)
		if !ok {
			return netip.Addr{}, false
		}
		v := uint32(new(big.Int).And(n, big.NewInt(0xFFFFFFFF)).Uint64())
		return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}), true
	}
	parts := strings.Split(h, ".")
	if len(parts) == 4 {
		var b [4]byte
		for i, p := range parts {
			if !reDotPart.MatchString(p) {
				return netip.Addr{}, false
			}
			n, ok := intAny(p)
			if !ok {
				return netip.Addr{}, false
			}
			b[i] = byte(new(big.Int).And(n, big.NewInt(0xFF)).Uint64())
		}
		return netip.AddrFrom4(b), true
	}
	return netip.Addr{}, false
}

// CheckURL validates raw for server-side fetching without resolving DNS;
// the dial-time check in NewClient covers resolved addresses.
func CheckURL(raw string, allowHTTP bool) error {
	return checkURL(raw, allowHTTP, nil)
}

func checkURL(raw string, allowHTTP bool, allow map[string]struct{}) error {
	if raw == "" {
		return blocked("empty url")
	}
	s := strings.NewReplacer("\t", "", "\r", "", "\n", "").Replace(strings.TrimSpace(raw))
	u, err := url.Parse(s)
	if err != nil {
		return blocked("unparseable url")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && !(allowHTTP && scheme == "http") {
		return blocked("scheme '%s' not allowed", scheme)
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return blocked("no host")
	}
	if _, ok := allow[host]; ok {
		return nil
	}
	if _, ok := blockedHostnames[host]; ok {
		return blocked("host '%s' is blocked", host)
	}
	if ip, ok := parseIPAnyEncoding(host); ok && IsBlockedIP(ip) {
		return blocked("ip '%s' is private/blocked", ip)
	}
	return nil
}
