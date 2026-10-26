// Package smtpmail builds outbound outreach messages: header sanitising,
// recipient validation, template rendering, unsubscribe footer and MIME
// composition. Everything here is pure (no I/O) and matches the Python
// reference (services/outreach/sender.py and sending.py), including its quirks.
package smtpmail

import (
	"regexp"
	"strings"
	"unicode"
)

// isPySpace reports whether r is whitespace in the Python sense (str.isspace /
// re \s for str patterns): Unicode White_Space plus the C0 separators
// U+001C..U+001F.
func isPySpace(r rune) bool {
	return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f)
}

// pyStrip is Python str.strip().
func pyStrip(s string) string { return strings.TrimFunc(s, isPySpace) }

// pySpaceClass is the body of a regexp character class matching isPySpace.
const pySpaceClass = `\t-\r\x{1c}-\x{1f} \x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}`

// SanitizeHeader mirrors sanitize_header_value: it drops C0 controls and DEL
// (so CR, LF and TAB vanish rather than becoming spaces), trims Python
// whitespace and cuts to maxLen code points.
func SanitizeHeader(v string, maxLen int) string {
	var b strings.Builder
	for _, r := range v {
		if r == ' ' || (r >= 32 && r != 127) {
			b.WriteRune(r)
		}
	}
	out := pyStrip(strings.NewReplacer("\r", " ", "\n", " ").Replace(b.String()))
	if maxLen < 0 {
		maxLen = 0
	}
	if r := []rune(out); len(r) > maxLen {
		return string(r[:maxLen])
	}
	return out
}

var emailRe = regexp.MustCompile(`^[^` + pySpaceClass + `@,;<>]+@[^` + pySpaceClass + `@,;<>]+\.[^` + pySpaceClass + `@,;<>]+$`)

// ValidRecipient mirrors validate_recipient: one address, no control or
// separator characters, a dot in the domain part.
func ValidRecipient(addr string) bool {
	if addr == "" || strings.ContainsAny(addr, "\r\n") {
		return false
	}
	return emailRe.MatchString(pyStrip(addr))
}
