package scrape

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// Python's re on str patterns is Unicode-aware: \w, \d and \s cover all
// scripts, Go's RE2 classes are ASCII-only (same expansion as leadweb).
const (
	pyW = `\p{L}\p{N}_`
	pyD = `\p{Nd}`
	pyS = "\\t\\n\\x0b\\f\\r\\x1c-\\x1f\\x85\\p{Z}"
)

// pyRe compiles a legacy pattern after expanding \w, \d, \s and \D to their
// Unicode meaning. \b and look-arounds are not supported (see boundaryMatches).
func pyRe(p string) *regexp.Regexp {
	var b strings.Builder
	inClass := false
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c == '\\' && i+1 < len(p) {
			esc := p[i : i+2]
			i++
			var exp string
			switch esc {
			case `\w`:
				exp = pyW
			case `\d`:
				exp = pyD
			case `\s`:
				exp = pyS
			case `\D`:
				if inClass {
					panic("scrape: \\D inside a class")
				}
				b.WriteString("[^" + pyD + "]")
				continue
			default:
				b.WriteString(esc)
				continue
			}
			if inClass {
				b.WriteString(exp)
			} else {
				b.WriteString("[" + exp + "]")
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

func pyIsSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\x0b', '\f', '\r', '\x1c', '\x1d', '\x1e', '\x1f', '\x85':
		return true
	}
	return unicode.In(r, unicode.Zs, unicode.Zl, unicode.Zp)
}

func isPyWord(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r) }

func isPyDigit(r rune) bool { return unicode.Is(unicode.Nd, r) }

// pyStrip is str.strip().
func pyStrip(s string) string { return strings.TrimFunc(s, pyIsSpace) }

// pySplit is str.split() (runs of Python whitespace).
func pySplit(s string) []string { return strings.FieldsFunc(s, pyIsSpace) }

// runeLen is len() of a Python str.
func runeLen(s string) int { return utf8.RuneCountInString(s) }

