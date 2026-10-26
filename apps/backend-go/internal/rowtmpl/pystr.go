package rowtmpl

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
)

// NOTE: PyStr/pyLower/pyStrip are minimal copies of the helpers in
// internal/colgraph (unexported) and internal/colcompute (in progress, not
// importable). Dedupe onto one shared exported helper once colcompute lands.

func pyIsSpace(r rune) bool {
	switch {
	case r >= 0x09 && r <= 0x0d, r >= 0x1c && r <= 0x20, r == 0x85, r == 0xa0, r == 0x1680,
		r >= 0x2000 && r <= 0x200a, r == 0x2028, r == 0x2029, r == 0x202f, r == 0x205f, r == 0x3000:
		return true
	}
	return false
}

// pyStrip is str.strip().
func pyStrip(s string) string { return strings.TrimFunc(s, pyIsSpace) }

func isCased(r rune) bool { return unicode.IsUpper(r) || unicode.IsLower(r) || unicode.IsTitle(r) }

func isCaseIgnorable(r rune) bool {
	if unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf, unicode.Lm, unicode.Sk) {
		return true
	}
	return strings.ContainsRune("'.:^`·‘’․‧﹒＇．：", r)
}

// pyLower is str.lower(): full case mapping for U+0130 and the final-sigma rule.
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
				b.WriteRune('ς')
			} else {
				b.WriteRune('σ')
			}
		default:
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

func pyFloatRepr(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	s, _ := canonjson.FloatRepr(f)
	return s
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
		case r != ' ' && !unicode.IsPrint(r):
			switch {
			case r < 0x100:
				fmt.Fprintf(&b, `\x%02x`, r)
			case r < 0x10000:
				fmt.Fprintf(&b, `\u%04x`, r)
			default:
				fmt.Fprintf(&b, `\U%08x`, r)
			}
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
	return PyStr(v)
}

func numberStr(t json.Number) string {
	s := string(t)
	if strings.ContainsAny(s, ".eE") {
		if f, err := strconv.ParseFloat(s, 64); err == nil || math.IsInf(f, 0) {
			return pyFloatRepr(f)
		}
		return s
	}
	if strings.Trim(s, "-0") == "" {
		return "0"
	}
	return s
}

func dictRepr(vals map[string]any, keys []string) string {
	if keys == nil {
		for k := range vals {
			keys = append(keys, k)
		}
		sort.Strings(keys)
	}
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = pyStrRepr(k) + ": " + pyRepr(vals[k])
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// PyStr is Python str(value) for decoded JSON values (json.Number keeps the
// int/float distinction). nil is "None"; use CellText for the nil-is-empty rule.
func PyStr(v any) string {
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
		return numberStr(t)
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
		return dictRepr(t, nil)
	case colgraph.Col:
		return dictRepr(t.Vals, t.Keys)
	}
	return fmt.Sprint(v)
}

// CellText is cell_text: only nil is absent; zero and False are kept.
func CellText(v any) string {
	if v == nil {
		return ""
	}
	return PyStr(v)
}
