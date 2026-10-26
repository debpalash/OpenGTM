package colcompute

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// A JSON reader and writer with Python's json module semantics: object key order is kept,
// ints are arbitrary precision, NaN/Infinity are accepted, control characters in strings
// are rejected, and json.dumps defaults (", " and ": " separators, ensure_ascii) apply.

const maxJSONDepth = 500

var errJSON = errors.New("invalid JSON")

type jsonReader struct {
	s     string
	i     int
	depth int
}

// parseJSON is json.loads for a UTF-8 document.
func parseJSON(b []byte) (any, error) {
	if !utf8.Valid(b) {
		return nil, errJSON
	}
	r := &jsonReader{s: string(b)}
	r.ws()
	v, err := r.value()
	if err != nil {
		return nil, err
	}
	r.ws()
	if r.i != len(r.s) {
		return nil, errJSON
	}
	return v, nil
}

func (r *jsonReader) ws() {
	for r.i < len(r.s) && (r.s[r.i] == ' ' || r.s[r.i] == '\t' || r.s[r.i] == '\n' || r.s[r.i] == '\r') {
		r.i++
	}
}

func (r *jsonReader) lit(word string, v any) (any, error) {
	if strings.HasPrefix(r.s[r.i:], word) {
		r.i += len(word)
		return v, nil
	}
	return nil, errJSON
}

func (r *jsonReader) value() (any, error) {
	if r.i >= len(r.s) {
		return nil, errJSON
	}
	switch c := r.s[r.i]; {
	case c == '{':
		return r.object()
	case c == '[':
		return r.array()
	case c == '"':
		return r.str()
	case c == 'n':
		return r.lit("null", nil)
	case c == 't':
		return r.lit("true", true)
	case c == 'f':
		return r.lit("false", false)
	case c == 'N':
		return r.lit("NaN", math.NaN())
	case c == 'I':
		return r.lit("Infinity", math.Inf(1))
	case c == '-' && strings.HasPrefix(r.s[r.i:], "-Infinity"):
		r.i += len("-Infinity")
		return math.Inf(-1), nil
	case c == '-' || c >= '0' && c <= '9':
		return r.number()
	}
	return nil, errJSON
}

func (r *jsonReader) number() (any, error) {
	start := r.i
	if r.s[r.i] == '-' {
		r.i++
	}
	digits := func() int {
		n := 0
		for r.i < len(r.s) && r.s[r.i] >= '0' && r.s[r.i] <= '9' {
			r.i++
			n++
		}
		return n
	}
	switch {
	case r.i < len(r.s) && r.s[r.i] == '0':
		r.i++
	case digits() == 0:
		return nil, errJSON
	}
	isFloat := false
	if r.i+1 < len(r.s) && r.s[r.i] == '.' && r.s[r.i+1] >= '0' && r.s[r.i+1] <= '9' {
		r.i++
		digits()
		isFloat = true
	}
	if r.i < len(r.s) && (r.s[r.i] == 'e' || r.s[r.i] == 'E') {
		save := r.i
		r.i++
		if r.i < len(r.s) && (r.s[r.i] == '+' || r.s[r.i] == '-') {
			r.i++
		}
		if digits() == 0 {
			r.i = save
		} else {
			isFloat = true
		}
	}
	text := r.s[start:r.i]
	if isFloat {
		f, err := strconv.ParseFloat(text, 64)
		if err != nil && !math.IsInf(f, 0) {
			return nil, errJSON
		}
		return f, nil
	}
	if len(text) > maxStrDigits {
		return nil, errJSON
	}
	i, ok := new(big.Int).SetString(text, 10)
	if !ok {
		return nil, errJSON
	}
	return i, nil
}

