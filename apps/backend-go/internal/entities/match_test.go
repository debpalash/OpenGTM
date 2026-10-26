package entities

import "testing"

func TestNormalizersMatchPython(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.Match.Companies {
		if got := NormalizeCompany(c[0]); got != c[1] {
			t.Errorf("NormalizeCompany(%q) = %q, python %q", c[0], got, c[1])
		}
	}
	for _, d := range v.Match.Domains {
		if got := NormalizeDomain(d[0]); got != d[1] {
			t.Errorf("NormalizeDomain(%q) = %q, python %q", d[0], got, d[1])
		}
		if got := IdentityDomain(d[0]); got != d[2] {
			t.Errorf("IdentityDomain(%q) = %q, python %q", d[0], got, d[2])
		}
	}
	for _, p := range v.Match.Phones {
		if got := NormalizePhone(p[0]); got != p[1] {
			t.Errorf("NormalizePhone(%q) = %q, python %q", p[0], got, p[1])
		}
	}
}

func TestCompareLeadsMatchesPython(t *testing.T) {
	v := loadVectors(t)
	lead := func(m map[string]*string) Lead {
		s := func(k string) string {
			if p := m[k]; p != nil {
				return *p
			}
			return ""
		}
		return Lead{Company: s("company"), Website: s("website"), Phone: s("phone"), Email: s("email"), City: s("city")}
	}
	if len(v.Match.Pairs) == 0 {
		t.Fatal("no pair vectors")
	}
	for _, p := range v.Match.Pairs {
		i, j := int(p[0]), int(p[1])
		if got := CompareLeads(lead(v.Match.Leads[i]), lead(v.Match.Leads[j])); got != p[2] {
			t.Errorf("CompareLeads(%d,%d) = %v, python %v", i, j, got, p[2])
		}
	}
}

func TestScopeKeyMatchesPython(t *testing.T) {
	// python: _scope_key(["e-keep"]) -> accounts_4acbe2aba5095861263ca7bd (recorded in merge_vectors.json watch w-both)
	if got := scopeKey([]string{"e-keep"}); got != "accounts_4acbe2aba5095861263ca7bd" {
		t.Errorf("scopeKey = %s", got)
	}
}
