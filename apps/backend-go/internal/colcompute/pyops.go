package colcompute

import (
	"math"
	"math/big"
	"strconv"
	"strings"
)

// Resource caps: Python would grind or die with MemoryError; the evaluator refuses
// with a FormulaError instead (documented deviation).
const (
	maxResultLen = 1 << 22 // characters / items in any string, list or bytes result
	maxIntBits   = 1 << 18 // bits in any integer result
)

func checkInt(i *big.Int) *big.Int {
	if i.BitLen() > maxIntBits {
		raise("OverflowError", "result too large")
	}
	return i
}

func unsupported(op string, l, r any) {
	raise("TypeError", "unsupported operand type(s) for %s: '%s' and '%s'", op, typeName(l), typeName(r))
}

func isSeq(v any) bool {
	switch v.(type) {
	case string, []any, []byte:
		return true
	}
	return false
}

func isNum(v any) bool { return isNumber(v) || isComplex(v) }

// binop is `l <op> r` for the whitelisted operators + - * / // %.
func binop(op string, l, r any) any {
	switch op {
	case "+":
		return opAdd(l, r)
	case "-":
		return opSub(l, r)
	case "*":
		return opMul(l, r)
	case "/":
		return opDiv(l, r)
	case "//":
		return opFloorDiv(l, r)
	case "%":
		return opMod(l, r)
	}
	raise("TypeError", "unsupported operator %s", op)
	return nil
}

func opAdd(l, r any) any {
	_, ls := l.(string)
	_, rs := r.(string)
	if ls || rs { // formula_column: any str operand concatenates str() of both
		a, b := pyStr(l), pyStr(r)
		if len(a)+len(b) > maxResultLen {
			raise("ValueError", "result too large")
		}
		return a + b
	}
	if isNum(l) && isNum(r) {
		if isComplex(l) || isComplex(r) {
			return asComplex(l) + asComplex(r)
		}
		if isIntLike(l) && isIntLike(r) {
			return checkInt(new(big.Int).Add(asBig(l), asBig(r)))
		}
		return asFloat(l) + asFloat(r)
	}
	switch x := l.(type) {
	case []any:
		if y, ok := r.([]any); ok {
			if len(x)+len(y) > maxResultLen {
				raise("ValueError", "result too large")
			}
			return append(append(make([]any, 0, len(x)+len(y)), x...), y...)
		}
		raise("TypeError", "can only concatenate list (not \"%s\") to list", typeName(r))
	case []byte:
		if y, ok := r.([]byte); ok {
			if len(x)+len(y) > maxResultLen {
				raise("ValueError", "result too large")
			}
			return append(append(make([]byte, 0, len(x)+len(y)), x...), y...)
		}
		raise("TypeError", "can't concat %s to bytes", typeName(r))
	}
	unsupported("+", l, r)
	return nil
}

func opSub(l, r any) any {
	if isNum(l) && isNum(r) {
		if isComplex(l) || isComplex(r) {
			return asComplex(l) - asComplex(r)
		}
		if isIntLike(l) && isIntLike(r) {
			return checkInt(new(big.Int).Sub(asBig(l), asBig(r)))
		}
		return asFloat(l) - asFloat(r)
	}
	unsupported("-", l, r)
	return nil
}

func opMul(l, r any) any {
	if isNum(l) && isNum(r) {
		if isComplex(l) || isComplex(r) {
			return asComplex(l) * asComplex(r)
		}
		if isIntLike(l) && isIntLike(r) {
			a, b := asBig(l), asBig(r)
			if a.BitLen()+b.BitLen() > maxIntBits+1 {
				raise("OverflowError", "result too large")
			}
			return checkInt(new(big.Int).Mul(a, b))
		}
		return asFloat(l) * asFloat(r)
	}
	switch {
	case isSeq(l):
		if !isIntLike(r) {
			raise("TypeError", "can't multiply sequence by non-int of type '%s'", typeName(r))
		}
		return repeat(l, asBig(r))
	case isSeq(r):
		if !isIntLike(l) {
			raise("TypeError", "can't multiply sequence by non-int of type '%s'", typeName(l))
		}
		return repeat(r, asBig(l))
	}
	unsupported("*", l, r)
	return nil
}