func (r *jsonReader) str() (string, error) {
	r.i++ // opening quote
	var b strings.Builder
	for {
		if r.i >= len(r.s) {
			return "", errJSON
		}
		c := r.s[r.i]
		switch {
		case c == '"':
			r.i++
			return b.String(), nil
		case c < 0x20:
			return "", errJSON
		case c != '\\':
			b.WriteByte(c)
			r.i++
		default:
			r.i++
			if r.i >= len(r.s) {
				return "", errJSON
			}
			e := r.s[r.i]
			r.i++
			switch e {
			case '"', '\\', '/':
				b.WriteByte(e)
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'u':
				u, ok := r.hex4()
				if !ok {
					return "", errJSON
				}
				if utf16.IsSurrogate(rune(u)) {
					if u < 0xdc00 && strings.HasPrefix(r.s[r.i:], "\\u") {
						save := r.i
						r.i += 2
						if lo, ok := r.hex4(); ok && lo >= 0xdc00 && lo <= 0xdfff {
							b.WriteRune(utf16.DecodeRune(rune(u), rune(lo)))
							continue
						}
						r.i = save
					}
					b.WriteRune(utf8.RuneError) // lone surrogate: a Go string cannot hold one
					continue
				}
				b.WriteRune(rune(u))
			default:
				return "", errJSON
			}
		}
	}
}

func (r *jsonReader) hex4() (int, bool) {
	if r.i+4 > len(r.s) {
		return 0, false
	}
	v, err := strconv.ParseUint(r.s[r.i:r.i+4], 16, 32)
	if err != nil {
		return 0, false
	}
	r.i += 4
	return int(v), true
}

func (r *jsonReader) array() (any, error) {
	if r.depth++; r.depth > maxJSONDepth {
		return nil, errJSON
	}
	defer func() { r.depth-- }()
	r.i++
	out := []any{}
	r.ws()
	if r.i < len(r.s) && r.s[r.i] == ']' {
		r.i++
		return out, nil
	}
	for {
		r.ws()
		v, err := r.value()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		r.ws()
		if r.i >= len(r.s) {
			return nil, errJSON
		}
		switch r.s[r.i] {
		case ',':
			r.i++
		case ']':
			r.i++
			return out, nil
		default:
			return nil, errJSON
		}
	}
}

func (r *jsonReader) object() (any, error) {
	if r.depth++; r.depth > maxJSONDepth {
		return nil, errJSON
	}
	defer func() { r.depth-- }()
	r.i++
	d := &pyDict{vals: map[string]any{}}
	r.ws()
	if r.i < len(r.s) && r.s[r.i] == '}' {
		r.i++
		return d, nil
	}
	for {
		r.ws()
		if r.i >= len(r.s) || r.s[r.i] != '"' {
			return nil, errJSON
		}
		k, err := r.str()
		if err != nil {
			return nil, err
		}
		r.ws()
		if r.i >= len(r.s) || r.s[r.i] != ':' {
			return nil, errJSON
		}
		r.i++
		r.ws()
		v, err := r.value()
		if err != nil {
			return nil, err
		}
		if _, dup := d.vals[k]; !dup {
			d.keys = append(d.keys, k)
		}
		d.vals[k] = v
		r.ws()
		if r.i >= len(r.s) {
			return nil, errJSON
		}
		switch r.s[r.i] {
		case ',':
			r.i++
		case '}':
			r.i++
			return d, nil
		default:
			return nil, errJSON
		}
	}
}

// jsonDumps is json.dumps. compact selects separators (",", ":") and asciiOnly is
// ensure_ascii.
func jsonDumps(v any, compact, asciiOnly bool) (string, error) {
	var b strings.Builder
	if err := dumpJSON(&b, v, compact, asciiOnly, 0); err != nil {
		return "", err
	}
	return b.String(), nil
}

func dumpJSONString(b *strings.Builder, s string, asciiOnly bool) {
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
		case r < 0x20:
			fmt.Fprintf(b, `\u%04x`, r)
		case asciiOnly && (r == 0x7f || r > 0x7e):
			if r > 0xffff {
				hi, lo := utf16.EncodeRune(r)
				fmt.Fprintf(b, `\u%04x\u%04x`, hi, lo)
			} else {
				fmt.Fprintf(b, `\u%04x`, r)
			}
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
}

