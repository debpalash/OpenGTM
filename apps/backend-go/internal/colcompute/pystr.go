package colcompute

import (
	"math"
	"math/big"
	"strconv"
	"strings"
	"unicode"
)

// pyIsSpace is str.isspace() for one code point (CPython's Py_UNICODE_ISSPACE).
func pyIsSpace(r rune) bool {
	switch {
	case r >= 0x09 && r <= 0x0d, r >= 0x1c && r <= 0x20:
		return true
	case r == 0x85, r == 0xa0, r == 0x1680, r >= 0x2000 && r <= 0x200a:
		return true
	case r == 0x2028, r == 0x2029, r == 0x202f, r == 0x205f, r == 0x3000:
		return true
	}
	return false
}

func pyStripSpace(s string) string {
	return strings.TrimFunc(s, pyIsSpace)
}

func isCased(r rune) bool {
	return unicode.IsUpper(r) || unicode.IsLower(r) || unicode.IsTitle(r) ||
		unicode.Is(unicode.Other_Lowercase, r) || unicode.Is(unicode.Other_Uppercase, r)
}

func isCaseIgnorable(r rune) bool {
	switch r {
	case 0x27, 0x2e, 0x3a, 0xb7, 0x387, 0x55f, 0x5f4, 0x2018, 0x2019, 0x2024, 0x2027, 0xfe13, 0xfe52, 0xfe55, 0xff07, 0xff0e, 0xff1a:
		return true
	}
	return unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf, unicode.Lm, unicode.Sk)
}

func finalSigma(rs []rune, i int) bool {
	j := i - 1
	for j >= 0 && isCaseIgnorable(rs[j]) {
		j--
	}
	if j < 0 || !isCased(rs[j]) {
		return false
	}
	j = i + 1
	for j < len(rs) && isCaseIgnorable(rs[j]) {
		j++
	}
	return j == len(rs) || !isCased(rs[j])
}

func lowerRune(b *strings.Builder, rs []rune, i int) {
	r := rs[i]
	if r == 0x3a3 {
		if finalSigma(rs, i) {
			b.WriteRune(0x3c2)
		} else {
			b.WriteRune(0x3c3)
		}
		return
	}
	if m, ok := pyLowerMap[r]; ok {
		b.WriteString(m)
		return
	}
	b.WriteRune(r)
}

// pyLower is str.lower().
func pyLower(s string) string {
	rs := []rune(s)
	var b strings.Builder
	b.Grow(len(s))
	for i := range rs {
		lowerRune(&b, rs, i)
	}
	return b.String()
}

// pyUpper is str.upper().
func pyUpper(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if m, ok := pyUpperMap[r]; ok {
			b.WriteString(m)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// pyTitle is str.title().
func pyTitle(s string) string {
	rs := []rune(s)
	var b strings.Builder
	prevCased := false
	for i, r := range rs {
		if prevCased {
			lowerRune(&b, rs, i)
		} else if m, ok := pyTitleMap[r]; ok {
			b.WriteString(m)
		} else {
			b.WriteRune(r)
		}
		prevCased = isCased(r)
	}
	return b.String()
}

// digitVal is the decimal value of a Unicode Nd code point, -1 otherwise.
func digitVal(r rune) int {
	if r >= '0' && r <= '9' {
		return int(r - '0')
	}
	if r < 0x80 || !unicode.IsDigit(r) {
		return -1
	}
	for _, rg := range unicode.Nd.R16 {
		if rune(rg.Lo) <= r && r <= rune(rg.Hi) {
			return int(r-rune(rg.Lo)) % 10
		}
	}
	for _, rg := range unicode.Nd.R32 {
		if rune(rg.Lo) <= r && r <= rune(rg.Hi) {
			return int(r-rune(rg.Lo)) % 10
		}
	}
	return -1
}

// asciiDigits rewrites Unicode decimal digits to ASCII (what int()/float() accept).
func asciiDigits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if d := digitVal(r); d >= 0 && r > 0x7f {
			b.WriteByte(byte('0' + d))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// validUnderscores enforces Python's rule: an underscore only between two digits.
func validUnderscores(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '_' {
			continue
		}
		if i == 0 || i == len(s)-1 || s[i-1] < '0' || s[i-1] > '9' || s[i+1] < '0' || s[i+1] > '9' {
			return false
		}
	}
	return true
}

// parsePyInt is int(str) in base 10 (ok=false for ValueError, including the digit limit).
func parsePyInt(s string) (*big.Int, bool) {
	s = asciiDigits(pyStripSpace(s))
	if s == "" {
		return nil, false
	}
	neg := false
	switch s[0] {
	case '+':
		s = s[1:]
	case '-':
		neg, s = true, s[1:]
	}
	if s == "" || s[0] == '_' || !validUnderscores(s) {
		return nil, false
	}
	s = strings.ReplaceAll(s, "_", "")
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return nil, false
		}
	}
	if len(s) > maxStrDigits {
		return nil, false
	}
	i, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return nil, false
	}
	if neg {
		i.Neg(i)
	}
	return i, true
}

