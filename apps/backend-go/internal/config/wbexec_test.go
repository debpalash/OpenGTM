package config

import "testing"

func wbGet(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestLoadWbExec(t *testing.T) {
	w := LoadWbExec(wbGet(nil))
	if w.FullRun || w.ProvenanceTracking || len(w.Problems) != 0 {
		t.Fatalf("defaults must be off: %+v", w)
	}
	w = LoadWbExec(wbGet(map[string]string{"GO_EXECUTOR_FULL_RUN": "true", "PROVENANCE_TRACKING_ENABLED": "1"}))
	if !w.FullRun || !w.ProvenanceTracking || len(w.Problems) != 0 {
		t.Fatalf("on: %+v", w)
	}
}

// Review L1/L2: a malformed switch never stops a process and reads as off everywhere.
func TestLoadWbExecMalformedIsOffAndReported(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		full bool
		prov bool
		bad  int
	}{
		{"bad provenance, full run off", map[string]string{"PROVENANCE_TRACKING_ENABLED": "maybe"}, false, false, 1},
		{"bad provenance, full run on", map[string]string{"GO_EXECUTOR_FULL_RUN": "1", "PROVENANCE_TRACKING_ENABLED": "maybe"}, true, false, 1},
		{"bad full run", map[string]string{"GO_EXECUTOR_FULL_RUN": "maybe", "PROVENANCE_TRACKING_ENABLED": "1"}, false, true, 1},
		{"both bad", map[string]string{"GO_EXECUTOR_FULL_RUN": "x", "PROVENANCE_TRACKING_ENABLED": "z"}, false, false, 2},
		{"spellings", map[string]string{"GO_EXECUTOR_FULL_RUN": " On ", "PROVENANCE_TRACKING_ENABLED": "T"}, true, true, 0},
		{"false spellings", map[string]string{"GO_EXECUTOR_FULL_RUN": "no", "PROVENANCE_TRACKING_ENABLED": "OFF"}, false, false, 0},
	}
	for _, c := range cases {
		w := LoadWbExec(wbGet(c.env))
		if w.FullRun != c.full || w.ProvenanceTracking != c.prov || len(w.Problems) < c.bad {
			t.Errorf("%s: %+v", c.name, w)
		}
	}
}
