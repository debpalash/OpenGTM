package wbcrud

import "sort"

// Ports of column_deps.topo_sort_columns and downstream_columns.

// TopoSortColumns orders columns so referencing columns follow what they
// reference (stable Kahn sort); a cycle falls back to config order.
func TopoSortColumns(cols []Column) []Column {
	n := len(cols)
	if n <= 1 {
		return append([]Column(nil), cols...)
	}
	deps := make([]map[int]struct{}, n)
	succ := make([]map[int]struct{}, n)
	for i := range deps {
		deps[i], succ[i] = map[int]struct{}{}, map[int]struct{}{}
	}
	for i, c := range cols {
		for ref := range refsIn(c) {
			for j := range referenceIndexes(ref, cols) {
				if j != i {
					deps[i][j] = struct{}{}
				}
			}
		}
	}
	indeg := make([]int, n)
	for i := 0; i < n; i++ {
		indeg[i] = len(deps[i])
		for j := range deps[i] {
			succ[j][i] = struct{}{}
		}
	}
	var ready, out []int
	for i := 0; i < n; i++ {
		if indeg[i] == 0 {
			ready = append(ready, i)
		}
	}
	for len(ready) > 0 {
		i := ready[0]
		ready = ready[1:]
		out = append(out, i)
		var next []int
		for k := range succ[i] {
			next = append(next, k)
		}
		sort.Ints(next)
		for _, k := range next {
			indeg[k]--
			if indeg[k] == 0 {
				ready = append(ready, k)
			}
		}
		sort.Ints(ready)
	}
	if len(out) != n {
		return append([]Column(nil), cols...)
	}
	res := make([]Column, n)
	for i, idx := range out {
		res[i] = cols[idx]
	}
	return res
}

// DownstreamColumns returns the transitive dependants of the edited fields in
// execution order, optionally filtered by eligible.
func DownstreamColumns(cols []Column, changed []string, eligible func(Column) bool) []Column {
	tainted := map[string]struct{}{}
	taintedIdx := map[int]struct{}{}
	for _, f := range changed {
		if pyTrim(f) != "" {
			tainted[normAlias(f)] = struct{}{}
		}
		for i := range referenceIndexes(pyTrim(f), cols) {
			taintedIdx[i] = struct{}{}
		}
	}
	for i, c := range cols {
		if lf := c.Get("lead_field"); truthy(lf) {
			if _, ok := tainted[normAlias(lf)]; ok {
				taintedIdx[i] = struct{}{}
			}
		}
	}
	var selected []Column
	for _, col := range TopoSortColumns(cols) {
		affected := false
		for ref := range refsIn(col) {
			matches := referenceIndexes(ref, cols)
			if len(matches) > 0 {
				for i := range matches {
					if _, ok := taintedIdx[i]; ok {
						affected = true
					}
				}
			} else if _, ok := tainted[normAlias(ref)]; ok {
				affected = true
			}
			if affected {
				break
			}
		}
		if !affected || (eligible != nil && !eligible(col)) {
			continue
		}
		selected = append(selected, col)
		id := col.Get("id")
		for i, cand := range cols {
			if sameValue(cand.Get("id"), id) {
				taintedIdx[i] = struct{}{}
			}
		}
		for _, k := range []string{"id", "name", "lead_field", "target_field"} {
			if v := col.Get(k); truthy(v) {
				tainted[normAlias(v)] = struct{}{}
			}
		}
	}
	return selected
}

var runnableTypes = map[string]bool{"enrichment": true, "waterfall": true, "ai_formula": true, "output": true,
	"research": true, "agent": true, "http": true, "formula": true}

// ReactiveEligible is the PATCH-rows predicate: a runnable column that is
// reactive (default: every runnable type except output).
func ReactiveEligible(c Column) bool {
	typ, _ := c.Get("type").(string)
	if !runnableTypes[typ] {
		return false
	}
	if v, ok := c.Lookup("reactive"); ok {
		return truthy(v)
	}
	return typ != "output"
}
