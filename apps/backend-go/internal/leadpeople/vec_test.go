package leadpeople

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func loadVec(t testing.TB, name string, v any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

// norm renders v through JSON so Go structs/maps and recorded vectors compare as data.
func norm(t testing.TB, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func sameJSON(t testing.TB, what string, got, want any) {
	t.Helper()
	g, w := norm(t, got), norm(t, want)
	if !reflect.DeepEqual(g, w) {
		gb, _ := json.MarshalIndent(g, "", " ")
		wb, _ := json.MarshalIndent(w, "", " ")
		t.Errorf("%s mismatch\n got: %s\nwant: %s", what, gb, wb)
	}
}

func asMaps(v []any) []map[string]any {
	out := make([]map[string]any, 0, len(v))
	for _, x := range v {
		if m, ok := x.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}
