package wbcrud

import (
	"encoding/json"
	"math"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

// telemetryVectors mirrors testdata/wbcrud/telemetry_vectors.json, produced by the real
// Python (legacy/python/scripts/dump_telemetry_vectors.py).
type telemetryVectors struct {
	SelectedCellCount []struct {
		Rows       json.RawMessage `json:"rows"`
		Columns    json.RawMessage `json:"columns"`
		RowColumns json.RawMessage `json:"row_columns"`
		Want       *int            `json:"want"`
	} `json:"selected_cell_count"`
	OutputAttempts []struct {
		In   json.RawMessage `json:"in"`
		Want json.RawMessage `json:"want"`
	} `json:"output_attempts"`
	BudgetStrings []struct {
		In   string `json:"in"`
		Want string `json:"want"`
		Err  string `json:"err"`
		Msg  string `json:"msg"`
		Ctx  any    `json:"ctx"`
	} `json:"budget_strings"`
	IntStrings []struct {
		In   string `json:"in"`
		Want string `json:"want"`
		Err  string `json:"err"`
	} `json:"int_strings"`
	Exposure []struct {
		Micro *int64 `json:"micro"`
		Want  string `json:"want"`
	} `json:"exposure"`
	Rounds []struct {
		X    float64 `json:"x"`
		Nd   int     `json:"nd"`
		Want string  `json:"want"`
	} `json:"rounds"`
}

func loadTelemetryVectors(t *testing.T) telemetryVectors {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/wbcrud/telemetry_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v telemetryVectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func telDecode(t *testing.T, raw json.RawMessage) any {
	t.Helper()
	if len(raw) == 0 {
		return nil
	}
	v, err := decodeDocument(raw, true)
	if err != nil {
		t.Fatalf("vector %s: %v", raw, err)
	}
	return v
}

func TestSelectedCellCountVectors(t *testing.T) {
	for i, c := range loadTelemetryVectors(t).SelectedCellCount {
		got := SelectedCellCountJSON(telDecode(t, c.Rows), telDecode(t, c.Columns), telDecode(t, c.RowColumns))
		switch {
		case c.Want == nil && got != nil:
			t.Errorf("#%d rows=%s cols=%s rc=%s: got %d want null", i, c.Rows, c.Columns, c.RowColumns, *got)
		case c.Want != nil && (got == nil || *got != *c.Want):
			t.Errorf("#%d rows=%s cols=%s rc=%s: got %v want %d", i, c.Rows, c.Columns, c.RowColumns, got, *c.Want)
		}
	}
}

func TestSelectedCellCountTypedMap(t *testing.T) {
	rows := []any{json.Number("1"), json.Number("2")}
	cols := []any{"a", "b"}
	if got := SelectedCellCount(rows, cols, nil); got == nil || *got != 4 {
		t.Fatalf("nil map: %v", got)
	}
	if got := SelectedCellCount(rows, cols, map[string][]string{"1": {"a"}, "2": {"a", "b"}}); got == nil || *got != 3 {
		t.Fatalf("map: %v", got)
	}
	if got := SelectedCellCount(rows, cols, map[string][]string{}); got != nil {
		t.Fatalf("empty (present) map must not match two rows: %d", *got)
	}
}

func TestSummarizeOutputAttemptsVectors(t *testing.T) {
	for i, c := range loadTelemetryVectors(t).OutputAttempts {
		got := SummarizeOutputAttempts(telDecode(t, c.In))
		var gotJSON []byte
		if got == nil {
			gotJSON = []byte("null")
		} else {
			gotJSON, _ = json.Marshal(got)
		}
		// Key order matters (total, awaiting_receipt, succeeded, failed, unknown): compare compact text.
		want := string(c.Want)
		want = telCompact(want)
		if string(gotJSON) != want {
			t.Errorf("#%d in=%s: got %s want %s", i, c.In, gotJSON, want)
		}
	}
}

func telCompact(s string) string {
	var out []byte
	inStr := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			out = append(out, c)
			if c == '\\' && i+1 < len(s) {
				i++
				out = append(out, s[i])
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case ' ', '\n', '\t', '\r':
		case '"':
			inStr = true
			out = append(out, c)
		default:
			out = append(out, c)
		}
	}
	return string(out)
}

func TestBudgetBodyVectors(t *testing.T) {
	for _, c := range loadTelemetryVectors(t).BudgetStrings {
		raw, _ := json.Marshal(map[string]string{"max_usd": c.In})
		got, errs := ParseBudgetBody(raw)
		if c.Err == "" {
			if len(errs) != 0 {
				t.Errorf("%q: unexpected errors %+v", c.In, errs)
				continue
			}
			if s := PyFloatRepr(got); s != c.Want {
				t.Errorf("%q: got %s want %s", c.In, s, c.Want)
			}
			continue
		}
		if len(errs) != 1 || errs[0].Type != c.Err || errs[0].Msg != c.Msg {
			t.Errorf("%q: got %+v want %s %q", c.In, errs, c.Err, c.Msg)
			continue
		}
		if c.Ctx != nil && errs[0].Ctx == nil {
			t.Errorf("%q: missing ctx %v", c.In, c.Ctx)
		}
		if loc := errs[0].Loc; len(loc) != 2 || loc[0] != "body" || loc[1] != "max_usd" {
			t.Errorf("%q: loc %v", c.In, loc)
		}
	}
}

func TestBudgetNegative422AndNaN422(t *testing.T) {
	for _, tc := range []struct {
		body, typ string
	}{
		{`{"max_usd":-1}`, "greater_than_equal"},
		{`{"max_usd":-0.0000001}`, "greater_than_equal"},
		{`{"max_usd":"-5"}`, "greater_than_equal"},
		{`{"max_usd":"nan"}`, "finite_number"},
		{`{"max_usd":"inf"}`, "finite_number"},
		{`{"max_usd":1e999}`, "finite_number"},
		{`{"max_usd":null}`, "float_type"},
		{`{"max_usd":[1]}`, "float_type"},
		{`{"max_usd":{}}`, "float_type"},
		{`{"max_usd":"abc"}`, "float_parsing"},
		{`[1]`, "model_attributes_type"},
		{`"x"`, "model_attributes_type"},
		{``, "missing"},
		{`null`, "missing"},
		{`{"max_usd":`, "json_invalid"},
		{`{"max_usd":NaN}`, "json_invalid"},
	} {
		_, errs := ParseBudgetBody([]byte(tc.body))
		if len(errs) != 1 || errs[0].Type != tc.typ {
			t.Errorf("%s: got %+v want %s", tc.body, errs, tc.typ)
		}
	}
	if _, errs := ParseBudgetBody([]byte(`{"max_usd":-1}`)); len(errs) == 1 {
		e := errs[0]
		if e.Msg != "Input should be greater than or equal to 0" || e.Ctx["ge"] != 0.0 {
			t.Errorf("negative detail: %+v", e)
		}
	}
	for body, want := range map[string]float64{`{}`: 0, `{"max_usd":3}`: 3, `{"max_usd":true}`: 1, `{"max_usd":false}`: 0, `{"max_usd":"7.5"}`: 7.5,
		`{"max_usd":3,"x":1}`: 3, `{"max_usd":1e300}`: 1e300} {
		got, errs := ParseBudgetBody([]byte(body))
		if len(errs) != 0 || got != want {
			t.Errorf("%s: got %v %+v want %v", body, got, errs, want)
		}
	}
	if got, errs := ParseBudgetBody([]byte(`{"max_usd":-0.0}`)); len(errs) != 0 || !math.Signbit(got) {
		t.Errorf("-0.0 must stay negative zero: %v %+v", got, errs)
	}
}

func TestIntParamVectors(t *testing.T) {
	for _, c := range loadTelemetryVectors(t).IntStrings {
		n, ok := ParseIntParam(c.In)
		if c.Err != "" {
			if ok {
				t.Errorf("%q: parsed %v, want int_parsing", c.In, n)
			}
			continue
		}
		if !ok || n.String() != c.Want {
			t.Errorf("%q: got %v %v want %s", c.In, n, ok, c.Want)
		}
	}
}

func TestQueryIntBounds(t *testing.T) {
	ge1, le100 := int64(1), int64(100)
	q := url.Values{"limit": {"0"}, "before_id": {"0"}}
	var errs []httpx.FieldError
	if _, e := QueryInt(q, "before_id", nil, &ge1, &BeforeIDMax); e != nil {
		errs = append(errs, *e)
	}
	if _, e := QueryInt(q, "limit", telP64(20), &ge1, &le100); e != nil {
		errs = append(errs, *e)
	}
	if len(errs) != 2 || errs[0].Loc[1] != "before_id" || errs[1].Loc[1] != "limit" {
		t.Fatalf("errors in declaration order: %+v", errs)
	}
	if errs[0].Type != "greater_than_equal" || errs[0].Ctx["ge"] != int64(1) || errs[0].Input != "0" {
		t.Fatalf("before_id: %+v", errs[0])
	}
	// Python answers 500 for before_id >= 2**31 (int4 overflow); Go: 422 less_than_equal.
	huge := url.Values{"before_id": {"2147483648"}}
	if _, e := QueryInt(huge, "before_id", nil, &ge1, &BeforeIDMax); e == nil || e.Type != "less_than_equal" || e.Ctx["le"] != int64(2147483647) {
		t.Fatalf("before_id 2**31: %+v", e)
	}
	beyond := url.Values{"limit": {"99999999999999999999"}}
	if _, e := QueryInt(beyond, "limit", telP64(20), &ge1, &le100); e == nil || e.Type != "less_than_equal" || e.Input != "99999999999999999999" {
		t.Fatalf("limit beyond int64: %+v", e)
	}
	absent := url.Values{}
	if v, e := QueryInt(absent, "limit", telP64(20), &ge1, &le100); e != nil || v != 20 {
		t.Fatalf("default: %v %+v", v, e)
	}
	if v, e := QueryInt(url.Values{"limit": {"2.0"}}, "limit", telP64(20), &ge1, &le100); e != nil || v != 2 {
		t.Fatalf("2.0: %v %+v", v, e)
	}
	if _, e := QueryInt(url.Values{"limit": {"x"}}, "limit", telP64(20), &ge1, &le100); e == nil || e.Type != "int_parsing" {
		t.Fatalf("x: %+v", e)
	}
	// An optional parameter (no default) that is absent reports present=false via a zero value and no error.
	if _, e := QueryInt(url.Values{}, "before_id", nil, &ge1, &BeforeIDMax); e != nil {
		t.Fatalf("absent optional: %+v", e)
	}
}

func telP64(n int64) *int64 { return &n }

func TestPyRoundVectors(t *testing.T) {
	for _, c := range loadTelemetryVectors(t).Rounds {
		if got := PyFloatRepr(PyRound(c.X, c.Nd)); got != c.Want {
			t.Errorf("round(%v,%d): got %s want %s", c.X, c.Nd, got, c.Want)
		}
	}
}

func TestExposureUSDVectors(t *testing.T) {
	for _, c := range loadTelemetryVectors(t).Exposure {
		got := ExposureJSON(c.Micro)
		if got != c.Want {
			t.Errorf("micro=%v: got %s want %s", c.Micro, got, c.Want)
		}
	}
}

func telF(v float64) *float64 { return &v }

// TestCostExposureMath: reserved = reserved + dispatched, uncertain apart, remaining null when unlimited.
func TestCostExposureMath(t *testing.T) {
	for _, tc := range []struct {
		name     string
		max      *float64
		spent    *float64
		exposure map[string]int64
		want     string
	}{
		{"no exposure unlimited", nil, nil, nil,
			`{"budget_max_usd":0.0,"budget_spent_usd":0.0,"reserved_usd":0.0,"uncertain_usd":0.0,"remaining_usd":null,"accounting_basis":"catalog_estimates_and_recorded_charges","unlimited":true}`},
		{"reserved plus dispatched", nil, telF(0.5), map[string]int64{"reserved": 2000, "dispatched": 1500000, "uncertain": 2500000},
			`{"budget_max_usd":0.0,"budget_spent_usd":0.5,"reserved_usd":1.502,"uncertain_usd":2.5,"remaining_usd":null,"accounting_basis":"catalog_estimates_and_recorded_charges","unlimited":true}`},
		{"only dispatched integer valued", nil, nil, map[string]int64{"dispatched": 1000000},
			`{"budget_max_usd":0.0,"budget_spent_usd":0.0,"reserved_usd":1,"uncertain_usd":0.0,"remaining_usd":null,"accounting_basis":"catalog_estimates_and_recorded_charges","unlimited":true}`},
		{"capped no exposure", telF(12.5), telF(1.23456789), nil,
			`{"budget_max_usd":12.5,"budget_spent_usd":1.2346,"reserved_usd":0.0,"uncertain_usd":0.0,"remaining_usd":11.265432,"accounting_basis":"catalog_estimates_and_recorded_charges","unlimited":false}`},
		// FastAPI answers 500 here (float - Decimal); Go computes the figure (documented deviation).
		{"capped with exposure", telF(10), telF(1), map[string]int64{"reserved": 2000000, "uncertain": 500000},
			`{"budget_max_usd":10.0,"budget_spent_usd":1.0,"reserved_usd":2,"uncertain_usd":0.5,"remaining_usd":6.5,"accounting_basis":"catalog_estimates_and_recorded_charges","unlimited":false}`},
		{"overspent goes negative", telF(1), telF(3), nil,
			`{"budget_max_usd":1.0,"budget_spent_usd":3.0,"reserved_usd":0.0,"uncertain_usd":0.0,"remaining_usd":-2.0,"accounting_basis":"catalog_estimates_and_recorded_charges","unlimited":false}`},
		{"negative cap is unlimited", telF(-5), nil, nil,
			`{"budget_max_usd":-5.0,"budget_spent_usd":0.0,"reserved_usd":0.0,"uncertain_usd":0.0,"remaining_usd":null,"accounting_basis":"catalog_estimates_and_recorded_charges","unlimited":true}`},
		{"settled and released are not exposure", telF(2), nil, map[string]int64{},
			`{"budget_max_usd":2.0,"budget_spent_usd":0.0,"reserved_usd":0.0,"uncertain_usd":0.0,"remaining_usd":2.0,"accounting_basis":"catalog_estimates_and_recorded_charges","unlimited":false}`},
	} {
		b, err := json.Marshal(BuildCost(tc.max, tc.spent, tc.exposure))
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", tc.name, b, tc.want)
		}
	}
}

