package collectintent

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// isPySpace mirrors str.isspace (and the Unicode \s of Python's re module).
func isPySpace(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\v', '\f', '\r', 0x1c, 0x1d, 0x1e, 0x1f, 0x85, 0xa0,
		0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// pyStrip mirrors str.strip().
func pyStrip(s string) string { return strings.TrimFunc(s, isPySpace) }

// collapseWS mirrors re.sub(r"\s+", " ", s).
func collapseWS(s string) string {
	changed := false
	prevSpace := false
	for _, r := range s {
		sp := isPySpace(r)
		if sp && (r != ' ' || prevSpace) {
			changed = true
			break
		}
		prevSpace = sp
	}
	if !changed {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	inSpace := false
	for _, r := range s {
		if isPySpace(r) {
			if !inSpace {
				b.WriteByte(' ')
			}
			inSpace = true
			continue
		}
		inSpace = false
		b.WriteRune(r)
	}
	return b.String()
}

// isPyWord mirrors Python's Unicode \w: alphanumerics and underscore.
func isPyWord(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r)
}

func isCased(r rune) bool {
	return unicode.IsUpper(r) || unicode.IsLower(r) || unicode.IsTitle(r)
}

func isCaseIgnorable(r rune) bool {
	switch r {
	case 0x27, 0x2e, 0x3a, 0xb7, 0x387, 0x55f, 0x5f4, 0x2018, 0x2019, 0x2024, 0x2027,
		0xfe13, 0xfe52, 0xfe55, 0xff07, 0xff0e, 0xff1a, 0x5e, 0x60, 0xa8, 0xad, 0xaf, 0xb4, 0xb8:
		return true
	}
	return unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf, unicode.Lm, unicode.Sk)
}

// pyLower mirrors str.lower(): U+0130 becomes "i" + U+0307 and capital sigma
// becomes final sigma at the end of a word.
func pyLower(s string) string {
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		return strings.ToLower(s)
	}
	rs := []rune(s)
	var b strings.Builder
	b.Grow(len(s) + 4)
	for i, r := range rs {
		switch r {
		case 0x130:
			b.WriteString("i̇")
		case 0x3a3:
			j := i - 1
			for j >= 0 && isCaseIgnorable(rs[j]) {
				j--
			}
			final := j >= 0 && isCased(rs[j])
			if final {
				k := i + 1
				for k < len(rs) && isCaseIgnorable(rs[k]) {
					k++
				}
				if k < len(rs) && isCased(rs[k]) {
					final = false
				}
			}
			if final {
				b.WriteRune(0x3c2)
			} else {
				b.WriteRune(0x3c3)
			}
		default:
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

// pyTitle mirrors str.title().
func pyTitle(s string) string {
	var b strings.Builder
	prevCased := false
	for _, r := range s {
		if prevCased {
			if r == 0x130 {
				b.WriteString("i̇")
			} else {
				b.WriteRune(unicode.ToLower(r))
			}
		} else {
			b.WriteRune(unicode.ToTitle(r))
		}
		prevCased = isCased(r)
	}
	return b.String()
}

func lastRune(s string) rune {
	r, _ := utf8.DecodeLastRuneInString(s)
	return r
}
