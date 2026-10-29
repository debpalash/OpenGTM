package wbcrud

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
)

// Port of apps/api/services/workbook/column_deps.py (reference rules used by
// the column-edit guards) and formula_column.formula_references.

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

func sameValue(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return reflect.DeepEqual(a, b)
}

func idString(c Column) (string, bool) {
	v, ok := c.Lookup("id")
	if !ok || v == nil {
		return "", false
	}
	return pyStr(v), true
}

// The reference, rename and cycle rules live in internal/colgraph (one copy,
// pinned by the Python vectors); these wrappers adapt wbcrud columns to it.

// graphCols reads columns as colgraph columns (a column always serialises back
// to a valid object, so a failure is a programming error: the zero Col has no
// references and blocks nothing).
func graphCols(cols []Column) []colgraph.Col {
	out := make([]colgraph.Col, len(cols))
	for i, c := range cols {
		raw, err := c.MarshalJSON()
		if err == nil {
			out[i], err = colgraph.ParseCol(raw)
		}
		if err != nil {
			out[i] = colgraph.MakeCol()
		}
	}
	return out
}

// identity is the address of a Col's value map: colgraph returns copies of the
// Cols it was given, and the map header identifies which input each one is.
func identity(c colgraph.Col) uintptr { return reflect.ValueOf(c.Vals).Pointer() }

// pick maps colgraph results back to the input columns they came from.
func pick(cols []Column, graph, picked []colgraph.Col) []Column {
	at := make(map[uintptr]int, len(graph))
	for i, g := range graph {
		at[identity(g)] = i
	}
	out := []Column{}
	for _, p := range picked {
		out = append(out, cols[at[identity(p)]])
	}
	return out
}

// ReferencingColumns: columns directly referencing c by id or alias.
func ReferencingColumns(cols []Column, c Column) []Column {
	graph := graphCols(cols)
	return pick(cols, graph, colgraph.Referencing(graph, graphCols([]Column{c})[0]))
}

// BrokenRenameReferences: retained columns referencing a display alias that a
// rename removed.
func BrokenRenameReferences(before, after []Column) []Column {
	out := []Column{}
	for _, i := range colgraph.BrokenRenames(graphCols(before), graphCols(after)) {
		out = append(out, after[i])
	}
	return out
}

// CycleBlockedColumns: ids in cycles or downstream of cycles (self references
// included; a column's own `condition` may read its own value).
func CycleBlockedColumns(cols []Column) map[string]struct{} {
	out := map[string]struct{}{}
	for id := range colgraph.CycleBlocked(graphCols(cols)) {
		out[id] = struct{}{}
	}
	return out
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
