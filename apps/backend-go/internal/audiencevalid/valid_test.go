package audiencevalid

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadsread"
)

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		t.Fatal(err)
	}
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(b.String())
}

// keySeq lists every object key of a JSON document in document order.
func keySeq(t *testing.T, raw string) []string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var keys []string
	var stack []bool // true = object
	expectKey := false
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch x := tok.(type) {
		case json.Delim:
			switch x {
			case '{':
				stack = append(stack, true)
				expectKey = true
				continue
			case '[':
				stack = append(stack, false)
				expectKey = false
				continue
			default:
				stack = stack[:len(stack)-1]
			}
		case string:
			if expectKey && len(stack) > 0 && stack[len(stack)-1] {
				keys = append(keys, x)
				expectKey = false
				continue
			}
		}
		expectKey = len(stack) > 0 && stack[len(stack)-1]
	}
	return keys
}

func TestFilterKeysAre14(t *testing.T) {
	if len(FilterKeys) != 14 {
		t.Fatalf("FilterKeys = %d", len(FilterKeys))
	}
}

func TestValidateFiltersMatchesVectors(t *testing.T) {
	var v struct {
		Cases []struct {
			Name    string         `json:"name"`
			Filters map[string]any `json:"filters"`
			OK      bool           `json:"ok"`
			Result  map[string]any `json:"result"`
			Error   string         `json:"error"`
		} `json:"cases"`
	}
	readJSON(t, "testdata/filters.json", &v)
	if len(v.Cases) != 32 {
		t.Fatalf("cases = %d", len(v.Cases))
	}
	for _, c := range v.Cases {
		t.Run(c.Name, func(t *testing.T) {
			got, err := ValidateFilters(c.Filters)
			if c.OK {
				if err != nil {
					t.Fatalf("unexpected error %v", err)
				}
				if jsonOf(t, got) != jsonOf(t, c.Result) {
					t.Fatalf("result %s want %s", jsonOf(t, got), jsonOf(t, c.Result))
				}
				return
			}
			var ve *ValueError
			if !errors.As(err, &ve) || err.Error() != c.Error {
				t.Fatalf("error %v want %q", err, c.Error)
			}
		})
	}
}

func TestValidateFiltersBigAndGoInts(t *testing.T) {
	if _, err := ValidateFilters(map[string]any{"min_score": 1, "max_score": int64(2)}); err != nil {
		t.Fatal(err)
	}
	huge := map[string]any{"min_score": json.Number("99999999999999999999999"), "max_score": json.Number("5")}
	if _, err := ValidateFilters(huge); err == nil || err.Error() != "min_score cannot exceed max_score" {
		t.Fatalf("got %v", err)
	}
	if _, err := ValidateFilters(map[string]any{"min_score": json.Number("1e2")}); err == nil || err.Error() != "min_score must be an integer" {
		t.Fatalf("exp float: %v", err)
	}
}

func TestToLeadFilterMatchesQueryLeadsPageVectors(t *testing.T) {
	var v struct {
		Cases []struct {
			Filter map[string]any `json:"filter"`
		} `json:"cases"`
	}
	readJSON(t, "../../testdata/leads_vectors.json", &v)
	checked := 0
	for i, c := range v.Cases {
		if _, ok := c.Filter["lead_ids_companies"]; ok {
			continue
		}
		if _, err := ValidateFilters(c.Filter); err != nil {
			continue
		}
		got, err := ToLeadFilter(c.Filter)
		if err != nil {
			t.Fatalf("case %d %v: %v", i, c.Filter, err)
		}
		raw, _ := json.Marshal(c.Filter)
		var want leadsread.Filter
		if err := json.Unmarshal(raw, &want); err != nil {
			t.Fatal(err)
		}
		if len(want.JobIDs) == 0 {
			want.JobIDs = nil // an empty list and an unset one filter alike
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("case %d %s: got %+v want %+v", i, raw, got, want)
		}
		checked++
	}
	if checked < 50 {
		t.Fatalf("only %d vector cases exercised", checked)
	}
}

