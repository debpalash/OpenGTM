package wbregistry_test

import (
	"encoding/json"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbregistry"
)

// External test package: wbcrud imports wbregistry, so the normaliser can only
// be consumed from outside it.
func TestEveryRecipeColumnPassesNormalize(t *testing.T) {
	reg, err := wbregistry.Load()
	if err != nil {
		t.Fatal(err)
	}
	all, _ := reg.Recipes("")
	if len(all) != 19 {
		t.Fatalf("recipes = %d, want 19", len(all))
	}
	for _, rc := range all {
		var wb struct {
			Columns []json.RawMessage `json:"columns"`
		}
		if err := json.Unmarshal(rc.Workbook, &wb); err != nil {
			t.Fatalf("%s: %v", rc.Slug, err)
		}
		if len(wb.Columns) == 0 {
			t.Errorf("%s: no columns", rc.Slug)
		}
		var cols []wbcrud.Column
		seen := map[string]bool{}
		for i, raw := range wb.Columns {
			c, errs := wbcrud.NormalizeRequestColumn(raw, "columns", i)
			if len(errs) > 0 {
				t.Errorf("%s column %d: %+v", rc.Slug, i, errs)
				continue
			}
			id, _ := c.Get("id").(string)
			if id == "" || seen[id] {
				t.Errorf("%s column %d: empty or duplicate id %q", rc.Slug, i, id)
			}
			seen[id] = true
			cols = append(cols, c)
		}
		if g := wbcrud.ValidateNewCycles(nil, cols); g != nil {
			t.Errorf("%s: %v", rc.Slug, g)
		}
	}
}
