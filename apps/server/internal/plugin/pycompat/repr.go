package pycompat

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"unicode"
)

// Str mirrors Python's str(v) for the value model.
func Str(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case Timestamp:
		return tsStr(t)
	}
	return Repr(v)
}

// Repr mirrors Python's repr(v) for the value model.
func Repr(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case bool:
		if t {
			return "True"
		}
		return "False"
	case int64:
		return strconv.FormatInt(t, 10)
	case int:
		return strconv.Itoa(t)
	case *big.Int:
		return t.String()
	case float64:
		return FloatRepr(t)
	case string:
		return reprString(t)
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = Repr(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case Tuple:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = Repr(e)
		}
		if len(parts) == 1 {
			return "(" + parts[0] + ",)"
		}
		return "(" + strings.Join(parts, ", ") + ")"
	case *Map:
		parts := make([]string, 0, t.Len())
		for _, e := range t.Entries() {
			parts = append(parts, Repr(e.Key)+": "+Repr(e.Value))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case Set:
		if len(t) == 0 {
			return "set()"
		}
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = Repr(e)
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case Bytes:
		return "b" + reprString(string(t))
	case Timestamp:
		if t.DateOnly {
			return fmt.Sprintf("datetime.date(%d, %d, %d)", t.Time.Year(), int(t.Time.Month()), t.Time.Day())
		}
		return dtRepr(t)
	}
	return fmt.Sprint(v)
}

func dtRepr(t Timestamp) string {
	tm := t.Time
	parts := []int{tm.Year(), int(tm.Month()), tm.Day(), tm.Hour(), tm.Minute(), tm.Second(), tm.Nanosecond() / 1000}
	for i := 0; i < 2 && parts[len(parts)-1] == 0; i++ {
		parts = parts[:len(parts)-1]
	}
	strs := make([]string, len(parts))
	for i, p := range parts {
		strs[i] = strconv.Itoa(p)
	}
	s := "datetime.datetime(" + strings.Join(strs, ", ")
	if t.Aware {
		_, off := tm.Zone()
		if off == 0 {
			s += ", tzinfo=datetime.timezone.utc"
		} else {
			days, secs := 0, off
			if off < 0 {
				days = -1
				secs = off + 86400
			}
			if days != 0 {
				s += fmt.Sprintf(", tzinfo=datetime.timezone(datetime.timedelta(days=%d, seconds=%d))", days, secs)
			} else {
				s += fmt.Sprintf(", tzinfo=datetime.timezone(datetime.timedelta(seconds=%d))", secs)
			}
		}
	}
	return s + ")"
}

func tsStr(t Timestamp) string {
	if t.DateOnly {
		return t.Time.Format("2006-01-02")
	}
	s := t.Time.Format("2006-01-02 15:04:05")
	if us := t.Time.Nanosecond() / 1000; us != 0 {
		s += fmt.Sprintf(".%06d", us)
	}
	if t.Aware {
		_, off := t.Time.Zone()
		sign := "+"
		if off < 0 {
			sign = "-"
			off = -off
		}
		s += fmt.Sprintf("%s%02d:%02d", sign, off/3600, (off%3600)/60)
	}
	return s
}

func reprString(s string) string {
	quote := byte('\'')
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for _, r := range s {
		switch {
		case r == rune(quote) || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r < 0x7f:
			b.WriteRune(r)
		case !unicode.IsPrint(r):
			switch {
			case r < 0x100:
				fmt.Fprintf(&b, `\x%02x`, r)
			case r < 0x10000:
				fmt.Fprintf(&b, `\u%04x`, r)
			default:
				fmt.Fprintf(&b, `\U%08x`, r)
			}
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}

// IsSpace mirrors str.isspace() for one rune (Python's whitespace set).
func IsSpace(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\v', '\f', '\r', 0x1c, 0x1d, 0x1e, 0x1f, 0x85, 0xa0, 0x1680,
		0x2028, 0x2029, 0x202f, 0x205f, 0x3000:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// Strip mirrors str.strip() with no arguments.
func Strip(s string) string { return strings.TrimFunc(s, IsSpace) }

// Upper mirrors str.upper(), including the full case mappings that expand to
// several characters (e.g. "ß" -> "SS", "ﬅ" -> "ST").
func Upper(s string) string {
	var b strings.Builder
	for _, r := range s {
		if exp, ok := specialUpper[r]; ok {
			b.WriteString(exp)
			continue
		}
		b.WriteRune(unicode.ToUpper(r))
	}
	return b.String()
}

var specialUpper = map[rune]string{
	'ß': "SS", 'ŉ': "ʼN", 'ǰ': "J̌", 'ΐ': "Ϊ́", 'ΰ': "Ϋ́", 'և': "ԵՒ",
	'ẖ': "H̱", 'ẗ': "T̈", 'ẘ': "W̊", 'ẙ': "Y̊", 'ẚ': "Aʾ",
	'ﬀ': "FF", 'ﬁ': "FI", 'ﬂ': "FL", 'ﬃ': "FFI", 'ﬄ': "FFL", 'ﬅ': "ST", 'ﬆ': "ST",
	'ﬓ': "ՄՆ", 'ﬔ': "ՄԵ", 'ﬕ': "ՄԻ", 'ﬖ': "ՎՆ", 'ﬗ': "ՄԽ",
}
