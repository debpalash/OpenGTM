package workbook

import (
	"errors"
	"strings"
	"testing"
)

const manifestJSON = `{"name":"hunter","capability":"email","auth":{"type":"none"},"request":{"url":"http://x/y"},"response":{"mappings":{"email":"$.email"}}}`

func payload(extra, goBlock string) []byte {
	return []byte(`{"workspace_id":"ws","workbook_id":"wb"` + extra + `,"go":` + goBlock + `}`)
}

func goOK(version string) string {
	return `{"version":` + version + `,"manifests":{"hunter":` + manifestJSON + `},"manifest_sha256":{"hunter":"ab"},"costs":{"hunter":0.05},"default_concurrency":7}`
}

func TestParsePayloadRefusesOtherVersions(t *testing.T) {
	for _, v := range []string{"0", "2", "99"} {
		if _, err := parsePayload(payload("", goOK(v))); !errors.Is(err, ErrUnsupportedPayload) {
			t.Fatalf("version %s: got %v", v, err)
		}
	}
	if _, err := parsePayload([]byte(`{"workspace_id":"ws","workbook_id":"wb"}`)); !errors.Is(err, ErrUnsupportedPayload) {
		t.Fatalf("missing go block: %v", err)
	}
}

func TestParsePayloadRequiresManifestDigest(t *testing.T) {
	bad := strings.Replace(goOK("1"), `"hunter":"ab"`, `"other":"ab"`, 1)
	if _, err := parsePayload(payload("", bad)); !errors.Is(err, ErrUnsupportedPayload) {
		t.Fatalf("got %v", err)
	}
}

func TestParsePayloadDefaultsAndScope(t *testing.T) {
	p, err := parsePayload(payload("", goOK("1")))
	if err != nil {
		t.Fatal(err)
	}
	if p.Concurrency != 7 || p.RetryPasses != 1 || p.ProviderTimeout != 10 || p.ColumnIDs != nil || p.HasRowIDs {
		t.Fatalf("defaults: %+v", p)
	}
	p, err = parsePayload(payload(`,"concurrency":3,"retry_passes":0,"column_ids":["a"],"row_ids":[],"fill_missing":true,"provider_timeout":2.5,"row_columns":{"5":["a"]}`, goOK("1")))
	if err != nil {
		t.Fatal(err)
	}
	if p.Concurrency != 3 || p.RetryPasses != 0 || !p.ColumnIDs["a"] || !p.HasRowIDs || len(p.RowIDs) != 0 || !p.FillMissing || p.ProviderTimeout != 2.5 || len(p.RowColumns["5"]) != 1 {
		t.Fatalf("parsed: %+v", p)
	}
	if p.Costs["hunter"] != 0.05 || p.Manifests["hunter"].Name != "hunter" {
		t.Fatalf("manifests/costs: %+v", p)
	}
}

func TestParsePayloadRejectsBadScope(t *testing.T) {
	for _, extra := range []string{
		`,"row_columns":{"0":["a"]}`, `,"row_columns":{"01":["a"]}`, `,"row_columns":{"x":["a"]}`,
		`,"row_columns":{"1":[""]}`, `,"row_columns":{"1":"a"}`, `,"provider_timeout":0`, `,"provider_timeout":-3`,
	} {
		if _, err := parsePayload(payload(extra, goOK("1"))); err == nil {
			t.Fatalf("accepted %s", extra)
		}
	}
}

func TestRowExecutionDataHydration(t *testing.T) {
	cols, err := parseColumns([]byte(`[{"id":"company","type":"lead_field","name":"Company"},
	  {"id":"email","type":"waterfall","name":"Email"},{"id":"phone","type":"waterfall","name":"Phone"}]`))
	if err != nil {
		t.Fatal(err)
	}
	lead := int64(9)
	row, err := rowExecutionData(4, &lead,
		[]byte(`{"company":"Acme","email":"stale@x.io","phone":"+1","extra":1}`),
		[]byte(`{"email":{"status":"error","value":null},"phone":{"status":"complete","value":"+15550001"}}`), cols)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := row["email"]; ok {
		t.Fatalf("failed cell must invalidate stale data: %v", row)
	}
	if row["phone"] != "+15550001" || row["Phone"] != "+15550001" || row["Company"] != "Acme" {
		t.Fatalf("overlay/aliases wrong: %v", row)
	}
	if row["id"] != int64(9) || row["__row_id"] != int64(4) || row["__lead_id"] != int64(9) {
		t.Fatalf("identities: %v", row)
	}
	row, _ = rowExecutionData(4, nil, []byte(`{}`), nil, cols)
	if row["id"] != int64(4) || row["__lead_id"] != nil {
		t.Fatalf("row-only identity: %v", row)
	}
}

func TestColumnChainSelection(t *testing.T) {
	cols, _ := parseColumns([]byte(`[{"id":"a","waterfall":["x","y"]},{"id":"b","provider":"z"},{"id":"c","waterfall":[]},{"id":"d","target_field":"email"}]`))
	if ch, ex := cols[0].explicitChain(); !ex || len(ch) != 2 {
		t.Fatal("waterfall")
	}
	if ch, ex := cols[1].explicitChain(); !ex || ch[0] != "z" {
		t.Fatal("provider")
	}
	if ch, ex := cols[2].explicitChain(); !ex || len(ch) != 0 {
		t.Fatal("empty waterfall is explicit")
	}
	if _, ex := cols[3].explicitChain(); ex || cols[3].target() != "email" || cols[0].target() != "a" {
		t.Fatal("default")
	}
}

func TestLooksRateLimited(t *testing.T) {
	for s, want := range map[string]bool{"HTTP 429": true, "Rate Limit hit": true, "quota exceeded": true, "boom": false, "": false} {
		if looksRateLimited(s) != want {
			t.Fatalf("%q", s)
		}
	}
}

func TestParsePayloadClampsKnobs(t *testing.T) {
	cases := []struct {
		extra                string
		conc, retry, maxProv int
		timeout              float64
	}{
		{`,"concurrency":1000000000000,"retry_passes":1e12,"max_providers":1e30,"provider_timeout":1e12`, 64, 5, 1 << 20, 120},
		{`,"concurrency":-1,"retry_passes":-1,"max_providers":-5`, 1, 0, 0, 10},
		{`,"concurrency":0`, 1, 1, 0, 10},
		{`,"concurrency":2.9,"retry_passes":2.5,"provider_timeout":0.5`, 2, 2, 0, 0.5},
		{``, 7, 1, 0, 10},
	}
	for _, c := range cases {
		p, err := parsePayload(payload(c.extra, goOK("1")))
		if err != nil {
			t.Fatalf("%s: %v", c.extra, err)
		}
		if p.Concurrency != c.conc || p.RetryPasses != c.retry || p.MaxProviders != c.maxProv || p.ProviderTimeout != c.timeout {
			t.Fatalf("%s: got %d %d %d %v", c.extra, p.Concurrency, p.RetryPasses, p.MaxProviders, p.ProviderTimeout)
		}
	}
	// default_concurrency from the go block is clamped too
	p, err := parsePayload(payload("", strings.Replace(goOK("1"), `"default_concurrency":7`, `"default_concurrency":9999`, 1)))
	if err != nil || p.Concurrency != 64 {
		t.Fatalf("go default: %v %v", p, err)
	}
}
