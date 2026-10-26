package runplan

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func fieldErrJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseRunRequestDefaults(t *testing.T) {
	for _, body := range []string{"", "  ", "null", "{}"} {
		req, errs := ParseRunRequest([]byte(body))
		if errs != nil {
			t.Fatalf("body %q: %v", body, errs)
		}
		if req.ColumnIDs != nil || req.RowIDs != nil || req.LeadIDs != nil || req.RowColumns != nil ||
			req.ViewID != "" || req.Search != "" || req.ExpectedRows != nil || req.FillMissing || req.Force {
			t.Fatalf("body %q: not the all-None default: %+v", body, req)
		}
	}
}

func TestParseRunRequestFull(t *testing.T) {
	req, errs := ParseRunRequest([]byte(`{"column_ids":["a","b"],"row_ids":[1,"2",3.0],"row_columns":{"1":["a"],"2":[]},
		"lead_ids":[],"view_id":"v","search":"x","expected_rows":4,"fill_missing":"true","force":1,"extra":5}`))
	if errs != nil {
		t.Fatal(errs)
	}
	if !reflect.DeepEqual(req.ColumnIDs, []string{"a", "b"}) || !reflect.DeepEqual(req.RowIDs, []int64{1, 2, 3}) {
		t.Fatalf("%+v", req)
	}
	if req.LeadIDs == nil || len(req.LeadIDs) != 0 {
		t.Fatalf("empty lead_ids must stay non-nil: %#v", req.LeadIDs)
	}
	if !reflect.DeepEqual(req.RowColumns, map[string][]string{"1": {"a"}, "2": {}}) {
		t.Fatalf("%#v", req.RowColumns)
	}
	if req.ViewID != "v" || req.Search != "x" || req.ExpectedRows == nil || *req.ExpectedRows != 4 || !req.FillMissing || !req.Force {
		t.Fatalf("%+v", req)
	}
}

func TestParseRunRequestErrors(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"negative expected_rows", `{"expected_rows":-1}`,
			`[{"type":"greater_than_equal","loc":["body","expected_rows"],"msg":"Input should be greater than or equal to 0","input":-1,"ctx":{"ge":0}}]`},
		{"column_ids not list", `{"column_ids":"a"}`,
			`[{"type":"list_type","loc":["body","column_ids"],"msg":"Input should be a valid list","input":"a"}]`},
		{"column id not string", `{"column_ids":["a",1]}`,
			`[{"type":"string_type","loc":["body","column_ids",1],"msg":"Input should be a valid string","input":1}]`},
		{"row_ids bad", `{"row_ids":["x"]}`,
			`[{"type":"int_parsing","loc":["body","row_ids",0],"msg":"Input should be a valid integer, unable to parse string as an integer","input":"x"}]`},
		{"row_columns not dict", `{"row_columns":[1]}`,
			`[{"type":"dict_type","loc":["body","row_columns"],"msg":"Input should be a valid dictionary","input":[1]}]`},
		{"row_columns value not list", `{"row_columns":{"1":"a"}}`,
			`[{"type":"list_type","loc":["body","row_columns","1"],"msg":"Input should be a valid list","input":"a"}]`},
		{"row_columns element", `{"row_columns":{"1":[2]}}`,
			`[{"type":"string_type","loc":["body","row_columns","1",0],"msg":"Input should be a valid string","input":2}]`},
		{"view_id not string", `{"view_id":5}`,
			`[{"type":"string_type","loc":["body","view_id"],"msg":"Input should be a valid string","input":5}]`},
		{"search too long", `{"search":"` + strings.Repeat("é", 501) + `"}`,
			`[{"type":"string_too_long","loc":["body","search"],"msg":"String should have at most 500 characters","input":"` + strings.Repeat("é", 501) + `","ctx":{"max_length":500}}]`},
		{"bool bad", `{"force":"maybe"}`,
			`[{"type":"bool_parsing","loc":["body","force"],"msg":"Input should be a valid boolean, unable to interpret input","input":"maybe"}]`},
		{"not an object", `[1]`,
			`[{"type":"model_attributes_type","loc":["body"],"msg":"Input should be a valid dictionary or object to extract fields from","input":[1]}]`},
		{"json invalid", `{`, `[{"type":"json_invalid","loc":["body",1],"msg":"JSON decode error","input":{},"ctx":{"error":"Expecting property name enclosed in double quotes"}}]`},
	}
	for _, c := range cases {
		_, errs := ParseRunRequest([]byte(c.body))
		if errs == nil {
			t.Fatalf("%s: no errors", c.name)
		}
		if c.name == "json invalid" {
			if errs[0].Type != "json_invalid" {
				t.Fatalf("%s: %s", c.name, fieldErrJSON(t, errs))
			}
			continue
		}
		if got := fieldErrJSON(t, errs); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
	}
}

func TestParseRunRequestHugeRowID(t *testing.T) {
	req, errs := ParseRunRequest([]byte(`{"row_ids":[7,99999999999999999999999],"row_columns":{"99999999999999999999999":["a"],"7":["a"]}}`))
	if errs != nil {
		t.Fatal(errs)
	}
	if !reflect.DeepEqual(req.RowIDs, []int64{7}) {
		t.Fatalf("ids that cannot match a row are dropped: %v", req.RowIDs)
	}
	want := []string{"7", "99999999999999999999999"}
	if !reflect.DeepEqual(req.RowIDKeys(), want) {
		t.Fatalf("keys %v", req.RowIDKeys())
	}
}
