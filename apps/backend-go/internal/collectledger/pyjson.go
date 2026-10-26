package collectledger

import (
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
)

// ErrNotRenderable marks a stored JSON document that Python's json.loads accepts
// but that its response render rejects (NaN/Infinity/overflowing floats, which
// json.dumps(allow_nan=False) refuses, or a lone surrogate, which cannot be
// UTF-8 encoded). The route answers 500 for these, exactly as FastAPI does.
var ErrNotRenderable = errors.New("collectledger: stored JSON is not renderable")

// maxNesting stands in for CPython's recursion limit: deeper documents make
// json.loads raise RecursionError, which the route swallows (-> {}). The exact
// CPython threshold depends on the stack depth at the call, so this is approximate.
const maxNesting = 900

// maxIntDigits is CPython's int/str conversion limit (4300): longer integer
// literals make json.loads raise ValueError (-> {}).
const maxIntDigits = 4300

// ParseDetails is `json.loads(text)` followed by the response render, for the
// intent_details and stage details columns: any parse failure (empty string,
// trailing data, BOM, ...) yields `{}`; otherwise the parsed value is re-rendered
// compactly in Python's shapes (object key order kept, a duplicate key keeps its
// first position with the last value, ints as digits, floats as repr, non-ASCII
// literal). It returns ErrNotRenderable when the render would fail.
func ParseDetails(text string) ([]byte, error) {
	p := &pyParser{s: text}
	p.skipWS()
	v, ok := p.value(0)
	if ok {
		p.skipWS()
		ok = p.i == len(p.s)
	}
	if !ok {
		return []byte("{}"), nil
	}
	if p.bad {
		return nil, ErrNotRenderable
	}
	var b strings.Builder
	render(&b, v)
	return []byte(b.String()), nil
}

type pyObj struct {
	keys []string
	vals map[string]any
}

type numLit string // already rendered

type pyParser struct {
	s   string
	i   int
	bad bool // valid for json.loads but not renderable
}

