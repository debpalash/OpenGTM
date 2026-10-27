package scrape

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/browserclient"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadpeople"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

type renderFake struct {
	reqs []browserclient.RenderRequest
	fn   func(r browserclient.RenderRequest) (browserclient.RenderResult, error)
}

func (f *renderFake) Render(_ context.Context, r browserclient.RenderRequest) (browserclient.RenderResult, error) {
	f.reqs = append(f.reqs, r)
	return f.fn(r)
}
func (f *renderFake) Holehe(context.Context, browserclient.HoleheRequest) (browserclient.HoleheResult, error) {
	return browserclient.HoleheResult{}, &browserclient.Error{Code: "busy"}
}
func (f *renderFake) StaffSpy(context.Context, browserclient.StaffSpyRequest) (browserclient.StaffSpyResult, error) {
	return browserclient.StaffSpyResult{}, nil
}

func deep(t *testing.T, b Browser, input string) map[string]any {
	t.Helper()
	Configure(Deps{Browser: b, Sleep: noSleep})
	t.Cleanup(func() { Configure(Deps{}) })
	o, err := RPCs()["person_intel_deep"](context.Background(), nil, json.RawMessage(input))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := o.MarshalJSON()
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return m
}

func TestPersonIntelDeepFastPath(t *testing.T) {
	f := &renderFake{fn: func(r browserclient.RenderRequest) (browserclient.RenderResult, error) {
		return browserclient.RenderResult{Status: 200, HTML: "<p>hi</p>", Text: "Hello a@b.io and a@b.io, c@d.org " + strings.Repeat("x ", 900)}, nil
	}}
	m := deep(t, f, `{"url":"https://example.com/p"}`)
	if m["success"] != true || len(f.reqs) != 1 || f.reqs[0].Engine != "impersonate" {
		t.Fatalf("%v %+v", m, f.reqs)
	}
	if got := m["extracted_emails"].([]any); len(got) != 2 || got[0] != "a@b.io" {
		t.Errorf("emails %v", got)
	}
	if n := runeLen(m["preview_text"].(string)); n != piMaxText {
		t.Errorf("preview %d", n)
	}
	if m["word_count"].(float64) != 905 || m["error"] != nil {
		t.Errorf("%v", m)
	}
}

func TestPersonIntelDeepJSWallFallsBackToBrowser(t *testing.T) {
	f := &renderFake{fn: func(r browserclient.RenderRequest) (browserclient.RenderResult, error) {
		if r.Engine == "impersonate" {
			return browserclient.RenderResult{Status: 200, HTML: "Please enable JavaScript"}, nil
		}
		return browserclient.RenderResult{Status: 200, Text: "rendered"}, nil
	}}
	m := deep(t, f, `{"url":"https://example.com"}`)
	if len(f.reqs) != 2 || f.reqs[1].Engine != "browser" || !f.reqs[1].Scroll || f.reqs[1].MaxScrolls != 5 || m["preview_text"] != "rendered" {
		t.Fatalf("%v %+v", m, f.reqs)
	}
}

func TestPersonIntelDeepFailures(t *testing.T) {
	blocked := &renderFake{fn: func(browserclient.RenderRequest) (browserclient.RenderResult, error) {
		return browserclient.RenderResult{}, &browserclient.Error{Code: "egress_blocked"}
	}}
	if m := deep(t, blocked, `{"url":"https://example.com"}`); m["error"] != "url blocked" || len(blocked.reqs) != 1 {
		t.Errorf("blocked: %v %d", m, len(blocked.reqs))
	}
	for in, want := range map[string]string{`{"url":"ftp://x"}`: "url must be an http(s) URL", `{}`: "url must be an http(s) URL",
		`{"url":"https://` + strings.Repeat("a", 2050) + `"}`: "url must be an http(s) URL"} {
		if m := deep(t, blocked, in); m["error"] != want || m["success"] != false {
			t.Errorf("%s: %v", in[:min(30, len(in))], m)
		}
	}
	down := &renderFake{fn: func(browserclient.RenderRequest) (browserclient.RenderResult, error) {
		return browserclient.RenderResult{}, &browserclient.Error{Code: "navigation_failed"}
	}}
	if m := deep(t, down, `{"url":"https://example.com"}`); m["error"] != "scrape failed: navigation_failed" {
		t.Errorf("down: %v", m)
	}
}

func TestScrapeProvidersAreGated(t *testing.T) {
	for _, n := range []string{"website_scraper", "deep_scraper", "company_intel", "local_business", "crosslinked",
		"email_harvester", "facebook_pages", "social_finder", "ddg_company", "ddg_email", "decision_maker", "jobspy",
		"holehe", "staffspy", "mailscout", "company_size_heuristic", "lead_scorer"} {
		p, ok := providers.Get(n)
		if !ok {
			t.Fatalf("%s not registered", n)
		}
		providers.SetLocalGate(nil)
		if providers.InProcess(p) {
			t.Errorf("%s runs in process with the gate empty", n)
		}
		providers.SetLocalGate(func(name string) bool { return name == n })
		if !providers.InProcess(p) {
			t.Errorf("%s not in process when listed", n)
		}
	}
	providers.SetLocalGate(nil)
	if p, _ := providers.Get("hunter_io"); p != nil && !providers.InProcess(p) {
		t.Error("ungated provider refused")
	}
}

