// Package runplan plans a workbook run: it validates the request, selects and
// orders the columns, resolves the exact row set once, checks cell scope and
// cycles, projects the platform cost and decides the executor and job payload.
// It is the planning half of POST /api/workbooks/{id}/run
// (workbooks.py run_workbook and go_executor.enqueue_run_workbook); debiting
// and enqueueing belong to the route.
package runplan

import (
	"bytes"
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// RunRequest is RunWorkbookRequest. A nil slice or map is Python's None
// ("everything"); a non-nil empty one is the empty list ("nothing"), which the
// two treat differently, so callers must not normalise them.
type RunRequest struct {
	ColumnIDs    []string
	RowIDs       []int64
	LeadIDs      []int64
	RowColumns   map[string][]string
	ViewID       string // "" is None
	Search       string
	ExpectedRows *int64
	FillMissing  bool
	Force        bool

	// rowIDKeys holds the decimal text of every requested row id, including
	// ones beyond int64 that can never match a stored row (RowIDs drops them).
	rowIDKeys []string
}

// RowIDKeys is {str(rid) for rid in row_ids}, sorted: the key set a cell scope
// must match. Ids parsed from JSON that exceed int64 are kept here.
func (r RunRequest) RowIDKeys() []string {
	keys := r.rowIDKeys
	if keys == nil {
		for _, id := range r.RowIDs {
			keys = append(keys, strconv.FormatInt(id, 10))
		}
	}
	out := append([]string(nil), keys...)
	sort.Strings(out)
	return out
}

const maxSearchChars = 500

func loc(rest ...any) []any { return append([]any{"body"}, rest...) }

// ParseRunRequest validates a request body like FastAPI does for
// `body: RunWorkbookRequest = None`: an empty or null body is the default
// request, anything else must be an object whose fields validate with
// pydantic's messages. Unknown fields are ignored.
func ParseRunRequest(raw []byte) (RunRequest, []httpx.FieldError) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return RunRequest{}, nil
	}
	doc, errs := wbcrud.DecodeBody(raw)
	if errs != nil {
		return RunRequest{}, errs
	}
	o, errs := wbcrud.PydModel(doc)
	if errs != nil {
		return RunRequest{}, errs
	}
	var req RunRequest
	add := func(e []httpx.FieldError) { errs = append(errs, e...) }

	if v, ok := o.Get("column_ids"); ok && v != nil {
		req.ColumnIDs, errs = appendStrList(errs, v, "column_ids")
	}
	if v, ok := o.Get("row_ids"); ok && v != nil {
		req.RowIDs, req.rowIDKeys, errs = appendIntList(errs, v, "row_ids")
	}
	if v, ok := o.Get("row_columns"); ok && v != nil {
		req.RowColumns, errs = appendScope(errs, v)
	}
	if v, ok := o.Get("lead_ids"); ok && v != nil {
		req.LeadIDs, _, errs = appendIntList(errs, v, "lead_ids")
	}
	if v, ok := o.Get("view_id"); ok && v != nil {
		if s, isStr := v.(string); isStr {
			req.ViewID = s
		} else {
			add(wbcrud.PydError("string_type", "Input should be a valid string", loc("view_id"), v))
		}
	}
	if v, ok := o.Get("search"); ok && v != nil {
		switch s := v.(type) {
		case string:
			if utf8.RuneCountInString(s) > maxSearchChars {
				add([]httpx.FieldError{{Type: "string_too_long", Loc: loc("search"), Msg: "String should have at most 500 characters",
					Input: s, Ctx: map[string]any{"max_length": maxSearchChars}}})
			} else {
				req.Search = s
			}
		default:
			add(wbcrud.PydError("string_type", "Input should be a valid string", loc("search"), v))
		}
	}
	if v, ok := o.Get("expected_rows"); ok && v != nil {
		n, fits, e := wbcrud.PydInt(v, loc("expected_rows"))
		negative := false
		if num, isNum := v.(json.Number); isNum && !fits {
			negative = strings.HasPrefix(string(num), "-")
		}
		switch {
		case e != nil:
			add(e)
		case n < 0 || negative:
			add([]httpx.FieldError{{Type: "greater_than_equal", Loc: loc("expected_rows"), Msg: "Input should be greater than or equal to 0",
				Input: v, Ctx: map[string]any{"ge": 0}}})
		default:
			if !fits {
				n = math.MaxInt64
			}
			req.ExpectedRows = &n
		}
	}
	for _, f := range []struct {
		name string
		dst  *bool
	}{{"fill_missing", &req.FillMissing}, {"force", &req.Force}} {
		if v, ok := o.Get(f.name); ok {
			b, e := wbcrud.PydBool(v, loc(f.name))
			if e != nil {
				add(e)
			} else {
				*f.dst = b
			}
		}
	}
	if errs != nil {
		return RunRequest{}, errs
	}
	return req, nil
}

func appendStrList(errs []httpx.FieldError, v any, name string) ([]string, []httpx.FieldError) {
	list, ok := v.([]any)
	if !ok {
		return nil, append(errs, wbcrud.PydError("list_type", "Input should be a valid list", loc(name), v)...)
	}
	out := make([]string, 0, len(list))
	for i, e := range list {
		s, isStr := e.(string)
		if !isStr {
			errs = append(errs, wbcrud.PydError("string_type", "Input should be a valid string", loc(name, i), e)...)
			continue
		}
		out = append(out, s)
	}
	return out, errs
}

func appendIntList(errs []httpx.FieldError, v any, name string) (ids []int64, keys []string, _ []httpx.FieldError) {
	list, ok := v.([]any)
	if !ok {
		return nil, nil, append(errs, wbcrud.PydError("list_type", "Input should be a valid list", loc(name), v)...)
	}
	ids, keys = make([]int64, 0, len(list)), make([]string, 0, len(list))
	for i, e := range list {
		n, fits, e2 := wbcrud.PydInt(e, loc(name, i))
		if e2 != nil {
			errs = append(errs, e2...)
			continue
		}
		if fits {
			ids = append(ids, n)
			keys = append(keys, strconv.FormatInt(n, 10))
		} else if num, isNum := e.(json.Number); isNum {
			keys = append(keys, string(num))
		}
	}
	return ids, keys, errs
}

func appendScope(errs []httpx.FieldError, v any) (map[string][]string, []httpx.FieldError) {
	o, ok := v.(*wbcrud.Obj)
	if !ok {
		return nil, append(errs, wbcrud.PydError("dict_type", "Input should be a valid dictionary", loc("row_columns"), v)...)
	}
	out := make(map[string][]string, o.Len())
	for _, key := range o.Keys() {
		val, _ := o.Get(key)
		list, isList := val.([]any)
		if !isList {
			errs = append(errs, wbcrud.PydError("list_type", "Input should be a valid list", loc("row_columns", key), val)...)
			continue
		}
		ids := make([]string, 0, len(list))
		for i, e := range list {
			s, isStr := e.(string)
			if !isStr {
				errs = append(errs, wbcrud.PydError("string_type", "Input should be a valid string", loc("row_columns", key, i), e)...)
				continue
			}
			ids = append(ids, s)
		}
		out[key] = ids
	}
	return out, errs
}
