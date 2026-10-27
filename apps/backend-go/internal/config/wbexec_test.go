package config

import "testing"

func TestLoadWbExec(t *testing.T) {
	get := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	w, err := LoadWbExec(get(nil))
	if err != nil || w.FullRun || w.ProvenanceTracking {
		t.Fatalf("defaults must be off: %+v %v", w, err)
	}
	w, err = LoadWbExec(get(map[string]string{"GO_EXECUTOR_FULL_RUN": "true", "PROVENANCE_TRACKING_ENABLED": "1"}))
	if err != nil || !w.FullRun || !w.ProvenanceTracking {
		t.Fatalf("on: %+v %v", w, err)
	}
	if _, err := LoadWbExec(get(map[string]string{"GO_EXECUTOR_FULL_RUN": "maybe"})); err == nil {
		t.Fatal("a malformed flag must fail startup")
	}
}
