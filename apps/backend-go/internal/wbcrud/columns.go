// Package wbcrud holds the workbook CRUD building blocks ported from
// apps/api/services/workbook: column config model, dependency rules and saved
// view validation.
package wbcrud

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

// Column is one stored column object. It preserves insertion order of every
// key (Python returns the stored JSON dict untouched), unknown keys, and number
// text. Nested objects are *Obj, numbers json.Number. Model-field order applies
// only to NormalizeRequestColumn output.
type Column struct{ o *Obj }

// MakeColumn builds a column from alternating key, value arguments.
func MakeColumn(kv ...any) Column {
	c := Column{o: NewObj()}
	for i := 0; i+1 < len(kv); i += 2 {
		c.o.Set(kv[i].(string), kv[i+1])
	}
	return c
}

// Get returns the value for k (nil when absent or null).
func (c Column) Get(k string) any { v, _ := c.o.Get(k); return v }

// Lookup returns the value and whether the key is present.
func (c Column) Lookup(k string) (any, bool) { return c.o.Get(k) }

// Keys returns keys in stored order.
func (c Column) Keys() []string { return c.o.Keys() }

// Set stores v under k; an existing key keeps its position.
func (c *Column) Set(k string, v any) {
	if c.o == nil {
		c.o = NewObj()
	}
	c.o.Set(k, v)
}

// Clone returns a copy whose top-level key set can be changed independently.
func (c Column) Clone() Column {
	n := Column{o: NewObj()}
	for _, k := range c.o.Keys() {
		n.o.Set(k, c.o.vals[k])
	}
	return n
}

func (c Column) without(skip string) Column {
	n := Column{o: NewObj()}
	for _, k := range c.o.Keys() {
		if k != skip {
			n.o.Set(k, c.o.vals[k])
		}
	}
	return n
}

// MergeChanges is Python's {**col, **changes}: existing keys keep their
// position, new keys are appended in changes order.
func MergeChanges(col Column, changes *Obj) Column {
	n := col.Clone()
	for _, k := range changes.Keys() {
		v, _ := changes.Get(k)
		n.Set(k, v)
	}
	return n
}

// ParseObject decodes a JSON object body preserving key order (for PATCH
// changes). Numbers keep their text.
func ParseObject(raw []byte) (*Obj, error) {
	doc, err := decodeDocument(raw, false)
	if err != nil {
		return nil, err
	}
	o, ok := doc.(*Obj)
	if !ok {
		return nil, fmt.Errorf("not a JSON object")
	}
	return o, nil
}

// MarshalJSON writes keys in stored order.
func (c Column) MarshalJSON() ([]byte, error) {
	if c.o == nil {
		return []byte("null"), nil
	}
	return c.o.MarshalJSON()
}

// columnOrder is the ColumnConfig field order (schemas.py).
var columnOrder = []string{
	"id", "name", "type", "width", "reactive", "lead_field", "provider", "waterfall", "target_field", "verify",
	"icp", "channels", "target_rows", "prompt", "input_columns", "max_steps", "cell_budget_usd", "output_format",
	"max_tokens", "goal", "tools", "policy", "condition", "destination", "destination_config", "run_once",
	"http_url", "http_method", "http_headers", "http_body", "http_extract", "formula",
}

// ParseColumns decodes a stored columns_config JSON array byte-faithfully.
func ParseColumns(raw []byte) ([]Column, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return []Column{}, nil
	}
	doc, err := decodeDocument(raw, false)
	if err != nil {
		return nil, fmt.Errorf("columns_config: %w", err)
	}
	if doc == nil {
		return []Column{}, nil
	}
	arr, ok := doc.([]any)
	if !ok {
		return nil, fmt.Errorf("columns_config is not a JSON array")
	}
	out := make([]Column, 0, len(arr))
	for i, e := range arr {
		o, ok := e.(*Obj)
		if !ok {
			return nil, fmt.Errorf("columns_config[%d] is not an object", i)
		}
		out = append(out, Column{o: o})
	}
	return out, nil
}

type kind int

const (
	kStr kind = iota
	kInt
	kStrictInt
	kFloat
	kBool
	kStrList
	kDict
	kAny
	kPolicy
)

type field struct {
	name     string
	kind     kind
	required bool // required, or non-Optional with a default (null is an error)
	def      any
	ge, gt   *int
}

func zero() *int { z := 0; return &z }

