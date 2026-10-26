package colcompute

import (
	"fmt"
	"math"
	"math/big"
	"sort"
	"strings"
	"time"
)

// FormulaError is the Python FormulaError: its text is what lands in the cell's error.
type FormulaError struct{ Msg string }

func (e *FormulaError) Error() string { return e.Msg }

const maxFormulaLen = 100_000 // bytes of expression text (deviation: Python has no cap)

// evalTimeout bounds one evaluation (deviation: Python has no deadline).
var evalTimeout = 100 * time.Millisecond

// ---- reference substitution ----

// replaceFormulaReferences replaces every {column} reference that is outside Python string
// literals and comments; replace receives the raw text between the braces.
func replaceFormulaReferences(expr string, replace func(raw string) string) string {
	rs := []rune(expr)
	var out strings.Builder
	clamp := func(i int) int {
		if i > len(rs) {
			return len(rs)
		}
		return i
	}
	hasPrefix := func(at int, s string) bool {
		sr := []rune(s)
		if at+len(sr) > len(rs) {
			return false
		}
		for i, r := range sr {
			if rs[at+i] != r {
				return false
			}
		}
		return true
	}
	cursor := 0
	for cursor < len(rs) {
		start := cursor
		ch := rs[cursor]
		switch {
		case ch == '\'' || ch == '"':
			delim := string(ch)
			if hasPrefix(cursor, strings.Repeat(string(ch), 3)) {
				delim = strings.Repeat(string(ch), 3)
			}
			cursor += len(delim)
			for cursor < len(rs) {
				if rs[cursor] == '\\' {
					cursor += 2
				} else if hasPrefix(cursor, delim) {
					cursor += len(delim)
					break
				} else {
					cursor++
				}
			}
			out.WriteString(string(rs[start:clamp(cursor)]))
		case ch == '#':
			end := cursor
			for end < len(rs) && rs[end] != '\n' {
				end++
			}
			cursor = end
			out.WriteString(string(rs[start:cursor]))
		case ch == '{':
			if raw, end, ok := matchPlaceholder(rs, cursor); ok {
				out.WriteString(replace(raw))
				cursor = end
			} else {
				out.WriteRune(ch)
				cursor++
			}
		default:
			out.WriteRune(ch)
			cursor++
		}
	}
	return out.String()
}

// matchPlaceholder is re.match(r"\{([^}]+)\}", s, at).
func matchPlaceholder(rs []rune, at int) (group string, end int, ok bool) {
	k := at + 1
	for k < len(rs) && rs[k] != '}' {
		k++
	}
	if k >= len(rs) || k == at+1 {
		return "", 0, false
	}
	return string(rs[at+1 : k]), k + 1, true
}