// trunc is s[:n] on a Python str.
func trunc(s string, n int) string {
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

// digitsOnly is re.sub(r'\D', '', s).
func digitsOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if isPyDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// asciiDigits maps Unicode decimal digits to ASCII (int()/float() accept them).
func asciiDigits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r > unicode.MaxASCII && isPyDigit(r) {
			b.WriteByte(byte('0' + digitValue(r)))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func digitValue(r rune) int {
	if r <= 0xFFFF {
		for _, rt := range unicode.Nd.R16 {
			if uint16(r) >= rt.Lo && uint16(r) <= rt.Hi {
				return int(uint16(r)-rt.Lo) % 10
			}
		}
	}
	for _, rt := range unicode.Nd.R32 {
		if uint32(r) >= rt.Lo && uint32(r) <= rt.Hi {
			return int(uint32(r)-rt.Lo) % 10
		}
	}
	return 0
}

// pyInt is int(s) for a string of decimal digits (Unicode digits included).
func pyInt(s string) (int, bool) {
	s = asciiDigits(pyStrip(s))
	n, err := strconv.Atoi(s)
	return n, err == nil
}

// pyFloat is float(s) for the digit/dot strings the legacy regexes capture.
func pyFloat(s string) (float64, bool) {
	s = asciiDigits(pyStrip(s))
	if s == "" || s == "." {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	return f, err == nil
}

// floatRepr is repr(float).
func floatRepr(f float64) string {
	if f == 0 {
		if math.Signbit(f) {
			return "-0.0"
		}
		return "0.0"
	}
	if math.IsInf(f, 0) {
		if f > 0 {
			return "inf"
		}
		return "-inf"
	}
	e := strconv.FormatFloat(math.Abs(f), 'e', -1, 64)
	exp, _ := strconv.Atoi(e[strings.IndexByte(e, 'e')+1:])
	if exp >= -4 && exp < 16 {
		s := strconv.FormatFloat(f, 'f', -1, 64)
		if !strings.Contains(s, ".") {
			s += ".0"
		}
		return s
	}
	return strconv.FormatFloat(f, 'e', -1, 64) // 1e+16, 1.5e-05 as Python
}

// pyRound3 is round(x, 3) (correctly rounded, half-even on the exact value).
func pyRound3(x float64) float64 {
	f, _ := strconv.ParseFloat(strconv.FormatFloat(x, 'f', 3, 64), 64)
	return f
}

// str is str(v) for the scalar lead values a Python Lead attribute can hold.
func str(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "True"
		}
		return "False"
	case json.Number:
		if f, err := t.Float64(); err == nil && strings.ContainsAny(string(t), ".eE") {
			return floatRepr(f)
		}
		return string(t)
	case float64:
		return floatRepr(t)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	}
	return ""
}

// ls reads a Lead attribute as text ("" when absent or None).
func ls(lead map[string]any, k string) string {
	if lead == nil {
		return ""
	}
	return str(lead[k])
}

// ---- json.dumps --------------------------------------------------------------

// pyDumps is json.dumps(v) (", " / ": " separators) with ensure_ascii as given.
func pyDumps(v any, ensureASCII bool) string {
	var b bytes.Buffer
	dumpValue(&b, v, ensureASCII)
	return b.String()
}

func dumpValue(b *bytes.Buffer, v any, ascii bool) {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case string:
		dumpString(b, t, ascii)
	case bool:
		if t {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case int:
		b.WriteString(strconv.Itoa(t))
	case int64:
		b.WriteString(strconv.FormatInt(t, 10))
	case float64:
		switch {
		case math.IsNaN(t):
			b.WriteString("NaN")
		case math.IsInf(t, 1):
			b.WriteString("Infinity")
		case math.IsInf(t, -1):
			b.WriteString("-Infinity")
		default:
			b.WriteString(floatRepr(t))
		}
	case json.Number:
		b.WriteString(string(t))
	case []string:
		b.WriteByte('[')
		for i, s := range t {
			if i > 0 {
				b.WriteString(", ")
			}
			dumpString(b, s, ascii)
		}
		b.WriteByte(']')
	case []any:
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteString(", ")
			}
			dumpValue(b, e, ascii)
		}
		b.WriteByte(']')
	case *providers.Obj:
		b.WriteByte('{')
		for i, k := range t.Keys() {
			if i > 0 {
				b.WriteString(", ")
			}
			dumpString(b, k, ascii)
			b.WriteString(": ")
			val, _ := t.Get(k)
			dumpValue(b, val, ascii)
		}
		b.WriteByte('}')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		o := providers.NewObj()
		for _, k := range keys {
			o.Set(k, t[k])
		}
		dumpValue(b, o, ascii)
	default:
		dumpString(b, fmt.Sprint(t), ascii)
	}
}

func dumpString(b *bytes.Buffer, s string, ascii bool) {
	const hex = "0123456789abcdef"
	b.WriteByte('"')
	for _, r := range strings.ToValidUTF8(s, "�") {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r < 0x20 || (ascii && r > 0x7e):
			if r > 0xFFFF {
				r1, r2 := utf16Pair(r)
				writeU(b, r1, hex)
				writeU(b, r2, hex)
			} else {
				writeU(b, r, hex)
			}
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
}

func utf16Pair(r rune) (rune, rune) {
	r -= 0x10000
	return 0xD800 + (r>>10)&0x3FF, 0xDC00 + r&0x3FF
}

func writeU(b *bytes.Buffer, r rune, hex string) {
	b.WriteString(`\u`)
	b.WriteByte(hex[(r>>12)&0xF])
	b.WriteByte(hex[(r>>8)&0xF])
	b.WriteByte(hex[(r>>4)&0xF])
	b.WriteByte(hex[r&0xF])
}

// ---- URLs --------------------------------------------------------------------

// urlSplit is the scheme / netloc part of urllib.parse.urlparse.
func urlSplit(u string) (scheme, netloc, rest string) {
	if i := strings.IndexByte(u, ':'); i > 0 {
		cand := u[:i]
		valid := true
		for j, r := range cand {
			if !(r < 128 && (unicode.IsLetter(r) || (j > 0 && (unicode.IsDigit(r) || r == '+' || r == '-' || r == '.')))) {
				valid = false
				break
			}
		}
		if valid {
			scheme, u = strings.ToLower(cand), u[i+1:]
		}
	}
	if strings.HasPrefix(u, "//") {
		u = u[2:]
		end := len(u)
		for _, c := range "/?#" {
			if j := strings.IndexRune(u, c); j >= 0 && j < end {
				end = j
			}
		}
		return scheme, u[:end], u[end:]
	}
	return scheme, "", u
}

// normalizeWebsiteURL is website_scraper.normalize_website_url.
func normalizeWebsiteURL(u string) string {
	if u == "" {
		return u
	}
	u = pyStrip(u)
	if !strings.HasPrefix(u, "http") {
		u = "https://" + u
	}
	scheme, netloc, _ := urlSplit(u)
	return scheme + "://" + netloc
}

// domainFromURL is email_finder._domain_from_url.
func domainFromURL(u string) string {
	if u == "" {
		return ""
	}
	u = pyStrip(strings.ToLower(u))
	if !strings.HasPrefix(u, "http") {
		u = "https://" + u
	}
	_, netloc, _ := urlSplit(u)
	return strings.TrimPrefix(netloc, "www.")
}

// bareDomain is the _extract_domain of email_harvester (lower=true) and
// deep_scraper (lower applied last): prefixes https://, http://, www. removed
// in that order, then everything from the first "/" and "?" dropped.
func bareDomain(u string, lowerFirst bool) string {
	if u == "" {
		return ""
	}
	if lowerFirst {
		u = strings.ToLower(u)
	}
	u = pyStrip(u)
	for _, p := range []string{"https://", "http://", "www."} {
		u = strings.TrimPrefix(u, p)
	}
	u = strings.SplitN(u, "/", 2)[0]
	u = strings.SplitN(u, "?", 2)[0]
	return strings.ToLower(u)
}

// isAlpha is str.isalpha().
func isAlpha(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsLetter(r) {
			return false
		}
	}
	return true
}

