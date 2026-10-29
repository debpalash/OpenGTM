package dsnparse

import (
	"strings"
	"unicode/utf8"
)

// Helpers reproducing the Python str / bytes behaviours the parser relies on.

// asciiLower lower-cases ASCII letters only (raw bytes >= 0x80 are untouched).
func asciiLower(s string) string {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'A' && c <= 'Z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if b[j] >= 'A' && b[j] <= 'Z' {
					b[j] += 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return s
}

// pyWS is the ASCII subset of str.isspace() that can occur in surrogate-escaped
// text (str.strip() with no argument).
const pyWS = " \t\n\v\f\r\x1c\x1d\x1e\x1f"

func pyStrip(s string) string  { return strings.Trim(s, pyWS) }
func pyRStrip(s string) string { return strings.TrimRight(s, pyWS) }

func hasHigh(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return true
		}
	}
	return false
}

// strValue is str(header): a non-ASCII value is a Header whose str() turns
// every undecodable byte into U+FFFD.
func strValue(s string) string {
	if !hasHigh(s) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			b.WriteRune('�')
		} else {
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// decodeUTF8Replace is bytes.decode("utf-8", errors="replace"): one U+FFFD per
// maximal invalid subpart.
func decodeUTF8Replace(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var out strings.Builder
	out.Grow(len(b))
	i := 0
	for i < len(b) {
		c := b[i]
		if c < 0x80 {
			out.WriteByte(c)
			i++
			continue
		}
		var need int
		lo, hi := byte(0x80), byte(0xBF)
		switch {
		case c >= 0xC2 && c <= 0xDF:
			need = 1
		case c == 0xE0:
			need, lo = 2, 0xA0
		case c >= 0xE1 && c <= 0xEC, c == 0xEE, c == 0xEF:
			need = 2
		case c == 0xED:
			need, hi = 2, 0x9F
		case c == 0xF0:
			need, lo = 3, 0x90
		case c >= 0xF1 && c <= 0xF3:
			need = 3
		case c == 0xF4:
			need, hi = 3, 0x8F
		default:
			out.WriteRune('�')
			i++
			continue
		}
		j := i + 1
		ok := true
		for k := 0; k < need; k++ {
			if j >= len(b) || b[j] < lo || b[j] > hi {
				ok = false
				break
			}
			lo, hi = 0x80, 0xBF
			j++
		}
		if ok {
			out.Write(b[i:j])
		} else {
			out.WriteRune('�')
		}
		i = j
	}
	return out.String()
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'A' && c <= 'F' || c >= 'a' && c <= 'f'
}

func hexVal(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	}
	return c - 'A' + 10
}

// qpDecode is quopri.decodestring (binascii.a2b_qp, header=False).
func qpDecode(data []byte) []byte {
	out := make([]byte, 0, len(data))
	n := len(data)
	for i := 0; i < n; {
		if data[i] != '=' {
			out = append(out, data[i])
			i++
			continue
		}
		i++
		if i >= n {
			break
		}
		switch c := data[i]; {
		case c == '\n' || c == '\r':
			if c != '\n' {
				for i < n && data[i] != '\n' {
					i++
				}
			}
			if i < n {
				i++
			}
		case c == '=':
			out = append(out, '=')
			i++
		case i+1 < n && isHex(c) && isHex(data[i+1]):
			out = append(out, hexVal(c)<<4|hexVal(data[i+1]))
			i += 2
		default:
			out = append(out, '=')
		}
	}
	return out
}

func b64val(c byte) int {
	switch {
	case c >= 'A' && c <= 'Z':
		return int(c - 'A')
	case c >= 'a' && c <= 'z':
		return int(c-'a') + 26
	case c >= '0' && c <= '9':
		return int(c-'0') + 52
	case c == '+':
		return 62
	case c == '/':
		return 63
	}
	return -1
}

// a2bBase64 is binascii.a2b_base64; ok is false where Python raises binascii.Error.
func a2bBase64(s []byte, strict bool) ([]byte, bool) {
	if strict && len(s) > 0 && s[0] == '=' {
		return nil, false
	}
	out := make([]byte, 0, len(s)/4*3+3)
	quad, left, pads := 0, 0, 0
	padStarted := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '=' {
			padStarted = true
			pads++
			if quad >= 2 && quad+pads >= 4 {
				if strict && i+1 < len(s) {
					return nil, false
				}
				quad = 0
				break
			}
			continue
		}
		v := b64val(c)
		if v < 0 {
			if strict {
				return nil, false
			}
			continue
		}
		if strict && padStarted {
			return nil, false
		}
		pads = 0
		switch quad {
		case 0:
			quad, left = 1, v
		case 1:
			quad = 2
			out = append(out, byte(left<<2|v>>4))
			left = v & 0xf
		case 2:
			quad = 3
			out = append(out, byte(left<<4|v>>2))
			left = v & 3
		case 3:
			quad = 0
			out = append(out, byte(left<<6|v))
			left = 0
		}
	}
	if quad != 0 {
		return nil, false
	}
	return out, true
}

// decodeB is email._encoded_words.decode_b applied to the joined lines.
func decodeB(enc []byte) []byte {
	padErr := len(enc) % 4
	padded := enc
	if padErr != 0 {
		padded = append(append([]byte{}, enc...), "==="[:4-padErr]...)
	}
	if v, ok := a2bBase64(padded, true); ok {
		return v
	}
	if v, ok := a2bBase64(enc, false); ok {
		return v
	}
	if v, ok := a2bBase64(append(append([]byte{}, enc...), '=', '='), false); ok {
		return v
	}
	return enc
}

// payloadBytes is Message.get_payload(decode=True): nil for multiparts. UU
// encodings (x-uuencode, uuencode, uue, x-uue) are returned undecoded.
func payloadBytes(p *part) []byte {
	if p.isList || !p.isStr {
		return nil
	}
	cte := ""
	if v, ok := p.getRaw("content-transfer-encoding"); ok {
		cte = asciiLower(pyStrip(strValue(v)))
	}
	b := []byte(p.payload)
	switch cte {
	case "quoted-printable":
		return qpDecode(b)
	case "base64":
		// bytes.splitlines() then join: drops CR and LF only.
		joined := make([]byte, 0, len(b))
		for _, c := range b {
			if c != '\r' && c != '\n' {
				joined = append(joined, c)
			}
		}
		return decodeB(joined)
	}
	return b
}