// FormulaReferences are the {column} references an expression evaluates, excluding string
// literals and comments, de-duplicated and sorted.
func FormulaReferences(expr string) []string {
	seen := map[string]bool{}
	replaceFormulaReferences(expr, func(raw string) string {
		seen[pyStripSpace(raw)] = true
		return raw
	})
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---- evaluation ----

type kv struct {
	k string
	v any
}

type rowLookup []kv

// get is the exact-then-case-insensitive lookup of evaluate_formula (first match in order).
func (r rowLookup) get(key string) any {
	for _, e := range r {
		if e.k == key {
			if e.v != nil {
				return e.v
			}
			break
		}
	}
	kl := pyLower(key)
	for _, e := range r {
		if pyLower(e.k) == kl {
			return e.v
		}
	}
	return nil
}

// EvalFormula evaluates expr against row (column -> value) exactly as
// formula_column.evaluate_formula does: {column} references resolve exactly, then case
// insensitively (ambiguity resolved in sorted key order, Python uses dict order); missing
// references are "". Every failure is a *FormulaError whose text equals Python's.
func EvalFormula(expr string, row map[string]any) (any, error) {
	keys := make([]string, 0, len(row))
	for k := range row {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	lk := make(rowLookup, 0, len(keys))
	for _, k := range keys {
		lk = append(lk, kv{k, row[k]})
	}
	return evalFormula(expr, lk)
}

func evalFormula(expr string, row rowLookup) (result any, err error) {
	if strings.TrimFunc(expr, pyIsSpace) == "" {
		return nil, &FormulaError{"empty formula"}
	}
	if len(expr) > maxFormulaLen {
		return nil, &FormulaError{fmt.Sprintf("formula too long (more than %d characters)", maxFormulaLen)}
	}
	defer func() {
		if r := recover(); r != nil {
			pe, ok := r.(*pyError)
			if !ok {
				panic(r)
			}
			result, err = nil, &FormulaError{pe.msg}
		}
	}()
	vars := map[string]any{}
	nameFor := map[string]string{}
	safe := replaceFormulaReferences(expr, func(raw string) string {
		key := pyStripSpace(raw)
		if name, ok := nameFor[key]; ok {
			return name
		}
		name := fmt.Sprintf("_v%d", len(nameFor))
		nameFor[key] = name
		val := norm(row.get(key))
		if val == nil {
			val = ""
		}
		vars[name] = val
		return name
	})
	tree, serr, budget := parseExpression(safe)
	if budget {
		return nil, &FormulaError{fmt.Sprintf("formula too complex (more than %d nodes)", maxNodes)}
	}
	if serr != nil {
		return nil, &FormulaError{"syntax error: " + serr.String()}
	}
	validate(tree)
	ev := &evaluator{vars: vars, deadline: time.Now().Add(evalTimeout)}
	return ev.eval(tree), nil
}

var allowedKinds = map[string]bool{
	"Expression": true, "Constant": true, "Name": true, "Load": true,
	"BinOp": true, "Add": true, "Sub": true, "Mult": true, "Div": true, "Mod": true, "FloorDiv": true,
	"UnaryOp": true, "UAdd": true, "USub": true,
	"Call": true, "Attribute": true, "Subscript": true, "Slice": true,
	"List": true, "Tuple": true,
	"Compare": true, "Eq": true, "NotEq": true, "Lt": true, "LtE": true, "Gt": true, "GtE": true,
	"BoolOp": true, "And": true, "Or": true, "IfExp": true,
}

var strMethods = map[string]bool{"split": true, "strip": true, "lower": true, "upper": true, "replace": true, "title": true,
	"lstrip": true, "rstrip": true, "startswith": true, "endswith": true, "zfill": true, "find": true, "join": true}

// validate is _validate: a breadth-first ast.walk that stops at the first offender.
func validate(root *node) {
	queue := []*node{root}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		if !allowedKinds[n.kind] {
			raise("FormulaError", "disallowed expression element: %s", n.kind)
		}
		if n.kind == "Call" {
			switch fn := n.x; fn.kind {
			case "Name":
				if _, ok := funcTable[fn.name]; !ok {
					raise("FormulaError", "unknown function: %s", fn.name)
				}
			case "Attribute":
				if !strMethods[fn.name] {
					raise("FormulaError", "method not allowed: .%s", fn.name)
				}
			default:
				raise("FormulaError", "unsupported call target")
			}
		}
		if n.kind == "Attribute" && strings.HasPrefix(n.name, "_") {
			raise("FormulaError", "dunder/private access not allowed")
		}
		queue = append(queue, n.kids...)
	}
}

type evaluator struct {
	vars     map[string]any
	deadline time.Time
	count    int
}

func (ev *evaluator) tick() {
	ev.count++
	if ev.count > maxNodes {
		raise("FormulaError", "formula too complex (more than %d nodes)", maxNodes)
	}
	if !time.Now().Before(ev.deadline) {
		raise("FormulaError", "formula evaluation timed out")
	}
}

func (ev *evaluator) eval(n *node) any {
	ev.tick()
	switch n.kind {
	case "Expression":
		return ev.eval(n.x)
	case "Constant":
		return n.val
	case "Name":
		v, ok := ev.vars[n.name]
		if !ok {
			raise("FormulaError", "unknown name: %s", n.name)
		}
		return v
	case "BinOp":
		l := ev.eval(n.x)
		r := ev.eval(n.y)
		return binop(n.op, l, r)
	case "UnaryOp":
		return unaryOp(n.op, ev.eval(n.x))
	case "Call":
		args := make([]any, len(n.list))
		for i, a := range n.list {
			args[i] = ev.eval(a)
		}
		if n.x.kind == "Name" {
			return callFunc(n.x.name, args)
		}
		target := ev.eval(n.x.x)
		return callMethod(n.x.name, pyStr(target), args)
	case "Attribute":
		raise("FormulaError", "bare attribute access not allowed")
	case "Subscript":
		target := ev.eval(n.x)
		if n.y.kind == "Slice" {
			var lo, hi, step any
			if n.y.x != nil {
				lo = ev.eval(n.y.x)
			}
			if n.y.y != nil {
				hi = ev.eval(n.y.y)
			}
			if n.y.z != nil {
				step = ev.eval(n.y.z)
			}
			return sliceTarget(target, lo, hi, step)
		}
		return subscript(target, ev.eval(n.y))
	case "List", "Tuple":
		out := make([]any, len(n.list))
		for i, e := range n.list {
			out[i] = ev.eval(e)
		}
		return out
	case "Compare":
		left := ev.eval(n.x)
		for i, o := range n.ops {
			right := ev.eval(n.list[i])
			var ok bool
			switch o {
			case "==":
				ok = pyEq(left, right)
			case "!=":
				ok = !pyEq(left, right)
			default:
				ok = pyOrder(o, left, right)
			}
			if !ok {
				return false
			}
			left = right
		}
		return true
	case "BoolOp":
		value := ev.eval(n.list[0])
		for _, operand := range n.list[1:] {
			if n.op == "and" && !truthy(value) {
				return value
			}
			if n.op == "or" && truthy(value) {
				return value
			}
			value = ev.eval(operand)
		}
		return value
	case "IfExp":
		if truthy(ev.eval(n.x)) {
			return ev.eval(n.y)
		}
		return ev.eval(n.z)
	}
	raise("FormulaError", "cannot evaluate %s", n.kind)
	return nil
}

// ---- whitelisted functions ----

type funcDef struct {
	name     string // as Python reports it in arity errors
	params   []string
	required int
	variadic bool
	call     func(a []any) any
}

var funcTable map[string]*funcDef

func init() {
	lambda := "<lambda>"
	funcTable = map[string]*funcDef{
		"default": {name: "_default", variadic: true, call: fnDefault},
		"lower":   {name: lambda, params: []string{"s"}, required: 1, call: func(a []any) any { return pyLower(pyStr(a[0])) }},
		"upper":   {name: lambda, params: []string{"s"}, required: 1, call: func(a []any) any { return pyUpper(pyStr(a[0])) }},
		"strip":   {name: lambda, params: []string{"s"}, required: 1, call: func(a []any) any { return pyStripSpace(pyStr(a[0])) }},
		"trim":    {name: lambda, params: []string{"s"}, required: 1, call: func(a []any) any { return pyStripSpace(pyStr(a[0])) }},
		"len":     {name: lambda, params: []string{"s"}, required: 1, call: fnLen},
		"str":     {name: lambda, params: []string{"s"}, required: 1, call: func(a []any) any { return pyStr(a[0]) }},
		"int":     {name: "_to_int", params: []string{"value"}, required: 1, call: fnInt},
		"float":   {name: lambda, params: []string{"s"}, required: 1, call: fnFloat},
		"round":   {name: lambda, params: []string{"s", "n"}, required: 1, call: fnRound},
		"title":   {name: lambda, params: []string{"s"}, required: 1, call: func(a []any) any { return pyTitle(pyStr(a[0])) }},
		"replace": {name: lambda, params: []string{"s", "a", "b"}, required: 3, call: fnReplace},
		"concat":  {name: lambda, variadic: true, call: fnConcat},
	}
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func quotedList(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = "'" + n + "'"
	}
	switch len(q) {
	case 1:
		return q[0]
	case 2:
		return q[0] + " and " + q[1]
	}
	return strings.Join(q[:len(q)-1], ", ") + ", and " + q[len(q)-1]
}

func callFunc(name string, args []any) any {
	f := funcTable[name]
	if !f.variadic {
		max := len(f.params)
		if len(args) > max {
			span := fmt.Sprint(max)
			if f.required != max {
				span = fmt.Sprintf("from %d to %d", f.required, max)
			}
			verb := "were"
			if len(args) == 1 {
				verb = "was"
			}
			raise("TypeError", "%s() takes %s positional argument%s but %d %s given", f.name, span, pluralS(max), len(args), verb)
		}
		if len(args) < f.required {
			missing := f.params[len(args):f.required]
			raise("TypeError", "%s() missing %d required positional argument%s: %s", f.name, len(missing), pluralS(len(missing)), quotedList(missing))
		}
	}
	return f.call(args)
}

func fnDefault(a []any) any {
	for _, v := range a {
		switch x := v.(type) {
		case nil:
			continue
		case string:
			if x == "" {
				continue
			}
		case []any:
			if len(x) == 0 {
				continue
			}
		case *pyDict:
			if len(x.keys) == 0 {
				continue
			}
		}
		return v
	}
	return ""
}

func fnLen(a []any) any {
	switch x := a[0].(type) {
	case string:
		return big.NewInt(int64(runeLen(x)))
	case []any:
		return big.NewInt(int64(len(x)))
	case []byte:
		return big.NewInt(int64(len(x)))
	case *pyDict:
		return big.NewInt(int64(len(x.keys)))
	}
	raise("TypeError", "object of type '%s' has no len()", typeName(a[0]))
	return nil
}

// builtinInt is int(value) for the types a row value can hold.
func builtinInt(v any) *big.Int {
	switch x := v.(type) {
	case bool, *big.Int:
		return new(big.Int).Set(asBig(x))
	case float64:
		if math.IsNaN(x) {
			raise("ValueError", "cannot convert float NaN to integer")
		}
		if math.IsInf(x, 0) {
			raise("OverflowError", "cannot convert float infinity to integer")
		}
		i, _ := new(big.Float).SetFloat64(math.Trunc(x)).Int(nil)
		return i
	case string:
		if i, ok := parsePyInt(x); ok {
			return i
		}
		raise("ValueError", "invalid literal for int() with base 10: %s", strRepr(x))
	case []byte:
		if i, ok := parsePyInt(string(x)); ok {
			return i
		}
		raise("ValueError", "invalid literal for int() with base 10: %s", bytesRepr(x))
	}
	raise("TypeError", "int() argument must be a string, a bytes-like object or a real number, not '%s'", typeName(v))
	return nil
}

// builtinFloat is float(value).
func builtinFloat(v any) float64 {
	switch x := v.(type) {
	case bool, *big.Int:
		return bigToFloat(asBig(x))
	case float64:
		return x
	case string:
		if f, ok := parsePyFloat(x); ok {
			return f
		}
		raise("ValueError", "could not convert string to float: %s", strRepr(x))
	case []byte:
		if f, ok := parsePyFloat(string(x)); ok {
			return f
		}
		raise("ValueError", "could not convert string to float: %s", bytesRepr(x))
	}
	raise("TypeError", "float() argument must be a string or a real number, not '%s'", typeName(v))
	return 0
}

func blankText(v any) bool {
	t := pyStripSpace(pyStr(v))
	return t == "" || t == "None"
}

// fnInt is _to_int: blank -> 0, int(value), else int(float(value)).
func fnInt(a []any) any {
	v := a[0]
	if blankText(v) {
		return big.NewInt(0)
	}
	var result *big.Int
	func() {
		defer func() {
			if r := recover(); r != nil {
				pe, ok := r.(*pyError)
				if !ok || (pe.kind != "TypeError" && pe.kind != "ValueError") {
					panic(r)
				}
				result = nil
			}
		}()
		result = builtinInt(v)
	}()
	if result != nil {
		return result
	}
	return builtinInt(builtinFloat(v))
}

func fnFloat(a []any) any {
	if blankText(a[0]) {
		return 0.0
	}
	return builtinFloat(a[0])
}

func fnRound(a []any) any {
	f := builtinFloat(a[0])
	nd := int64(0)
	if len(a) > 1 {
		n := builtinInt(a[1])
		switch {
		case n.IsInt64():
			nd = n.Int64()
		case n.Sign() > 0:
			nd = math.MaxInt64
		default:
			nd = math.MinInt64
		}
	}
	return roundFloat(f, nd)
}

// roundFloat is round(float, ndigits): exact decimal rounding, ties to even.
func roundFloat(x float64, nd int64) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return x
	}
	if nd > 323 {
		return x
	}
	if nd < -308 {
		return 0 * x
	}
	rat := new(big.Rat).SetFloat64(x)
	abs := nd
	if abs < 0 {
		abs = -abs
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(abs), nil)
	if nd >= 0 {
		rat.Mul(rat, new(big.Rat).SetInt(scale))
	} else {
		rat.Quo(rat, new(big.Rat).SetInt(scale))
	}
	// round half to even
	neg := rat.Sign() < 0
	num, den := new(big.Int).Abs(rat.Num()), rat.Denom()
	q, r := new(big.Int).QuoRem(num, den, new(big.Int))
	twice := new(big.Int).Lsh(r, 1)
	switch c := twice.Cmp(den); {
	case c > 0, c == 0 && q.Bit(0) == 1:
		q.Add(q, big.NewInt(1))
	}
	if neg {
		q.Neg(q)
	}
	res := new(big.Rat).SetInt(q)
	if nd >= 0 {
		res.Quo(res, new(big.Rat).SetInt(scale))
	} else {
		res.Mul(res, new(big.Rat).SetInt(scale))
	}
	f, _ := res.Float64()
	if math.IsInf(f, 0) {
		raise("OverflowError", "rounded value too large to represent")
	}
	if f == 0 {
		return math.Copysign(0, x)
	}
	return f
}

