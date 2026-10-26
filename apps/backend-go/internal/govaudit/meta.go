package govaudit

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
)

const maxMetaDepth = 100

// pyJSONString renders s like Python json.dumps: ensureASCII escapes non-ASCII
// as \uXXXX (surrogate pairs above the BMP); otherwise they stay literal.
func pyJSONString(s string, ensureASCII bool) string {
	var b strings.Builder
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
			b.WriteByte("0123456789abcdef"[r>>4])
			b.WriteByte("0123456789abcdef"[r&0xf])
		case r < 0x80 || !ensureASCII:
			if r == utf8.RuneError {
				b.WriteString("�")
			} else {
				b.WriteRune(r)
			}
		case r < 0x10000:
			b.WriteString(`\u`)
			b.WriteString(hex4(int(r)))
		default:
			r -= 0x10000
			b.WriteString(`\u` + hex4(0xd800+int(r>>10)) + `\u` + hex4(0xdc00+int(r&0x3ff)))
		}
	}
	b.WriteByte('"')
	return b.String()
}

func hex4(v int) string {
	s := strconv.FormatInt(int64(v), 16)
	return strings.Repeat("0", 4-len(s)) + s
}

// normalizeMetadata re-renders a stored JSON document the way Python does when
// it json.loads the column and json.dumps it back with ensure_ascii=False and
// compact separators: key order preserved, duplicate keys keep the first
// position with the last value, floats as repr, non-ASCII literal. Python's
// `metadata_json or {}` maps falsy documents to {}.
func normalizeMetadata(raw []byte) ([]byte, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return []byte("{}"), nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var b strings.Builder
	if err := encodeValue(dec, &b, 0); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("govaudit: trailing data in metadata")
	}
	switch out := b.String(); out {
	case "null", "{}", "[]", `""`, "0", "0.0", "false":
		return []byte("{}"), nil
	default:
		return []byte(out), nil
	}
}

func encodeValue(dec *json.Decoder, b *strings.Builder, depth int) error {
	if depth > maxMetaDepth {
		return errors.New("govaudit: metadata nested too deeply")
	}
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch v := tok.(type) {
	case json.Delim:
		if v == '[' {
			b.WriteByte('[')
			first := true
			for dec.More() {
				if !first {
					b.WriteByte(',')
				}
				first = false
				if err := encodeValue(dec, b, depth+1); err != nil {
					return err
				}
			}
			_, err := dec.Token()
			b.WriteByte(']')
			return err
		}
		var keys []string
		vals := map[string]string{}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return err
			}
			k := kt.(string)
			var vb strings.Builder
			if err := encodeValue(dec, &vb, depth+1); err != nil {
				return err
			}
			if _, seen := vals[k]; !seen {
				keys = append(keys, k)
			}
			vals[k] = vb.String()
		}
		if _, err := dec.Token(); err != nil {
			return err
		}
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(pyJSONString(k, false))
			b.WriteByte(':')
			b.WriteString(vals[k])
		}
		b.WriteByte('}')
	case string:
		b.WriteString(pyJSONString(v, false))
	case bool:
		b.WriteString(strconv.FormatBool(v))
	case nil:
		b.WriteString("null")
	case json.Number:
		s := v.String()
		if strings.ContainsAny(s, ".eE") {
			f, err := strconv.ParseFloat(s, 64)
			if err != nil {
				return err
			}
			r, err := canonjson.FloatRepr(f)
			if err != nil {
				return err
			}
			b.WriteString(r)
		} else if s == "-0" {
			b.WriteString("0")
		} else {
			b.WriteString(s)
		}
	}
	return nil
}
