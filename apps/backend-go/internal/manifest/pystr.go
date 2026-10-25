// Package manifest executes declarative provider manifests. It is a
// behavior-exact port of the Python declarative engine (template.py,
// compiler.py, normalize.py); parity is pinned by testdata/manifest_vectors.json.
package manifest

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
)

// pyWS is the character class Python's re \s (and str.isspace) uses; Go's \s
// is ASCII-only.
const pyWS = `[\t-\r\x1c-\x1f \x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}]`

func pyIsSpace(r rune) bool {
	switch {
	case r >= 9 && r <= 13, r >= 0x1c && r <= 0x20, r == 0x85, r == 0xa0, r == 0x1680,
		r >= 0x2000 && r <= 0x200a, r == 0x2028, r == 0x2029, r == 0x202f, r == 0x205f, r == 0x3000:
		return true
	}
	return false
}

// pyStrip mirrors str.strip().
func pyStrip(s string) string { return strings.TrimFunc(s, pyIsSpace) }

// pyFields mirrors str.split() with no separator.
func pyFields(s string) []string { return strings.FieldsFunc(s, pyIsSpace) }

// truncRunes mirrors Python's s[:n] (code points, not bytes).
func truncRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := 0
	for p := range s {
		if i == n {
			return s[:p]
		}
		i++
	}
	return s
}

// pyTruthy mirrors bool(v) for JSON-shaped values.
func pyTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case int64:
		return x != 0
	case int:
		return x != 0
	case float64:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

func floatStr(f float64) string {
	s, err := canonjson.FloatRepr(f)
	if err != nil { // NaN / inf
		switch {
		case f != f:
			return "nan"
		case f > 0:
			return "inf"
		}
		return "-inf"
	}
	return s
}

// pyStr mirrors str(v) for JSON-shaped values (True, None, 1.0, {'a': 1} ...).
func pyStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return pyRepr(v)
}

// pyRepr mirrors repr(v) for JSON-shaped values.
func pyRepr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case string:
		return reprString(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case int:
		return strconv.Itoa(x)
	case float64:
		return floatStr(x)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = pyRepr(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys) // Go maps carry no insertion order
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = reprString(k) + ": " + pyRepr(x[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return fmt.Sprint(v)
}

func reprString(s string) string {
	q := '\''
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		q = '"'
	}
	var b strings.Builder
	b.WriteRune(q)
	for _, r := range s {
		switch {
		case r == q || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == ' ' || (r > 0x20 && r != 0x7f && unicode.IsPrint(r)):
			b.WriteRune(r)
		case r < 0x100:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r < 0x10000:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
	}
	b.WriteRune(q)
	return b.String()
}