func repeat(seq any, n *big.Int) any {
	if !n.IsInt64() {
		raise("OverflowError", "cannot fit 'int' into an index-sized integer")
	}
	k := n.Int64()
	var length int
	switch s := seq.(type) {
	case string:
		length = len(s)
	case []any:
		length = len(s)
	case []byte:
		length = len(s)
	}
	if k > 0 && length > 0 && k > int64(maxResultLen/length) {
		raise("ValueError", "result too large")
	}
	if k < 0 {
		k = 0
	}
	switch s := seq.(type) {
	case string:
		return strings.Repeat(s, int(k))
	case []byte:
		out := make([]byte, 0, len(s)*int(k))
		for i := int64(0); i < k; i++ {
			out = append(out, s...)
		}
		return out
	case []any:
		out := make([]any, 0, len(s)*int(k))
		for i := int64(0); i < k; i++ {
			out = append(out, s...)
		}
		return out
	}
	return nil
}

func opDiv(l, r any) any {
	if !(isNum(l) && isNum(r)) {
		unsupported("/", l, r)
	}
	if isComplex(l) || isComplex(r) {
		b := asComplex(r)
		if b == 0 {
			raise("ZeroDivisionError", "division by zero")
		}
		return asComplex(l) / b
	}
	if isIntLike(l) && isIntLike(r) {
		a, b := asBig(l), asBig(r)
		if b.Sign() == 0 {
			raise("ZeroDivisionError", "division by zero")
		}
		if a.IsInt64() && b.IsInt64() && abs64(a.Int64()) < 1<<53 && abs64(b.Int64()) < 1<<53 {
			return float64(a.Int64()) / float64(b.Int64())
		}
		f, _ := new(big.Rat).SetFrac(a, b).Float64()
		if math.IsInf(f, 0) {
			raise("OverflowError", "integer division result too large for a float")
		}
		return f
	}
	a, b := asFloat(l), asFloat(r)
	if b == 0 {
		raise("ZeroDivisionError", "float division by zero")
	}
	return a / b
}

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// floorDivMod is Python's int divmod (floor), b != 0.
func floorDivMod(a, b *big.Int) (*big.Int, *big.Int) {
	q, m := new(big.Int).QuoRem(a, b, new(big.Int))
	if m.Sign() != 0 && (m.Sign() < 0) != (b.Sign() < 0) {
		q.Sub(q, big.NewInt(1))
		m.Add(m, b)
	}
	return q, m
}

// floatDivMod is CPython's float_divmod, wx != 0.
func floatDivMod(vx, wx float64) (float64, float64) {
	mod := math.Mod(vx, wx)
	div := (vx - mod) / wx
	if mod != 0 {
		if (wx < 0) != (mod < 0) {
			mod += wx
			div--
		}
	} else {
		mod = math.Copysign(0, wx)
	}
	var floordiv float64
	if div != 0 {
		floordiv = math.Floor(div)
		if div-floordiv > 0.5 {
			floordiv++
		}
	} else {
		floordiv = math.Copysign(0, vx/wx)
	}
	return floordiv, mod
}

func opFloorDiv(l, r any) any {
	if !(isNumber(l) && isNumber(r)) {
		unsupported("//", l, r)
	}
	if isIntLike(l) && isIntLike(r) {
		b := asBig(r)
		if b.Sign() == 0 {
			raise("ZeroDivisionError", "integer division or modulo by zero")
		}
		q, _ := floorDivMod(asBig(l), b)
		return checkInt(q)
	}
	a, b := asFloat(l), asFloat(r)
	if b == 0 {
		raise("ZeroDivisionError", "float floor division by zero")
	}
	q, _ := floatDivMod(a, b)
	return q
}

func opMod(l, r any) any {
	if s, ok := l.(string); ok {
		return percentFormat(s, r)
	}
	if !(isNumber(l) && isNumber(r)) {
		unsupported("%", l, r)
	}
	if isIntLike(l) && isIntLike(r) {
		b := asBig(r)
		if b.Sign() == 0 {
			raise("ZeroDivisionError", "integer modulo by zero")
		}
		_, m := floorDivMod(asBig(l), b)
		return m
	}
	a, b := asFloat(l), asFloat(r)
	if b == 0 {
		raise("ZeroDivisionError", "float modulo by zero")
	}
	_, m := floatDivMod(a, b)
	return m
}

