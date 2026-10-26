package wbcrud

import (
	"encoding/json"
	"net/url"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

// detailOf renders errs as FastAPI's 422 body.
func detailOf(errs []httpx.FieldError) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"detail": errs})
	return b
}

func assertValidation(t *testing.T, name string, errs []httpx.FieldError) {
	t.Helper()
	fx := contract.Load(t, "rows/"+name)
	if len(errs) == 0 {
		t.Fatalf("%s: no validation errors, FastAPI answered %s", name, fx.Response.Body)
	}
	contract.AssertEqual(t, contract.Resp{Status: 422, Headers: fx.Response.Headers, Body: detailOf(errs)}, fx.Response)
}

func bodyOf(t *testing.T, name string) []byte {
	t.Helper()
	fx := contract.Load(t, "rows/"+name)
	if string(fx.Request.Body) == "null" || len(fx.Request.Body) == 0 {
		return nil
	}
	return fx.Request.Body
}

func decode(t *testing.T, name string) (any, []httpx.FieldError) {
	t.Helper()
	return DecodeBody(bodyOf(t, name))
}

func TestAddRowsRequestValidationMatchesFixtures(t *testing.T) {
	for _, name := range []string{"add_empty_rows", "add_missing_rows", "add_rows_not_dicts", "add_dedupe_not_bool", "add_body_not_object", "add_no_body"} {
		doc, errs := decode(t, name)
		if len(errs) == 0 {
			_, errs = ParseAddRows(doc)
		}
		assertValidation(t, name, errs)
	}
}

func TestAddRowsRequestDefaultsAndOrder(t *testing.T) {
	doc, errs := DecodeBody([]byte(`{"rows":[{"b":1,"a":{"z":1.50,"y":[1e2]}}]}`))
	if errs != nil {
		t.Fatal(errs)
	}
	req, errs := ParseAddRows(doc)
	if errs != nil || !req.Dedupe || len(req.Rows) != 1 {
		t.Fatalf("%v %+v", errs, req)
	}
	if got := PyDumps(req.Rows[0], false); got != `{"b": 1, "a": {"z": 1.5, "y": [100.0]}}` {
		t.Fatalf("row not kept in request order with python numbers: %s", got)
	}
}

func TestDeleteRowsRequestValidationMatchesFixtures(t *testing.T) {
	for _, name := range []string{"delete_empty", "delete_bad_ids"} {
		doc, errs := decode(t, name)
		if len(errs) == 0 {
			_, errs = ParseDeleteRows(doc)
		}
		assertValidation(t, name, errs)
	}
	doc, _ := DecodeBody([]byte(`{"row_ids":[1,"2",3.0,true,99999999999999999999]}`))
	req, errs := ParseDeleteRows(doc)
	if errs != nil || req.Requested != 5 || len(req.IDs) != 4 {
		t.Fatalf("lax ints: %v %+v", errs, req)
	}
}

func TestDeleteMatchingRequestValidationMatchesFixtures(t *testing.T) {
	for _, name := range []string{"dq_invalid", "dq_missing"} {
		doc, errs := decode(t, name)
		if len(errs) == 0 {
			_, errs = ParseDeleteMatching(doc)
		}
		assertValidation(t, name, errs)
	}
	doc, _ := DecodeBody([]byte(`{"expected_count":3,"confirmation":"c","view_id":null,"search":null}`))
	if req, errs := ParseDeleteMatching(doc); errs != nil || req.ExpectedCount != 3 || req.ViewID != nil || req.Search != nil {
		t.Fatalf("%v %+v", errs, req)
	}
}

func TestBulkUpdateRequestValidationMatchesFixtures(t *testing.T) {
	for _, name := range []string{"bulk_empty_fields", "bulk_empty_updates", "bulk_missing_updates", "bulk_1001_updates"} {
		doc, errs := decode(t, name)
		if len(errs) == 0 {
			_, errs = ParseBulkUpdate(doc)
		}
		assertValidation(t, name, errs)
	}
	// bulk_bad_items also carries a recompute query error, so only the body part is compared.
	doc, _ := decode(t, "bulk_bad_items")
	_, errs := ParseBulkUpdate(doc)
	fx := contract.Load(t, "rows/bulk_bad_items")
	var want struct {
		Detail []httpx.FieldError `json:"detail"`
	}
	if err := json.Unmarshal(fx.Response.Body, &want); err != nil {
		t.Fatal(err)
	}
	contract.AssertEqual(t, contract.Resp{Status: 422, Headers: fx.Response.Headers, Body: detailOf(errs)},
		contract.Resp{Status: 422, Headers: fx.Response.Headers, Body: detailOf(want.Detail[1:])})
}

func TestDictBodyValidationMatchesFixtures(t *testing.T) {
	for _, name := range []string{"row_body_not_object", "lead_body_not_object"} {
		doc, errs := decode(t, name)
		if len(errs) == 0 {
			_, errs = ParseDictBody(doc)
		}
		assertValidation(t, name, errs)
	}
	doc, errs := DecodeBody(nil)
	if errs == nil {
		_, errs = ParseDictBody(doc)
	}
	assertValidation(t, "row_no_body", errs)
}

func TestDecodeBodyJSONInvalid(t *testing.T) {
	_, errs := DecodeBody([]byte(`{bad`))
	if len(errs) != 1 || errs[0].Type != "json_invalid" || errs[0].Msg != "JSON decode error" {
		t.Fatalf("%+v", errs)
	}
}

func TestQueryHelpersParseLikePydantic(t *testing.T) {
	q, _ := url.ParseQuery("recompute=maybe")
	if _, fe := RecomputeParam(q); fe == nil || fe.Type != "bool_parsing" || fe.Loc[0] != "query" {
		t.Fatalf("%+v", fe)
	}
	for in, want := range map[string]bool{"": true, "false": false, "0": false, "Off": false, "yes": true} {
		raw := "recompute=" + in
		if in == "" {
			raw = ""
		}
		q, _ := url.ParseQuery(raw)
		if got, fe := RecomputeParam(q); fe != nil || got != want {
			t.Errorf("%q: %v %+v", in, got, fe)
		}
	}
	if _, fe := PathInt("abc", "row_id"); fe == nil || fe.Type != "int_parsing" || fe.Loc[1] != "row_id" {
		t.Fatalf("%+v", fe)
	}
	if n, fe := PathInt(" 7 ", "row_id"); fe != nil || n != 7 {
		t.Fatalf("%d %+v", n, fe)
	}
	if n, fe := PathInt("99999999999999999999", "row_id"); fe != nil || n != -1 {
		t.Fatalf("out of range must read as 'no such row': %d %+v", n, fe)
	}
}
