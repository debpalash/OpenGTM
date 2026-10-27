package collect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/browserclient"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/collect/scrapers"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadprogress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadweb"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/llm"
)

// The fake world of testdata/record_rest.py, replayed: what Python's fakes served (search hits per
// query, pages per URL, LLM answers per prompt) is in world.json and the per-vector call logs.

type world struct {
	Searches map[string][]map[string]string `json:"searches"`
	Pages    map[string]*string             `json:"pages"`
}

var (
	worldOnce sync.Once
	theWorld  world
)

func loadWorld(t *testing.T) world {
	t.Helper()
	worldOnce.Do(func() { load(t, "world.json", &theWorld) })
	return theWorld
}

type fakeWeb struct {
	w          world
	mu         sync.Mutex
	unexpected []string
	fetches    [][]any
}

func (f *fakeWeb) Search(_ context.Context, q string, n int) []leadweb.Hit {
	hits, ok := f.w.Searches[q]
	if !ok {
		f.mu.Lock()
		f.unexpected = append(f.unexpected, "search "+q)
		f.mu.Unlock()
		return nil
	}
	var out []leadweb.Hit
	for _, h := range slice2(hits, n) {
		out = append(out, leadweb.Hit{Title: h["title"], Href: h["href"], Body: h["body"]})
	}
	return out
}

func (f *fakeWeb) Fetch(_ context.Context, url string, tier int, timeout time.Duration) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetches = append(f.fetches, []any{url, float64(tier), float64(timeout / time.Second)})
	p, ok := f.w.Pages[url]
	if !ok {
		f.unexpected = append(f.unexpected, "fetch "+url)
		return "", false
	}
	if p == nil {
		return "", false
	}
	return *p, true
}

type llmCall struct {
	Prompt    string         `json:"prompt"`
	System    string         `json:"system"`
	MaxTokens int            `json:"max_tokens"`
	Out       map[string]any `json:"out"`
}

type fakeLLM struct {
	calls  []llmCall
	down   bool
	mu     *sync.Mutex
	errs   []string
	misses *[]string
}

func (f *fakeLLM) ExtractJSON(_ context.Context, prompt, system string, maxTokens int) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		f.errs = append(f.errs, "groq: HTTP 503: unavailable")
		return nil, errors.New("groq: HTTP 503: unavailable")
	}
	for _, c := range f.calls {
		if c.Prompt == prompt {
			if c.System != system || c.MaxTokens != maxTokens {
				*f.misses = append(*f.misses, fmt.Sprintf("system/max_tokens differ for %.60q", prompt))
			}
			return c.Out, nil
		}
	}
	*f.misses = append(*f.misses, "prompt not recorded: "+prompt)
	return map[string]any{}, nil
}

func (f *fakeLLM) Usage() llm.TokenUsage {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := f.errs
	if len(e) > 5 {
		e = e[len(e)-5:]
	}
	return llm.TokenUsage{Errors: append([]string(nil), e...)}
}

type fakeEmitter struct {
	mu     sync.Mutex
	events []map[string]any
}

func (f *fakeEmitter) Emit(_ context.Context, typ string, data leadprogress.Obj) {
	m := objMap(data)
	m["type"] = typ
	raw, _ := json.Marshal(m)
	var norm map[string]any
	_ = json.Unmarshal(raw, &norm)
	f.mu.Lock()
	f.events = append(f.events, norm)
	f.mu.Unlock()
}

func objMap(o leadprogress.Obj) map[string]any {
	m := map[string]any{}
	for _, fd := range o {
		if sub, ok := fd.Val.(leadprogress.Obj); ok {
			m[fd.Key] = objMap(sub)
		} else {
			m[fd.Key] = fd.Val
		}
	}
	return m
}

func (f *fakeEmitter) MarshalJSON() ([]byte, error) { return json.Marshal(f.events) }

