package wbcrud

import (
	"encoding/json"
	"testing"
)

func TestViewConfigVectors(t *testing.T) {
	v := loadVectors(t)
	known := map[string]struct{}{}
	for _, k := range v.ViewsKnown {
		known[k] = struct{}{}
	}
	cols := []Column{}
	for k := range known {
		cols = append(cols, MakeColumn("id", k))
	}
	for _, c := range v.Views {
		t.Run(c.Name, func(t *testing.T) {
			cfg, errs := ParseViewConfig(json.RawMessage(c.Input))
			if len(c.Errors) > 0 {
				if !sameJSON(t, errs, c.Errors) {
					t.Errorf("errors = %s, want %v", compact(t, errs), c.Errors)
				}
				return
			}
			if len(errs) > 0 {
				t.Fatalf("unexpected errors %s", compact(t, errs))
			}
			if got := compact(t, cfg); got != c.Config {
				t.Errorf("config = %s\nwant     %s", got, c.Config)
			}
			checkGuard(t, ValidateViewColumns(cols, cfg), c.Guard)
			checkGuard(t, ValidateViewColumns([]Column{MakeColumn("id", "zz")}, cfg), c.GuardUnknown)
		})
	}
}

func TestViewConfigDefaults(t *testing.T) {
	cfg, errs := ParseViewConfig(json.RawMessage(`{"filters":[{"column":"a"}],"sort":[{"column":"b"}]}`))
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	if cfg.Filters[0].Op != "contains" || cfg.Sort[0].Dir != "asc" || cfg.HiddenColumns == nil {
		t.Errorf("defaults wrong: %+v", cfg)
	}
}

func TestViewMissingColumnsSorted(t *testing.T) {
	cfg, _ := ParseViewConfig(json.RawMessage(`{"filters":[{"column":"z"},{"column":"a"}],"sort":[{"column":"m"}],"hidden_columns":["a","k"]}`))
	got := cfg.MissingColumns(map[string]struct{}{"k": {}})
	want := []string{"a", "m", "z"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("got %v want %v", got, want)
	}
}
