package wbcrud

import (
	"fmt"
	"strings"
	"testing"
)

func TestViewConfigFilterAndSortCaps(t *testing.T) {
	withLimits(t, Limits{MaxViewFilters: 2, MaxViewSorts: 1})
	f := `{"column":"a","op":"equals","value":"x"}`
	if _, errs := ParseViewConfig([]byte(`{"filters":[`+f+`,`+f+`,`+f+`]}`), "body", "config"); len(errs) != 1 || errs[0].Type != "too_long" {
		t.Fatalf("filters: %+v", errs)
	}
	s := `{"column":"a"}`
	if _, errs := ParseViewConfig([]byte(`{"sort":[`+s+`,`+s+`]}`), "body", "config"); len(errs) != 1 || errs[0].Type != "too_long" {
		t.Fatalf("sorts: %+v", errs)
	}
	if _, errs := ParseViewConfig([]byte(`{"filters":[`+f+`,`+f+`],"sort":[`+s+`]}`), "body", "config"); errs != nil {
		t.Fatalf("at cap: %+v", errs)
	}
	// Stored views are read without the cap so an old oversized view still loads.
	if _, errs := ParseStoredViewConfig([]byte(`{"filters":[` + f + `,` + f + `,` + f + `]}`)); errs != nil {
		t.Fatalf("stored: %+v", errs)
	}
}

func TestColumnListCap(t *testing.T) {
	withLimits(t, Limits{MaxColumns: 2})
	col := func(i int) string {
		return fmt.Sprintf(`{"id":"c%d","name":"C%d","type":"text","width":120}`, i, i)
	}
	body := func(n int) []byte {
		var cs []string
		for i := 0; i < n; i++ {
			cs = append(cs, col(i))
		}
		return []byte(`{"name":"w","columns_config":[` + strings.Join(cs, ",") + `]}`)
	}
	if _, errs := ParseCreateBody(body(3)); len(errs) != 1 || errs[0].Type != "too_long" {
		t.Fatalf("create over cap: %+v", errs)
	}
	if _, errs := ParseCreateBody(body(2)); errs != nil {
		t.Fatalf("create at cap: %+v", errs)
	}
	if _, errs := ParseUpdateBody(body(3)); len(errs) != 1 || errs[0].Type != "too_long" {
		t.Fatalf("update over cap: %+v", errs)
	}
}