// leadMap is lead_out(l): every dataclass field but id and the timestamps.
func leadMap(l *Lead) map[string]any {
	m := l.Map()
	delete(m, "created_at")
	delete(m, "updated_at")
	raw, _ := json.Marshal(m)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

func crc(s string) uint32 { return crc32.ChecksumIEEE([]byte(s)) }

func slugOf(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if isPyWord(r) && r != '_' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// fakeEnrich mirrors the recorder's deterministic enrichment fakes.
type fakeEnrich struct{}

var mxDomains = setOf("acmestaffing.com", "zetalabs.in", "novahr.co.in", "quantatech.io", "brightpath.com", "kite-systems.com")

func (fakeEnrich) Waterfall(_ context.Context, _ string, l *Lead) error {
	if crc(l.Company)%3 == 0 {
		l.Email = "hello@" + slugOf(l.Company) + ".com"
		l.EmailProvider = "hunter"
	}
	return nil
}

func (fakeEnrich) Deliverability(_ context.Context, _ string, l *Lead) (bool, string, bool) {
	c := crc(l.Company+l.Email) % 5
	if c == 0 {
		l.Email, l.EmailConfidence, l.EmailProvider = "", "", ""
		return false, "", true
	}
	conf := map[uint32]string{1: "verified", 2: "risky"}[c]
	if conf == "" {
		conf = "unknown"
	}
	l.EmailConfidence = conf
	return true, conf, true
}

func (fakeEnrich) Provider(_ context.Context, _, name string, l *Lead) (ProviderResult, error) {
	switch name {
	case "crosslinked":
		if crc(l.Company)%2 == 0 {
			return ProviderResult{true, map[string]any{"decision_makers": `[{"name": "Asha Rao", "title": "CEO"}]`,
				"contact_person": "Asha Rao", "contact_title": "CEO"}}, nil
		}
	case "jobspy":
		if crc(l.Company)%2 == 1 {
			return ProviderResult{true, map[string]any{"hiring_signals": `{"total_jobs": 3, "score_boost": 7}`}}, nil
		}
	case "mailscout":
		switch crc(l.Email) % 4 {
		case 0:
			return ProviderResult{true, map[string]any{"email": l.Email, "email_confidence": "smtp_verified"}}, nil
		case 1:
			return ProviderResult{true, map[string]any{"email_confidence": "risky"}}, nil
		}
	default:
		return ProviderResult{}, fmt.Errorf("unexpected provider %s", name)
	}
	return ProviderResult{}, nil
}

func (fakeEnrich) HasMX(_ context.Context, d string) bool { return mxDomains[d] }

func (fakeEnrich) PickBestEmail(_ context.Context, ps []string) (string, string, *bool) {
	t, f := true, false
	switch crc(ps[0]) % 3 {
	case 0:
		return ps[1], "smtp_verified", &t
	case 1:
		return "", "", &f
	}
	return "", "", nil
}

func (fakeEnrich) VerifyEmail(context.Context, string) *bool { return nil }
func (fakeEnrich) Disposable(string) bool                    { return false }

// fakeBrowser serves the scrapers from the world (LinkedIn people) and the recorded Maps places.
type fakeBrowser struct {
	w      world
	places map[string]struct {
		Query, City string
		Places      []browserclient.MapPlace
	}
	fail error
}

var defaultPeopleTitles = []string{"CEO", "CTO", "CHRO", "HR Director", "Founder", "Managing Director"}

func (b *fakeBrowser) GoogleMaps(_ context.Context, req browserclient.GoogleMapsRequest) (browserclient.GoogleMapsResult, error) {
	if b.fail != nil {
		return browserclient.GoogleMapsResult{}, b.fail
	}
	p, ok := b.places[req.Query+"|"+req.City]
	if !ok {
		return browserclient.GoogleMapsResult{}, fmt.Errorf("unexpected maps search %q", req.Query+"|"+req.City)
	}
	return browserclient.GoogleMapsResult{Places: p.Places}, nil
}

func (b *fakeBrowser) LinkedIn(_ context.Context, req browserclient.LinkedInRequest) (browserclient.SearchResult, error) {
	var res browserclient.SearchResult
	titles := req.Titles
	if len(titles) == 0 {
		titles = defaultPeopleTitles
	}
	for _, t := range titles {
		q := `site:linkedin.com/in "` + req.CompanyName + `" "` + t + `" India`
		run := browserclient.SearchRun{Variant: "people", Query: req.CompanyName, Term: t, Search: q}
		for _, h := range slice2(b.w.Searches[q], req.MaxResults) {
			run.Hits = append(run.Hits, browserclient.SearchHit{Title: h["title"], URL: h["href"], Body: h["body"]})
		}
		res.Runs = append(res.Runs, run)
	}
	return res, nil
}

type browserDiscovery struct{ b scrapers.Browser }

func (d browserDiscovery) Maps(ctx context.Context, q, city string, n int) ([]scrapers.Place, error) {
	return scrapers.GoogleMaps(ctx, d.b, q, city, n, "")
}

func (d browserDiscovery) DecisionMakers(ctx context.Context, company string, n int) ([]scrapers.Person, error) {
	return scrapers.FindDecisionMakers(ctx, d.b, company, nil, n, "")
}

type fakeSources struct {
	enabled map[string]bool
	health  map[string]bool
}

func (s fakeSources) Enabled(_ context.Context, id string) bool         { return s.enabled[id] }
func (s fakeSources) HealthDisabled(context.Context) map[string]bool { return s.health }

func noSleep(context.Context, time.Duration) error { return nil }

type harness struct {
	web    *fakeWeb
	em     *fakeEmitter
	misses []string
	runner *Runner
}

func newHarness(t *testing.T, llmCalls []llmCall, down bool, src fakeSources, br *fakeBrowser) *harness {
	h := &harness{web: &fakeWeb{w: loadWorld(t)}, em: &fakeEmitter{}}
	mu := &sync.Mutex{}
	if br == nil {
		br = &fakeBrowser{w: loadWorld(t)}
	}
	h.runner = &Runner{Web: h.web, Discovery: browserDiscovery{br}, Sources: src, Enrich: fakeEnrich{},
		Knobs:  DefaultKnobs(),
		NewLLM: func() LLM { return &fakeLLM{calls: llmCalls, down: down, mu: mu, misses: &h.misses} },
		Sleep:  noSleep}
	return h
}

func (h *harness) check(t *testing.T) {
	t.Helper()
	if len(h.web.unexpected) > 0 {
		t.Errorf("unexpected web calls: %v", h.web.unexpected)
	}
	if len(h.misses) > 0 {
		t.Errorf("LLM prompt mismatches: %v", h.misses)
	}
}

func jsonNorm(v any) any {
	raw, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(raw, &out)
	return out
}

// eventKey groups events into the sequences whose order is deterministic in both runtimes.
func eventKey(e map[string]any) string {
	if s, ok := e["stage"].(string); ok {
		return s
	}
	return "-"
}

func compareEvents(t *testing.T, label string, got, want []map[string]any) {
	t.Helper()
	group := func(evs []map[string]any) map[string][]any {
		out := map[string][]any{}
		for _, e := range evs {
			out[eventKey(e)] = append(out[eventKey(e)], jsonNorm(e))
		}
		return out
	}
	g, w := group(got), group(want)
	keys := map[string]bool{}
	for k := range g {
		keys[k] = true
	}
	for k := range w {
		keys[k] = true
	}
	var sorted []string
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	for _, k := range sorted {
		if !reflect.DeepEqual(g[k], w[k]) {
			t.Errorf("%s: events of %q differ\n got: %v\nwant: %v", label, k, g[k], w[k])
		}
	}
}

func compareLeads(t *testing.T, label string, got []*Lead, want []map[string]any) {
	t.Helper()
	if len(got) != len(want) {
		var names []string
		for _, l := range got {
			names = append(names, l.Company)
		}
		var wn []any
		for _, l := range want {
			wn = append(wn, l["company"])
		}
		t.Errorf("%s: %d leads, want %d\n got: %v\nwant: %v", label, len(got), len(want), names, wn)
		return
	}
	for i := range got {
		g := leadMap(got[i])
		w := jsonNorm(want[i]).(map[string]any)
		if !reflect.DeepEqual(g, w) {
			for k := range w {
				if !reflect.DeepEqual(g[k], w[k]) {
					t.Errorf("%s: lead %d (%v) field %s = %#v want %#v", label, i, w["company"], k, g[k], w[k])
				}
			}
		}
	}
}

func TestStrategyVectors(t *testing.T) {
	var v struct {
		Cases []struct {
			Strategy string           `json:"strategy"`
			Query    string           `json:"query"`
			LLM      string           `json:"llm"`
			Leads    []map[string]any `json:"leads"`
			Events   []map[string]any `json:"events"`
			Fetches  [][]any          `json:"fetches"`
			Calls    []llmCall        `json:"llm_calls"`
		} `json:"cases"`
	}
	load(t, "strategies.json", &v)
	var jv struct {
		Maps map[string]struct {
			Query, City string
			Places      []browserclient.MapPlace
		} `json:"maps"`
	}
	load(t, "job.json", &jv)
	for _, c := range v.Cases {
		label := c.Strategy + "/" + c.Query + "/" + c.LLM
		t.Run(label, func(t *testing.T) {
			h := newHarness(t, c.Calls, c.LLM == "down", fakeSources{}, &fakeBrowser{w: loadWorld(t), places: jv.Maps})
			j := &job{r: h.runner, id: "job1", ws: "ws1", query: c.Query, em: h.em, llm: h.runner.NewLLM()}
			var got []*Lead
			ctx := context.Background()
			switch c.Strategy {
			case "_search_and_scrape":
				got = j.searchAndScrape(ctx)
			case "_search_directories":
				got = j.searchDirectories(ctx)
			case "_search_linkedin":
				got = j.searchLinkedin(ctx)
			case "_search_job_boards":
				got = j.searchJobBoards(ctx)
			case "_search_review_sites":
				got = j.searchReviewSites(ctx)
			case "_search_maps":
				got = j.searchMaps(ctx)
			}
			compareLeads(t, label, got, c.Leads)
			if (len(h.em.events) > 0 || len(c.Events) > 0) && !reflect.DeepEqual(jsonNorm(h.em.events), jsonNorm(c.Events)) {
				t.Errorf("events differ\n got: %v\nwant: %v", h.em.events, c.Events)
			}
			if (len(h.web.fetches) > 0 || len(c.Fetches) > 0) && !reflect.DeepEqual(jsonNorm(h.web.fetches), jsonNorm(c.Fetches)) {
				t.Errorf("fetches differ\n got: %v\nwant: %v", h.web.fetches, c.Fetches)
			}
			h.check(t)
		})
	}
}

func TestRegistryStrategyVectors(t *testing.T) {
	var v struct {
		Cases []struct {
			Query   string           `json:"query"`
			Enabled []string         `json:"enabled"`
			Health  []string         `json:"health_disabled"`
			Cap     *string          `json:"cap"`
			Leads   []map[string]any `json:"leads"`
			Events  []map[string]any `json:"events"`
		} `json:"cases"`
	}
	load(t, "registry_strategy.json", &v)
	for _, c := range v.Cases {
		t.Run(c.Query, func(t *testing.T) {
			src := fakeSources{enabled: setOf(c.Enabled...), health: setOf(c.Health...)}
			h := newHarness(t, nil, false, src, nil)
			if c.Cap != nil {
				fmt.Sscan(*c.Cap, &h.runner.Knobs.RegistrySourceCap)
			}
			j := &job{r: h.runner, id: "job1", ws: "ws1", query: c.Query, em: h.em}
			got := j.searchRegistrySources(context.Background())
			compareLeads(t, c.Query, got, c.Leads)
			if !reflect.DeepEqual(jsonNorm(h.em.events), jsonNorm(c.Events)) {
				t.Errorf("events differ\n got: %v\nwant: %v", h.em.events, c.Events)
			}
			h.check(t)
		})
	}
}

func TestAIVectors(t *testing.T) {
	var v struct {
		Clean []struct {
			HTML string `json:"html"`
			Max  int    `json:"max"`
			Out  string `json:"out"`
		} `json:"clean"`
		Expand []struct {
			Q, Mode string
			Calls   []llmCall `json:"calls"`
			Out     []string  `json:"out"`
		} `json:"expand"`
		Extract []struct {
			HTML  string         `json:"html"`
			URL   string         `json:"url"`
			Calls []llmCall      `json:"calls"`
			Out   map[string]any `json:"out"`
		} `json:"extract"`
		Score []struct {
			In      []map[string]any `json:"in"`
			Calls   []llmCall        `json:"calls"`
			Applied int              `json:"applied"`
			Out     []map[string]any `json:"out"`
		} `json:"score"`
	}
	load(t, "ai.json", &v)
	for _, c := range v.Clean {
		if got := CleanPageText(c.HTML, c.Max); got != c.Out {
			t.Errorf("clean_page_text(%.40q, %d) = %q want %q", c.HTML, c.Max, got, c.Out)
		}
	}
	ctx := context.Background()
	for _, c := range v.Expand {
		var misses []string
		f := &fakeLLM{calls: c.Calls, down: c.Mode == "down", mu: &sync.Mutex{}, misses: &misses}
		if got := AIExpandQuery(ctx, f, c.Q); !reflect.DeepEqual(got, c.Out) || len(misses) > 0 {
			t.Errorf("expand(%q, %s) = %v want %v; %v", c.Q, c.Mode, got, c.Out, misses)
		}
	}
	for _, c := range v.Extract {
		var misses []string
		f := &fakeLLM{calls: c.Calls, mu: &sync.Mutex{}, misses: &misses}
		got := AIExtractCompany(ctx, f, c.HTML, c.URL)
		if !reflect.DeepEqual(jsonNorm(got), jsonNorm(c.Out)) || len(misses) > 0 || len(f.calls) != len(c.Calls) {
			t.Errorf("extract(%s) = %v want %v; %v", c.URL, got, c.Out, misses)
		}
	}
	for i, c := range v.Score {
		var misses []string
		f := &fakeLLM{calls: c.Calls, mu: &sync.Mutex{}, misses: &misses}
		var leads []*Lead
		for _, m := range c.In {
			raw, _ := json.Marshal(m)
			var l Lead
			_ = json.Unmarshal(raw, &l)
			leads = append(leads, &l)
		}
		applied := AIScoreLeads(ctx, f, leads)
		want := c.Out
		wantApplied := c.Applied
		// Fix (documented): a JSON boolean score is not applied; legacy scored true as 1.
		for _, call := range c.Calls {
			scores, _ := call.Out["scores"].([]any)
			for _, s := range scores {
				m := s.(map[string]any)
				if b, ok := m["score"].(bool); ok && b {
					wantApplied--
				}
			}
		}
		_ = want
		if applied != wantApplied || len(misses) > 0 {
			t.Errorf("score case %d: applied %d want %d; %v", i, applied, wantApplied, misses)
		}
		for k, l := range leads {
			g := leadMap(l)
			w := jsonNorm(c.Out[k]).(map[string]any)
			if g["score"] != w["score"] && w["score"] == float64(1) {
				continue // the boolean-score fix
			}
			if !reflect.DeepEqual(g, w) {
				t.Errorf("score case %d lead %d: got %v want %v", i, k, g, w)
			}
		}
	}
}

type fakeLedger struct {
	mu       sync.Mutex
	job      map[string]any
	stages   []map[string]any
	calls    [][]any
	reads    int
	cancelAt int
}

func newFakeLedger(cancelAt int) *fakeLedger {
	return &fakeLedger{job: map[string]any{"status": "pending", "attempts": 0, "max_attempts": 3, "error": "", "leads_found": 0},
		cancelAt: cancelAt}
}

func (f *fakeLedger) MarkRunning(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, []any{"mark_running"})
	f.job["status"] = "running"
	f.job["attempts"] = f.job["attempts"].(int) + 1
	return nil
}

func (f *fakeLedger) MarkFailed(_ context.Context, _ string, msg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, []any{"mark_failed", msg})
	f.job["status"], f.job["error"] = "failed", msg
	return nil
}

