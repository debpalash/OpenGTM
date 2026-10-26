package providers_test

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	_ "github.com/debpalash/OpenGTM/apps/backend-go/internal/providers/contact"
	_ "github.com/debpalash/OpenGTM/apps/backend-go/internal/providers/firmo"
)

const legacyProviders = "../../../../legacy/python/apps/api/services/leadgen/enrichment/providers"

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

// Every Python provider module is ported (registered), listed as a Python
// worker provider with a reason.
func TestEveryProviderPortedOrListed(t *testing.T) {
	var inv struct {
		Providers []struct {
			Module  string `json:"module"`
			Classes []struct {
				Name *string `json:"name"`
			} `json:"classes"`
		} `json:"providers"`
	}
	raw, err := os.ReadFile("testdata/python_provider_inventory.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &inv); err != nil {
		t.Fatal(err)
	}
	names := map[string][]string{}
	for _, p := range inv.Providers {
		for _, c := range p.Classes {
			if c.Name != nil {
				names[p.Module] = append(names[p.Module], *c.Name)
			}
		}
	}
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
		ported := false
		for _, n := range names[mod] {
			if _, ok := providers.Get(n); ok {
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
