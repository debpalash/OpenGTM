package workbook

import (
	"strings"
	"testing"
)

func mustColumns(t *testing.T, raw string) []column {
	t.Helper()
	cols, err := parseColumns([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return cols
}

const refColumns = `[
 {"id":"a","type":"waterfall","name":"A","target_field":"founded_year","waterfall":["free1"],"prompt":"{b}"},
 {"id":"b","type":"waterfall","name":"B","target_field":"founded_year","waterfall":["free1"]}]`

// {col} references and input_columns no longer make the go executor refuse.
func TestRefColumnsNoLongerHandedBack(t *testing.T) {
	cols := mustColumns(t, refColumns)
	if err := validateGoColumn(cols[0]); err != nil {
		t.Fatalf("column with {col} reference refused: %v", err)
	}
	ic := mustColumns(t, `[{"id":"a","type":"waterfall","target_field":"founded_year","waterfall":["x"],"input_columns":["b"]}]`)
	if err := validateGoColumn(ic[0]); err != nil {
		t.Fatalf("column with input_columns refused: %v", err)
	}
}

func ids(cols []column) string {
	var s []string
	for _, c := range cols {
		s = append(s, c.ID)
	}
	return strings.Join(s, ",")
}

func TestOrderColumnsFollowsDependencies(t *testing.T) {
	cols := mustColumns(t, refColumns)
	if got := ids(orderColumns(cols)); got != "b,a" {
		t.Fatalf("order %s", got)
	}
	// Independent columns keep config order; a cycle falls back to config order.
	cyc := mustColumns(t, `[
 {"id":"a","type":"waterfall","prompt":"{b}"},{"id":"b","type":"waterfall","prompt":"{a}"},{"id":"c","type":"waterfall"}]`)
	if got := ids(orderColumns(cyc)); got != "a,b,c" {
		t.Fatalf("cycle order %s", got)
	}
}
