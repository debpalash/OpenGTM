package leadprogress

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
)

// Field is one key/value pair of an ordered JSON object.
type Field struct {
	Key string
	Val any
}

// Obj is an insertion-ordered JSON object, like a Python dict. Go maps have
// no order, and Python's json.dumps emits keys in insertion order, so events
// whose bytes must match Python are built from Obj.
type Obj []Field

// set assigns like a Python dict: an existing key keeps its position.
func (o Obj) set(k string, v any) Obj {
	for i := range o {
		if o[i].Key == k {
			o[i].Val = v
			return o
		}
	}
	return append(o, Field{k, v})
}

func (o Obj) get(k string) (any, bool) {
	for _, f := range o {
		if f.Key == k {
			return f.Val, true
		}
	}
	return nil, false
}

// toMap converts to a plain map (last duplicate wins).
func (o Obj) toMap() map[string]any {
	m := make(map[string]any, len(o))
	for _, f := range o {
		m[f.Key] = f.Val
	}
	return m
}

// objFromMap orders a map's keys sorted (deterministic; Go has no map order).
func objFromMap(m map[string]any) Obj {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	o := make(Obj, 0, len(keys))
	for _, k := range keys {
		o = append(o, Field{k, m[k]})
	}
	return o
}

// dumps renders v like Python json.dumps(v, default=str, separators=(",",":")):
// ensure_ascii, allow_nan (bare NaN/Infinity), Python float repr, insertion
// order for Obj (map[string]any keys are sorted). Values json cannot express
// are stringified like default=str.
func dumps(v any) []byte {
	var b bytes.Buffer
	writeValue(&b, v, 0)
	return b.Bytes()
}

const maxEncodeDepth = 200

func writeValue(b *bytes.Buffer, v any, depth int) {
	if depth > maxEncodeDepth {
		writeString(b, "<max depth>")
		return
	}
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case string:
		writeString(b, x)
	case json.Number:
		writeNumber(b, x)
	case float64:
		writeFloat(b, x)
	case float32:
		f, _ := strconv.ParseFloat(strconv.FormatFloat(float64(x), 'g', -1, 32), 64)
		writeFloat(b, f)
	case int:
		b.WriteString(strconv.FormatInt(int64(x), 10))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case Obj:
		writeObj(b, x, depth)
	case map[string]any:
		if x == nil {
			b.WriteString("null")
			return
		}
		writeObj(b, objFromMap(x), depth)
	case []any:
		if x == nil {
			b.WriteString("null")
			return
		}
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			writeValue(b, e, depth+1)
		}
		b.WriteByte(']')
	case time.Time:
		writeString(b, pyDatetime(x))
	case []byte:
		writeString(b, "b'"+string(x)+"'")
	default:
		writeReflect(b, v, depth)
	}
}

func writeReflect(b *bytes.Buffer, v any, depth int) {
	switch x := v.(type) {
	case error:
		writeString(b, x.Error())
		return
	case fmt.Stringer:
		writeString(b, x.String())
		return
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Ptr, reflect.Interface:
		if rv.IsNil() {
			b.WriteString("null")
			return
		}
		if _, ok := v.(fmt.Stringer); !ok {
			writeValue(b, rv.Elem().Interface(), depth+1)
			return
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		b.WriteString(strconv.FormatInt(rv.Int(), 10))
		return
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		b.WriteString(strconv.FormatUint(rv.Uint(), 10))
		return
	case reflect.Bool:
		b.WriteString(strconv.FormatBool(rv.Bool()))
		return
	case reflect.Float32, reflect.Float64:
		writeFloat(b, rv.Float())
		return
	case reflect.Slice, reflect.Array:
		if rv.Kind() == reflect.Slice && rv.IsNil() {
			b.WriteString("null")
			return
		}
		if _, ok := v.(fmt.Stringer); !ok {
			b.WriteByte('[')
			for i := 0; i < rv.Len(); i++ {
				if i > 0 {
					b.WriteByte(',')
				}
				writeValue(b, rv.Index(i).Interface(), depth+1)
			}
			b.WriteByte(']')
			return
		}
	case reflect.Map:
		if rv.IsNil() {
			b.WriteString("null")
			return
		}
		if _, ok := v.(fmt.Stringer); !ok && rv.Type().Key().Kind() == reflect.String {
			m := make(map[string]any, rv.Len())
			for _, k := range rv.MapKeys() {
				m[k.String()] = rv.MapIndex(k).Interface()
			}
			writeValue(b, m, depth+1)
			return
		}
	case reflect.String:
		if _, ok := v.(fmt.Stringer); !ok {
			writeString(b, rv.String())
			return
		}
	}
	// default=str
	writeString(b, fmt.Sprint(v))
}

func writeObj(b *bytes.Buffer, o Obj, depth int) {
	// A Python dict holds each key once: a duplicate keeps its first position
	// and takes the last value.
	seen := make(map[string]int, len(o))
	uniq := make(Obj, 0, len(o))
	for _, f := range o {
		if i, ok := seen[f.Key]; ok {
			uniq[i].Val = f.Val
			continue
		}
		seen[f.Key] = len(uniq)
		uniq = append(uniq, f)
	}
	b.WriteByte('{')
	for i, f := range uniq {
		if i > 0 {
			b.WriteByte(',')
		}
		writeString(b, f.Key)
		b.WriteByte(':')
		writeValue(b, f.Val, depth+1)
	}
	b.WriteByte('}')
}

func writeNumber(b *bytes.Buffer, n json.Number) {
	s := n.String()
	if strings.ContainsAny(s, ".eE") {
		if f, err := strconv.ParseFloat(s, 64); err == nil && !math.IsInf(f, 0) {
			writeFloat(b, f)
			return
		}
	}
	b.WriteString(s)
}

func writeFloat(b *bytes.Buffer, f float64) {
	switch {
	case math.IsNaN(f):
		b.WriteString("NaN")
	case math.IsInf(f, 1):
		b.WriteString("Infinity")
	case math.IsInf(f, -1):
		b.WriteString("-Infinity")
	default:
		s, _ := canonjson.FloatRepr(f)
		b.WriteString(s)
	}
}

const hexdigits = "0123456789abcdef"

func writeU(b *bytes.Buffer, u uint32) {
	b.WriteString(`\u`)
	b.WriteByte(hexdigits[u>>12&0xf])
	b.WriteByte(hexdigits[u>>8&0xf])
	b.WriteByte(hexdigits[u>>4&0xf])
	b.WriteByte(hexdigits[u&0xf])
}

// writeString is json.dumps' ensure_ascii string. Invalid UTF-8 (which a
// Python str cannot hold) becomes U+FFFD.
func writeString(b *bytes.Buffer, s string) {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
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
		case r >= ' ' && r <= '~':
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
}

// pyDatetime renders like str(datetime): "YYYY-MM-DD HH:MM:SS[.ffffff]+HH:MM".
func pyDatetime(t time.Time) string {
	s := t.Format("2006-01-02 15:04:05")
	if us := t.Nanosecond() / 1000; us != 0 {
		s += fmt.Sprintf(".%06d", us)
	}
	return s + t.Format("-07:00")
}
