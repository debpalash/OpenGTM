package audiencevalid

import (
	"encoding/json"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Helpers reproducing the Python value semantics the audience code relies on
// (truthiness, str(), str.lower(), str.split()). JSON numbers must arrive as
// json.Number (decoder.UseNumber) so ints and floats stay distinguishable;
// a float64 is taken as a Python float.

func isPySpace(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }

func pyStrip(s string) string { return strings.TrimFunc(s, isPySpace) }

// pyCollapse is " ".join(s.split()).
func pyCollapse(s string) string { return strings.Join(strings.FieldsFunc(s, isPySpace), " ") }

func isIntNumber(n json.Number) bool {
	s := string(n)
	if s == "" {
		return false
	}
	return !strings.ContainsAny(s, ".eE")
}

func toBig(v any) (*big.Int, bool) {
	switch x := v.(type) {
	case int:
		return big.NewInt(int64(x)), true
	case int8:
		return big.NewInt(int64(x)), true
	case int16:
		return big.NewInt(int64(x)), true
	case int32:
		return big.NewInt(int64(x)), true
	case int64:
		return big.NewInt(x), true
	case uint:
		return new(big.Int).SetUint64(uint64(x)), true
	case uint8:
		return big.NewInt(int64(x)), true
	case uint16:
		return big.NewInt(int64(x)), true
	case uint32:
		return big.NewInt(int64(x)), true
	case uint64:
		return new(big.Int).SetUint64(x), true
	case json.Number:
		if !isIntNumber(x) {
			return nil, false
		}
		return new(big.Int).SetString(string(x), 10)
	}
	return nil, false
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	case float32:
		return x != 0
	case json.Number:
		if b, ok := toBig(x); ok {
			return b.Sign() != 0
		}
		f, err := strconv.ParseFloat(string(x), 64)
		return err != nil || f != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	if b, ok := toBig(v); ok {
		return b.Sign() != 0
	}
	return true
}

// pyStr is str(v).
func pyStr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case string:
		return x
	case bool:
		if x {
			return "True"
		}
		return "False"
	case float64:
		return pyFloatRepr(x)
	case float32:
		return pyFloatRepr(float64(x))
	case json.Number:
		if b, ok := toBig(x); ok {
			return b.String()
		}
		f, err := strconv.ParseFloat(string(x), 64)
		if err != nil && !math.IsInf(f, 0) {
			return string(x)
		}
		return pyFloatRepr(f)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = pyRepr(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(x)) // Python keeps insertion order; a Go map has none
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = pyRepr(k) + ": " + pyRepr(x[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	if b, ok := toBig(v); ok {
		return b.String()
	}
	return ""
}

func pyRepr(v any) string {
	s, ok := v.(string)
	if !ok {
		return pyStr(v)
	}
	q := byte('\'')
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		q = '"'
	}
	var b strings.Builder
	b.WriteByte(q)
	for _, r := range s {
		switch {
		case r == rune(q) || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			b.WriteString(`\x` + strconv.FormatInt(int64(r)+0x100, 16)[1:])
		case r == utf8.RuneError || !unicode.IsPrint(r):
			if r < 0x100 {
				b.WriteString(`\x` + strconv.FormatInt(int64(r)+0x100, 16)[1:])
			} else if r < 0x10000 {
				b.WriteString(`\u` + strconv.FormatInt(int64(r)+0x10000, 16)[1:])
			} else {
				b.WriteString(`\U` + strconv.FormatInt(int64(r)+0x100000000, 16)[1:])
			}
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(q)
	return b.String()
}

// pyFloatRepr is repr(float): shortest round-trip digits, fixed notation for
// decimal exponents in (-4, 16], else scientific with a two-digit exponent.
func pyFloatRepr(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	s := strconv.FormatFloat(f, 'e', -1, 64) // d.ddde±XX
	sign := ""
	if s[0] == '-' {
		sign, s = "-", s[1:]
	}
	mant, expS, _ := strings.Cut(s, "e")
	exp, _ := strconv.Atoi(expS)
	digits := strings.Replace(mant, ".", "", 1)
	decpt := exp + 1
	if decpt > -4 && decpt <= 16 {
		switch {
		case decpt <= 0:
			return sign + "0." + strings.Repeat("0", -decpt) + digits
		case decpt >= len(digits):
			return sign + digits + strings.Repeat("0", decpt-len(digits)) + ".0"
		default:
			return sign + digits[:decpt] + "." + digits[decpt:]
		}
	}
	m := digits[:1]
	if len(digits) > 1 {
		m += "." + digits[1:]
	}
	es := "+"
	if exp < 0 {
		es, exp = "-", -exp
	}
	e := strconv.Itoa(exp)
	if len(e) < 2 {
		e = "0" + e
	}
	return sign + m + "e" + es + e
}

func isCased(r rune) bool {
	return unicode.IsUpper(r) || unicode.IsLower(r) || unicode.IsTitle(r) ||
		unicode.Is(unicode.Other_Lowercase, r) || unicode.Is(unicode.Other_Uppercase, r)
}

func isCaseIgnorable(r rune) bool {
	return unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf, unicode.Lm, unicode.Sk) ||
		strings.ContainsRune("'.:··՟״‘’․‧︓﹒﹕＇．：", r)
}

// pyLower is str.lower(): full case mapping, U+0130 becomes "i" plus a
// combining dot and a final-position capital sigma becomes U+03C2.
func pyLower(s string) string {
	rs := []rune(s)
	var b strings.Builder
	for i, r := range rs {
		switch r {
		case 0x130:
			b.WriteString("i̇")
		case 0x3a3:
			before := false
			for j := i - 1; j >= 0; j-- {
				if isCaseIgnorable(rs[j]) {
					continue
				}
				before = isCased(rs[j])
				break
			}
			after := false
			for j := i + 1; j < len(rs); j++ {
				if isCaseIgnorable(rs[j]) {
					continue
				}
				after = isCased(rs[j])
				break
			}
			if before && !after {
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
