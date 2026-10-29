package runplan

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
)

// Review M4: the go block records the capabilities a run needs beyond the default executor, so
// a worker without them hands the job back instead of silently dropping provenance or automations.
func TestRequiredCapabilities(t *testing.T) {
	cols, err := colgraph.ParseCols([]byte(`[{"id":"e","type":"enrichment","provider":"p","target_field":"email"},
	  {"id":"c","type":"enrichment","provider":"p","target_field":"phone","condition":"{company} != \"\""},
	  {"id":"o","type":"output","destination":"webhook"}]`))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		cols []colgraph.Col
		cfg  Config
		want []string
	}{
		{"all", cols, Config{ProvenanceTracking: true, AutomationsEnabled: true},
			[]string{"automations", "conditions", "email_verify", "output", "provenance"}},
		{"columns only", cols, Config{}, []string{"conditions", "email_verify", "output"}},
		{"verify off", cols[:1], Config{}, []string{"email_verify"}},
		{"flags only", cols[1:2], Config{ProvenanceTracking: true}, []string{"conditions", "provenance"}},
		{"nothing", nil, Config{}, nil},
	}
	for _, c := range cases {
		cols := c.cols
		if c.name == "verify off" {
			cols, _ = colgraph.ParseCols([]byte(`[{"id":"e","type":"enrichment","provider":"p","target_field":"email","verify":false}]`))
			c.want = nil
		}
		if got := RequiredCapabilities(cols, c.cfg); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %v want %v", c.name, got, c.want)
		}
	}
}

func TestGoBlockRecordsRequirements(t *testing.T) {
	idx := map[string]manifest.Entry{}
	plain, err := GoBlock(nil, idx, func(string) float64 { return 0 }, 12)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(plain, &m); err != nil || m["requires"] != nil {
		t.Fatalf("a run with no extra capability keeps the block unchanged: %s", plain)
	}
	with, err := GoBlockRequiring(nil, idx, func(string) float64 { return 0 }, 12, []string{"output", "provenance"})
	if err != nil {
		t.Fatal(err)
	}
	m = nil
	if err := json.Unmarshal(with, &m); err != nil || !reflect.DeepEqual(m["requires"], []any{"output", "provenance"}) {
		t.Fatalf("%s", with)
	}
	none, _ := GoBlockRequiring(nil, idx, func(string) float64 { return 0 }, 12, nil)
	if string(none) != string(plain) {
		t.Fatal("no requirements must render the historical block")
	}
}

// A native (registry) provider has no declarative manifest: with native providers on, the go
// block carries only its cost; a manifest-less provider is still an error otherwise.
func TestGoBlockNativeProviders(t *testing.T) {
	cols, err := colgraph.ParseCols([]byte(`[{"id":"e","type":"enrichment","provider":"hunter_io","target_field":"email"}]`))
	if err != nil {
		t.Fatal(err)
	}
	cost := func(string) float64 { return 0.5 }
	if _, err := GoBlockNative(cols, map[string]manifest.Entry{}, cost, 12, nil, false); err == nil {
		t.Fatal("a provider without a manifest must fail when native providers are off")
	}
	b, err := GoBlockNative(cols, map[string]manifest.Entry{}, cost, 12, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Manifests map[string]any     `json:"manifests"`
		Costs     map[string]float64 `json:"costs"`
	}
	if err := json.Unmarshal(b, &m); err != nil || len(m.Manifests) != 0 || m.Costs["hunter_io"] != 0.5 {
		t.Fatalf("%s %v", b, err)
	}
}
