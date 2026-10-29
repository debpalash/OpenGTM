package domainintel

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// maxCleanInput bounds the text Clean will look at; a domain or URL longer than this cannot yield a valid host
// (253 bytes) and a megabyte of dots would only burn CPU in the parser and the IDNA tables.
const maxCleanInput = 8192

var hostnameRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$`)

// validDomain reports whether d is already a clean domain: lowercase LDH labels of at most 63 bytes, at least two
// labels, 253 bytes overall (the _HOSTNAME check of clean_domain, applied to input that must not be transformed).
func validDomain(d string) bool { return len(d) >= 1 && len(d) <= 253 && hostnameRE.MatchString(d) }

// Clean is clean_domain: the bare lowercase (IDNA/punycode) hostname of a domain or URL, "" when it is not one.
// The result goes into DoH, RDAP and crt.sh requests, so it carries no scheme, path, query, port, credentials,
// case, trailing dot or leading "www." (only that prefix, never one elsewhere in the name).
func Clean(raw string) string {
	if len(raw) > maxCleanInput || !utf8.ValidString(raw) {
		return ""
	}
	text := pyStrip(raw)
	if text == "" {
		return ""
	}
	url := text
	if !strings.Contains(text, "://") && !strings.HasPrefix(text, "//") {
		url = "//" + text
	}
	host, ok := urlHostname(url)
	if !ok {
		return ""
	}
	host = strings.Trim(host, ".")
	host = pyLower(host)
	host = strings.TrimPrefix(host, "www.")
	enc, err := idnaEncode(host)
	if err != nil {
		return ""
	}
	if !validDomain(enc) {
		return ""
	}
	return enc
}

const schemeChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789+-."

var vfutureRE = regexp.MustCompile(`^v[a-fA-F0-9]+\..+$`)

// urlHostname is urllib.parse.urlparse(url).hostname (Python 3.13), with ok=false where urlparse raises
// ValueError and "" when there is no host.
func urlHostname(u string) (string, bool) {
	u = strings.TrimLeftFunc(u, func(r rune) bool { return r <= 0x20 })
	u = strings.NewReplacer("\t", "", "\r", "", "\n", "").Replace(u)
	if i := strings.IndexByte(u, ':'); i > 0 && isASCIIAlpha(rune(u[0])) {
		if strings.Trim(u[:i], schemeChars) == "" {
			u = u[i+1:]
		}
	}
	if !strings.HasPrefix(u, "//") {
		return "", true
	}
	end := len(u)
	for _, c := range "/?#" {
		if w := strings.IndexRune(u[2:], c); w >= 0 && w+2 < end {
			end = w + 2
		}
	}
	netloc := u[2:end]
	hasOpen, hasClose := strings.Contains(netloc, "["), strings.Contains(netloc, "]")
	if hasOpen != hasClose {
		return "", false
	}
	if hasOpen && hasClose {
		// _check_bracketed_netloc: nothing before the bracket, only ":port" after it, and the bracketed host an
		// IPvFuture literal (IPv6 literals pass Python's check but can never become a valid hostname below, so
		// they and every other content give "").
		info := netloc[strings.LastIndexByte(netloc, '@')+1:]
		before, rest, found := strings.Cut(info, "[")
		var host string
		if found {
			if before != "" {
				return "", false
			}
			var port string
			host, port, _ = strings.Cut(rest, "]")
			if port != "" && !strings.HasPrefix(port, ":") {
				return "", false
			}
		} else {
			host, _, _ = strings.Cut(info, ":")
		}
		if !strings.HasPrefix(host, "v") {
			return "", true
		}
		if !vfutureRE.MatchString(host) {
			return "", false
		}
	}
	if !isASCII(netloc) {
		n := strings.NewReplacer("@", "", ":", "", "#", "", "?", "").Replace(netloc)
		if n2 := norm.NFKC.String(n); n2 != n && strings.ContainsAny(n2, "/?#@:") {
			return "", false
		}
	}
	hostinfo := netloc[strings.LastIndexByte(netloc, '@')+1:]
	var hostname string
	if _, br, ok := strings.Cut(hostinfo, "["); ok {
		hostname, _, _ = strings.Cut(br, "]")
	} else {
		hostname, _, _ = strings.Cut(hostinfo, ":")
	}
	if hostname == "" {
		return "", true
	}
	h, zone, found := strings.Cut(hostname, "%")
	h = pyLower(h)
	if found {
		return h + "%" + zone, true
	}
	return h, true
}
