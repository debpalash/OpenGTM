package runplan

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

type seedColumns struct {
	Columns   json.RawMessage `json:"columns"`
	V1Columns json.RawMessage `json:"v1_columns"`
	BigCols   json.RawMessage `json:"big_columns"`
}

// loadSeed reads the Python seed's column configs (testdata/seed_columns.json,
// exported from scripts/contract_seed.py).
func loadSeed(t testing.TB) seedColumns {
	t.Helper()
	raw, err := os.ReadFile("testdata/seed_columns.json")
	if err != nil {
		t.Fatal(err)
	}
	var s seedColumns
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func workbookOf(t testing.TB, id string, cols json.RawMessage) Workbook {
	t.Helper()
	parsed, err := wbcrud.ParseColumns(cols)
	if err != nil {
		t.Fatal(err)
	}
	return Workbook{ID: id, SourceType: "manual", SourceConfig: wbcrud.NewObj(), Columns: parsed}
}

func guard(t testing.TB, err error) *wbcrud.GuardError {
	t.Helper()
	var g *wbcrud.GuardError
	if !errors.As(err, &g) {
		t.Fatalf("want *wbcrud.GuardError, got %T %v", err, err)
	}
	return g
}

func ids(cols []colgraph.Col) []string {
	var out []string
	for _, c := range cols {
		s, _ := c.Get("id").(string)
		out = append(out, s)
	}
	return out
}

func TestOutputColumnsRunLast(t *testing.T) {
	cols, err := colgraph.ParseCols([]byte(`[
		{"id":"o1","type":"output"},{"id":"e1","type":"enrichment"},{"id":"lf","type":"lead_field"},
		{"id":"f1","type":"formula"},{"id":"o2","type":"output"},{"id":"h1","type":"http"},
		{"id":"r1","type":"research"},{"id":"a1","type":"agent"},{"id":"w1","type":"waterfall"},{"id":"ai","type":"ai_formula"}]`))
	if err != nil {
		t.Fatal(err)
	}
	got := ids(SelectColumns(cols, nil))
	want := []string{"e1", "f1", "h1", "r1", "a1", "w1", "ai", "o1", "o2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	got = ids(SelectColumns(cols, []string{"o2", "f1", "lf", "nope"}))
	if !reflect.DeepEqual(got, []string{"f1", "o2"}) {
		t.Fatalf("subset: %v", got)
	}
	if got := SelectColumns(cols, []string{}); len(got) != 0 {
		t.Fatalf("empty column_ids selects nothing, got %v", got)
	}
}

// Cell-scope and cycle failures happen before any database access, so a nil tx proves it.
func TestBuildRejectsBeforeTouchingDB(t *testing.T) {
	wb := workbookOf(t, "wb-enrich-1", loadSeed(t).Columns)
	cfg := Config{WorkspaceID: "ws"}
	cases := []struct {
		name   string
		req    RunRequest
		status int
		detail string
	}{
		{"mismatched rows", RunRequest{ColumnIDs: []string{"calc_len"}, RowColumns: map[string][]string{"101": {"calc_len"}}, RowIDs: []int64{102}},
			422, "Cell scope requires matching explicit row IDs"},
		{"row_columns without row_ids", RunRequest{ColumnIDs: []string{"calc_len"}, RowColumns: map[string][]string{"101": {"calc_len"}}},
			422, "Cell scope requires matching explicit row IDs"},
		{"unavailable column", RunRequest{ColumnIDs: []string{"calc_len"}, RowColumns: map[string][]string{"101": {"calc_dom"}}, RowIDs: []int64{101}},
			422, "Cell scope references an unavailable run column"},
		{"invalid identity", RunRequest{RowColumns: map[string][]string{"0": {"calc_len"}}, RowIDs: []int64{0}},
			422, "Invalid row identity in cell scope"},
		{"invalid before matching", RunRequest{RowColumns: map[string][]string{"01": {"x"}}, RowIDs: []int64{1}},
			422, "Invalid row identity in cell scope"},
		{"cycle all", RunRequest{},
			422, "Selected columns depend on a circular reference: cyc_a, cyc_b. Edit their column references to break the cycle before running."},
		{"cycle selected", RunRequest{ColumnIDs: []string{"cyc_a"}},
			422, "Selected columns depend on a circular reference: cyc_a. Edit their column references to break the cycle before running."},
	}
	for _, c := range cases {
		_, err := Build(context.Background(), nil, wb, c.req, cfg)
		g := guard(t, err)
		if g.Status != c.status || g.Detail != c.detail {
			t.Errorf("%s: got %d %q want %d %q", c.name, g.Status, g.Detail, c.status, c.detail)
		}
	}
}

func TestCycle422Message(t *testing.T) {
	// A cell-scoped run only checks the columns named in the scope.
	wb := workbookOf(t, "wb", json.RawMessage(`[
		{"id":"a","type":"formula","formula":"{b}+1"},{"id":"b","type":"formula","formula":"{a}+1"},
		{"id":"ok","type":"formula","formula":"1"}]`))
	_, err := Build(context.Background(), nil, wb, RunRequest{
		RowColumns: map[string][]string{"5": {"b", "a"}}, RowIDs: []int64{5}}, Config{})
	g := guard(t, err)
	want := "Selected columns depend on a circular reference: a, b. Edit their column references to break the cycle before running."
	if g.Status != 422 || g.Detail != want {
		t.Fatalf("%d %q", g.Status, g.Detail)
	}
}

func TestSkippedWhenNoColumns(t *testing.T) {
	wb := workbookOf(t, "wb-enrich-1", loadSeed(t).Columns)
	for _, ids := range [][]string{{"nope"}, {}, {"company"}} {
		p, err := Build(context.Background(), nil, wb, RunRequest{ColumnIDs: ids}, Config{})
		if err != nil {
			t.Fatal(err)
		}
		if !p.Skipped || p.Message != "No enrichment columns to run" || p.RunID != "" || len(p.Payload) != 0 || p.Executor != "" {
			t.Fatalf("%v: %+v", ids, p)
		}
	}
}
