package jobkit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode"
)

// Error carries the text Python's exception would have produced (str(exc)),
// so job errors and domain error columns stay identical across executors.
// Type, when set, is the Python exception class name (type(exc).__name__),
// for the domain columns that record "Type: message".
type Error struct {
	Type string
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

// Errorf formats a Python-compatible error message.
func Errorf(format string, args ...any) error { return &Error{Msg: fmt.Sprintf(format, args...)} }

// TypedErrorf is Errorf for an error whose Python exception class matters.
func TypedErrorf(typ, format string, args ...any) error {
	return &Error{Type: typ, Msg: fmt.Sprintf(format, args...)}
}

// Describe is f"{type(exc).__name__}: {exc}": the text handlers store when
// they record a failure. Errors that do not originate from a port of Python
// text (driver errors) get a generic class name; their message wording is a
// documented difference.
func Describe(err error) string {
	var pe *Error
	if errors.As(err, &pe) && pe.Type != "" {
		return pe.Type + ": " + pe.Msg
	}
	return "Error: " + err.Error()
}

// Object is a decoded JSON object that remembers key order, because several
// Python behaviours (error reporting, json.dumps) depend on dict order.
type Object struct {
	Keys []string
	Vals map[string]any
}

// Get returns the value for key and whether it was present.
func (o *Object) Get(key string) (any, bool) {
	if o == nil {
		return nil, false
	}
	v, ok := o.Vals[key]
	return v, ok
}

// Decode parses JSON the way json.loads does for the shapes jobs store:
// objects keep their key order (later duplicates win), numbers stay
// json.Number so int and float literals remain distinguishable.
func Decode(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := decodeAny(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("jobkit: trailing data after JSON value")
	}
	return v, nil
}

func decodeAny(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			o := &Object{Vals: map[string]any{}}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k := kt.(string)
				v, err := decodeAny(dec)
				if err != nil {
					return nil, err
				}
				if _, seen := o.Vals[k]; !seen {
					o.Keys = append(o.Keys, k)
				}
				o.Vals[k] = v
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return o, nil
		case '[':
			arr := []any{}
			for dec.More() {
				v, err := decodeAny(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return arr, nil
		}
	}
	return tok, nil // string, json.Number, bool, nil
}

// TypeName is type(value).__name__ for a decoded JSON value.
func TypeName(v any) string {
	switch t := v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case string:
		return "str"
	case json.Number:
		if IsFloatLiteral(t) {
			return "float"
		}
		return "int"
	case []any:
		return "list"
	case *Object:
		return "dict"
	}
	return "object"
}

// IsFloatLiteral reports whether the JSON number would decode as a Python
// float rather than an int.
func IsFloatLiteral(n json.Number) bool { return strings.ContainsAny(string(n), ".eE") }

// Truthy is Python truthiness for a decoded JSON value.
func Truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case json.Number:
		f, _ := strconv.ParseFloat(string(t), 64)
		return f != 0
	case []any:
		return len(t) > 0
	case *Object:
		return len(t.Keys) > 0
	}
	return true
}

// Str is `str(v) if v else ""` for a decoded payload value, the idiom
// handlers use for identifiers read from a job payload. Containers are not
// identifiers and read as empty.
func Str(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		if Truthy(t) {
			return string(t)
		}
	case bool:
		if t {
			return "True"
		}
	}
	return ""
}

// Int ports int(value) for decoded JSON values. Integers beyond int64 are
// clamped: callers range-check, and such values fail every range check.
func Int(v any) (int64, error) {
	switch t := v.(type) {
	case bool:
		if t {
			return 1, nil
		}
		return 0, nil
	case json.Number:
		if !IsFloatLiteral(t) {
			n, err := strconv.ParseInt(string(t), 10, 64)
			if err != nil {
				if strings.HasPrefix(string(t), "-") {
					return math.MinInt64, nil
				}
				return math.MaxInt64, nil
			}
			return n, nil
		}
		f, _ := strconv.ParseFloat(string(t), 64)
		if math.IsInf(f, 0) {
			return 0, Errorf("cannot convert float infinity to integer")
		}
		return ClampTrunc(f), nil
	case string:
		return IntString(t)
	}
	return 0, Errorf("int() argument must be a string, a bytes-like object or a real number, not '%s'", TypeName(v))
}

// ClampTrunc truncates toward zero, saturating at the int64 range.
func ClampTrunc(f float64) int64 {
	f = math.Trunc(f)
	switch {
	case f >= math.MaxInt64:
		return math.MaxInt64
	case f <= math.MinInt64:
		return math.MinInt64
	}
	return int64(f)
}

// IntString ports int(str): surrounding whitespace is ignored, an optional
// sign, digits with single underscores allowed between digits.
//
// Divergence: Python also accepts non-ASCII Unicode decimal digits; they are
// rejected here (the API never produces them).
func IntString(s string) (int64, error) {
	bad := func() (int64, error) {
		return 0, Errorf("invalid literal for int() with base 10: %s", Repr(s))
	}
	t := strings.TrimFunc(s, unicode.IsSpace)
	neg := false
	switch {
	case strings.HasPrefix(t, "-"):
		neg, t = true, t[1:]
	case strings.HasPrefix(t, "+"):
		t = t[1:]
	}
	if t == "" {
		return bad()
	}
	var digits []rune
	prevUnderscore := true // an underscore may not lead
	for _, r := range t {
		switch {
		case r == '_':
			if prevUnderscore {
				return bad()
			}
			prevUnderscore = true
		case r >= '0' && r <= '9':
			digits = append(digits, r)
			prevUnderscore = false
		default:
			return bad()
		}
	}
	if prevUnderscore { // trailing underscore
		return bad()
	}
	n, overflow := int64(0), false
	for _, d := range digits {
		if n > (math.MaxInt64-int64(d-'0'))/10 {
			overflow = true
			break
		}
		n = n*10 + int64(d-'0')
	}
	if overflow {
		n = math.MaxInt64
	}
	if neg {
		n = -n
	}
	return n, nil
}

// Repr is repr(str) for the characters that occur in error messages.
func Repr(s string) string {
	quote := byte('\'')
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for _, r := range s {
		switch {
		case r == rune(quote) || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case !unicode.IsPrint(r) && r > 0x7f:
			if r > 0xffff {
				fmt.Fprintf(&b, `\U%08x`, r)
			} else if r > 0xff {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				fmt.Fprintf(&b, `\x%02x`, r)
			}
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}

// TruncateChars is s[:n] for a Python str: it counts characters, not bytes.
func TruncateChars(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
