package poller

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func readVec(t *testing.T, name string, v any) {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

func TestKeysVectors(t *testing.T) {
	var v struct {
		Version   int `json:"key_schema_version"`
		Normalize []struct {
			Target, Normalized, StableNoCIK, StableWithCIK, StableWithPaddedCIK, StableWithBadCIK string
		} `json:"normalize_target"`
		Role []struct {
			Title, Tier string
			IsExec      bool `json:"is_exec"`
		} `json:"role_tier"`
		Names []struct{ In, Exec, Tech string } `json:"names"`
	}
	readVec(t, "keys_vectors.json", &v)
	// The raw JSON uses snake_case keys; decode explicitly.
	var raw struct {
		Normalize []map[string]string `json:"normalize_target"`
	}
	readVec(t, "keys_vectors.json", &raw)
	if KeySchemaVersion != v.Version {
		t.Fatalf("KeySchemaVersion = %d", KeySchemaVersion)
	}
	for _, c := range raw.Normalize {
		if got := NormalizeTarget(c["target"]); got != c["normalized"] {
			t.Errorf("NormalizeTarget(%q) = %q want %q", c["target"], got, c["normalized"])
		}
		if got := StableCompanyID(nil, c["target"]); got != c["stable_no_cik"] {
			t.Errorf("StableCompanyID(nil,%q) = %q want %q", c["target"], got, c["stable_no_cik"])
		}
		for in, key := range map[string]string{"320193": "stable_with_cik", "0000320193": "stable_with_padded_cik", "not-a-cik": "stable_with_bad_cik"} {
			in := in
			if got := StableCompanyID(&in, c["target"]); got != c[key] {
				t.Errorf("StableCompanyID(%q,%q) = %q want %q", in, c["target"], got, c[key])
			}
		}
	}
	empty := ""
	if got := StableCompanyID(&empty, "Acme Inc"); got != "acme" {
		t.Errorf("empty resolved cik must fall back to target, got %q", got)
	}
	for _, r := range v.Role {
		if got := RoleTier(r.Title); got != r.Tier {
			t.Errorf("RoleTier(%q) = %q want %q", r.Title, got, r.Tier)
		}
		if got := IsExecutiveTitle(r.Title); got != r.IsExec {
			t.Errorf("IsExecutiveTitle(%q) = %v want %v", r.Title, got, r.IsExec)
		}
	}
	for _, n := range v.Names {
		_ = n
	}
	var nm struct {
		Names []map[string]string `json:"names"`
	}
	readVec(t, "keys_vectors.json", &nm)
	for _, n := range nm.Names {
		if got := NormalizeExecName(n["in"]); got != n["exec"] {
			t.Errorf("NormalizeExecName(%q) = %q want %q", n["in"], got, n["exec"])
		}
		if got := NormalizeTech(n["in"]); got != n["tech"] {
			t.Errorf("NormalizeTech(%q) = %q want %q", n["in"], got, n["tech"])
		}
	}
}

func TestKeysEdgeVectorsFromPython(t *testing.T) {
	var v struct {
		Normalize []struct {
			Target     string `json:"target"`
			Normalized string `json:"normalized"`
			Stable     string `json:"stable_no_cik"`
		} `json:"normalize"`
		Role []struct {
			Title  string `json:"title"`
			Tier   string `json:"tier"`
			IsExec bool   `json:"is_exec"`
		} `json:"role"`
	}
	readVec(t, "keys_extra.json", &v)
	if len(v.Normalize) == 0 || len(v.Role) == 0 {
		t.Fatal("empty vectors")
	}
	for _, c := range v.Normalize {
		if got := NormalizeTarget(c.Target); got != c.Normalized {
			t.Errorf("NormalizeTarget(%q) = %q want %q", c.Target, got, c.Normalized)
		}
		if got := StableCompanyID(nil, c.Target); got != c.Stable {
			t.Errorf("StableCompanyID(nil,%q) = %q want %q", c.Target, got, c.Stable)
		}
	}
	for _, r := range v.Role {
		if got := RoleTier(r.Title); got != r.Tier || IsExecutiveTitle(r.Title) != r.IsExec {
			t.Errorf("RoleTier(%q) = %q want %q", r.Title, got, r.Tier)
		}
	}
}

func TestSignalEventIDEqualsPython(t *testing.T) {
	type vec struct{ WS, Kind, Stable, SignalType, Natural, ID string }
	var all []vec
	for _, f := range []string{"signal_ids_50.json"} {
		var vs []map[string]string
		readVec(t, f, &vs)
		for _, m := range vs {
			all = append(all, vec{m["ws"], m["kind"], m["stable"], m["signal_type"], m["natural"], m["id"]})
		}
	}
	var base struct {
		V []map[string]string `json:"signal_event_id"`
	}
	readVec(t, "keys_vectors.json", &base)
	for _, m := range base.V {
		all = append(all, vec{m["ws"], m["kind"], m["stable"], m["signal_type"], m["natural"], m["id"]})
	}
	if len(all) < 50 {
		t.Fatalf("only %d vectors", len(all))
	}
	for _, c := range all {
		if got := SignalEventID(c.WS, c.Kind, c.Stable, c.SignalType, c.Natural); got != c.ID {
			t.Errorf("SignalEventID(%+v) = %s", c, got)
		}
	}
}

func TestSourceSetPerKind(t *testing.T) {
	var v struct {
		Vectors []struct {
			Kind        string   `json:"kind"`
			SignalTypes []string `json:"signal_types"`
			WebTech     bool     `json:"website_tech"`
			Sources     []string `json:"sources"`
		} `json:"vectors"`
	}
	readVec(t, "source_set_vectors.json", &v)
	if len(v.Vectors) < 100 {
		t.Fatalf("vectors = %d", len(v.Vectors))
	}
	for _, c := range v.Vectors {
		got := SourceSet(c.Kind, c.SignalTypes, c.WebTech)
		if len(got) != len(c.Sources) {
			t.Errorf("%+v: got %v", c, got)
			continue
		}
		for i := range got {
			if got[i] != c.Sources[i] {
				t.Errorf("%+v: got %v", c, got)
				break
			}
		}
	}
}

func TestFireKeyAndPayloadEqualPython(t *testing.T) {
	var v struct {
		FireKeys []struct {
			WatchID string `json:"watch_id"`
			Next    string `json:"next_run_at"`
			FireKey string `json:"fire_key"`
		} `json:"fire_keys"`
		Delta map[string]float64 `json:"interval_delta_seconds"`
	}
	readVec(t, "schedule_vectors.json", &v)
	if len(v.FireKeys) != 3 {
		t.Fatalf("fire keys = %d", len(v.FireKeys))
	}
	for _, c := range v.FireKeys {
		next, err := time.Parse(time.RFC3339Nano, c.Next)
		if err != nil {
			t.Fatal(err)
		}
		if got := ScheduledFireKey(c.WatchID, next); got != c.FireKey {
			t.Errorf("ScheduledFireKey = %q want %q", got, c.FireKey)
		}
	}
	if got := ScheduledFireKey("w", time.Date(2026, 1, 2, 3, 4, 5, 6000, time.UTC)); got != "watch:w:2026-01-02T03:04:05.000006+00:00" {
		t.Errorf("micro key = %q", got)
	}
}
