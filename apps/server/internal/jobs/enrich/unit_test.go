package enrich

import (
	"crypto/sha256"
	"encoding/hex"
	"math/big"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/pycompat"
)

// Golden values below were produced by the Python implementation
// (execution_identity.py, spend_service.py, compiler.py, vendor_catalog.py).

func TestAttemptKeyMatchesPython(t *testing.T) {
	got, err := attemptKey("ws-éü", "wb-1", "job:7", "row:3", `col"x`, "prov")
	if err != nil {
		t.Fatal(err)
	}
	if want := "ebe8be1aa822335ed894cee20aeeaf02a68468e040f7788ef5aea4647904d84b"; got != want {
		t.Errorf("attempt key = %s, want %s", got, want)
	}
	if _, err := attemptKey("w", "b", "r", "", "c", "p"); err == nil {
		t.Error("an empty identity must be refused")
	}
}

func TestContractDigestMatchesPython(t *testing.T) {
	lead := pycompat.NewMap()
	_ = lead.Set("company", "Ünï")
	_ = lead.Set("n", 1.0)
	big20, _ := new(big.Int).SetString("100000000000000000000", 10)
	_ = lead.Set("big", big20)
	_ = lead.Set("f", 1e-07)
	_ = lead.Set("id", int64(3))
	_ = lead.Set("__lead_id", nil)
	r := reservation{
		workbookID: "wb", runID: "job:1", rowIdentity: "row:1", columnID: "c", provider: "p",
		exposure: 50000, cellLimit: 50000,
		costBasis: obj("kind", "catalog_estimate", "provider", "p"),
		operation: obj("inputs", lead, "column", obj("id", "c", "verify", false)),
	}
	raw, err := contractJSON(r)
	if err != nil {
		t.Fatal(err)
	}
	wantJSON := `{"cell_limit":50000,"column_id":"c","cost_basis":{"kind":"catalog_estimate","provider":"p"},"exposure":50000,` +
		`"operation":{"column":{"id":"c","verify":false},"inputs":{"__lead_id":null,"big":100000000000000000000,` +
		"\"company\":\"\\u00dcn\\u00ef\",\"f\":1e-07," +
		`"id":3,"n":1.0}},"provider":"p","row_identity":"row:1","run_id":"job:1","workbook_id":"wb"}`
	if string(raw) != wantJSON {
		t.Fatalf("contract JSON differs:\n got %s\nwant %s", raw, wantJSON)
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != "50433a8b87f351e56a6f62a38033353677340aeae8314b9f575a2f3ebfa62d15" {
		t.Errorf("digest = %s", got)
	}
}

func TestMicroUSDRoundsUpLikeDecimal(t *testing.T) {
	a, b := 0.1, 0.2
	sum := a + b // 0.30000000000000004 at run time (an untyped constant would be exactly 0.3)
	for _, c := range []struct {
		in   float64
		want int64
	}{{0.05, 50000}, {0.008, 8000}, {1e-05, 10}, {sum, 300001}, {1.0, 1000000}, {0, 0}, {123456.789012345, 123456789013}} {
		if got := microUSD(c.in); got != c.want {
			t.Errorf("microUSD(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestProviderInputsMatchPython(t *testing.T) {
	for in, want := range map[string]string{
		"https://www.Acme.EXAMPLE/a?b": "acme.example", "HTTP://WWW.x.io": "x.io", " www.y.org/ ": "y.org",
		"": "", "foo.com?x=1": "foo.com", "https://https://z.com": "https:",
	} {
		got, err := domainOf(in)
		if err != nil || got != want {
			t.Errorf("domainOf(%q) = %q %v, want %q", in, got, err, want)
		}
	}
	lead := obj("contact_person", "  Jane   Q  Public ", "website", "x.com", "company", int64(5), "email", nil)
	in, err := providerInputs(lead)
	if err != nil {
		t.Fatal(err)
	}
	if in["first_name"] != "Jane" || in["last_name"] != "Public" || in["domain"] != "x.com" || in["company"] != int64(5) || in["email"] != "" {
		t.Errorf("inputs = %v", in)
	}
	// A non-string value breaks the Python provider with this exact text.
	_, err = providerInputs(obj("website", int64(123)))
	if err == nil || err.Error() != "'int' object has no attribute 'strip'" {
		t.Errorf("website int: %v", err)
	}
	_, err = providerInputs(obj("contact_person", []any{"x"}))
	if err == nil || err.Error() != "'list' object has no attribute 'split'" {
		t.Errorf("contact list: %v", err)
	}
}

func TestCellTextSummarisesJSONBlobs(t *testing.T) {
	for in, want := range map[string]string{
		"plain":                                  "plain",
		`[{"name":"Jane"},{"name":"Bob"}]`:       "Jane +1 more",
		`[{"email":"j@x.example"}]`:              "j@x.example",
		`[{"id":1},{"id":2},{"id":3}]`:           "3 results",
		`{"a": 1}`:                               "{\"a\": 1}",
		`[{"broken"`:                             `[{"broken"`,
		`[{"name": "` + strings.Repeat("x", 100): `[{"name": "` + strings.Repeat("x", 69), // sv[:80]
	} {
		got, err := cellText(in)
		if err != nil || got != want {
			t.Errorf("cellText(%q) = %q %v, want %q", in, got, err, want)
		}
	}
	if got, _ := cellText(int64(42)); got != "42" {
		t.Errorf("number cell = %q", got)
	}
	if _, err := cellText(`[{"a":1}, 5]`); err != nil {
		t.Errorf("only the first element is inspected: %v", err)
	}
}

func TestPayloadParsingAndScopeValidation(t *testing.T) {
	p, err := parsePayload([]byte(`{"workspace_id":"w","workbook_id":"b","concurrency":500,"retry_passes":-3,
		"provider_timeout":"x","row_ids":[1,2],"row_columns":{"1":["a"],"2":[]},"fill_missing":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if p.Concurrency != MaxConcurrency || p.RetryPasses != 0 || p.ProviderTimeout != DefaultProviderTimeout ||
		len(p.RowIDs) != 2 || p.ColumnIDs != nil || len(p.RowColumns[2]) != 0 || !p.FillMissing {
		t.Errorf("payload = %+v", p)
	}
	for _, bad := range []string{
		`[]`, `{"workspace_id":"w"}`, `{"workspace_id":"w","workbook_id":"b","row_columns":{"01":["a"]}}`,
		`{"workspace_id":"w","workbook_id":"b","row_columns":{"1":"a"}}`, `{"workspace_id":"w","workbook_id":"b","row_columns":{"1":[""]}}`,
		`{"workspace_id":"w","workbook_id":"b","row_ids":["1"]}`, `{"workspace_id":"w","workbook_id":"b","column_ids":[1]}`,
	} {
		if _, err := parsePayload([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	// empty list is "no rows", absent is "all rows"
	p, _ = parsePayload([]byte(`{"workspace_id":"w","workbook_id":"b","row_ids":[]}`))
	if p.RowIDs == nil || len(p.RowIDs) != 0 {
		t.Error("an empty row_ids list must stay non-nil (no rows)")
	}
}

func TestHydrationInvalidatesFailedSnapshotsAndPrefersStableIDs(t *testing.T) {
	cols := []*column{}
	for _, c := range []*pycompat.Map{
		obj("id", "email", "name", "Email", "type", "enrichment"),
		obj("id", "phone", "name", "Phone", "type", "enrichment"),
	} {
		col, _ := newColumn(c)
		cols = append(cols, col)
	}
	data := `{"company":"Acme","email":"stale@x","Email":"stale@x","Phone":"old-alias","note":"keep"}`
	enr := `{"email":{"status":"error","value":null,"error":"timeout"},"phone":{"status":"complete","value":"+1 555"}}`
	lead, err := hydrate(9, nil, []byte(data), []byte(enr), cols)
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]any{"company": "Acme", "note": "keep", "phone": "+1 555", "Phone": "+1 555", "id": int64(9), "__row_id": int64(9), "__lead_id": nil} {
		got, ok := lead.Get(k)
		if !ok || got != want {
			t.Errorf("%s = %v (%v), want %v", k, got, ok, want)
		}
	}
	for _, k := range []string{"email", "Email"} {
		if _, ok := lead.Get(k); ok {
			t.Errorf("a failed cell's stale copy %q survived", k)
		}
	}
	linked := int64(77)
	lead, _ = hydrate(9, &linked, []byte(`{}`), []byte(`null`), cols)
	if id, _ := lead.Get("id"); id != int64(77) {
		t.Errorf("id of a linked row = %v", id)
	}
}

func TestRateLimitHeuristicAndCatalog(t *testing.T) {
	for in, want := range map[string]bool{"HTTP 429": true, "Rate Limit hit": true, "quota exceeded": true, "boom": false} {
		if looksRateLimited(in) != want {
			t.Errorf("looksRateLimited(%q) != %v", in, want)
		}
	}
	if baseCost("leadmagic_email", nil) != 0.05 || baseCost("anything", nil) != 0 {
		t.Error("built-in vendor prices")
	}
}