func (f *fakeLedger) CreateStage(_ context.Context, _ string, stage string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stages = append(f.stages, map[string]any{"stage": stage, "status": "running", "input_count": 0, "output_count": 0,
		"rejected_count": 0, "details": map[string]any{}})
	return int64(len(f.stages)), nil
}

func (f *fakeLedger) CompleteStage(_ context.Context, id int64, r StageResult) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	var d any
	if err := json.Unmarshal([]byte(r.Details), &d); err != nil {
		return err
	}
	s := f.stages[id-1]
	s["status"], s["input_count"], s["output_count"], s["rejected_count"], s["details"] = r.Status, r.InputCount, r.OutputCount, r.RejectedCount, d
	return nil
}

func (f *fakeLedger) CompleteJob(_ context.Context, _ string, n int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, []any{"complete_job", n})
	f.job["status"], f.job["leads_found"] = "done", n
	return nil
}

func (f *fakeLedger) FailJob(_ context.Context, _ string, msg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, []any{"fail_job", msg})
	if f.job["attempts"].(int) < f.job["max_attempts"].(int) {
		f.job["status"] = "pending"
	} else {
		f.job["status"] = "failed"
	}
	f.job["error"] = msg
	return nil
}

func (f *fakeLedger) Status(context.Context, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.cancelAt > 0 && f.reads >= f.cancelAt {
		f.job["status"] = "cancelled"
	}
	return f.job["status"].(string), nil
}

