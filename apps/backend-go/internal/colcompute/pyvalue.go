// Package colcompute evaluates the computed column types that need no provider:
// formula columns (a hand-written parser and evaluator for exactly the grammar
// legacy/python/apps/api/services/workbook/formula_column.py accepts; nothing is ever
// compiled, eval'd or reflected) and http columns (http_column.py behind the SSRF guard).
package colcompute

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// Ellipsis is Python's `...` singleton.
type Ellipsis struct{}

// pyDict is an insertion-ordered dict (only produced by the JSON decoder and by callers
// that pass a map; formulas never see one).
type pyDict struct {
	keys []string
	vals map[string]any
}

// pyError is the panic payload for every Python exception the evaluator can raise. The
// kind mirrors the Python class so the top-level recover can decide what is reportable.
type pyError struct{ kind, msg string }

func raise(kind, format string, a ...any) {
	panic(&pyError{kind: kind, msg: fmt.Sprintf(format, a...)})
}

const maxStrDigits = 4300 // sys.int_info.default_max_str_digits

// typeName is type(v).__name__.
func typeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case *big.Int:
		return "int"
	case float64:
		return "float"
	case string:
		return "str"
	case []any:
		return "list"
	case []byte:
		return "bytes"
	case complex128:
		return "complex"
	case Ellipsis:
		return "ellipsis"
	case *pyDict:
		return "dict"
	}
	return fmt.Sprintf("%T", v)
}

func bigFromInt(n int64) *big.Int { return big.NewInt(n) }

// norm converts Go-side values (ints of any width, json.Number, []string, maps...) to the
// evaluator's value model: nil, bool, *big.Int, float64, string, []any, []byte, complex128,
// Ellipsis, *pyDict.
func norm(v any) any {
	switch x := v.(type) {
	case nil, bool, string, float64, []byte, complex128, Ellipsis, *pyDict:
		return x
	case *big.Int:
		if x == nil {
			return nil
		}
		return x
	case big.Int:
		return new(big.Int).Set(&x)
	case int:
		return big.NewInt(int64(x))
	case int8:
		return big.NewInt(int64(x))
	case int16:
		return big.NewInt(int64(x))
	case int32:
		return big.NewInt(int64(x))
	case int64:
		return big.NewInt(x)
	case uint:
		return new(big.Int).SetUint64(uint64(x))
	case uint8:
		return big.NewInt(int64(x))
	case uint16:
		return big.NewInt(int64(x))
	case uint32:
		return big.NewInt(int64(x))
	case uint64:
		return new(big.Int).SetUint64(x)
	case float32:
		return float64(x)
	case json.Number:
		if i, ok := new(big.Int).SetString(string(x), 10); ok {
			return i
		}
		f, _ := strconv.ParseFloat(string(x), 64)
		return f
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = norm(e)
		}
		return out
	case []string:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = e
		}
		return out
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		d := &pyDict{keys: keys, vals: make(map[string]any, len(x))}
		for _, k := range keys {
			d.vals[k] = norm(x[k])
		}
		return d
	case map[string]string:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		d := &pyDict{keys: keys, vals: make(map[string]any, len(x))}
		for _, k := range keys {
			d.vals[k] = x[k]
		}
		return d
	}
	raise("TypeError", "unsupported value type %T", v)
	return nil
}

// ---- repr / str ----

// floatRepr is repr(float): the shortest round-trip digits, exponent form when the decimal
// point lies outside (-4, 16].
func floatRepr(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	s := strconv.FormatFloat(f, 'e', -1, 64)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	mant, exp, _ := strings.Cut(s, "e")
	digits := strings.Replace(mant, ".", "", 1)
	e, _ := strconv.Atoi(exp)
	decpt := e + 1
	var out string
	switch {
	case decpt > 16 || decpt <= -4:
		out = digits[:1]
		if len(digits) > 1 {
			out += "." + digits[1:]
		}
		sign := "+"
		if decpt-1 < 0 {
			sign = "-"
		}
		ex := decpt - 1
		if ex < 0 {
			ex = -ex
		}
		out += fmt.Sprintf("e%s%02d", sign, ex)
	case decpt <= 0:
		out = "0." + strings.Repeat("0", -decpt) + digits
	case decpt >= len(digits):
		out = digits + strings.Repeat("0", decpt-len(digits)) + ".0"
	default:
		out = digits[:decpt] + "." + digits[decpt:]
	}
	if neg {
		return "-" + out
	}
	return out
}

