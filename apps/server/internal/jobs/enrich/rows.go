package enrich

import (
	"fmt"
	"strings"

	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/pycompat"
)

// column is one workbook column definition (columns_config entry).
type column struct {
	cfg  *pycompat.Map
	id   string
	name string
	typ  string
}

func mapGet(m *pycompat.Map, key string) any {
	v, _ := m.Get(key)
	return v
}

func newColumn(v any) (*column, bool) {
	m, ok := v.(*pycompat.Map)
	if !ok {
		return nil, false
	}
	c := &column{cfg: m}
	c.id, _ = mapGet(m, "id").(string)
	c.name, _ = mapGet(m, "name").(string)
	c.typ, _ = mapGet(m, "type").(string)
	return c, true
}

// truthyString is `col.get(key)` when it is a non-empty string, else "".
func (c *column) truthyString(key string) string {
	s, _ := mapGet(c.cfg, key).(string)
	return s
}

// targetField is `target_field or lead_field or column id`.
func (c *column) targetField() string {
	for _, k := range []string{"target_field", "lead_field"} {
		if v := mapGet(c.cfg, k); pycompat.Truthy(v) {
			return pycompat.Str(v)
		}
	}
	return c.id
}

// targetFieldForVerify is `target_field or lead_field` without the column-id
// fallback: the key the email verify cascade is gated on.
func (c *column) targetFieldForVerify() string {
	for _, k := range []string{"target_field", "lead_field"} {
		if v := mapGet(c.cfg, k); pycompat.Truthy(v) {
			return pycompat.Str(v)
		}
	}
	return ""
}

// explicitChain is the user-selected chain: a waterfall list (even empty), or
// the single provider. selected mirrors `explicit_selection`.
func (c *column) explicitChain() (chain []string, selected bool) {
	if wf, ok := c.cfg.Get("waterfall"); ok && wf != nil {
		list, _ := wf.([]any)
		for _, e := range list {
			chain = append(chain, pycompat.Str(e))
		}
		return chain, true
	}
	if p := mapGet(c.cfg, "provider"); pycompat.Truthy(p) {
		return []string{pycompat.Str(p)}, true
	}
	return nil, false
}

// workRow is one workbook row ready to run. lead is the hydrated execution
// dictionary of cell_scope.row_execution_data and is never mutated: each run
// of a row works on a copy.
type workRow struct {
	id   int64
	lead *pycompat.Map
}

func (r *workRow) subject() int64 { return r.id }

// hydrate is cell_scope.row_execution_data: saved dependency values are
// overlaid on the row data, failed or partial snapshots invalidate stale
// copies, and the database identities are asserted last.
func hydrate(id int64, leadID *int64, dataJSON, enrJSON []byte, columns []*column) (*pycompat.Map, error) {
	data, err := decodeObject(dataJSON)
	if err != nil {
		return nil, fmt.Errorf("row %d data: %w", id, err)
	}
	enr, err := decodeObject(enrJSON)
	if err != nil {
		return nil, fmt.Errorf("row %d enrichments: %w", id, err)
	}
	data = data.Copy()
	values, aliases := pycompat.NewMap(), pycompat.NewMap()
	for _, col := range columns {
		cell, _ := enr.Get(col.id)
		cm, isCell := cell.(*pycompat.Map)
		if !isCell {
			continue
		}
		status, _ := mapGet(cm, "status").(string)
		value, hasValue := cm.Get("value")
		if status != "complete" || !hasValue || value == nil {
			data.Delete(col.id)
			if col.name != "" {
				data.Delete(col.name)
			}
			continue
		}
		_ = values.Set(col.id, value)
		if col.name != "" {
			_ = aliases.Set(col.name, value)
		}
	}
	out := data
	for _, e := range aliases.Entries() {
		_ = out.Set(e.Key, e.Value)
	}
	for _, e := range values.Entries() {
		_ = out.Set(e.Key, e.Value)
	}
	if leadID != nil && *leadID != 0 {
		_ = out.Set("id", *leadID)
	} else {
		_ = out.Set("id", id)
	}
	_ = out.Set("__row_id", id)
	if leadID != nil {
		_ = out.Set("__lead_id", *leadID)
	} else {
		_ = out.Set("__lead_id", nil)
	}
	return out, nil
}

// decodeObject parses a JSON column holding an object; NULL and JSON null are
// `{}` (Python's `row.data or {}`).
func decodeObject(raw []byte) (*pycompat.Map, error) {
	if len(raw) == 0 {
		return pycompat.NewMap(), nil
	}
	v, err := pycompat.LoadJSON(raw)
	if err != nil {
		return nil, err
	}
	switch t := v.(type) {
	case nil:
		return pycompat.NewMap(), nil
	case *pycompat.Map:
		return t, nil
	}
	if !pycompat.Truthy(v) {
		return pycompat.NewMap(), nil
	}
	return nil, fmt.Errorf("expected a JSON object, got %s", pycompat.TypeName(v))
}

// providerInputs is declarative.compiler._lead_input_ctx over
// Lead.from_dict(lead). A non-string contact or website raises in Python
// (AttributeError inside the provider); the same text is returned as an error.
func providerInputs(lead *pycompat.Map) (map[string]any, error) {
	get := func(k string) any {
		v, _ := lead.Get(k)
		if !pycompat.Truthy(v) {
			return ""
		}
		return v
	}
	contact := get("contact_person")
	first, last := "", ""
	if pycompat.Truthy(contact) {
		s, ok := contact.(string)
		if !ok {
			return nil, fmt.Errorf("'%s' object has no attribute 'split'", pycompat.TypeName(contact))
		}
		parts := strings.FieldsFunc(s, pycompat.IsSpace)
		if len(parts) > 0 {
			first = parts[0]
		}
		if len(parts) > 1 {
			last = parts[len(parts)-1]
		}
	}
	website := get("website")
	domain, err := domainOf(website)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"company":        get("company"),
		"company_name":   get("company"),
		"website":        website,
		"domain":         domain,
		"email":          get("email"),
		"linkedin_url":   get("linkedin_url"),
		"city":           get("city"),
		"contact_person": contact,
		"full_name":      contact,
		"first_name":     first,
		"last_name":      last,
		"phone":          get("phone"),
	}, nil
}

// domainOf is compiler._domain_of.
func domainOf(website any) (string, error) {
	if !pycompat.Truthy(website) {
		return "", nil
	}
	s, ok := website.(string)
	if !ok {
		return "", fmt.Errorf("'%s' object has no attribute 'strip'", pycompat.TypeName(website))
	}
	d := strings.ToLower(pycompat.Strip(s))
	for _, p := range []string{"https://", "http://", "www."} {
		d = strings.TrimPrefix(d, p)
	}
	d, _, _ = strings.Cut(d, "/")
	d, _, _ = strings.Cut(d, "?")
	return d, nil
}
