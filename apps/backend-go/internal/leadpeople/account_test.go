package leadpeople

import "testing"

func TestAccountVectors(t *testing.T) {
	var v struct {
		Briefs []struct {
			Query   string         `json:"query"`
			Brief   map[string]any `json:"brief"`
			Request map[string]any `json:"request"`
		} `json:"briefs"`
		BriefIDs []struct {
			Brief map[string]any `json:"brief"`
			ID    string         `json:"id"`
		} `json:"brief_ids"`
		Fits []struct {
			Brief map[string]any `json:"brief"`
			Lead  map[string]any `json:"lead"`
			Fit   map[string]any `json:"fit"`
		} `json:"fits"`
	}
	loadVec(t, "account.json", &v)
	// Python bug fixed, not mirrored: "build a list of 30 B2B software companies"
	// came back with the company type "of 30 B2B software".
	fixed := map[string]string{
		"Build me a list of 30 B2B software companies in Canada": "B2B software",
		"build a list of 30 B2B software companies in Canada":    "B2B software",
	}
	for _, c := range v.Briefs {
		got := BuildAccountDiscoveryBrief(c.Query)
		want := c.Brief
		if ct, ok := fixed[c.Query]; ok {
			want = cloneMap(want)
			want["company_types"] = []any{ct}
			want["brief_id"] = AccountDiscoveryBriefID(want)
		}
		sameJSON(t, "brief "+c.Query, got, want)
		req := ExtractAccountDiscoveryRequest(c.Query)
		if _, deviates := fixed[c.Query]; deviates {
			if req == nil {
				t.Errorf("request %q: want a complete request", c.Query)
			}
			continue
		}
		if (req == nil) != (c.Request == nil) {
			t.Errorf("request %q: got %v want %v", c.Query, req, c.Request)
		} else if req != nil {
			sameJSON(t, "request "+c.Query, req, c.Request)
		}
	}
	for _, c := range v.BriefIDs {
		if got := AccountDiscoveryBriefID(c.Brief); got != c.ID {
			t.Errorf("brief_id %v: got %s want %s", c.Brief, got, c.ID)
		}
	}
	for i, c := range v.Fits {
		got := EvaluateAccountFit(c.Lead, c.Brief, fixedNow)
		sameJSON(t, "fit#"+string(rune('a'+i)), got, c.Fit)
	}
}