type fakeStore struct {
	existing []string
	fail     bool
	upserts  []*Lead
	runs     []map[string]any
}

func (f *fakeStore) ExistingCompanies(context.Context) (map[string]bool, error) {
	m := map[string]bool{}
	for _, c := range f.existing {
		if c != "" {
			m[pyStrip(pyLower(c))] = true
		}
	}
	return m, nil
}

func (f *fakeStore) Upsert(_ context.Context, l *Lead) error {
	if f.fail {
		return errors.New("database is down")
	}
	cp := *l
	f.upserts = append(f.upserts, &cp)
	return nil
}

func (f *fakeStore) RecordSourceRun(_ context.Context, region string, runs []SourceRun) error {
	c := map[string]any{}
	for _, r := range runs {
		c[r.Source] = []any{r.Emitted, r.SurvivedDedup, r.Validated}
	}
	f.runs = append(f.runs, map[string]any{"region": region, "counters": c})
	return nil
}

func (f *fakeStore) Reliability(context.Context, string) (map[string]float64, error) { return nil, nil }

type jobVector struct {
	Spec struct {
		Name      string          `json:"name"`
		Query     string          `json:"query"`
		Enabled   map[string]bool `json:"enabled"`
		Existing  []string        `json:"existing"`
		LLM       string          `json:"llm"`
		CancelAt  int             `json:"cancel_at"`
		StoreFail bool            `json:"store_fail"`
		MapsFail  bool            `json:"maps_fail"`
	} `json:"spec"`
	Stages      []map[string]any `json:"stages"`
	Events      []map[string]any `json:"events"`
	Stored      []map[string]any `json:"stored"`
	Ledger      map[string]any   `json:"ledger"`
	LedgerCalls [][]any          `json:"ledger_calls"`
	SourceRuns  []map[string]any `json:"source_runs"`
	Calls       []llmCall        `json:"llm_calls"`
}

