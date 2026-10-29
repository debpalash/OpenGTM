package wbcrud

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

func validationResp(errs []httpx.FieldError) contract.Resp {
	b, _ := json.Marshal(map[string]any{"detail": errs})
	return contract.Resp{Status: 422, Headers: map[string]string{"content-type": "application/json"}, Body: b}
}

// replay422 feeds a recorded request body through parse and compares the
// validation errors with the FastAPI fixture.
func replay422(t *testing.T, name string, parse func([]byte) []httpx.FieldError) {
	t.Helper()
	fx := contract.Load(t, "workbooks/"+name)
	errs := parse(fx.Request.Body)
	if errs == nil {
		t.Fatalf("%s: expected validation errors", name)
	}
	contract.AssertEqual(t, validationResp(errs), fx.Response)
}

func parseCreate(b []byte) []httpx.FieldError { _, e := ParseCreateBody(b); return e }

func TestCreateValidationMatchesFixtures(t *testing.T) {
	for _, n := range []string{"create_max_rows_zero", "create_max_rows_big", "create_name_empty", "create_name_long", "create_missing_name", "create_bad_types"} {
		t.Run(n, func(t *testing.T) { replay422(t, n, parseCreate) })
	}
}

func TestCreateMaxRowsOutOfRange422(t *testing.T) {
	for _, body := range []string{`{"name":"x","max_rows":0}`, `{"name":"x","max_rows":50001}`, `{"name":"x","max_rows":-3}`} {
		if _, errs := ParseCreateBody([]byte(body)); len(errs) != 1 || errs[0].Loc[1] != "max_rows" {
			t.Errorf("%s: %+v", body, errs)
		}
	}
	for _, body := range []string{`{"name":"x","max_rows":1}`, `{"name":"x","max_rows":50000}`, `{"name":"x","max_rows":"7"}`, `{"name":"x","max_rows":5.0}`} {
		if _, errs := ParseCreateBody([]byte(body)); errs != nil {
			t.Errorf("%s: %+v", body, errs)
		}
	}
}

func TestParseCreateBodyDefaultsAndNormalisation(t *testing.T) {
	b, errs := ParseCreateBody([]byte(`{"name":"x"}`))
	if errs != nil || b.MaxRows != 1000 || b.Source != "empty" || b.Description != "" || b.SourceConfig != nil || b.FilterCriteria != nil || len(b.Columns) != 0 {
		t.Fatalf("%+v %+v", b, errs)
	}
	fx := contract.Load(t, "workbooks/create_with_columns")
	b, errs = ParseCreateBody(fx.Request.Body)
	if errs != nil {
		t.Fatal(errs)
	}
	got, _ := json.Marshal(b.Columns)
	want := `[{"id":"company","name":"Company","type":"lead_field","width":200,"lead_field":"company"},` +
		`{"id":"mail","name":"Mail","type":"waterfall","width":250,"waterfall":["hunter"],"verify":true},` +
		`{"id":"sum","name":"Sum","type":"ai_formula","width":200,"prompt":"About {Company}","input_columns":["company"],"policy":{"max_steps":3}}]`
	if string(got) != want {
		t.Fatalf("columns:\n got %s\nwant %s", got, want)
	}
}

func TestParseCreateBodyFilterCriteriaOrderAndSourceConfig(t *testing.T) {
	b, errs := ParseCreateBody([]byte(`{"name":"x","filter_criteria":{"search":"s","bogus":1,"city":"Oslo","has_email":true,"min_score":"5"},"source_config":{"z":1,"a":{"y":1.50,"x":2}}}`))
	if errs != nil {
		t.Fatal(errs)
	}
	fc, _ := json.Marshal(b.FilterCriteria)
	if string(fc) != `{"city":"Oslo","has_email":true,"min_score":5,"search":"s"}` {
		t.Fatalf("filter order: %s", fc)
	}
	sc, _ := json.Marshal(b.SourceConfig)
	if string(sc) != `{"z":1,"a":{"y":1.5,"x":2}}` {
		t.Fatalf("source_config order/canonical floats: %s", sc)
	}
}

func TestParseCreateBodyEnvelope(t *testing.T) {
	for body, typ := range map[string]string{"": "missing", "{": "json_invalid", `[1]`: "model_attributes_type"} {
		if _, errs := ParseCreateBody([]byte(body)); len(errs) != 1 || errs[0].Type != typ {
			t.Errorf("%q: %+v", body, errs)
		}
	}
}

func TestUpdateValidationMatchesFixture(t *testing.T) {
	replay422(t, "update_bad_body", func(b []byte) []httpx.FieldError { _, e := ParseUpdateBody(b); return e })
}

func TestParseUpdateBodyNullsAndSyncToLeads(t *testing.T) {
	b, errs := ParseUpdateBody([]byte(`{"name":null,"columns_config":null,"filter_criteria":null,"sync_to_leads":"yes","status":"paused"}`))
	if errs != nil || b.Name != nil || b.Columns != nil || b.FilterCriteria != nil || b.Status == nil || *b.Status != "paused" {
		t.Fatalf("%+v %+v", b, errs)
	}
	if _, errs := ParseUpdateBody([]byte(`{"sync_to_leads":"maybe"}`)); len(errs) != 1 || errs[0].Loc[1] != "sync_to_leads" {
		t.Fatalf("%+v", errs)
	}
	if b, _ := ParseUpdateBody([]byte(`{"filter_criteria":{}}`)); b.FilterCriteria == nil || b.FilterCriteria.Len() != 0 {
		t.Fatalf("an empty filter_criteria object must still be assigned: %+v", b)
	}
}

func TestFromJobsValidationMatchesFixtures(t *testing.T) {
	for _, n := range []string{"from_jobs_empty", "from_jobs_missing"} {
		t.Run(n, func(t *testing.T) {
			replay422(t, n, func(b []byte) []httpx.FieldError { _, e := ParseFromJobsBody(b); return e })
		})
	}
	if _, errs := ParseFromJobsBody([]byte(`{"job_ids":["a",2]}`)); len(errs) != 1 || errs[0].Loc[2] != 1 {
		t.Fatalf("%+v", errs)
	}
	long := `{"job_ids":["a"],"name":"` + strings.Repeat("n", 256) + `"}`
	if _, errs := ParseFromJobsBody([]byte(long)); len(errs) != 1 || errs[0].Type != "string_too_long" {
		t.Fatalf("over-long name must be a 422, not a database 500: %+v", errs)
	}
}
