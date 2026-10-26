package api

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// sourceColumnBody is SourceColumnRequest (routers/workbooks.py), validated like
// pydantic v2 in lax mode: errors come in field order with the loc prefix "body",
// unknown keys are ignored. Dict fields keep their key order and number text.
type sourceColumnBody struct {
	Name          string
	Kind          string
	ICP           *wbcrud.Obj
	Channels      *wbcrud.Obj
	TargetRows    int64
	Companies     []string
	FromColumn    string
	Titles        []string
	Seniority     string
	Geo           string
	MaxPerCompany int64
	MaxSearches   int64
	Source        *wbcrud.Obj
}

func srcLoc(rest ...any) []any { return append([]any{"body"}, rest...) }

func srcErr(typ, msg string, loc []any, in any) httpx.FieldError {
	return httpx.FieldError{Type: typ, Loc: loc, Msg: msg, Input: in}
}

// parseSourceColumnBody validates the request body bytes.
func parseSourceColumnBody(raw []byte) (sourceColumnBody, []httpx.FieldError) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return sourceColumnBody{}, []httpx.FieldError{srcErr("missing", "Field required", srcLoc(), nil)}
	}
	o, err := wbcrud.ParseObject(raw)
	if err != nil {
		var any0 any
		if json.Unmarshal(raw, &any0) != nil {
			return sourceColumnBody{}, []httpx.FieldError{srcErr("json_invalid", "JSON decode error", srcLoc(0), map[string]any{})}
		}
		return sourceColumnBody{}, []httpx.FieldError{srcErr("model_attributes_type",
			"Input should be a valid dictionary or object to extract fields from", srcLoc(), any0)}
	}
	b := sourceColumnBody{Name: "Source", Kind: "icp", ICP: wbcrud.NewObj(), Channels: wbcrud.NewObj(),
		Companies: []string{}, Titles: []string{}, MaxPerCompany: 10, MaxSearches: 100, Source: wbcrud.NewObj()}
	var errs []httpx.FieldError
	str := func(name string, dst *string) {
		if v, ok := o.Get(name); ok {
			s, isStr := v.(string)
			if !isStr {
				errs = append(errs, srcErr("string_type", "Input should be a valid string", srcLoc(name), v))
				return
			}
			*dst = s
		}
	}
	dict := func(name string, dst **wbcrud.Obj) {
		if v, ok := o.Get(name); ok {
			d, isObj := v.(*wbcrud.Obj)
			if !isObj {
				errs = append(errs, srcErr("dict_type", "Input should be a valid dictionary", srcLoc(name), v))
				return
			}
			*dst = d
		}
	}
	integer := func(name string, dst *int64) {
		if v, ok := o.Get(name); ok {
			n, fe := srcLaxInt(v, srcLoc(name))
			if fe != nil {
				errs = append(errs, *fe)
				return
			}
			*dst = n
		}
	}
	strs := func(name string, dst *[]string) {
		v, ok := o.Get(name)
		if !ok {
			return
		}
		arr, isArr := v.([]any)
		if !isArr {
			errs = append(errs, srcErr("list_type", "Input should be a valid list", srcLoc(name), v))
			return
		}
		out := make([]string, 0, len(arr))
		bad := false
		for i, e := range arr {
			s, isStr := e.(string)
			if !isStr {
				bad = true
				errs = append(errs, srcErr("string_type", "Input should be a valid string", srcLoc(name, i), e))
				continue
			}
			out = append(out, s)
		}
		if !bad {
			*dst = out
		}
	}
	str("name", &b.Name)
	str("kind", &b.Kind)
	dict("icp", &b.ICP)
	dict("channels", &b.Channels)
	integer("target_rows", &b.TargetRows)
	strs("companies", &b.Companies)
	str("from_column", &b.FromColumn)
	strs("titles", &b.Titles)
	str("seniority", &b.Seniority)
	str("geo", &b.Geo)
	integer("max_per_company", &b.MaxPerCompany)
	integer("max_searches", &b.MaxSearches)
	dict("source", &b.Source)
	if len(errs) > 0 {
		return sourceColumnBody{}, errs
	}
	return b, nil
}

// srcLaxInt is pydantic v2 lax-mode int validation for a JSON value.
func srcLaxInt(v any, loc []any) (int64, *httpx.FieldError) {
	parseErr := srcErr("int_parsing", "Input should be a valid integer, unable to parse string as an integer", loc, v)
	switch t := v.(type) {
	case json.Number:
		s := string(t)
		if !strings.ContainsAny(s, ".eE") {
			n, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				return 0, &parseErr
			}
			return n, nil
		}
		f, _ := strconv.ParseFloat(s, 64)
		if f != math.Trunc(f) || math.IsInf(f, 0) {
			e := srcErr("int_from_float", "Input should be a valid integer, got a number with a fractional part", loc, v)
			return 0, &e
		}
		if math.Abs(f) >= 1<<63 {
			return 0, &parseErr
		}
		return int64(f), nil
	case bool:
		if t {
			return 1, nil
		}
		return 0, nil
	case string:
		s := strings.TrimSpace(t)
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return n, nil
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil && f == math.Trunc(f) && math.Abs(f) < 1<<63 {
			return int64(f), nil
		}
		return 0, &parseErr
	}
	e := srcErr("int_type", "Input should be a valid integer", loc, v)
	return 0, &e
}
