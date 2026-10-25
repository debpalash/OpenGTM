package spend_test

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/spend"
)

type vecFile struct {
	AttemptKey []struct {
		WorkspaceID string `json:"workspace_id"`
		WorkbookID  string `json:"workbook_id"`
		JobID       int64  `json:"job_id"`
		RowIdentity string `json:"row_identity"`
		ColumnID    string `json:"column_id"`
		Provider    string `json:"provider"`
		Key         string `json:"key"`
	} `json:"attempt_key"`
	ContractHash []struct {
		WorkbookID  string          `json:"workbook_id"`
		RunID       string          `json:"run_id"`
		RowIdentity string          `json:"row_identity"`
		ColumnID    string          `json:"column_id"`
		Provider    string          `json:"provider"`
		Exposure    int64           `json:"exposure"`
		CellLimit   int64           `json:"cell_limit"`
		CostBasis   json.RawMessage `json:"cost_basis"`
		Operation   json.RawMessage `json:"operation"`
		Serialized  string          `json:"serialized"`
		Digest      string          `json:"digest"`
	} `json:"contract_hash"`
}

func loadVectors(t *testing.T) vecFile {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/canonjson_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vecFile
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestAttemptKeyMatchesPython(t *testing.T) {
	v := loadVectors(t)
	if len(v.AttemptKey) < 5 {
		t.Fatal("too few vectors")
	}
	for _, c := range v.AttemptKey {
		if got := spend.AttemptKey(c.WorkspaceID, c.WorkbookID, c.JobID, c.RowIdentity, c.ColumnID, c.Provider); got != c.Key {
			t.Errorf("%+v: got %s", c, got)
		}
	}
	for _, bad := range [][3]string{{"", "c", "p"}, {"r", "", "p"}, {"r", "c", ""}} {
		if spend.AttemptKey("w", "b", 1, bad[0], bad[1], bad[2]) != "" {
			t.Errorf("empty identity must yield empty key: %v", bad)
		}
	}
	if spend.AttemptKey("w", "b", 0, "r", "c", "p") != "" {
		t.Error("job id must be positive")
	}
}

func mustMap(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	v, err := canonjson.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	return v.(map[string]any)
}

func TestContractHashMatchesPython(t *testing.T) {
	for _, c := range loadVectors(t).ContractHash {
		r := spend.Reservation{WorkbookID: c.WorkbookID, RunID: c.RunID, RowIdentity: c.RowIdentity,
			ColumnID: c.ColumnID, Provider: c.Provider, ExposureMicroUSD: c.Exposure, CellLimitMicroUSD: c.CellLimit,
			CostBasis: mustMap(t, c.CostBasis), Operation: mustMap(t, c.Operation)}
		ser, digest, err := spend.ContractHash(r)
		if err != nil {
			t.Fatal(err)
		}
		if ser != c.Serialized || digest != c.Digest {
			t.Errorf("mismatch:\n got %s %s\nwant %s %s", ser, digest, c.Serialized, c.Digest)
		}
	}
}

func TestExposureFromUSD(t *testing.T) {
	a, b := 0.1, 0.2
	for _, c := range []struct {
		usd  float64
		want int64
	}{{0.05, 50000}, {1e-07, 1}, {a + b, 300001}, {0, 0}, {1.0, 1000000}, {0.000001, 1}, {0.0000010000000000000002, 2}, {19.99, 19990000}} {
		got, err := spend.ExposureFromUSD(c.usd)
		if err != nil || got != c.want {
			t.Errorf("%v: got %d err %v want %d", c.usd, got, err, c.want)
		}
	}
	for _, bad := range []float64{-1, 1e300} {
		if _, err := spend.ExposureFromUSD(bad); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

func TestAccountingEnvelope(t *testing.T) {
	ok := spend.ProviderResponse{"provider": "p", "success": true, "data": map[string]any{"x": 1.5}}
	env, err := spend.AccountingEnvelope("p", ok, 500)
	if err != nil || env.ChargedMicroUSD != 500 || env.AccountingBasis != "catalog_estimate" {
		t.Fatalf("%+v %v", env, err)
	}
	env.Result["data"].(map[string]any)["x"] = 9.0
	if ok["data"].(map[string]any)["x"] != 1.5 {
		t.Fatal("result must be a deep copy")
	}
	free, _ := spend.AccountingEnvelope("p", spend.ProviderResponse{"provider": "p", "success": false}, 0)
	if free.AccountingBasis != "catalog_estimate" || free.ChargedMicroUSD != 0 {
		t.Fatalf("%+v", free)
	}
	ev := func(amt any, ref any) spend.ProviderResponse {
		return spend.ProviderResponse{"provider": "p", "success": false, "billing_evidence": map[string]any{"charged_microusd": amt, "reference": ref}}
	}
	if e, err := spend.AccountingEnvelope("p", ev(int64(7), "inv-1"), 500); err != nil || e.AccountingBasis != "vendor_confirmed" || e.ChargedMicroUSD != 7 {
		t.Fatalf("%+v %v", e, err)
	}
	if e, err := spend.AccountingEnvelope("p", ev(int64(0), "inv-1"), 500); err != nil || e.AccountingBasis != "confirmed_nonbillable" {
		t.Fatalf("%+v %v", e, err)
	}
	uncertain := []spend.ProviderResponse{
		nil, {"provider": "other", "success": true}, {"provider": "p"}, {"provider": "p", "success": "yes"},
		{"provider": "p", "success": false}, // paid, unsuccessful, no evidence
		ev(int64(-1), "r"), ev(1.5, "r"), ev(true, "r"), ev(int64(1), "  \t"), ev(int64(1), 5), ev(int64(1), nil),
		{"provider": "p", "success": true, "billing_evidence": "x"},
	}
	for i, r := range uncertain {
		if _, err := spend.AccountingEnvelope("p", r, 500); !errors.Is(err, spend.ErrUncertainAccounting) {
			t.Errorf("case %d: got %v", i, err)
		}
	}
	if _, err := spend.AccountingEnvelope("p", ok, -1); err == nil || errors.Is(err, spend.ErrUncertainAccounting) {
		t.Errorf("invalid catalog exposure must be a plain error, got %v", err)
	}
}

func TestJSONEqualIsStructural(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{`["x","y"]`, `["x s:y"]`, false},
		{`["a b"]`, `["a","b"]`, false},
		{`{"k":"s:x"}`, `{"k":"x"}`, false},
		{`["n:1"]`, `[1]`, false},
		{`[["x"],"y"]`, `["x","y"]`, false},
		{`{"a":[1,2]}`, `{"a":"[1 2]"}`, false},
		{`{"a":1}`, `{"a":1.0}`, true},
		{`{"a":1e0,"b":[2]}`, `{"b":[2.0],"a":1}`, true},
		{`{"a":true}`, `{"a":1}`, true},
		{`{"a":0.1}`, `{"a":0.10}`, true},
		{`{"a":1}`, `{"a":2}`, false},
		{`{"a":null}`, `{"a":"null"}`, false},
		{`{"a":1e999999999}`, `{"a":1e999999999}`, false},
		{`{"a":1e-999999999}`, `{"a":0}`, false},
	}
	for _, c := range cases {
		if got := spend.JSONEqual([]byte(c.a), []byte(c.b)); got != c.want {
			t.Errorf("%s vs %s: got %v want %v", c.a, c.b, got, c.want)
		}
	}
	if spend.JSONEqual([]byte("1"+strings.Repeat("0", 1200)), []byte("1")) {
		t.Error("oversized mantissa treated as equal")
	}
}
