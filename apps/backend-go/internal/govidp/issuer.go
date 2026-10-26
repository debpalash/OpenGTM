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
	msgIssuerClean = "OIDC issuer must be a clean HTTPS origin or path"
	msgIssuerHost  = "OIDC issuer host is not approved by SSO_ALLOWED_ISSUER_HOSTS"
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
		if h = strings.ToLower(pyStrip(h)); h != "" {
			out[h] = true
		}
	}
	return out
}

// ValidateIssuer is oidc.validate_issuer. It reproduces urllib.parse.urlparse
// for the pieces the check reads (scheme, userinfo, hostname, query, fragment)
// instead of net/url so host extraction cannot differ from Python's. Hardening
// beyond Python (documented): control characters, a non-numeric port and an
// unbalanced IPv6 bracket are rejected as "clean HTTPS origin" errors.
func ValidateIssuer(issuer string) (string, error) {
	bad := &ValueError{msgIssuerClean}
	normalized := strings.TrimRight(pyStrip(issuer), "/")
	for _, r := range normalized {
		if r < 0x20 || r == 0x7f {
			return "", bad
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
	_ = rest
	if strings.Contains(netloc, "[") != strings.Contains(netloc, "]") {
		return "", bad
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
			return "", bad
		}
		host = hostinfo[1:end]
		if tail := hostinfo[end+1:]; tail != "" {
			if !strings.HasPrefix(tail, ":") {
				return "", bad
			}
			port = tail[1:]
		}
	} else {
		host, port, _ = strings.Cut(hostinfo, ":")
	}
	for _, c := range port {
		if c < '0' || c > '9' {
			return "", bad
		}
	}
	host = strings.ToLower(host)
	if scheme != "https" || host == "" || username != "" || password != "" || query != "" || fragment != "" {
		return "", bad
	}
	if !allowedIssuerHosts()[host] {
		return "", &ValueError{msgIssuerHost}
	}
	return normalized, nil
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
