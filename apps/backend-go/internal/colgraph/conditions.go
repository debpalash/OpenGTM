package colgraph

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
)

var (
	placeholderRe = regexp.MustCompile(`\{([^}]+)\}`)
	parenRe       = regexp.MustCompile(`\((.+)\)`)
	compareOps    = []string{"!=", ">=", "<=", "==", ">", "<"}
)

// EvalCondition is conditions.evaluate_condition for a row given as plain
// cell values or {"value": v} cell objects, without a column config (display
// aliases then equal the ids). A nil or empty condition runs the column.
func EvalCondition(cond any, row map[string]any) bool {
	return EvalConditionCols(cond, row, nil)
}

// EvalConditionCols is evaluate_condition with the workbook column config, so
// a {Display Name} resolves to the column's value. Row keys are visited in
// column config order, then the remaining keys sorted (Python uses dict
// insertion order of lead_data).
//
// Deviation: a non-string, truthy condition raises in Python; here it fails
// closed (false), like an unparseable one.
func EvalConditionCols(cond any, row map[string]any, cols []Col) bool {
	var s string
	switch t := cond.(type) {
	case nil:
		return true
	case string:
		s = t
	default:
		return !pyTruthy(cond)
	}
	if s == "" {
		return true
	}
	s = pyStrip(s)
	switch pyLower(s) {
	case "true", "always", "1", "yes":
		return true
	case "false", "never", "0", "no":
		return false
	}
	vals := newCondValues(row, cols)
	for _, group := range splitUnquoted(s, literalMatcher(" OR "), 0) {
		ok := true
		for _, part := range splitUnquoted(group, literalMatcher(" AND "), 0) {
			if !evalSingle(pyStrip(part), vals) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// condValues is the flattened {col_id: value, col_name: value} dict, ids
// winning over display names, in Python's iteration order.
type condValues struct {
	keys  []string
	vals  map[string]string
	lower map[string]string // lowercased key -> value of the first such key
}

func newCondValues(row map[string]any, cols []Col) *condValues {
	nameOf := map[string]string{}
	for _, c := range cols {
		id, ok := c.Get("id").(string)
		if !ok {
			continue
		}
		if n, ok := c.Lookup("name"); ok {
			if ns, ok := n.(string); ok {
				nameOf[id] = ns
				continue
			}
		}
		nameOf[id] = id
	}
	// Python iterates lead_data order; the closest deterministic proxy is the
	// column config order, then any remaining keys sorted.
	ids := make([]string, 0, len(row))
	seenID := map[string]bool{}
	for _, c := range cols {
		if id, ok := c.Get("id").(string); ok && !seenID[id] {
			if _, in := row[id]; in {
				seenID[id] = true
				ids = append(ids, id)
			}
		}
	}
	var rest []string
	for k := range row {
		if !seenID[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	ids = append(ids, rest...)
	values := map[string]string{}
	aliases := map[string]string{}
	var aliasKeys []string
	for _, id := range ids {
		v := cellValue(row[id])
		values[id] = v
		name, ok := nameOf[id]
		if !ok {
			name = id
		}
		if _, seen := aliases[name]; !seen {
			aliasKeys = append(aliasKeys, name)
		}
		aliases[name] = v
	}
	cv := &condValues{vals: map[string]string{}, lower: map[string]string{}}
	for _, k := range aliasKeys {
		cv.keys = append(cv.keys, k)
		cv.vals[k] = aliases[k]
	}
	for _, id := range ids {
		if _, ok := cv.vals[id]; !ok {
			cv.keys = append(cv.keys, id)
		}
		cv.vals[id] = values[id]
	}
	for _, k := range cv.keys {
		lk := pyLower(k)
		if _, ok := cv.lower[lk]; !ok {
			cv.lower[lk] = cv.vals[k]
		}
	}
	return cv
}

func cellValue(cell any) string {
	var v any = cell
	if m, ok := toMap(cell); ok {
		v = m["value"] // missing -> "" in Python; nil maps to "" below
		if _, has := m["value"]; !has {
			return ""
		}
	}
	if v == nil {
		return ""
	}
	return pyStrValue(v)
}

func (cv *condValues) resolve(expr string) string {
	return placeholderRe.ReplaceAllStringFunc(expr, func(m string) string {
		key := pyStrip(m[1 : len(m)-1])
		if v, ok := cv.vals[key]; ok {
			return `"` + v + `"`
		}
		if v, ok := cv.lower[pyLower(key)]; ok {
			return `"` + v + `"`
		}
		return `""`
	})
}

func evalSingle(expr string, vals *condValues) bool {
	expr = pyStrip(expr)
	low := pyLower(expr)
	if strings.HasPrefix(low, "not_empty(") {
		return !isEmptyToken(extractParen(vals.resolve(expr)))
	}
	if strings.HasPrefix(low, "is_empty(") {
		return isEmptyToken(extractParen(vals.resolve(expr)))
	}
	for _, op := range compareOps {
		if parts := splitUnquoted(expr, literalMatcher(op), 1); len(parts) == 2 {
			return compareValues(cleanValue(vals.resolve(parts[0])), cleanValue(vals.resolve(parts[1])), op)
		}
	}
	if parts := splitUnquoted(expr, containsMatcher, 0); len(parts) == 2 {
		left := cleanValue(vals.resolve(parts[0]))
		right := cleanValue(vals.resolve(parts[1]))
		return strings.Contains(pyLower(left), pyLower(right))
	}
	// Fail closed: an unparseable condition must not run (and bill) the column.
	return false
}

func isEmptyToken(v string) bool {
	switch v {
	case "", `""`, "None", "null", "N/A":
		return true
	}
	return false
}

func extractParen(expr string) string {
	if m := parenRe.FindStringSubmatch(expr); m != nil {
		return cleanValue(m[1])
	}
	return ""
}

func cleanValue(v string) string {
	v = pyStrip(v)
	if n := len(v); n >= 2 && ((v[0] == '"' && v[n-1] == '"') || (v[0] == '\'' && v[n-1] == '\'')) {
		return v[1 : n-1]
	}
	return v
}

// --- operator splitting ---------------------------------------------------

// matcher returns the end index of a separator match at rs[i:], or -1.
type matcher func(rs []rune, i int) int

func literalMatcher(pat string) matcher {
	p := []rune(pat)
	return func(rs []rune, i int) int {
		if i+len(p) > len(rs) {
			return -1
		}
		for k, r := range p {
			if rs[i+k] != r {
				return -1
			}
		}
		return i + len(p)
	}
}

func foldEq(a, b rune) bool {
	if a == b {
		return true
	}
	for f := unicode.SimpleFold(a); f != a; f = unicode.SimpleFold(f) {
		if f == b {
			return true
		}
	}
	return false
}

// containsMatcher is re.compile(r"\s+contains\s+", re.IGNORECASE).match.
func containsMatcher(rs []rune, i int) int {
	j := i
	for j < len(rs) && pyIsSpace(rs[j]) {
		j++
	}
	if j == i {
		return -1
	}
	for _, r := range "contains" {
		if j >= len(rs) || !foldEq(rs[j], r) {
			return -1
		}
		j++
	}
	k := j
	for k < len(rs) && pyIsSpace(rs[k]) {
		k++
	}
	if k == j {
		return -1
	}
	return k
}

// splitUnquoted is _split_unquoted: split on a separator outside quoted
// literals and {column} references.
func splitUnquoted(expr string, m matcher, maxsplit int) []string {
	rs := []rune(expr)
	n := len(rs)
	// nextClose[i] = index of the first '}' at or after i, or -1.
	nextClose := make([]int, n+2)
	nextClose[n], nextClose[n+1] = -1, -1
	for i := n - 1; i >= 0; i-- {
		if rs[i] == '}' {
			nextClose[i] = i
		} else {
			nextClose[i] = nextClose[i+1]
		}
	}
	var parts []string
	start, i := 0, 0
	var quote rune
	for i < n {
		ch := rs[i]
		if quote != 0 {
			if ch == '\\' {
				i += 2
				continue
			}
			if ch == quote {
				quote = 0
			}
		} else if (ch == '"' || ch == '\'') && (i == 0 || pyIsSpace(rs[i-1]) || strings.ContainsRune("(=<>!", rs[i-1])) {
			quote = ch
		} else if ch == '{' && nextClose[i+1] != -1 {
			i = nextClose[i+1] + 1
			continue
		} else if end := m(rs, i); end != -1 {
			parts = append(parts, string(rs[start:i]))
			start, i = end, end
			if maxsplit != 0 && len(parts) == maxsplit {
				break
			}
			continue
		}
		i++
	}
	if start > n {
		start = n
	}
	return append(parts, string(rs[start:]))
}
