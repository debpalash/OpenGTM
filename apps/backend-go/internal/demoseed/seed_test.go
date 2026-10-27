package demoseed_test

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/demoseed"
)

// seed_demo_vectors.json is recorded from legacy seed_demo.py (testdata/gen_vectors.py.txt): the demo
// workbook, its 25 rows and the column ids of the first-run job.
type vectors struct {
	CreatedFirst  bool `json:"created_first"`
	CreatedSecond bool `json:"created_second"`
	Workbooks     []struct {
		ID            string          `json:"id"`
		Name          string          `json:"name"`
		Description   string          `json:"description"`
		Status        string          `json:"status"`
		SourceType    string          `json:"source_type"`
		ColumnsConfig json.RawMessage `json:"columns_config"`
		TotalRows     int             `json:"total_rows"`
		CompletedRows int             `json:"completed_rows"`
		SyncToLeads   bool            `json:"sync_to_leads"`
		BudgetMax     float64         `json:"budget_max_usd"`
		BudgetSpent   float64         `json:"budget_spent_usd"`
	} `json:"workbooks"`
	Rows []struct {
		Position    int               `json:"position"`
		Data        map[string]string `json:"data"`
		Enrichments map[string]any    `json:"enrichments"`
	} `json:"rows"`
	Jobs []struct {
		Payload struct {
			ColumnIDs []string `json:"column_ids"`
		} `json:"payload"`
	} `json:"jobs"`
}

func load(t *testing.T) vectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/seed_demo_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func canon(t *testing.T, raw []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestDemoWorkbookMatchesPython(t *testing.T) {
	v := load(t)
	wb := v.Workbooks[0]
	if demoseed.WorkbookID != wb.ID || demoseed.WorkbookName != wb.Name || demoseed.WorkbookDescription != wb.Description {
		t.Fatalf("identity = %q %q %q", demoseed.WorkbookID, demoseed.WorkbookName, demoseed.WorkbookDescription)
	}
	if got, want := canon(t, demoseed.ColumnsJSON()), canon(t, wb.ColumnsConfig); !reflect.DeepEqual(got, want) {
		t.Fatalf("columns differ\n go:     %v\n python: %v", got, want)
	}
	cs := demoseed.Companies()
	if len(cs) != wb.TotalRows || len(cs) != len(v.Rows) {
		t.Fatalf("%d companies, Python seeded %d rows", len(cs), len(v.Rows))
	}
	for i, r := range v.Rows {
		if r.Position != i || cs[i].Name != r.Data["company"] || cs[i].Domain != r.Data["website"] || len(r.Enrichments) != 0 {
			t.Fatalf("row %d: %+v vs %+v", i, cs[i], r)
		}
	}
	if got, want := demoseed.EnrichmentColumnIDs(), v.Jobs[0].Payload.ColumnIDs; !reflect.DeepEqual(got, want) {
		t.Fatalf("run column ids\n go:     %v\n python: %v", got, want)
	}
}
