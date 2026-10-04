package pycompat

import (
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// DumpOptions mirrors the json.dumps keyword arguments OpenGTM uses.
type DumpOptions struct {
	SortKeys      bool
	EnsureASCII   bool
	Indent        int    // 0 means compact (Python indent=None)
	ItemSep       string // default ", " (or "," when Indent > 0)
	KeySep        string // default ": "
	DisallowNaN   bool   // allow_nan=False
	indentEnabled bool
}

// Canonical is json.dumps(v, sort_keys=True, separators=(",", ":"),
// ensure_ascii=False) — the connector signing canonical form.
func Canonical(v any) ([]byte, error) {
	return Dumps(v, DumpOptions{SortKeys: true, ItemSep: ",", KeySep: ":"})
}

// TypeError mirrors Python's TypeError raised by json.dumps.
type TypeError struct{ Msg string }

func (e *TypeError) Error() string { return e.Msg }

// Dumps serializes v like Python's json.dumps.
func Dumps(v any, o DumpOptions) ([]byte, error) {
	if o.Indent > 0 {
		o.indentEnabled = true
	}
	if o.ItemSep == "" {
		if o.indentEnabled {
			o.ItemSep = ","
		} else {
			o.ItemSep = ", "
		}
	}
	if o.KeySep == "" {
		o.KeySep = ": "
	}
	var b strings.Builder
	if err := encode(&b, v, o, 0); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

func newline(b *strings.Builder, o DumpOptions, level int) {
	if o.indentEnabled {
		b.WriteByte('\n')
		b.WriteString(strings.Repeat(" ", o.Indent*level))
	}
}

func encode(b *strings.Builder, v any, o DumpOptions, level int) error {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if t {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case int64:
		b.WriteString(strconv.FormatInt(t, 10))
	case int:
		b.WriteString(strconv.Itoa(t))
	case *big.Int:
		b.WriteString(t.String())
	case float64:
		s, err := jsonFloat(t, o)
		if err != nil {
			return err
		}
		b.WriteString(s)
	case string:
		b.WriteString(QuoteJSON(t, o.EnsureASCII))
	case Tuple:
		return encode(b, []any(t), o, level)
	case []any:
		if len(t) == 0 {
			b.WriteString("[]")
			return nil
		}
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteString(o.ItemSep)
			}
			newline(b, o, level+1)
			if err := encode(b, e, o, level+1); err != nil {
				return err
			}
		}
		newline(b, o, level)
		b.WriteByte(']')
	case *Map:
		if t.Len() == 0 {
			b.WriteString("{}")
			return nil
		}
		entries := t.Entries()
		if o.SortKeys {
			if err := sortEntries(entries); err != nil {
				return err
			}
		}
		b.WriteByte('{')
		for i, e := range entries {
			ks, err := jsonKey(e.Key, o)
			if err != nil {
				return err
			}
			if i > 0 {
				b.WriteString(o.ItemSep)
			}
			newline(b, o, level+1)
			b.WriteString(QuoteJSON(ks, o.EnsureASCII))
			b.WriteString(o.KeySep)
			if err := encode(b, e.Value, o, level+1); err != nil {
				return err
			}
		}
		newline(b, o, level)
		b.WriteByte('}')
	default:
		return &TypeError{Msg: fmt.Sprintf("Object of type %s is not JSON serializable", TypeName(v))}
	}
	return nil
}

func jsonFloat(f float64, o DumpOptions) (string, error) {
	switch {
	case math.IsNaN(f):
		if o.DisallowNaN {
			return "", fmt.Errorf("Out of range float values are not JSON compliant: nan")
		}
		return "NaN", nil
	case math.IsInf(f, 1):
		if o.DisallowNaN {
			return "", fmt.Errorf("Out of range float values are not JSON compliant: inf")
		}
		return "Infinity", nil
	case math.IsInf(f, -1):
		if o.DisallowNaN {
			return "", fmt.Errorf("Out of range float values are not JSON compliant: -inf")
		}
		return "-Infinity", nil
	}
	return FloatRepr(f), nil
}

func jsonKey(k any, o DumpOptions) (string, error) {
	switch t := k.(type) {
	case string:
		return t, nil
	case float64:
		return jsonFloat(t, o)
	case bool:
		if t {
			return "true", nil
		}
		return "false", nil
	case nil:
		return "null", nil
	case int64:
		return strconv.FormatInt(t, 10), nil
	case int:
		return strconv.Itoa(t), nil
	case *big.Int:
		return t.String(), nil
	}
	return "", &TypeError{Msg: fmt.Sprintf("keys must be str, int, float, bool or None, not %s", TypeName(k))}
}

// sortEntries sorts like sorted(dict.items()): keys compare with Python's
// ordering, raising TypeError for incomparable key types.
func sortEntries(entries []Entry) error {
	var err error
	sort.SliceStable(entries, func(i, j int) bool {
		lt, e := pyLess(entries[i].Key, entries[j].Key)
		if e != nil && err == nil {
			err = e
		}
		return lt
	})
	return err
}

