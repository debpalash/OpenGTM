package leaddedup

import "testing"

// Entity-resolution benchmark built from real-world alias/dupe scenarios observed
// in NL discovery runs (and analogous public cases). Each scenario pins the
// expected behaviour of the lead-level matcher; failures document precision/recall
// gaps rather than being "fixed" by loosening the expectations blindly.

func erRec(id int64, company, website, email, phone, city string) Rec {
	r := Rec{Vals: map[string]any{"id": id, "company": company}}
	if website != "" {
		r.Vals["website"] = website
	}
	if email != "" {
		r.Vals["email"] = email
	}
	if phone != "" {
		r.Vals["phone"] = phone
	}
	if city != "" {
		r.Vals["city"] = city
	}
	return r
}

// ER-1: company name variations / aliases — same domain → must merge.
func TestERNameVariationsSameDomain(t *testing.T) {
	cases := []struct {
		name string
		a, b Rec
	}{
		{"suffix alias", erRec(1, "Secura", "https://secura.com", "", "", "Eindhoven"),
			erRec(2, "Secura B.V.", "https://www.secura.com/about", "info@secura.com", "", "Eindhoven")},
		{"expanded name", erRec(3, "Northwave", "https://northwave.nl", "", "", "Utrecht"),
			erRec(4, "Northwave Cyber Security", "https://northwave.nl/services", "", "", "Utrecht")},
		{"abbrev + domain", erRec(5, "Nederlands Forensisch Instituut", "https://www.nfir.nl", "", "", "Rijswijk"),
			erRec(6, "NFIR", "https://nfir.nl", "contact@nfir.nl", "", "Rijswijk")},
		{"case/spacing", erRec(7, "weaviate", "https://weaviate.io", "", "", "Amsterdam"),
			erRec(8, "Weaviate  B.V.", "https://weaviate.io/", "", "", "Amsterdam")},
	}
	for _, c := range cases {
		m := Compare(c.a, c.b)
		if !m.IsDuplicate {
			t.Errorf("ER-1 %s: score=%.3f want duplicate (same domain)", c.name, m.Score)
		}
	}
}

// ER-2: different domains of the same company (brand .com + marketing .nl).
// The matcher CANNOT know these are one legal entity from strings alone, and the pinned
// synthetic_5000 find_duplicates contract forbids merging on exact name+city alone
// (2400 such same-name-same-city pairs must stay split there). Documented limitation:
// secondary-brand domains require manual merge or enrichment-assisted identity.
func TestERSecondaryBrandDomain(t *testing.T) {
	a := erRec(1, "BSM", "https://bsm.nl", "hello@bsm.nl", "", "Amsterdam")
	b := erRec(2, "BSM", "https://bsm-security.com", "", "", "Amsterdam")
	m := Compare(a, b)
	if m.IsDuplicate {
		t.Errorf("ER-2: name+city alone must NOT merge (pinned conservative contract), score=%.3f", m.Score)
	}
	t.Logf("ER-2 documented: brand .nl vs .com without shared evidence stays split (score %.3f)", m.Score)
}

// ER-3: parent/subsidiary on the SAME domain family — Fox-IT (Delft) is part of
// NCC Group but operates under fox-it.com; they are DISTINCT entities for GTM
// purposes and must NOT merge on brand overlap alone... except when the domain
// is literally equal. Document the current behaviour:
func TestERParentSubsidiary(t *testing.T) {
	a := erRec(1, "Fox-IT", "https://fox-it.com", "", "", "Delft")
	b := erRec(2, "NCC Group", "https://nccgroup.com", "", "", "Manchester")
	if m := Compare(a, b); m.IsDuplicate {
		t.Errorf("ER-3: parent/child distinct entities must not merge, score=%.3f", m.Score)
	}
	c := erRec(3, "Fox-IT", "https://fox-it.com", "", "", "Delft")
	d := erRec(4, "Fox-IT (NCC Group)", "https://fox-it.com", "", "", "Delft")
	if m := Compare(c, d); !m.IsDuplicate {
		t.Errorf("ER-3b: same domain alias must merge, score=%.3f", m.Score)
	}
}

