package signalscan

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/provcall"
)

// vectors.json is Python-generated (legacy/python/scripts/gen_enrichment_vectors.py, section signalscan).
type vectorFile struct {
	SchemaVersion int `json:"schema_version"`
	IDs           []struct {
		WorkspaceID string          `json:"workspace_id"`
		LeadID      json.RawMessage `json:"lead_id"`
		SignalType  string          `json:"signal_type"`
		NaturalKey  string          `json:"natural_key"`
		ID          string          `json:"id"`
	} `json:"scan_signal_id"`
	Bands []struct {
		Total int64  `json:"total"`
		Band  string `json:"band"`
	} `json:"hiring_band"`
	Buckets []struct {
		Name   string  `json:"name"`
		TS     float64 `json:"ts"`
		Bucket string  `json:"bucket"`
	} `json:"period_bucket"`
	Now    float64 `json:"now_for_detection"`
	Detect []struct {
		Name  string `json:"name"`
		Leads []struct {
			ID      int64  `json:"id"`
			Company string `json:"company"`
		} `json:"leads"`
		Cap     *int `json:"cap"`
		Signals []struct {
			ID          string `json:"id"`
			WorkspaceID string `json:"workspace_id"`
			LeadID      int64  `json:"lead_id"`
			Company     string `json:"company"`
			SignalType  string `json:"signal_type"`
			Title       string `json:"title"`
			Description string `json:"description"`
			Source      string `json:"source"`
			SourceURL   string `json:"source_url"`
			Weight      int    `json:"weight"`
		} `json:"signals"`
	} `json:"detect_hiring_signals"`
	Extract []struct {
		Name   string `json:"name"`
		Result struct {
			Value map[string]any `json:"value"`
		} `json:"result"`
	} `json:"extract_hiring_data"`
	Boosts []struct {
		Name  string `json:"name"`
		Leads []struct {
			ID    int64  `json:"id"`
			Score *int64 `json:"score"`
		} `json:"leads"`
		Signals []struct {
			LeadID int64 `json:"lead_id"`
			Weight *int  `json:"weight"`
		} `json:"signals"`
		Boosted int `json:"boosted"`
		Updates []struct {
			ID    int64  `json:"id"`
			Score int64  `json:"score"`
			Tier  string `json:"score_tier"`
		} `json:"updates"`
	} `json:"apply_signal_boosts"`
}

