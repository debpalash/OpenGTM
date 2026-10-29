package aicol

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
)

// Obj is a JSON object that keeps Python dict insertion order (research metadata).
type Obj struct {
	Keys []string
	Vals map[string]any
}

// NewObj builds an Obj from alternating key, value pairs.
func NewObj(kv ...any) *Obj {
	o := &Obj{Vals: map[string]any{}}
	for i := 0; i+1 < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o
}

// Set adds or replaces a key (a replaced key keeps its position).
func (o *Obj) Set(k string, v any) {
	if _, ok := o.Vals[k]; !ok {
		o.Keys = append(o.Keys, k)
	}
	o.Vals[k] = v
}

// Get returns a value.
func (o *Obj) Get(k string) any {
	if o == nil {
		return nil
	}
	return o.Vals[k]
}

// ToMap converts to plain maps and slices (nested Objs included).
func (o *Obj) ToMap() map[string]any {
	if o == nil {
		return nil
	}
	out := make(map[string]any, len(o.Keys))
	for _, k := range o.Keys {
		out[k] = plain(o.Vals[k])
	}
	return out
}

func plain(v any) any {
	switch t := v.(type) {
	case *Obj:
		return t.ToMap()
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = plain(e)
		}
		return out
	}
	return v
}

// MarshalJSON is compact JSON in insertion order with Python float text.
func (o *Obj) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	if err := writeJSON(&b, o, false, false); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// JSON is compact JSON of v (Objs in insertion order, maps sorted, Python float text).
func JSON(v any) ([]byte, error) {
	var b bytes.Buffer
	if err := writeJSON(&b, v, false, false); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// writeJSON writes v; pySep uses json.dumps' default ", " / ": " separators and ascii
// escapes non-ASCII like ensure_ascii=True.
func writeJSON(b *bytes.Buffer, v any, pySep, ascii bool) error {
	comma, colon := ",", ":"
	if pySep {
		comma, colon = ", ", ": "
	}
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(t))
	case string:
		writeStr(b, t, ascii)
	case int:
		b.WriteString(strconv.Itoa(t))
	case int64:
		b.WriteString(strconv.FormatInt(t, 10))
	case json.Number:
		b.WriteString(string(t))
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			b.WriteString(map[bool]string{true: "NaN", false: "Infinity"}[math.IsNaN(t)])
			return nil
		}
		s, err := canonjson.FloatRepr(t)
		if err != nil {
			return err
		}
		b.WriteString(s)
	case *Obj:
		if t == nil {
			b.WriteString("null")
			return nil
		}
		b.WriteByte('{')
		for i, k := range t.Keys {
			if i > 0 {
				b.WriteString(comma)
			}
			writeStr(b, k, ascii)
			b.WriteString(colon)
			if err := writeJSON(b, t.Vals[k], pySep, ascii); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteString(comma)
			}
			if err := writeJSON(b, e, pySep, ascii); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case map[string]any:
		o := &Obj{Vals: t}
		for _, k := range sortedKeys(t) {
			o.Keys = append(o.Keys, k)
		}
		return writeJSON(b, o, pySep, ascii)
	default:
		raw, err := json.Marshal(t)
		if err != nil {
			return err
		}
		b.Write(raw)
	}
	return nil
}

func writeStr(b *bytes.Buffer, s string, ascii bool) {
	b.WriteByte('"')
	for _, r := range s {
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
		case r < 0x20 || (ascii && r > 0x7f):
			if r > 0xffff {
				a, c := utf16.EncodeRune(r)
				b.WriteString(`\u` + hex4(a) + `\u` + hex4(c))
			} else {
				b.WriteString(`\u` + hex4(r))
			}
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
}

func hex4(r rune) string {
	s := strconv.FormatInt(int64(r), 16)
	return strings.Repeat("0", 4-len(s)) + s
}

func sortedKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	for i := 1; i < len(ks); i++ {
		for j := i; j > 0 && ks[j] < ks[j-1]; j-- {
			ks[j], ks[j-1] = ks[j-1], ks[j]
		}
	}
	return ks
}

// pyJSONSize is len(json.dumps(v, default=str).encode()): default separators, ensure_ascii.
func pyJSONSize(v any) int {
	var b bytes.Buffer
	if err := writeJSON(&b, v, true, true); err != nil {
		return math.MaxInt
	}
	return b.Len()
}

func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// parseIntText is int(str) for a stripped string (optional sign, digits, underscores
// between digits).
func parseIntText(s string) (int, error) {
	n, err := strconv.ParseInt(strings.ReplaceAll(s, "_", ""), 10, 64)
	if err != nil || strings.HasPrefix(s, "_") || strings.HasSuffix(s, "_") || strings.Contains(s, "__") {
		return 0, errNotInt
	}
	return int(n), nil
}

// pyFloat is float(v) for a decoded JSON value.
func pyFloat(v any) (float64, bool) {
	if s, ok := v.(string); ok {
		f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		return f, err == nil
	}
	return toFloat(v)
}
