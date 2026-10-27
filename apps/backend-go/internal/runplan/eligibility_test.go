package runplan

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
)

type eligVectors struct {
	Manifests map[string]json.RawMessage `json:"manifests"`
	Cases     []struct {
		Name  string `json:"name"`
		Flags struct {
			Enabled    *bool `json:"enabled"`
			Cohort     *bool `json:"cohort"`
			Provenance bool  `json:"provenance"`
			Automation bool  `json:"automations"`
		} `json:"flags"`
		Columns     []json.RawMessage `json:"columns"`
		WorkspaceID string            `json:"workspace_id"`
		ColumnIDs   []string          `json:"column_ids"`
		OK          bool              `json:"ok"`
		Reason      string            `json:"reason"`
	} `json:"cases"`
}

func loadEligVectors(t testing.TB) eligVectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/eligibility_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v eligVectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func indexOf(t testing.TB, ms map[string]json.RawMessage) map[string]manifest.Entry {
	t.Helper()
	idx := map[string]manifest.Entry{}
	for name, raw := range ms {
		e, err := manifest.Decode(raw, name)
		if err != nil {
			t.Fatalf("manifest %s: %v", name, err)
		}
		idx[name] = e
	}
	return idx
}

// TestEligibilityMatrix replays Python's go_executor.go_eligibility answers.
func TestEligibilityMatrix(t *testing.T) {
	v := loadEligVectors(t)
	idx := indexOf(t, v.Manifests)
	if len(v.Cases) < 30 {
		t.Fatalf("vector file shrank: %d cases", len(v.Cases))
	}
	for _, c := range v.Cases {
		var all []colgraph.Col
		for _, raw := range c.Columns {
			col, err := colgraph.ParseCol(raw)
			if err != nil {
				t.Fatal(err)
			}
			all = append(all, col)
		}
		cfg := Config{
			WorkspaceID:        c.WorkspaceID,
			GoExecutorEnabled:  c.Flags.Enabled == nil || *c.Flags.Enabled,
			GoCohort:           map[string]bool{"ws-1": c.Flags.Cohort == nil || *c.Flags.Cohort, "ws-2": true},
			ProvenanceTracking: c.Flags.Provenance,
			AutomationsEnabled: c.Flags.Automation,
		}
		if c.Flags.Cohort != nil && !*c.Flags.Cohort {
			cfg.GoCohort = map[string]bool{"ws-9": true}
		}
		ok, reason := Eligible(colsByID(all, c.ColumnIDs), idx, cfg)
		if ok != c.OK || reason != c.Reason {
			t.Errorf("%s: got (%v, %q) want (%v, %q)", c.Name, ok, reason, c.OK, c.Reason)
		}
	}
}