func unaryOp(op string, v any) any {
	switch x := v.(type) {
	case bool, *big.Int:
		i := asBig(x)
		if op == "-" {
			return new(big.Int).Neg(i)
		}
		return new(big.Int).Set(i)
	case float64:
		if op == "-" {
			return -x
		}
		return x
	case complex128:
		if op == "-" {
			return -x
		}
		return x
	}
	raise("TypeError", "bad operand type for unary %s: '%s'", op, typeName(v))
	return nil
}

// ---- subscripts ----

func notSubscriptable(v any) {
	raise("TypeError", "'%s' object is not subscriptable", typeName(v))
}

// subscript is target[key] for a non-slice key; out-of-range yields "" (formula_column
// swallows IndexError/KeyError).
func subscript(target, key any) any {
	switch t := target.(type) {
	case string:
		if !isIntLike(key) {
			raise("TypeError", "string indices must be integers, not '%s'", typeName(key))
		}
		rs := []rune(t)
		i, ok := seqIdx(len(rs), asBig(key))
		if !ok {
			return ""
		}
		return string(rs[i])
	case []any:
		if !isIntLike(key) {
			raise("TypeError", "list indices must be integers or slices, not %s", typeName(key))
		}
		i, ok := seqIdx(len(t), asBig(key))
		if !ok {
			return ""
		}
		return t[i]
	case []byte:
		if !isIntLike(key) {
			raise("TypeError", "byte indices must be integers or slices, not %s", typeName(key))
		}
		i, ok := seqIdx(len(t), asBig(key))
		if !ok {
			return ""
		}
		return big.NewInt(int64(t[i]))
	}
	notSubscriptable(target)
	return nil
}

func seqIdx(n int, k *big.Int) (int, bool) {
	if !k.IsInt64() {
		return 0, false
	}
	i := k.Int64()
	if i < 0 {
		i += int64(n)
	}
	if i < 0 || i >= int64(n) {
		return 0, false
	}
	return int(i), true
}

func sliceBound(v any) (int64, bool) { // value, present
	switch x := v.(type) {
	case nil:
		return 0, false
	case bool, *big.Int:
		b := asBig(x)
		switch {
		case b.IsInt64():
			return b.Int64(), true
		case b.Sign() > 0:
			return math.MaxInt64, true
		}
		return math.MinInt64, true
	}
	raise("TypeError", "slice indices must be integers or None or have an __index__ method")
	return 0, false
}

// sliceTarget is target[lo:hi:step].
func sliceTarget(target, lo, hi, step any) any {
	var n int
	switch t := target.(type) {
	case string:
		n = runeLen(t)
	case []any:
		n = len(t)
	case []byte:
		n = len(t)
	default:
		notSubscriptable(target)
	}
	stepV, hasStep := sliceBound(step)
	if !hasStep {
		stepV = 1
	}
	loV, hasLo := sliceBound(lo)
	hiV, hasHi := sliceBound(hi)
	if hasStep && stepV == 0 {
		raise("ValueError", "slice step cannot be zero")
	}
	if stepV < -math.MaxInt64 {
		stepV = -math.MaxInt64
	}
	if !hasLo {
		if stepV < 0 {
			loV = math.MaxInt64
		} else {
			loV = 0
		}
	}
	if !hasHi {
		if stepV < 0 {
			hiV = math.MinInt64
		} else {
			hiV = math.MaxInt64
		}
	}
	length := int64(n)
	adjust := func(v int64) int64 {
		if v < 0 {
			if v > math.MinInt64+length { // v += length without overflow
				v += length
			} else {
				v = -1
			}
			if v < 0 {
				if stepV < 0 {
					return -1
				}
				return 0
			}
		} else if v >= length {
			if stepV < 0 {
				return length - 1
			}
			return length
		}
		return v
	}
	start, stop := adjust(loV), adjust(hiV)
	var idx []int
	if stepV < 0 {
		for i := start; i > stop; i += stepV {
			idx = append(idx, int(i))
			if i+stepV > i { // overflow guard
				break
			}
		}
	} else {
		for i := start; i < stop; i += stepV {
			idx = append(idx, int(i))
			if i+stepV < i {
				break
			}
		}
	}
	switch t := target.(type) {
	case string:
		rs := []rune(t)
		out := make([]rune, len(idx))
		for k, i := range idx {
			out[k] = rs[i]
		}
		return string(out)
	case []byte:
		out := make([]byte, len(idx))
		for k, i := range idx {
			out[k] = t[i]
		}
		return out
	case []any:
		out := make([]any, len(idx))
		for k, i := range idx {
			out[k] = t[i]
		}
		return out
	}
	return nil
}

