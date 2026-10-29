package firmo

import (
	"errors"
	"net/netip"
	"regexp"
	"strings"

	"golang.org/x/text/unicode/norm"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// pyURL is the (scheme, netloc) of Python 3.13's urllib.parse.urlsplit.
type pyURL struct{ scheme, netloc string }

var errIPv6 = errors.New("Invalid IPv6 URL")

const c0AndSpace = "\x00\x01\x02\x03\x04\x05\x06\x07\x08\t\n\x0b\x0c\r\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f "

var ipvFutureRE = regexp.MustCompile(`^v[a-fA-F0-9]+\..+$`)

func isSchemeChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.'
}

// pySplitURL mirrors urlsplit: scheme, netloc and the validations that raise
// ValueError.
func pySplitURL(url string) (pyURL, error) {
	url = strings.TrimLeft(url, c0AndSpace)
	url = strings.NewReplacer("\t", "", "\r", "", "\n", "").Replace(url)
	var out pyURL
	if i := strings.IndexByte(url, ':'); i > 0 && (url[0] >= 'a' && url[0] <= 'z' || url[0] >= 'A' && url[0] <= 'Z') {
		ok := true
		for j := 0; j < i; j++ {
			if !isSchemeChar(url[j]) {
				ok = false
				break
			}
		}
		if ok {
			out.scheme, url = strings.ToLower(url[:i]), url[i+1:]
		}
	}
	if strings.HasPrefix(url, "//") {
		end := len(url)
		for _, c := range "/?#" {
			if w := strings.IndexRune(url[2:], c); w >= 0 && w+2 < end {
				end = w + 2
			}
		}
		out.netloc = url[2:end]
		hasOpen, hasClose := strings.Contains(out.netloc, "["), strings.Contains(out.netloc, "]")
		if hasOpen != hasClose {
			return out, errIPv6
		}
		if hasOpen && hasClose {
			if err := checkBracketedNetloc(out.netloc); err != nil {
				return out, err
			}
		}
	}
	if err := checkNetloc(out.netloc); err != nil {
		return out, err
	}
	return out, nil
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func checkNetloc(netloc string) error {
	if netloc == "" || isASCII(netloc) {
		return nil
	}
	n := strings.NewReplacer("@", "", ":", "", "#", "", "?", "").Replace(netloc)
	n2 := norm.NFKC.String(n)
	if n == n2 {
		return nil
	}
	if strings.ContainsAny(n2, "/?#@:") {
		return errors.New("netloc '" + netloc + "' contains invalid characters under NFKC normalization")
	}
	return nil
}

func rpartition(s, sep string) (before, after string, found bool) {
	if i := strings.LastIndex(s, sep); i >= 0 {
		return s[:i], s[i+len(sep):], true
	}
	return "", s, false
}

func partition(s, sep string) (before, after string, found bool) {
	if i := strings.Index(s, sep); i >= 0 {
		return s[:i], s[i+len(sep):], true
	}
	return s, "", false
}

func checkBracketedNetloc(netloc string) error {
	_, hp, _ := rpartition(netloc, "@")
	before, bracketed, open := partition(hp, "[")
	var host string
	if open {
		if before != "" {
			return errIPv6
		}
		var port string
		host, port, _ = partition(bracketed, "]")
		if port != "" && !strings.HasPrefix(port, ":") {
			return errIPv6
		}
	} else {
		host, _, _ = partition(hp, ":")
	}
	if strings.HasPrefix(host, "v") {
		if !ipvFutureRE.MatchString(host) {
			return errors.New("IPvFuture address is invalid")
		}
		return nil
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return errors.New("'" + host + "' does not appear to be an IPv4 or IPv6 address")
	}
	if ip.Is4() {
		return errors.New("An IPv4 address cannot be in brackets")
	}
	return nil
}

// hostname is SplitResult.hostname ("" for None).
func (u pyURL) hostname() string {
	_, hp, _ := rpartition(u.netloc, "@")
	_, bracketed, open := partition(hp, "[")
	var host string
	if open {
		host, _, _ = partition(bracketed, "]")
	} else {
		host, _, _ = partition(hp, ":")
	}
	if host == "" {
		return ""
	}
	h, zone, hasZone := partition(host, "%")
	h = strings.ToLower(h)
	if hasZone {
		return h + "%" + zone
	}
	return h
}

// normalizeWebsiteURL is website_scraper.normalize_website_url.
func normalizeWebsiteURL(url string) string {
	if url == "" {
		return url
	}
	url = providers.PyStrip(url)
	if !strings.HasPrefix(url, "http") {
		url = "https://" + url
	}
	u, err := pySplitURL(url)
	if err != nil {
		return url
	}
	return u.scheme + "://" + u.netloc
}
