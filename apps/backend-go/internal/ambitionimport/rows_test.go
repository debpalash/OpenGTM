package ambitionimport_test

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ambitionimport"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

func TestCompanyToRowMatchesVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/ambitionbox_rows.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Columns []json.RawMessage `json:"AMBITIONBOX_COLUMNS"`
		Rows    []struct {
			Name    string          `json:"name"`
			Company json.RawMessage `json:"company"`
			Value   json.RawMessage `json:"value"`
		} `json:"company_to_row"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Rows) == 0 {
		t.Fatal("no vectors")
	}
	for _, c := range v.Rows {
		in, err := wbcrud.ParseObject(c.Company)
		if err != nil {
			t.Fatal(err)
		}
		got, err := json.Marshal(ambitionimport.CompanyToRow(in))
		if err != nil {
			t.Fatal(err)
		}
		// Compare as ordered documents: re-parse the expectation to canonical order.
		want, _ := wbcrud.ParseObject(c.Value)
		wb, _ := json.Marshal(want)
		if string(got) != string(wb) {
			t.Errorf("%s:\n got %s\nwant %s", c.Name, got, wb)
		}
	}
	// Columns are verbatim (order and values).
	cols, err := json.Marshal(ambitionimport.Columns())
	if err != nil {
		t.Fatal(err)
	}
	var want []json.RawMessage = v.Columns
	wantB, _ := json.Marshal(want)
	var a, b any
	_ = json.Unmarshal(cols, &a)
	_ = json.Unmarshal(wantB, &b)
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	if string(ab) != string(bb) {
		t.Fatalf("columns differ:\n%s\n%s", ab, bb)
	}
}
