// Package leadevents relays per-workspace lead progress events from Redis
// pub/sub to SSE clients. Python parses each payload and re-emits
// json.dumps(event); EncodeEvent reproduces those bytes exactly.
package leadevents

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
)

const maxDepth = 900

// Channel is the Redis channel one workspace's progress events use.
func Channel(workspaceID string) string { return "leadgen:progress:" + workspaceID }

// HeartbeatFrame is the SSE keep-alive frame.
func HeartbeatFrame() []byte { return []byte("data: {\"type\": \"heartbeat\"}\n\n") }

// Frame wraps an encoded event as one SSE data frame.
func Frame(event []byte) []byte {
	out := make([]byte, 0, len(event)+8)
	out = append(out, "data: "...)
	out = append(out, event...)
	return append(out, '\n', '\n')
}

// EncodeEvent re-encodes a JSON payload like Python's json.loads followed by
// json.dumps with defaults: ", " and ": " separators, ensure_ascii, key order
// kept (duplicate keys: first position, last value), Python float repr, NaN
// and Infinity accepted, arbitrary-size integers preserved. Invalid JSON
// returns an error.
func EncodeEvent(raw []byte) ([]byte, error) {
	if !utf8.Valid(raw) {
		return nil, errors.New("leadevents: invalid UTF-8")
	}
	p := &parser{b: raw}
	var out bytes.Buffer
	p.ws()
	if err := p.value(&out, 0); err != nil {
		return nil, err
	}
	p.ws()
	if p.i != len(p.b) {
		return nil, p.errf("extra data")
	}
	return out.Bytes(), nil
}

type parser struct {
	b []byte
	i int
}

func (p *parser) errf(msg string) error { return fmt.Errorf("leadevents: %s at %d", msg, p.i) }

