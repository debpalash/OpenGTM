package smtpclient

import (
	"strings"
	"unicode"
)

var hardBounceTokens = []string{
	"mailbox unavailable", "user unknown", "no such user",
	"recipient rejected", "does not exist",
}

// hardBounceCodes count only as standalone words: Python matched them as substrings, so a port, address or
// queue id containing "553" (dial tcp 127.0.0.1:35539) read as a permanent failure.
var hardBounceCodes = []string{"550", "551", "553", "554"}

// IsHardBounce is an exact port of sending._is_hard_bounce: a 5xx code token
// (only considered when the text starts with "5" or has " 5" in it) or one of
// the known permanent-failure phrases. Python's \b and \d are Unicode-aware, so
// the scan below is too (Go's regexp \b is ASCII only).
func IsHardBounce(err string) bool {
	e := strings.ToLower(err)
	if strings.Contains(" "+e, " 5") && hasCode5xx([]rune(e)) {
		return true
	}
	for _, t := range hardBounceTokens {
		if strings.Contains(e, t) {
			return true
		}
	}
	rs := []rune(e)
	for _, code := range hardBounceCodes {
		for i := strings.Index(e, code); i >= 0; {
			start := len([]rune(e[:i]))
			end := start + len(code)
			if (start == 0 || !isWord(rs[start-1])) && (end == len(rs) || !isWord(rs[end])) {
				return true
			}
			next := strings.Index(e[i+1:], code)
			if next < 0 {
				break
			}
			i += 1 + next
		}
	}
	return false
}

func isWord(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r) }

// hasCode5xx is re.search(r"\b5\d\d\b", e).
func hasCode5xx(rs []rune) bool {
	for i := 0; i+3 <= len(rs); i++ {
		if rs[i] != '5' || !unicode.IsDigit(rs[i+1]) || !unicode.IsDigit(rs[i+2]) {
			continue
		}
		if i > 0 && isWord(rs[i-1]) {
			continue
		}
		if i+3 < len(rs) && isWord(rs[i+3]) {
			continue
		}
		return true
	}
	return false
}
