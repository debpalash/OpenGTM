package runplan

import (
	"encoding/json"
	"os"
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
