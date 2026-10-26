package wbcrud

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

// ViewFilter is one filter clause of a saved view.
type ViewFilter struct {
	Column string `json:"column"`
	Op     string `json:"op"`
	Value  any    `json:"value"`
}

// ViewSort is one sort clause.
type ViewSort struct {
	Column string `json:"column"`
	Dir    string `json:"dir"`
}

// ViewConfig is the saved-view config (schemas.ViewConfig).
type ViewConfig struct {
	Filters       []ViewFilter `json:"filters"`
	Sort          []ViewSort   `json:"sort"`
	HiddenColumns []string     `json:"hidden_columns"`
}

const filterOpMsg = "Input should be 'equals', 'not_equals', 'contains', 'not_contains', 'empty' or 'not_empty'"

var filterOps = map[string]bool{"equals": true, "not_equals": true, "contains": true, "not_contains": true, "empty": true, "not_empty": true}

// ParseViewConfig validates like ViewConfig.model_validate (defaults applied,
// unknown keys dropped). Error locs start with prefix (e.g. "body","config").
func ParseViewConfig(raw json.RawMessage, prefix ...any) (ViewConfig, []httpx.FieldError) {
	return parseViewConfig(raw, true, prefix)
}

func parseViewConfig(raw json.RawMessage, capped bool, prefix []any) (ViewConfig, []httpx.FieldError) {
	lim := CurrentLimits()
	cfg := ViewConfig{Filters: []ViewFilter{}, Sort: []ViewSort{}, HiddenColumns: []string{}}
	doc, err := decodeDocument(raw, false)
	if err != nil {
		return cfg, []httpx.FieldError{{Type: "json_invalid", Loc: append([]any{}, prefix...), Msg: "JSON decode error"}}
	}
	obj, ok := doc.(*Obj)
	if !ok {
		return cfg, []httpx.FieldError{{Type: "model_type", Loc: append([]any{}, prefix...), Msg: "Input should be a valid dictionary or instance of ViewConfig", Input: doc}}
	}
	var errs []httpx.FieldError
	if v, ok := obj.Get("filters"); ok {
		arr, isArr := v.([]any)
		if !isArr {
			errs = append(errs, fe("list_type", "Input should be a valid list", pl(prefix, "filters"), v)...)
		}
		if capped && len(arr) > lim.MaxViewFilters {
			return ViewConfig{}, tooLong("List", lim.MaxViewFilters, len(arr), pl(prefix, "filters"))
		}
		for i, e := range arr {
			loc := pl(prefix, "filters", i)
			o, isObj := e.(*Obj)
			if !isObj {
				errs = append(errs, fe("model_type", "Input should be a valid dictionary or instance of ViewFilter", loc, e)...)
				continue
			}
			f := ViewFilter{Op: "contains"}
			var ferr []httpx.FieldError
			f.Column, ferr = clauseColumn(o, loc)
			errs = append(errs, ferr...)
			if opv, ok := o.Get("op"); ok {
				if s, isStr := opv.(string); isStr && filterOps[s] {
					f.Op = s
				} else {
					errs = append(errs, fe("literal_error", filterOpMsg, append(append([]any{}, loc...), "op"), opv)...)
				}
			}
			f.Value, _ = o.Get("value")
			cfg.Filters = append(cfg.Filters, f)
		}
	}
	if v, ok := obj.Get("sort"); ok {
		arr, isArr := v.([]any)
		if !isArr {
			errs = append(errs, fe("list_type", "Input should be a valid list", pl(prefix, "sort"), v)...)
		}
		if capped && len(arr) > lim.MaxViewSorts {
			return ViewConfig{}, tooLong("List", lim.MaxViewSorts, len(arr), pl(prefix, "sort"))
		}
		for i, e := range arr {
			loc := pl(prefix, "sort", i)
			o, isObj := e.(*Obj)
			if !isObj {
				errs = append(errs, fe("model_type", "Input should be a valid dictionary or instance of ViewSort", loc, e)...)
				continue
			}
			s := ViewSort{Dir: "asc"}
			var ferr []httpx.FieldError
			s.Column, ferr = clauseColumn(o, loc)
			errs = append(errs, ferr...)
			if dv, ok := o.Get("dir"); ok {
				if d, isStr := dv.(string); isStr && (d == "asc" || d == "desc") {
					s.Dir = d
				} else {
					errs = append(errs, fe("literal_error", "Input should be 'asc' or 'desc'", append(append([]any{}, loc...), "dir"), dv)...)
				}
			}
			cfg.Sort = append(cfg.Sort, s)
		}
	}
	if v, ok := obj.Get("hidden_columns"); ok {
		arr, isArr := v.([]any)
		if !isArr {
			errs = append(errs, fe("list_type", "Input should be a valid list", pl(prefix, "hidden_columns"), v)...)
		}
		for i, e := range arr {
			if s, isStr := e.(string); isStr {
				cfg.HiddenColumns = append(cfg.HiddenColumns, s)
			} else {
				errs = append(errs, fe("string_type", "Input should be a valid string", pl(prefix, "hidden_columns", i), e)...)
			}
		}
	}
	if len(errs) > 0 {
		return ViewConfig{}, errs
	}
	return cfg, nil
}

func clauseColumn(o *Obj, loc []any) (string, []httpx.FieldError) {
	cl := append(append([]any{}, loc...), "column")
	v, ok := o.Get("column")
	if !ok {
		return "", fe("missing", "Field required", cl, o)
	}
	s, isStr := v.(string)
	if !isStr {
		return "", fe("string_type", "Input should be a valid string", cl, v)
	}
	if s == "" {
		return "", fe("string_too_short", "String should have at least 1 character", cl, v)
	}
	return s, nil
}

// MissingColumns returns the sorted, de-duplicated referenced column ids that
// are not in known.
func (v ViewConfig) MissingColumns(known map[string]struct{}) []string {
	seen := map[string]struct{}{}
	add := func(c string) {
		if _, ok := known[c]; !ok {
			seen[c] = struct{}{}
		}
	}
	for _, f := range v.Filters {
		add(f.Column)
	}
	for _, s := range v.Sort {
		add(s.Column)
	}
	for _, h := range v.HiddenColumns {
		add(h)
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// ValidateViewColumns is _validate_view_columns against a workbook's columns.
func ValidateViewColumns(cols []Column, v ViewConfig) *GuardError {
	known := map[string]struct{}{}
	for _, c := range cols {
		if id, ok := idString(c); ok {
			known[id] = struct{}{}
		}
	}
	missing := v.MissingColumns(known)
	if len(missing) == 0 {
		return nil
	}
	return &GuardError{Status: 422, Detail: fmt.Sprintf("View references missing columns: %s. Refresh the workbook and update the view.", strings.Join(missing, ", "))}
}

func pl(prefix []any, rest ...any) []any {
	return append(append([]any{}, prefix...), rest...)
}

// ParseStoredViewConfig parses a config already in the database; unlike
// ParseViewConfig it applies no request-shape caps.
func ParseStoredViewConfig(raw json.RawMessage, prefix ...any) (ViewConfig, []httpx.FieldError) {
	return parseViewConfig(raw, false, prefix)
}