// ---- str % args ----

func percentFormat(format string, arg any) string {
	var args []any
	argIdx := 0
	// Python tuples become lists in this evaluator, so a list is one argument (as in
	// `"%s-%s" % (1, 2)` raising "not enough arguments" in formula_column).
	args = []any{arg}
	next := func() any {
		if argIdx < len(args) {
			a := args[argIdx]
			argIdx++
			return a
		}
		raise("TypeError", "not enough arguments for format string")
		return nil
	}
	rs := []rune(format)
	var out strings.Builder
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		if c != '%' {
			out.WriteRune(c)
			continue
		}
		i++
		if i >= len(rs) {
			raise("ValueError", "incomplete format")
		}
		if rs[i] == '(' { // %(name)s needs a mapping; a list is not one
			depth, j := 1, i+1
			for j < len(rs) && depth > 0 {
				switch rs[j] {
				case '(':
					depth++
				case ')':
					depth--
				}
				j++
			}
			if depth > 0 {
				raise("ValueError", "incomplete format key")
			}
			raise("TypeError", "list indices must be integers or slices, not str")
		}
		var left, plus, space, alt, zero bool
	flags:
		for i < len(rs) {
			switch rs[i] {
			case '-':
				left = true
			case '+':
				plus = true
			case ' ':
				space = true
			case '#':
				alt = true
			case '0':
				zero = true
			default:
				break flags
			}
			i++
		}
		width, prec := -1, -1
		readNum := func(star bool) int {
			if star && i < len(rs) && rs[i] == '*' {
				i++
				a := next()
				if !isIntLike(a) {
					raise("TypeError", "* wants int")
				}
				v := asBig(a)
				if !v.IsInt64() || v.Int64() > 1<<20 || v.Int64() < -(1<<20) {
					raise("ValueError", "width too big")
				}
				return int(v.Int64())
			}
			n, any := 0, false
			for i < len(rs) && rs[i] >= '0' && rs[i] <= '9' {
				n = n*10 + int(rs[i]-'0')
				if n > 1<<20 {
					raise("ValueError", "width too big")
				}
				i++
				any = true
			}
			if !any {
				return -1
			}
			return n
		}
		width = readNum(true)
		if width < -1 {
			left, width = true, -width
		}
		if i < len(rs) && rs[i] == '.' {
			i++
			prec = readNum(true)
			if prec < 0 {
				prec = 0
			}
		}
		for i < len(rs) && (rs[i] == 'h' || rs[i] == 'l' || rs[i] == 'L') {
			i++
		}
		if i >= len(rs) {
			raise("ValueError", "incomplete format")
		}
		conv := rs[i]
		if conv == '%' {
			out.WriteByte('%')
			continue
		}
		a := next()
		var body, sign string
		numeric := false
		switch conv {
		case 's', 'r', 'a':
			switch conv {
			case 's':
				body = pyStr(a)
			case 'r':
				body = pyRepr(a)
			default:
				body = asciiRepr(pyRepr(a))
			}
			if prec >= 0 {
				if r := []rune(body); len(r) > prec {
					body = string(r[:prec])
				}
			}
		case 'c':
			switch x := a.(type) {
			case string:
				if runeLen(x) != 1 {
					raise("TypeError", "%%c requires an int or a unicode character, not a string of length %d", runeLen(x))
				}
				body = x
			case bool, *big.Int:
				v := asBig(x)
				if !v.IsInt64() || v.Int64() < 0 || v.Int64() > 0x10ffff {
					raise("OverflowError", "%%c arg not in range(0x110000)")
				}
				body = string(rune(v.Int64()))
			default:
				raise("TypeError", "%%c requires an int or a unicode character, not %s", typeName(a))
			}
		case 'd', 'i', 'u':
			numeric = true
			v := percentInt(conv, a)
			if v.Sign() < 0 {
				sign = "-"
				v = new(big.Int).Neg(v)
			}
			body = intStr(v)
			if prec >= 0 && len(body) < prec {
				body = strings.Repeat("0", prec-len(body)) + body
			}
		case 'o', 'x', 'X':
			numeric = true
			v := percentInt(conv, a)
			if v.Sign() < 0 {
				sign = "-"
				v = new(big.Int).Neg(v)
			}
			base := map[rune]int{'o': 8, 'x': 16, 'X': 16}[conv]
			body = v.Text(base)
			if prec >= 0 && len(body) < prec {
				body = strings.Repeat("0", prec-len(body)) + body
			}
			if alt {
				pre := map[rune]string{'o': "0o", 'x': "0x", 'X': "0X"}[conv]
				body = pre + body
			}
			if conv == 'X' {
				body = strings.ToUpper(body)
			}
		case 'e', 'E', 'f', 'F', 'g', 'G':
			numeric = true
			if !isNumber(a) {
				raise("TypeError", "must be real number, not %s", typeName(a))
			}
			f := asFloat(a)
			if math.Signbit(f) {
				sign = "-"
				f = -f
			}
			p := prec
			if p < 0 {
				p = 6
			}
			switch {
			case math.IsNaN(f):
				body = "nan"
			case math.IsInf(f, 0):
				body = "inf"
			default:
				fc := byte(conv)
				if fc == 'F' {
					fc = 'f'
				}
				lower := fc | 0x20
				if lower == 'g' && p == 0 {
					p = 1
				}
				body = strconv.FormatFloat(f, lower, p, 64)
				if alt && lower == 'f' && p == 0 {
					body += "."
				}
			}
			if conv == 'E' || conv == 'F' || conv == 'G' {
				body = strings.ToUpper(body)
			}
		default:
			raise("ValueError", "unsupported format character '%s' (0x%x) at index %d", string(conv), conv, i)
		}
		if numeric && sign == "" {
			switch {
			case plus:
				sign = "+"
			case space:
				sign = " "
			}
		}
		total := runeLen(body) + len(sign)
		if width > total {
			pad := width - total
			switch {
			case left:
				body = sign + body + strings.Repeat(" ", pad)
			case zero && numeric && !strings.HasSuffix(body, "inf") && !strings.HasSuffix(body, "nan") && !strings.HasSuffix(body, "INF") && !strings.HasSuffix(body, "NAN"):
				body = sign + strings.Repeat("0", pad) + body
			default:
				body = strings.Repeat(" ", pad) + sign + body
			}
		} else {
			body = sign + body
		}
		out.WriteString(body)
		if out.Len() > maxResultLen {
			raise("ValueError", "result too large")
		}
	}
	if argIdx < len(args) && !isMappingLike(arg) {
		raise("TypeError", "not all arguments converted during string formatting")
	}
	return out.String()
}

