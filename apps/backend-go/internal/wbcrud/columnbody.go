package wbcrud

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

// Request bodies of the column routes, validated like FastAPI/pydantic v2
// (loc prefix "body", errors in field order, then forbidden extras). Numbers
// are canonical (a float round-trips as Python's repr) and objects keep their
// key order.

func bodyLoc(rest ...any) []any { return append([]any{"body"}, rest...) }

func missingErr(loc []any, whole any) httpx.FieldError {
	return httpx.FieldError{Type: "missing", Loc: loc, Msg: "Field required", Input: whole}
}

// decodeBody decodes a body that is present: an empty body is "missing", bad
// JSON json_invalid.
func decodeBody(raw []byte) (any, []httpx.FieldError) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, []httpx.FieldError{missingErr(bodyLoc(), nil)}
	}
	doc, err := decodeDocument(raw, true)
	if err != nil {
		return nil, []httpx.FieldError{{Type: "json_invalid", Loc: bodyLoc(0), Msg: "JSON decode error", Input: map[string]any{}}}
	}
	return doc, nil
}

func notObjectErr(doc any) []httpx.FieldError {
	return []httpx.FieldError{{Type: "model_attributes_type", Loc: bodyLoc(), Msg: "Input should be a valid dictionary or object to extract fields from", Input: doc}}
}

func bodyObject(raw []byte) (*Obj, []httpx.FieldError) {
	doc, errs := decodeBody(raw)
	if errs != nil {
		return nil, errs
	}
	o, ok := doc.(*Obj)
	if !ok {
		return nil, notObjectErr(doc)
	}
	return o, nil
}

func dictField(o *Obj, name string, errs *[]httpx.FieldError) *Obj {
	v, present := o.Get(name)
	if !present {
		*errs = append(*errs, missingErr(bodyLoc(name), o))
		return nil
	}
	d, ok := v.(*Obj)
	if !ok {
		*errs = append(*errs, httpx.FieldError{Type: "dict_type", Loc: bodyLoc(name), Msg: "Input should be a valid dictionary", Input: v})
		return nil
	}
	return d
}

func strListField(o *Obj, name string, errs *[]httpx.FieldError) []string {
	v, present := o.Get(name)
	if !present {
		*errs = append(*errs, missingErr(bodyLoc(name), o))
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		*errs = append(*errs, httpx.FieldError{Type: "list_type", Loc: bodyLoc(name), Msg: "Input should be a valid list", Input: v})
		return nil
	}
	out := make([]string, 0, len(arr))
	bad := false
	for i, e := range arr {
		s, ok := e.(string)
		if !ok {
			bad = true
			*errs = append(*errs, httpx.FieldError{Type: "string_type", Loc: bodyLoc(name, i), Msg: "Input should be a valid string", Input: e})
			continue
		}
		out = append(out, s)
	}
	if bad {
		return nil
	}
	return out
}

// forbidExtras appends extra_forbidden for every key outside known (input order).
func forbidExtras(o *Obj, known []string, errs *[]httpx.FieldError) {
	for _, k := range o.Keys() {
		isKnown := false
		for _, n := range known {
			isKnown = isKnown || n == k
		}
		if !isKnown {
			v, _ := o.Get(k)
			*errs = append(*errs, httpx.FieldError{Type: "extra_forbidden", Loc: bodyLoc(k), Msg: "Extra inputs are not permitted", Input: v})
		}
	}
}

// SettingsBody is ColumnSettingsUpdate.
type SettingsBody struct{ Changes, Expected *Obj }

// ParseSettingsBody validates {"changes": dict, "expected": dict} (extra forbidden).
func ParseSettingsBody(raw []byte) (SettingsBody, []httpx.FieldError) {
	o, errs := bodyObject(raw)
	if errs != nil {
		return SettingsBody{}, errs
	}
	b := SettingsBody{Changes: dictField(o, "changes", &errs), Expected: dictField(o, "expected", &errs)}
	forbidExtras(o, []string{"changes", "expected"}, &errs)
	if len(errs) > 0 {
		return SettingsBody{}, errs
	}
	return b, nil
}

// OrderBody is ColumnOrderUpdate.
type OrderBody struct{ ColumnIDs, Expected []string }

// ParseOrderBody validates {"column_ids": [str], "expected_column_ids": [str]} (extra forbidden).
func ParseOrderBody(raw []byte) (OrderBody, []httpx.FieldError) {
	o, errs := bodyObject(raw)
	if errs != nil {
		return OrderBody{}, errs
	}
	b := OrderBody{ColumnIDs: strListField(o, "column_ids", &errs), Expected: strListField(o, "expected_column_ids", &errs)}
	forbidExtras(o, []string{"column_ids", "expected_column_ids"}, &errs)
	if len(errs) > 0 {
		return OrderBody{}, errs
	}
	return b, nil
}