var columnFields = []field{
	{name: "id", kind: kStr, required: true},
	{name: "name", kind: kStr, required: true},
	{name: "type", kind: kStr, def: "lead_field"},
	{name: "width", kind: kInt, def: json.Number("200")},
	{name: "reactive", kind: kBool},
	{name: "lead_field", kind: kStr},
	{name: "provider", kind: kStr},
	{name: "waterfall", kind: kStrList},
	{name: "target_field", kind: kStr},
	{name: "verify", kind: kBool},
	{name: "icp", kind: kDict},
	{name: "channels", kind: kDict},
	{name: "target_rows", kind: kInt},
	{name: "prompt", kind: kStr},
	{name: "input_columns", kind: kStrList},
	{name: "max_steps", kind: kInt},
	{name: "cell_budget_usd", kind: kFloat, ge: zero()},
	{name: "output_format", kind: kStr},
	{name: "max_tokens", kind: kStrictInt, gt: zero()},
	{name: "goal", kind: kStr},
	{name: "tools", kind: kStrList},
	{name: "policy", kind: kPolicy},
	{name: "condition", kind: kStr},
	{name: "destination", kind: kStr},
	{name: "destination_config", kind: kDict},
	{name: "run_once", kind: kBool},
	{name: "http_url", kind: kStr},
	{name: "http_method", kind: kStr},
	{name: "http_headers", kind: kDict},
	{name: "http_body", kind: kAny},
	{name: "http_extract", kind: kStr},
	{name: "formula", kind: kStr},
}

var policyFields = []field{
	{name: "max_steps", kind: kStrictInt, ge: zero()},
	{name: "max_cost_usd", kind: kFloat, ge: zero()},
	{name: "prefer", kind: kStr},
}

// NormalizeRequestColumn validates a request-body column exactly like
// ColumnConfig.model_validate(...).model_dump(exclude_none=True): defaults
// applied, unknown keys dropped, None dropped, model field order. Error locs
// are prefixed with prefix, e.g. ("body","columns_config",i), ("body","column")
// (Python locs); with no prefix they are relative to the column.
func NormalizeRequestColumn(raw json.RawMessage, prefix ...any) (Column, []httpx.FieldError) {
	doc, err := decodeDocument(raw, true)
	if err != nil {
		return Column{}, []httpx.FieldError{{Type: "json_invalid", Loc: append([]any{}, prefix...), Msg: "JSON decode error", Input: nil}}
	}
	obj, ok := doc.(*Obj)
	if !ok {
		return Column{}, []httpx.FieldError{{Type: "model_type", Loc: append([]any{}, prefix...), Msg: "Input should be a valid dictionary or instance of ColumnConfig", Input: doc}}
	}
	vals, errs := validateModel(obj, columnFields, prefix, doc)
	if len(errs) > 0 {
		return Column{}, errs
	}
	col := Column{o: NewObj()}
	for _, k := range columnOrder {
		if v, ok := vals[k]; ok {
			col.o.Set(k, v)
		}
	}
	return col, nil
}

func validateModel(obj *Obj, fields []field, prefix []any, whole any) (map[string]any, []httpx.FieldError) {
	out := map[string]any{}
	var errs []httpx.FieldError
	for _, f := range fields {
		loc := append(append([]any{}, prefix...), f.name)
		v, present := obj.Get(f.name)
		if !present {
			if f.required && f.def == nil {
				errs = append(errs, httpx.FieldError{Type: "missing", Loc: loc, Msg: "Field required", Input: whole})
			} else if f.def != nil {
				out[f.name] = f.def
			}
			continue
		}
		if v == nil && !f.required && f.def == nil {
			continue // Optional field set to null: excluded by exclude_none
		}
		val, ferrs := validateField(f, v, loc)
		if len(ferrs) > 0 {
			errs = append(errs, ferrs...)
			continue
		}
		if val != nil {
			out[f.name] = val
		}
	}
	return out, errs
}

func fe(typ, msg string, loc []any, in any) []httpx.FieldError {
	return []httpx.FieldError{{Type: typ, Loc: loc, Msg: msg, Input: in}}
}