// ER-4: similarly named but DIFFERENT companies — the classic false-merge risk.
func TestERSimilarNamesDistinct(t *testing.T) {
	cases := []struct {
		name string
		a, b Rec
	}{
		{"apex domain differs", erRec(1, "Apex Security", "https://apexsecurity.nl", "", "", "Amsterdam"),
			erRec(2, "Apex Security", "https://apexsecurity.com", "sales@apexsecurity.com", "", "Dallas")},
		{"same brand, other sector", erRec(3, "Vest", "https://vest.nl", "", "", "Amsterdam"),
			erRec(4, "VEST", "https://vest.com", "", "", "New York")},
		{"generic consulting names", erRec(5, "Alpha Consulting", "https://alphaconsulting.nl", "", "", "Utrecht"),
			erRec(6, "Alpha Consultancy", "https://alphaconsultancy.be", "", "", "Brussel")},
	}
	for _, c := range cases {
		m := Compare(c.a, c.b)
		if m.IsDuplicate {
			t.Errorf("ER-4 %s: distinct companies merged, score=%.3f", c.name, m.Score)
		}
	}
}

// ER-5: duplicate people — same person harvested from two profile URLs, and
// person identity keyed by email.
func TestERPersonIdentity(t *testing.T) {
	a := erRec(1, "Jan Visser", "https://linkedin.com/in/jan-visser-9f2b1", "j.visser@bsm.nl", "", "Amsterdam")
	b := erRec(2, "Jan Visser", "https://twitter.com/janvisser_security", "", "", "")
	if m := Compare(a, b); m.IsDuplicate {
		t.Logf("person A/B merged (score %.3f) — acceptable: same name, no contradicting evidence", m.Score)
	}
	c := erRec(3, "J. Visser", "https://bsm.nl/team/jan-visser", "j.visser@bsm.nl", "+31 6 12345678", "Amsterdam")
	if m := Compare(a, c); !m.IsDuplicate {
		t.Errorf("ER-5: same person, same email, same domain must merge, score=%.3f", m.Score)
	}
}

// ER-6: person changed employer — the same person name on two DIFFERENT
// company domains must not merge when the company context differs.
func TestERPersonChangedEmployer(t *testing.T) {
	a := erRec(1, "Peter Bakker", "https://linkedin.com/in/peterbakker", "peter.bakker@secura.com", "", "Eindhoven")
	b := erRec(2, "Peter Bakker", "https://linkedin.com/in/peter-bakker-a1b2c3", "p.bakker@northwave.nl", "", "Utrecht")
	if m := Compare(a, b); m.IsDuplicate {
		t.Errorf("ER-6: same person name but different employer domains must not merge, score=%.3f", m.Score)
	}
}

// ER-7: false-negative recall check — the same company seen with one strong
// signal missing (no website, email only; or no email, website only).
func TestERPartialEvidenceRecall(t *testing.T) {
	// email-domain identity WITHOUT a website on one side: the pinned synthetic_5000
	// contract keeps such pairs split (same mail domain + fuzzy name, 450 pairs, must not
	// merge — they model different people at one company). Exact same mailbox + strong
	// name is the safe subset (covered by sameMailbox floor); broader recall needs a
	// deliberate contract regeneration.
	a := erRec(1, "Socar MDR", "https://socarnl.nl", "ops@socarnl.nl", "", "Utrecht")
	b := erRec(2, "Socar", "", "info@socarnl.nl", "", "")
	if m := Compare(a, b); m.IsDuplicate {
		t.Errorf("ER-7a: same-domain different-mailbox must stay split per pinned contract, score=%.3f", m.Score)
	}
	// exact mailbox, website lost on one side — must merge via sameMailbox floor
	e := erRec(5, "Socar MDR", "https://socarnl.nl", "ops@socarnl.nl", "", "Utrecht")
	f := erRec(6, "Socar", "", "ops@socarnl.nl", "", "")
	if m := Compare(e, f); !m.IsDuplicate {
		t.Errorf("ER-7b: exact shared mailbox + strong name should merge, score=%.3f", m.Score)
	}
	// phone identity
	c := erRec(3, "Tien Security", "https://tiensecurity.nl", "", "+31 297 123 456", "Mijdrecht")
	d := erRec(4, "Tien Security B.V.", "", "", "+31 297 123 456", "Mijdrecht")
	if m := Compare(c, d); !m.IsDuplicate {
		t.Errorf("ER-7b: phone identity should merge, score=%.3f", m.Score)
	}
}