func numeric(v any) (*big.Float, bool) {
	switch t := v.(type) {
	case bool:
		if t {
			return big.NewFloat(1), true
		}
		return big.NewFloat(0), true
	case int64:
		return new(big.Float).SetInt64(t), true
	case int:
		return new(big.Float).SetInt64(int64(t)), true
	case *big.Int:
		return new(big.Float).SetInt(t), true
	case float64:
		if math.IsNaN(t) {
			return nil, true
		}
		return big.NewFloat(t), true
	}
	return nil, false
}

func pyLess(a, b any) (bool, error) {
	if as, ok := a.(string); ok {
		if bs, ok := b.(string); ok {
			return as < bs, nil
		}
	}
	af, aok := numeric(a)
	bf, bok := numeric(b)
	if aok && bok {
		if af == nil || bf == nil {
			return false, nil
		}
		return af.Cmp(bf) < 0, nil
	}
	return false, &TypeError{Msg: fmt.Sprintf("'<' not supported between instances of '%s' and '%s'", TypeName(a), TypeName(b))}
}

// FloatRepr mirrors Python's repr(float).
func FloatRepr(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	if f == 0 {
		if math.Signbit(f) {
			return "-0.0"
		}
		return "0.0"
	}
	// Shortest round-trip digits, then Python's formatting rules.
	e := strconv.FormatFloat(f, 'e', -1, 64) // e.g. -1.2345e+06
	neg := strings.HasPrefix(e, "-")
	e = strings.TrimPrefix(e, "-")
	mant, expStr, _ := strings.Cut(e, "e")
	exp, _ := strconv.Atoi(expStr)
	digits := strings.Replace(mant, ".", "", 1)
	decpt := exp + 1 // value = 0.DIGITS * 10^decpt
	var out string
	if decpt <= -4 || decpt > 16 {
		m := digits[:1]
		if len(digits) > 1 {
			m += "." + digits[1:]
		}
		sign := "+"
		x := decpt - 1
		if x < 0 {
			sign = "-"
			x = -x
		}
		out = fmt.Sprintf("%se%s%02d", m, sign, x)
	} else if decpt <= 0 {
		out = "0." + strings.Repeat("0", -decpt) + digits
	} else if decpt >= len(digits) {
		out = digits + strings.Repeat("0", decpt-len(digits)) + ".0"
	} else {
		out = digits[:decpt] + "." + digits[decpt:]
	}
	if neg {
		out = "-" + out
	}
	return out
}

