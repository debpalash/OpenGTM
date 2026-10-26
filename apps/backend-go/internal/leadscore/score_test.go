package leadscore

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"testing"
)

type vectors struct {
	ReliabilitySwing float64 `json:"reliability_swing"`
	Cases            []struct {
		Lead        map[string]any `json:"lead"`
		Reliability *float64       `json:"reliability"`
		BaseScore   int            `json:"base_score"`
		Score       int            `json:"score"`
		Tier        string         `json:"tier"`
	} `json:"cases"`
	NormalizeBand []struct {
		In  any    `json:"in"`
		Out string `json:"out"`
	} `json:"normalize_band"`
	Tiers []struct {
		Score float64 `json:"score"`
		Tier  string  `json:"tier"`
	} `json:"tiers"`
	Crashes []struct {
		Lead  map[string]any `json:"lead"`
		Score *int           `json:"score"`
		Error *string        `json:"error"`
	} `json:"crashes"`
}

func loadVectors(t *testing.T) vectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/score_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestScoreVectors(t *testing.T) {
	v := loadVectors(t)
	if len(v.Cases) != 300 {
		t.Fatalf("cases = %d", len(v.Cases))
	}
	if ReliabilitySwing != v.ReliabilitySwing {
		t.Fatalf("swing = %v", ReliabilitySwing)
	}
	for i, c := range v.Cases {
		l := MapLead(c.Lead)
		base, err := ScoreE(l, nil)
		if err != nil || base != c.BaseScore && c.Reliability == nil {
			t.Errorf("case %d base = %d (%v), want %d", i, base, err, c.BaseScore)
		}
		if got := Score(l, c.Reliability); got != c.Score {
			t.Errorf("case %d score = %d, want %d (%v)", i, got, c.Score, c.Lead)
		}
		if got := Tier(Score(l, c.Reliability)); got != c.Tier {
			t.Errorf("case %d tier = %s, want %s", i, got, c.Tier)
		}
	}
	for i, c := range v.Crashes {
		got, err := ScoreE(MapLead(c.Lead), nil)
		if c.Error != nil {
			var pe *PyError
			if !errors.As(err, &pe) || pe.Kind != *c.Error {
				t.Errorf("crash %d err = %v, want %s", i, err, *c.Error)
			}
			continue
		}
		if err != nil || got != *c.Score {
			t.Errorf("crash %d = %d (%v), want %d", i, got, err, *c.Score)
		}
	}
}

func f(x float64) *float64 { return &x }

func TestReliabilityNudgeBankersRounding(t *testing.T) {
	// 8*(r-0.5) = +/-0.5 exactly at r = 0.5625 / 0.4375.
	cases := []struct {
		base int
		r    float64
		want int
	}{
		{10, 0.5625, 10}, {11, 0.5625, 12}, {12, 0.5625, 12}, {13, 0.5625, 14},
		{10, 0.4375, 10}, {11, 0.4375, 10}, {12, 0.4375, 12}, {13, 0.4375, 12},
		{0, 0.0, 0}, {100, 1.0, 100}, {0, 0.4375, 0},
	}
	for _, c := range cases {
		if got := ApplyReliability(c.base, f(c.r)); got != c.want {
			t.Errorf("ApplyReliability(%d,%v) = %d, want %d", c.base, c.r, got, c.want)
		}
	}
	if ApplyReliability(37, nil) != 37 {
		t.Error("nil reliability must be identity")
	}
}

func TestHasPropertiesTreatNAAndNanAsAbsent(t *testing.T) {
	for _, bad := range []string{"N/A", "nan", ""} {
		l := MapLead{"company": "x", "website": bad, "email": bad, "phone": bad, "linkedin_url": bad, "contact_person": bad}
		if got := Score(l, nil); got != 0 {
			t.Errorf("%q scored %d", bad, got)
		}
	}
	// email needs an "@"; "NaN" (case differs) is a real value.
	if Score(MapLead{"email": "noatsign"}, nil) != 0 {
		t.Error("email without @")
	}
	if Score(MapLead{"website": "NaN"}, nil) != 10 {
		t.Error("NaN is not nan")
	}
}

