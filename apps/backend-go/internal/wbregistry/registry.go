// Package wbregistry holds the static workbook registries (column types, lead
// fields, AI column presets, enrichment providers, recipes, templates) exported
// from the Python source of truth by legacy/python/scripts/export_workbook_registry.py.
package wbregistry

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
)

//go:embed registry.json
var registryJSON []byte

// Provider is one /meta/providers entry (certification fields included).
type Provider struct {
	Name           string          `json:"name"`
	Capabilities   []string        `json:"capabilities"`
	Confidence     float64         `json:"confidence"`
	RequiredChecks []string        `json:"required_checks"`
	Maturity       string          `json:"maturity"`
	Certification  json.RawMessage `json:"certification"`
}

// Recipe is a shipped recipe. Raw is the full object in Python key order.
type Recipe struct {
	Slug        string          `json:"slug"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Category    string          `json:"category"`
	Icon        string          `json:"icon"`
	Workbook    json.RawMessage `json:"workbook"`
	Raw         json.RawMessage `json:"-"`
}

// UnmarshalJSON keeps the raw bytes alongside the typed fields.
func (r *Recipe) UnmarshalJSON(b []byte) error {
	type plain Recipe
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*r = Recipe(p)
	r.Raw = append(json.RawMessage(nil), b...)
	return nil
}

// MarshalJSON emits the original object (key order preserved).
func (r Recipe) MarshalJSON() ([]byte, error) { return r.Raw, nil }

// Template is a workbook template. Raw is the full object in Python key order.
type Template struct {
	ID                string          `json:"id"`
	Name              string          `json:"name"`
	Description       string          `json:"description"`
	Category          string          `json:"category"`
	Columns           json.RawMessage `json:"columns"`
	EnrichmentColumns json.RawMessage `json:"enrichment_columns"`
	Filter            json.RawMessage `json:"filter"`
	Raw               json.RawMessage `json:"-"`
}

// UnmarshalJSON keeps the raw bytes alongside the typed fields.
func (t *Template) UnmarshalJSON(b []byte) error {
	type plain Template
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*t = Template(p)
	t.Raw = append(json.RawMessage(nil), b...)
	return nil
}

// MarshalJSON emits the original object (key order preserved).
func (t Template) MarshalJSON() ([]byte, error) { return t.Raw, nil }

// Registry is the decoded registry.json.
type Registry struct {
	ColumnTypesRaw json.RawMessage `json:"column_types"`
	LeadFieldList  []string        `json:"lead_fields"`
	AIPresetsRaw   struct {
		Presets    []json.RawMessage `json:"presets"`
		Categories json.RawMessage   `json:"categories"`
	} `json:"ai_presets"`
	ProviderList []Provider `json:"providers"`
	RecipeSet    struct {
		Categories json.RawMessage `json:"categories"`
		Items      []Recipe        `json:"items"`
	} `json:"recipes"`
	TemplateSet struct {
		Categories json.RawMessage `json:"categories"`
		Items      []Template      `json:"items"`
	} `json:"templates"`
}

// Load decodes the embedded registry.
func Load() (*Registry, error) {
	var r Registry
	if err := json.Unmarshal(registryJSON, &r); err != nil {
		return nil, fmt.Errorf("wbregistry: decode registry: %w", err)
	}
	if len(r.ColumnTypesRaw) == 0 || len(r.RecipeSet.Items) == 0 {
		return nil, errors.New("wbregistry: registry incomplete")
	}
	return &r, nil
}

// ColumnTypes is the COLUMN_TYPES object, key order preserved.
func (r *Registry) ColumnTypes() json.RawMessage { return r.ColumnTypesRaw }

// LeadFields are the LEAD_FIELD_MAP keys in order.
func (r *Registry) LeadFields() []string { return append([]string(nil), r.LeadFieldList...) }

// AIPresets returns the preset array (filtered by category when non-empty) and
// the unfiltered categories array.
func (r *Registry) AIPresets(category string) (presets json.RawMessage, categories json.RawMessage) {
	out := []json.RawMessage{}
	for _, p := range r.AIPresetsRaw.Presets {
		if category != "" {
			var c struct {
				Category string `json:"category"`
			}
			if json.Unmarshal(p, &c) != nil || c.Category != category {
				continue
			}
		}
		out = append(out, p)
	}
	b, _ := json.Marshal(out)
	return b, r.AIPresetsRaw.Categories
}

// Providers lists enrichment providers with certification metadata.
func (r *Registry) Providers() []Provider { return append([]Provider(nil), r.ProviderList...) }

// RecipeCategories is the RECIPE_CATEGORIES object, key order preserved.
func (r *Registry) RecipeCategories() json.RawMessage { return r.RecipeSet.Categories }

// TemplateCategories is the TEMPLATE_CATEGORIES object, key order preserved.
func (r *Registry) TemplateCategories() json.RawMessage { return r.TemplateSet.Categories }

// Recipes lists recipes, optionally filtered by category. ok is false when a
// non-empty category is not one of RECIPE_CATEGORIES.
func (r *Registry) Recipes(category string) ([]Recipe, bool) {
	if category != "" {
		var cats map[string]json.RawMessage
		if json.Unmarshal(r.RecipeSet.Categories, &cats) != nil {
			return nil, false
		}
		if _, known := cats[category]; !known {
			return nil, false
		}
	}
	out := []Recipe{}
	for _, rc := range r.RecipeSet.Items {
		if category == "" || rc.Category == category {
			out = append(out, rc)
		}
	}
	return out, true
}

// Recipe returns one recipe by slug.
func (r *Registry) Recipe(slug string) (Recipe, bool) {
	for _, rc := range r.RecipeSet.Items {
		if rc.Slug == slug {
			return rc, true
		}
	}
	return Recipe{}, false
}

// Templates lists templates, optionally filtered by category.
func (r *Registry) Templates(category string) []Template {
	out := []Template{}
	for _, t := range r.TemplateSet.Items {
		if category == "" || t.Category == category {
			out = append(out, t)
		}
	}
	return out
}

// Template returns one template by id.
func (r *Registry) Template(id string) (Template, bool) {
	for _, t := range r.TemplateSet.Items {
		if t.ID == id {
			return t, true
		}
	}
	return Template{}, false
}

// typeCounts is a column-type histogram that marshals in first-seen order,
// like the Python dict built by recipe_summary.
type typeCounts struct {
	keys   []string
	counts map[string]int
}

// MarshalJSON emits the counts in first-seen order.
func (c typeCounts) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range c.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		buf.Write(kb)
		fmt.Fprintf(&buf, ":%d", c.counts[k])
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// RecipeSummaryView is the gallery-card view; fields marshal in the key order
// of Python recipe_summary.
type RecipeSummaryView struct {
	Slug         string     `json:"slug"`
	Name         string     `json:"name"`
	Description  string     `json:"description"`
	Category     string     `json:"category"`
	Icon         string     `json:"icon"`
	ColumnsCount int        `json:"columns_count"`
	ColumnTypes  typeCounts `json:"column_types"`
}

// RecipeSummary is the Python recipe_summary view of a recipe.
func RecipeSummary(rc Recipe) RecipeSummaryView {
	var wb struct {
		Columns []struct {
			Type string `json:"type"`
		} `json:"columns"`
	}
	_ = json.Unmarshal(rc.Workbook, &wb)
	tc := typeCounts{counts: map[string]int{}}
	for _, c := range wb.Columns {
		if _, seen := tc.counts[c.Type]; !seen {
			tc.keys = append(tc.keys, c.Type)
		}
		tc.counts[c.Type]++
	}
	return RecipeSummaryView{
		Slug: rc.Slug, Name: rc.Name, Description: rc.Description,
		Category: rc.Category, Icon: rc.Icon,
		ColumnsCount: len(wb.Columns), ColumnTypes: tc,
	}
}

// orderedKeys decodes the top-level keys of a JSON object in document order.
func orderedKeys(raw json.RawMessage, out *[]string) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil {
		return err
	} else if d, ok := tok.(json.Delim); !ok || d != '{' {
		return errors.New("wbregistry: expected JSON object")
	}
	*out = nil
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return err
		}
		*out = append(*out, kt.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return err
		}
	}
	return nil
}
