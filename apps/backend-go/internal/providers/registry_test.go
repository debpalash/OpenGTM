package providers_test

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	_ "github.com/debpalash/OpenGTM/apps/backend-go/internal/providers/contact"
	_ "github.com/debpalash/OpenGTM/apps/backend-go/internal/providers/firmo"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testutil"
)

func listed(t *testing.T) map[string]string {
	f, err := os.Open("python_providers.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		mod, reason, _ := strings.Cut(line, " ")
		if strings.TrimSpace(reason) == "" {
			t.Errorf("python_providers.txt: %q has no reason", mod)
		}
		out[mod] = strings.TrimSpace(reason)
	}
	return out
}

// pyProviderName matches a provider class attribute (`    name = "hunter_io"`) in a Python module.
var pyProviderName = regexp.MustCompile(`(?m)^    name\s*=\s*"([^"]+)"`)

// python_providers.txt is Go-side data: every entry carries a reason (runs without the Python tree).
func TestPythonProvidersListHasReasons(t *testing.T) {
	if len(listed(t)) == 0 {
		t.Fatal("python_providers.txt lists no modules")
	}
}

// Every Python provider module is ported (registered in Go) or listed in python_providers.txt with a
// reason. The check derives from the directory glob, the module source and python_providers.txt only:
// testdata/python_provider_inventory.json is informational (its static analysis misses lazy imports)
// and must not decide coverage.
func TestEveryProviderPortedOrListed(t *testing.T) {
	legacyProviders := testutil.LegacyPath(t, "apps", "api", "services", "leadgen", "enrichment", "providers")
	py := listed(t)
	files, err := filepath.Glob(filepath.Join(legacyProviders, "*.py"))
	if err != nil || len(files) < 30 {
		t.Fatalf("legacy providers not found: %v (%d)", err, len(files))
	}
	for _, f := range files {
		mod := strings.TrimSuffix(filepath.Base(f), ".py")
		if mod == "__init__" {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		ported := false
		for _, m := range pyProviderName.FindAllStringSubmatch(string(src), -1) {
			if _, ok := providers.Get(m[1]); ok {
				ported = true
			}
		}
		_, isListed := py[mod]
		switch {
		case ported && isListed:
			t.Errorf("%s is both ported and listed", mod)
		case ported || isListed:
		default:
			t.Errorf("%s is neither ported nor listed in python_providers.txt", mod)
		}
	}
	for mod := range py {
		if _, err := os.Stat(filepath.Join(legacyProviders, mod+".py")); err != nil {
			t.Errorf("python_providers.txt lists %q which is not a Python module", mod)
		}
	}
}

func TestRegistryBasics(t *testing.T) {
	for _, n := range []string{"hunter_io", "apollo_io", "prospeo", "snovio", "people_data_labs", "debounce", "abstract_api", "numverify", "reacher"} {
		p, ok := providers.Get(n)
		if !ok || p.Name() != n {
			t.Errorf("%s not registered under its own name", n)
		}
	}
	if _, ok := providers.Get("nope"); ok {
		t.Error("unknown provider found")
	}
	if got := providers.SplitKeys(providers.JoinKeys("a", "b")); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("JoinKeys/SplitKeys round trip: %v", got)
	}
}