func TestNormalizeBandVectors(t *testing.T) {
	v := loadVectors(t)
	if len(v.NormalizeBand) != 62 {
		t.Fatalf("n = %d", len(v.NormalizeBand))
	}
	for _, c := range v.NormalizeBand {
		if got := NormalizeBand(c.In); got != c.Out {
			t.Errorf("NormalizeBand(%#v) = %q, want %q", c.In, got, c.Out)
		}
	}
}

func TestTierBoundaries(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.Tiers {
		if got := TierFloat(c.Score); got != c.Tier {
			t.Errorf("TierFloat(%v) = %s, want %s", c.Score, got, c.Tier)
		}
	}
	for s, want := range map[int]string{24: "unqualified", 25: "cold", 49: "cold", 50: "warm", 74: "warm", 75: "hot", 100: "hot", 101: "unqualified", -1: "unqualified"} {
		if got := Tier(s); got != want {
			t.Errorf("Tier(%d) = %s, want %s", s, got, want)
		}
	}
}

func TestScoreCapAt100(t *testing.T) {
	l := MapLead{"website": "w", "email": "a@b", "phone": "1", "linkedin_url": "l", "company_size": "500+",
		"specialization": "IT Staffing", "industry_tags": "IT Staffing, Talent Acquisition", "city": "Pune",
		"contact_person": "x", "decision_makers": `[1,2]`, "twitter_url": "t", "founded_year": "1", "revenue_range": "1", "glassdoor_rating": "1"}
	if got := Score(l, nil); got != 100 {
		t.Errorf("score = %d", got)
	}
	// the nudge applies after the cap: 100 + 4 clamps to 100, 100 - 4 = 96.
	if Score(l, f(1)) != 100 || Score(l, f(0)) != 96 {
		t.Error("nudge after cap")
	}
}

func TestSpecializationSubstringBothDirections(t *testing.T) {
	// pref in spec ("it staffing" inside a longer text) and spec in pref ("staffing").
	for _, s := range []string{"Leading IT Staffing firm", "staffing", "HR SaaS", "hr saas platform"} {
		if got := Score(MapLead{"specialization": s}, nil); got != 20 {
			t.Errorf("%q = %d, want 20", s, got)
		}
	}
	// industry-only match gives the half credit; "HR Consultancy" is an industry, not a preferred specialization.
	for _, s := range []string{"HR Consultancy", "hr consultancy firm", "Executive Search"} {
		if got := Score(MapLead{"specialization": s}, nil); got != 10 {
			t.Errorf("%q = %d, want 10", s, got)
		}
	}
	if Score(MapLead{"specialization": "Plumbing"}, nil) != 0 {
		t.Error("no match")
	}
	// exact-case city membership.
	if Score(MapLead{"city": "pune"}, nil) != 0 || Score(MapLead{"city": "Pune"}, nil) != 10 || Score(MapLead{"city": "Chennai"}, nil) != 5 {
		t.Error("city tiers")
	}
}

func TestDecisionMakersInvalidJSONIgnored(t *testing.T) {
	for _, s := range []string{"not json", "{", "[1]", "5", "null", "true", `""`, `"a"`, "[1,2"} {
		if got := Score(MapLead{"decision_makers": s}, nil); got != 0 {
			t.Errorf("%q = %d, want 0", s, got)
		}
	}
	for _, s := range []string{"[1,2]", `{"a":1,"b":2}`, `"ab"`, " [NaN, 1] ", `{"a":1,"a":2,"b":3}`} {
		if got := Score(MapLead{"decision_makers": s}, nil); got != 5 {
			t.Errorf("%q = %d, want 5", s, got)
		}
	}
	if Score(MapLead{"decision_makers": `{"a":1,"a":2}`}, nil) != 0 {
		t.Error("duplicate keys count once")
	}
}
