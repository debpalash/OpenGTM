package config

import "testing"

func wbGet(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// GO_EXECUTOR_FULL_RUN is retired (WP-CUTOVER): always on. PROVENANCE_TRACKING_ENABLED is still read.
func TestLoadWbExec(t *testing.T) {
	w := LoadWbExec(wbGet(nil))
	if !w.FullRun || w.ProvenanceTracking || len(w.Problems) != 0 {
		t.Fatalf("defaults: %+v", w)
	}
	w = LoadWbExec(wbGet(map[string]string{"GO_EXECUTOR_FULL_RUN": "false", "PROVENANCE_TRACKING_ENABLED": "1"}))
	if !w.FullRun || !w.ProvenanceTracking || len(w.Problems) != 0 {
		t.Fatalf("on: %+v", w)
	}
}

// Review L1/L2: a malformed switch never stops a process and reads as off everywhere.
func TestLoadWbExecMalformedIsOffAndReported(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		prov bool
		bad  int
	}{
		{"bad provenance", map[string]string{"PROVENANCE_TRACKING_ENABLED": "maybe"}, false, 1},
		{"retired full run ignored", map[string]string{"GO_EXECUTOR_FULL_RUN": "maybe", "PROVENANCE_TRACKING_ENABLED": "1"}, true, 0},
		{"spellings", map[string]string{"PROVENANCE_TRACKING_ENABLED": "T"}, true, 0},
		{"false spellings", map[string]string{"PROVENANCE_TRACKING_ENABLED": "OFF"}, false, 0},
	}
	for _, c := range cases {
		w := LoadWbExec(wbGet(c.env))
		if !w.FullRun || w.ProvenanceTracking != c.prov || len(w.Problems) != c.bad {
			t.Errorf("%s: %+v", c.name, w)
		}
	}
}