func dumpJSON(b *strings.Builder, v any, compact, asciiOnly bool, depth int) error {
	if depth > maxJSONDepth {
		return errors.New("maximum recursion depth exceeded")
	}
	itemSep, keySep := ", ", ": "
	if compact {
		itemSep, keySep = ",", ":"
	}
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case *big.Int:
		b.WriteString(intStr(x))
	case float64:
		switch {
		case math.IsNaN(x):
			if compact {
				return errors.New("Out of range float values are not JSON compliant")
			}
			b.WriteString("NaN")
		case math.IsInf(x, 1):
			if compact {
				return errors.New("Out of range float values are not JSON compliant")
			}
			b.WriteString("Infinity")
		case math.IsInf(x, -1):
			if compact {
				return errors.New("Out of range float values are not JSON compliant")
			}
			b.WriteString("-Infinity")
		default:
			b.WriteString(floatRepr(x))
		}
	case string:
		dumpJSONString(b, x, asciiOnly)
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteString(itemSep)
			}
			if err := dumpJSON(b, e, compact, asciiOnly, depth+1); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case *pyDict:
		b.WriteByte('{')
		for i, k := range x.keys {
			if i > 0 {
				b.WriteString(itemSep)
			}
			dumpJSONString(b, k, asciiOnly)
			b.WriteString(keySep)
			if err := dumpJSON(b, x.vals[k], compact, asciiOnly, depth+1); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		dumpJSONString(b, pyStr(v), asciiOnly) // json.dumps(default=str)
	}
	return nil
}

// ---- JSONPath-lite (declarative.template._walk_path / project_value) ----

// walkJSONPath walks `key`, `key[]` (first non-empty), `key[2]` and `[2]` segments.
func walkJSONPath(data any, path string) any {
	cur := data
	parts := strings.Split(path, ".")
	for position, raw := range parts {
		if cur == nil {
			return nil
		}
		key, idx, hasIdx, ok := splitPathToken(raw)
		if !ok {
			return nil
		}
		if key != "" {
			d, isDict := cur.(*pyDict)
			if !isDict {
				cur = nil
			} else {
				cur = d.vals[key]
			}
		}
		if !hasIdx {
			continue
		}
		list, isList := cur.([]any)
		if !isList {
			return nil
		}
		if idx == "" {
			remainder := strings.Join(parts[position+1:], ".")
			for _, item := range list {
				v := item
				if remainder != "" {
					v = walkJSONPath(item, remainder)
				}
				if v != nil && !pyEq(v, "") {
					return v
				}
			}
			return nil
		}
		i, err := strconv.Atoi(idx)
		if err != nil || i < 0 || i >= len(list) {
			cur = nil
		} else {
			cur = list[i]
		}
	}
	return cur
}

// splitPathToken is re.match(r"^([A-Za-z0-9_]*)(?:\[(\d*)\])?$", raw) ($ also matches
// before one trailing newline).
func splitPathToken(raw string) (key, idx string, hasIdx, ok bool) {
	raw = strings.TrimSuffix(raw, "\n")
	i := 0
	for i < len(raw) && (raw[i] == '_' || raw[i] >= '0' && raw[i] <= '9' || raw[i] >= 'a' && raw[i] <= 'z' || raw[i] >= 'A' && raw[i] <= 'Z') {
		i++
	}
	key = raw[:i]
	rest := raw[i:]
	if rest == "" {
		return key, "", false, true
	}
	if len(rest) < 2 || rest[0] != '[' || rest[len(rest)-1] != ']' {
		return "", "", false, false
	}
	idx = rest[1 : len(rest)-1]
	for j := 0; j < len(idx); j++ {
		if idx[j] < '0' || idx[j] > '9' {
			return "", "", false, false
		}
	}
	return key, idx, true, true
}

// projectValue is project_value: no "$." means a static literal; text before "$." is a
// literal prefix.
func projectValue(data any, expr string) any {
	i := strings.Index(expr, "$.")
	if i == -1 {
		return expr
	}
	prefix, path := expr[:i], expr[i+2:]
	val := data
	if path != "" {
		val = walkJSONPath(data, path)
	}
	if val == nil {
		return nil
	}
	if prefix != "" {
		return prefix + pyStr(val)
	}
	return val
}
