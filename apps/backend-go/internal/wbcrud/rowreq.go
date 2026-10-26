package wbcrud

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

// Request parsing for the row routes: pydantic's messages, locs and ordering
// (path, query, then body errors are concatenated by the route).

// DecodeBody decodes a request body into an ordered JSON tree. An empty body is
// pydantic's missing-body error; undecodable text is json_invalid. Numbers with
// a fraction or exponent are rewritten to Python's float repr.
func DecodeBody(raw []byte) (any, []httpx.FieldError) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fe("missing", "Field required", []any{"body"}, nil)
	}
	doc, err := decodeDocument(raw, true)
	if err != nil {
		return nil, fe("json_invalid", "JSON decode error", []any{"body", 0}, map[string]any{})
	}
	return doc, nil
}

const modelAttrMsg = "Input should be a valid dictionary or object to extract fields from"

func bodyModel(doc any) (*Obj, []httpx.FieldError) {
	o, ok := doc.(*Obj)
	if !ok {
		return nil, fe("model_attributes_type", modelAttrMsg, []any{"body"}, doc)
	}
	return o, nil
}

func missing(o *Obj, loc ...any) []httpx.FieldError {
	return fe("missing", "Field required", loc, o)
}

// ParseDictBody validates a `body: dict` parameter.
func ParseDictBody(doc any) (*Obj, []httpx.FieldError) {
	o, ok := doc.(*Obj)
	if !ok {
		return nil, fe("dict_type", "Input should be a valid dictionary", []any{"body"}, doc)
	}
	return o, nil
}

// AddRowsRequest is AddRowsRequest (rows min 1, dedupe default true).
type AddRowsRequest struct {
	Rows   []*Obj
	Dedupe bool
}

func tooShort(kind string, in any) []httpx.FieldError {
	return []httpx.FieldError{{Type: "too_short", Msg: kind + " should have at least 1 item after validation, not 0", Input: in,
		Ctx: map[string]any{"field_type": kind, "min_length": 1, "actual_length": 0}}}
}

func ParseAddRows(doc any) (AddRowsRequest, []httpx.FieldError) {
	o, errs := bodyModel(doc)
	if errs != nil {
		return AddRowsRequest{}, errs
	}
	req := AddRowsRequest{Dedupe: true}
	rv, present := o.Get("rows")
	switch {
	case !present:
		errs = append(errs, missing(o, "body", "rows")...)
	default:
		list, isList := rv.([]any)
		if !isList {
			errs = append(errs, fe("list_type", "Input should be a valid list", []any{"body", "rows"}, rv)...)
			break
		}
		n := len(errs)
		for i, el := range list {
			if row, ok := el.(*Obj); ok {
				req.Rows = append(req.Rows, row)
			} else {
				errs = append(errs, fe("dict_type", "Input should be a valid dictionary", []any{"body", "rows", i}, el)...)
			}
		}
		if len(list) == 0 && len(errs) == n {
			e := tooShort("List", list)
			e[0].Loc = []any{"body", "rows"}
			errs = append(errs, e...)
		}
	}
	if dv, present := o.Get("dedupe"); present {
		b, e := parseBool(dv, []any{"body", "dedupe"})
		if e != nil {
			errs = append(errs, e...)
		} else {
			req.Dedupe = b.(bool)
		}
	}
	if errs != nil {
		return AddRowsRequest{}, errs
	}
	return req, nil
}

// DeleteRowsRequest is DeleteRowsRequest. Requested counts every id (the audit
// log's requested_count); IDs holds those that fit an int64 (larger ids cannot
// match a row).
type DeleteRowsRequest struct {
	IDs       []int64
	Requested int
}

// lenientInt is pydantic's lax int, accepting integers of any size (reported
// as fits=false when they overflow int64).
func lenientInt(v any, loc []any) (n int64, fits bool, errs []httpx.FieldError) {
	if num, ok := v.(json.Number); ok {
		s := string(num)
		if isIntText(s) {
			if _, err := strconv.ParseInt(s, 10, 64); err != nil {
				if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
					return 0, false, nil
				}
			}
		}
	}
	n, errs = parseInt(v, false, loc)
	return n, errs == nil, errs
}

