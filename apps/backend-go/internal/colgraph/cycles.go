package colgraph

import (
	"container/heap"
	"errors"
	"sort"
)

// ErrCycle is returned by Order when the columns contain a dependency cycle.
// Order still returns the columns in config order, exactly as Python falls back.
var ErrCycle = errors.New("column dependency cycle detected")

const nilIDKey = "\x00<nil>"

func idKey(c Col) string {
	if s, ok := idString(c); ok {
		return s
	}
	return nilIDKey
}

type intHeap []int

func (h intHeap) Len() int           { return len(h) }
func (h intHeap) Less(i, j int) bool { return h[i] < h[j] }
func (h intHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *intHeap) Push(x any)        { *h = append(*h, x.(int)) }
func (h *intHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

// depGraph holds per-column dependency index sets (sorted, deduplicated).
func depGraph(cols []Col, ix *refIndex, skipSelf bool) [][]int {
	deps := make([][]int, len(cols))
	for i, c := range cols {
		set := map[int]struct{}{}
		for ref := range refsIn(c) {
			for _, j := range ix.lookup(ref) {
				if !skipSelf || j != i {
					set[j] = struct{}{}
				}
			}
		}
		deps[i] = sortedSet(set)
	}
	return deps
}

func sortedSet(set map[int]struct{}) []int {
	out := make([]int, 0, len(set))
	for j := range set {
		out = append(out, j)
	}
	sort.Ints(out)
	return out
}

// Order is topo_sort_columns: a column that references another runs after it,
// ties broken by config order. Lead-field style columns are nodes too. On a
// cycle it returns the columns in config order together with ErrCycle.
func Order(cols []Col) ([]Col, error) {
	n := len(cols)
	if n <= 1 {
		return append([]Col(nil), cols...), nil
	}
	deps := depGraph(cols, newRefIndex(cols), true)
	indeg := make([]int, n)
	succ := make([][]int, n)
	for i, d := range deps {
		indeg[i] = len(d)
		for _, j := range d {
			succ[j] = append(succ[j], i)
		}
	}
	h := &intHeap{}
	for i := 0; i < n; i++ {
		if indeg[i] == 0 {
			*h = append(*h, i)
		}
	}
	heap.Init(h)
	out := make([]Col, 0, n)
	for h.Len() > 0 {
		i := heap.Pop(h).(int)
		out = append(out, cols[i])
		for _, k := range succ[i] {
			if indeg[k]--; indeg[k] == 0 {
				heap.Push(h, k)
			}
		}
	}
	if len(out) != n {
		return append([]Col(nil), cols...), ErrCycle
	}
	return out, nil
}

// cycleBlockedKeys returns id keys of columns in cycles or downstream of
// cycles. Self references count; a column's own condition may read its own
// value and is not a cycle.
func cycleBlockedKeys(cols []Col, ix *refIndex) map[string]bool {
	n := len(cols)
	remaining := make([]bool, n)
	for i, c := range cols {
		if t, ok := c.Get("type").(string); ok && executableSet[t] {
			remaining[i] = true
		}
	}
	deps := make([][]int, n)
	for i, c := range cols {
		if !remaining[i] {
			continue
		}
		set := map[int]struct{}{}
		for ref := range refsIn(c.without("condition")) {
			for _, j := range ix.lookup(ref) {
				set[j] = struct{}{}
			}
		}
		for ref := range refsIn(c) {
			for _, j := range ix.lookup(ref) {
				if j != i {
					set[j] = struct{}{}
				}
			}
		}
		for j := range set {
			if remaining[j] {
				deps[i] = append(deps[i], j)
			}
		}
	}
	indeg := make([]int, n)
	succ := make([][]int, n)
	var queue []int
	for i := 0; i < n; i++ {
		if !remaining[i] {
			continue
		}
		indeg[i] = len(deps[i])
		for _, j := range deps[i] {
			succ[j] = append(succ[j], i)
		}
	}
	for i := 0; i < n; i++ {
		if remaining[i] && indeg[i] == 0 {
			queue = append(queue, i)
		}
	}
	for len(queue) > 0 {
		i := queue[0]
		queue = queue[1:]
		remaining[i] = false
		for _, k := range succ[i] {
			if indeg[k]--; indeg[k] == 0 {
				queue = append(queue, k)
			}
		}
	}
	out := map[string]bool{}
	for i, r := range remaining {
		if r {
			out[idKey(cols[i])] = true
		}
	}
	return out
}

// CycleBlocked is column_deps.cycle_blocked_columns: ids of columns in a cycle
// or downstream of one (columns without an id are omitted).
func CycleBlocked(cols []Col) map[string]bool {
	out := cycleBlockedKeys(cols, newRefIndex(cols))
	delete(out, nilIDKey)
	return out
}

// Independent is independent_columns: columns with no dependency edge to or
// from another column and not cycle blocked.
func Independent(cols []Col) []Col {
	n := len(cols)
	if n == 0 {
		return []Col{}
	}
	ix := newRefIndex(cols)
	out := make([]bool, n)
	in := make([]bool, n)
	for i, c := range cols {
		for ref := range refsIn(c) {
			for _, j := range ix.lookup(ref) {
				if j != i {
					out[i] = true
					in[j] = true
				}
			}
		}
	}
	blocked := cycleBlockedKeys(cols, ix)
	res := []Col{}
	for i, c := range cols {
		if !out[i] && !in[i] && !blocked[idKey(c)] {
			res = append(res, c)
		}
	}
	return res
}

// Downstream is downstream_columns: transitive dependants of the changed
// fields in execution order. A changed key may address an input by id, display
// name or lead_field.
func Downstream(cols []Col, changed []string, eligible func(Col) bool) []Col {
	ix := newRefIndex(cols)
	tainted := map[string]bool{}
	taintedIdx := map[int]bool{}
	for _, f := range changed {
		f = pyStrip(f)
		if f == "" {
			continue
		}
		tainted[normAlias(f)] = true
		for _, j := range ix.lookup(f) {
			taintedIdx[j] = true
		}
	}
	for i, c := range cols {
		if lf := c.Get("lead_field"); pyTruthy(lf) && tainted[normAlias(lf)] {
			taintedIdx[i] = true
		}
	}
	byID := map[string][]int{}
	for i, c := range cols {
		k := idKey(c)
		byID[k] = append(byID[k], i)
	}
	ordered, _ := Order(cols)
	selected := []Col{}
	for _, col := range ordered {
		affected := false
		for ref := range refsIn(col) {
			if m := ix.lookup(ref); len(m) > 0 {
				for _, j := range m {
					if taintedIdx[j] {
						affected = true
						break
					}
				}
			} else if tainted[normAlias(ref)] {
				affected = true
			}
			if affected {
				break
			}
		}
		if !affected {
			continue
		}
		if eligible != nil && !eligible(col) {
			continue
		}
		selected = append(selected, col)
		for _, i := range byID[idKey(col)] {
			taintedIdx[i] = true
		}
		for _, key := range []string{"id", "name", "lead_field", "target_field"} {
			if v := col.Get(key); pyTruthy(v) {
				tainted[normAlias(v)] = true
			}
		}
	}
	return selected
}