func (p *pyParser) skipWS() {
	for p.i < len(p.s) {
		switch p.s[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

func (p *pyParser) lit(word string) bool {
	if strings.HasPrefix(p.s[p.i:], word) {
		p.i += len(word)
		return true
	}
	return false
}

func (p *pyParser) value(depth int) (any, bool) {
	if p.i >= len(p.s) {
		return nil, false
	}
	switch c := p.s[p.i]; {
	case c == '"':
		return p.str()
	case c == '{':
		return p.object(depth)
	case c == '[':
		return p.array(depth)
	case c == 'n':
		return nil, p.lit("null")
	case c == 't':
		return true, p.lit("true")
	case c == 'f':
		return false, p.lit("false")
	case c == 'N':
		ok := p.lit("NaN")
		p.bad = p.bad || ok
		return numLit("NaN"), ok
	case c == 'I':
		ok := p.lit("Infinity")
		p.bad = p.bad || ok
		return numLit("Infinity"), ok
	case c == '-' && strings.HasPrefix(p.s[p.i:], "-Infinity"):
		p.i += len("-Infinity")
		p.bad = true
		return numLit("-Infinity"), true
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number()
	}
	return nil, false
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func (p *pyParser) number() (any, bool) {
	start := p.i
	if p.s[p.i] == '-' {
		p.i++
	}
	if p.i >= len(p.s) || !isDigit(p.s[p.i]) {
		return nil, false
	}
	if p.s[p.i] == '0' {
		p.i++
	} else {
		for p.i < len(p.s) && isDigit(p.s[p.i]) {
			p.i++
		}
	}
	intEnd := p.i
	isFloat := false
	if p.i+1 < len(p.s) && p.s[p.i] == '.' && isDigit(p.s[p.i+1]) {
		isFloat = true
		p.i++
		for p.i < len(p.s) && isDigit(p.s[p.i]) {
			p.i++
		}
	}
	if p.i < len(p.s) && (p.s[p.i] == 'e' || p.s[p.i] == 'E') {
		j := p.i + 1
		if j < len(p.s) && (p.s[j] == '+' || p.s[j] == '-') {
			j++
		}
		if j < len(p.s) && isDigit(p.s[j]) {
			for j < len(p.s) && isDigit(p.s[j]) {
				j++
			}
			isFloat = true
			p.i = j
		}
	}
	text := p.s[start:p.i]
	if !isFloat {
		digits := strings.TrimPrefix(text[:intEnd-start], "-")
		if len(digits) > maxIntDigits {
			return nil, false
		}
		if digits == "0" {
			return numLit("0"), true // int("-0") == 0
		}
		return numLit(text), true
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil { // overflow: float("1e400") is inf in Python
		p.bad = true
		return numLit("Infinity"), true
	}
	r, err := canonjson.FloatRepr(f)
	if err != nil {
		p.bad = true
		return numLit("Infinity"), true
	}
	return numLit(r), true
}

func hexVal(s string) (rune, bool) {
	if len(s) != 4 {
		return 0, false
	}
	var v rune
	for i := 0; i < 4; i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			v = v<<4 | rune(c-'0')
		case c >= 'a' && c <= 'f':
			v = v<<4 | rune(c-'a'+10)
		case c >= 'A' && c <= 'F':
			v = v<<4 | rune(c-'A'+10)
		default:
			return 0, false
		}
	}
	return v, true
}

func (p *pyParser) str() (any, bool) {
	p.i++ // opening quote
	var b strings.Builder
	for p.i < len(p.s) {
		c := p.s[p.i]
		switch {
		case c == '"':
			p.i++
			return b.String(), true
		case c < 0x20:
			return nil, false // strict mode: raw control characters are invalid
		case c != '\\':
			r, n := utf8.DecodeRuneInString(p.s[p.i:])
			b.WriteRune(r)
			p.i += n
		default:
			if p.i+1 >= len(p.s) {
				return nil, false
			}
			e := p.s[p.i+1]
			p.i += 2
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
				if p.i+4 > len(p.s) {
					return nil, false
				}
				r, ok := hexVal(p.s[p.i : p.i+4])
				if !ok {
					return nil, false
				}
				p.i += 4
				if r >= 0xd800 && r <= 0xdbff && strings.HasPrefix(p.s[p.i:], `\u`) && p.i+6 <= len(p.s) {
					if lo, ok := hexVal(p.s[p.i+2 : p.i+6]); ok && lo >= 0xdc00 && lo <= 0xdfff {
						p.i += 6
						r = 0x10000 + (r-0xd800)<<10 + (lo - 0xdc00)
					}
				}
				if r >= 0xd800 && r <= 0xdfff {
					p.bad = true // lone surrogate: loads accepts it, UTF-8 encoding the response fails
					b.WriteRune(utf8.RuneError)
				} else {
					b.WriteRune(r)
				}
			default:
				return nil, false
			}
		}
	}
	return nil, false
}

func (p *pyParser) array(depth int) (any, bool) {
	if depth >= maxNesting {
		return nil, false
	}
	p.i++
	out := []any{}
	p.skipWS()
	if p.i < len(p.s) && p.s[p.i] == ']' {
		p.i++
		return out, true
	}
	for {
		p.skipWS()
		v, ok := p.value(depth + 1)
		if !ok {
			return nil, false
		}
		out = append(out, v)
		p.skipWS()
		if p.i >= len(p.s) {
			return nil, false
		}
		switch p.s[p.i] {
		case ',':
			p.i++
		case ']':
			p.i++
			return out, true
		default:
			return nil, false
		}
	}
}

func (p *pyParser) object(depth int) (any, bool) {
	if depth >= maxNesting {
		return nil, false
	}
	p.i++
	obj := &pyObj{vals: map[string]any{}}
	p.skipWS()
	if p.i < len(p.s) && p.s[p.i] == '}' {
		p.i++
		return obj, true
	}
	for {
		p.skipWS()
		if p.i >= len(p.s) || p.s[p.i] != '"' {
			return nil, false
		}
		k, ok := p.str()
		if !ok {
			return nil, false
		}
		p.skipWS()
		if p.i >= len(p.s) || p.s[p.i] != ':' {
			return nil, false
		}
		p.i++
		p.skipWS()
		v, ok := p.value(depth + 1)
		if !ok {
			return nil, false
		}
		key := k.(string)
		if _, dup := obj.vals[key]; !dup {
			obj.keys = append(obj.keys, key)
		}
		obj.vals[key] = v
		p.skipWS()
		if p.i >= len(p.s) {
			return nil, false
		}
		switch p.s[p.i] {
		case ',':
			p.i++
		case '}':
			p.i++
			return obj, true
		default:
			return nil, false
		}
	}
}

func render(b *strings.Builder, v any) {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case numLit:
		b.WriteString(string(x))
	case string:
		renderString(b, x)
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			render(b, e)
		}
		b.WriteByte(']')
	case *pyObj:
		b.WriteByte('{')
		for i, k := range x.keys {
			if i > 0 {
				b.WriteByte(',')
			}
			renderString(b, k)
			b.WriteByte(':')
			render(b, x.vals[k])
		}
		b.WriteByte('}')
	}
}

// renderString is json.dumps(ensure_ascii=False) string escaping.
func renderString(b *strings.Builder, s string) {
	const hex = "0123456789abcdef"
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
			b.WriteString(`\u00`)
			b.WriteByte(hex[r>>4])
			b.WriteByte(hex[r&0xf])
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
}