func floatReprNoDot(f float64) string { // complex components: 1.0 shows as 1
	return strings.TrimSuffix(floatRepr(f), ".0")
}

func complexRepr(c complex128) string {
	re, im := real(c), imag(c)
	if re == 0 && !math.Signbit(re) {
		return floatReprNoDot(im) + "j"
	}
	sign := "+"
	imStr := floatReprNoDot(im)
	if !strings.HasPrefix(imStr, "-") {
		return "(" + floatReprNoDot(re) + sign + imStr + "j)"
	}
	return "(" + floatReprNoDot(re) + imStr + "j)"
}

func intStr(i *big.Int) string {
	if i.BitLen() > 14300 { // ~4305 digits: cheap pre-check before formatting
		s := i.String()
		d := len(strings.TrimPrefix(s, "-"))
		if d > maxStrDigits {
			raise("ValueError", "Exceeds the limit (%d digits) for integer string conversion; use sys.set_int_max_str_digits() to increase the limit", maxStrDigits)
		}
		return s
	}
	return i.String()
}

func strRepr(s string) string {
	quote := byte('\'')
	if strings.Contains(s, "'") && !strings.Contains(s, "\"") {
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
		case r < ' ' || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r < 0x7f:
			b.WriteRune(r)
		case unicode.IsPrint(r):
			b.WriteRune(r)
		case r < 0x100:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r < 0x10000:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}

func bytesRepr(bs []byte) string {
	quote := byte('\'')
	if bytesContains(bs, '\'') && !bytesContains(bs, '"') {
		quote = '"'
	}
	var b strings.Builder
	b.WriteString("b")
	b.WriteByte(quote)
	for _, c := range bs {
		switch {
		case c == quote || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c == '\t':
			b.WriteString(`\t`)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\r':
			b.WriteString(`\r`)
		case c < ' ' || c >= 0x7f:
			fmt.Fprintf(&b, `\x%02x`, c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte(quote)
	return b.String()
}

func bytesContains(bs []byte, c byte) bool {
	for _, x := range bs {
		if x == c {
			return true
		}
	}
	return false
}

// pyRepr is repr(v).
func pyRepr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case *big.Int:
		return intStr(x)
	case float64:
		return floatRepr(x)
	case string:
		return strRepr(x)
	case []byte:
		return bytesRepr(x)
	case complex128:
		return complexRepr(x)
	case Ellipsis:
		return "Ellipsis"
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = pyRepr(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case *pyDict:
		parts := make([]string, 0, len(x.keys))
		for _, k := range x.keys {
			parts = append(parts, strRepr(k)+": "+pyRepr(x.vals[k]))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return fmt.Sprint(v)
}

// pyStr is str(v).
func pyStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return pyRepr(v)
}

// truthy is bool(v).
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case *big.Int:
		return x.Sign() != 0
	case float64:
		return x != 0
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case []byte:
		return len(x) > 0
	case complex128:
		return x != 0
	case *pyDict:
		return len(x.keys) > 0
	}
	return true
}

// ---- numbers ----

func isNumber(v any) bool { // bool, int, float (the real tower)
	switch v.(type) {
	case bool, *big.Int, float64:
		return true
	}
	return false
}

func isIntLike(v any) bool {
	switch v.(type) {
	case bool, *big.Int:
		return true
	}
	return false
}

func asBig(v any) *big.Int {
	switch x := v.(type) {
	case bool:
		if x {
			return big.NewInt(1)
		}
		return big.NewInt(0)
	case *big.Int:
		return x
	}
	return nil
}

func bigToFloat(i *big.Int) float64 {
	if i.IsInt64() {
		return float64(i.Int64())
	}
	f, _ := new(big.Float).SetInt(i).Float64()
	if math.IsInf(f, 0) {
		raise("OverflowError", "int too large to convert to float")
	}
	return f
}

func asFloat(v any) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return bigToFloat(asBig(v))
}

func asComplex(v any) complex128 {
	if c, ok := v.(complex128); ok {
		return c
	}
	return complex(asFloat(v), 0)
}

// cmpNum compares two reals exactly; ok is false when either is NaN.
func cmpNum(a, b any) (int, bool) {
	af, aIsF := a.(float64)
	bf, bIsF := b.(float64)
	switch {
	case aIsF && bIsF:
		if math.IsNaN(af) || math.IsNaN(bf) {
			return 0, false
		}
		switch {
		case af < bf:
			return -1, true
		case af > bf:
			return 1, true
		}
		return 0, true
	case aIsF:
		c, ok := cmpFloatBig(af, asBig(b))
		return c, ok
	case bIsF:
		c, ok := cmpFloatBig(bf, asBig(a))
		return -c, ok
	}
	return asBig(a).Cmp(asBig(b)), true
}

func cmpFloatBig(f float64, i *big.Int) (int, bool) {
	if math.IsNaN(f) {
		return 0, false
	}
	if math.IsInf(f, 1) {
		return 1, true
	}
	if math.IsInf(f, -1) {
		return -1, true
	}
	return new(big.Float).SetFloat64(f).Cmp(new(big.Float).SetInt(i)), true
}

// pyEq is ==.
func pyEq(a, b any) bool {
	if isNumber(a) && isNumber(b) {
		c, ok := cmpNum(a, b)
		return ok && c == 0
	}
	if ca, ok := a.(complex128); ok {
		if isNumber(b) || isComplex(b) {
			return ca == asComplex(b)
		}
		return false
	}
	if cb, ok := b.(complex128); ok && isNumber(a) {
		return cb == asComplex(a)
	}
	switch x := a.(type) {
	case nil:
		return b == nil
	case string:
		y, ok := b.(string)
		return ok && x == y
	case []byte:
		y, ok := b.([]byte)
		return ok && string(x) == string(y)
	case Ellipsis:
		_, ok := b.(Ellipsis)
		return ok
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !pyEq(x[i], y[i]) {
				return false
			}
		}
		return true
	case *pyDict:
		y, ok := b.(*pyDict)
		if !ok || len(x.keys) != len(y.keys) {
			return false
		}
		for k, v := range x.vals {
			w, ok := y.vals[k]
			if !ok || !pyEq(v, w) {
				return false
			}
		}
		return true
	}
	return false
}

