// Package canonjson renders values byte-identically to Python's
// json.dumps(v, sort_keys=..., separators=(",", ":"), ensure_ascii=True,
// allow_nan=False). Contract hashes and attempt keys are SHA-256 digests of
// these bytes, so any divergence silently breaks cross-executor idempotency.
package canonjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Marshal accepts only []any, map[string]any, string, float64, int64, bool and
// nil (typed nil maps/slices render as null). Go maps have no insertion order, so with
// sortKeys=false map keys are still emitted in sorted order (deterministic). NaN, Inf and invalid UTF-8
// (Python would need lone surrogates) are rejected.
func Marshal(v any, sortKeys bool) ([]byte, error) {
	var b bytes.Buffer
	if err := write(&b, v, sortKeys); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func write(b *bytes.Buffer, v any, sortKeys bool) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case float64:
		s, err := FloatRepr(x)
		if err != nil {
			return err
		}
		b.WriteString(s)
	case string:
		return writeString(b, x)
	case []any:
		if x == nil {
			b.WriteString("null")
			return nil
		}
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := write(b, e, sortKeys); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case map[string]any:
		if x == nil {
			b.WriteString("null")
			return nil
		}
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		// Go compares UTF-8 bytes, which orders identically to Python's code
		// point comparison. Sorted even when !sortKeys: map order is random.
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeString(b, k); err != nil {
				return err
			}
			b.WriteByte(':')
			if err := write(b, x[k], sortKeys); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		return fmt.Errorf("canonjson: unsupported type %T", v)
	}
	return nil
}

const hexdigits = "0123456789abcdef"

func writeString(b *bytes.Buffer, s string) error {
	if !utf8.ValidString(s) {
		return errors.New("canonjson: invalid UTF-8 string")
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
	return nil
}

func writeU(b *bytes.Buffer, u uint32) {
	b.WriteString(`\u`)
	b.WriteByte(hexdigits[u>>12&0xf])
	b.WriteByte(hexdigits[u>>8&0xf])
	b.WriteByte(hexdigits[u>>4&0xf])
	b.WriteByte(hexdigits[u&0xf])
}

// FloatRepr formats like Python's repr(float): shortest round-trip digits,
// fixed notation when -4 <= exp10 < 16, otherwise d[.ddd]e[+-]XX.
func FloatRepr(f float64) (string, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "", errors.New("canonjson: NaN/Inf not allowed")
	}
	if f == 0 {
		if math.Signbit(f) {
			return "-0.0", nil
		}
		return "0.0", nil
	}
	e := strconv.FormatFloat(f, 'e', -1, 64) // [-]d[.ddd]e[+-]XX
	sign := ""
	if e[0] == '-' {
		sign, e = "-", e[1:]
	}
	mant, expStr, _ := strings.Cut(e, "e")
	exp, _ := strconv.Atoi(expStr)
	digits := strings.Replace(mant, ".", "", 1)
	if exp < -4 || exp >= 16 {
		out := digits[:1]
		if len(digits) > 1 {
			out += "." + digits[1:]
		}
		es := "+"
		if exp < 0 {
			es, exp = "-", -exp
		}
		return fmt.Sprintf("%s%se%s%02d", sign, out, es, exp), nil
	}
	switch {
	case exp < 0:
		return sign + "0." + strings.Repeat("0", -exp-1) + digits, nil
	case len(digits) <= exp+1:
		return sign + digits + strings.Repeat("0", exp+1-len(digits)) + ".0", nil
	default:
		return sign + digits[:exp+1] + "." + digits[exp+1:], nil
	}
}

// Decode parses JSON into the shapes Marshal accepts. Numbers with '.', 'e'
// or 'E' are float64 (as in Python); the rest must fit int64.
func Decode(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := convert(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("canonjson: trailing data")
	}
	return v, nil
}

func convert(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		if t == '[' {
			out := []any{}
			for dec.More() {
				e, err := convert(dec)
				if err != nil {
					return nil, err
				}
				out = append(out, e)
			}
			_, err := dec.Token()
			return out, err
		}
		out := map[string]any{}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			e, err := convert(dec)
			if err != nil {
				return nil, err
			}
			out[kt.(string)] = e
		}
		_, err := dec.Token()
		return out, err
	case json.Number:
		s := t.String()
		if strings.ContainsAny(s, ".eE") {
			return strconv.ParseFloat(s, 64)
		}
		return strconv.ParseInt(s, 10, 64)
	default: // string, bool, nil
		return tok, nil
	}
}
