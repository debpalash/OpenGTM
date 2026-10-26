package safecond

import (
	"math"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
)

// Result is the outcome of evaluating a condition. Reason is empty unless the
// evaluation failed closed (Passed is then false).
type Result struct {
	Passed bool
	Reason string
}

// Column is the part of a workbook column the evaluator needs. A column with
// an empty ID is ignored; an empty Name means the name equals the id.
type Column struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// localZone reads naive ISO timestamps (Python datetime.timestamp() uses the
// process local zone); tests pin it to UTC.
var localZone = time.Local

// Evaluate is safe_evaluate_condition at the current time.
func Evaluate(condition string, cells map[string]any, cols []Column) Result {
	return EvaluateAt(condition, cells, cols, time.Now())
}

// EvaluateAt is Evaluate with an explicit clock (older_than). It never panics:
// an unexpected failure is a fail-closed "evaluator error".
func EvaluateAt(condition string, cells map[string]any, cols []Column, now time.Time) Result {
	return EvaluateOrdered(condition, jsonbOrder(cells), cells, cols, now)
}

// jsonbOrder orders keys as PostgreSQL jsonb does (shorter first, then
// bytewise), which is the iteration order Python sees for a row read from a
// jsonb column.
func jsonbOrder(cells map[string]any) []string {
	keys := make([]string, 0, len(cells))
	for k := range cells {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if len(keys[i]) != len(keys[j]) {
			return len(keys[i]) < len(keys[j])
		}
		return keys[i] < keys[j]
	})
	return keys
}

// EvaluateOrdered is EvaluateAt with the cell keys in Python dict insertion
// order (keys absent from cells are skipped). The order only matters when a
// column name equals another cell's key or two keys differ only by case.
func EvaluateOrdered(condition string, keys []string, cells map[string]any, cols []Column, now time.Time) (res Result) {
	defer func() {
		if r := recover(); r != nil {
			res = Result{false, "evaluator error: internal"}
		}
	}()
	if len(condition) > maxConditionBytes {
		return Result{false, condErr("condition too long (more than %d bytes)", maxConditionBytes).Msg}
	}
	cond := colgraph.PyStrip(condition)
	if cond == "" {
		return Result{true, ""}
	}
	switch colgraph.PyLower(cond) {
	case "true", "always", "1", "yes":
		return Result{true, ""}
	case "false", "never", "0", "no":
		return Result{false, ""}
	}
	toks, err := tokenize(cond)
	if err != nil {
		return Result{false, err.Error()}
	}
	if len(toks) == 0 {
		return Result{false, "empty condition"}
	}
	ev := &evaluator{values: buildValues(keys, cells, cols), now: float64(now.UnixNano()) / 1e9}
	ok, err := ev.evalTokens(toks)
	if err != nil {
		return Result{false, err.Error()}
	}
	return Result{ok, ""}
}

// values is the flat {id: str, name: str} map in insertion order.
type values struct {
	keys  []string
	m     map[string]string
	lower map[string]string // lowercased key -> first such key, built on demand
}

func (v *values) set(k, val string) {
	if _, ok := v.m[k]; !ok {
		v.keys = append(v.keys, k)
	}
	v.m[k] = val
}

// buildValues is _build_values over cells visited in the given key order.
func buildValues(keys []string, cells map[string]any, cols []Column) *values {
	nameMap := map[string]string{}
	for _, c := range cols {
		if c.ID == "" {
			continue
		}
		name := c.Name
		if name == "" {
			name = c.ID
		}
		nameMap[c.ID] = name
	}
	out := &values{m: make(map[string]string, 2*len(cells))}
	for _, k := range keys {
		val, present := cells[k]
		if !present {
			continue
		}
		if m, ok := val.(map[string]any); ok {
			if inner, has := m["value"]; has {
				val = inner
			}
		} else if m, ok := val.(colgraph.Col); ok {
			if inner, has := m.Lookup("value"); has {
				val = inner
			}
		}
		s := ""
		if val != nil {
			s = colgraph.PyStr(val)
		}
		out.set(k, s)
		if nm, ok := nameMap[k]; ok {
			out.set(nm, s)
		}
	}
	return out
}

