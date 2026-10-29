package leadweb

import (
	"regexp"
	"strings"
	"unicode"
)

// Python's re on str patterns is Unicode-aware: \w, \d, \s and \b cover all
// scripts, Go's RE2 classes are ASCII-only. These expansions keep the legacy
// regexes' behaviour (Devanagari digits, NBSP and other separators included).
const (
	pyW = `\p{L}\p{N}_`                             // \w (str.isalnum() or "_")
	pyD = `\p{Nd}`                                  // \d
	pyS = "\\t\\n\\x0b\\f\\r\\x1c-\\x1f\\x85\\p{Z}" // \s (str.isspace())
)

// pyRe compiles a legacy pattern after expanding \w, \d and \s to their
// Unicode meaning. \b is not supported (see tenDigitWords).
func pyRe(p string) *regexp.Regexp {
	r := strings.NewReplacer(`\w`, pyW, `\d`, pyD, `\s`, pyS)
	// classes like [\w.\-+] expand in place; bare \w etc. need brackets
	var b strings.Builder
	inClass := false
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c == '\\' && i+1 < len(p) {
			esc := p[i : i+2]
			i++
			switch esc {
			case `\w`, `\d`, `\s`:
				if inClass {
					b.WriteString(r.Replace(esc))
				} else {
					b.WriteString("[" + r.Replace(esc) + "]")
				}
			case `\D`:
				if inClass {
					panic("leadweb: \\D inside a class")
				}
				b.WriteString("[^" + pyD + "]")
			default:
				b.WriteString(esc)
			}
			continue
		}
		switch c {
		case '[':
			inClass = true
		case ']':
			inClass = false
		}
		b.WriteByte(c)
	}
	return regexp.MustCompile(b.String())
}

func isPyWord(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r) }

func isPyDigit(r rune) bool { return unicode.Is(unicode.Nd, r) }

// pyDigits is re.sub(r"\D", "", s).
func pyDigits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if isPyDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func runeLen(s string) int { return len([]rune(s)) }

// tenDigitWords finds the matches of Python's `\b\d{10}\b`: a match must sit
// between word boundaries, so it is exactly a maximal run of word characters
// made of ten decimal digits.
func tenDigitWords(text string) []string {
	var out []string
	rs := []rune(text)
	for i := 0; i < len(rs); {
		if !isPyWord(rs[i]) {
			i++
			continue
		}
		j := i
		digits := true
		for j < len(rs) && isPyWord(rs[j]) {
			if !isPyDigit(rs[j]) {
				digits = false
			}
			j++
		}
		if digits && j-i == 10 {
			out = append(out, string(rs[i:j]))
		}
		i = j
	}
	return out
}

// pyStrip is str.strip() (Python whitespace).
func pyStrip(s string) string { return strings.TrimFunc(s, pyIsSpace) }

func pyIsSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\x0b', '\f', '\r', '\x1c', '\x1d', '\x1e', '\x1f', '\x85':
		return true
	}
	return unicode.In(r, unicode.Zs, unicode.Zl, unicode.Zp)
}

// truncRunes is s[:n] on a Python str.
func truncRunes(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n])
}

// uniq is list(dict.fromkeys(xs)).
func uniq(xs []string) []string {
	seen := make(map[string]struct{}, len(xs))
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if _, ok := seen[x]; !ok {
			seen[x] = struct{}{}
			out = append(out, x)
		}
	}
	return out
}

func head(xs []string, n int) []string {
	if len(xs) > n {
		return xs[:n]
	}
	return xs
}