// WP-PEOPLE reaches crosslinked through the registry.
func TestRegisteredCrossLinkedIsAPeopleFinder(t *testing.T) {
	p, ok := providers.Get("crosslinked")
	if !ok {
		t.Fatal("crosslinked not registered")
	}
	cl, ok := p.(leadpeople.CrossLinked)
	if !ok {
		t.Fatal("registered crosslinked does not implement leadpeople.CrossLinked")
	}
	w := &fakeWorld{sc: scenario{Search: map[string]json.RawMessage{
		`site:linkedin.com/in "Acme" "CTO" "Pune"`: json.RawMessage(`[{"title":"Raj Kumar - CTO - Acme | LinkedIn","href":"https://www.linkedin.com/in/raj","body":""},{"title":"Raj Kumar - CTO","href":"https://www.linkedin.com/in/raj","body":""}]`),
		`site:linkedin.com/in "Acme" "CEO" "Pune"`: json.RawMessage(`"raise"`),
	}}}
	var slept []time.Duration
	Configure(Deps{Search: w, Sleep: func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }})
	t.Cleanup(func() { Configure(Deps{}) })
	res, err := cl.FindPeopleByTitles(context.Background(), leadpeople.PeopleQuery{Company: "Acme",
		Titles: []string{"CTO", "CEO", "VP"}, Geo: "Pune", MaxPeople: 5, MaxSearches: 2, ResultsPerSearch: 12, Delay: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if res.SearchesUsed != 2 || res.SearchFailures != 1 || len(res.People) != 1 || res.People[0]["linkedin"] != "https://www.linkedin.com/in/raj" ||
		res.People[0]["evidence_title"] != "Raj Kumar - CTO - Acme | LinkedIn" {
		t.Fatalf("%+v", res)
	}
	// 200 ms between searches, 0.5 s and 1 s between the failed search's retries.
	if len(slept) != 3 || slept[0] != 200*time.Millisecond || slept[1] != 500*time.Millisecond || slept[2] != time.Second {
		t.Errorf("sleeps %v", slept)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	Configure(Deps{Search: w})
	if _, err := cl.FindPeopleByTitles(ctx, leadpeople.PeopleQuery{Company: "Acme", Titles: []string{"CEO", "CTO"}, Geo: "Pune"}); err == nil {
		t.Error("expired budget: no error")
	}
}

type redirector struct{ hops []string }

func (r *redirector) RoundTrip(req *http.Request) (*http.Response, error) {
	r.hops = append(r.hops, req.URL.String())
	h := http.Header{}
	status := 200
	body := "<html><head><meta name=description content=ok></head></html>"
	switch req.URL.Path {
	case "/":
		status, body = 302, ""
		h.Set("Location", "http://169.254.169.254/latest")
	case "/about":
		status, body = 301, ""
		h.Set("Location", "/about-us")
	}
	h.Set("Content-Type", "text/html")
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
}

// guarded_get: every redirect hop is re-checked; a hop to a metadata address
// is never requested.
func TestDeepScraperRedirectGuard(t *testing.T) {
	rt := &redirector{}
	out, err := deepScraper{MaxPages: 1}.Enrich(context.Background(), &http.Client{Transport: rt}, nil,
		providers.Input{Lead: map[string]any{"website": "https://ex.com"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range rt.hops {
		if strings.Contains(h, "169.254") {
			t.Fatalf("requested %s", h)
		}
	}
	if len(rt.hops) < 3 || rt.hops[1] != "https://ex.com/about" || rt.hops[2] != "https://ex.com/about-us" {
		t.Fatalf("hops %v", rt.hops)
	}
	if out.Error != "No data extracted from 1 pages" { // only "/" feeds the description
		t.Fatalf("%+v", out)
	}
}

func TestStaffSpyOffWithoutOptIn(t *testing.T) {
	t.Cleanup(func() { Configure(Deps{}) })
	in := providers.Input{Lead: map[string]any{"company": "Acme"}}
	Configure(Deps{Browser: &renderFake{}})
	out, _ := staffSpy{}.Enrich(context.Background(), nil, nil, in)
	if out.Error != staffspyUnavailable {
		t.Errorf("%q", out.Error)
	}
	Configure(Deps{Browser: &renderFake{}, StaffSpyEnabled: true,
		LinkedInCookies: func(context.Context, string) ([]browserclient.Cookie, error) { return nil, nil }})
	if out, _ := (staffSpy{}).Enrich(context.Background(), nil, nil, in); out.Error != staffspyUnavailable {
		t.Errorf("no li_at: %q", out.Error)
	}
	Configure(Deps{Browser: &renderFake{}})
	if out, _ := (holehe{}).Enrich(context.Background(), nil, nil, providers.Input{Lead: map[string]any{"email": "a@b.co"}}); out.Error != "holehe unavailable: busy" {
		t.Errorf("holehe: %q", out.Error)
	}
}

func TestNoSeamIsAnErrorOutput(t *testing.T) {
	Configure(Deps{})
	for _, n := range []string{"social_finder", "crosslinked", "website_scraper", "jobspy"} {
		p, _ := providers.Get(n)
		out, err := p.Enrich(context.Background(), nil, nil, providers.Input{Lead: map[string]any{"company": "Acme", "website": "acme.in"}})
		if err != nil || out.Error != errNoWeb {
			t.Errorf("%s: %v %q", n, err, out.Error)
		}
	}
}