// QuoteJSON mirrors json's py_encode_basestring(_ascii).
func QuoteJSON(s string, ensureASCII bool) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			switch {
			case r < 0x20:
				fmt.Fprintf(&b, `\u%04x`, r)
			case ensureASCII && r > 0x7e:
				if r > 0xffff {
					r1, r2 := utf16.EncodeRune(r)
					fmt.Fprintf(&b, `\u%04x\u%04x`, r1, r2)
				} else {
					fmt.Fprintf(&b, `\u%04x`, r)
				}
			default:
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// JSONError mirrors json.JSONDecodeError.
type JSONError struct{ Msg string }

func (e *JSONError) Error() string { return e.Msg }

// LoadJSON parses text like Python's json.loads: NaN/Infinity literals,
// exact integers, duplicate keys where the last wins, strict strings.
func LoadJSON(data []byte) (any, error) {
	if !utf8.Valid(data) {
		return nil, &JSONError{"invalid UTF-8"}
	}
	s := string(data)
	s = strings.TrimPrefix(s, "\ufeff")
	p := &jparser{s: s}
	p.ws()
	v, err := p.value()
	if err != nil {
		return nil, err
	}
	p.ws()
	if p.i != len(p.s) {
		return nil, p.fail("Extra data")
	}
	return v, nil
}

type jparser struct {
	s string
	i int
}

func (p *jparser) fail(msg string) error {
	return &JSONError{Msg: fmt.Sprintf("%s: char %d", msg, p.i)}
}

func (p *jparser) ws() {
	for p.i < len(p.s) {
		switch p.s[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

func (p *jparser) value() (any, error) {
	if p.i >= len(p.s) {
		return nil, p.fail("Expecting value")
	}
	switch c := p.s[p.i]; {
	case c == '{':
		return p.object()
	case c == '[':
		return p.array()
	case c == '"':
		return p.str()
	case strings.HasPrefix(p.s[p.i:], "null"):
		p.i += 4
		return nil, nil
	case strings.HasPrefix(p.s[p.i:], "true"):
		p.i += 4
		return true, nil
	case strings.HasPrefix(p.s[p.i:], "false"):
		p.i += 5
		return false, nil
	case strings.HasPrefix(p.s[p.i:], "NaN"):
		p.i += 3
		return math.NaN(), nil
	case strings.HasPrefix(p.s[p.i:], "Infinity"):
		p.i += 8
		return math.Inf(1), nil
	case strings.HasPrefix(p.s[p.i:], "-Infinity"):
		p.i += 9
		return math.Inf(-1), nil
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number()
	}
	return nil, p.fail("Expecting value")
}

func (p *jparser) number() (any, error) {
	start := p.i
	if p.s[p.i] == '-' {
		p.i++
	}
	digits := func() int {
		n := 0
		for p.i < len(p.s) && p.s[p.i] >= '0' && p.s[p.i] <= '9' {
			p.i++
			n++
		}
		return n
	}
	if p.i < len(p.s) && p.s[p.i] == '0' {
		p.i++
	} else if digits() == 0 {
		p.i = start
		return nil, p.fail("Expecting value")
	}
	isFloat := false
	if p.i+1 < len(p.s) && p.s[p.i] == '.' && p.s[p.i+1] >= '0' && p.s[p.i+1] <= '9' {
		p.i++
		digits()
		isFloat = true
	}
	if p.i < len(p.s) && (p.s[p.i] == 'e' || p.s[p.i] == 'E') {
		save := p.i
		p.i++
		if p.i < len(p.s) && (p.s[p.i] == '+' || p.s[p.i] == '-') {
			p.i++
		}
		if digits() == 0 {
			p.i = save
		} else {
			isFloat = true
		}
	}
	lit := p.s[start:p.i]
	if isFloat {
		f, err := strconv.ParseFloat(lit, 64)
		if err != nil && !isRangeErr(err) {
			return nil, p.fail("Invalid number")
		}
		return f, nil
	}
	return NumberFromLiteral(lit), nil
}

func (p *jparser) str() (string, error) {
	p.i++ // opening quote
	var b strings.Builder
	for {
		if p.i >= len(p.s) {
			return "", p.fail("Unterminated string starting at")
		}
		c := p.s[p.i]
		switch {
		case c == '"':
			p.i++
			return b.String(), nil
		case c == '\\':
			p.i++
			if p.i >= len(p.s) {
				return "", p.fail("Unterminated string starting at")
			}
			e := p.s[p.i]
			p.i++
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
				r, ok := p.hex4()
				if !ok {
					return "", p.fail("Invalid \\uXXXX escape")
				}
				if utf16.IsSurrogate(r) && strings.HasPrefix(p.s[p.i:], `\u`) {
					save := p.i
					p.i += 2
					r2, ok := p.hex4()
					if ok {
						if d := utf16.DecodeRune(r, r2); d != utf8.RuneError {
							b.WriteRune(d)
							continue
						}
					}
					p.i = save
				}
				b.WriteRune(r)
			default:
				return "", p.fail("Invalid \\escape")
			}
		case c < 0x20:
			return "", p.fail("Invalid control character at")
		default:
			b.WriteByte(c)
			p.i++
		}
	}
}

func (p *jparser) hex4() (rune, bool) {
	if p.i+4 > len(p.s) {
		return 0, false
	}
	n, err := strconv.ParseUint(p.s[p.i:p.i+4], 16, 32)
	if err != nil {
		return 0, false
	}
	p.i += 4
	return rune(n), true
}

func (p *jparser) array() (any, error) {
	p.i++
	out := []any{}
	p.ws()
	if p.i < len(p.s) && p.s[p.i] == ']' {
		p.i++
		return out, nil
	}
	for {
		p.ws()
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		p.ws()
		if p.i >= len(p.s) {
			return nil, p.fail("Expecting ',' delimiter")
		}
		if p.s[p.i] == ']' {
			p.i++
			return out, nil
		}
		if p.s[p.i] != ',' {
			return nil, p.fail("Expecting ',' delimiter")
		}
		p.i++
	}
}

func (p *jparser) object() (any, error) {
	p.i++
	m := NewMap()
	p.ws()
	if p.i < len(p.s) && p.s[p.i] == '}' {
		p.i++
		return m, nil
	}
	for {
		p.ws()
		if p.i >= len(p.s) || p.s[p.i] != '"' {
			return nil, p.fail("Expecting property name enclosed in double quotes")
		}
		k, err := p.str()
		if err != nil {
			return nil, err
		}
		p.ws()
		if p.i >= len(p.s) || p.s[p.i] != ':' {
			return nil, p.fail("Expecting ':' delimiter")
		}
		p.i++
		p.ws()
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		// Python dicts keep the first insertion position and the last value.
		_ = m.Set(k, v)
		p.ws()
		if p.i >= len(p.s) {
			return nil, p.fail("Expecting ',' delimiter")
		}
		if p.s[p.i] == '}' {
			p.i++
			return m, nil
		}
		if p.s[p.i] != ',' {
			return nil, p.fail("Expecting ',' delimiter")
		}
		p.i++
	}
}

func sortStrings(s []string) { sort.Strings(s) }