func loadVectors(t *testing.T) vectorFile {
	t.Helper()
	raw, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectorFile
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func unixTime(f float64) time.Time {
	sec := int64(f)
	if float64(sec) > f {
		sec--
	}
	return time.Unix(sec, int64((f-float64(sec))*1e9)).UTC()
}

func TestScanSignalIDVectors(t *testing.T) {
	v := loadVectors(t)
	if v.SchemaVersion != SchemaVersion {
		t.Fatalf("schema version %d != %d", v.SchemaVersion, SchemaVersion)
	}
	checked := 0
	for _, c := range v.IDs {
		var lead int64
		if err := json.Unmarshal(c.LeadID, &lead); err != nil {
			continue // Python-only type quirks (True, "14")
		}
		if got := ScanSignalID(c.WorkspaceID, lead, c.SignalType, c.NaturalKey, time.Time{}); got != c.ID {
			t.Errorf("id(%q,%d,%q,%q) = %s, want %s", c.WorkspaceID, lead, c.SignalType, c.NaturalKey, got, c.ID)
		}
		checked++
	}
	if checked < 15 {
		t.Fatalf("only %d id vectors checked", checked)
	}
}

func TestPeriodBucketAndBandVectors(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.Buckets {
		if got := PeriodBucket(unixTime(c.TS)); got != c.Bucket {
			t.Errorf("%s: bucket(%v) = %s, want %s", c.Name, c.TS, got, c.Bucket)
		}
	}
	for _, c := range v.Bands {
		if got := HiringBand(c.Total); got != c.Band {
			t.Errorf("band(%d) = %s, want %s", c.Total, got, c.Band)
		}
	}
}

// With a timestamp the natural key gets the Python "|<period bucket>" suffix.
func TestScanSignalIDAppendsPeriodBucket(t *testing.T) {
	ts := unixTime(1787835600) // 2026-W35
	a := ScanSignalID("ws-alpha", 14, "hiring", "moderate", ts)
	b := ScanSignalID("ws-alpha", 14, "hiring", "moderate|2026-W35", time.Time{})
	if a != b || a != "45ab93d19dbfbcaf3d1b35a110cc33afc504609a2e3a390f607070c626428bef" {
		t.Fatalf("%s %s", a, b)
	}
	if c := ScanSignalID("ws-alpha", 14, "hiring", "moderate", ts.Add(7*24*time.Hour)); c == a {
		t.Fatal("next week must give a new id")
	}
}

func TestExtractHiringDataVectors(t *testing.T) {
	v := loadVectors(t)
	// Only the shapes a provider-call result can take ({"success","fields":{...}}); the
	// ".data" / no-attribute cases exist only on Python test doubles.
	fields := map[string]string{
		"fields_str":         `{"hiring_signals":"{\"x\": 2}"}`,
		"fields_dict":        `{"hiring_signals":{"x":3}}`,
		"fields_empty_value": `{"hiring_signals":""}`,
		"fields_bad":         `{"hiring_signals":"x"}`,
		"fields_number":      `{"hiring_signals":5}`,
	}
	n := 0
	for _, c := range v.Extract {
		f, ok := fields[c.Name]
		if !ok {
			continue
		}
		n++
		res := provcall.Result{Status: "ok", Output: json.RawMessage(`{"success":true,"fields":` + f + `}`)}
		got := ExtractHiringData(res)
		if (got == nil) != (c.Result.Value == nil) || (got != nil && len(got) != len(c.Result.Value)) {
			t.Errorf("%s: got %v want %v", c.Name, got, c.Result.Value)
		}
	}
	if n != len(fields) {
		t.Fatalf("matched %d of %d cases", n, len(fields))
	}
	if ExtractHiringData(provcall.Result{Status: "timeout"}) != nil || ExtractHiringData(provcall.Result{Status: "error", Output: json.RawMessage(`{"success":false,"fields":{"hiring_signals":{"total_jobs":9}}}`)}) != nil {
		t.Fatal("non-ok results carry no hiring data")
	}
}

func TestBoostsMatchPython(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.Boosts {
		scores := map[int64]*int64{}
		for _, l := range c.Leads {
			scores[l.ID] = l.Score
		}
		var sigs []Detected
		for _, s := range c.Signals {
			w := 0
			if s.Weight != nil {
				w = *s.Weight
			}
			sigs = append(sigs, Detected{LeadID: s.LeadID, Weight: w})
		}
		ups := PlanBoosts(sigs, func(id int64) (*int64, bool) {
			if id == 0 {
				return nil, false // lead id 0 never reaches the store
			}
			s, ok := scores[id]
			return s, ok
		})
		if len(ups) != c.Boosted || len(ups) != len(c.Updates) {
			t.Errorf("%s: %d updates, want %d", c.Name, len(ups), len(c.Updates))
			continue
		}
		for i, u := range c.Updates {
			if ups[i].LeadID != u.ID || ups[i].Score != u.Score || ups[i].Tier != u.Tier {
				t.Errorf("%s[%d]: got %+v want %+v", c.Name, i, ups[i], u)
			}
		}
	}
}

type scripted struct {
	byCompany map[string]provcall.Result
	errFor    map[string]bool
	calls     []provcall.Call
}

func (s *scripted) Call(_ context.Context, c provcall.Call) (provcall.Result, error) {
	s.calls = append(s.calls, c)
	var in struct {
		Company string `json:"company"`
	}
	_ = json.Unmarshal(c.Input, &in)
	if s.errFor[in.Company] {
		return provcall.Result{}, errors.New("boom")
	}
	return s.byCompany[in.Company], nil
}

func okHiring(sig string) provcall.Result {
	return provcall.Result{Status: "ok", Output: json.RawMessage(`{"success":true,"fields":{"hiring_signals":` + sig + `}}`)}
}

func TestDetectHiringVectors(t *testing.T) {
	v := loadVectors(t)
	// The scripted provider answers of gen_enrichment_vectors.py (data= cases
	// become fields.hiring_signals; the two ".data wins" cases are Python-double only).
	hs := func(total int, titles string) string {
		s := `{"total_jobs":` + itoa(total)
		if titles != "" {
			s += `,"top_titles":` + titles
		}
		return s + `}`
	}
	all8 := map[string]provcall.Result{"A": okHiring(hs(8, `["t"]`)), "B": okHiring(hs(8, `["t"]`)), "C": okHiring(hs(8, `["t"]`))}
	script := map[string]struct {
		res  map[string]provcall.Result
		errs map[string]bool
	}{
		"single_high_band":                           {res: map[string]provcall.Result{"A": okHiring(hs(30, `["SDR","AE","CS","VP"]`))}},
		"fields_json_string":                         {res: map[string]provcall.Result{"A": okHiring(`"{\"total_jobs\": 7, \"top_titles\": [\"X\"]}"`)}},
		"fields_dict":                                {res: map[string]provcall.Result{"A": okHiring(hs(3, `[]`))}},
		"fields_bad_json_skipped":                    {res: map[string]provcall.Result{"A": okHiring(`"{not json"`)}},
		"fields_json_list_skipped":                   {res: map[string]provcall.Result{"A": okHiring(`"[1]"`)}},
		"no_data_no_fields":                          {res: map[string]provcall.Result{"A": {Status: "ok", Output: json.RawMessage(`{"success":true,"fields":{}}`)}}},
		"failure_result_skipped":                     {res: map[string]provcall.Result{"A": {Status: "error", Output: json.RawMessage(`{"success":false,"fields":{"hiring_signals":{"total_jobs":9}}}`)}}},
		"zero_jobs_skipped":                          {res: map[string]provcall.Result{"A": okHiring(hs(0, `[]`))}},
		"missing_total_jobs_skipped":                 {res: map[string]provcall.Result{"A": okHiring(`{"top_titles":["x"]}`)}},
		"weight_capped_at_10":                        {res: map[string]provcall.Result{"A": okHiring(hs(500, `["x"]`))}},
		"weight_equals_total_when_small":             {res: map[string]provcall.Result{"A": okHiring(hs(4, `["x"]`))}},
		"negative_total_skipped":                     {res: map[string]provcall.Result{"A": okHiring(hs(-3, `["x"]`))}},
		"top_titles_first_three":                     {res: map[string]provcall.Result{"A": okHiring(hs(5, `["a","b","c","d"]`))}},
		"top_titles_missing":                         {res: map[string]provcall.Result{"A": okHiring(hs(5, ``))}},
		"lead_without_company_skipped":               {res: map[string]provcall.Result{"B": okHiring(hs(25, `["t"]`))}},
		"provider_exception_isolated":                {res: map[string]provcall.Result{"B": okHiring(hs(25, `["t"]`))}, errs: map[string]bool{"A": true}},
		"cap_limits_leads_scanned":                   {res: all8},
		"cap_zero_scans_none":                        {res: all8},
		"band_boundaries_one_lead_each":              {res: map[string]provcall.Result{"C1": okHiring(hs(5, `["x"]`)), "C2": okHiring(hs(6, `["x"]`)), "C3": okHiring(hs(20, `["x"]`)), "C4": okHiring(hs(21, `["x"]`)), "C5": okHiring(hs(51, `["x"]`))}},
		"unicode_company_title":                      {res: map[string]provcall.Result{"Société Générale": okHiring(hs(12, `["Ingénieur"]`))}},
		"same_lead_twice_same_id":                    {res: map[string]provcall.Result{"A": okHiring(hs(12, `["x"]`))}},
		"data_dict_wins_over_fields":                 {},
		"empty_data_dict_is_present_but_falsy_total": {},
	}
	ran := 0
	for _, c := range v.Detect {
		sc, ok := script[c.Name]
		if !ok {
			t.Fatalf("no script for vector %s", c.Name)
		}
		if sc.res == nil {
			continue // Python-double-only
		}
		ran++
		var leads []Lead
		for _, l := range c.Leads {
			leads = append(leads, Lead{ID: l.ID, Company: l.Company})
		}
		cap := len(leads)
		if c.Cap != nil {
			cap = *c.Cap
		}
		py := &scripted{byCompany: sc.res, errFor: sc.errs}
		d := Deps{Py: py, Now: func() time.Time { return unixTime(v.Now) }}
		got := detectHiring(context.Background(), d, "ws-alpha", leads, cap)
		if len(got) != len(c.Signals) {
			t.Errorf("%s: %d signals, want %d (%+v)", c.Name, len(got), len(c.Signals), got)
			continue
		}
		for i, w := range c.Signals {
			g := got[i]
			if g.ID != w.ID || g.WorkspaceID != w.WorkspaceID || g.LeadID != w.LeadID || g.Company != w.Company || g.SignalType != w.SignalType ||
				g.Title != w.Title || g.Description != w.Description || g.Source != w.Source || g.SourceURL != w.SourceURL || g.Weight != w.Weight {
				t.Errorf("%s[%d]: got %+v want %+v", c.Name, i, g, w)
			}
		}
		for _, call := range py.calls {
			if call.Provider != "jobspy" || call.WorkspaceID != "ws-alpha" || call.CallID == "" {
				t.Errorf("%s: bad call %+v", c.Name, call)
			}
		}
	}
	if ran < 20 {
		t.Fatalf("only %d detect vectors ran", ran)
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }
