package wbcrud

import (
	"testing"
	"time"
)

// Expected JSON produced by pydantic WorkbookResponse.model_dump_json() for the
// same values (legacy/python, 2026-10-26): key order, naive timestamps, stored
// unknown column keys and source_config order preserved.
const wantWorkbookJSON = `{"id":"w1","name":"N","description":"","status":"draft","source_type":"leads_filter","source_config":{"row_storage_version":2,"b":1.5},"filter_criteria":null,"columns_config":[{"id":"c1","name":"X","type":"lead_field","width":200,"zz":1}],"total_rows":3,"completed_rows":0,"sync_to_leads":true,"created_at":"2026-10-26T10:00:00.123456","updated_at":"2026-10-26T10:00:00","last_run_at":null}`

func TestWorkbookJSONShapeMatchesFixture(t *testing.T) {
	c1 := time.Date(2026, 10, 26, 10, 0, 0, 123456000, time.UTC)
	u := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	row := rowFixture("w1", c1, u)
	w, err := FromRow(row)
	if err != nil {
		t.Fatal(err)
	}
	w.TotalRows = 3
	b, err := w.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != wantWorkbookJSON {
		t.Fatalf("got  %s\nwant %s", b, wantWorkbookJSON)
	}
}

func TestFromRowDefaults(t *testing.T) {
	w, err := FromRow(nullRowFixture())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := w.MarshalJSON()
	want := `{"id":"w2","name":"N","description":"","status":"draft","source_type":"leads_filter","source_config":{},"filter_criteria":null,"columns_config":[],"total_rows":0,"completed_rows":0,"sync_to_leads":true,"created_at":null,"updated_at":null,"last_run_at":null}`
	if string(b) != want {
		t.Fatalf("got  %s\nwant %s", b, want)
	}
}

func TestUsesLegacyLeads(t *testing.T) {
	cases := []struct {
		src, cfg string
		want     bool
	}{
		{"leads_filter", `{}`, true},
		{"leads_filter", `{"row_storage_version":2}`, false},
		{"leads_filter", `{"row_storage_version":1}`, true},
		{"csv", `{}`, false},
		{"empty", `{"row_storage_version":2}`, false},
	}
	for _, c := range cases {
		w := Workbook{SourceType: c.src, SourceConfig: jsonValue([]byte(c.cfg))}
		if got := w.UsesLegacyLeads(); got != c.want {
			t.Errorf("%s %s: got %v", c.src, c.cfg, got)
		}
	}
}

func TestNewIDFormat(t *testing.T) {
	id := NewID()
	if len(id) != 36 || id[14] != '4' || id == NewID() {
		t.Fatalf("bad id %q", id)
	}
}
