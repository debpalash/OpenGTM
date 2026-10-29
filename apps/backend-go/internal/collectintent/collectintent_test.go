package collectintent

import (
	"bytes"
	"encoding/json"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testutil"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

type vectors struct {
	Cases []struct {
		Query           string          `json:"query"`
		RequestedIntent *string         `json:"requested_intent"`
		Decision        json.RawMessage `json:"decision"`
	} `json:"cases"`
	NormalizeDomain []struct {
		In  string `json:"in"`
		Out string `json:"out"`
	} `json:"normalize_domain"`
	DomainInQuery []struct {
		In  string `json:"in"`
		Out string `json:"out"`
	} `json:"domain_in_query"`
	CompanyTeam []struct {
		In  string `json:"in"`
		Out *struct {
			Entity   string `json:"entity"`
			Function string `json:"function"`
		} `json:"out"`
	} `json:"extract_company_team_request"`
	ExplicitPeople []struct {
		In  string `json:"in"`
		Out *struct {
			Entity   string `json:"entity"`
			Function string `json:"function"`
		} `json:"out"`
	} `json:"extract_explicit_people_request"`
	Infer []struct {
		In  string `json:"in"`
		Out string `json:"out"`
	} `json:"infer_collection_intent"`
}

func load(t *testing.T) *vectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/decide_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return &v
}

func decodeNum(t *testing.T, raw []byte) any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

var decisionKeys = `{"query":`

func TestDecideVectors(t *testing.T) {
	v := load(t)
	if len(v.Cases) < 300 {
		t.Fatalf("only %d cases", len(v.Cases))
	}
	for i, c := range v.Cases {
		d := Decide(c.Query, c.RequestedIntent)
		got, err := json.Marshal(d.ToDict())
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(decodeNum(t, got), decodeNum(t, c.Decision)) {
			t.Errorf("case %d %q intent=%v:\n got %s\nwant %s", i, c.Query, c.RequestedIntent, got, c.Decision)
		}
		if !strings.HasPrefix(string(got), decisionKeys) {
			t.Fatalf("bad key order: %s", got)
		}
		wantCan := d.Intent == "market_search" && !d.ClarificationRequired
		if d.CanCollect() != wantCan {
			t.Errorf("case %d CanCollect", i)
		}
	}
}

func TestDecisionKeyOrder(t *testing.T) {
	q := "stripe.com"
	d := Decide(q, nil)
	got, _ := json.Marshal(d.ToDict())
	idx := -1
	for _, k := range []string{"query", "intent", "domain", "entity", "clarification_kind", "clarification_required", "reason", "options"} {
		j := strings.Index(string(got), `"`+k+`":`)
		if j <= idx {
			t.Fatalf("key %s out of order in %s", k, got)
		}
		idx = j
	}
	opt := `"key":`
	i := strings.Index(string(got), opt)
	order := []string{"key", "intent", "label", "description", "route", "draft"}
	last := i - 1
	for _, k := range order {
		j := strings.Index(string(got)[i:], `"`+k+`":`) + i
		if j <= last {
			t.Fatalf("option key %s out of order", k)
		}
		last = j
	}
}

func TestInferIntentVectors(t *testing.T) {
	for _, c := range load(t).Infer {
		if got := Infer(c.In); got != c.Out {
			t.Errorf("Infer(%q)=%q want %q", c.In, got, c.Out)
		}
	}
}

func TestNormalizeDomainVectors(t *testing.T) {
	for _, c := range load(t).NormalizeDomain {
		if got := NormalizeDomain(c.In); got != c.Out {
			t.Errorf("NormalizeDomain(%q)=%q want %q", c.In, got, c.Out)
		}
	}
	for _, c := range load(t).DomainInQuery {
		if got := DomainInQuery(c.In); got != c.Out {
			t.Errorf("DomainInQuery(%q)=%q want %q", c.In, got, c.Out)
		}
	}
}

func TestCompanyTeamRequestVectors(t *testing.T) {
	for _, c := range load(t).CompanyTeam {
		got := ExtractCompanyTeamRequest(c.In)
		if (got == nil) != (c.Out == nil) || (got != nil && (got.Entity != c.Out.Entity || got.Function != c.Out.Function)) {
			t.Errorf("%q: got %+v want %+v", c.In, got, c.Out)
		}
	}
}

