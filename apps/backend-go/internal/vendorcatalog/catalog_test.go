package vendorcatalog

import (
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

type vectors struct {
	Registry map[string]struct {
		CostPerLookup float64 `json:"cost_per_lookup"`
	} `json:"registry"`
	Vendors []struct {
		Name         string   `json:"name"`
		BaseCost     float64  `json:"base_cost"`
		BYOK         bool     `json:"byok"`
		Capabilities []string `json:"capabilities"`
		Notes        string   `json:"notes"`
	} `json:"vendors"`
	Lookups []struct {
		Name         string  `json:"name"`
		ResearchFlag bool    `json:"research_flag"`
		BaseCost     float64 `json:"base_cost"`
		IsPaid       bool    `json:"is_paid"`
		HasKnownCost bool    `json:"has_known_cost"`
		IsBYOK       bool    `json:"is_byok"`
	} `json:"lookups"`
	CalculateCost []struct {
		Name      string         `json:"name"`
		Operation string         `json:"operation"`
		Params    map[string]any `json:"params"`
		Cost      float64        `json:"cost"`
	} `json:"calculate_cost"`
	Estimate []struct {
		Name      string          `json:"name"`
		Flag      bool            `json:"research_flag"`
		NumRows   int             `json:"num_rows"`
		ProvOrder json.RawMessage `json:"providers_by_column"`
		Result    json.RawMessage `json:"result"`
	} `json:"estimate_run_cost"`
}

func load(t *testing.T) vectors {
	t.Helper()
	b, err := os.ReadFile("testdata/catalog_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	reg := v.Registry
	SetDeclaredCost(func(name string) (float64, bool) {
		r, ok := reg[name]
		return r.CostPerLookup, ok
	})
	t.Cleanup(func() { SetDeclaredCost(nil); SetResearchFlag(nil) })
	return v
}

func flag(on bool) { SetResearchFlag(func() bool { return on }) }

func TestCostTableMatchesPython(t *testing.T) {
	v := load(t)
	got := ListVendors()
	if len(got) != len(v.Vendors) {
		t.Fatalf("vendors: got %d want %d", len(got), len(v.Vendors))
	}
	for i, w := range v.Vendors {
		b, _ := got[i].MarshalJSON()
		var m struct {
			Name         string   `json:"name"`
			BaseCost     float64  `json:"base_cost"`
			BYOK         bool     `json:"byok"`
			Capabilities []string `json:"capabilities"`
			Notes        string   `json:"notes"`
		}
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		if m.Name != w.Name || m.BaseCost != w.BaseCost || m.BYOK != w.BYOK || m.Notes != w.Notes ||
			strings.Join(m.Capabilities, ",") != strings.Join(w.Capabilities, ",") {
			t.Errorf("vendor %d: got %+v want %+v", i, m, w)
		}
		if !HasKnownCost(w.Name) {
			t.Errorf("%s missing from catalog", w.Name)
		}
	}
	// key order of a vendor object
	b, _ := got[0].MarshalJSON()
	if !strings.HasPrefix(string(b), `{"name":"hunter_io","base_cost":0.04,"byok":true,"capabilities":["email"],"notes":""}`) {
		t.Errorf("vendor key order/repr: %s", b)
	}
}

func TestLookupsMatchVectors(t *testing.T) {
	v := load(t)
	for _, l := range v.Lookups {
		flag(l.ResearchFlag)
		if g := BaseCost(l.Name); g != l.BaseCost {
			t.Errorf("BaseCost(%q,flag=%v)=%v want %v", l.Name, l.ResearchFlag, g, l.BaseCost)
		}
		if g := IsPaid(l.Name); g != l.IsPaid {
			t.Errorf("IsPaid(%q)=%v want %v", l.Name, g, l.IsPaid)
		}
		if g := HasKnownCost(l.Name); g != l.HasKnownCost {
			t.Errorf("HasKnownCost(%q)=%v want %v", l.Name, g, l.HasKnownCost)
		}
		if g := IsBYOK(l.Name); g != l.IsBYOK {
			t.Errorf("IsBYOK(%q)=%v want %v", l.Name, g, l.IsBYOK)
		}
	}
}

func TestCalculateCostMatchesVectors(t *testing.T) {
	v := load(t)
	flag(false)
	for _, c := range v.CalculateCost {
		if g := CalculateCost(c.Name, c.Operation, c.Params); g != c.Cost {
			// research cases are recorded with the flag on
			flag(true)
			g2 := CalculateCost(c.Name, c.Operation, c.Params)
			flag(false)
			if g2 != c.Cost {
				t.Errorf("CalculateCost(%q,%q,%v)=%v/%v want %v", c.Name, c.Operation, c.Params, g, g2, c.Cost)
			}
		}
	}
}

func TestEstimateRunCostMatchesVectors(t *testing.T) {
	v := load(t)
	for _, e := range v.Estimate {
		flag(e.Flag)
		order := orderedProviders(t, e.ProvOrder)
		got, err := EstimateRunCost(e.NumRows, order.m, order.keys).MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		if !jsonTextEqual(t, got, e.Result) {
			t.Errorf("%s flag=%v:\n got  %s\n want %s", e.Name, e.Flag, got, e.Result)
		}
	}
}

func TestEstimateFloatRepr(t *testing.T) {
	v := load(t)
	flag(false)
	for _, e := range v.Estimate {
		if e.Name != "float_rounding" {
			continue
		}
		order := orderedProviders(t, e.ProvOrder)
		o := EstimateRunCost(e.NumRows, order.m, order.keys)
		w, _ := o.Get("worst_usd")
		if s := wbcrud.PyFloatRepr(w.(float64)); s != "0.069" {
			t.Errorf("worst_usd repr %s", s)
		}
	}
	// free-only column: Python's sum([]) * rows is the int 0, best stays 0.0.
	o := EstimateRunCost(5, map[string][]string{"a": {"ip"}}, []string{"a"})
	b, _ := o.MarshalJSON()
	if !strings.Contains(string(b), `"worst_usd":0,"best_usd":0.0`) {
		t.Errorf("free-only repr: %s", b)
	}
}

func TestIsBYOKFollowsEnvVar(t *testing.T) {
	load(t)
	SetResearchFlag(nil) // back to the RESEARCH_VENDOR_COST env default
	for _, tc := range []struct {
		env  string
		set  bool
		cost float64
	}{{"", false, 0}, {"0", true, 0}, {" OFF ", true, 0}, {"No", true, 0}, {"false", true, 0}, {"", true, 0}, {"1", true, 0.05}, {"yes", true, 0.05}, {"on", true, 0.05}} {
		if tc.set {
			t.Setenv("RESEARCH_VENDOR_COST", tc.env)
		} else {
			os.Unsetenv("RESEARCH_VENDOR_COST")
		}
		for _, n := range []string{"research", "claygent"} {
			if g := BaseCost(n); g != tc.cost {
				t.Errorf("env=%q %s: %v want %v", tc.env, n, g, tc.cost)
			}
		}
	}
	if IsBYOK("research") || !IsBYOK("hunter_io") || !IsBYOK("unknown") {
		t.Error("IsBYOK")
	}
}

func TestDeclaredCostRejectsInvalid(t *testing.T) {
	load(t)
	for _, c := range []float64{math.NaN(), math.Inf(1), -1} {
		SetDeclaredCost(func(string) (float64, bool) { return c, true })
		if HasKnownCost("zz") || BaseCost("zz") != 0 {
			t.Errorf("declared %v accepted", c)
		}
	}
	SetDeclaredCost(func(n string) (float64, bool) { return 0.07, n == "zz" })
	if BaseCost("zz") != 0.07 || !IsPaid("zz") {
		t.Error("declared positive cost not used")
	}
	SetDeclaredCost(func(n string) (float64, bool) { return 0.5, n == "hunter_io" })
	if BaseCost("hunter_io") != 0.5 {
		t.Error("declared cost must win over the table")
	}
}
