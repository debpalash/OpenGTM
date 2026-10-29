package api

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadscore"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

func TestParseScoreBody(t *testing.T) {
	type want struct {
		id       *int64
		hasLead  bool
		errTypes []string // type per error, in order
		errLocs  []string
	}
	i := func(n int64) *int64 { return &n }
	for name, c := range map[string]struct {
		in string
		w  want
	}{
		"id":             {`{"lead_id":14}`, want{id: i(14)}},
		"id numeric str": {`{"lead_id":"14"}`, want{id: i(14)}},
		"id padded str":  {`{"lead_id":" 14 "}`, want{id: i(14)}},
		"id float whole": {`{"lead_id":14.0}`, want{id: i(14)}},
		"id bool":        {`{"lead_id":true}`, want{id: i(1)}},
		"id null":        {`{"lead_id":null}`, want{}},
		"id zero":        {`{"lead_id":0}`, want{id: i(0)}},
		"id negative":    {`{"lead_id":-1}`, want{id: i(-1)}},
		"id str bad":     {`{"lead_id":"x"}`, want{errTypes: []string{"int_parsing"}, errLocs: []string{"body.lead_id"}}},
		"id frac":        {`{"lead_id":1.5}`, want{errTypes: []string{"int_from_float"}, errLocs: []string{"body.lead_id"}}},
		"id list":        {`{"lead_id":[1]}`, want{errTypes: []string{"int_type"}, errLocs: []string{"body.lead_id"}}},
		"lead":           {`{"lead":{"company":"A"}}`, want{hasLead: true}},
		"lead empty":     {`{"lead":{}}`, want{hasLead: true}},
		"lead null":      {`{"lead":null}`, want{}},
		"lead str":       {`{"lead":"x"}`, want{errTypes: []string{"dict_type"}, errLocs: []string{"body.lead"}}},
		"lead list":      {`{"lead":[]}`, want{errTypes: []string{"dict_type"}, errLocs: []string{"body.lead"}}},
		"both bad":       {`{"lead_id":"x","lead":5}`, want{errTypes: []string{"int_parsing", "dict_type"}, errLocs: []string{"body.lead_id", "body.lead"}}},
		"empty":          {`{}`, want{}},
		"not object":     {`[1]`, want{errTypes: []string{"model_attributes_type"}, errLocs: []string{"body"}}},
		"no body":        {``, want{errTypes: []string{"missing"}, errLocs: []string{"body"}}},
		"bad json":       {`{`, want{errTypes: []string{"json_invalid"}, errLocs: []string{"body.0"}}},
	} {
		t.Run(name, func(t *testing.T) {
			b, errs := parseScoreBody([]byte(c.in))
			var types, locs []string
			for _, e := range errs {
				types = append(types, e.Type)
				var parts []string
				for _, l := range e.Loc {
					parts = append(parts, toStr(l))
				}
				locs = append(locs, strings.Join(parts, "."))
			}
			if !reflect.DeepEqual(types, c.w.errTypes) || !reflect.DeepEqual(locs, c.w.errLocs) {
				t.Fatalf("errors: %v %v, want %v %v", types, locs, c.w.errTypes, c.w.errLocs)
			}
			if errs != nil {
				return
			}
			if (b.LeadID == nil) != (c.w.id == nil) || (b.LeadID != nil && *b.LeadID != *c.w.id) {
				t.Errorf("lead_id %v, want %v", b.LeadID, c.w.id)
			}
			if (b.Lead != nil) != c.w.hasLead {
				t.Errorf("lead present %v, want %v", b.Lead != nil, c.w.hasLead)
			}
		})
	}
}

func toStr(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case int:
		return string(rune('0' + x))
	}
	return "?"
}

func TestParseStringField(t *testing.T) {
	for name, c := range map[string]struct {
		in, want, errType string
	}{
		"ok":       {`{"email":"a@b.co"}`, "a@b.co", ""},
		"extra":    {`{"email":"a@b.co","x":1}`, "a@b.co", ""},
		"missing":  {`{}`, "", "missing"},
		"int":      {`{"email":5}`, "", "string_type"},
		"null":     {`{"email":null}`, "", "string_type"},
		"list":     {`["a"]`, "", "model_attributes_type"},
		"empty":    {`{"email":""}`, "", ""},
		"unicode":  {`{"email":"é x"}`, "é x", ""},
		"no body":  {``, "", "missing"},
		"bad json": {`{"email":`, "", "json_invalid"},
	} {
		t.Run(name, func(t *testing.T) {
			got, errs := parseStringField([]byte(c.in), "email")
			if c.errType == "" {
				if errs != nil || got != c.want {
					t.Fatalf("%q %v", got, errs)
				}
				return
			}
			if len(errs) != 1 || errs[0].Type != c.errType {
				t.Fatalf("errs %v, want %s", errs, c.errType)
			}
		})
	}
}

func lead(t *testing.T, doc string) *wbcrud.Obj {
	t.Helper()
	o, errs := wbcrud.DecodeBodyObject([]byte(doc))
	if errs != nil {
		t.Fatal(errs)
	}
	return o
}

func TestScoreInline(t *testing.T) {
	co, n, err := scoreInline(lead(t, `{"company":"Full","website":"https://x.example","email":"a@b.co","phone":"1","linkedin_url":"l","company_size":"201-500","city":"Mumbai"}`))
	if err != nil || co != "Full" || n <= 0 {
		t.Fatalf("%v %v %v", co, n, err)
	}
	if co, n, err = scoreInline(lead(t, `{"nonsense":1}`)); err != nil || co != "" || n != 0 {
		t.Fatalf("absent company must be the dataclass default \"\": %#v %v %v", co, n, err)
	}
	// a wrong-typed company is echoed as sent (the dataclass does no coercion)
	if co, _, err = scoreInline(lead(t, `{"company":7}`)); err != nil || co == nil || co == "" {
		t.Fatalf("company %#v %v", co, err)
	}
	// Python raises TypeError (a bare 500) on email=5; Go reports it so the route answers 422
	_, _, err = scoreInline(lead(t, `{"company":"T","email":5}`))
	var pe *leadscore.PyError
	if !errors.As(err, &pe) {
		t.Fatalf("want *PyError, got %v", err)
	}
	// nested objects and arrays reach the scorer as plain values, not wbcrud types
	if _, _, err = scoreInline(lead(t, `{"company":"T","decision_makers":[{"name":"A"}],"specialization":{"a":1}}`)); err == nil {
		t.Fatal("dict specialization has no .lower(): want an error")
	}
}
