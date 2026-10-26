package wbcrud

import (
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
)

// Port of apps/api/services/workbook/column_deps.py (reference rules used by
// the column-edit guards) and formula_column.formula_references.

var (
	refRe         = regexp.MustCompile(`\{([^}]+)\}`)
	placeholderRe = regexp.MustCompile(`^\{([^}]+)\}`)
	templated     = []string{"prompt", "formula", "http_url", "condition", "goal"}
	executable    = map[string]bool{"enrichment": true, "waterfall": true, "ai_formula": true, "research": true, "agent": true, "http": true, "formula": true, "output": true}
)

func templateRefs(s string) []string {
	var out []string
	for _, m := range refRe.FindAllStringSubmatch(s, -1) {
		out = append(out, strings.TrimSpace(m[1]))
	}
	return out
}

// formulaReferences returns {placeholders} outside string literals/comments.
func formulaReferences(expr string) map[string]struct{} {
	refs := map[string]struct{}{}
	cur := 0
	for cur < len(expr) {
		ch := expr[cur]
		switch {
		case ch == '\'' || ch == '"':
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
		case ch == '#':
			if end := strings.IndexByte(expr[cur:], '\n'); end == -1 {
				cur = len(expr)
			} else {
				cur += end
			}
		default:
			if m := placeholderRe.FindStringSubmatch(expr[cur:]); m != nil {
				refs[strings.TrimSpace(m[1])] = struct{}{}
				cur += len(m[0])
			} else {
				cur++
			}
		}
	}
	return refs
}

// walkStrings yields every string inside nested objects/arrays.
func walkStrings(v any, fn func(string)) {
	switch t := v.(type) {
	case string:
		fn(t)
	case *Obj:
		if t != nil {
			for _, k := range t.keys {
				walkStrings(t.vals[k], fn)
			}
		}
	case map[string]any:
		for _, e := range t {
			walkStrings(e, fn)
		}
	case Column:
		walkStrings(t.o, fn)
	case []any:
		for _, e := range t {
			walkStrings(e, fn)
		}
	}
}

func asString(v any) (string, bool) { s, ok := v.(string); return s, ok }

// truthy mirrors Python truthiness for the config values used here.
func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case string:
		return t != ""
	case bool:
		return t
	case []any:
		return len(t) > 0
	case *Obj:
		return t.Len() > 0
	case map[string]any:
		return len(t) > 0
	}
	if n, ok := v.(interface{ String() string }); ok {
		s := n.String()
		return s != "0" && s != "0.0"
	}
	return true
}

func pyStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return "None"
	}
	if b, ok := v.(bool); ok {
		if b {
			return "True"
		}
		return "False"
	}
	return fmt.Sprint(v)
}

func normAlias(v any) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(pyStr(v))), "_", " ")
}

