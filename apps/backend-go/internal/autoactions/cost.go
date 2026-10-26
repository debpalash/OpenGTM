package autoactions

import (
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/billing"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

// Action types whose external effect is idempotent (safe to re-attempt on the
// same in_flight marker) and the at-most-once ones (actions.py).
var (
	IdempotentTypes    = map[string]bool{"push_crm": true, "re_enrich": true}
	NonIdempotentTypes = map[string]bool{"webhook": true, "send_email": true}
)

// configOf is `action.get("config") or {}`: a falsy or non-object config is
// empty. Ordered objects (colgraph.Col, *wbcrud.Obj) are read through their
// own accessors but their nested values stay ordered.
func configOf(v any) map[string]any {
	switch m := v.(type) {
	case map[string]any:
		return m
	case Column:
		return m.Vals
	case interface {
		Keys() []string
		Get(string) (any, bool)
	}:
		out := map[string]any{}
		for _, k := range m.Keys() {
			out[k], _ = m.Get(k)
		}
		return out
	}
	return map[string]any{}
}

func colID(c Column) (string, bool) {
	s, ok := c.Get("id").(string)
	return s, ok
}

// ProvidersByColumn is _providers_by_column (routers/workbooks.py): per
// configured column id present in the workbook, its waterfall, else
// [provider], else defaults[target] with target = target_field or lead_field
// or id. Unknown ids contribute nothing; the map key is the column id (or the
// target when the id is empty). Never nil chains.
func ProvidersByColumn(columnIDs []any, cols []Column, defaults map[string][]string) map[string][]string {
	byID := map[string]Column{}
	for _, c := range cols {
		if id, ok := colID(c); ok {
			byID[id] = c
		}
	}
	out := map[string][]string{}
	for _, raw := range columnIDs {
		cid, ok := raw.(string)
		if !ok {
			continue
		}
		c, ok := byID[cid]
		if !ok {
			continue
		}
		target := ""
		for _, k := range []string{"target_field", "lead_field", "id"} {
			if v := c.Get(k); providers.Truthy(v) {
				target = str(v)
				break
			}
		}
		chain := []string{}
		if w, ok := c.Get("waterfall").([]any); ok && len(w) > 0 {
			for _, p := range w {
				chain = append(chain, str(p))
			}
		} else if p := c.Get("provider"); providers.Truthy(p) {
			chain = []string{str(p)}
		} else {
			chain = append(chain, defaults[target]...)
		}
		key := cid
		if key == "" {
			key = target
		}
		out[key] = chain
	}
	return out
}

// DefaultWaterfallsFor is DEFAULT_WATERFALLS restricted to the targets of cols
// (the third argument of ProjectCost).
func DefaultWaterfallsFor(cols []Column) map[string][]string {
	out := map[string][]string{}
	for _, c := range cols {
		for _, k := range []string{"target_field", "lead_field", "id"} {
			if v := c.Get(k); providers.Truthy(v) {
				t := str(v)
				if w := workbook.DefaultWaterfall(t); len(w) > 0 {
					out[t] = w
				}
			}
		}
	}
	return out
}

// ProjectCost is project_action_cost: the worst-case platform-billed USD of one
// row's action. Only re_enrich costs anything (send_email enqueues for free;
// metering is on the send job).
func ProjectCost(action map[string]any, cols []Column, defaults map[string][]string) float64 {
	if t, _ := action["type"].(string); t != "re_enrich" {
		return 0
	}
	ids, _ := configOf(action["config"])["column_ids"].([]any)
	return billing.ProjectedPlatformCost(1, ProvidersByColumn(ids, cols, defaults), nil)
}