// isMappingLike: PyMapping_Check minus str/tuple (a list or bytes counts, so it never triggers
// "not all arguments converted").
func isMappingLike(v any) bool {
	switch v.(type) {
	case []any, []byte, *pyDict:
		return true
	}
	return false
}

func percentInt(conv rune, a any) *big.Int {
	switch x := a.(type) {
	case bool, *big.Int:
		return asBig(x)
	case float64:
		if conv == 'd' || conv == 'i' || conv == 'u' {
			if math.IsNaN(x) {
				raise("ValueError", "cannot convert float NaN to integer")
			}
			if math.IsInf(x, 0) {
				raise("OverflowError", "cannot convert float infinity to integer")
			}
			i, _ := new(big.Float).SetFloat64(math.Trunc(x)).Int(nil)
			return i
		}
		raise("TypeError", "%%%c format: an integer is required, not float", conv)
	}
	if conv == 'd' || conv == 'i' || conv == 'u' {
		raise("TypeError", "%%%c format: a real number is required, not %s", conv, typeName(a))
	}
	raise("TypeError", "%%%c format: an integer is required, not %s", conv, typeName(a))
	return nil
}

func asciiRepr(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < 0x80:
			b.WriteRune(r)
		case r < 0x100:
			b.WriteString("\\x" + strconv.FormatInt(int64(r)+0x100, 16)[1:])
		case r < 0x10000:
			b.WriteString("\\u" + strconv.FormatInt(int64(r)+0x10000, 16)[1:])
		default:
			b.WriteString("\\U" + strconv.FormatInt(int64(r)+0x100000000, 16)[1:])
		}
	}
	return b.String()
}
