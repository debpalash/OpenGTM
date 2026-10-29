// Package crmpush is output._push_crm: push one workbook row to the workspace's
// CRM (HubSpot or Salesforce), updating by external id when the row came from a
// CRM import and creating otherwise.
package crmpush

import (
	"sort"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/crm"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadsread"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/rowtmpl"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// FieldMapOf reads destination_config.field_map as an ordered map. The order
// of a decoded JSON object is only known for colgraph.Col, *wbcrud.Obj and
// crm.FieldMap; a plain map[string]any is sorted by key (deterministic). A
// missing, empty or non-object value yields nil ("use the CRM default").
func FieldMapOf(v any) crm.FieldMap {
	var out crm.FieldMap
	switch m := v.(type) {
	case crm.FieldMap:
		out = append(out, m...)
	case colgraph.Col:
		for _, k := range m.Keys {
			out = append(out, crm.FieldPair{Key: k, Value: rowtmpl.PyStr(m.Vals[k])})
		}
	case *wbcrud.Obj:
		if m == nil {
			return nil
		}
		for _, k := range m.Keys() {
			x, _ := m.Get(k)
			out = append(out, crm.FieldPair{Key: k, Value: rowtmpl.PyStr(x)})
		}
	case map[string]any:
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			out = append(out, crm.FieldPair{Key: k, Value: rowtmpl.PyStr(m[k])})
		}
	case map[string]string:
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			out = append(out, crm.FieldPair{Key: k, Value: m[k]})
		}
	}
	return out
}

// UpdateFieldMap is _crm_update_fields with the property order Python's dict
// would have (first insertion position, last value). Only mapped fields are
// sent: cfg.field_map ({lead_field: crm_field}) when set, else the CRM's
// default map; contact_person is split into first/last name fields.
func UpdateFieldMap(crmType string, cfg, leadData map[string]any) crm.FieldMap {
	var defaults crm.FieldMap
	firstKey, lastKey := "FirstName", "LastName"
	if crmType == "hubspot" {
		defaults, firstKey, lastKey = crm.DefaultFieldMap, "firstname", "lastname"
	} else {
		defaults = crm.ContactUpdateFieldMap
	}
	fmap := FieldMapOf(cfg["field_map"])
	if len(fmap) == 0 {
		fmap = defaults
	}
	var out crm.FieldMap
	set := func(k, v string) {
		for i := range out {
			if out[i].Key == k {
				out[i].Value = v
				return
			}
		}
		out = append(out, crm.FieldPair{Key: k, Value: v})
	}
	for _, p := range fmap {
		value := leadData[p.Key]
		if s, isStr := value.(string); value == nil || (isStr && s == "") {
			continue
		}
		text := rowtmpl.PyStr(value)
		if first, last, found := strings.Cut(text, " "); p.Key == "contact_person" && found {
			set(firstKey, first)
			set(lastKey, last)
		} else {
			set(p.Value, text)
		}
	}
	return out
}

// UpdateFields is _crm_update_fields as the Python dict.
func UpdateFields(crmType string, cfg, leadData map[string]any) map[string]string {
	out := map[string]string{}
	for _, p := range UpdateFieldMap(crmType, cfg, leadData) {
		out[p.Key] = p.Value
	}
	return out
}

// dictLead is Lead.from_dict(lead_data): keys that are not Lead fields are
// ignored, unset Lead fields are empty (score_tier and status keep their
// dataclass defaults).
type dictLead map[string]any

func (d dictLead) Get(key string) (any, bool) {
	if _, known := (leadsread.Lead{}).Get(key); !known {
		return nil, false
	}
	if v, ok := d[key]; ok {
		return v, true
	}
	switch key {
	case "score_tier":
		return "unqualified", true
	case "status":
		return "new", true
	}
	return nil, true
}
