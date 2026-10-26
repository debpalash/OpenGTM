package colgraph

// BrokenRenames is _broken_rename_references: the positions in after of
// retained columns that reference a display alias a rename removed (a column
// still reachable by id, or whose alias survived, is not broken). The rule
// guards PATCH column edits; the wbcrud package delegates to it.
func BrokenRenames(before, after []Col) []int {
	oldByID := map[string]Col{}
	for _, c := range before {
		if id, ok := idString(c); ok {
			oldByID[id] = c
		}
	}
	lost := map[string]bool{}
	for _, c := range after {
		id, ok := idString(c)
		if !ok {
			continue
		}
		old, found := oldByID[id]
		if !found || !pyTruthy(old.Get("name")) {
			continue
		}
		name := normAlias(old.Get("name"))
		aliases := map[string]bool{}
		for _, key := range []string{"id", "name"} {
			if pyTruthy(c.Get(key)) {
				aliases[normAlias(c.Get(key))] = true
			}
		}
		if !aliases[name] {
			lost[name] = true
		}
	}
	retained := map[string]bool{}
	for _, c := range after {
		if pyTruthy(c.Get("id")) {
			retained[pyStrValue(c.Get("id"))] = true
		}
	}
	out := []int{}
	for i, c := range after {
		for ref := range refsIn(c) {
			if retained[ref] {
				continue
			}
			if lost[normAlias(ref)] {
				out = append(out, i)
				break
			}
		}
	}
	return out
}
