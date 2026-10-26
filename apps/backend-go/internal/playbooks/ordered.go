package playbooks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
)

// obj is an insertion-ordered JSON object: request bodies are echoed back in
// 422 "input" fields and result metadata is re-emitted, both with the key order
// Python's dict preserves.
type obj struct {
	keys []string
	vals map[string]any
}

func newObj() *obj { return &obj{vals: map[string]any{}} }

func (o *obj) set(k string, v any) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v // duplicate key: last value wins, first position (dict semantics)
}

func (o *obj) get(k string) (any, bool) { v, ok := o.vals[k]; return v, ok }

// MarshalJSON writes compact JSON in insertion order without HTML escaping.
func (o *obj) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	writeValue(&b, o)
	return b.Bytes(), nil
}

// maxDepth bounds nesting so a hostile body cannot exhaust the stack (Python
// would answer 500 with RecursionError at ~1000 levels; we answer 422).
const maxDepth = 200

type tooDeepError struct{}

func (tooDeepError) Error() string { return "json nesting too deep" }

// decodeDoc parses one JSON document. Objects become *obj, arrays []any,
// numbers json.Number (fractional/exponent forms rewritten to Python's float
// repr, what a parse + dump round trip produces).
func decodeDoc(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := decodeValue(dec, 0)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing data after JSON value")
	}
	return v, nil
}

func decodeValue(dec *json.Decoder, depth int) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		if depth >= maxDepth {
			return nil, tooDeepError{}
		}
		if t == '{' {
			o := newObj()
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, ok := kt.(string)
				if !ok {
					return nil, fmt.Errorf("object key is not a string")
				}
				v, err := decodeValue(dec, depth+1)
				if err != nil {
					return nil, err
				}
				o.set(k, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return o, nil
		}
		arr := []any{}
		for dec.More() {
			v, err := decodeValue(dec, depth+1)
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
		if strings.ContainsAny(string(t), ".eE") {
			if f, err := strconv.ParseFloat(string(t), 64); err == nil && !math.IsInf(f, 0) {
				return json.Number(pyFloat(f)), nil
			}
		}
		return t, nil
	}
	return tok, nil
}

// writeString writes s like json.dumps(s, ensure_ascii=False): only the quote,
// the backslash and control characters are escaped.
func writeString(b *bytes.Buffer, s string) {
	const hex = "0123456789abcdef"
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			b.WriteString(`\"`)
		case c == '\\':
			b.WriteString(`\\`)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\r':
			b.WriteString(`\r`)
		case c == '\t':
			b.WriteString(`\t`)
		case c == '\b':
			b.WriteString(`\b`)
		case c == '\f':
			b.WriteString(`\f`)
		case c < 0x20:
			b.WriteString(`\u00`)
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0xf])
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
}

// pyFloat formats like Python's repr(float).
func pyFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case f == 0:
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
	// Python: 1e+16, 1.5e-07 (two-digit exponent minimum, explicit sign).
	m := strconv.FormatFloat(f, 'e', -1, 64)
	i := strings.IndexByte(m, 'e')
	mant, ex := m[:i], m[i+1:]
	sign := ex[0]
	digits := ex[1:]
	if len(digits) < 2 {
		digits = "0" + digits
	}
	return mant + "e" + string(sign) + digits
}

// writeValue emits v as compact JSON without HTML escaping.
func writeValue(b *bytes.Buffer, v any) {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case string:
		writeString(b, t)
	case bool:
		b.WriteString(strconv.FormatBool(t))
	case json.Number:
		b.WriteString(string(t))
	case float64:
		b.WriteString(pyFloat(t))
	case int:
		b.WriteString(strconv.Itoa(t))
	case int64:
		b.WriteString(strconv.FormatInt(t, 10))
	case []any:
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			writeValue(b, e)
		}
		b.WriteByte(']')
	case *obj:
		if t == nil {
			b.WriteString("null")
			return
		}
		b.WriteByte('{')
		for i, k := range t.keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeString(b, k)
			b.WriteByte(':')
			writeValue(b, t.vals[k])
		}
		b.WriteByte('}')
	default:
		enc := json.NewEncoder(b)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(t)
		// Encode appends a newline.
		if n := b.Len(); n > 0 && b.Bytes()[n-1] == '\n' {
			b.Truncate(n - 1)
		}
	}
}

// raw returns v as a pre-rendered JSON value suitable for httpx.FieldError.Input
// (so the 422 body is emitted with Python's number and string formatting).
func raw(v any) json.RawMessage {
	var b bytes.Buffer
	writeValue(&b, v)
	return json.RawMessage(b.Bytes())
}

// isoNaive renders a naive-UTC column value like datetime.isoformat().
func isoNaive(t time.Time) string {
	t = t.UTC()
	s := t.Format("2006-01-02T15:04:05")
	if us := t.Nanosecond() / 1000; us != 0 {
		s += fmt.Sprintf(".%06d", us)
	}
	return s
}