type evaluator struct {
	values *values
	now    float64
	spent  int // resolved operand bytes so far
}

func (e *evaluator) resolve(t token) (string, error) {
	switch t.kind {
	case tPlaceholder:
		if v, ok := e.values.m[t.value]; ok {
			return e.charge(v)
		}
		if e.values.lower == nil {
			e.values.lower = make(map[string]string, len(e.values.keys))
			for _, k := range e.values.keys {
				lk := colgraph.PyLower(k)
				if _, seen := e.values.lower[lk]; !seen {
					e.values.lower[lk] = k
				}
			}
		}
		if k, ok := e.values.lower[colgraph.PyLower(t.value)]; ok {
			return e.charge(e.values.m[k])
		}
		return "", condErr("unresolved field '{%s}'", t.value)
	case tString, tWord:
		return t.value, nil
	}
	return "", condErr("expected operand, got %s", t.kind)
}

// charge enforces the per-operand cap and the per-evaluation total, so many
// tokens naming one large cell cannot multiply the work.
func (e *evaluator) charge(v string) (string, error) {
	if len(v) > maxOperandLen {
		return "", condErr("operand too long (more than %d bytes)", maxOperandLen)
	}
	e.spent += len(v)
	if e.spent > maxTotalOperands {
		return "", condErr("operands too large (more than %d bytes resolved)", maxTotalOperands)
	}
	return v, nil
}

func isEmpty(v string) bool {
	switch colgraph.PyStrip(v) {
	case "", "None", "null", "N/A", "NaN":
		return true
	}
	return false
}

func compare(left, right, op string) bool {
	if ln, ok := colgraph.PyParseFloat(left); ok {
		if rn, ok := colgraph.PyParseFloat(right); ok {
			switch op {
			case "==":
				return ln == rn
			case "!=":
				return ln != rn
			case ">":
				return ln > rn
			case "<":
				return ln < rn
			case ">=":
				return ln >= rn
			case "<=":
				return ln <= rn
			}
		}
	}
	c := strings.Compare(left, right)
	switch op {
	case "==":
		return c == 0
	case "!=":
		return c != 0
	case ">":
		return c > 0
	case "<":
		return c < 0
	case ">=":
		return c >= 0
	}
	return c <= 0
}

func isKeyword(t token, kw string) bool {
	return t.kind == tWord && strings.ToUpper(t.value) == kw
}

// splitKeyword splits on a top-level keyword token (never on placeholder or
// string content: those are different token kinds).
func splitKeyword(toks []token, kw string) [][]token {
	groups := [][]token{nil}
	for _, t := range toks {
		if isKeyword(t, kw) {
			groups = append(groups, nil)
		} else {
			groups[len(groups)-1] = append(groups[len(groups)-1], t)
		}
	}
	return groups
}

// evalTokens: OR binds looser than AND, evaluated lazily left to right exactly
// like Python's any()/all() so a later malformed clause is not reached when an
// earlier one decides the result.
func (e *evaluator) evalTokens(toks []token) (bool, error) {
	if g := splitKeyword(toks, "OR"); len(g) > 1 {
		for _, part := range g {
			ok, err := e.evalTokens(part)
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil
			}
		}
		return false, nil
	}
	if g := splitKeyword(toks, "AND"); len(g) > 1 {
		for _, part := range g {
			ok, err := e.evalTokens(part)
			if err != nil {
				return false, err
			}
			if !ok {
				return false, nil
			}
		}
		return true, nil
	}
	return e.evalComparison(toks)
}

