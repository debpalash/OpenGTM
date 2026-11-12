package collect

import (
	"context"
	"testing"
)

func withEnv(t *testing.T, k, v string) {
	t.Helper()
	old := getenv
	getenv = func(x string) string {
		if x == k {
			return v
		}
		return old(x)
	}
	t.Cleanup(func() { getenv = old })
}

func TestCityDefaultFollowsPrimaryMarket(t *testing.T) {
	withEnv(t, "DEFAULT_REGION", "")
	if got := cityDefault(); got != "India" {
		t.Fatalf("unset market: %q", got)
	}
	withEnv(t, "DEFAULT_REGION", "nl")
	if got := cityDefault(); got != "Amsterdam" {
		t.Fatalf("nl: %q", got)
	}
	if got := marketDefault(); got != "Netherlands" {
		t.Fatalf("nl label: %q", got)
	}
	withEnv(t, "DEFAULT_REGION", "Germany")
	if got := cityDefault(); got != "Berlin" {
		t.Fatalf("Germany: %q", got)
	}
	withEnv(t, "DEFAULT_REGION", "atlantis")
	if got := cityDefault(); got != "India" {
		t.Fatalf("unknown market must keep legacy default: %q", got)
	}
}

func TestGeoCityRecognisesMarketCities(t *testing.T) {
	withEnv(t, "DEFAULT_REGION", "")
	if got := GeoCity("cybersecurity SaaS companies in Amsterdam"); got != "Amsterdam" {
		t.Fatalf("Amsterdam: %q", got)
	}
	if got := GeoCity("IT staffing companies in Bangalore"); got != "Bangalore" {
		t.Fatalf("legacy India first: %q", got)
	}
	if got := GeoCity("software companies"); got != "" {
		t.Fatalf("no city: %q", got)
	}
}

func TestGeoNamed(t *testing.T) {
	m := geoNamed("Netherlands-based B2B cybersecurity SaaS companies")
	if m == nil || m.key != "nl" {
		t.Fatalf("nl not named: %+v", m)
	}
	if m := geoNamed("IT staffing companies in Bangalore"); m != nil {
		t.Fatalf("Bangalore alone must not name a market")
	}
	if m := geoNamed("german manufacturing exporters"); m == nil || m.key != "de" {
		t.Fatalf("de not named: %+v", m)
	}
}

func TestMarketContradictsCity(t *testing.T) {
	m := &marketSpecs[0] // nl
	if !m.contradictsCity("India") {
		t.Fatal("legacy India default must contradict an NL market")
	}
	if !m.contradictsCity("Mumbai") {
		t.Fatal("Mumbai contradicts nl")
	}
	if !m.contradictsCity("Berlin") {
		t.Fatal("Berlin contradicts nl")
	}
	if m.contradictsCity("Rotterdam") || m.contradictsCity("amsterdam") {
		t.Fatal("market's own cities must not contradict")
	}
	if m.contradictsCity("") {
		t.Fatal("empty city never contradicts")
	}
	if m.contradictsCity("Springfield") {
		t.Fatal("unknown city must not contradict")
	}
}

func TestDomainCC(t *testing.T) {
	for in, want := range map[string]string{
		"tuv.nl": "nl", "www.apexsecurity.nl": "nl", "m.rbi.org.in": "in",
		"talentleads.co.in": "in", "example.com": "", "localhost": "", "": "",
	} {
		if got := domainCC(in); got != want {
			t.Fatalf("domainCC(%q)=%q want %q", in, got, want)
		}
	}
}

func TestHostOfStripsUserinfo(t *testing.T) {
	if got := hostOf("https://security@tuv.nl/"); got != "tuv.nl" {
		t.Fatalf("hostOf userinfo: %q", got)
	}
	if got := hostOf("https://www.tuv.nl/security/"); got != "www.tuv.nl" {
		t.Fatalf("hostOf plain: %q", got)
	}
	if got := DomainFromURL("https://info@example.com/x"); got != "example.com" {
		t.Fatalf("DomainFromURL userinfo: %q", got)
	}
}

func TestClassifyHarvestedEmail(t *testing.T) {
	if got := classifyHarvestedEmail("info@tuv.nl"); got != "role" {
		t.Fatalf("info@ -> %q", got)
	}
	if got := classifyHarvestedEmail("sales@novahr.co.in"); got != "role" {
		t.Fatalf("sales@ -> %q", got)
	}
	if got := classifyHarvestedEmail("someone@gmail.com"); got != "free" {
		t.Fatalf("gmail -> %q", got)
	}
	if got := classifyHarvestedEmail("j.vermeer@kynexis.nl"); got != "found" {
		t.Fatalf("personal harvested -> %q", got)
	}
}

func TestGeoStageDropsForeignEvidence(t *testing.T) {
	lg := newFakeLedger(-1)
	j := &job{id: "gj1", query: "Netherlands-based cybersecurity SaaS companies", ledger: lg, store: &fakeStore{}}
	leads := []*Lead{
		{Company: "Tuv", Website: "https://www.tuv.nl/", Source: "web_search"},
		{Company: "SOEBIT", Website: "https://m.rbi.org.in/x", City: "India", Source: "linkedin"},
		{Company: "Apex", Website: "https://www.apexsecurity.nl/", Source: "web_search"},
		{Company: "NoGeo", Source: "web_search"}, // no evidence -> kept
	}
	kept, err := j.geoStage(context.Background(), leads)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 3 {
		t.Fatalf("kept %d: %+v", len(kept), geoTestNames(kept))
	}
	for _, k := range kept {
		if k.Company == "SOEBIT" {
			t.Fatal("foreign-market lead kept")
		}
	}
	var st map[string]any
	for _, s := range lg.stages {
		if s["stage"] == "geo_relevance" {
			st = s
		}
	}
	if st == nil || st["rejected_count"] != 1 || st["output_count"] != 3 {
		t.Fatalf("stage: %+v", st)
	}
	// no market in query -> stage is a no-op, no stage row
	j2 := &job{id: "gj2", query: "IT staffing companies", ledger: lg, store: &fakeStore{}}
	got, err := j2.geoStage(context.Background(), leads)
	if err != nil || len(got) != len(leads) {
		t.Fatalf("no-market pass-through: %d %v", len(got), err)
	}
	n := 0
	for _, s := range lg.stages {
		if s["stage"] == "geo_relevance" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expected exactly one geo_relevance stage row, got %d", n)
	}
}

func geoTestNames(ls []*Lead) []string {
	out := []string{}
	for _, l := range ls {
		out = append(out, l.Company)
	}
	return out
}
