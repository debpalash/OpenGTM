package wbcrud

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

// PyDumps renders v exactly like Python's json.dumps(v) (", " / ": "
// separators, ensure_ascii) or json.dumps(v, sort_keys=True). Row data lives in
// json (not jsonb) columns and is searched as text, so the stored text must be
// what SQLAlchemy would have written. v is the ordered tree produced by
// decodeDocument (*Obj, []any, string, json.Number, bool, nil).
func PyDumps(v any, sortKeys bool) string {
	var b strings.Builder
	pyDump(&b, v, sortKeys)
	return b.String()
}

func pyDump(b *strings.Builder, v any, sortKeys bool) {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(t))
	case string:
		pyString(b, t)
	case json.Number:
		b.WriteString(string(t))
	case float64:
		b.WriteString(pyFloat(t))
	case int:
		b.WriteString(strconv.Itoa(t))
	case int64:
		b.WriteString(strconv.FormatInt(t, 10))
	case []any:
		if len(t) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteString(", ")
			}
			pyDump(b, e, sortKeys)
		}
		b.WriteByte(']')
	case *Obj:
		if t == nil || t.Len() == 0 {
			b.WriteString("{}")
			return
		}
		keys := t.Keys()
		if sortKeys {
			sort.Strings(keys) // UTF-8 byte order == code point order
		}
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteString(", ")
			}
			pyString(b, k)
			b.WriteString(": ")
			val, _ := t.Get(k)
			pyDump(b, val, sortKeys)
		}
		b.WriteByte('}')
	default:
		raw, err := json.Marshal(t)
		if err != nil {
			b.WriteString("null")
			return
		}
		b.Write(raw)
	}
}

const hexDigits = "0123456789abcdef"

func pyString(b *strings.Builder, s string) {
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
			pyU(b, uint16(r))
		default:
			hi, lo := utf16.EncodeRune(r)
			pyU(b, uint16(hi))
			pyU(b, uint16(lo))
		}
	}
	b.WriteByte('"')
}

func pyU(b *strings.Builder, u uint16) {
	b.WriteString(`\u`)
	b.WriteByte(hexDigits[u>>12&0xf])
	b.WriteByte(hexDigits[u>>8&0xf])
	b.WriteByte(hexDigits[u>>4&0xf])
	b.WriteByte(hexDigits[u&0xf])
}