func (e *evaluator) evalComparison(toks []token) (bool, error) {
	if len(toks) == 0 {
		return false, condErr("empty comparison")
	}
	first := toks[0]
	if first.kind == tWord {
		if fn := colgraph.PyLower(first.value); fn == "not_empty" || fn == "is_empty" {
			if len(toks) < 4 || toks[1].kind != tLParen || toks[len(toks)-1].kind != tRParen {
				return false, condErr("malformed %s() call", first.value)
			}
			inner := toks[2 : len(toks)-1]
			if len(inner) != 1 {
				return false, condErr("%s() takes exactly one operand", first.value)
			}
			v, err := e.resolve(inner[0])
			if err != nil {
				return false, err
			}
			if fn == "not_empty" {
				return !isEmpty(v), nil
			}
			return isEmpty(v), nil
		}
	}
	for idx, t := range toks {
		if t.kind != tWord {
			continue
		}
		kw := colgraph.PyLower(t.value)
		if kw != "contains" && kw != "older_than" {
			continue
		}
		left, right := toks[:idx], toks[idx+1:]
		if len(left) != 1 || len(right) != 1 {
			return false, condErr("malformed '%s' expression", t.value)
		}
		l, err := e.resolve(left[0])
		if err != nil {
			return false, err
		}
		if kw == "contains" {
			r, err := e.resolve(right[0])
			if err != nil {
				return false, err
			}
			return strings.Contains(colgraph.PyLower(l), colgraph.PyLower(r)), nil
		}
		return e.olderThan(l, right[0])
	}
	for idx, t := range toks {
		if t.kind != tOp {
			continue
		}
		left, right := toks[:idx], toks[idx+1:]
		if len(left) != 1 || len(right) != 1 {
			return false, condErr("malformed comparison")
		}
		l, err := e.resolve(left[0])
		if err != nil {
			return false, err
		}
		r, err := e.resolve(right[0])
		if err != nil {
			return false, err
		}
		return compare(l, r, t.value), nil
	}
	return false, condErr("comparison has no operator")
}

var durationSeconds = map[rune]float64{'s': 1, 'm': 60, 'h': 3600, 'd': 86400, 'w': 604800}

// parseDuration is re.match(r"^(\d+)\s*([smhdw])$", IGNORECASE) on the
// stripped token: Unicode digits and spaces, and the case-insensitive
// matches of the long s (U+017F) and Kelvin sign. ok is false on no match;
// unit is the matched rune.
func parseDuration(s string) (digits string, unit rune, ok bool) {
	rs := []rune(s)
	i := 0
	for i < len(rs) && unicode.Is(unicode.Nd, rs[i]) {
		i++
	}
	if i == 0 || i >= len(rs) {
		return "", 0, false
	}
	digits = string(rs[:i])
	j := i
	for j < len(rs) && colgraph.PyIsSpace(rs[j]) {
		j++
	}
	if j != len(rs)-1 {
		return "", 0, false
	}
	u := rs[j]
	switch u {
	case 's', 'S', 'ſ', 'm', 'M', 'h', 'H', 'd', 'D', 'w', 'W':
		return digits, u, true
	}
	return "", 0, false
}

func (e *evaluator) olderThan(left string, dur token) (bool, error) {
	if dur.kind != tWord && dur.kind != tString {
		return false, condErr("older_than duration must be like '30d'")
	}
	digits, unit, ok := parseDuration(colgraph.PyStrip(dur.value))
	if !ok {
		return false, condErr("bad duration %s (use e.g. 30d, 12h)", colgraph.PyStrRepr(dur.value))
	}
	if unit == 'ſ' {
		// Python: the regex matches, then the unit lookup raises KeyError('ſ').
		return false, condErr("evaluator error: 'ſ'")
	}
	mult := durationSeconds[unicode.ToLower(unit)]
	seconds := asciiNumber(digits) * mult
	ts, ok := parseTimestamp(left)
	if !ok {
		return false, condErr("older_than left operand is not a timestamp")
	}
	diff := e.now - ts
	if math.IsInf(diff, 1) {
		return true, nil
	}
	return diff > seconds, nil
}

// asciiNumber converts a run of Unicode decimal digits to a float64 (huge
// values saturate to +Inf, which no timestamp age exceeds).
func asciiNumber(digits string) float64 {
	var f float64
	for _, r := range digits {
		d := 0
		for x := r - 1; unicode.Is(unicode.Nd, x); x-- {
			d++
		}
		f = f*10 + float64(d%10)
	}
	return f
}
