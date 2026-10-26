package entities

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

// Ordered JSON values. The entity tables keep their payloads in `json` columns that Python
// reads into dicts and writes back with `{**d, k: v}`; key order is therefore part of what a
// round trip preserves. Values are *OMap (object), []any (array), json.Number, string, bool
// or nil.

// OMap is a JSON object that remembers key order.
type OMap struct {
	keys []string
	vals map[string]any
}

// NewOMap returns an empty object.
func NewOMap() *OMap { return &OMap{vals: map[string]any{}} }

// Get returns the value for k.
func (m *OMap) Get(k string) (any, bool) { v, ok := m.vals[k]; return v, ok }

// Has reports whether k is present.
func (m *OMap) Has(k string) bool { _, ok := m.vals[k]; return ok }

// Set stores v, keeping the position of an existing key (Python dict semantics).
func (m *OMap) Set(k string, v any) {
	if _, ok := m.vals[k]; !ok {
		m.keys = append(m.keys, k)
	}
	m.vals[k] = v
}

// SetDefault is dict.setdefault.
func (m *OMap) SetDefault(k string, v any) {
	if _, ok := m.vals[k]; !ok {
		m.Set(k, v)
	}
}

// Delete removes k (dict.pop).
func (m *OMap) Delete(k string) {
	if _, ok := m.vals[k]; !ok {
		return
	}
	delete(m.vals, k)
	for i, x := range m.keys {
		if x == k {
			m.keys = append(m.keys[:i:i], m.keys[i+1:]...)
			break
		}
	}
}

// Keys returns the keys in insertion order (a copy).
func (m *OMap) Keys() []string { return append([]string(nil), m.keys...) }

// Len is the number of keys.
func (m *OMap) Len() int { return len(m.keys) }

// Clone is a shallow copy (dict(m) / {**m}).
func (m *OMap) Clone() *OMap {
	c := NewOMap()
	for _, k := range m.keys {
		c.Set(k, m.vals[k])
	}
	return c
}

// ParseJSON decodes raw into ordered values. Empty input is an error.
func ParseJSON(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := parseValue(dec, 0)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("entities: trailing data in json")
	}
	return v, nil
}

// maxJSONDepth bounds container nesting (matches wbcrud's maxJSONDepth).
const maxJSONDepth = 200

var errJSONTooDeep = fmt.Errorf("entities: json nesting deeper than %d levels", maxJSONDepth)

func parseValue(dec *json.Decoder, depth int) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return tok, nil // string, json.Number, bool, nil
	}
	if depth >= maxJSONDepth {
		return nil, errJSONTooDeep
	}
	if d == '{' {
		m := NewOMap()
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			v, err := parseValue(dec, depth+1)
			if err != nil {
				return nil, err
			}
			m.Set(kt.(string), v)
		}
		_, err := dec.Token()
		return m, err
	}
	arr := []any{}
	for dec.More() {
		v, err := parseValue(dec, depth+1)
		if err != nil {
			return nil, err
		}
		arr = append(arr, v)
	}
	_, err = dec.Token()
	return arr, err
}

// parseOr parses raw (SQL NULL / empty -> def).
func parseOr(raw []byte, def any) any {
	if len(bytes.TrimSpace(raw)) == 0 {
		return def
	}
	v, err := ParseJSON(raw)
	if err != nil {
		return def
	}
	return v
}

// MarshalJSON encodes ordered values; json.RawMessage is embedded verbatim and float64 is
// rendered like Python's repr.
func MarshalJSON(v any) []byte {
	var b bytes.Buffer
	writeValue(&b, v)
	return b.Bytes()
}

func writeValue(b *bytes.Buffer, v any) {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case *OMap:
		b.WriteByte('{')
		for i, k := range x.keys {
			if i > 0 {
				b.WriteString(", ")
			}
			writeString(b, k)
			b.WriteString(": ")
			writeValue(b, x.vals[k])
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteString(", ")
			}
			writeValue(b, e)
		}
		b.WriteByte(']')
	case []string:
		a := make([]any, len(x))
		for i, s := range x {
			a[i] = s
		}
		writeValue(b, a)
	case string:
		writeString(b, x)
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case json.Number:
		b.WriteString(x.String())
	case json.RawMessage:
		b.Write(x)
	case float64:
		b.WriteString(pyFloat(x))
	case int:
		b.WriteString(strconv.Itoa(x))
	case int32:
		b.WriteString(strconv.Itoa(int(x)))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	default:
		raw, _ := json.Marshal(x)
		b.Write(raw)
	}
}

func writeString(b *bytes.Buffer, s string) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	b.Write(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
}

// pyFloat renders a float like Python's repr for the magnitudes stored here.
func pyFloat(f float64) string {
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.ContainsAny(s, ".") {
		s += ".0"
	}
	return s
}

// truthy is Python truthiness of a decoded JSON value.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case json.Number:
		f, err := x.Float64()
		return err != nil || f != 0
	case []any:
		return len(x) > 0
	case *OMap:
		return x.Len() > 0
	}
	return true
}

// jsonEqual is Python == over decoded JSON (objects order-insensitive, 1 == 1.0).
func jsonEqual(a, b any) bool {
	switch x := a.(type) {
	case *OMap:
		y, ok := b.(*OMap)
		if !ok || x.Len() != y.Len() {
			return false
		}
		for _, k := range x.keys {
			bv, ok := y.vals[k]
			if !ok || !jsonEqual(x.vals[k], bv) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !jsonEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	case json.Number:
		y, ok := b.(json.Number)
		if !ok {
			return false
		}
		fx, e1 := x.Float64()
		fy, e2 := y.Float64()
		return e1 == nil && e2 == nil && (fx == fy || (math.IsNaN(fx) && math.IsNaN(fy)))
	}
	return a == b
}

// keyOf is a map key for a decoded value used when counting/deduplicating.
func keyOf(v any) string {
	if s, ok := v.(string); ok {
		return "s:" + s
	}
	return "j:" + string(MarshalJSON(v))
}

func asObject(v any) (*OMap, bool) { m, ok := v.(*OMap); return m, ok }
func asList(v any) ([]any, bool)   { l, ok := v.([]any); return l, ok }

func asInt(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	if i, err := n.Int64(); err == nil {
		return i, true
	}
	if f, err := n.Float64(); err == nil && f == math.Trunc(f) {
		return int64(f), true
	}
	return 0, false
}
