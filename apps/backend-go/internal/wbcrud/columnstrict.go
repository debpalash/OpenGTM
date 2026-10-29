package wbcrud

import (
	"bytes"
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// ValidateColumnStrict is Python's
//
//	ColumnConfig.model_validate(obj, strict=True).model_dump(exclude_unset=True)
//
// used by the column settings PATCH. obj is the merged candidate
// {"name": column_id, **current, **changes}. ok is false for any validation
// error (the route answers a fixed 422 string and never reflects the input).
// Strict mode accepts no lax coercion ("3" for an int, 1 for a bool, a bool or
// string for a float; an int is accepted for a float and becomes a float), keeps
// explicit nulls on optional fields, materialises no defaults, drops unknown
// keys and emits keys in model field order.
func ValidateColumnStrict(obj *Obj) (*Obj, bool) {
	vals, errs := validateModelMode(obj, columnFields, nil, obj, true)
	if len(errs) > 0 {
		return nil, false
	}
	out := NewObj()
	for _, k := range columnOrder {
		if v, ok := vals[k]; ok {
			out.Set(k, v)
		}
	}
	return out, true
}

// IsBlank reports whether s.strip() is empty in Python (str.isspace characters,
// which include the ASCII separators 0x1c-0x1f).
func IsBlank(s string) bool {
	for _, r := range s {
		if !unicode.IsSpace(r) && (r < 0x1c || r > 0x1f) {
			return false
		}
	}
	return true
}

// DumpsSorted renders v like json.dumps(v, sort_keys=True) up to whitespace: an
// injective text for Python value equality (1 != 1.0, True != 1, key order
// ignored, list order kept). Used for the "expected" comparisons.
func DumpsSorted(v any) string {
	var b bytes.Buffer
	dumpSorted(&b, v)
	return b.String()
}

func dumpSorted(b *bytes.Buffer, v any) {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(t))
	case string:
		_ = writeString(b, t)
	case json.Number:
		b.WriteString(canonNumber(string(t)))
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
			dumpSorted(b, e)
		}
		b.WriteByte(']')
	case *Obj:
		dumpKeys(b, t.Keys(), func(k string) any { x, _ := t.Get(k); return x })
	case Column:
		dumpSorted(b, t.o)
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		dumpKeys(b, keys, func(k string) any { return t[k] })
	default:
		enc, _ := json.Marshal(t)
		b.Write(enc)
	}
}

func dumpKeys(b *bytes.Buffer, keys []string, get func(string) any) {
	sort.Strings(keys) // UTF-8 byte order == code point order, like Python's str sort
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		_ = writeString(b, k)
		b.WriteByte(':')
		dumpSorted(b, get(k))
	}
	b.WriteByte('}')
}

// canonNumber is Python's parse + dump of a JSON number: floats print as repr,
// integers as-is ("-0" is the int 0).
func canonNumber(s string) string {
	if strings.ContainsAny(s, ".eE") {
		if f, err := strconv.ParseFloat(s, 64); err == nil && !math.IsInf(f, 0) {
			return pyFloat(f)
		}
		return s
	}
	if s == "-0" {
		return "0"
	}
	return s
}
