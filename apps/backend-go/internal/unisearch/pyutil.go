package unisearch

import (
	"bytes"
	"encoding/json"
	"math"
	"net/url"
	"strconv"
	"strings"
	"unicode"
)

// pyQuote is urllib.parse.quote(s, safe): UTF-8 percent-encoding that leaves letters, digits "_.-~" and the safe
// characters alone.
func pyQuote(s, safe string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '.', c == '-', c == '~':
			b.WriteByte(c)
		case strings.IndexByte(safe, c) >= 0:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteString(strings.ToUpper(strconv.FormatInt(int64(c)+256, 16)[1:]))
		}
	}
	return b.String()
}

// pyURLEncode is urllib.parse.urlencode for string pairs in the given order (quote_plus).
func pyURLEncode(pairs ...string) string {
	var parts []string
	for i := 0; i+1 < len(pairs); i += 2 {
		parts = append(parts, url.QueryEscape(pairs[i])+"="+url.QueryEscape(pairs[i+1]))
	}
	return strings.Join(parts, "&")
}

// pyStrip is str.strip(): Unicode whitespace plus the ASCII separators \x1c-\x1f.
func pyStrip(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) })
}

// head is s[:n] in characters.
func head(s string, n int) string {
	if n < 0 {
		n = 0
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

// ── JSON access with Python semantics ───────────────────────────────────────

type jm = map[string]any

func decodeJSON(b []byte) (any, bool) {
	var v any
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	return v, true
}

func asMap(v any) jm {
	m, _ := v.(map[string]any)
	return m
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

// has is "key present and not None".
func has(m jm, k string) bool { v, ok := m[k]; return ok && v != nil }

// truthy is Python truthiness for decoded JSON.
func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case float64:
		return t != 0
	case string:
		return t != ""
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	}
	return true
}

// strField is `m.get(k, def)` for a field the result model needs as str: missing gives def; anything but a
// string (null, number, list) is a pydantic validation error (ok=false: the caller skips the entry).
func strField(m jm, k, def string) (string, bool) {
	v, ok := m[k]
	if !ok {
		return def, true
	}
	s, ok := v.(string)
	return s, ok
}

// strOrNone is an Optional[str] model field.
func strOrNone(v any) (*string, bool) {
	switch t := v.(type) {
	case nil:
		return nil, true
	case string:
		return &t, true
	}
	return nil, false
}

// yearField is Optional[int] with pydantic's lax validation: null is None, an integral number or a numeric
// string converts, anything else is a validation error.
func yearField(v any) (*int, bool) {
	switch t := v.(type) {
	case nil:
		return nil, true
	case float64:
		if t != math.Trunc(t) || math.IsInf(t, 0) {
			return nil, false
		}
		n := int(t)
		return &n, true
	case string:
		n, err := strconv.Atoi(pyStrip(t))
		if err != nil {
			return nil, false
		}
		return &n, true
	}
	return nil, false
}

// authorString is ', '.join(names[:3]) plus ' et al.' past three.
func authorString(names []string) string {
	n := names
	if len(n) > 3 {
		n = n[:3]
	}
	s := strings.Join(n, ", ")
	if len(names) > 3 {
		s += " et al."
	}
	return s
}
