package colgraph

import (
	"regexp"
	"sort"
	"strings"
)

var (
	refRe         = regexp.MustCompile(`\{([^}]+)\}`)
	templated     = []string{"prompt", "formula", "http_url", "condition", "goal"}
	executableSet = map[string]bool{"enrichment": true, "waterfall": true, "ai_formula": true, "research": true, "agent": true, "http": true, "formula": true, "output": true}
)

func templateRefs(s string) []string {
	var out []string
	for _, m := range refRe.FindAllStringSubmatch(s, -1) {
		out = append(out, pyStrip(m[1]))
	}
	return out
}

// formulaReferences is formula_column.formula_references: {placeholders}
// outside string literals and comments.
func formulaReferences(expr string) map[string]struct{} {
	refs := map[string]struct{}{}
	cur := 0
	for cur < len(expr) {
		ch := expr[cur]
		switch ch {
		case '\'', '"':
			delim := string(ch)
			if strings.HasPrefix(expr[cur:], strings.Repeat(delim, 3)) {
				delim = strings.Repeat(delim, 3)
			}
			cur += len(delim)
			for cur < len(expr) {
				if expr[cur] == '\\' {
					cur += 2
				} else if strings.HasPrefix(expr[cur:], delim) {
					cur += len(delim)
					break
				} else {
					cur++
				}
			}
		case '#':
			if end := strings.IndexByte(expr[cur:], '\n'); end == -1 {
				cur = len(expr)
			} else {
				cur += end
			}
		case '{':
			rest := expr[cur+1:]
			if end := strings.IndexByte(rest, '}'); end > 0 {
				refs[pyStrip(rest[:end])] = struct{}{}
				cur += end + 2
			} else {
				cur++
			}
		default:
			cur++
		}
	}
	return refs
}

func walkStrings(v any, fn func(string)) {
	switch t := v.(type) {
	case string:
		fn(t)
	case map[string]any:
		for _, e := range t {
			walkStrings(e, fn)
		}
	case map[string]string:
		for _, e := range t {
			fn(e)
		}
	case Col:
		for _, e := range t.Vals {
			walkStrings(e, fn)
		}
	case []any:
		for _, e := range t {
			walkStrings(e, fn)
		}
	case []string:
		for _, e := range t {
			fn(e)
		}
	}
}

// refsIn is _refs_in: every referenced name in a column's templated config.
func refsIn(col Col) map[string]struct{} {
	refs := map[string]struct{}{}
	for _, f := range templated {
		s, ok := col.Get(f).(string)
		if !ok {
			continue
		}
		if f == "formula" {
			for r := range formulaReferences(s) {
				refs[r] = struct{}{}
			}
		} else {
			for _, r := range templateRefs(s) {
				refs[r] = struct{}{}
			}
		}
	}
	if ic := col.Get("input_columns"); pyTruthy(ic) {
		var items []string
		switch t := ic.(type) {
		case string:
			for _, r := range t {
				items = append(items, string(r))
			}
		case map[string]any:
			for k := range t {
				items = append(items, k)
			}
		default:
			if l, ok := toList(ic); ok {
				for _, e := range l {
					if s, ok := e.(string); ok {
						items = append(items, s)
					}
				}
			}
		}
		for _, s := range items {
			if s = pyStrip(s); s != "" {
				refs[s] = struct{}{}
			}
		}
	}
	for _, f := range []string{"http_headers", "http_body", "destination_config"} {
		if v := col.Get(f); v != nil {
			walkStrings(v, func(s string) {
				for _, r := range templateRefs(s) {
					refs[r] = struct{}{}
				}
			})
		}
	}
	if col.Get("type") == "output" {
		destination := ""
		if d := col.Get("destination"); pyTruthy(d) {
			destination = pyLower(pyStrValue(d))
		}
		cfg, _ := toMap(col.Get("destination_config"))
		if cfg != nil {
			var sources []string
			switch destination {
			case "airtable", "crm", "instantly", "smartlead":
				if fm, ok := toMap(cfg["field_map"]); ok {
					for k := range fm {
						sources = append(sources, k)
					}
				}
			case "sheets":
				if l, ok := toList(cfg["columns"]); ok {
					for _, e := range l {
						if s, ok := e.(string); ok {
							sources = append(sources, s)
						}
					}
				}
			}
			for _, s := range sources {
				if pyStrip(s) == "" {
					continue
				}
				if (destination == "instantly" || destination == "smartlead") && strings.Contains(s, "{") {
					for _, r := range templateRefs(s) {
						refs[r] = struct{}{}
					}
				} else {
					refs[pyStrip(s)] = struct{}{}
				}
			}
		}
	}
	return refs
}