func validateField(f field, v any, loc []any) (any, []httpx.FieldError) {
	switch f.kind {
	case kStr:
		if s, ok := v.(string); ok {
			return s, nil
		}
		return nil, fe("string_type", "Input should be a valid string", loc, v)
	case kInt, kStrictInt:
		n, errs := parseInt(v, f.kind == kStrictInt, loc)
		if len(errs) > 0 {
			return nil, errs
		}
		if f.ge != nil && n < int64(*f.ge) {
			return nil, fe("greater_than_equal", fmt.Sprintf("Input should be greater than or equal to %d", *f.ge), loc, v)
		}
		if f.gt != nil && n <= int64(*f.gt) {
			return nil, fe("greater_than", fmt.Sprintf("Input should be greater than %d", *f.gt), loc, v)
		}
		return json.Number(strconv.FormatInt(n, 10)), nil
	case kFloat:
		x, errs := parseFloat(v, loc)
		if len(errs) > 0 {
			return nil, errs
		}
		if f.ge != nil && x < float64(*f.ge) {
			return nil, fe("greater_than_equal", fmt.Sprintf("Input should be greater than or equal to %d", *f.ge), loc, v)
		}
		return json.Number(pyFloat(x)), nil
	case kBool:
		return parseBool(v, loc)
	case kStrList:
		arr, ok := v.([]any)
		if !ok {
			return nil, fe("list_type", "Input should be a valid list", loc, v)
		}
		var errs []httpx.FieldError
		for i, e := range arr {
			if _, ok := e.(string); !ok {
				errs = append(errs, fe("string_type", "Input should be a valid string", append(append([]any{}, loc...), i), e)...)
			}
		}
		if len(errs) > 0 {
			return nil, errs
		}
		return arr, nil
	case kDict:
		if o, ok := v.(*Obj); ok {
			return o, nil
		}
		return nil, fe("dict_type", "Input should be a valid dictionary", loc, v)
	case kAny:
		return v, nil
	case kPolicy:
		o, ok := v.(*Obj)
		if !ok {
			return nil, fe("model_type", "Input should be a valid dictionary or instance of AgentColumnPolicy", loc, v)
		}
		vals, errs := validateModel(o, policyFields, loc, o)
		if len(errs) > 0 {
			return nil, errs
		}
		p := NewObj()
		for _, pf := range policyFields {
			if x, ok := vals[pf.name]; ok {
				p.Set(pf.name, x)
			}
		}
		return p, nil
	}
	return v, nil
}

func isIntText(s string) bool { return !strings.ContainsAny(s, ".eE") }

func parseInt(v any, strict bool, loc []any) (int64, []httpx.FieldError) {
	typeErr := fe("int_type", "Input should be a valid integer", loc, v)
	switch t := v.(type) {
	case json.Number:
		s := string(t)
		if isIntText(s) {
			n, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				return 0, fe("int_parsing", "Input should be a valid integer, unable to parse string as an integer", loc, v)
			}
			return n, nil
		}
		if strict {
			return 0, typeErr
		}
		f, _ := strconv.ParseFloat(s, 64)
		if f != math.Trunc(f) || math.IsInf(f, 0) {
			return 0, fe("int_from_float", "Input should be a valid integer, got a number with a fractional part", loc, v)
		}
		if math.Abs(f) >= 1<<63 {
			return 0, fe("int_parsing", "Input should be a valid integer, unable to parse string as an integer", loc, v)
		}
		return int64(f), nil
	case bool:
		if strict {
			return 0, typeErr
		}
		if t {
			return 1, nil
		}
		return 0, nil
	case string:
		if strict {
			return 0, typeErr
		}
		s := strings.TrimSpace(t)
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return n, nil
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil && f == math.Trunc(f) && math.Abs(f) < 1<<63 {
			return int64(f), nil
		}
		return 0, fe("int_parsing", "Input should be a valid integer, unable to parse string as an integer", loc, v)
	}
	return 0, typeErr
}

func parseFloat(v any, loc []any) (float64, []httpx.FieldError) {
	var f float64
	switch t := v.(type) {
	case json.Number:
		x, err := strconv.ParseFloat(string(t), 64)
		if err != nil || math.IsInf(x, 0) {
			return 0, fe("finite_number", "Input should be a finite number", loc, v)
		}
		f = x
	case bool:
		if t {
			f = 1
		}
	case string:
		x, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
				return 0, fe("finite_number", "Input should be a finite number", loc, v)
			}
			return 0, fe("float_parsing", "Input should be a valid number, unable to parse string as a number", loc, v)
		}
		if math.IsInf(x, 0) || math.IsNaN(x) {
			return 0, fe("finite_number", "Input should be a finite number", loc, v)
		}
		f = x
	default:
		return 0, fe("float_type", "Input should be a valid number", loc, v)
	}
	return f, nil
}

func parseBool(v any, loc []any) (any, []httpx.FieldError) {
	bad := fe("bool_parsing", "Input should be a valid boolean, unable to interpret input", loc, v)
	switch t := v.(type) {
	case bool:
		return t, nil
	case json.Number:
		switch f, _ := strconv.ParseFloat(string(t), 64); f {
		case 0:
			return false, nil
		case 1:
			return true, nil
		}
		return nil, bad
	case string:
		switch strings.ToLower(t) {
		case "0", "off", "f", "false", "n", "no":
			return false, nil
		case "1", "on", "t", "true", "y", "yes":
			return true, nil
		}
		return nil, bad
	}
	return nil, fe("bool_type", "Input should be a valid boolean", loc, v)
}
