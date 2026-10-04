package manifest

import (
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
)

// Pattern is one parsed capabilities.network entry.
//
// Syntax: scheme://host[:port][/path-prefix]
//
//   - scheme is https or http. An https URL is also allowed by an http
//     pattern (an upgrade), never the reverse.
//   - host is an exact lower-case name or IP, "*.example.com" (example.com
//     itself and any subdomain, but never example.com.evil.net), or "*" for
//     any host.
//   - port is optional. Without it only the scheme's default port matches;
//     ":*" matches any port.
//   - path-prefix is optional and matches whole segments: "/v1" allows "/v1"
//     and "/v1/x" but not "/v10". Paths are compared after dot-segment
//     cleaning, and encoded slashes are refused when a prefix is set.
type Pattern struct {
	Raw        string
	Scheme     string
	Host       string // "" when AnyHost
	AnyHost    bool
	Subdomains bool // host was "*.<Host>"
	Port       string
	PathPrefix string
}

// NetworkPatternRE is the syntax accepted for capabilities.network entries.
// The JSON Schema in packages/contracts uses the same expression.
const NetworkPatternRE = `^(https?)://(\*|(\*\.)?[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*|\[[0-9a-f:.]+\])(:([0-9]{1,5}|\*))?(/[^\s?#]*)?$`

var networkRE = regexp.MustCompile(NetworkPatternRE)

// ParsePattern parses one network capability pattern.
func ParsePattern(raw string) (Pattern, error) {
	m := networkRE.FindStringSubmatch(raw)
	if m == nil {
		return Pattern{}, fmt.Errorf("invalid network pattern %q: want scheme://host[:port][/path] with a lower-case host, *.domain or *", raw)
	}
	p := Pattern{Raw: raw, Scheme: m[1], Port: m[8], PathPrefix: m[9]}
	host := m[2]
	switch {
	case host == "*":
		p.AnyHost = true
	case strings.HasPrefix(host, "*."):
		p.Subdomains = true
		p.Host = host[2:]
	default:
		p.Host = strings.Trim(host, "[]")
	}
	return p, nil
}

func defaultPort(scheme string) string {
	if scheme == "http" {
		return "80"
	}
	return "443"
}

// Match reports whether u is covered by the pattern.
func (p Pattern) Match(u *url.URL) bool {
	if u == nil || u.User != nil || u.Opaque != "" {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != p.Scheme && !(p.Scheme == "http" && scheme == "https") {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return false
	}
	switch {
	case p.AnyHost:
	case p.Subdomains:
		if host != p.Host && !strings.HasSuffix(host, "."+p.Host) {
			return false
		}
	default:
		if host != p.Host {
			return false
		}
	}
	port := u.Port()
	switch p.Port {
	case "*":
	case "":
		if port != "" && port != defaultPort(scheme) {
			return false
		}
	default:
		if port != p.Port {
			return false
		}
	}
	prefix := strings.TrimSuffix(p.PathPrefix, "/")
	if prefix == "" {
		return true
	}
	if raw := strings.ToLower(u.EscapedPath()); strings.Contains(raw, "%2f") || strings.Contains(raw, "%5c") {
		return false
	}
	clean := path.Clean("/" + u.Path)
	return clean == prefix || strings.HasPrefix(clean, prefix+"/")
}

// Network is a compiled set of patterns.
type Network []Pattern

// CompileNetwork parses every pattern.
func CompileNetwork(raw []string) (Network, error) {
	out := make(Network, 0, len(raw))
	for _, r := range raw {
		p, err := ParsePattern(r)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// ErrNetworkDenied is returned when a URL is outside the declared capability.
type ErrNetworkDenied struct{ URL string }

func (e *ErrNetworkDenied) Error() string {
	return fmt.Sprintf("network capability denied: %s is not covered by capabilities.network", e.URL)
}

// Allows checks a URL string against the patterns.
func (n Network) Allows(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return &ErrNetworkDenied{URL: raw}
	}
	return n.AllowsURL(u)
}

// AllowsURL checks a parsed URL against the patterns.
func (n Network) AllowsURL(u *url.URL) error {
	for _, p := range n {
		if p.Match(u) {
			return nil
		}
	}
	return &ErrNetworkDenied{URL: redactURL(u)}
}

func redactURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	c := *u
	c.User = nil
	c.RawQuery = ""
	c.Fragment = ""
	return c.String()
}
