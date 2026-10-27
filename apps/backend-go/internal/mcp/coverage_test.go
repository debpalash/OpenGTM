package mcp

import (
	"context"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testutil"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// pythonFile reads a legacy Python source file; the legacy tree is untracked and
// may be absent (CI), in which case the test skips.
func pythonFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(testutil.LegacyPath(t, rel))
	if err != nil {
		t.Skipf("legacy python tree not available: %v", err)
	}
	return string(b)
}

func TestEveryPythonToolIsPortedOrUnavailable(t *testing.T) {
	src := pythonFile(t, "apps/api/services/mcp/tools.py")
	start := strings.Index(src, "TOOL_SPECS = [")
	if start < 0 {
		t.Fatal("TOOL_SPECS not found")
	}
	block := src[start:]
	if end := strings.Index(block, "\n]\n"); end > 0 {
		block = block[:end]
	}
	re := regexp.MustCompile(`(?m)^        "name": "([a-z_]+)",\n\s+"capability": auth\.CAP_([A-Z_]+),`)
	m := re.FindAllStringSubmatch(block, -1)
	if len(m) != 15 {
		t.Fatalf("parsed %d Python tools, want 15", len(m))
	}
	var py, goNames []string
	for _, x := range m {
		py = append(py, x[1])
	}
	for _, s := range Specs {
		goNames = append(goNames, s.Name)
	}
	if strings.Join(py, ",") != strings.Join(goNames, ",") {
		t.Fatalf("catalog differs from TOOL_SPECS (order matters on the wire)\n py: %v\n go: %v", py, goNames)
	}
	// Python capability constant per tool equals the Go capability.
	for i, x := range m {
		want := strings.ToLower(strings.Replace(x[2], "_", ":", 1))
		if Specs[i].Capability != want {
			t.Fatalf("%s: capability %q, Python %q", x[1], Specs[i].Capability, want)
		}
	}
	// The registry holds each of them, as a handler or an explicit unavailable stub:
	// with no ports wired every tool still answers, never "Unknown tool".
	d := Deps{InWorkspace: nilTxRunner, Cfg: Config{WriteEnabled: true}}
	reg := BuildRegistry(d)
	for _, n := range py {
		_, r := reg.Reads[n]
		_, w := reg.Writes[n]
		if !r && !w {
			t.Fatalf("tool %s is in neither registry", n)
		}
	}
}

func TestEveryPythonConfigNamePresent(t *testing.T) {
	src := pythonFile(t, "apps/api/core/config.py")
	cfg, err := os.ReadFile(filepath.Join("..", "config", "mcp.go"))
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"MCP_REQUIRE_AUTH", "MCP_WRITE_ENABLED", "MCP_TOKEN_TTL_DAYS", "MCP_MAX_WRITES_PER_DAY",
		"AUTOMATIONS_ENABLED", "AUTOMATIONS_ALLOW_LEGACY_OUTREACH", "AUTOMATIONS_MAX_RULES_PER_WS"}
	for _, n := range names {
		if !strings.Contains(src, "\n    "+n+":") {
			t.Fatalf("%s not declared in Python config.py (test list is stale)", n)
		}
		if !strings.Contains(string(cfg), `"`+n+`"`) {
			t.Fatalf("%s not read by internal/config/mcp.go", n)
		}
	}
	for _, n := range []string{"OPENGTM_MCP_TOKEN", "YUPCHA_MCP_TOKEN", "OPENGTM_MCP_SSE_HOST", "YUPCHA_MCP_SSE_HOST"} {
		if !strings.Contains(string(cfg), `"`+n+`"`) {
			t.Fatalf("%s not read by internal/config/mcp.go", n)
		}
	}
}

// TestNoToolImportsDomainPackagesDirectly pins the import graph: tool files reach
// data and providers only through the ports in ports.go. The one exception is
// wbcrud.MakeColumn, a pure column constructor used by planColumns.
func TestNoToolImportsDomainPackagesDirectly(t *testing.T) {
	const mod = "github.com/debpalash/OpenGTM/apps/backend-go/"
	allowed := map[string]bool{mod + "internal/mcp/pyjson": true, mod + "internal/wbcrud": true}
	files, err := filepath.Glob("tools_*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob: %v %d", err, len(files))
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, im := range af.Imports {
			p, _ := strconv.Unquote(im.Path.Value)
			if strings.HasPrefix(p, mod) && !allowed[p] {
				t.Errorf("%s imports %s; tools must go through ports.go", f, p)
			}
		}
	}
}

// TestArgErrorWireText pins the D3 text a client sees for a mistyped argument.
func TestArgErrorWireText(t *testing.T) {
	d := readDeps(&fakeLeads{})
	s := NewServer(d, NewRegistry(readTools(d), nil))
	c := Ctx{WorkspaceID: "w", Capabilities: map[string]bool{CapLeadsRead: true}}
	got := s.Execute(context.Background(), c, "find_leads", Args{"query": "x", "city": float64(1)})
	if want := `{"error": "city must be a string"}`; got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	got = s.Execute(context.Background(), c, "find_leads", Args{"query": "x", "limit": "x"})
	if !strings.HasPrefix(got, `{"error": "limit must be`) {
		t.Fatalf("limit: %s", got)
	}
}
