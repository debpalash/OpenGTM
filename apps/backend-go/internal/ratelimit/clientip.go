package ratelimit

import (
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// DefaultTrustedProxies: loopback and the ranges a docker bridge network draws from (docker-compose.yml's lead_net has no
// fixed subnet). nginx reaches the API from one of these; any other peer is the client itself.
const DefaultTrustedProxies = "127.0.0.0/8,::1/128,172.16.0.0/12,192.168.0.0/16,fc00::/7"

// ParseTrustedProxies parses TRUST_PROXY_CIDRS (comma-separated CIDRs or bare addresses); unset or blank means the defaults.
// Unparseable entries are skipped with a warning when log is set (never fail open to "trust everything").
func ParseTrustedProxies(raw string, log *slog.Logger) []netip.Prefix {
	if strings.TrimSpace(raw) == "" {
		raw = DefaultTrustedProxies
	}
	var out []netip.Prefix
	for _, f := range strings.Split(raw, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if p, err := netip.ParsePrefix(f); err == nil {
			out = append(out, p.Masked())
		} else if a, err := netip.ParseAddr(f); err == nil {
			out = append(out, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
		} else if log != nil {
			log.Warn("ratelimit: ignoring unparseable TRUST_PROXY_CIDRS entry")
		}
	}
	return out
}

// TrustedPeer reports whether peer is inside one of the trusted proxy networks.
func TrustedPeer(nets []netip.Prefix, peer netip.Addr) bool {
	for _, n := range nets {
		if n.Contains(peer) {
			return true
		}
	}
	return false
}

// ClientIP is the rate-limit identity of an unauthenticated caller. Behind nginx every request arrives from the proxy, and
// nginx overwrites X-Real-IP with $remote_addr (infra/nginx/default.conf), so that header is the client when the socket peer
// is a trusted proxy. X-Forwarded-For is never used (nginx appends to a client-supplied value), and an untrusted peer is
// always the client itself, so a header cannot be used to dodge the limit. IPv6 clients are keyed by their /64.
func ClientIP(r *http.Request, trusted []netip.Prefix) string {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = h
	}
	peer, perr := netip.ParseAddr(host)
	if perr != nil {
		if host == "" {
			return "?"
		}
		return host
	}
	peer = peer.Unmap().WithZone("")
	client := peer
	if TrustedPeer(trusted, peer) {
		if a, err := netip.ParseAddr(strings.TrimSpace(r.Header.Get("X-Real-IP"))); err == nil {
			client = a.Unmap().WithZone("")
		}
	}
	if client.Is6() {
		return netip.PrefixFrom(client, 64).Masked().String()
	}
	return client.String()
}
