package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// callValidation runs a read handler without a workspace: only requests rejected by
// FastAPI-style validation (before any database access) may be exercised this way.
func callValidation(t *testing.T, h http.HandlerFunc, path string, pathID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	if pathID != "" {
		req.SetPathValue("lead_id", pathID)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

type valErr struct {
	Type  string `json:"type"`
	Loc   []any  `json:"loc"`
	Msg   string `json:"msg"`
	Input any    `json:"input"`
}

func decodeValErrs(t *testing.T, rec *httptest.ResponseRecorder) []valErr {
	t.Helper()
	if rec.Code != 422 {
		t.Fatalf("status %d, want 422: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Detail []valErr `json:"detail"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Detail
}

func TestListLeadsBadBool422(t *testing.T) {
	h := leadReadRoutes{}
	errs := decodeValErrs(t, callValidation(t, h.list, "/api/leads?has_email=maybe&has_phone=nope", ""))
	if len(errs) != 2 || errs[0].Type != "bool_parsing" || !reflect.DeepEqual(errs[0].Loc, []any{"query", "has_email"}) ||
		errs[1].Loc[1] != "has_phone" {
		t.Fatalf("errors %+v", errs)
	}
}

func TestListLeadsBadIntegers422(t *testing.T) {
	h := leadReadRoutes{}
	errs := decodeValErrs(t, callValidation(t, h.list, "/api/leads?score_min=abc&limit=x&offset=1.5", ""))
	if len(errs) != 3 {
		t.Fatalf("errors %+v", errs)
	}
	for i, name := range []string{"score_min", "limit", "offset"} {
		if errs[i].Type != "int_parsing" || errs[i].Loc[1] != name {
			t.Errorf("error %d = %+v, want int_parsing on %s", i, errs[i], name)
		}
	}
}

// D2: Python forwards a negative LIMIT/OFFSET to Postgres, which fails (500); Go answers 422.
func TestListLeadsNegativeLimitOffset422(t *testing.T) {
	h := leadReadRoutes{}
	errs := decodeValErrs(t, callValidation(t, h.list, "/api/leads?limit=-1&offset=-5", ""))
	if len(errs) != 2 || errs[0].Type != "greater_than_equal" || errs[0].Loc[1] != "limit" ||
		errs[1].Type != "greater_than_equal" || errs[1].Loc[1] != "offset" {
		t.Fatalf("errors %+v", errs)
	}
}

func TestListLimitCap(t *testing.T) {
	for _, c := range []struct {
		raw  string
		want int
	}{
		{"200", 200}, {"0", 0}, {"10000", 10000}, {"10001", 10000}, {"99999999999999999999999", 10000},
	} {
		v := &queryValidator{q: map[string][]string{"limit": {c.raw}}}
		if got := listLimit(v); got != c.want || len(v.errs) != 0 {
			t.Errorf("limit %s = %d (%v), want %d", c.raw, got, v.errs, c.want)
		}
	}
	if got := listLimit(&queryValidator{q: map[string][]string{}}); got != 200 {
		t.Errorf("default limit = %d", got)
	}
}

func TestCleanAllSentinel(t *testing.T) {
	for in, want := range map[string]string{"": "", "__all__": "", "Pune": "Pune", "__ALL__": "__ALL__", " ": " "} {
		if got := cleanFilter(in); got != want {
			t.Errorf("cleanFilter(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGetLeadNonIntPath422(t *testing.T) {
	h := leadReadRoutes{}
	for _, fn := range []http.HandlerFunc{h.get, h.similar} {
		errs := decodeValErrs(t, callValidation(t, fn, "/api/lead/abc", "abc"))
		if len(errs) != 1 || errs[0].Type != "int_parsing" || !reflect.DeepEqual(errs[0].Loc, []any{"path", "lead_id"}) || errs[0].Input != "abc" {
			t.Fatalf("errors %+v", errs)
		}
	}
}

func TestSimilarBadLimit422(t *testing.T) {
	h := leadReadRoutes{}
	errs := decodeValErrs(t, callValidation(t, h.similar, "/api/lead/1/similar?limit=zz", "1"))
	if len(errs) != 1 || errs[0].Loc[1] != "limit" || errs[0].Type != "int_parsing" {
		t.Fatalf("errors %+v", errs)
	}
}

func TestFiltersStatusesConstant(t *testing.T) {
	if want := []string{"new", "contacted", "qualified", "negotiating", "converted", "dead"}; !reflect.DeepEqual(leadStatuses, want) {
		t.Errorf("statuses %v", leadStatuses)
	}
	if want := []string{"hot", "warm", "cold", "unqualified"}; !reflect.DeepEqual(leadTiers, want) {
		t.Errorf("tiers %v", leadTiers)
	}
}

func TestPyJSONKeepsLineSeparatorsRaw(t *testing.T) {
	in := []byte(`["a\u2028b","\\u2028","c\u2029"]`)
	want := "[\"a\u2028b\",\"\\\\u2028\",\"c\u2029\"]"
	if got := string(pyJSON(in)); got != want {
		t.Errorf("got %q want %q", got, want)
	}
	if strings.Contains(string(pyJSON([]byte(`"\\\u2028"`))), `\u2028`) {
		t.Error("escaped backslash then escaped separator mishandled")
	}
}
