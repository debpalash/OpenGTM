package wbcrud

import "github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"

// Ports of column_deps.topo_sort_columns and downstream_columns: delegated to
// internal/colgraph (one copy of the dependency rules).

// TopoSortColumns orders columns so referencing columns follow what they
// reference (stable Kahn sort); a cycle falls back to config order.
func TopoSortColumns(cols []Column) []Column {
	idx, _ := colgraph.OrderIndexes(graphCols(cols)) // on a cycle: config order
	res := make([]Column, len(idx))
	for i, j := range idx {
		res[i] = cols[j]
	}
	return res
}

// DownstreamColumns returns the transitive dependants of the edited fields in
// execution order, optionally filtered by eligible.
func DownstreamColumns(cols []Column, changed []string, eligible func(Column) bool) []Column {
	graph := graphCols(cols)
	byIdentity := make(map[uintptr]int, len(graph))
	for i, g := range graph {
		byIdentity[identity(g)] = i
	}
	var pred func(colgraph.Col) bool
	if eligible != nil {
		pred = func(g colgraph.Col) bool { return eligible(cols[byIdentity[identity(g)]]) }
	}
	return pick(cols, graph, colgraph.Downstream(graph, changed, pred))
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