// ER-8: end-to-end cluster precision/recall on a mixed 12-lead set:
// 6 true entities, each represented by 1-2 harvest variants.
func TestERClusterBenchmark(t *testing.T) {
	recs := []Rec{
		erRec(1, "BSM", "https://bsm.nl", "hello@bsm.nl", "", "Amsterdam"),
		erRec(2, "BSM Security", "https://www.bsm.nl/about", "", "", "Amsterdam"), // dup of BSM
		erRec(3, "NFIR", "https://nfir.nl", "", "", "Rijswijk"),
		erRec(4, "Netherlands Forensic Institute", "https://www.nfir.nl/contact", "info@nfir.nl", "", "Rijswijk"), // dup of NFIR
		erRec(5, "Northwave", "https://northwave.nl", "", "", "Utrecht"),
		erRec(6, "Resilience", "https://resilience.nl", "hello@resilience.nl", "", "Utrecht"), // rebrand — distinct domain, NOT a dup by current rules
		erRec(7, "Apex Security", "https://apexsecurity.nl", "", "", "Amsterdam"),
		erRec(8, "Apex Security", "https://apexsecurity.com", "sales@apexsecurity.com", "", "Dallas"), // distinct (US namesake)
		erRec(9, "Weaviate", "https://weaviate.io", "", "", "Amsterdam"),
		erRec(10, "Weaviate B.V.", "https://weaviate.io", "", "+31 20 555 0110", "Amsterdam"), // dup of Weaviate
		erRec(11, "Secura", "https://secura.com", "info@secura.com", "", "Eindhoven"),
		erRec(12, "Bureau Veritas Cybersecurity", "https://cybersecurity.bureauveritas.com", "", "", "Amsterdam"), // parent-brand, distinct harvest
	}
	res := FindDuplicates(recs, 0.85)
	clusters := map[string][]any{}
	for _, c := range res.Clusters {
		clusters[c.Key] = c.IDs
	}
	// Expected merges: (1,2), (3,4), (9,10). Expected non-merges: 7 vs 8, 5 vs 6, 11 vs 12.
	pairsFound := map[[2]int64]float64{}
	for _, p := range res.Pairs {
		a, _ := p.AID.(int64)
		b, _ := p.BID.(int64)
		if a == 0 {
			ai, _ := p.AID.(int32)
			bi, _ := p.BID.(int32)
			a, b = int64(ai), int64(bi)
		}
		pairsFound[[2]int64{min(a, b), max(a, b)}] = p.Score
	}
	mustMerge := [][2]int64{{1, 2}, {3, 4}, {9, 10}}
	mustNotMerge := [][2]int64{{7, 8}, {5, 6}, {11, 12}}
	var fp, fn int
	for _, pr := range mustMerge {
		if _, ok := pairsFound[pr]; !ok {
			fn++
			t.Errorf("ER-8 miss (false negative): %v not clustered", pr)
		}
	}
	for _, pr := range mustNotMerge {
		if _, ok := pairsFound[pr]; ok {
			fp++
			t.Errorf("ER-8 false merge: %v clustered (score %.2f)", pr, pairsFound[pr])
		}
	}
	t.Logf("ER-8 cluster precision=%d/%d recall=%d/%d", len(mustNotMerge)-fp, len(mustNotMerge), len(mustMerge)-fn, len(mustMerge))
}
