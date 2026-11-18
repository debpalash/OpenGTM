package collect

import "testing"

// withIdentityDedup turns the COLLECT_DEDUP_DOMAIN_IDENTITY gate on for one test.
func withIdentityDedup(t *testing.T) {
	t.Helper()
	old := getenv
	getenv = func(k string) string {
		if k == "COLLECT_DEDUP_DOMAIN_IDENTITY" {
			return "true"
		}
		return old(k)
	}
	t.Cleanup(func() { getenv = old })
}

func TestRegistrableDomain(t *testing.T) {
	cases := map[string]string{
		"www.bsm.nl":             "bsm.nl",
		"security.example.co.uk": "example.co.uk",
		"blog.acme.com":          "acme.com",
		"deep.sub.northwave.nl":  "northwave.nl",
		"":                       "",
		"localhost":              "",
		"substack.com":           "", // platform host: no company identity
		"acme.substack.com":      "", // its registrable domain IS the platform
		"acme.github.io":         "",
		"news.medium.com":        "",
		"tuv.nl":                 "tuv.nl",
		"WWW.Example.COM":        "example.com",
	}
	for in, want := range cases {
		if got := registrableDomain(in); got != want {
			t.Errorf("registrableDomain(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDedupeMergesDomainAliases(t *testing.T) {
	withIdentityDedup(t)
	// Same company harvested under different names on the same domain must merge
	// (the richer lead wins the sort, the alias is dropped).
	leads := []*Lead{
		{Company: "BSM", Website: "https://bsm.nl", Email: ""},
		{Company: "BSM Security", Website: "https://www.bsm.nl/about", Email: "hello@bsm.nl", Phone: "+31 20 1234"},
	}
	got := Dedupe(leads, DedupThreshold)
	if len(got) != 1 {
		t.Fatalf("domain-alias pair should merge to 1, got %d", len(got))
	}
	if got[0].Company != "BSM Security" {
		t.Errorf("richer lead should survive, got %q", got[0].Company)
	}
}

func TestDedupeMergesOnCompanyEmailDomain(t *testing.T) {
	withIdentityDedup(t)
	// No websites, but the same non-consumer mail domain identifies the company.
	leads := []*Lead{
		{Company: "NFIR", Email: "incident@nfir.nl"},
		{Company: "Netherlands Forensic Institute", Email: "info@nfir.nl"},
	}
	if got := Dedupe(leads, DedupThreshold); len(got) != 1 {
		t.Fatalf("shared company mail domain should merge, got %d", len(got))
	}
}

func TestDedupeKeepsDistinctCompaniesOnSharedHosts(t *testing.T) {
	withIdentityDedup(t)
	// Consumer email providers and website platforms are NOT identity: distinct
	// companies sharing them must all survive.
	leads := []*Lead{
		{Company: "Verhoeven Consulting", Email: "owner@gmail.com"},
		{Company: "Van Dijk Automatisering", Email: "admin@gmail.com"},
		{Company: "Noord Nieuws", Website: "https://noord.substack.com"},
		{Company: "Zuid Rapporten", Website: "https://zuid.substack.com"},
	}
	if got := Dedupe(leads, DedupThreshold); len(got) != 4 {
		t.Fatalf("shared gmail/substack hosts must not merge distinct companies, got %d", len(got))
	}
}

func TestDedupeIdentityGateOffKeepsLegacy(t *testing.T) {
	// Without COLLECT_DEDUP_DOMAIN_IDENTITY the behaviour is name-only: a domain
	// alias with dissimilar names survives as its own lead (legacy vector contract).
	leads := []*Lead{
		{Company: "Kynexis", Website: "https://kynexis.nl"},
		{Company: "Pentest Divisie", Website: "https://kynexis.nl/pentesten"},
	}
	if got := Dedupe(leads, DedupThreshold); len(got) != 2 {
		t.Fatalf("gate off must preserve legacy name-only dedupe, got %d", len(got))
	}
}

func TestDedupeNameOnlyBehaviourUnchanged(t *testing.T) {
	// Leads with no domain identity still dedupe purely by name as before.
	leads := []*Lead{
		{Company: "Acme Staffing"},
		{Company: "Acme  Staffing"},
		{Company: "Acmé Staffing"},
	}
	if got := Dedupe(leads, DedupThreshold); len(got) != 1 {
		t.Fatalf("name-fuzzy dedupe changed, got %d want 1", len(got))
	}
}