func ParseDeleteRows(doc any) (DeleteRowsRequest, []httpx.FieldError) {
	o, errs := bodyModel(doc)
	if errs != nil {
		return DeleteRowsRequest{}, errs
	}
	var req DeleteRowsRequest
	rv, present := o.Get("row_ids")
	if !present {
		return req, missing(o, "body", "row_ids")
	}
	list, isList := rv.([]any)
	if !isList {
		return req, fe("list_type", "Input should be a valid list", []any{"body", "row_ids"}, rv)
	}
	for i, el := range list {
		n, fits, e := lenientInt(el, []any{"body", "row_ids", i})
		if e != nil {
			errs = append(errs, e...)
			continue
		}
		req.Requested++
		if fits {
			req.IDs = append(req.IDs, n)
		}
	}
	if len(list) == 0 {
		e := tooShort("List", list)
		e[0].Loc = []any{"body", "row_ids"}
		errs = append(errs, e...)
	}
	if errs != nil {
		return DeleteRowsRequest{}, errs
	}
	return req, nil
}

// DeleteMatchingRequest is DeleteMatchingRowsRequest.
type DeleteMatchingRequest struct {
	ExpectedCount int64
	Confirmation  string
	ViewID        *string
	Search        *string
}

func strMax(v any, loc []any, max int) (string, []httpx.FieldError) {
	s, ok := v.(string)
	if !ok {
		return "", fe("string_type", "Input should be a valid string", loc, v)
	}
	if utf8.RuneCountInString(s) > max {
		return "", []httpx.FieldError{{Type: "string_too_long", Loc: loc, Input: v,
			Msg: fmt.Sprintf("String should have at most %d characters", max), Ctx: map[string]any{"max_length": max}}}
	}
	return s, nil
}

func ParseDeleteMatching(doc any) (DeleteMatchingRequest, []httpx.FieldError) {
	o, errs := bodyModel(doc)
	if errs != nil {
		return DeleteMatchingRequest{}, errs
	}
	var req DeleteMatchingRequest
	if v, present := o.Get("expected_count"); !present {
		errs = append(errs, missing(o, "body", "expected_count")...)
	} else if n, e := parseInt(v, false, []any{"body", "expected_count"}); e != nil {
		errs = append(errs, e...)
	} else if n < 1 {
		errs = append(errs, httpx.FieldError{Type: "greater_than_equal", Loc: []any{"body", "expected_count"}, Input: v,
			Msg: "Input should be greater than or equal to 1", Ctx: map[string]any{"ge": 1}})
	} else {
		req.ExpectedCount = n
	}
	if v, present := o.Get("confirmation"); !present {
		errs = append(errs, missing(o, "body", "confirmation")...)
	} else if s, e := strMax(v, []any{"body", "confirmation"}, 64); e != nil {
		errs = append(errs, e...)
	} else {
		req.Confirmation = s
	}
	if v, _ := o.Get("view_id"); v != nil {
		if s, ok := v.(string); ok {
			req.ViewID = &s
		} else {
			errs = append(errs, fe("string_type", "Input should be a valid string", []any{"body", "view_id"}, v)...)
		}
	}
	if v, _ := o.Get("search"); v != nil {
		if s, e := strMax(v, []any{"body", "search"}, 500); e != nil {
			errs = append(errs, e...)
		} else {
			req.Search = &s
		}
	}
	if errs != nil {
		return DeleteMatchingRequest{}, errs
	}
	return req, nil
}

// RowUpdate is WorkbookRowUpdate.
type RowUpdate struct {
	RowID  int64
	Fields *Obj
}

// BulkUpdateRequest is BulkUpdateRowsRequest (updates 1..1000).
type BulkUpdateRequest struct{ Updates []RowUpdate }

