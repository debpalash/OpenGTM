package firmo

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// Vectors are computed by the real Python modules (legacy/python/scripts/
// gen_firmo_vectors.py); only the inputs are hand-written.

func readVectors(t *testing.T, name string, into any) {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

type outcome struct {
	Value   json.RawMessage `json:"value"`
	Error   string          `json:"error"`
	Message string          `json:"message"`
}

// canon re-encodes JSON with sorted keys and compact separators.
func canon(t *testing.T, raw []byte) string {
	t.Helper()
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("canon %q: %v", raw, err)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// objectKeys lists the top-level keys of a JSON object in document order.
func objectKeys(t *testing.T, raw []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("not an object: %s", raw)
	}
	var keys []string
	for dec.More() {
		k, _ := dec.Token()
		keys = append(keys, k.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

func fieldsOf(o ofields) *fields {
	f := newFields()
	for _, e := range o {
		f.Set(e.k, e.v)
	}
	return f
}

type extractCase struct {
	Name string `json:"name"`
	HTML string `json:"html"`
	outcome
}

// TestJSONLDExtractionVectors compares extract_firmographics on 100+ pages,
// including malformed markup, entity handling and unusual JSON-LD shapes.
func TestJSONLDExtractionVectors(t *testing.T) {
	var v struct {
		Extract []extractCase `json:"extract"`
		Fuzz    []extractCase `json:"fuzz"`
	}
	readVectors(t, "jsonld_vectors.json", &v)
	if len(v.Extract) < 100 || len(v.Fuzz) < 400 {
		t.Fatalf("only %d+%d vectors", len(v.Extract), len(v.Fuzz))
	}
	for _, c := range append(v.Extract, v.Fuzz...) {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			if c.Error != "" {
				t.Fatalf("python raised %s: %s", c.Error, c.Message)
			}
			got, err := fieldsOf(extractFirmographics(c.HTML)).MarshalJSON()
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != compact(t, c.Value) {
				t.Errorf("fields\n got %s\nwant %s", got, compact(t, c.Value))
			}
			if !reflect.DeepEqual(objectKeys(t, got), objectKeys(t, c.Value)) {
				t.Errorf("key order %v want %v", objectKeys(t, got), objectKeys(t, c.Value))
			}
		})
	}
}

// normalize_website_url and the sameAs hostname extraction (urlsplit) share the
// Python URL splitting rules, including its ValueErrors.
func TestJSONLDNormalizeAndHostnames(t *testing.T) {
	var v struct {
		Normalize []struct {
			URL string `json:"url"`
			outcome
		} `json:"normalize"`
		Hostnames []struct {
			URL string `json:"url"`
			outcome
		} `json:"hostnames"`
	}
	readVectors(t, "jsonld_vectors.json", &v)
	for _, c := range v.Normalize {
		var want string
		_ = json.Unmarshal(c.Value, &want)
		if got := normalizeWebsiteURL(c.URL); got != want {
			t.Errorf("normalize(%q) = %q want %q", c.URL, got, want)
		}
	}
	for _, c := range v.Hostnames {
		got, wantErr := "", c.Error != ""
		pu, err := pySplitURL(strings.TrimSpace(c.URL))
		if err == nil {
			got = strings.TrimRight(pu.hostname(), ".")
		}
		if wantErr {
			if err == nil || err.Error() != c.Message {
				t.Errorf("hostname(%q): err %v want %q", c.URL, err, c.Message)
			}
			continue
		}
		var want string
		_ = json.Unmarshal(c.Value, &want)
		if err != nil || got != want {
			t.Errorf("hostname(%q) = %q (%v) want %q", c.URL, got, err, want)
		}
	}
}

// TestTechStackVectors compares the fingerprint compilation (names and order
// after merging the curated list over the bundled DB) and detect_tech on 1100+
// responses generated from the real fingerprint set.
func TestTechStackVectors(t *testing.T) {
	var v struct {
		Detect []struct {
			Name    string            `json:"name"`
			Headers map[string]string `json:"headers"`
			HTML    *string           `json:"html"`
			Cookies []string          `json:"cookies"`
			outcome
		} `json:"detect"`
		Fields struct {
			In []struct {
				Name       string   `json:"name"`
				Category   *string  `json:"category"`
				Confidence *float64 `json:"confidence"`
			} `json:"in"`
			Out map[string]string `json:"out"`
		} `json:"fields"`
		CompiledCount int      `json:"compiled_count"`
		CompiledNames []string `json:"compiled_names"`
	}
	readVectors(t, "techstack_vectors.json", &v)
	var names []string
	for _, fp := range techFingerprints() {
		names = append(names, fp.name)
	}
	if !reflect.DeepEqual(names, v.CompiledNames) {
		t.Fatalf("compiled %d fingerprints, python %d; first diff at %d", len(names), v.CompiledCount, firstDiff(names, v.CompiledNames))
	}
	if len(v.Detect) < 1000 {
		t.Fatalf("only %d detect vectors", len(v.Detect))
	}
	for _, c := range v.Detect {
		html := ""
		if c.HTML != nil {
			html = *c.HTML
		}
		var out []jo
		for _, d := range detectTech(c.Headers, html, c.Cookies) {
			out = append(out, jo{{"name", d.name}, {"category", d.category}, {"confidence", d.confidence}})
		}
		got := dumpsCompact(out)
		if out == nil {
			got = "[]"
		}
		if canonStr(t, got) != canon(t, c.Value) {
			t.Errorf("%s:\n got %s\nwant %s", c.Name, got, c.Value)
		}
		// order matters (found order, then implied order): compare names in sequence
		var wantList []struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(c.Value, &wantList)
		var gotNames, wantNames []string
		for _, d := range out {
			gotNames = append(gotNames, d[0].v.(string))
		}
		for _, w := range wantList {
			wantNames = append(wantNames, w.Name)
		}
		if !reflect.DeepEqual(gotNames, wantNames) {
			t.Errorf("%s: order\n got %v\nwant %v", c.Name, gotNames, wantNames)
		}
	}
	// _structured_to_fields
	var in []detected
	for _, s := range v.Fields.In {
		d := detected{name: s.Name, category: "Other", confidence: 0.75}
		if s.Category != nil {
			d.category = *s.Category
		}
		if s.Confidence != nil {
			d.confidence = *s.Confidence
		}
		in = append(in, d)
	}
	f := structuredFields(in)
	for k, want := range v.Fields.Out {
		got, _ := f.Get(k)
		if got != want {
			t.Errorf("fields[%s]\n got %v\nwant %s", k, got, want)
		}
	}
}

func canonStr(t *testing.T, s string) string { return canon(t, []byte(s)) }

func firstDiff(a, b []string) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

// TestRobotsVectors compares RobotFileParser.parse + can_fetch for the site
// root across 40 robots.txt bodies, including Python's parsing quirks.
func TestRobotsVectors(t *testing.T) {
	var v []struct {
		Body string `json:"body"`
		outcome
	}
	readVectors(t, "robots_vectors.json", &v)
	for _, c := range v {
		var want bool
		_ = json.Unmarshal(c.Value, &want)
		if got := parseRobots(c.Body).canFetchRoot(techUA); got != want {
			t.Errorf("robots %q: can_fetch %v want %v", c.Body, got, want)
		}
	}
}

// TestJobIntentVectors compares analyze_job_text, _titles_to_signals and
// _slug_candidates (the helpers behind ats_hiring).
func TestJobIntentVectors(t *testing.T) {
	var v struct {
		Analyze []struct {
			Text      string `json:"text"`
			OpenRoles *int   `json:"open_roles"`
			outcome
		} `json:"analyze"`
		Signals []struct {
			Titles []string `json:"titles"`
			outcome
		} `json:"signals"`
		Slugs []struct {
			Lead map[string]any `json:"lead"`
			outcome
		} `json:"slugs"`
	}
	readVectors(t, "jobintent_vectors.json", &v)
	n := 0
	for _, c := range v.Analyze {
		if c.OpenRoles == nil {
			continue
		}
		n++
		tech, sig, vel := analyzeJobText(c.Text, *c.OpenRoles)
		got := dumpsCompact(jo{{"technologies", tech}, {"tech_adoption_signal", sig}, {"hiring_velocity", vel}})
		if canonStr(t, got) != canon(t, c.Value) {
			t.Errorf("analyze(%q, %d):\n got %s\nwant %s", c.Text, *c.OpenRoles, got, c.Value)
		}
	}
	if n < 70 {
		t.Fatalf("only %d analyze vectors", n)
	}
	for _, c := range v.Signals {
		got := dumpsCompact(titlesToSignals(c.Titles))
		if canonStr(t, got) != canon(t, c.Value) {
			t.Errorf("signals(%q):\n got %s\nwant %s", c.Titles, got, c.Value)
		}
	}
	for _, c := range v.Slugs {
		got := slugCandidates(inputOf(c.Lead))
		want := []string{}
		_ = json.Unmarshal(c.Value, &want)
		if len(got) == 0 {
			got = []string{}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("slugs(%v) = %v want %v", c.Lead, got, want)
		}
	}
}