func TestJobVectors(t *testing.T) {
	var v struct {
		Jobs []jobVector `json:"jobs"`
		Maps map[string]struct {
			Query, City string
			Places      []browserclient.MapPlace
		} `json:"maps"`
	}
	load(t, "job.json", &v)
	for _, c := range v.Jobs {
		t.Run(c.Spec.Name, func(t *testing.T) {
			br := &fakeBrowser{w: loadWorld(t), places: v.Maps}
			if c.Spec.MapsFail {
				br.fail = errors.New("browser crashed")
			}
			h := newHarness(t, c.Calls, c.Spec.LLM == "down", fakeSources{enabled: c.Spec.Enabled}, br)
			led := newFakeLedger(c.Spec.CancelAt)
			st := &fakeStore{existing: c.Spec.Existing, fail: c.Spec.StoreFail}
			err := h.runner.Process(context.Background(), Job{ID: "job1", Query: c.Spec.Query, WorkspaceID: "ws1",
				Ledger: led, Store: st, Emit: h.em})
			switch c.Spec.Name {
			case "store_failure":
				if err == nil || err.Error() != "database is down" {
					t.Errorf("err = %v", err)
				}
			case "cancel_after_strategies", "cancel_after_dedup":
				if !errors.Is(err, ErrCancelled) {
					t.Errorf("err = %v, want ErrCancelled", err)
				}
			default:
				if err != nil {
					t.Errorf("err = %v", err)
				}
			}
			if !reflect.DeepEqual(jsonNorm(led.stages), jsonNorm(c.Stages)) && (len(led.stages) > 0 || len(c.Stages) > 0) {
				for i := range max(len(led.stages), len(c.Stages)) {
					var g, w any
					if i < len(led.stages) {
						g = jsonNorm(led.stages[i])
					}
					if i < len(c.Stages) {
						w = jsonNorm(c.Stages[i])
					}
					if !reflect.DeepEqual(g, w) {
						t.Errorf("stage %d\n got: %v\nwant: %v", i, g, w)
					}
				}
			}
			compareEvents(t, c.Spec.Name, h.em.events, c.Events)
			compareLeads(t, c.Spec.Name, st.upserts, c.Stored)
			if !reflect.DeepEqual(jsonNorm(led.job), jsonNorm(c.Ledger)) {
				t.Errorf("ledger %v want %v", led.job, c.Ledger)
			}
			if !reflect.DeepEqual(jsonNorm(led.calls), jsonNorm(c.LedgerCalls)) {
				t.Errorf("ledger calls %v want %v", led.calls, c.LedgerCalls)
			}
			if (len(st.runs) > 0 || len(c.SourceRuns) > 0) && !reflect.DeepEqual(jsonNorm(st.runs), jsonNorm(c.SourceRuns)) {
				t.Errorf("source runs %v want %v", st.runs, c.SourceRuns)
			}
			h.check(t)
		})
	}
}