func TestRunItemMaskingAndShape(t *testing.T) {
	payload := `{"workbook_id":"w","run_id":"r1","row_ids":[1,2,3],"column_ids":["a","b"],"fill_missing":1,"force":[],
	 "output_attempts":{"x":{"state":"dispatch_claimed"}},
	 "execution_result":{"retry_count":1.0,"completed":2,"errors":0,"total":3,"rows":3,"stopped":false,"recorded_at":"t","secret":"x"}}`
	it := BuildRunItem(RunJob{ID: 7, Payload: []byte(payload), Status: telStr("running"), RetryCount: telInt(1)})
	b, _ := json.Marshal(it)
	want := `{"job_id":7,"run_id":"r1","status":"running","row_count":3,"column_count":2,"selected_cell_count":6,"output_attempts":{"total":1,"awaiting_receipt":1,"succeeded":0,"failed":0,"unknown":0},` +
		`"fill_missing":true,"force":false,"retry_count":1,"error":null,"created_at":null,"started_at":null,"completed_at":null,"last_heartbeat":null,"next_run_at":null,` +
		`"result":{"completed":2,"errors":0,"total":3,"rows":3,"stopped":false,"recorded_at":"t"}}`
	if string(b) != want {
		t.Fatalf("\n got %s\nwant %s", b, want)
	}
	// A previous attempt's result (retry_count differs, or absent, or not a number) is hidden.
	for _, res := range []string{`{"retry_count":0,"completed":1}`, `{"completed":1}`, `{"retry_count":"1","completed":1}`, `{"retry_count":null}`, `"oops"`, `[1]`} {
		p := `{"execution_result":` + res + `}`
		it := BuildRunItem(RunJob{ID: 1, Payload: []byte(p), RetryCount: telInt(1)})
		b, _ := json.Marshal(it)
		if !telHas(string(b), `"result":null`) {
			t.Errorf("result %s not masked: %s", res, b)
		}
	}
	// NULL job.retry_count counts as 0; True == 1 in Python.
	for res, shown := range map[string]bool{`{"retry_count":0}`: true, `{"retry_count":false}`: true, `{"retry_count":0.0}`: true, `{"retry_count":1}`: false} {
		it := BuildRunItem(RunJob{ID: 1, Payload: []byte(`{"execution_result":` + res + `}`)})
		b, _ := json.Marshal(it)
		if telHas(string(b), `"result":null`) == shown {
			t.Errorf("result %s: shown=%v got %s", res, shown, b)
		}
	}
	// Non-object payload behaves like an empty one (Python would raise AttributeError for a list).
	it = BuildRunItem(RunJob{ID: 2, Payload: []byte(`[1]`)})
	b, _ = json.Marshal(it)
	if !telHas(string(b), `"row_count":null`) {
		t.Errorf("list payload: %s", b)
	}
}

func telStr(s string) *string   { return &s }
func telInt(n int) *int         { return &n }
func telHas(s, sub string) bool { return strings.Contains(s, sub) }