// MaxBulkUpdates bounds PATCH rows.
const MaxBulkUpdates = 1000

func ParseBulkUpdate(doc any) (BulkUpdateRequest, []httpx.FieldError) {
	o, errs := bodyModel(doc)
	if errs != nil {
		return BulkUpdateRequest{}, errs
	}
	uv, present := o.Get("updates")
	if !present {
		return BulkUpdateRequest{}, missing(o, "body", "updates")
	}
	list, isList := uv.([]any)
	if !isList {
		return BulkUpdateRequest{}, fe("list_type", "Input should be a valid list", []any{"body", "updates"}, uv)
	}
	if len(list) > MaxBulkUpdates {
		return BulkUpdateRequest{}, []httpx.FieldError{{Type: "too_long", Loc: []any{"body", "updates"}, Input: list,
			Msg: fmt.Sprintf("List should have at most %d items after validation, not %d", MaxBulkUpdates, len(list)),
			Ctx: map[string]any{"field_type": "List", "max_length": MaxBulkUpdates, "actual_length": len(list)}}}
	}
	var req BulkUpdateRequest
	for i, el := range list {
		item, ok := el.(*Obj)
		if !ok {
			errs = append(errs, fe("model_attributes_type", modelAttrMsg, []any{"body", "updates", i}, el)...)
			continue
		}
		upd := RowUpdate{}
		bad := false
		if v, present := item.Get("row_id"); !present {
			errs = append(errs, missing(item, "body", "updates", i, "row_id")...)
			bad = true
		} else if n, e := parseInt(v, false, []any{"body", "updates", i, "row_id"}); e != nil {
			errs = append(errs, e...)
			bad = true
		} else {
			upd.RowID = n
		}
		if v, present := item.Get("fields"); !present {
			errs = append(errs, missing(item, "body", "updates", i, "fields")...)
			bad = true
		} else if f, ok := v.(*Obj); !ok {
			errs = append(errs, fe("dict_type", "Input should be a valid dictionary", []any{"body", "updates", i, "fields"}, v)...)
			bad = true
		} else if f.Len() == 0 {
			errs = append(errs, httpx.FieldError{Type: "too_short", Loc: []any{"body", "updates", i, "fields"}, Input: v,
				Msg: "Dictionary should have at least 1 item after validation, not 0",
				Ctx: map[string]any{"field_type": "Dictionary", "min_length": 1, "actual_length": 0}})
			bad = true
		} else {
			upd.Fields = f
		}
		if !bad {
			req.Updates = append(req.Updates, upd)
		}
	}
	if len(list) == 0 {
		e := tooShort("List", list)
		e[0].Loc = []any{"body", "updates"}
		errs = append(errs, e...)
	}
	if errs != nil {
		return BulkUpdateRequest{}, errs
	}
	return req, nil
}

// RecomputeParam reads `recompute: bool = Query(True)`; the last repeated value wins.
func RecomputeParam(q url.Values) (bool, *httpx.FieldError) {
	vals, ok := q["recompute"]
	if !ok || len(vals) == 0 {
		return true, nil
	}
	v := vals[len(vals)-1]
	switch strings.ToLower(v) {
	case "1", "true", "t", "yes", "y", "on":
		return true, nil
	case "0", "false", "f", "no", "n", "off":
		return false, nil
	}
	return false, &httpx.FieldError{Type: "bool_parsing", Loc: []any{"query", "recompute"}, Input: v,
		Msg: "Input should be a valid boolean, unable to interpret input"}
}

// PathInt reads an int path parameter. Integers beyond int64 read as -1 (no
// such row), a non-integer is the int_parsing error.
func PathInt(raw, name string) (int64, *httpx.FieldError) {
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
			return -1, nil
		}
		return 0, &httpx.FieldError{Type: "int_parsing", Loc: []any{"path", name}, Input: raw,
			Msg: "Input should be a valid integer, unable to parse string as an integer"}
	}
	return n, nil
}
