package copilot

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testutil"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ssestream"
)

type rawCatalog struct {
	Tools []struct {
		Name      string          `json:"name"`
		Executor  string          `json:"executor"`
		Dangerous bool            `json:"dangerous"`
		Safe      bool            `json:"safe"`
		Schema    json.RawMessage `json:"schema"`
	} `json:"tools"`
	Dangerous map[string]struct {
		Level  string `json:"level"`
		Label  string `json:"label"`
		Reason string `json:"reason"`
	} `json:"dangerous_tools"`
	Safe []string `json:"safe_tools"`
}

func loadRaw(t *testing.T) rawCatalog {
	t.Helper()
	b, err := os.ReadFile("catalog.json")
	if err != nil {
		t.Fatal(err)
	}
	var c rawCatalog
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCatalogHas25ToolsInPythonOrder(t *testing.T) {
	raw := loadRaw(t)
	got := Catalog()
	if len(got) != 25 || len(got) != len(raw.Tools) {
		t.Fatalf("catalog has %d tools, want 25", len(got))
	}
	for i, tl := range got {
		if tl.Name != raw.Tools[i].Name || tl.Executor != raw.Tools[i].Executor || tl.Dangerous != raw.Tools[i].Dangerous {
			t.Fatalf("tool %d = %+v, want %s", i, tl, raw.Tools[i].Name)
		}
		if tl.Dangerous {
			m := raw.Dangerous[tl.Name]
			if tl.Level != m.Level || tl.Label != m.Label || tl.Reason != m.Reason {
				t.Errorf("%s meta = %q/%q/%q, want %+v", tl.Name, tl.Level, tl.Label, tl.Reason, m)
			}
		}
	}
	// the returned slice is a copy: mutating it must not change the catalog
	got[0].Name = "mutated"
	if Catalog()[0].Name == "mutated" {
		t.Fatal("Catalog() leaks its backing slice")
	}
}

// Schemas must be exactly what Python's json.dumps(tool) emits for build_tools(): default separators,
// ensure_ascii, key order preserved.
func TestSchemasMatchPython(t *testing.T) {
	raw := loadRaw(t)
	schemas := Schemas()
	if len(schemas) != len(raw.Tools) {
		t.Fatalf("Schemas() = %d, want %d", len(schemas), len(raw.Tools))
	}
	for i, s := range schemas {
		want, err := ssestream.Reformat(raw.Tools[i].Schema)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(s, want) {
			t.Fatalf("schema %s differs:\n got %s\nwant %s", raw.Tools[i].Name, s, want)
		}
		if !strings.HasPrefix(string(s), `{"type": "function", "function": {"name": "`+raw.Tools[i].Name+`"`) {
			t.Fatalf("schema %s lost key order: %.80s", raw.Tools[i].Name, s)
		}
	}
}

// When uv and the legacy tree are present, compare against the live Python build_tools() byte for byte.
func TestSchemasMatchLivePython(t *testing.T) {
	py := legacyDir(t)
	cmd := exec.Command("uv", "run", "python", "-c",
		"import os,json;os.environ.setdefault('SQLITE_STORES_BACKEND','sqlite')\n"+
			"from apps.api.services.copilot import tools\n"+
			"print(json.dumps([json.dumps(t) for t in tools.build_tools()]))")
	cmd.Dir = py
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("python build_tools unavailable: %v", err)
	}
	var want []string
	if err := json.Unmarshal(out, &want); err != nil {
		t.Fatal(err)
	}
	got := Schemas()
	if len(got) != len(want) {
		t.Fatalf("python has %d tools, go %d", len(want), len(got))
	}
	for i := range got {
		if string(got[i]) != want[i] {
			t.Fatalf("tool %d differs from python:\n go %s\n py %s", i, got[i], want[i])
		}
	}
}

func legacyDir(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skip("uv not installed")
	}
	return testutil.LegacyPythonDir(t)
}

// Drift guard: catalog.json must match the committed hash, and (when python is available) the generator's --check.
func TestCatalogDriftGuard(t *testing.T) {
	b, err := os.ReadFile("catalog.json")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	want, err := os.ReadFile("testdata/catalog.sha256")
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(sum[:]); got != strings.TrimSpace(string(want)) {
		t.Fatalf("catalog.json changed (sha256 %s); regenerate with gen_copilot_catalog.py and update testdata/catalog.sha256", got)
	}
}

func TestCatalogMatchesGeneratorCheck(t *testing.T) {
	py := legacyDir(t)
	cmd := exec.Command("uv", "run", "python", "scripts/gen_copilot_catalog.py", "--check")
	cmd.Dir = py
	if out, err := cmd.CombinedOutput(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			t.Fatalf("catalog.json drifted from Python: %s", out)
		}
		t.Skipf("generator unavailable: %v", err)
	}
}

func TestCatalogEveryToolHasExecutor(t *testing.T) {
	native := map[string]bool{}
	for _, n := range NativeToolNames() {
		native[n] = true
	}
	for _, tl := range Catalog() {
		switch tl.Executor {
		case "go":
			if !native[tl.Name] {
				t.Errorf("%s is executor=go with no Go implementation", tl.Name)
			}
		case "python":
			if native[tl.Name] {
				t.Errorf("%s is executor=python but has a Go implementation", tl.Name)
			}
		default:
			t.Errorf("%s has executor %q", tl.Name, tl.Executor)
		}
	}
	for n := range native {
		found := false
		for _, tl := range Catalog() {
			found = found || tl.Name == n
		}
		if !found {
			t.Errorf("native tool %s is not in the catalog", n)
		}
	}
	if len(native) != 4 {
		t.Errorf("native tools = %d, want 4", len(native))
	}
}

func TestNeedsConfirmationFailsClosed(t *testing.T) {
	raw := loadRaw(t)
	safe := map[string]bool{}
	for _, n := range raw.Safe {
		safe[n] = true
	}
	for _, tl := range raw.Tools {
		if got, want := NeedsConfirmation(tl.Name), !safe[tl.Name]; got != want {
			t.Errorf("NeedsConfirmation(%s) = %v, want %v", tl.Name, got, want)
		}
		if tl.Dangerous && !NeedsConfirmation(tl.Name) {
			t.Errorf("dangerous tool %s bypasses confirmation", tl.Name)
		}
	}
	for _, n := range []string{"", "rm_rf", "SEARCH_LEADS", "search_leads ", "delete_everything"} {
		if !NeedsConfirmation(n) {
			t.Errorf("unknown tool %q must need confirmation", n)
		}
	}
	if NeedsConfirmation("search_leads") {
		t.Error("search_leads is safe")
	}
}

func TestMetaDefaults(t *testing.T) {
	lvl, label := Meta("update_lead_status")
	if lvl != "medium" || label != "📝 Update Lead Status" {
		t.Errorf("Meta(update_lead_status) = %q %q", lvl, label)
	}
	lvl, label = Meta("start_collection")
	if lvl != "high" || label != "🚀 Launch Lead Collection" {
		t.Errorf("Meta(start_collection) = %q %q", lvl, label)
	}
	for _, n := range []string{"nope", "", "search_leads"} {
		lvl, label = Meta(n)
		if lvl != "high" || label != "Unrecognized write tool" {
			t.Errorf("Meta(%q) = %q %q, want defaults", n, lvl, label)
		}
	}
}

func TestIsDangerous(t *testing.T) {
	if !IsDangerous("update_lead_status") || !IsDangerous("execute_plan") {
		t.Error("expected dangerous")
	}
	if IsDangerous("search_leads") || IsDangerous("unknown_tool") {
		t.Error("expected not dangerous")
	}
}
