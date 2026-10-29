// Package pyjson renders values byte-identically to Python's
// json.dumps(v, indent=N or None, default=str) with the default
// ensure_ascii=True and allow_nan=True. MCP tool payloads are wire parity.
package pyjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
)

// Obj is an insertion-ordered JSON object (a Python dict).
type Obj struct {
	Keys []string
	Vals map[string]any
}

// NewObj builds an Obj from alternating key, value arguments. A repeated key
// keeps its first position and takes the last value, like a Python dict.
// Non-string keys or an odd count are programmer errors and panic.
func NewObj(kv ...any) *Obj {
	if len(kv)%2 != 0 {
		panic("pyjson.NewObj: odd argument count")
	}
	o := &Obj{Vals: make(map[string]any, len(kv)/2)}
	for i := 0; i < len(kv); i += 2 {
		k, ok := kv[i].(string)
		if !ok {
			panic("pyjson.NewObj: key is not a string")
		}
		o.Set(k, kv[i+1])
	}
	return o
}

// Set assigns a key, appending it when new.
func (o *Obj) Set(k string, v any) {
	if o.Vals == nil {
		o.Vals = map[string]any{}
	}
	if _, ok := o.Vals[k]; !ok {
		o.Keys = append(o.Keys, k)
	}
	o.Vals[k] = v
}

// maxDepth matches the depth limit of canonjson and wbcrud.
const maxDepth = canonjson.MaxDepth

// Dumps encodes v like Python json.dumps(v, indent=indent or None,
// default=str). Supported: *Obj, []any, string, bool, nil, int64, float64,
// json.RawMessage (re-parsed, key order kept) and time.Time (str(datetime)).
// Anything else is an error rather than a guess.
func Dumps(v any, indent int) ([]byte, error) {
	var b bytes.Buffer
	if err := write(&b, v, indent, 0); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func newline(b *bytes.Buffer, indent, depth int) {
	b.WriteByte('\n')
	for i := 0; i < indent*depth; i++ {
		b.WriteByte(' ')
	}
}

func write(b *bytes.Buffer, v any, indent, depth int) error {
	if depth > maxDepth {
		return errors.New("pyjson: nesting too deep")
	}
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case float64:
		switch {
		case math.IsNaN(x):
			b.WriteString("NaN")
		case math.IsInf(x, 1):
			b.WriteString("Infinity")
		case math.IsInf(x, -1):
			b.WriteString("-Infinity")
		default:
			s, err := canonjson.FloatRepr(x)
			if err != nil {
				return err
			}
			b.WriteString(s)
		}
	case string:
		return writeString(b, x)
	case time.Time:
		return writeString(b, pyDatetime(x))
	case json.RawMessage:
		tree, err := decodeOrdered(x)
		if err != nil {
			return err
		}
		return write(b, tree, indent, depth)
	case []any:
		if x == nil {
			b.WriteString("null")
			return nil
		}
		if len(x) == 0 {
			b.WriteString("[]")
			return nil
		}
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
				if indent == 0 {
					b.WriteByte(' ')
				}
			}
			if indent > 0 {
				newline(b, indent, depth+1)
			}
			if err := write(b, e, indent, depth+1); err != nil {
				return err
			}
		}
		if indent > 0 {
			newline(b, indent, depth)
		}
		b.WriteByte(']')
	case *Obj:
		if x == nil {
			b.WriteString("null")
			return nil
		}
		if len(x.Keys) == 0 {
			b.WriteString("{}")
			return nil
		}
		b.WriteByte('{')
		for i, k := range x.Keys {
			val, ok := x.Vals[k]
			if !ok {
				return fmt.Errorf("pyjson: key %q has no value", k)
			}
			if i > 0 {
				b.WriteByte(',')
				if indent == 0 {
					b.WriteByte(' ')
				}
			}
			if indent > 0 {
				newline(b, indent, depth+1)
			}
			if err := writeString(b, k); err != nil {
				return err
			}
			b.WriteString(": ")
			if err := write(b, val, indent, depth+1); err != nil {
				return err
			}
		}
		if indent > 0 {
			newline(b, indent, depth)
		}
		b.WriteByte('}')
	default:
		return fmt.Errorf("pyjson: unsupported type %T", v)
	}
	return nil
}

const hexdigits = "0123456789abcdef"

func writeString(b *bytes.Buffer, s string) error {
	if !utf8.ValidString(s) {
		return errors.New("pyjson: invalid UTF-8 string")
	}
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
		case r >= ' ' && r < 0x7f:
			b.WriteRune(r)
		case r < 0x10000:
			writeU(b, uint32(r))
		default:
			r -= 0x10000
			writeU(b, 0xd800+uint32(r>>10))
			writeU(b, 0xdc00+uint32(r&0x3ff))
		}
	}
	b.WriteByte('"')
	return nil
}

func writeU(b *bytes.Buffer, u uint32) {
	b.WriteString(`\u`)
	for _, sh := range [...]uint{12, 8, 4, 0} {
		b.WriteByte(hexdigits[u>>sh&0xf])
	}
}

// pyDatetime renders str(aware datetime): "YYYY-MM-DD HH:MM:SS[.ffffff]+HH:MM".
// Go times are always zone-aware, so the naive form cannot occur.
func pyDatetime(t time.Time) string {
	s := t.Format("2006-01-02 15:04:05")
	if us := t.Nanosecond() / 1000; us != 0 {
		s += "." + fmt.Sprintf("%06d", us)
	}
	_, off := t.Zone()
	sign := byte('+')
	if off < 0 {
		sign, off = '-', -off
	}
	s += fmt.Sprintf("%c%02d:%02d", sign, off/3600, off%3600/60)
	if sec := off % 60; sec != 0 {
		s += fmt.Sprintf(":%02d", sec)
	}
	return s
}

// decodeOrdered parses JSON into the Dumps tree keeping object key order
// (canonjson.Decode returns Go maps, which lose it). Numbers with '.', 'e' or
// 'E' are float64, the rest int64 (overflow is an error), as in canonjson.
func decodeOrdered(raw []byte) (any, error) {
	if !utf8.Valid(raw) {
		return nil, errors.New("pyjson: invalid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := decodeValue(dec, 0)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("pyjson: trailing data")
	}
	return v, nil
}

func decodeValue(dec *json.Decoder, depth int) (any, error) {
	if depth > maxDepth {
		return nil, errors.New("pyjson: nesting too deep")
	}
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		if t == '[' {
			arr := []any{}
			for dec.More() {
				e, err := decodeValue(dec, depth+1)
				if err != nil {
					return nil, err
				}
				arr = append(arr, e)
			}
			_, err := dec.Token()
			return arr, err
		}
		o := NewObj()
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			e, err := decodeValue(dec, depth+1)
			if err != nil {
				return nil, err
			}
			o.Set(kt.(string), e)
		}
		_, err := dec.Token()
		return o, err
	case json.Number:
		s := t.String()
		if bytes.ContainsAny([]byte(s), ".eE") {
			f, err := strconv.ParseFloat(s, 64)
			if err != nil {
				return nil, err
			}
			return f, nil
		}
		i, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("pyjson: integer %s out of int64 range", s)
		}
		return i, nil
	default: // string, bool, nil
		return tok, nil
	}
}
