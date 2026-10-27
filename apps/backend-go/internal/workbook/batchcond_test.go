package workbook

import "testing"

// Review wave 2b M5: a condition-gated ai_formula column never enters the batch pre-pass. The
// per-cell path evaluates the condition (preRunGates) and skips the cell; the pre-pass writes
// complete cells without that gate, so a gated column must stay on the per-cell path.
func TestBatchEligibleExcludesConditionedColumns(t *testing.T) {
	cols := mustColumns(t, `[
 {"id":"company","type":"lead_field","name":"Company"},
 {"id":"plain","type":"ai_formula","prompt":"Summarize {company}"},
 {"id":"gated","type":"ai_formula","prompt":"Pitch {company}","condition":{"column":"company","op":"is_not_empty"}},
 {"id":"gated_str","type":"ai_formula","prompt":"Pitch","condition":"company == 'Acme'"},
 {"id":"falsy_cond","type":"ai_formula","prompt":"Hi","condition":""}]`)
	rs := &runState{cols: cols}
	got := rs.batchEligible()
	if !got["plain"] || !got["falsy_cond"] {
		t.Fatalf("unconditioned ai_formula columns must stay eligible: %v", got)
	}
	if got["gated"] || got["gated_str"] {
		t.Fatalf("condition-gated ai_formula columns must not be batched: %v", got)
	}
}
