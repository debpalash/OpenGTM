package collect

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Python str semantics used by the legacy leadgen helpers.

// pyIsSpace is str.isspace for one character (also Python re's Unicode \s).
func pyIsSpace(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\v', '\f', '\r', 0x1c, 0x1d, 0x1e, 0x1f, 0x85, 0xa0,
		0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// pyStrip is str.strip().
func pyStrip(s string) string { return strings.TrimFunc(s, pyIsSpace) }

// pySplit is str.split() (runs of whitespace, no empty fields).
func pySplit(s string) []string { return strings.FieldsFunc(s, pyIsSpace) }

// pyLower is str.lower() (U+0130 lowers to "i" + combining dot, as Python).
func pyLower(s string) string {
	if !strings.ContainsRune(s, 'İ') {
		return strings.ToLower(s)
	}
	return strings.ToLower(strings.ReplaceAll(s, "İ", "i̇"))
}

// isPyWord is Python re's Unicode \w: str.isalnum() or "_".
func isPyWord(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r) }

// isPyAlpha is str.isalpha() for one character.
func isPyAlpha(r rune) bool { return unicode.IsLetter(r) }

// pyIsAlphaStr is str.isalpha(): non-empty and every character a letter.
func pyIsAlphaStr(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !isPyAlpha(r) {
			return false
		}
	}
	return true
}

func isCased(r rune) bool { return unicode.IsUpper(r) || unicode.IsLower(r) || unicode.IsTitle(r) }

// pyTitle is str.title(): a cased character is upper-cased after an uncased one and lower-cased
// after a cased one.
func pyTitle(s string) string {
	var b strings.Builder
	prevCased := false
	for _, r := range s {
		if isCased(r) {
			if prevCased {
				b.WriteString(strings.ToLower(string(r)))
			} else {
				b.WriteRune(unicode.ToTitle(r))
			}
			prevCased = true
			continue
		}
		b.WriteRune(r)
		prevCased = false
	}
	return b.String()
}

// runeLen is len(str) in Python (code points).
func runeLen(s string) int { return utf8.RuneCountInString(s) }

// truncRunes is s[:n] in Python.
func truncRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	i := 0
	for j := range s {
		if i == n {
			return s[:j]
		}
		i++
	}
	return s
}

// ── Python re with Unicode semantics ───────────────────────────────────────

// pyRx is a legacy pattern evaluated like Python's re on str: \w, \d, \s and \b are Unicode-aware
// there and ASCII in RE2. The subject is folded one character for one character before matching
// (non-ASCII word characters to "_", non-ASCII decimal digits to their ASCII digit, Python
// whitespace to " "), which keeps every word boundary where Python sees it; captures are mapped
// back to the original text. Residual difference: case-insensitive matches between an ASCII
// pattern letter and a non-ASCII letter (U+212A KELVIN SIGN ~ k, U+017F LONG S ~ s) do not occur.
//
// Only the classes a pattern uses are folded (a pattern without \w or \b keeps non-ASCII letters,
// so a literal "_" or an ASCII class like [a-z] never sees a placeholder).
type pyRx struct {
	re                  *regexp.Regexp
	word, digit, spaces bool
}

func mustRx(p string) pyRx {
	return pyRx{re: regexp.MustCompile(p), word: strings.Contains(p, `\w`) || strings.Contains(p, `\b`) ||
		strings.Contains(p, `\W`) || strings.Contains(p, `\B`), digit: strings.Contains(p, `\d`) || strings.Contains(p, `\D`),
		spaces: strings.Contains(p, `\s`) || strings.Contains(p, `\S`)}
}

func (p pyRx) fold(s string) folded { return foldWith(s, p.word, p.digit, p.spaces) }

// folded is the subject after folding plus, per folded byte, the original byte offset.
type folded struct {
	t    string
	orig []int
	src  string
}

func fold(s string) folded { return foldWith(s, true, true, true) }

func foldWith(s string, word, digit, spaces bool) folded {
	ascii := true
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 0x80 || c == '\v' || (c >= 0x1c && c <= 0x1f) {
			ascii = false
			break
		}
	}
	if ascii {
		return folded{t: s, src: s}
	}
	var b strings.Builder
	orig := make([]int, 0, len(s)+1)
	for i, r := range s {
		switch {
		case r < 0x80 && (!spaces || r != '\v' && (r < 0x1c || r > 0x1f)):
			b.WriteRune(r)
			orig = append(orig, i)
		case digit && unicode.Is(unicode.Nd, r):
			b.WriteByte(byte('0' + digitValue(r)))
			orig = append(orig, i)
		case spaces && pyIsSpace(r):
			b.WriteByte(' ')
			orig = append(orig, i)
		case word && isPyWord(r):
			b.WriteByte('_')
			orig = append(orig, i)
		default:
			n := utf8.RuneLen(r)
			if r == utf8.RuneError {
				n = 1
			}
			b.WriteString(s[i : i+n])
			for k := 0; k < n; k++ {
				orig = append(orig, i+k)
			}
		}
	}
	orig = append(orig, len(s))
	return folded{t: b.String(), orig: orig, src: s}
}

func (f folded) at(i int) int {
	if f.orig == nil {
		return i
	}
	return f.orig[i]
}

func (f folded) slice(a, b int) string {
	if a < 0 {
		return ""
	}
	return f.src[f.at(a):f.at(b)]
}

// digitValue is int() of a Unicode decimal digit: digits come in runs of ten starting at zero.
func digitValue(r rune) int {
	start := r
	for start > 0 && unicode.Is(unicode.Nd, start-1) {
		start--
	}
	return int(r-start) % 10
}

// Match is re.search(p, s) is not None.
func (p pyRx) Match(s string) bool { return p.re.MatchString(p.fold(s).t) }

// Find is re.search(p, s): the groups of the first match (nil when none); unmatched groups are "".
func (p pyRx) Find(s string) []string {
	f := p.fold(s)
	loc := p.re.FindStringSubmatchIndex(f.t)
	if loc == nil {
		return nil
	}
	out := make([]string, len(loc)/2)
	for i := range out {
		out[i] = f.slice(loc[2*i], loc[2*i+1])
	}
	return out
}

// FindFolded is Find returning the folded text of each group (decimal digits already ASCII, so
// int() of a captured number is strconv.Atoi of the result).
func (p pyRx) FindFolded(s string) []string {
	f := p.fold(s)
	loc := p.re.FindStringSubmatchIndex(f.t)
	if loc == nil {
		return nil
	}
	out := make([]string, len(loc)/2)
	for i := range out {
		if loc[2*i] >= 0 {
			out[i] = f.t[loc[2*i]:loc[2*i+1]]
		}
	}
	return out
}

// FindAll is re.findall(p, s) for a pattern without groups.
func (p pyRx) FindAll(s string) []string {
	f := p.fold(s)
	var out []string
	for _, loc := range p.re.FindAllStringIndex(f.t, -1) {
		out = append(out, f.slice(loc[0], loc[1]))
	}
	return out
}

// Sub is re.sub(p, repl, s) with a literal replacement (no group references).
func (p pyRx) Sub(s, repl string) string {
	f := p.fold(s)
	locs := p.re.FindAllStringIndex(f.t, -1)
	if locs == nil {
		return s
	}
	var b strings.Builder
	last := 0
	for _, loc := range locs {
		b.WriteString(f.src[f.at(last):f.at(loc[0])])
		b.WriteString(repl)
		last = loc[1]
	}
	b.WriteString(f.src[f.at(last):])
	return b.String()
}
