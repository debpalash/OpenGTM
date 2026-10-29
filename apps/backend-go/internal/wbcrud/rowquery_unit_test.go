package wbcrud

import (
	"strings"
	"testing"
)

func qcols() []Column {
	return []Column{
		MakeColumn("id", "name", "name", "Name", "type", "lead_field"),
		MakeColumn("id", "mail", "name", "Mail", "type", "input", "lead_field", "email"),
		MakeColumn("id", "score", "name", "Score", "type", "enrichment"),
	}
}

func TestBuildRowQueryNeverInterpolatesUserText(t *testing.T) {
	evil := "x'; DROP TABLE workbook_rows; --"
	cols := append(qcols(), MakeColumn("id", evil, "name", "E", "type", "lead_field", "lead_field", evil))
	view := &QueryView{ID: "v", Config: ViewConfig{
		Filters: []ViewFilter{{Column: evil, Op: "contains", Value: evil}},
		Sort:    []ViewSort{{Column: evil, Dir: "desc"}},
	}}
	q, g := BuildRowQuery(QueryWorkbook{ID: "wb1", Columns: cols}, view, evil)
	if g != nil {
		t.Fatal(g)
	}
	if strings.Contains(q.SQL, "DROP") {
		t.Fatalf("user text interpolated: %s", q.SQL)
	}
}

func TestMissingSortColumn409(t *testing.T) {
	_, g := BuildRowQuery(QueryWorkbook{ID: "w", Columns: qcols()}, &QueryView{ID: "v", Config: ViewConfig{Sort: []ViewSort{{Column: "nope", Dir: "asc"}}}}, "")
	if g == nil || g.Status != 409 || g.Detail != "Saved view references a missing sort column. Repair the view before continuing." {
		t.Fatalf("sort: %v", g)
	}
	_, g = BuildRowQuery(QueryWorkbook{ID: "w", Columns: qcols()}, &QueryView{ID: "v", Config: ViewConfig{Filters: []ViewFilter{{Column: "nope", Op: "equals"}}}}, "")
	if g == nil || g.Status != 409 || g.Detail != "Saved view references a missing filter column. Repair the view before continuing." {
		t.Fatalf("filter: %v", g)
	}
}

func TestAfterRejectsBadCursorValues(t *testing.T) {
	view := &QueryView{ID: "v", Config: ViewConfig{Sort: []ViewSort{{Column: "name", Dir: "asc"}}}}
	q, _ := BuildRowQuery(QueryWorkbook{ID: "w", Columns: qcols()}, view, "")
	for _, vals := range [][]any{
		{int64(1), int64(1), int64(1)},  // sort term must be text
		{"a", "x", int64(1)},            // position must be integral
		{"a", int64(1), nil},            // id required
		{"a", int64(1)},                 // wrong count
		{"a\x00b", int64(1), int64(1)},  // NUL cannot reach postgres
		{"a", int64(1), int64(1) << 40}, // id out of int4 range
	} {
		if _, g := q.After(vals); g == nil || g.Status != 400 || g.Detail != "Invalid workbook row cursor" {
			t.Fatalf("After(%v) = %v", vals, g)
		}
	}
	if _, g := q.After([]any{"a", nil, int64(3)}); g != nil {
		t.Fatalf("null position is coalesced to 0, got %v", g)
	}
}
