package runplan

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/manifest"
)

type payloadGolden struct {
	Columns   []json.RawMessage `json:"columns"`
	ColumnIDs []string          `json:"column_ids"`
	Payload   json.RawMessage   `json:"payload"`
	SHA256    map[string]string `json:"sha256"`
}

// realIndex loads the signed manifests copied from the Python repo with the
// Go registry decoder (no signature policy: the copies are the YAML only).
func realIndex(t testing.TB) map[string]manifest.Entry {
	t.Helper()
	files, err := filepath.Glob("testdata/manifests/*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no manifests: %v", err)
	}
	idx := map[string]manifest.Entry{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		e, err := manifest.Decode(raw, f)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		idx[e.Manifest.Name] = e
	}
	return idx
}

func loadGolden(t testing.TB) payloadGolden {
	t.Helper()
	raw, err := os.ReadFile("testdata/payload_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var g payloadGolden
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

// TestPayloadMatchesPythonGolden: the go block equals Python's build_go_payload
// value for value (manifest dump, entry sha256, costs, default concurrency).
func TestPayloadMatchesPythonGolden(t *testing.T) {
	g := loadGolden(t)
	idx := realIndex(t)
	for name, sha := range g.SHA256 {
		if idx[name].SHA256 != sha {
			t.Errorf("sha256 of %s: go %s python %s", name, idx[name].SHA256, sha)
		}
	}
	var all []colgraph.Col
	for _, raw := range g.Columns {
		c, err := colgraph.ParseCol(raw)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, c)
	}
	cost := func(n string) float64 {
		e, ok := idx[n]
		if !ok {
			return 0
		}
		return e.Manifest.CostPerLookup
	}
	got, err := GoBlock(colsByID(all, g.ColumnIDs), idx, cost, 12)
	if err != nil {
		t.Fatal(err)
	}
	gv, err := canonjson.Decode(got)
	if err != nil {
		t.Fatalf("go block is not canonical json: %v\n%s", err, got)
	}
	wv, err := canonjson.Decode(g.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gv, wv) {
		t.Fatalf("go block differs from python\n got %s\nwant %s", got, g.Payload)
	}
}

// The provider order of "manifests" is the dict.fromkeys order over the
// selected columns (Python insertion order), not sorted.
func TestGoBlockProviderOrder(t *testing.T) {
	g := loadGolden(t)
	idx := realIndex(t)
	var all []colgraph.Col
	for _, raw := range g.Columns {
		c, _ := colgraph.ParseCol(raw)
		all = append(all, c)
	}
	got, err := GoBlock(colsByID(all, g.ColumnIDs), idx, func(string) float64 { return 0 }, 12)
	if err != nil {
		t.Fatal(err)
	}
	var probe struct {
		Order []string
	}
	dec := json.NewDecoder(bytesReader(got))
	_ = probe
	names := topLevelKeyOrder(t, dec, "manifest_sha256")
	want := []string{"leadmagic_email", "prospeo_mobile", "leadmagic_mobile", "leadmagic_company"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("order %v want %v", names, want)
	}
}
