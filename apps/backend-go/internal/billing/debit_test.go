package billing

import (
	"encoding/json"
	"errors"
	"os"
	"testing"
)

type projVector struct {
	Name              string              `json:"name"`
	ResearchFlag      bool                `json:"research_flag"`
	NumRows           int                 `json:"num_rows"`
	ProvidersByColumn map[string][]string `json:"providers_by_column"`
	ColumnCounts      map[string]int      `json:"column_counts"`
	Result            struct {
		Value float64 `json:"value"`
	} `json:"result"`
}

func TestProjectedPlatformCostMatchesVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/debit_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Projected []projVector `json:"projected_platform_cost"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Projected) < 20 {
		t.Fatalf("expected >= 20 vectors, got %d", len(doc.Projected))
	}
	orig := ResearchVendorCost
	t.Cleanup(func() { ResearchVendorCost = orig })
	for _, v := range doc.Projected {
		ResearchVendorCost = func() bool { return v.ResearchFlag }
		got := ProjectedPlatformCost(v.NumRows, v.ProvidersByColumn, v.ColumnCounts)
		if got != v.Result.Value {
			t.Errorf("%s (flag=%v): got %v want %v", v.Name, v.ResearchFlag, got, v.Result.Value)
		}
	}
}

func TestInsufficientCreditsErrorText(t *testing.T) {
	var err error = &InsufficientCreditsError{BalanceUSD: 0.75314, RequiredUSD: 1.23456789}
	want := "Insufficient credits: balance $0.7531 < required $1.2346. Top up to run this enrichment."
	if err.Error() != want {
		t.Fatalf("got %q want %q", err.Error(), want)
	}
	var ice *InsufficientCreditsError
	if !errors.As(err, &ice) || ice.BalanceUSD != 0.7531 && ice.BalanceUSD != 0.75314 {
		t.Fatalf("errors.As failed: %+v", ice)
	}
}
