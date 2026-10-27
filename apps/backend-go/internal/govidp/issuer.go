// Package govidp holds the workspace OIDC configuration and the SCIM bearer
// token lifecycle (governance /sso and /scim-token). Secrets (OIDC client
// secret, SCIM plaintext token) never appear in logs, errors or responses.
package govidp

import (
	"os"
	"strings"
)

// ValueError is Python's ValueError: a user-facing validation message that the
// route layer answers as 422 {"detail": msg}.
type ValueError struct{ Msg string }

func (e *ValueError) Error() string { return e.Msg }

const (
	msgIssuerClean    = "OIDC issuer must be a clean HTTPS origin or path"
	msgIssuerHost     = "OIDC issuer host is not approved by SSO_ALLOWED_ISSUER_HOSTS"
	msgIssuerNotExact = "OIDC issuer is not approved by SSO_ALLOWED_ISSUERS"
)

// pyWhitespace is Python str.isspace (str.strip with no argument).
func pyWhitespace(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\v', '\f', '\r', 0x1c, 0x1d, 0x1e, 0x1f, 0x85, 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

func pyStrip(s string) string { return strings.TrimFunc(s, pyWhitespace) }

// allowedIssuerHosts mirrors oidc.allowed_issuer_hosts (read per call like
// Python's settings attribute).
func allowedIssuerHosts() map[string]bool {
	out := map[string]bool{}
	for _, h := range strings.Split(os.Getenv("SSO_ALLOWED_ISSUER_HOSTS"), ",") {
		if h = asciiLower(pyStrip(h)); h != "" {
			out[h] = true
		}
	}
	return out
}

// allowedIssuers is SSO_ALLOWED_ISSUERS (comma list of full issuer URLs) as
// canonical keys; entries that are not clean HTTPS issuers are ignored. raw
// reports whether the variable has any non-blank entry.
func allowedIssuers() (keys map[string]bool, raw bool) {
	keys = map[string]bool{}
	for _, e := range strings.Split(os.Getenv("SSO_ALLOWED_ISSUERS"), ",") {
		if pyStrip(e) == "" {
			continue
		}
		raw = true
		if p, err := parseIssuer(e); err == nil {
			keys[p.key] = true
		}
	}
	return keys, raw
}

// ValidateIssuer is oidc.validate_issuer, hardened (S-M5): the issuer must
// equal an SSO_ALLOWED_ISSUERS entry exactly (scheme, host, port and path;
// scheme and host compare case-insensitively, a trailing slash is ignored).
// SSO_ALLOWED_ISSUER_HOSTS is deprecated: when set, the host must also be in it,
// but it never approves an issuer on its own. With neither set nothing is
// approved (Python's host message).
func ValidateIssuer(issuer string) (string, error) {
	p, err := parseIssuer(issuer)
	if err != nil {
		return "", err
	}
	hosts := allowedIssuerHosts()
	exact, raw := allowedIssuers()
	if (len(hosts) > 0 && !hosts[p.host]) || (len(hosts) == 0 && !raw) {
		return "", &ValueError{msgIssuerHost}
	}
	if !exact[p.key] {
		return "", &ValueError{msgIssuerNotExact}
	}
	return p.normalized, nil
}

type parsedIssuer struct {
	normalized string // stripped, trailing slashes removed, case kept
	host       string // ASCII-lowered hostname
	key        string // "https://" + lowered netloc + path: the exact-match key
}

// parseIssuer is the clean-origin half of oidc.validate_issuer. It reproduces
// urllib.parse.urlparse for the pieces the check reads (scheme, userinfo,
// hostname, query, fragment) instead of net/url so host extraction cannot
// differ from Python's. Hardening beyond Python (documented): control
// characters, a non-numeric port and an unbalanced IPv6 bracket are rejected as
// "clean HTTPS origin" errors.
func parseIssuer(issuer string) (parsedIssuer, error) {
	bad := &ValueError{msgIssuerClean}
	normalized := strings.TrimRight(pyStrip(issuer), "/")
	for _, r := range normalized {
		if r < 0x20 || r == 0x7f {
			return parsedIssuer{}, bad
		}
	}
	scheme, rest := "", normalized
	if i := strings.IndexByte(normalized, ':'); i > 0 && validScheme(normalized[:i]) {
		scheme, rest = strings.ToLower(normalized[:i]), normalized[i+1:]
	}
	var netloc string
	if strings.HasPrefix(rest, "//") {
		rest = rest[2:]
		end := len(rest)
		for _, c := range "/?#" {
			if i := strings.IndexRune(rest, c); i >= 0 && i < end {
				end = i
			}
		}
		netloc, rest = rest[:end], rest[end:]
	}
	var query, fragment string
	if i := strings.IndexByte(rest, '#'); i >= 0 {
		rest, fragment = rest[:i], rest[i+1:]
	}
	if i := strings.IndexByte(rest, '?'); i >= 0 {
		rest, query = rest[:i], rest[i+1:]
	}
	path := rest
	if strings.Contains(netloc, "[") != strings.Contains(netloc, "]") {
		return parsedIssuer{}, bad
	}
	var userinfo string
	hostinfo := netloc
	hasUser := false
	if i := strings.LastIndexByte(netloc, '@'); i >= 0 {
		userinfo, hostinfo, hasUser = netloc[:i], netloc[i+1:], true
	}
	username, password := "", ""
	if hasUser {
		username, password, _ = strings.Cut(userinfo, ":")
	}
	host, port := hostinfo, ""
	if strings.HasPrefix(hostinfo, "[") {
		end := strings.IndexByte(hostinfo, ']')
		if end < 0 {
			return parsedIssuer{}, bad
		}
		host = hostinfo[1:end]
		if tail := hostinfo[end+1:]; tail != "" {
			if !strings.HasPrefix(tail, ":") {
				return parsedIssuer{}, bad
			}
			port = tail[1:]
		}
	} else {
		host, port, _ = strings.Cut(hostinfo, ":")
	}
	for _, c := range port {
		if c < '0' || c > '9' {
			return parsedIssuer{}, bad
		}
	}
	host = asciiLower(host)
	if scheme != "https" || host == "" || username != "" || password != "" || query != "" || fragment != "" {
		return parsedIssuer{}, bad
	}
	return parsedIssuer{normalized: normalized, host: host, key: "https://" + asciiLower(netloc) + path}, nil
}

func validScheme(s string) bool {
	for i, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case i > 0 && (c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.'):
		default:
			return false
		}
	}
	return s != ""
}

// asciiLower lowers A-Z only. Python's str.lower also maps non-ASCII, but Go's
// ToLower maps U+0130 to "i" where Python yields "i" + U+0307, which would make
// a look-alike host match the allow-list; non-ASCII hosts stay unmatched.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}
