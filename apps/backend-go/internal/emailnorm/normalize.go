// Package emailnorm ports services/outreach/normalize.py: the canonical
// email form and the suppression match keys used at enroll, send, suppress,
// webhook and unsubscribe.
package emailnorm

import (
	"errors"
	"strings"

	"golang.org/x/net/idna"
	"golang.org/x/text/unicode/norm"
)

// nameprep approximates IDNA2003 nameprep + ToASCII (Python's "idna" codec):
// UTS46 mapping with transitional processing (ß -> ss, final sigma folded).
var nameprep = idna.New(idna.MapForLookup(), idna.Transitional(true))

var errLabel = errors.New("emailnorm: bad idna label")

// Normalize is normalize_email: NFKC, trim, lower, rpartition on "@", IDNA
// punycode of the domain with fallback to plain lower-case on error.
func Normalize(email string) string {
	if email == "" {
		return ""
	}
	e := strings.ToLower(strings.TrimSpace(norm.NFKC.String(email)))
	at := strings.LastIndex(e, "@")
	if at < 0 {
		return e
	}
	local, domain := e[:at], e[at+1:]
	if local == "" || domain == "" {
		return e
	}
	if ascii, err := encodeIDNA(domain); err == nil {
		domain = strings.ToLower(ascii)
	}
	return local + "@" + domain
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// encodeIDNA mirrors str.encode("idna"): labels split on the four IDNA dots,
// ASCII labels pass through (length 1..63), others are nameprepped and
// punycoded; a trailing dot is preserved, any other empty label is an error.
func encodeIDNA(domain string) (string, error) {
	if domain == "" {
		return "", nil
	}
	r := strings.NewReplacer("。", ".", "．", ".", "｡", ".")
	labels := strings.Split(r.Replace(domain), ".")
	trailing := ""
	if labels[len(labels)-1] == "" {
		labels = labels[:len(labels)-1]
		trailing = "."
	}
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		if l == "" {
			return "", errLabel
		}
		if !isASCII(l) {
			a, err := nameprep.ToASCII(l)
			if err != nil {
				return "", err
			}
			l = a
		}
		if len(l) == 0 || len(l) > 63 {
			return "", errLabel
		}
		out = append(out, l)
	}
	return strings.Join(out, ".") + trailing, nil
}
