package domainintel

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

func inSpans(tab []span, r rune) bool {
	i := sort.Search(len(tab), func(i int) bool { return tab[i].hi >= r })
	return i < len(tab) && tab[i].lo <= r
}

// pyIsSpace is str.isspace for one character.
func pyIsSpace(r rune) bool { return inSpans(tabSpace, r) }

// pyIsDigit is str.isdigit for one character (decimal digits plus superscripts, circled digits, ...).
func pyIsDigit(r rune) bool { return inSpans(tabDigit, r) }

// pyStrip is str.strip() with no argument.
func pyStrip(s string) string { return strings.TrimFunc(s, pyIsSpace) }

// pyLower is str.lower() for the one character whose full lowercase mapping is two characters.
func pyLower(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, "İ", "i̇"))
}

// pySplitLines is str.splitlines() without keepends.
func pySplitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == '\r':
			out = append(out, s[start:i])
			if i+1 < len(s) && s[i+1] == '\n' {
				n++
			}
			start = i + n
		case r == '\n', r == '\v', r == '\f', r == 0x1c, r == 0x1d, r == 0x1e, r == 0x85, r == 0x2028, r == 0x2029:
			out = append(out, s[start:i])
			start = i + n
		}
		i += n
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

func isASCIIAlpha(r rune) bool { return r < 128 && unicode.IsLetter(r) }