// refsIn is _refs_in: every referenced name in a column's templated config.
func refsIn(col Column) map[string]struct{} {
	refs := map[string]struct{}{}
	for _, f := range templated {
		s, ok := asString(col.Get(f))
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
	if arr, ok := col.Get("input_columns").([]any); ok {
		for _, r := range arr {
			if s, ok := asString(r); ok && strings.TrimSpace(s) != "" {
				refs[strings.TrimSpace(s)] = struct{}{}
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
		if truthy(col.Get("destination")) {
			destination = strings.ToLower(pyStr(col.Get("destination")))
		}
		var cfg *Obj
		if truthy(col.Get("destination_config")) {
			cfg, _ = col.Get("destination_config").(*Obj)
		}
		if cfg != nil {
			var sources []any
			switch destination {
			case "airtable", "crm", "instantly", "smartlead":
				if fm, ok := cfg.vals["field_map"].(*Obj); ok {
					for _, k := range fm.keys {
						sources = append(sources, k)
					}
				}
			case "sheets":
				if cols, ok := cfg.vals["columns"].([]any); ok {
					sources = cols
				}
			}
			for _, src := range sources {
				s, ok := asString(src)
				if !ok || strings.TrimSpace(s) == "" {
					continue
				}
				if (destination == "instantly" || destination == "smartlead") && strings.Contains(s, "{") {
					for _, r := range templateRefs(s) {
						refs[r] = struct{}{}
					}
				} else {
					refs[strings.TrimSpace(s)] = struct{}{}
				}
			}
		}
	}
	return refs
}

func sameValue(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return reflect.DeepEqual(a, b)
}

// referenceIndexes: stable id first, then exact / lower / lower+underscore alias.
func referenceIndexes(ref string, cols []Column) map[int]struct{} {
	exact := map[int]struct{}{}
	for i, c := range cols {
		if s, ok := c.Get("id").(string); ok && s == ref {
			exact[i] = struct{}{}
		}
	}
	if len(exact) > 0 {
		return exact
	}
	norms := []func(string) string{
		func(s string) string { return s },
		strings.ToLower,
		func(s string) string { return strings.ReplaceAll(strings.ToLower(s), "_", " ") },
	}
	for _, n := range norms {
		m := map[int]struct{}{}
		for i, c := range cols {
			for _, key := range []string{"id", "name"} {
				if s, ok := c.Get(key).(string); ok && n(s) == n(ref) {
					m[i] = struct{}{}
					break
				}
			}
		}
		if len(m) > 0 {
			return m
		}
	}
	return map[int]struct{}{}
}

func idString(c Column) (string, bool) {
	v, ok := c.Lookup("id")
	if !ok || v == nil {
		return "", false
	}
	return pyStr(v), true
}

// ReferencingColumns: columns directly referencing c by id or alias.
func ReferencingColumns(cols []Column, c Column) []Column {
	aliases := map[string]struct{}{}
	for _, key := range []string{"id", "name"} {
		if truthy(c.Get(key)) {
			aliases[normAlias(c.Get(key))] = struct{}{}
		}
	}
	stable := map[string]struct{}{}
	for _, cand := range cols {
		if truthy(cand.Get("id")) {
			stable[pyStr(cand.Get("id"))] = struct{}{}
		}
	}
	out := []Column{}
	for _, cand := range cols {
		if sameValue(cand.Get("id"), c.Get("id")) {
			continue
		}
		for ref := range refsIn(cand) {
			hit := false
			if _, isStable := stable[ref]; isStable {
				hit = sameValue(ref, c.Get("id"))
			} else {
				_, hit = aliases[normAlias(ref)]
			}
			if hit {
				out = append(out, cand)
				break
			}
		}
	}
	return out
}

// BrokenRenameReferences: retained columns referencing a display alias that a
// rename removed.
func BrokenRenameReferences(before, after []Column) []Column {
	oldByID := map[string]Column{}
	for _, c := range before {
		if id, ok := idString(c); ok {
			oldByID[id] = c
		}
	}
	lost := map[string]struct{}{}
	for _, c := range after {
		id, ok := idString(c)
		if !ok {
			continue
		}
		old, found := oldByID[id]
		if !found || !truthy(old.Get("name")) {
			continue
		}
		name := normAlias(old.Get("name"))
		aliases := map[string]struct{}{}
		for _, key := range []string{"id", "name"} {
			if truthy(c.Get(key)) {
				aliases[normAlias(c.Get(key))] = struct{}{}
			}
		}
		if _, ok := aliases[name]; !ok {
			lost[name] = struct{}{}
		}
	}
	retained := map[string]struct{}{}
	for _, c := range after {
		if truthy(c.Get("id")) {
			retained[pyStr(c.Get("id"))] = struct{}{}
		}
	}
	out := []Column{}
	for _, c := range after {
		for ref := range refsIn(c) {
			if _, kept := retained[ref]; kept {
				continue
			}
			if _, gone := lost[normAlias(ref)]; gone {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

// CycleBlockedColumns: ids in cycles or downstream of cycles (self references
// included; a column's own `condition` may read its own value).
func CycleBlockedColumns(cols []Column) map[string]struct{} {
	deps := make([]map[int]struct{}, len(cols))
	for cur, col := range cols {
		edges := map[int]struct{}{}
		noCond := col.without("condition")
		for ref := range refsIn(noCond) {
			for i := range referenceIndexes(ref, cols) {
				edges[i] = struct{}{}
			}
		}
		for ref := range refsIn(col) {
			for i := range referenceIndexes(ref, cols) {
				if i != cur {
					edges[i] = struct{}{}
				}
			}
		}
		deps[cur] = edges
	}
	remaining := map[int]struct{}{}
	for i, c := range cols {
		if s, ok := c.Get("type").(string); ok && executable[s] {
			remaining[i] = struct{}{}
		}
	}
	for {
		ready := []int{}
		for i := range remaining {
			blocked := false
			for d := range deps[i] {
				if _, in := remaining[d]; in {
					blocked = true
					break
				}
			}
			if !blocked {
				ready = append(ready, i)
			}
		}
		if len(ready) == 0 {
			out := map[string]struct{}{}
			for i := range remaining {
				if id, ok := idString(cols[i]); ok {
					out[id] = struct{}{}
				}
			}
			return out
		}
		for _, i := range ready {
			delete(remaining, i)
		}
	}
}

// GuardError is a column/view guard failure the route turns into an
// HTTPException(Status, Detail).
type GuardError struct {
	Status int
	Detail string
}

func (g *GuardError) Error() string { return fmt.Sprintf("%d %s", g.Status, g.Detail) }

// ValidateRenames is _validate_column_renames.
func ValidateRenames(before, after []Column) *GuardError {
	dep := BrokenRenameReferences(before, after)
	if len(dep) == 0 {
		return nil
	}
	names := make([]string, len(dep))
	for i, c := range dep {
		if truthy(c.Get("name")) {
			names[i] = pyStr(c.Get("name"))
		} else {
			names[i] = pyStr(c.Get("id"))
		}
	}
	return &GuardError{Status: 409, Detail: "Rename breaks column-name references in: " + strings.Join(names, ", ") + ". Change those references to stable column IDs first."}
}

// ValidateNewCycles is _validate_new_cycle_dependencies.
func ValidateNewCycles(before, after []Column) *GuardError {
	old := CycleBlockedColumns(before)
	var fresh []string
	for id := range CycleBlockedColumns(after) {
		if _, ok := old[id]; !ok {
			fresh = append(fresh, id)
		}
	}
	if len(fresh) == 0 {
		return nil
	}
	sort.Strings(fresh)
	return &GuardError{Status: 422, Detail: "Column changes introduce circular dependencies affecting: " + strings.Join(fresh, ", ") + ". Break the circular references before saving."}
}