func fnReplace(a []any) any {
	s := pyStr(a[0])
	old, ok := a[1].(string)
	if !ok {
		raise("TypeError", "replace() argument 1 must be str, not %s", argTypeName(a[1]))
	}
	nw, ok := a[2].(string)
	if !ok {
		raise("TypeError", "replace() argument 2 must be str, not %s", argTypeName(a[2]))
	}
	out := strings.ReplaceAll(s, old, nw)
	if len(out) > maxResultLen {
		raise("ValueError", "result too large")
	}
	return out
}

func fnConcat(a []any) any {
	var b strings.Builder
	for _, v := range a {
		b.WriteString(pyStr(v))
		if b.Len() > maxResultLen {
			raise("ValueError", "result too large")
		}
	}
	return b.String()
}

// ---- whitelisted str methods ----

func indexArg(v any) int64 { // anything with __index__: int and bool
	if !isIntLike(v) {
		raise("TypeError", "'%s' object cannot be interpreted as an integer", typeName(v))
	}
	b := asBig(v)
	switch {
	case b.IsInt64():
		return b.Int64()
	case b.Sign() > 0:
		return math.MaxInt64
	}
	return math.MinInt64
}

func arityRange(method string, args []any, min, max int) {
	if len(args) < min {
		raise("TypeError", "%s expected at least %d argument%s, got %d", method, min, pluralS(min), len(args))
	}
	if len(args) > max {
		raise("TypeError", "%s expected at most %d argument%s, got %d", method, max, pluralS(max), len(args))
	}
}