// parsePyFloat is float(str) (ok=false for ValueError).
func parsePyFloat(s string) (float64, bool) {
	s = asciiDigits(pyStripSpace(s))
	if s == "" {
		return 0, false
	}
	sign := 1.0
	body := s
	switch body[0] {
	case '+':
		body = body[1:]
	case '-':
		sign, body = -1, body[1:]
	}
	switch strings.ToLower(body) {
	case "nan":
		return math.NaN(), true
	case "inf", "infinity":
		return math.Inf(int(sign)), true
	}
	if body == "" || !validUnderscores(body) {
		return 0, false
	}
	body = strings.ReplaceAll(body, "_", "")
	// grammar: digits [. digits] | . digits, then optional exponent
	i, nd := 0, 0
	for i < len(body) && body[i] >= '0' && body[i] <= '9' {
		i++
		nd++
	}
	if i < len(body) && body[i] == '.' {
		i++
		for i < len(body) && body[i] >= '0' && body[i] <= '9' {
			i++
			nd++
		}
	}
	if nd == 0 {
		return 0, false
	}
	if i < len(body) && (body[i] == 'e' || body[i] == 'E') {
		i++
		if i < len(body) && (body[i] == '+' || body[i] == '-') {
			i++
		}
		ed := 0
		for i < len(body) && body[i] >= '0' && body[i] <= '9' {
			i++
			ed++
		}
		if ed == 0 {
			return 0, false
		}
	}
	if i != len(body) {
		return 0, false
	}
	f, err := strconv.ParseFloat(body, 64)
	if err != nil && !math.IsInf(f, 0) {
		return 0, false
	}
	return sign * f, true
}

// pySplitSpace is str.split(None, maxsplit).
func pySplitSpace(s string, maxsplit int) []any {
	var out []any
	rs := []rune(s)
	i := 0
	for {
		for i < len(rs) && pyIsSpace(rs[i]) {
			i++
		}
		if i >= len(rs) {
			break
		}
		if maxsplit >= 0 && len(out) >= maxsplit {
			out = append(out, string(rs[i:]))
			break
		}
		j := i
		for j < len(rs) && !pyIsSpace(rs[j]) {
			j++
		}
		out = append(out, string(rs[i:j]))
		i = j
	}
	if out == nil {
		out = []any{}
	}
	return out
}

func pySplitSep(s, sep string, maxsplit int) []any {
	n := -1
	if maxsplit >= 0 {
		n = maxsplit + 1
	}
	parts := strings.SplitN(s, sep, n)
	out := make([]any, len(parts))
	for i, p := range parts {
		out[i] = p
	}
	return out
}

// runeLen is len(str) (code points).
func runeLen(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}