func isComplex(v any) bool { _, ok := v.(complex128); return ok }

// pyOrder compares for < <= > >=; op is one of "<", "<=", ">", ">=".
func pyOrder(op string, a, b any) bool {
	var c int
	var ok bool
	switch {
	case isNumber(a) && isNumber(b):
		c, ok = cmpNum(a, b)
		if !ok {
			return false // NaN
		}
	default:
		if sa, isS := a.(string); isS {
			if sb, isS2 := b.(string); isS2 {
				c, ok = strings.Compare(sa, sb), true
			}
		} else if ba, isB := a.([]byte); isB {
			if bb, isB2 := b.([]byte); isB2 {
				c, ok = strings.Compare(string(ba), string(bb)), true
			}
		} else if la, isL := a.([]any); isL {
			if lb, isL2 := b.([]any); isL2 {
				return listOrder(op, la, lb)
			}
		}
		if !ok {
			raise("TypeError", "'%s' not supported between instances of '%s' and '%s'", op, typeName(a), typeName(b))
		}
	}
	return orderResult(op, c)
}

func orderResult(op string, c int) bool {
	switch op {
	case "<":
		return c < 0
	case "<=":
		return c <= 0
	case ">":
		return c > 0
	}
	return c >= 0
}

func listOrder(op string, a, b []any) bool {
	i := 0
	for ; i < len(a) && i < len(b); i++ {
		if !pyEq(a[i], b[i]) {
			break
		}
	}
	if i >= len(a) || i >= len(b) {
		return orderResult(op, cmpInt(len(a), len(b)))
	}
	return pyOrder(op, a[i], b[i])
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