// ParseWidthBody validates ColumnWidthUpdate: width strict int, 80..600 (extra keys ignored).
func ParseWidthBody(raw []byte) (int64, []httpx.FieldError) {
	o, errs := bodyObject(raw)
	if errs != nil {
		return 0, errs
	}
	loc := bodyLoc("width")
	v, present := o.Get("width")
	if !present {
		return 0, []httpx.FieldError{missingErr(loc, o)}
	}
	if num, ok := v.(json.Number); ok && isIntText(string(num)) {
		if _, err := strconv.ParseInt(string(num), 10, 64); errors.Is(err, strconv.ErrRange) {
			if string(num)[0] == '-' {
				return 0, []httpx.FieldError{geErr(loc, v)}
			}
			return 0, []httpx.FieldError{leErr(loc, v)}
		}
	}
	n, ferrs := parseInt(v, true, loc)
	if len(ferrs) > 0 {
		return 0, ferrs
	}
	if n < 80 {
		return 0, []httpx.FieldError{geErr(loc, v)}
	}
	if n > 600 {
		return 0, []httpx.FieldError{leErr(loc, v)}
	}
	return n, nil
}

func geErr(loc []any, v any) httpx.FieldError {
	return httpx.FieldError{Type: "greater_than_equal", Loc: loc, Msg: "Input should be greater than or equal to 80", Input: v, Ctx: map[string]any{"ge": 80}}
}

func leErr(loc []any, v any) httpx.FieldError {
	return httpx.FieldError{Type: "less_than_equal", Loc: loc, Msg: "Input should be less than or equal to 600", Input: v, Ctx: map[string]any{"le": 600}}
}

// ParseAddBody validates AddColumnRequest and normalises the column exactly
// like ColumnConfig.model_dump(exclude_none=True).
func ParseAddBody(raw []byte) (Column, []httpx.FieldError) {
	o, errs := bodyObject(raw)
	if errs != nil {
		return Column{}, errs
	}
	v, present := o.Get("column")
	if !present {
		return Column{}, []httpx.FieldError{missingErr(bodyLoc("column"), o)}
	}
	col, cerrs := normalizeColumnDoc(v, bodyLoc("column"))
	return col, FastAPIModelErrors(cerrs)
}

// FastAPIModelErrors rewrites NormalizeRequestColumn's errors into what FastAPI
// answers for a request body. It validates with from_attributes=True, so a
// non-object model value reports model_attributes_type instead of model_type
// (fixture columns/add_column_null), and pydantic's ctx (the violated ge/gt
// bound) is part of the 422 body (fixture columns/add_bad_types).
func FastAPIModelErrors(errs []httpx.FieldError) []httpx.FieldError {
	for i := range errs {
		e := &errs[i]
		switch e.Type {
		case "model_type":
			e.Type = "model_attributes_type"
			e.Msg = "Input should be a valid dictionary or object to extract fields from"
		case "greater_than_equal", "greater_than":
			if f, ok := boundField(e.Loc); ok {
				if e.Type == "greater_than_equal" && f.ge != nil {
					e.Ctx = map[string]any{"ge": boundNumber(f, *f.ge)}
				} else if e.Type == "greater_than" && f.gt != nil {
					e.Ctx = map[string]any{"gt": boundNumber(f, *f.gt)}
				}
			}
		}
	}
	return errs
}

// boundField finds the field a bound error refers to (the last loc element,
// inside policy when the loc passes through it).
func boundField(loc []any) (field, bool) {
	if len(loc) == 0 {
		return field{}, false
	}
	name, _ := loc[len(loc)-1].(string)
	fields := columnFields
	for _, l := range loc {
		if l == "policy" && len(loc) > 0 && loc[len(loc)-1] != "policy" {
			fields = policyFields
		}
	}
	for _, f := range fields {
		if f.name == name {
			return f, true
		}
	}
	return field{}, false
}

// boundNumber renders a bound like pydantic: a float field's bound prints as 0.0.
func boundNumber(f field, n int) json.Number {
	if f.kind == kFloat {
		return json.Number(pyFloat(float64(n)))
	}
	return json.Number(strconv.Itoa(n))
}

// ParseDeleteBody validates the optional ColumnDeleteRequest. A missing or null
// body yields (nil, nil): the route then skips the "expected column" check.
func ParseDeleteBody(raw []byte) (*Obj, []httpx.FieldError) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	doc, errs := decodeBody(raw)
	if errs != nil {
		return nil, errs
	}
	if doc == nil {
		return nil, nil
	}
	o, ok := doc.(*Obj)
	if !ok {
		return nil, notObjectErr(doc)
	}
	exp := dictField(o, "expected_column", &errs)
	forbidExtras(o, []string{"expected_column"}, &errs)
	if len(errs) > 0 {
		return nil, errs
	}
	return exp, nil
}
