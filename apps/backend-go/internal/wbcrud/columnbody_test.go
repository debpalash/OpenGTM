package wbcrud

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

func errsJSON(t *testing.T, errs []httpx.FieldError) string {
	t.Helper()
	b, err := json.Marshal(errs)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseSettingsBody(t *testing.T) {
	b, errs := ParseSettingsBody([]byte(`{"changes":{"name":"X","icp":{"z":1.50}},"expected":{"name":"Y","icp":null}}`))
	if len(errs) != 0 {
		t.Fatal(errsJSON(t, errs))
	}
	if !reflect.DeepEqual(b.Changes.Keys(), []string{"name", "icp"}) || !reflect.DeepEqual(b.Expected.Keys(), []string{"name", "icp"}) {
		t.Fatalf("key order lost: %v %v", b.Changes.Keys(), b.Expected.Keys())
	}
	if got, _ := json.Marshal(b.Changes); string(got) != `{"name":"X","icp":{"z":1.5}}` {
		t.Fatalf("numbers not canonical: %s", got)
	}
	bad := map[string]string{
		`{"changes":{"name":"X"}}`:              `[{"type":"missing","loc":["body","expected"],"msg":"Field required","input":{"changes":{"name":"X"}}}]`,
		`{"changes":[1],"expected":"x"}`:        `[{"type":"dict_type","loc":["body","changes"],"msg":"Input should be a valid dictionary","input":[1]},{"type":"dict_type","loc":["body","expected"],"msg":"Input should be a valid dictionary","input":"x"}]`,
		`{"changes":{},"expected":{},"more":1}`: `[{"type":"extra_forbidden","loc":["body","more"],"msg":"Extra inputs are not permitted","input":1}]`,
		``:                                      `[{"type":"missing","loc":["body"],"msg":"Field required","input":null}]`,
		`[1]`:                                   `[{"type":"model_attributes_type","loc":["body"],"msg":"Input should be a valid dictionary or object to extract fields from","input":[1]}]`,
		`{"changes":{},"expected":{},"a":1,"b":2}`: `[{"type":"extra_forbidden","loc":["body","a"],"msg":"Extra inputs are not permitted","input":1},{"type":"extra_forbidden","loc":["body","b"],"msg":"Extra inputs are not permitted","input":2}]`,
		`{"changes":{},"expected":{`:               `[{"type":"json_invalid","loc":["body",0],"msg":"JSON decode error","input":{}}]`,
		`{"changes":null,"expected":{}}`:           `[{"type":"dict_type","loc":["body","changes"],"msg":"Input should be a valid dictionary","input":null}]`,
	}
	for in, want := range bad {
		if _, errs := ParseSettingsBody([]byte(in)); errsJSON(t, errs) != want {
			t.Errorf("%q:\n got %s\nwant %s", in, errsJSON(t, errs), want)
		}
	}
}

func TestParseOrderBody(t *testing.T) {
	b, errs := ParseOrderBody([]byte(`{"column_ids":["a","b"],"expected_column_ids":["b","a"]}`))
	if len(errs) != 0 || !reflect.DeepEqual(b.ColumnIDs, []string{"a", "b"}) || !reflect.DeepEqual(b.Expected, []string{"b", "a"}) {
		t.Fatalf("%v %v", b, errs)
	}
	bad := map[string]string{
		`{}`: `[{"type":"missing","loc":["body","column_ids"],"msg":"Field required","input":{}},{"type":"missing","loc":["body","expected_column_ids"],"msg":"Field required","input":{}}]`,
		`{"column_ids":"a","expected_column_ids":[1]}`:     `[{"type":"list_type","loc":["body","column_ids"],"msg":"Input should be a valid list","input":"a"},{"type":"string_type","loc":["body","expected_column_ids",0],"msg":"Input should be a valid string","input":1}]`,
		`{"column_ids":[],"expected_column_ids":[],"x":1}`: `[{"type":"extra_forbidden","loc":["body","x"],"msg":"Extra inputs are not permitted","input":1}]`,
	}
	for in, want := range bad {
		if _, errs := ParseOrderBody([]byte(in)); errsJSON(t, errs) != want {
			t.Errorf("%q:\n got %s\nwant %s", in, errsJSON(t, errs), want)
		}
	}
}

func TestParseWidthBody(t *testing.T) {
	for in, want := range map[string]int64{`{"width":80}`: 80, `{"width":600}`: 600, `{"width":250,"x":1}`: 250} {
		if got, errs := ParseWidthBody([]byte(in)); len(errs) != 0 || got != want {
			t.Errorf("%s: %d %v", in, got, errs)
		}
	}
	bad := map[string]string{
		`{"width":79}`:                   `[{"type":"greater_than_equal","loc":["body","width"],"msg":"Input should be greater than or equal to 80","input":79,"ctx":{"ge":80}}]`,
		`{"width":601}`:                  `[{"type":"less_than_equal","loc":["body","width"],"msg":"Input should be less than or equal to 600","input":601,"ctx":{"le":600}}]`,
		`{"width":200.0}`:                `[{"type":"int_type","loc":["body","width"],"msg":"Input should be a valid integer","input":200.0}]`,
		`{"width":"300"}`:                `[{"type":"int_type","loc":["body","width"],"msg":"Input should be a valid integer","input":"300"}]`,
		`{"width":true}`:                 `[{"type":"int_type","loc":["body","width"],"msg":"Input should be a valid integer","input":true}]`,
		`{"width":null}`:                 `[{"type":"int_type","loc":["body","width"],"msg":"Input should be a valid integer","input":null}]`,
		`{}`:                             `[{"type":"missing","loc":["body","width"],"msg":"Field required","input":{}}]`,
		`{"width":99999999999999999999}`: `[{"type":"less_than_equal","loc":["body","width"],"msg":"Input should be less than or equal to 600","input":99999999999999999999,"ctx":{"le":600}}]`,
	}
	for in, want := range bad {
		if _, errs := ParseWidthBody([]byte(in)); errsJSON(t, errs) != want {
			t.Errorf("%q:\n got %s\nwant %s", in, errsJSON(t, errs), want)
		}
	}
}

func TestParseAddBodyNormalises(t *testing.T) {
	col, errs := ParseAddBody([]byte(`{"column":{"name":"N","id":"n","zzz":1,"cell_budget_usd":3,"icp":null,"policy":{"prefer":"fast","x":1}},"ignored":true}`))
	if len(errs) != 0 {
		t.Fatal(errsJSON(t, errs))
	}
	b, _ := json.Marshal(col)
	if string(b) != `{"id":"n","name":"N","type":"lead_field","width":200,"cell_budget_usd":3.0,"policy":{"prefer":"fast"}}` {
		t.Fatalf("%s", b)
	}
	bad := map[string]string{
		`{}`:                  `[{"type":"missing","loc":["body","column"],"msg":"Field required","input":{}}]`,
		`{"column":null}`:     `[{"type":"model_attributes_type","loc":["body","column"],"msg":"Input should be a valid dictionary or object to extract fields from","input":null}]`,
		`{"column":{"id":1}}`: `[{"type":"string_type","loc":["body","column","id"],"msg":"Input should be a valid string","input":1},{"type":"missing","loc":["body","column","name"],"msg":"Field required","input":{"id":1}}]`,
	}
	for in, want := range bad {
		if _, errs := ParseAddBody([]byte(in)); errsJSON(t, errs) != want {
			t.Errorf("%q:\n got %s\nwant %s", in, errsJSON(t, errs), want)
		}
	}
}

func TestParseDeleteBody(t *testing.T) {
	for _, in := range []string{``, `  `, `null`} {
		o, errs := ParseDeleteBody([]byte(in))
		if o != nil || len(errs) != 0 {
			t.Errorf("%q: %v %v", in, o, errs)
		}
	}
	o, errs := ParseDeleteBody([]byte(`{"expected_column":{"id":"a","w":1.0}}`))
	if len(errs) != 0 || o == nil {
		t.Fatal(errs)
	}
	if b, _ := json.Marshal(o); string(b) != `{"id":"a","w":1.0}` {
		t.Fatal(string(b))
	}
	bad := map[string]string{
		`{}`:                           `[{"type":"missing","loc":["body","expected_column"],"msg":"Field required","input":{}}]`,
		`{"expected_column":{},"x":1}`: `[{"type":"extra_forbidden","loc":["body","x"],"msg":"Extra inputs are not permitted","input":1}]`,
		`{"expected_column":[1]}`:      `[{"type":"dict_type","loc":["body","expected_column"],"msg":"Input should be a valid dictionary","input":[1]}]`,
		`[1]`:                          `[{"type":"model_attributes_type","loc":["body"],"msg":"Input should be a valid dictionary or object to extract fields from","input":[1]}]`,
	}
	for in, want := range bad {
		if _, errs := ParseDeleteBody([]byte(in)); errsJSON(t, errs) != want {
			t.Errorf("%q:\n got %s\nwant %s", in, errsJSON(t, errs), want)
		}
	}
}