func TestToLeadFilterSemantics(t *testing.T) {
	tr, fa := true, false
	lo, hi := 1, 9
	got, err := ToLeadFilter(map[string]any{
		"city": "A", "state": "", "score_tier": "hot", "status": "new", "source": "s",
		"company_size": "1-10", "specialization": "x", "has_email": true, "has_phone": false,
		"has_website": true, "min_score": json.Number("1"), "max_score": 9, "search": "q",
		"job_ids": []any{"j1", "j2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := leadsread.Filter{City: "A", ScoreTier: "hot", Status: "new", Source: "s", CompanySize: "1-10",
		Specialization: "x", HasEmail: &tr, HasPhone: &fa, HasWebsite: &tr, MinScore: &lo, MaxScore: &hi,
		Search: "q", JobIDs: []string{"j1", "j2"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
	// falsy job_ids / search are unset; a bare string job_ids iterates characters like list("ab").
	got, _ = ToLeadFilter(map[string]any{"job_ids": []any{}, "search": ""})
	if got.JobIDs != nil || got.Search != "" {
		t.Fatalf("falsy not unset: %+v", got)
	}
	got, _ = ToLeadFilter(map[string]any{"job_ids": "ab"})
	if !reflect.DeepEqual(got.JobIDs, []string{"a", "b"}) {
		t.Fatalf("string job_ids: %+v", got.JobIDs)
	}
	if _, err := ToLeadFilter(map[string]any{"job_ids": []any{1}}); err == nil {
		t.Fatal("non-string job id accepted")
	}
}

func TestAccountKeyMatchesVectors(t *testing.T) {
	var v struct {
		Cases []struct {
			Snapshot map[string]any `json:"snapshot"`
			Key      string         `json:"key"`
			Company  string         `json:"company"`
			Error    string         `json:"error"`
		} `json:"cases"`
	}
	readJSON(t, "testdata/account_key.json", &v)
	if len(v.Cases) != 110 {
		t.Fatalf("cases = %d", len(v.Cases))
	}
	for i, c := range v.Cases {
		key, company := AccountKey(c.Snapshot)
		if c.Error != "" {
			// D3: an unparseable URL falls back to the company key.
			blank := map[string]any{"company": c.Snapshot["company"]}
			wk, wc := AccountKey(blank)
			if key != wk || company != wc {
				t.Errorf("case %d %v: fallback got (%q,%q) want (%q,%q)", i, c.Snapshot, key, company, wk, wc)
			}
			continue
		}
		if key != c.Key || company != c.Company {
			t.Errorf("case %d %v: got (%q,%q) want (%q,%q)", i, c.Snapshot, key, company, c.Key, c.Company)
		}
	}
}

func TestAccountKeyBadURLNoPanic(t *testing.T) {
	for _, w := range []any{"http://[bad", "https://[", "[::1", "https://a.b]/", "https://[v1.x]/", "https://[1.2.3.4]/", []any{"a.com"}, map[string]any{"a": json.Number("1")}, "https://%zz", "\x00\xff"} {
		key, company := AccountKey(map[string]any{"website": w, "company": "Acme"})
		if key == "" || company == "" {
			t.Errorf("%v -> empty (%q,%q)", w, key, company)
		}
	}
}

type rollupAudience struct {
	Name    string           `json:"name"`
	Members []map[string]any `json:"members"`
	Signals []struct {
		LeadID      int `json:"lead_id"`
		Count       int `json:"count"`
		WeightTotal int `json:"weight_total"`
	} `json:"signals"`
	Pages []struct {
		Limit    int             `json:"limit"`
		Offset   int             `json:"offset"`
		Response json.RawMessage `json:"response"`
	} `json:"pages"`
}

func (a rollupAudience) inputs() ([]Member, map[int]Signal) {
	var ms []Member
	for _, m := range a.Members {
		id, _ := m["id"].(json.Number).Int64()
		snap := map[string]any{}
		for k, v := range m {
			if k != "id" {
				snap[k] = v
			}
		}
		ms = append(ms, Member{LeadID: int(id), Snapshot: snap})
	}
	sig := map[int]Signal{}
	for _, s := range a.Signals {
		sig[s.LeadID] = Signal{Count: s.Count, Weight: s.WeightTotal}
	}
	return ms, sig
}

func seqOf(ms []Member) func(func(Member, error) bool) {
	return func(yield func(Member, error) bool) {
		for _, m := range ms {
			if !yield(m, nil) {
				return
			}
		}
	}
}

func compact(t *testing.T, raw []byte) string {
	t.Helper()
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestRollupMatchesPythonOutput(t *testing.T) {
	var v struct {
		Audiences []rollupAudience `json:"audiences"`
	}
	readJSON(t, "testdata/accounts_rollup.json", &v)
	n := 0
	// The generator inserts every audience's signals into one table that is
	// never cleared between audiences, so rows accumulate per lead_id.
	acc := map[int]Signal{}
	for _, a := range v.Audiences {
		ms, own := a.inputs()
		for id, s := range own {
			cur := acc[id]
			acc[id] = Signal{Count: cur.Count + s.Count, Weight: cur.Weight + s.Weight}
		}
		sig := map[int]Signal{}
		for id, s := range acc {
			sig[id] = s
		}
		for _, p := range a.Pages {
			res, err := Rollup(seqOf(ms), sig, p.Limit, p.Offset, 1000000)
			if err != nil {
				t.Fatalf("%s: %v", a.Name, err)
			}
			got, want := jsonOf(t, res), compact(t, p.Response)
			if !reflect.DeepEqual(keySeq(t, got), keySeq(t, want)) {
				t.Fatalf("%s limit=%d offset=%d key order differs\n got %s\nwant %s", a.Name, p.Limit, p.Offset, got, want)
			}
			if got != want {
				t.Fatalf("%s limit=%d offset=%d\n got %s\nwant %s", a.Name, p.Limit, p.Offset, got, want)
			}
			n++
		}
	}
	if n < 10 {
		t.Fatalf("only %d pages", n)
	}
}

func TestRollupSortStable(t *testing.T) {
	// Equal weight, score, contacts: order by company.lower(); identical company names keep first-seen order.
	snap := func(co, site string) map[string]any {
		return map[string]any{"company": co, "website": site, "score": json.Number("5")}
	}
	ms := []Member{
		{1, snap("b", "b1.example")}, {2, snap("B", "b2.example")}, {3, snap("a", "a.example")}, {4, snap("b", "b3.example")},
	}
	res, err := Rollup(seqOf(ms), nil, 50, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, a := range res.Accounts {
		keys = append(keys, a.Key)
	}
	if want := []string{"a.example", "b1.example", "b2.example", "b3.example"}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("keys %v want %v", keys, want)
	}
}

func TestRollupTooLarge(t *testing.T) {
	ms := []Member{{1, nil}, {2, nil}, {3, nil}}
	if _, err := Rollup(seqOf(ms), nil, 50, 0, 3); err != nil {
		t.Fatalf("at the bound: %v", err)
	}
	if _, err := Rollup(seqOf(ms), nil, 50, 0, 2); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %v", err)
	}
	boom := errors.New("boom")
	seq := func(yield func(Member, error) bool) { yield(Member{}, boom) }
	if _, err := Rollup(seq, nil, 50, 0, 2); !errors.Is(err, boom) {
		t.Fatalf("iter error: %v", err)
	}
}

func TestDecisionMakerTerms(t *testing.T) {
	titles := map[string]int{"CEO": 1, "Chief of Staff": 1, "VP Sales": 1, "Vice President": 1, "Head of X": 1,
		"Director": 1, "Co-Owner": 1, "Founder": 1, "CTO": 1, "CMO": 1, "Sales Rep": 0, "Supervisor": 0, "": 0, "Doctor Who": 1}
	for title, want := range titles {
		res, err := Rollup(seqOf([]Member{{1, map[string]any{"company": "C", "contact_title": title}}}), nil, 50, 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		if got := res.Accounts[0].DecisionMakers; got != want {
			t.Errorf("%q: decision_makers=%d want %d", title, got, want)
		}
	}
}

func TestPyRoundHalfEvenVectors(t *testing.T) {
	cases := []struct {
		x      float64
		nd     int
		expect float64
	}{
		{0.05, 1, 0.1}, {0.15, 1, 0.1}, {0.25, 1, 0.2}, {0.35, 1, 0.3}, {2.675, 2, 2.67}, {1.5, 0, 2}, {2.5, 0, 2},
		{0.5, 0, 0}, {-0.5, 0, 0}, {-1.5, 0, -2}, {-2.5, 0, -2}, {-2.675, 2, -2.67}, {-0.05, 1, -0.1},
		{38.45, 1, 38.5}, {7.45, 1, 7.5}, {1e300, 1, 1e300}, {math.Inf(1), 1, math.Inf(1)}, {33.333333333333336, 0, 33},
		{1234.5678, -2, 1200}, {1250, -2, 1200}, {1350, -2, 1400},
	}
	for _, c := range cases {
		if got := PyRound(c.x, c.nd); got != c.expect {
			t.Errorf("PyRound(%v,%d)=%v want %v", c.x, c.nd, got, c.expect)
		}
	}
	if !math.IsNaN(PyRound(math.NaN(), 1)) {
		t.Error("NaN")
	}
	if z := PyRound(-0.04, 1); !math.Signbit(z) || z != 0 {
		t.Errorf("-0.04 -> %v want -0.0", z)
	}
}
