package wbregistry

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"
)

func mustLoad(t *testing.T) *Registry {
	t.Helper()
	r, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return r
}

func TestRegistryEmbeddedMatchesPythonExportShape(t *testing.T) {
	r := mustLoad(t)
	var ct map[string]map[string]any
	if err := json.Unmarshal(r.ColumnTypes(), &ct); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"lead_field", "source", "enrichment"} {
		if _, ok := ct[k]; !ok {
			t.Errorf("column type %q missing", k)
		}
	}
	if len(r.LeadFields()) < 30 {
		t.Errorf("lead fields = %d", len(r.LeadFields()))
	}
	if len(r.Providers()) == 0 {
		t.Error("no providers")
	}
	if _, ok := r.Recipes(""); !ok {
		t.Error("all recipes must be a known category query")
	}
	if len(r.Templates("")) == 0 {
		t.Error("no templates")
	}
}

func TestRecipeOrderPreserved(t *testing.T) {
	r := mustLoad(t)
	all, _ := r.Recipes("")
	slugs := make([]string, len(all))
	for i, rc := range all {
		slugs[i] = rc.Slug
	}
	if !sort.StringsAreSorted(slugs) {
		// python loads yaml files sorted by filename; slug order follows file order
		t.Logf("slugs not alphabetical (file order): %v", slugs)
	}
	if len(slugs) != 19 {
		t.Errorf("recipes = %d, want 19", len(slugs))
	}
	var cats []string
	if err := orderedKeys(r.RecipeCategories(), &cats); err != nil {
		t.Fatal(err)
	}
	want := []string{"list-building", "crm-hygiene", "outbound", "research", "signals"}
	if !reflect.DeepEqual(cats, want) {
		t.Errorf("recipe categories order = %v", cats)
	}
	// Recipes keep the top-level key order slug,name,description,category,icon,workbook.
	var keys []string
	if err := orderedKeys(all[0].Raw, &keys); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(keys, []string{"slug", "name", "description", "category", "icon", "workbook"}) {
		t.Errorf("recipe key order = %v", keys)
	}
}

func TestEveryRecipeColumnPassesNormalize(t *testing.T) {
	t.Skip("consumes Task 3 column normalizer; Task 22 removes this skip")
}

func TestProviderCertificationFieldsPresent(t *testing.T) {
	r := mustLoad(t)
	for _, p := range r.Providers() {
		if p.Name == "" || p.Maturity == "" || len(p.RequiredChecks) == 0 {
			t.Errorf("provider %+v lacks certification fields", p)
		}
		if p.Capabilities == nil {
			t.Errorf("provider %s capabilities nil", p.Name)
		}
		if len(p.Certification) == 0 {
			t.Errorf("provider %s certification missing", p.Name)
		}
	}
}

func TestPresetsFilterByCategory(t *testing.T) {
	r := mustLoad(t)
	all, cats := r.AIPresets("")
	var allP []map[string]any
	if err := json.Unmarshal(all, &allP); err != nil {
		t.Fatal(err)
	}
	if string(cats) == "" || len(allP) == 0 {
		t.Fatal("empty presets")
	}
	var c []string
	_ = json.Unmarshal(cats, &c)
	if !reflect.DeepEqual(c, []string{"personalization", "qualification", "research"}) {
		t.Errorf("categories = %v", c)
	}
	sub, cats2 := r.AIPresets("research")
	var subP []map[string]any
	_ = json.Unmarshal(sub, &subP)
	if len(subP) == 0 || len(subP) >= len(allP) {
		t.Errorf("filtered %d of %d", len(subP), len(allP))
	}
	for _, p := range subP {
		if p["category"] != "research" {
			t.Errorf("preset %v not research", p["id"])
		}
	}
	if string(cats2) != string(cats) {
		t.Error("categories must be unfiltered")
	}
	none, _ := r.AIPresets("nope")
	if string(none) != "[]" {
		t.Errorf("unknown category presets = %s, want []", none)
	}
}

func TestRecipeSummaryKeysMatchFixture(t *testing.T) {
	r := mustLoad(t)
	all, _ := r.Recipes("")
	s := RecipeSummary(all[0])
	var keys []string
	for k := range s {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{"category", "column_types", "columns_count", "description", "icon", "name", "slug"}
	if !reflect.DeepEqual(keys, want) {
		t.Errorf("keys = %v", keys)
	}
	b, err := json.Marshal(s["column_types"])
	if err != nil || len(b) < 3 {
		t.Errorf("column_types marshal %s %v", b, err)
	}
	if _, ok := r.Recipes("bogus"); ok {
		t.Error("unknown recipe category must report false")
	}
	rc, ok := r.Recipe(all[0].Slug)
	if !ok || rc.Slug != all[0].Slug {
		t.Error("Recipe by slug")
	}
	tpl := r.Templates("sales")
	if len(tpl) == 0 {
		t.Fatal("no sales templates")
	}
	if got, ok := r.Template(tpl[0].ID); !ok || got.ID != tpl[0].ID {
		t.Error("Template by id")
	}
	if _, ok := r.Template("missing"); ok {
		t.Error("missing template found")
	}
}