// Refs returns the names a column references (placeholders in prompt, formula,
// http_url, condition, goal, headers, body, destination config, input_columns
// and output field maps), sorted. Placeholders resolve by column id or name.
func Refs(c Col) []string {
	set := refsIn(c)
	out := make([]string, 0, len(set))
	for r := range set {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// --- reference resolution -------------------------------------------------

type refIndex struct {
	exact map[string][]int
	norms [3]map[string][]int
}

func normFns() [3]func(string) string {
	return [3]func(string) string{
		func(s string) string { return s },
		pyLower,
		func(s string) string { return strings.ReplaceAll(pyLower(s), "_", " ") },
	}
}

func newRefIndex(cols []Col) *refIndex {
	ix := &refIndex{exact: map[string][]int{}}
	fns := normFns()
	for k := range ix.norms {
		ix.norms[k] = map[string][]int{}
	}
	for i, c := range cols {
		if s, ok := c.Get("id").(string); ok {
			ix.exact[s] = append(ix.exact[s], i)
		}
		for k, fn := range fns {
			seen := map[string]bool{}
			for _, key := range []string{"id", "name"} {
				if s, ok := c.Get(key).(string); ok {
					n := fn(s)
					if !seen[n] {
						seen[n] = true
						ix.norms[k][n] = append(ix.norms[k][n], i)
					}
				}
			}
		}
	}
	return ix
}

// lookup is reference_indexes: stable ids first, then exact / lower /
// lower+underscore aliases. Result is ascending.
func (ix *refIndex) lookup(ref string) []int {
	if m := ix.exact[ref]; len(m) > 0 {
		return m
	}
	for k, fn := range normFns() {
		if m := ix.norms[k][fn(ref)]; len(m) > 0 {
			return m
		}
	}
	return nil
}

// ReferenceIndexes returns the indexes in cols that ref resolves to.
func ReferenceIndexes(ref string, cols []Col) []int {
	return newRefIndex(cols).lookup(ref)
}

func normAlias(v any) string {
	return strings.ReplaceAll(pyLower(pyStrip(pyStrValue(v))), "_", " ")
}

// Referencing returns columns that directly reference c by id or alias,
// ambiguous aliases included.
func Referencing(cols []Col, c Col) []Col {
	aliases := map[string]bool{}
	for _, key := range []string{"id", "name"} {
		if pyTruthy(c.Get(key)) {
			aliases[normAlias(c.Get(key))] = true
		}
	}
	stable := map[string]bool{}
	for _, cand := range cols {
		if s, ok := cand.Get("id").(string); ok && s != "" {
			stable[s] = true
		}
	}
	cid, cidIsStr := c.Get("id").(string)
	out := []Col{}
	for _, cand := range cols {
		if sameValue(cand.Get("id"), c.Get("id")) {
			continue
		}
		for ref := range refsIn(cand) {
			var hit bool
			if stable[ref] {
				hit = cidIsStr && ref == cid
			} else {
				hit = aliases[normAlias(ref)]
			}
			if hit {
				out = append(out, cand)
				break
			}
		}
	}
	return out
}

// Upstream returns the ids of the columns the column with the given id
// directly depends on, in config order (self references excluded).
func Upstream(cols []Col, id string) []string {
	ix := newRefIndex(cols)
	dep := map[int]bool{}
	for i, c := range cols {
		if s, ok := idString(c); !ok || s != id {
			continue
		}
		for ref := range refsIn(c) {
			for _, j := range ix.lookup(ref) {
				if j != i {
					dep[j] = true
				}
			}
		}
	}
	idx := make([]int, 0, len(dep))
	for j := range dep {
		idx = append(idx, j)
	}
	sort.Ints(idx)
	out := []string{}
	seen := map[string]bool{}
	for _, j := range idx {
		if s, ok := idString(cols[j]); ok && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// UnavailableDeps is unavailable_computed_dependencies: ids of referenced
// executable columns that have no value (None) in row. Zero and false count as
// values.
func UnavailableDeps(column Col, cols []Col, row map[string]any) []string {
	ix := newRefIndex(cols)
	referenced := map[int]bool{}
	for ref := range refsIn(column) {
		for _, j := range ix.lookup(ref) {
			referenced[j] = true
		}
	}
	missing := []string{}
	for i, up := range cols {
		if !referenced[i] {
			continue
		}
		if t, ok := up.Get("type").(string); !ok || !executableSet[t] {
			continue
		}
		if sameValue(up.Get("id"), column.Get("id")) {
			continue
		}
		var value any
		if cid, ok := up.Get("id").(string); ok {
			if v, in := row[cid]; in {
				value = v
			} else if name, ok := up.Get("name").(string); ok {
				value = row[name]
			}
		} else if name, ok := up.Get("name").(string); ok {
			value = row[name]
		}
		if value == nil {
			s, _ := idString(up)
			if _, ok := idString(up); !ok {
				s = "None"
			}
			missing = append(missing, s)
		}
	}
	return missing
}

// FormulaReferences is the sorted set of {column} references a formula evaluates
// (formula_column.formula_references).
func FormulaReferences(expr string) []string {
	set := formulaReferences(expr)
	out := make([]string, 0, len(set))
	for r := range set {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}
