package colgraph

import (
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
)

func parseNumberFloat(n json.Number) (float64, bool) {
	f, err := strconv.ParseFloat(string(n), 64)
	if err != nil && !math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// pyFloatRepr is repr(float).
func pyFloatRepr(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	sign := ""
	if math.Signbit(f) {
		sign = "-"
		f = -f
	}
	if f == 0 {
		return sign + "0.0"
	}
	e := strconv.FormatFloat(f, 'e', -1, 64) // d.ddde+XX
	mant, expS, _ := strings.Cut(e, "e")
	exp, _ := strconv.Atoi(expS)
	digits := strings.Replace(mant, ".", "", 1)
	if exp >= -4 && exp < 16 {
		switch {
		case exp >= 0:
			if len(digits) <= exp+1 {
				return sign + digits + strings.Repeat("0", exp+1-len(digits)) + ".0"
			}
			return sign + digits[:exp+1] + "." + digits[exp+1:]
		default:
			return sign + "0." + strings.Repeat("0", -exp-1) + digits
		}
	}
	m := digits[:1]
	if len(digits) > 1 {
		m += "." + digits[1:]
	}
	es := strconv.Itoa(abs(exp))
	if len(es) < 2 {
		es = "0" + es
	}
	if exp < 0 {
		return sign + m + "e-" + es
	}
	return sign + m + "e+" + es
}

func abs(i int) int {
	if i < 0 {
		return -i
	}
	return i
}

func pyStrRepr(s string) string {
	q := byte('\'')
	if strings.Contains(s, "'") && !strings.Contains(s, "\"") {
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
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(q)
	return b.String()
}

func pyRepr(v any) string {
	if s, ok := v.(string); ok {
		return pyStrRepr(s)
	}
	return pyStrValue(v)
}

// pyStrValue is str(value) for decoded JSON values.
func pyStrValue(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case string:
		return t
	case bool:
		if t {
			return "True"
		}
		return "False"
	case json.Number:
		s := string(t)
		if strings.ContainsAny(s, ".eE") {
			if f, ok := parseNumberFloat(t); ok {
				return pyFloatRepr(f)
			}
			return s
		}
		if strings.Trim(s, "-0") == "" {
			return "0"
		}
		return s
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return pyFloatRepr(t)
	case float32:
		return pyFloatRepr(float64(t))
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = pyRepr(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case []string:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = pyStrRepr(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		return pyDictRepr(t, t, nil)
	case Col:
		return pyDictRepr(t.Vals, t.Vals, t.Keys)
	}
	return "<?>"
}

func pyDictRepr(m map[string]any, _ map[string]any, keys []string) string {
	if keys == nil {
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
	}
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = pyStrRepr(k) + ": " + pyRepr(m[k])
	}
	return "{" + strings.Join(parts, ", ") + "}"
}
