package autoactions

import (
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/billing"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
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
// own accessors and every nested value is normalised by plain, so the
// executors only ever see map[string]any, colgraph.Col, []any and scalars.
func configOf(v any) map[string]any {
	switch m := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(m))
		for k, e := range m {
			out[k] = plain(e)
		}
		return out
	case Column:
		out := make(map[string]any, len(m.Vals))
		for k, e := range m.Vals {
			out[k] = plain(e)
		}
		return out
	case *wbcrud.Obj:
		out := map[string]any{}
		if m == nil {
			return out
		}
		for _, k := range m.Keys() {
			e, _ := m.Get(k)
			out[k] = plain(e)
		}
		return out
	}
	return map[string]any{}
}

// plain normalises a decoded config value: *wbcrud.Obj becomes an ordered
// colgraph.Col (key order kept, as Python's dict keeps it), and containers are
// walked recursively. Scalars pass through.
func plain(v any) any {
	switch t := v.(type) {
	case *wbcrud.Obj:
		if t == nil {
			return nil
		}
		keys := t.Keys()
		out := Column{Keys: keys, Vals: make(map[string]any, len(keys))}
		for _, k := range keys {
			e, _ := t.Get(k)
			out.Vals[k] = plain(e)
		}
		return out
	case Column:
		out := Column{Keys: append([]string(nil), t.Keys...), Vals: make(map[string]any, len(t.Vals))}
		for k, e := range t.Vals {
			out.Vals[k] = plain(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = plain(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = plain(e)
		}
		return out
	}
	return v
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
