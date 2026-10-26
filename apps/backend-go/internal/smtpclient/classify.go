package smtpclient

import (
	"strings"
	"unicode"
)

var hardBounceTokens = []string{
	"mailbox unavailable", "user unknown", "no such user",
	"recipient rejected", "does not exist", "550", "551", "553", "554",
}

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