func (p *parser) ws() {
	for p.i < len(p.b) {
		switch p.b[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

func (p *parser) lit(s string) bool {
	if bytes.HasPrefix(p.b[p.i:], []byte(s)) {
		p.i += len(s)
		return true
	}
	return false
}

func (p *parser) value(out *bytes.Buffer, depth int) error {
	if depth > maxDepth {
		return p.errf("too deep")
	}
	if p.i >= len(p.b) {
		return p.errf("unexpected end")
	}
	switch c := p.b[p.i]; {
	case c == '{':
		return p.object(out, depth)
	case c == '[':
		return p.array(out, depth)
	case c == '"':
		u, err := p.str()
		if err != nil {
			return err
		}
		writeUnits(out, u)
		return nil
	case c == 't' && p.lit("true"):
		out.WriteString("true")
	case c == 'f' && p.lit("false"):
		out.WriteString("false")
	case c == 'n' && p.lit("null"):
		out.WriteString("null")
	case c == 'N' && p.lit("NaN"):
		out.WriteString("NaN")
	case c == 'I' && p.lit("Infinity"):
		out.WriteString("Infinity")
	case c == '-' && p.lit("-Infinity"):
		out.WriteString("-Infinity")
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number(out)
	default:
		return p.errf("unexpected token")
	}
	return nil
}

func (p *parser) digits() int {
	n := 0
	for p.i < len(p.b) && p.b[p.i] >= '0' && p.b[p.i] <= '9' {
		p.i++
		n++
	}
	return n
}

func (p *parser) number(out *bytes.Buffer) error {
	start := p.i
	if p.b[p.i] == '-' {
		p.i++
	}
	if p.i >= len(p.b) {
		return p.errf("bad number")
	}
	if p.b[p.i] == '0' {
		p.i++
	} else if p.digits() == 0 {
		return p.errf("bad number")
	}
	intEnd := p.i
	isFloat := false
	if p.i+1 < len(p.b) && p.b[p.i] == '.' && p.b[p.i+1] >= '0' && p.b[p.i+1] <= '9' {
		p.i++
		p.digits()
		isFloat = true
	}
	if p.i < len(p.b) && (p.b[p.i] == 'e' || p.b[p.i] == 'E') {
		j := p.i + 1
		if j < len(p.b) && (p.b[j] == '+' || p.b[j] == '-') {
			j++
		}
		if j < len(p.b) && p.b[j] >= '0' && p.b[j] <= '9' {
			p.i = j
			p.digits()
			isFloat = true
		}
	}
	if !isFloat {
		s := string(p.b[start:intEnd])
		if s == "-0" {
			s = "0"
		}
		out.WriteString(s)
		return nil
	}
	f, err := strconv.ParseFloat(string(p.b[start:p.i]), 64)
	if err != nil && !math.IsInf(f, 0) {
		return p.errf("bad number")
	}
	switch {
	case math.IsInf(f, 1):
		out.WriteString("Infinity")
	case math.IsInf(f, -1):
		out.WriteString("-Infinity")
	default:
		s, _ := canonjson.FloatRepr(f)
		out.WriteString(s)
	}
	return nil
}

// str parses a JSON string into UTF-16 code units, which represents lone
// surrogates exactly as Python keeps them.
func (p *parser) str() ([]uint16, error) {
	p.i++ // opening quote
	var u []uint16
	for {
		if p.i >= len(p.b) {
			return nil, p.errf("unterminated string")
		}
		c := p.b[p.i]
		switch {
		case c == '"':
			p.i++
			return u, nil
		case c < 0x20:
			return nil, p.errf("control character in string")
		case c == '\\':
			p.i++
			if p.i >= len(p.b) {
				return nil, p.errf("bad escape")
			}
			e := p.b[p.i]
			p.i++
			switch e {
			case '"', '\\', '/':
				u = append(u, uint16(e))
			case 'b':
				u = append(u, '\b')
			case 'f':
				u = append(u, '\f')
			case 'n':
				u = append(u, '\n')
			case 'r':
				u = append(u, '\r')
			case 't':
				u = append(u, '\t')
			case 'u':
				if p.i+4 > len(p.b) {
					return nil, p.errf("bad \\u escape")
				}
				v, err := strconv.ParseUint(string(p.b[p.i:p.i+4]), 16, 16)
				if err != nil || bytes.ContainsAny(p.b[p.i:p.i+4], "+-_") {
					return nil, p.errf("bad \\u escape")
				}
				p.i += 4
				u = append(u, uint16(v))
			default:
				return nil, p.errf("bad escape")
			}
		case c < utf8.RuneSelf:
			u = append(u, uint16(c))
			p.i++
		default:
			r, n := utf8.DecodeRune(p.b[p.i:])
			p.i += n
			if r >= 0x10000 {
				r1, r2 := utf16.EncodeRune(r)
				u = append(u, uint16(r1), uint16(r2))
			} else {
				u = append(u, uint16(r))
			}
		}
	}
}

const hexdigits = "0123456789abcdef"

func writeUnits(out *bytes.Buffer, u []uint16) {
	out.WriteByte('"')
	for _, c := range u {
		switch {
		case c == '"':
			out.WriteString(`\"`)
		case c == '\\':
			out.WriteString(`\\`)
		case c == '\n':
			out.WriteString(`\n`)
		case c == '\r':
			out.WriteString(`\r`)
		case c == '\t':
			out.WriteString(`\t`)
		case c == '\b':
			out.WriteString(`\b`)
		case c == '\f':
			out.WriteString(`\f`)
		case c >= ' ' && c <= '~':
			out.WriteByte(byte(c))
		default:
			out.WriteString(`\u`)
			out.WriteByte(hexdigits[c>>12&0xf])
			out.WriteByte(hexdigits[c>>8&0xf])
			out.WriteByte(hexdigits[c>>4&0xf])
			out.WriteByte(hexdigits[c&0xf])
		}
	}
	out.WriteByte('"')
}

func (p *parser) array(out *bytes.Buffer, depth int) error {
	p.i++
	out.WriteByte('[')
	p.ws()
	if p.i < len(p.b) && p.b[p.i] == ']' {
		p.i++
		out.WriteByte(']')
		return nil
	}
	for first := true; ; first = false {
		if !first {
			out.WriteString(", ")
		}
		p.ws()
		if err := p.value(out, depth+1); err != nil {
			return err
		}
		p.ws()
		if p.i >= len(p.b) {
			return p.errf("unterminated array")
		}
		switch p.b[p.i] {
		case ',':
			p.i++
		case ']':
			p.i++
			out.WriteByte(']')
			return nil
		default:
			return p.errf("expected , or ]")
		}
	}
}

func (p *parser) object(out *bytes.Buffer, depth int) error {
	p.i++
	p.ws()
	if p.i < len(p.b) && p.b[p.i] == '}' {
		p.i++
		out.WriteString("{}")
		return nil
	}
	var keys []string // key identity: UTF-16 units as string
	vals := map[string][]byte{}
	keyEnc := map[string][]byte{}
	for {
		p.ws()
		if p.i >= len(p.b) || p.b[p.i] != '"' {
			return p.errf("expected key")
		}
		u, err := p.str()
		if err != nil {
			return err
		}
		p.ws()
		if p.i >= len(p.b) || p.b[p.i] != ':' {
			return p.errf("expected :")
		}
		p.i++
		p.ws()
		var vb bytes.Buffer
		if err := p.value(&vb, depth+1); err != nil {
			return err
		}
		var kb bytes.Buffer
		writeUnits(&kb, u)
		id := kb.String() // escaped form is canonical per unit sequence
		if _, dup := vals[id]; !dup {
			keys = append(keys, id)
			keyEnc[id] = kb.Bytes()
		}
		vals[id] = vb.Bytes()
		p.ws()
		if p.i >= len(p.b) {
			return p.errf("unterminated object")
		}
		switch p.b[p.i] {
		case ',':
			p.i++
			continue
		case '}':
			p.i++
		default:
			return p.errf("expected , or }")
		}
		break
	}
	out.WriteByte('{')
	for n, id := range keys {
		if n > 0 {
			out.WriteString(", ")
		}
		out.Write(keyEnc[id])
		out.WriteString(": ")
		out.Write(vals[id])
	}
	out.WriteByte('}')
	return nil
}
