package leaddedup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
)

// orderedRec decodes a JSON object keeping key order; numbers become int64
// (no '.', 'e') or float64, as Python's json.loads would.
func orderedRec(t testing.TB, raw json.RawMessage) Rec {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		t.Fatalf("not an object: %s", raw)
	}
	r := Rec{Vals: map[string]any{}}
	for dec.More() {
		kt, _ := dec.Token()
		k := kt.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			t.Fatal(err)
		}
		r.Keys = append(r.Keys, k)
		r.Vals[k] = plain(t, v)
	}
	return r
}

func plain(t testing.TB, raw json.RawMessage) any {
	v, err := canonjson.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

type vectors struct {
	Compare []struct {
		A, B       json.RawMessage
		ResultJSON string `json:"result_json"`
	} `json:"compare_leads"`
	Normalize struct {
		Company []struct{ In, Out string }
		Domain  []struct{ In, Out string }
		Phone   []struct{ In, Out string }
		Email   []struct{ In, Out string }
		JW      []struct {
			A, B    string
			OutRepr string `json:"out_repr"`
		} `json:"jaro_winkler"`
	}
	Find []struct {
		Name           string
		Threshold      float64
		LeadsCount     int    `json:"leads_count"`
		ResultJSON     string `json:"result_json"`
		HashSeedStable bool   `json:"hash_seed_stable"`
		Leads          []json.RawMessage
		Projection     map[string]json.RawMessage
		ClustersCount  int `json:"clusters_count"`
		PairsCount     int `json:"pairs_count"`
		Spec           struct {
			Names, Suffixes, Cities []string
		}
	} `json:"find_duplicates"`
	Merge []struct {
		Name        string
		Master      json.RawMessage
		Duplicates  []json.RawMessage
		MergedJSON  string `json:"merged_json"`
		PythonError string `json:"python_error"`
	} `json:"merge_leads"`
	BlockCap int `json:"block_cap"`
}

func load(t testing.TB) *vectors {
	raw, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return &v
}

func TestNormalizeMatchesPython(t *testing.T) {
	v := load(t)
	for _, c := range v.Normalize.Company {
		if got := NormalizeCompany(c.In); got != c.Out {
			t.Errorf("company(%q) = %q, want %q", c.In, got, c.Out)
		}
	}
	for _, c := range v.Normalize.Domain {
		if got := NormalizeDomain(c.In); got != c.Out {
			t.Errorf("domain(%q) = %q, want %q", c.In, got, c.Out)
		}
	}
	for _, c := range v.Normalize.Phone {
		if got := NormalizePhone(c.In); got != c.Out {
			t.Errorf("phone(%q) = %q, want %q", c.In, got, c.Out)
		}
	}
	for _, c := range v.Normalize.Email {
		if got := NormalizeEmail(c.In); got != c.Out {
			t.Errorf("email(%q) = %q, want %q", c.In, got, c.Out)
		}
	}
}

// Expected values recorded from CPython 3.13 (legacy/python dedup.normalize_*):
// re \b is Unicode aware, str.lower is full case mapping, \s and strip use
// Python's whitespace set, urlsplit raises on bad brackets and NFKC netlocs.
func TestNormalizeUnicodeEdges(t *testing.T) {
	company := [][2]string{
		{"éco ltd", "éco"}, {"ltdé", "ltdé"}, {"éltd", "éltd"}, {"x_ltd", "x_ltd"}, {"a/ltd/b", "a//b"},
		{"ΑΣ Ltd", "ας"}, {"ΣΑ", "σα"}, {"ΑΣ.Β", "ασ β"}, {"İNC", "i̇nc"}, {"ſolutions", ""}, {"ıNC Foo", "foo"},
		{"Foo\u00a0Ltd", "foo"}, {"Foo\u001cBar", "foo bar"}, {"Foo\u0301 Ltd", "foo\u0301"},
		{"ltd\u0301x", "\u0301x"}, {"Ωmega Σ", "ωmega σ"}, {"ß Ltd", "ß"}, {"K Ltd", "k"},
	}
	for _, c := range company {
		if got := NormalizeCompany(c[0]); got != c[1] {
			t.Errorf("NormalizeCompany(%q) = %q, want %q", c[0], got, c[1])
		}
	}
	domain := [][2]string{
		{"https://WWW.Example.COM:8080/x?y#z", "example.com"}, {"user@host.example.com/path", "host.example.com"},
		{"http://[::1]/x", "::1"}, {"http://[::1", "http://[::1"}, {"http://[v1.x]/", "v1.x"},
		{"http://[1.2.3.4]/", "http://[1.2.3.4]"}, {"http://a\uff0fb.example.com", "http://a\uff0fb.example.com"},
		{"http://exa\tmple.com", "example.com"}, {"a\uff03b.example.com", "http://a\uff03b.example.com"},
		{"http://us:pw@[::ffff:1.2.3.4]:80", "::ffff:1.2.3.4"}, {"http://x.example.com%eth0/", "x.example.com%eth0"},
		{"\u2100.com", "http://\u2100.com"}, {"http://www.www.a.com", "www.a.com"}, {"http://%41.com", "%41.com"},
		{"HTTPS://X.COM?q=1", "x.com"}, {"  ", ""}, {"a b.com", "a b.com"}, {"http://:80/", ""},
	}
	for _, c := range domain {
		if got := NormalizeDomain(c[0]); got != c[1] {
			t.Errorf("NormalizeDomain(%q) = %q, want %q", c[0], got, c[1])
		}
	}
}

func TestJaroWinklerMatchesPython(t *testing.T) {
	v := load(t)
	for _, c := range v.Normalize.JW {
		got, _ := canonjson.FloatRepr(JaroWinkler(c.A, c.B))
		if got != c.OutRepr {
			t.Errorf("JaroWinkler(%q,%q) = %s, want %s", c.A, c.B, got, c.OutRepr)
		}
	}
}

func TestCompareMatchesPython(t *testing.T) {
	v := load(t)
	if len(v.Compare) != 40 {
		t.Fatalf("vectors: %d compare cases", len(v.Compare))
	}
	for i, c := range v.Compare {
		got, err := json.Marshal(Compare(orderedRec(t, c.A), orderedRec(t, c.B)))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != c.ResultJSON {
			t.Errorf("case %d:\n got %s\nwant %s", i, got, c.ResultJSON)
		}
	}
}

func TestCompareBoosts(t *testing.T) {
	mk := func(company, site, phone string) Rec {
		return Rec{Keys: []string{"id", "company", "website", "phone"},
			Vals: map[string]any{"id": int64(1), "company": company, "website": site, "phone": phone}}
	}
	if s := Compare(mk("Alpha", "https://x.example.com", ""), mk("Zeta", "x.example.com", "")).Score; s != 0.90 {
		t.Errorf("domain boost = %v, want 0.9", s)
	}
	if s := Compare(mk("Alpha", "", "+1 512 555 0014"), mk("Zeta", "", "(512) 555-0014")).Score; s != 0.92 {
		t.Errorf("phone boost = %v, want 0.92", s)
	}
}

func TestFindDuplicatesMatchesPython(t *testing.T) {
	v := load(t)
	n := 0
	for _, d := range v.Find {
		if d.Name == "synthetic_5000" {
			continue
		}
		n++
		t.Run(d.Name, func(t *testing.T) {
			leads := make([]Rec, len(d.Leads))
			for i, l := range d.Leads {
				leads[i] = orderedRec(t, l)
			}
			res := FindDuplicates(leads, d.Threshold)
			sc := len(leads)
			res.Scanned = &sc
			got, err := json.Marshal(res)
			if err != nil {
				t.Fatal(err)
			}
			if d.HashSeedStable {
				if string(got) != d.ResultJSON {
					t.Fatalf("result differs\n got %s\nwant %s", got, d.ResultJSON)
				}
				return
			}
			// seed-dependent ordering: compare the seed independent projection
			checkProjection(t, got, d.Projection)
		})
	}
	if n != 8 {
		t.Fatalf("expected 8 non-synthetic documents, ran %d", n)
	}
}

func checkProjection(t *testing.T, gotJSON []byte, proj map[string]json.RawMessage) {
	var got struct {
		Clusters map[string][]any
		Pairs    []struct {
			A     any             `json:"lead_a_id"`
			B     any             `json:"lead_b_id"`
			Score float64         `json:"score"`
			FS    json.RawMessage `json:"field_scores"`
		}
		Stats map[string]any
		Sug   []map[string]any `json:"merge_suggestions"`
	}
	if err := json.Unmarshal(gotJSON, &got); err != nil {
		t.Fatal(err)
	}
	var wantMembers [][]float64
	_ = json.Unmarshal(proj["cluster_member_sets"], &wantMembers)
	var gotMembers [][]float64
	for _, ids := range got.Clusters {
		m := []float64{}
		for _, id := range ids {
			m = append(m, id.(float64))
		}
		sort.Float64s(m)
		gotMembers = append(gotMembers, m)
	}
	sort.Slice(gotMembers, func(i, j int) bool { return gotMembers[i][0] < gotMembers[j][0] })
	if !reflect.DeepEqual(gotMembers, wantMembers) {
		t.Errorf("cluster member sets differ (got %d, want %d)", len(gotMembers), len(wantMembers))
	}
	var wantStats map[string]float64
	_ = json.Unmarshal(proj["stats"], &wantStats)
	for k, w := range wantStats {
		if got.Stats[k].(float64) != w {
			t.Errorf("stats[%s] = %v, want %v", k, got.Stats[k], w)
		}
	}
	if raw, ok := proj["pairs_digest_sha256"]; ok {
		var lines []string
		for _, p := range got.Pairs {
			s, _ := canonjson.FloatRepr(p.Score)
			fs := compactJSON(p.FS)
			lines = append(lines, fmt.Sprintf("%d:%d:%s:%s", int64(p.A.(float64)), int64(p.B.(float64)), s, fs))
		}
		sort.Strings(lines)
		sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
		var want string
		_ = json.Unmarshal(raw, &want)
		if hex.EncodeToString(sum[:]) != want {
			t.Errorf("pairs digest differs")
		}
	}
}

func compactJSON(raw json.RawMessage) string { return string(raw) }

func synthetic(v *vectors) []Rec {
	var sp = v.Find[len(v.Find)-1].Spec
	leads := make([]Rec, 0, 5000)
	for i := 1; i <= 5000; i++ {
		r := Rec{Keys: []string{"id", "company", "city", "website", "phone", "email"}, Vals: map[string]any{
			"id":      int64(i),
			"company": fmt.Sprintf("%s %d%s", sp.Names[i%40], i/40%13, sp.Suffixes[i%4]),
			"city":    sp.Cities[i%25],
			"website": "", "phone": "", "email": "",
		}}
		if i%3 != 0 {
			r.Vals["website"] = fmt.Sprintf("https://site%d.example.com", i%1800)
		}
		if i%5 != 0 {
			r.Vals["phone"] = fmt.Sprintf("+1 555 %04d %d", i%9000, i%10)
		}
		if i%4 != 0 {
			r.Vals["email"] = fmt.Sprintf("u%d@site%d.example.com", i, i%1800)
		}
		leads = append(leads, r)
	}
	return leads
}

func TestFindDuplicates5000Bounded(t *testing.T) {
	v := load(t)
	d := v.Find[len(v.Find)-1]
	if d.Name != "synthetic_5000" {
		t.Fatal("synthetic doc not last")
	}
	leads := synthetic(v)
	start := time.Now()
	res := FindDuplicates(leads, d.Threshold)
	el := time.Since(start)
	if el > 2*time.Second {
		t.Errorf("5000 leads took %v, want < 2s", el)
	}
	sc := 5000
	res.Scanned = &sc
	got, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	checkProjection(t, got, d.Projection)
	// suggestions (master, duplicate set, confidence repr) are seed independent too
	var gj struct {
		Sug []struct {
			Master int64   `json:"master_id"`
			Dups   []int64 `json:"duplicate_ids"`
			Conf   float64 `json:"confidence"`
		} `json:"merge_suggestions"`
	}
	_ = json.Unmarshal(got, &gj)
	var want [][]any
	_ = json.Unmarshal(d.Projection["suggestions"], &want)
	var gotS [][]any
	for _, s := range gj.Sug {
		sort.Slice(s.Dups, func(i, j int) bool { return s.Dups[i] < s.Dups[j] })
		ds := []any{}
		for _, x := range s.Dups {
			ds = append(ds, float64(x))
		}
		r, _ := canonjson.FloatRepr(s.Conf)
		gotS = append(gotS, []any{float64(s.Master), ds, r})
	}
	sort.Slice(gotS, func(i, j int) bool { return fmt.Sprint(gotS[i]) < fmt.Sprint(gotS[j]) })
	sort.Slice(want, func(i, j int) bool { return fmt.Sprint(want[i]) < fmt.Sprint(want[j]) })
	if !reflect.DeepEqual(gotS, want) {
		t.Errorf("merge suggestions differ (got %d, want %d)", len(gotS), len(want))
	}
}

func TestBlockOver100Skipped(t *testing.T) {
	v := load(t)
	if v.BlockCap != 100 {
		t.Fatalf("block cap %d", v.BlockCap)
	}
	var leads []Rec
	for i := 1; i <= 101; i++ { // one block of 101 sharing "name:big" and nothing else
		leads = append(leads, Rec{Keys: []string{"id", "company"}, Vals: map[string]any{"id": int64(i), "company": "Bigco Name"}})
	}
	if r := FindDuplicates(leads, 0.85); r.Stats.PairsCompared != 0 || len(r.Clusters) != 0 {
		t.Errorf("block of 101 compared: %+v", r.Stats)
	}
	if r := FindDuplicates(leads[:100], 0.85); r.Stats.PairsCompared != 4950 {
		t.Errorf("block of 100 compared %d pairs, want 4950", r.Stats.PairsCompared)
	}
}

func TestClusterAndPairOrder(t *testing.T) {
	// two clusters; pairs sorted score desc, ties keep discovery order; cluster keys are root indexes in first-member order
	mk := func(id int64, c, site string) Rec {
		return Rec{Keys: []string{"id", "company", "website"}, Vals: map[string]any{"id": id, "company": c, "website": site}}
	}
	leads := []Rec{
		mk(10, "Alpha One", "a.example.com"), mk(20, "Beta Two", "b.example.com"),
		mk(30, "Alpha One", "a.example.com"), mk(40, "Beta Two", "b.example.com"),
	}
	got, _ := json.Marshal(FindDuplicates(leads, 0.85))
	want := `{"clusters":{"0":[10,30],"1":[20,40]},"pairs":[{"lead_a_id":10,"lead_b_id":30,"score":0.9,"field_scores":{"company":1.0,"domain":1.0,"phone":0.0,"email":0.0,"city":0.0}},{"lead_a_id":20,"lead_b_id":40,"score":0.9,"field_scores":{"company":1.0,"domain":1.0,"phone":0.0,"email":0.0,"city":0.0}}],"stats":{"total_leads":4,"pairs_compared":2,"duplicates_found":2,"clusters":2,"unique_leads":2},"merge_suggestions":[{"master_id":10,"master_company":"Alpha One","duplicate_ids":[30],"duplicate_companies":["Alpha One"],"confidence":0.9},{"master_id":20,"master_company":"Beta Two","duplicate_ids":[40],"duplicate_companies":["Beta Two"],"confidence":0.9}]}`
	if string(got) != want {
		t.Errorf("\n got %s\nwant %s", got, want)
	}
}

func TestMergeLeadsMatchesPython(t *testing.T) {
	v := load(t)
	for _, c := range v.Merge {
		master := orderedRec(t, c.Master)
		var dups []Rec
		for _, d := range c.Duplicates {
			dups = append(dups, orderedRec(t, d))
		}
		if c.PythonError != "" {
			if _, err := MergeLeadsStrict(master, dups); err != ErrNonNumericScore {
				t.Errorf("%s: strict err = %v", c.Name, err)
			}
			MergeLeads(master, dups) // lenient variant must not panic
			continue
		}
		got, err := json.Marshal(MergeLeads(master, dups))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != c.MergedJSON {
			t.Errorf("%s:\n got %s\nwant %s", c.Name, got, c.MergedJSON)
		}
		strict, err := MergeLeadsStrict(master, dups)
		if err != nil {
			t.Errorf("%s: strict: %v", c.Name, err)
		}
		if b, _ := json.Marshal(strict); string(b) != c.MergedJSON {
			t.Errorf("%s: strict differs", c.Name)
		}
	}
}

func TestMergeDoesNotMutateInputs(t *testing.T) {
	m := Rec{Keys: []string{"id", "email"}, Vals: map[string]any{"id": int64(1), "email": ""}}
	d := Rec{Keys: []string{"id", "email"}, Vals: map[string]any{"id": int64(2), "email": "a@b.example.com"}}
	_ = MergeLeads(m, []Rec{d})
	if m.Vals["email"] != "" || len(m.Keys) != 2 {
		t.Errorf("master mutated: %+v", m)
	}
}
