package retention

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// Categories lists the retention categories in the order Python's
// DEFAULT_DAYS (and therefore every policy_snapshot) enumerates them.
var Categories = []string{"audit", "llm_usage", "signals", "activation", "audience_history", "agent_results", "outreach_history"}

// DefaultDays and MinDays mirror DEFAULT_DAYS and MIN_DAYS in
// apps/api/services/governance/retention.py.
var (
	DefaultDays = map[string]int64{"audit": 365, "llm_usage": 365, "signals": 365, "activation": 180, "audience_history": 365, "agent_results": 180, "outreach_history": 365}
	MinDays     = map[string]int64{"audit": 90, "llm_usage": 30, "signals": 30, "activation": 30, "audience_history": 30, "agent_results": 30, "outreach_history": 30}
)

const maxDays = 3650

// pyError carries the text Python's exception would have produced, so job
// errors and retention_runs.error stay identical across executors.
type pyError struct{ msg string }

func (e *pyError) Error() string { return e.msg }

func pyErrorf(format string, args ...any) error { return &pyError{fmt.Sprintf(format, args...)} }

// snapshotJSON renders days exactly as json.dumps(dict) would for the
// Python run: Categories order, ", " and ": " separators.
func snapshotJSON(days map[string]int64) string {
	var b strings.Builder
	b.WriteByte('{')
	for i, c := range Categories {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%q: %d", c, days[c])
	}
	b.WriteByte('}')
	return b.String()
}

// countsJSON renders deleted_counts like json.dumps of Python's dict, which
// holds only the categories visited so far, in visiting order.
func countsJSON(order []string, counts map[string]int64) string {
	var b strings.Builder
	b.WriteByte('{')
	for i, c := range order {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%q: %d", c, counts[c])
	}
	b.WriteByte('}')
	return b.String()
}

// object is a decoded JSON object that remembers key order, because Python's
// normalized_days reports the first invalid value in the stored key order.
type object struct {
	keys []string
	vals map[string]any
}

func decodeValue(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := decodeAny(dec)
	if err != nil {
		return nil, err
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
			o := &object{vals: map[string]any{}}
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
				if _, seen := o.vals[k]; !seen {
					o.keys = append(o.keys, k)
				}
				o.vals[k] = v // later duplicates win, like json.loads
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

// pyTypeName is type(value).__name__ for a decoded JSON value.
func pyTypeName(v any) string {
	switch t := v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case string:
		return "str"
	case json.Number:
		if isFloatLiteral(t) {
			return "float"
		}
		return "int"
	case []any:
		return "list"
	case *object:
		return "dict"
	}
	return "object"
}

func isFloatLiteral(n json.Number) bool { return strings.ContainsAny(string(n), ".eE") }

// truthy is Python truthiness for a decoded JSON value.
func truthy(v any) bool {
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
	case *object:
		return len(t.keys) > 0
	}
	return true
}

// normalizedDays ports normalized_days for the stored JSON text of a policy's
// retention_days (or any decoded value): unknown categories are rejected
// first, then each given value is coerced with int() and range-checked in
// key order. Defaults fill the categories that are absent.
//
// Divergence: a non-empty JSON value that is not an object makes Python fail
// with an AttributeError/TypeError whose text depends on the type; here it is
// one fixed message. The API only ever stores objects.
func normalizedDays(raw []byte) (map[string]int64, error) {
	value, err := decodeValue(raw)
	if err != nil {
		return nil, fmt.Errorf("decode retention_days: %w", err)
	}
	return normalizedDaysValue(value)
}

func normalizedDaysValue(value any) (map[string]int64, error) {
	result := make(map[string]int64, len(Categories))
	for k, v := range DefaultDays {
		result[k] = v
	}
	if !truthy(value) {
		return result, nil
	}
	obj, ok := value.(*object)
	if !ok {
		return nil, fmt.Errorf("retention_days must be a JSON object, got %s", pyTypeName(value))
	}
	var unknown []string
	for _, k := range obj.keys {
		if _, ok := DefaultDays[k]; !ok {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, pyErrorf("unsupported retention categories: %s", strings.Join(unknown, ", "))
	}
	for _, k := range obj.keys {
		days, err := pyInt(obj.vals[k])
		if err != nil {
			return nil, err
		}
		if days < MinDays[k] || days > maxDays {
			return nil, pyErrorf("%s retention must be %d..%d days", k, MinDays[k], maxDays)
		}
		result[k] = days
	}
	return result, nil
}

// pyInt ports int(value) for decoded JSON values. Integers beyond int64 are
// clamped: they fail the range check either way.
func pyInt(v any) (int64, error) {
	switch t := v.(type) {
	case bool:
		if t {
			return 1, nil
		}
		return 0, nil
	case json.Number:
		if !isFloatLiteral(t) {
			n, err := strconv.ParseInt(string(t), 10, 64)
			if err != nil {
				if strings.HasPrefix(string(t), "-") {
					return math.MinInt64, nil
				}
				return math.MaxInt64, nil
			}
			return n, nil
		}
		f, err := strconv.ParseFloat(string(t), 64)
		if math.IsInf(f, 0) {
			return 0, pyErrorf("cannot convert float infinity to integer")
		}
		_ = err
		return clampTrunc(f), nil
	case string:
		return pyIntString(t)
	}
	return 0, pyErrorf("int() argument must be a string, a bytes-like object or a real number, not '%s'", pyTypeName(v))
}

func clampTrunc(f float64) int64 {
	f = math.Trunc(f)
	switch {
	case f >= math.MaxInt64:
		return math.MaxInt64
	case f <= math.MinInt64:
		return math.MinInt64
	}
	return int64(f)
}

// pyIntString ports int(str): surrounding whitespace is ignored, an optional
// sign, digits (ASCII or other Unicode decimal digits) with single
// underscores allowed between digits.
func pyIntString(s string) (int64, error) {
	bad := func() (int64, error) {
		return 0, pyErrorf("invalid literal for int() with base 10: %s", pyRepr(s))
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
			// Divergence: Python also accepts non-ASCII Unicode decimal
			// digits; they are rejected here (never produced by the API).
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

// pyRepr is repr(str) for the characters that occur in error messages.
func pyRepr(s string) string {
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
