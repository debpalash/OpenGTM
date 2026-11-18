package vendorcatalog

import "testing"

func TestCompanyEnrichBoundedCostEstimates(t *testing.T) {
	for name, want := range map[string]float64{"companyenrich_company": 0.0098, "companyenrich_person": 0.049, "companyenrich_workforce": 0.049, "companyenrich_people": 0.196, "companyenrich_search": 0.196, "companyenrich_similar": 0.49} {
		if got := BaseCost(name); got != want || !IsPaid(name) || !HasKnownCost(name) || !IsBYOK(name) {
			t.Errorf("%s not correctly declared as paid BYOK: cost=%v", name, got)
		}
	}
}