// ---- \b emulation -------------------------------------------------------------

// phone10 matches `\b\d{3}[\s\-.]?\d{3}[\s\-.]?\d{4}\b` with Python's Unicode
// word boundaries, returning the non-overlapping matches left to right (the
// optional separators are tried present-first, as the backtracking engine does).
func phone10(text string) []string {
	rs := []rune(text)
	n := len(rs)
	word := func(i int) bool { return i >= 0 && i < n && isPyWord(rs[i]) }
	sep := func(i int) bool { return i < n && (pyIsSpace(rs[i]) || rs[i] == '-' || rs[i] == '.') }
	digits := func(i, k int) bool {
		if i+k > n {
			return false
		}
		for j := i; j < i+k; j++ {
			if !isPyDigit(rs[j]) {
				return false
			}
		}
		return true
	}
	var out []string
	for i := 0; i < n; {
		if word(i-1) || !word(i) {
			i++
			continue
		}
		end := -1
	try:
		for _, s1 := range []bool{true, false} {
			p := i
			if !digits(p, 3) {
				break
			}
			p += 3
			if s1 {
				if !sep(p) {
					continue
				}
				p++
			}
			if !digits(p, 3) {
				continue
			}
			p += 3
			for _, s2 := range []bool{true, false} {
				q := p
				if s2 {
					if !sep(q) {
						continue
					}
					q++
				}
				if !digits(q, 4) {
					continue
				}
				q += 4
				if word(q-1) != word(q) { // \b
					end = q
					break try
				}
			}
		}
		if end < 0 {
			i++
			continue
		}
		out = append(out, string(rs[i:end]))
		i = end
	}
	return out
}

// wordKeywordIndex is re.search(rf'(?<![A-Za-z]){re.escape(kw)}(?![A-Za-z])',
// text, re.IGNORECASE): the byte offset of the first case-insensitive
// occurrence of kw not touching an ASCII letter on either side, or -1.
func wordKeywordIndex(text, kw string) int {
	re := regexp.MustCompile(`(?i)^` + regexp.QuoteMeta(kw))
	asciiLetter := func(r rune) bool { return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') }
	prev := rune(-1)
	for i, r := range text {
		if prev < 0 || !asciiLetter(prev) {
			if loc := re.FindStringIndex(text[i:]); loc != nil {
				next, _ := utf8.DecodeRuneInString(text[i+loc[1]:])
				if i+loc[1] >= len(text) || !asciiLetter(next) {
					return i
				}
			}
		}
		prev = r
	}
	return -1
}

// decodeJSON is json.loads (numbers kept as json.Number); ok=false where
// Python raises.
func decodeJSON(s string) (any, bool) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil || dec.More() {
		return nil, false
	}
	return v, true
}