// GO_EXECUTOR_COLUMN_TYPES extends the executor's column types; computed and
// python-run columns skip the provider-chain checks, structured targets are
// written back by Go, and the default (flag unset) keeps the Python answers.
func TestEligibilityExtraColumnTypes(t *testing.T) {
	cols := func(s string) []colgraph.Col {
		c, err := colgraph.ParseCols([]byte(s))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	cfg := Config{WorkspaceID: "w", GoExecutorEnabled: true, GoCohort: map[string]bool{"w": true}}
	formula := cols(`[{"id":"f","type":"formula","formula":"upper({company})"}]`)
	if ok, why := Eligible(formula, nil, cfg); ok || why != ReasonColumnType {
		t.Fatalf("default must stay on python: %v %s", ok, why)
	}
	cfg.ExtraColumnTypes = map[string]bool{"formula": true, "http": true, "ai_formula": true, "research": true, "agent": true}
	for _, s := range []string{
		`[{"id":"f","type":"formula","formula":"upper({company})"}]`,
		`[{"id":"h","type":"http","http_url":"https://api.example.com/x"}]`,
		`[{"id":"a","type":"ai_formula","prompt":"describe"}]`,
		`[{"id":"r","type":"research","prompt":"who"}]`,
		`[{"id":"g","type":"agent","goal":"find"}]`,
	} {
		if ok, why := Eligible(cols(s), nil, cfg); !ok {
			t.Fatalf("%s: %s", s, why)
		}
	}
	// output columns never move, with or without the flag.
	if ok, why := Eligible(cols(`[{"id":"o","type":"output"}]`), nil, cfg); ok || why != ReasonColumnType {
		t.Fatalf("output: %v %s", ok, why)
	}
}

// GO_RUN_NATIVE_PROVIDERS (Go-only, default off = Python parity): a chain whose
// providers are not in the manifest index (Go-native or pyworker) may run on Go.
func TestEligibilityNativeProviders(t *testing.T) {
	cols, err := colgraph.ParseCols([]byte(`[{"id":"e","type":"enrichment","target_field":"description","waterfall":["not_a_manifest_provider"]}]`))
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{WorkspaceID: "w", GoExecutorEnabled: true, GoCohort: map[string]bool{"w": true}}
	if ok, why := Eligible(cols, nil, cfg); ok || why != ReasonNonDeclarative {
		t.Fatalf("default must hand back: %v %s", ok, why)
	}
	cfg.NativeProviders = true
	if ok, why := Eligible(cols, nil, cfg); !ok {
		t.Fatalf("flag on must allow: %s", why)
	}
	// a declarative provider is still checked as before.
	idx := map[string]manifest.Entry{"not_a_manifest_provider": {}}
	if ok, why := Eligible(cols, idx, cfg); ok || why != ReasonMappingNotTargetOnly {
		t.Fatalf("manifest checks stay: %v %s", ok, why)
	}
}

// GO_EXECUTOR_FULL_RUN (plan 15 WP-WBEXEC, default off): the Go executor writes provenance,
// emits automations, evaluates conditions, verifies emails, delivers output columns and
// writes structured targets back, so those refusals close. Every other answer is Python's.
func TestEligibilityFullRun(t *testing.T) {
	v := loadEligVectors(t)
	idx := indexOf(t, v.Manifests)
	closed := map[string]bool{"email_verify_default": true, "email_verify_true": true, "provenance_on": true,
		"automations_on": true, "column_type_formula": true, "condition": true, "refs_prompt": true,
		"refs_input_columns": true, "refs_formula_key": true, "structured_target": true}
	for _, c := range v.Cases {
		var all []colgraph.Col
		for _, raw := range c.Columns {
			col, err := colgraph.ParseCol(raw)
			if err != nil {
				t.Fatal(err)
			}
			all = append(all, col)
		}
		cfg := Config{WorkspaceID: c.WorkspaceID, FullRun: true,
			GoExecutorEnabled:  c.Flags.Enabled == nil || *c.Flags.Enabled,
			GoCohort:           map[string]bool{"ws-1": c.Flags.Cohort == nil || *c.Flags.Cohort, "ws-2": true},
			ProvenanceTracking: c.Flags.Provenance, AutomationsEnabled: c.Flags.Automation}
		if c.Flags.Cohort != nil && !*c.Flags.Cohort {
			cfg.GoCohort = map[string]bool{"ws-9": true}
		}
		wantOK, wantReason := c.OK, c.Reason
		if closed[c.Name] {
			wantOK, wantReason = true, ReasonOK
		}
		if c.Name == "structured_target" { // the next check answers: its provider maps more than the target
			wantOK, wantReason = false, ReasonMappingNotTargetOnly
		}
		ok, reason := Eligible(colsByID(all, c.ColumnIDs), idx, cfg)
		if ok != wantOK || reason != wantReason {
			t.Errorf("%s: got (%v, %q) want (%v, %q)", c.Name, ok, reason, wantOK, wantReason)
		}
	}
	cols, err := colgraph.ParseCols([]byte(`[{"id":"e","type":"enrichment","provider":"m_desc","target_field":"description","condition":"{company} != \"\""},
	  {"id":"f","type":"formula","formula":"{e}"},{"id":"h","type":"http","http_url":"https://api.example.com/{e}"},
	  {"id":"o","type":"output","destination":"webhook","destination_config":{"url":"https://h.example.com"}}]`))
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{WorkspaceID: "w", GoExecutorEnabled: true, GoCohort: map[string]bool{"w": true}, FullRun: true,
		ProvenanceTracking: true, AutomationsEnabled: true}
	if ok, why := Eligible(cols, idx, cfg); !ok {
		t.Fatalf("full run must take output/formula/http/conditions: %s", why)
	}
	// Python-run AI columns stay governed by GO_EXECUTOR_COLUMN_TYPES.
	ai, _ := colgraph.ParseCols([]byte(`[{"id":"a","type":"ai_formula","prompt":"x"}]`))
	if ok, why := Eligible(ai, idx, cfg); ok || why != ReasonColumnType {
		t.Fatalf("ai_formula without GO_EXECUTOR_COLUMN_TYPES: %v %s", ok, why)
	}
	cfg.FullRun = false
	if ok, why := Eligible(cols, idx, cfg); ok || why != ReasonProvenanceOn {
		t.Fatalf("flag off keeps python: %v %s", ok, why)
	}
}

// Output, formula and http columns carry no provider chain: an id that names a default
// waterfall (an output column called "email") must not pull that chain into the go block.
func TestGoBlockSkipsChainlessColumns(t *testing.T) {
	cols, err := colgraph.ParseCols([]byte(`[{"id":"email","type":"output","destination":"webhook"},{"id":"phone","type":"formula","formula":"1"}]`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := GoBlock(cols, nil, func(string) float64 { return 0 }, 4)
	if err != nil {
		t.Fatal(err)
	}
	if want := `"manifests":{}`; !strings.Contains(string(b), want) {
		t.Fatalf("go block %s", b)
	}
}
