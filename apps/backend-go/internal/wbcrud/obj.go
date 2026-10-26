package wbcrud

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Obj is an insertion-ordered JSON object. columns_config and config are JSON
// (not JSONB) columns, so Python preserves nested key order and the Go port
// must too. Nested objects inside a Column are *Obj.
type Obj struct {
	keys []string
	vals map[string]any
}

// NewObj returns an empty ordered object.
func NewObj() *Obj { return &Obj{vals: map[string]any{}} }

// Set stores v under k; an existing key keeps its position (dict semantics).
func (o *Obj) Set(k string, v any) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

// Get returns the value for k.
func (o *Obj) Get(k string) (any, bool) {
	if o == nil {
		return nil, false
	}
	v, ok := o.vals[k]
	return v, ok
}

// Keys returns the keys in insertion order.
func (o *Obj) Keys() []string {
	if o == nil {
		return nil
	}
	return append([]string(nil), o.keys...)
}

// Len returns the number of keys.
func (o *Obj) Len() int {
	if o == nil {
		return 0
	}
	return len(o.keys)
}

// MarshalJSON writes keys in insertion order.
func (o *Obj) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	if o == nil {
		return []byte("null"), nil
	}
	buf.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		if err := writeString(&buf, k); err != nil {
			return nil, err
		}
		buf.WriteByte(':')
		if err := writeValue(&buf, o.vals[k]); err != nil {
			return nil, err
		}
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func writeString(buf *bytes.Buffer, s string) error {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return err
	}
	buf.Write(bytes.TrimRight(b.Bytes(), "\n"))
	return nil
}

// writeValue emits v as compact JSON without HTML escaping, matching the text
// FastAPI would send.
func writeValue(buf *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case nil:
		buf.WriteString("null")
	case string:
		return writeString(buf, t)
	case bool:
		buf.WriteString(strconv.FormatBool(t))
	case json.Number:
		buf.WriteString(string(t))
	case float64:
		buf.WriteString(pyFloat(t))
	case int:
		buf.WriteString(strconv.Itoa(t))
	case int64:
		buf.WriteString(strconv.FormatInt(t, 10))
	case []any:
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeValue(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case []string:
		a := make([]any, len(t))
		for i, s := range t {
			a[i] = s
		}
		return writeValue(buf, a)
	case *Obj:
		b, err := t.MarshalJSON()
		if err != nil {
			return err
		}
		buf.Write(b)
	case Column:
		b, err := t.MarshalJSON()
		if err != nil {
			return err
		}
		buf.Write(b)
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeString(buf, k); err != nil {
				return err
			}
			buf.WriteByte(':')
			if err := writeValue(buf, t[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(t); err != nil {
			return err
		}
		buf.Write(bytes.TrimRight(b.Bytes(), "\n"))
	}
	return nil
}

// pyFloat formats like Python's repr(float).
func pyFloat(f float64) string {
	if f == 0 {
		if math.Signbit(f) {
			return "-0.0"
		}
		return "0.0"
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
	return strconv.FormatFloat(f, 'e', -1, 64)
}

// decodeOrdered reads one JSON value. Objects become *Obj, numbers json.Number.
// With canon, fractional/exponent numbers are rewritten to Python's float repr
// (what a parse + dump round trip produces).
func decodeOrdered(dec *json.Decoder, canon bool) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		if t == '{' {
			o := NewObj()
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, ok := kt.(string)
				if !ok {
					return nil, fmt.Errorf("object key is not a string")
				}
				v, err := decodeOrdered(dec, canon)
				if err != nil {
					return nil, err
				}
				o.Set(k, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return o, nil
		}
		arr := []any{}
		for dec.More() {
			v, err := decodeOrdered(dec, canon)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		return arr, nil
	case json.Number:
		if canon && strings.ContainsAny(string(t), ".eE") {
			if f, err := strconv.ParseFloat(string(t), 64); err == nil && !math.IsInf(f, 0) {
				return json.Number(pyFloat(f)), nil
			}
		}
		return t, nil
	}
	return tok, nil
}

func decodeDocument(raw []byte, canon bool) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := decodeOrdered(dec, canon)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing data after JSON value")
	}
	return v, nil
}