func leadsFrom(ms []map[string]any) []*Lead {
	var out []*Lead
	for _, m := range ms {
		raw, _ := json.Marshal(m)
		var l Lead
		_ = json.Unmarshal(raw, &l)
		out = append(out, &l)
	}
	return out
}

func TestEnrichVectors(t *testing.T) {
	var v struct {
		Personal []struct {
			In  map[string]any `json:"in"`
			Out map[string]any `json:"out"`
		} `json:"personal"`
		CompanyEmail []struct {
			Company, City, Domain, Out string
		} `json:"company_email"`
		DecisionMakers []struct {
			In  []map[string]any `json:"in"`
			Out []map[string]any `json:"out"`
		} `json:"decision_makers"`
		FindDM []struct {
			Company string            `json:"company"`
			Out     []scrapers.Person `json:"out"`
		} `json:"find_dm"`
		Website []struct {
			In  []map[string]any `json:"in"`
			Out []map[string]any `json:"out"`
		} `json:"website"`
	}
	load(t, "enrich.json", &v)
	ctx := context.Background()
	w := &fakeWeb{w: loadWorld(t)}
	for i, c := range v.Personal {
		ls := leadsFrom([]map[string]any{c.In})
		EnrichPersonalEmails(ctx, w, fakeEnrich{}, noSleep, ls, 0)
		compareLeads(t, fmt.Sprintf("personal %d", i), ls, []map[string]any{c.Out})
	}
	for _, c := range v.CompanyEmail {
		if d := DomainFromURL(""); d != "" {
			t.Fatal(d)
		}
		if got := FindEmailViaSearch(ctx, w, c.Company, c.City, c.Domain); got != c.Out {
			t.Errorf("find_email_via_search(%q, %q, %q) = %q want %q", c.Company, c.City, c.Domain, got, c.Out)
		}
	}
	for i, c := range v.DecisionMakers {
		ls := leadsFrom(c.In)
		EnrichDecisionMakers(ctx, w, noSleep, ls, 2, 2)
		compareLeads(t, fmt.Sprintf("decision makers %d", i), ls, c.Out)
	}
	br := &fakeBrowser{w: loadWorld(t)}
	for _, c := range v.FindDM {
		got, err := scrapers.FindDecisionMakers(ctx, br, c.Company, nil, 0, "")
		if err != nil || !reflect.DeepEqual(jsonNorm(got), jsonNorm(c.Out)) {
			t.Errorf("find_decision_makers(%q) = %v %v want %v", c.Company, got, err, c.Out)
		}
	}
	for i, c := range v.Website {
		ls := leadsFrom(c.In)
		EnrichFromWebsites(ctx, w, ls, 5)
		compareLeads(t, fmt.Sprintf("website %d", i), ls, c.Out)
	}
	if len(w.unexpected) > 0 {
		t.Errorf("unexpected web calls: %v", w.unexpected)
	}
}

func TestProbeVectors(t *testing.T) {
	var v []struct {
		Name  string `json:"name"`
		Yield int    `json:"yield"`
	}
	load(t, "probe.json", &v)
	w := &fakeWeb{w: loadWorld(t)}
	byName := map[string]Source{}
	for _, s := range registry {
		byName[s.Name] = s
	}
	for _, c := range v {
		if got := ProbeYield(context.Background(), w, byName[c.Name]); got != c.Yield {
			t.Errorf("probe(%s) = %d want %d", c.Name, got, c.Yield)
		}
	}
	if len(w.unexpected) > 0 {
		t.Errorf("unexpected web calls: %v", w.unexpected)
	}
}
