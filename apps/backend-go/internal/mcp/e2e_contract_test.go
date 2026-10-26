package mcp

import (
	"sort"
	"strings"
	"testing"
)

// TestRegistryMatchesCatalog: the six constructors produce exactly the 15 catalog
// tools (names and capabilities), and a registry that diverges is refused.
func TestRegistryMatchesCatalog(t *testing.T) {
	r := BuildRegistry(Deps{})
	if err := CheckRegistry(r); err != nil {
		t.Fatal(err)
	}
	if len(Specs) != 15 {
		t.Fatalf("catalog has %d tools, want 15", len(Specs))
	}
	var got []string
	for n := range r.Reads {
		got = append(got, n)
	}
	for n := range r.Writes {
		got = append(got, n)
	}
	sort.Strings(got)
	var want []string
	for _, s := range Specs {
		want = append(want, s.Name)
		if c := registryCapability(r, s.Name); c != s.Capability {
			t.Errorf("%s: capability %q, catalog %q", s.Name, c, s.Capability)
		}
	}
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("registry %v != catalog %v", got, want)
	}
}

func TestCheckRegistryRefusesDivergence(t *testing.T) {
	good := BuildRegistry(Deps{})

	missing := NewRegistry(nil, nil)
	if err := CheckRegistry(missing); err == nil {
		t.Fatal("empty registry accepted")
	}

	extra := NewRegistry(nil, nil)
	for n, tl := range good.Reads {
		extra.Reads[n] = tl
	}
	for n, tl := range good.Writes {
		extra.Writes[n] = tl
	}
	extra.Reads["rogue_tool"] = Tool{Name: "rogue_tool", Capability: "leads:read"}
	if err := CheckRegistry(extra); err == nil || !strings.Contains(err.Error(), "rogue_tool") {
		t.Fatalf("extra tool: %v", err)
	}

	wrongCap := NewRegistry(nil, nil)
	for n, tl := range good.Reads {
		wrongCap.Reads[n] = tl
	}
	for n, tl := range good.Writes {
		wrongCap.Writes[n] = tl
	}
	tl := wrongCap.Reads["find_leads"]
	tl.Capability = "leads:write"
	wrongCap.Reads["find_leads"] = tl
	if err := CheckRegistry(wrongCap); err == nil || !strings.Contains(err.Error(), "find_leads") {
		t.Fatalf("wrong capability: %v", err)
	}
}

func registryCapability(r Registry, name string) string {
	if t, ok := r.Reads[name]; ok {
		return t.Capability
	}
	if t, ok := r.Writes[name]; ok {
		return t.Capability
	}
	return ""
}