// clinicArity is the arity error of replace() and split(), which CPython words differently.
func clinicArity(method string, args []any, min, max int) {
	if len(args) < min {
		raise("TypeError", "%s() takes at least %d positional argument%s (%d given)", method, min, pluralS(min), len(args))
	}
	if len(args) > max {
		raise("TypeError", "%s() takes at most %d argument%s (%d given)", method, max, pluralS(max), len(args))
	}
}

func noArgs(method string, args []any) {
	if len(args) > 0 {
		raise("TypeError", "str.%s() takes no arguments (%d given)", method, len(args))
	}
}

func stripChars(method string, s string, args []any) string {
	arityRange(method, args, 0, 1)
	var chars *string
	if len(args) == 1 && args[0] != nil {
		c, ok := args[0].(string)
		if !ok {
			raise("TypeError", "%s arg must be None or str", method)
		}
		chars = &c
	}
	pred := pyIsSpace
	if chars != nil {
		set := *chars
		pred = func(r rune) bool { return strings.ContainsRune(set, r) }
	}
	switch method {
	case "lstrip":
		return strings.TrimLeftFunc(s, pred)
	case "rstrip":
		return strings.TrimRightFunc(s, pred)
	}
	return strings.TrimFunc(s, pred)
}

// adjustRange is ADJUST_INDICES for start/end optional arguments over n code points.
func adjustRange(n int, args []any) (int, int) {
	start, end := int64(0), int64(n)
	bound := func(v any) (int64, bool) {
		if v == nil {
			return 0, false
		}
		if !isIntLike(v) {
			raise("TypeError", "slice indices must be integers or None or have an __index__ method")
		}
		return indexArg(v), true
	}
	if len(args) > 0 {
		if v, ok := bound(args[0]); ok {
			start = v
		}
	}
	if len(args) > 1 {
		if v, ok := bound(args[1]); ok {
			end = v
		}
	}
	ln := int64(n)
	if end > ln {
		end = ln
	} else if end < 0 {
		end += ln
		if end < 0 {
			end = 0
		}
	}
	if start < 0 {
		start += ln
		if start < 0 {
			start = 0
		}
	}
	return int(start), int(end)
}

