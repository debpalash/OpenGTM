package collect

import "testing"

// Service-category SERP headlines (NL cybersecurity benchmark: 5/12 stored rows) must be
// rejected as generic_service_title, while real companies — including ones whose names
// legitimately contain those words — must pass.
func TestNameVerdictServiceHeadlines(t *testing.T) {
	reject := []string{
		"Cybersecurity Assessments",
		"Cybersecurity  in Amsterdam, Netherlands",
		"B2b Cybersecurity Sales",
		"Managed Detection and Response",
		"Auditing and Testing Services",
		"Penetration Testing",
		"Information Security",
		"Vulnerability Management",
		"Security Audit",
		"Managed Detection",
	}
	for _, n := range reject {
		if r := nameVerdict(n); r == "" {
			t.Errorf("headline %q accepted (want rejection)", n)
		}
	}
	accept := []string{
		"BSM", "Secura", "Netsmartsecurity", "Socnederland", "Sogeti Nederland",
		"Northwave Cyber Security", "Cybersecurity Bureau Veritas",
		"Tata Consultancy Services", "Bright Path Consultants",
	}
	for _, n := range accept {
		if r := nameVerdict(n); r != "" {
			t.Errorf("real company %q rejected as %q", n, r)
		}
	}
}
