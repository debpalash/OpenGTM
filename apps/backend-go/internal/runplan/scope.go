package runplan

import (
	"errors"
	"sort"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
)

// enrichmentTypes are the column types a run executes (the tuple in
// run_workbook, equal to enrichment.ENRICHMENT_COL_TYPES).
var enrichmentTypes = map[string]bool{
	"enrichment": true, "waterfall": true, "ai_formula": true, "output": true,
	"research": true, "agent": true, "http": true, "formula": true,
}

func colType(c colgraph.Col) string {
	t, _ := c.Get("type").(string)
	return t
}

func colID(c colgraph.Col) (string, bool) {
	s, ok := c.Get("id").(string)
	return s, ok
}

// SelectColumns picks the columns a run executes: enrichment-type columns
// whose id is in ids (nil ids = every one), output columns last (they push the
// enriched row somewhere), otherwise in configured order.
func SelectColumns(cols []colgraph.Col, ids []string) []colgraph.Col {
	var want map[string]bool
	if ids != nil {
		want = make(map[string]bool, len(ids))
		for _, id := range ids {
			want[id] = true
		}
	}
	var normal, outputs []colgraph.Col
	for _, c := range cols {
		if !enrichmentTypes[colType(c)] {
			continue
		}
		if want != nil {
			if id, ok := colID(c); !ok || !want[id] {
				continue
			}
		}
		if colType(c) == "output" {
			outputs = append(outputs, c)
		} else {
			normal = append(normal, c)
		}
	}
	return append(normal, outputs...)
}

// colsByID is go_executor._selected_columns: every configured column (any
// type) whose id is in ids.
func colsByID(cols []colgraph.Col, ids []string) []colgraph.Col {
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	var out []colgraph.Col
	for _, c := range cols {
		if id, ok := colID(c); ok && want[id] {
			out = append(out, c)
		}
	}
	return out
}

// ValidateCellScope is cell_scope.restrict_work_items([], row_columns): every
// key must be a canonical positive decimal row id and every column id a
// non-empty string. Entries are checked in key order.
func ValidateCellScope(rowColumns map[string][]string) error {
	keys := make([]string, 0, len(rowColumns))
	for k := range rowColumns {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !canonicalRowID(k) {
			return errors.New("Invalid row identity in cell scope")
		}
		for _, c := range rowColumns[k] {
			if c == "" {
				return errors.New("Invalid column identities in cell scope")
			}
		}
	}
	return nil
}

// canonicalRowID: key.isdecimal() and int(key) > 0 and str(int(key)) == key.
// Non-ASCII decimal digits parse in Python but never round-trip to themselves.
func canonicalRowID(k string) bool {
	if k == "" || k[0] == '0' {
		return false
	}
	for i := 0; i < len(k); i++ {
		if k[i] < '0' || k[i] > '9' {
			return false
		}
	}
	return true
}