func callMethod(name, s string, args []any) any {
	switch name {
	case "lower", "upper", "title":
		noArgs(name, args)
		switch name {
		case "lower":
			return pyLower(s)
		case "upper":
			return pyUpper(s)
		}
		return pyTitle(s)
	case "strip", "lstrip", "rstrip":
		return stripChars(name, s, args)
	case "split":
		clinicArity("split", args, 0, 2)
		maxsplit := int64(-1)
		if len(args) == 2 {
			maxsplit = indexArg(args[1])
		}
		if maxsplit > math.MaxInt32 {
			maxsplit = math.MaxInt32
		}
		if len(args) == 0 || args[0] == nil {
			return pySplitSpace(s, int(maxsplit))
		}
		sep, ok := args[0].(string)
		if !ok {
			raise("TypeError", "must be str or None, not %s", typeName(args[0]))
		}
		if sep == "" {
			raise("ValueError", "empty separator")
		}
		return pySplitSep(s, sep, int(maxsplit))
	case "replace":
		clinicArity("replace", args, 2, 3)
		old, ok := args[0].(string)
		if !ok {
			raise("TypeError", "replace() argument 1 must be str, not %s", argTypeName(args[0]))
		}
		nw, ok := args[1].(string)
		if !ok {
			raise("TypeError", "replace() argument 2 must be str, not %s", argTypeName(args[1]))
		}
		count := int64(-1)
		if len(args) == 3 {
			count = indexArg(args[2])
		}
		if count > math.MaxInt32 {
			count = math.MaxInt32
		}
		out := strings.Replace(s, old, nw, int(count))
		if len(out) > maxResultLen {
			raise("ValueError", "result too large")
		}
		return out
	case "startswith", "endswith":
		arityRange(name, args, 1, 3)
		rs := []rune(s)
		start, end := adjustRange(len(rs), args[1:])
		prefix, ok := args[0].(string)
		if !ok {
			raise("TypeError", "%s first arg must be str or a tuple of str, not %s", name, typeName(args[0]))
		}
		if start > end {
			return false
		}
		sub := string(rs[start:end])
		if name == "startswith" {
			return strings.HasPrefix(sub, prefix)
		}
		return strings.HasSuffix(sub, prefix)
	case "zfill":
		if len(args) != 1 {
			raise("TypeError", "str.zfill() takes exactly one argument (%d given)", len(args))
		}
		if isIntLike(args[0]) && !asBig(args[0]).IsInt64() {
			raise("OverflowError", "Python int too large to convert to C ssize_t")
		}
		width := indexArg(args[0])
		n := int64(runeLen(s))
		if width <= n {
			return s
		}
		if width > maxResultLen {
			raise("ValueError", "result too large")
		}
		fill := strings.Repeat("0", int(width-n))
		if s != "" && (s[0] == '+' || s[0] == '-') {
			return s[:1] + fill + s[1:]
		}
		return fill + s
	case "find":
		arityRange("find", args, 1, 3)
		sub, ok := args[0].(string)
		if !ok {
			raise("TypeError", "find() argument 1 must be str, not %s", argTypeName(args[0]))
		}
		rs := []rune(s)
		start, end := adjustRange(len(rs), args[1:])
		if end-start < runeLen(sub) {
			return big.NewInt(-1)
		}
		window := string(rs[start:end])
		idx := strings.Index(window, sub)
		if idx < 0 {
			return big.NewInt(-1)
		}
		return big.NewInt(int64(start + runeLen(window[:idx])))
	case "join":
		if len(args) != 1 {
			raise("TypeError", "str.join() takes exactly one argument (%d given)", len(args))
		}
		var items []string
		switch x := args[0].(type) {
		case string:
			for _, r := range x {
				items = append(items, string(r))
			}
		case []any:
			for i, e := range x {
				es, ok := e.(string)
				if !ok {
					raise("TypeError", "sequence item %d: expected str instance, %s found", i, typeName(e))
				}
				items = append(items, es)
			}
		case []byte:
			if len(x) > 0 {
				raise("TypeError", "sequence item 0: expected str instance, int found")
			}
		default:
			raise("TypeError", "can only join an iterable")
		}
		total := len(s) * len(items)
		for _, it := range items {
			total += len(it)
		}
		if total > maxResultLen {
			raise("ValueError", "result too large")
		}
		return strings.Join(items, s)
	}
	raise("FormulaError", "method not allowed: .%s", name)
	return nil
}

// argTypeName is the type name in argument-clinic errors, where None reads "None".
func argTypeName(v any) string {
	if v == nil {
		return "None"
	}
	return typeName(v)
}
