package workbook

import (
	"encoding/json"
	"testing"
	"time"
)

// The run's receipt carries the cells by outcome so the API can say "86 filled, 89 no data, 25 need setup".
func TestReceiptCarriesOutcomeCounts(t *testing.T) {
	payload := []byte(`{"workbook_id":"wb1"}`)
	out, ok, err := mergeReceipt(payload, "wb1", Result{Completed: 86, NoData: 89, NeedsSetup: 25, Errors: 3, Total: 203, Rows: 25}, 0, time.Now())
	if err != nil || !ok {
		t.Fatal(err, ok)
	}
	var doc struct {
		R map[string]any `json:"execution_result"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]float64{"completed": 86, "no_data": 89, "needs_setup": 25, "errors": 3} {
		if doc.R[k] != want {
			t.Errorf("execution_result.%s = %v, want %v", k, doc.R[k], want)
		}
	}
}