func TestExplicitPeopleRequestVectors(t *testing.T) {
	for _, c := range load(t).ExplicitPeople {
		got := ExtractExplicitPeopleRequest(c.In)
		if (got == nil) != (c.Out == nil) || (got != nil && (got.Entity != c.Out.Entity || got.Function != c.Out.Function)) {
			t.Errorf("%q: got %+v want %+v", c.In, got, c.Out)
		}
	}
}

// Expectations below were produced by running the Python implementation.
func TestUnicodeLowerAndWhitespaceCollapse(t *testing.T) {
	// U+0130 lowercases to "i" + U+0307 in Python, so "MONİTOR " does not
	// contain "monitor ".
	if got := Infer("MON\u0130TOR x"); got != "market_search" {
		t.Errorf("dotted capital I: %q", got)
	}
	if got := Infer("MONITOR x"); got != "signal_monitor" {
		t.Errorf("plain: %q", got)
	}
	// Unicode whitespace (NBSP, U+2003, U+0085, FS) collapses to one space.
	d := Decide("\u00a0 IT\u2003staffing\u0085\u001ccompanies  in\tPune ", nil)
	if d.Query != "IT staffing companies in Pune" {
		t.Errorf("collapse: %q", d.Query)
	}
	// Python IGNORECASE matches dotless/dotted i against i.
	if r := ExtractCompanyTeamRequest("Stripe partnersh\u0131p teams"); r == nil || r.Function != "partnersh\u0131p" {
		t.Errorf("dotless i: %+v", r)
	}
	// Final sigma in the lowercased function.
	if r := ExtractExplicitPeopleRequest("find people currently working on X's \u0391\u03a3 team"); r == nil || r.Function != "\u03b1\u03c2" {
		t.Errorf("final sigma: %+v", r)
	}
	// Unicode word boundary: a letter before "use" is not a boundary.
	if got := Infer("companies \u00e9use"); got != "market_search" {
		t.Errorf("unicode boundary: %q", got)
	}
	if got := Infer("companies use x"); got != "technology_users" {
		t.Errorf("use: %q", got)
	}
	// Lookbehind: domain preceded by a word char or @ is ignored.
	if got := DomainInQuery("mail bob@acme.io now"); got != "" {
		t.Errorf("lookbehind @: %q", got)
	}
	if got := DomainInQuery("\u00e9acme.io"); got != "" {
		t.Errorf("lookbehind unicode word: %q", got)
	}
}

// The only non-RE2 construct in the Python module is the leading negative
// lookbehind of _DOMAIN_IN_TEXT_RE; Go emulates it by hand. Every other
// pattern must be RE2-compatible, and the Go patterns must compile.
func TestPythonRegexesAreRE2Compatible(t *testing.T) {
	src, err := os.ReadFile(testutil.LegacyPath(t, "apps", "api", "services", "leadgen", "collection_intent.py"))
	if err != nil {
		t.Skip("python source not available")
	}
	text := string(src)
	if n := strings.Count(text, "(?<"); n != 1 {
		t.Errorf("expected exactly 1 lookbehind, got %d", n)
	}
	for _, bad := range []string{"(?=", "(?!", "(?<=", "(?P=", "(?(", `\1`, `\2`} {
		if strings.Contains(text, bad) {
			t.Errorf("python source uses non-RE2 construct %q", bad)
		}
	}
	if !strings.Contains(text, `(?<![@\w.-])`) {
		t.Errorf("the single lookbehind is no longer (?<![@\\w.-])")
	}
	for _, re := range []*regexp.Regexp{companyTeamRE, explicitPeopleRE} {
		if re == nil {
			t.Fatal("nil regex")
		}
	}
}

func TestDecideBoundedOnLongInput(t *testing.T) {
	inputs := []string{
		strings.Repeat("word ", 210000),
		strings.Repeat("a.", 524288),
		strings.Repeat("find ", 200000),
		strings.Repeat("companies ", 100000),
	}
	for i, in := range inputs {
		start := time.Now()
		Decide(in, nil)
		limit := 500 * time.Millisecond // generous: a quadratic regression takes seconds; shared CI runners jitter
		if raceEnabled {
			limit *= 10 // the race detector slows pure-CPU loops ~5-10x
		}
		if d := time.Since(start); d > limit {
			t.Errorf("input %d took %v", i, d)
		}
	}
}
