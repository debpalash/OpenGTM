package personintel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/browserclient"
)

type event struct {
	step string
	data map[string]any
}

type recorder struct {
	mu sync.Mutex
	ev []event
}

func (r *recorder) notify(step string, data map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ev = append(r.ev, event{step, data})
}

func (r *recorder) steps() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, e := range r.ev {
		out = append(out, e.step)
	}
	return out
}

func (r *recorder) count(step string) int {
	n := 0
	for _, s := range r.steps() {
		if s == step {
			n++
		}
	}
	return n
}

// withGitHub points the GitHub API at a stub answering 404 and the DDG
// endpoint at h; both are allowed by the returned egress client.
func withPipelineStubs(t *testing.T, ddg http.HandlerFunc) *http.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/lite") {
			ddg(w, r)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}))
	oldD, oldG, oldB := ddgEndpoint, githubAPI, batchDelay
	ddgEndpoint, githubAPI, batchDelay = srv.URL+"/lite/", srv.URL, 0
	t.Cleanup(func() { ddgEndpoint, githubAPI, batchDelay = oldD, oldG, oldB; srv.Close() })
	return stubClient(t, srv)
}

func recordedDDGHTML(t *testing.T) string {
	t.Helper()
	var v struct{ Cases []ddgCase }
	loadVectors(t, "ddg_lite.json", &v)
	return v.Cases[0].HTML
}

func ddgHTMLFor(urls []string) string {
	var b strings.Builder
	b.WriteString(`<html><body><form><table border="0">` + "\n")
	for i, u := range urls {
		fmt.Fprintf(&b, `<tr><td valign="top">%d.&nbsp;</td><td><a rel="nofollow" href="%s" class="result-url">Page %d</a></td></tr>`+"\n", i+1, u, i)
		fmt.Fprintf(&b, `<tr><td>&nbsp;&nbsp;&nbsp;</td><td class="result-snippet">snippet %d</td></tr>`+"\n", i)
		b.WriteString(`<tr><td>&nbsp;&nbsp;&nbsp;</td><td><span class="link-text">link</span></td></tr><tr><td colspan="2">&nbsp;</td></tr>` + "\n")
	}
	b.WriteString("</table></form></body></html>")
	return b.String()
}

func TestStepSequenceMatchesRecordedRun(t *testing.T) {
	var rec struct {
		URL      string   `json:"linkedin_url"`
		Messages []string `json:"messages"`
		Status   string   `json:"status"`
	}
	loadVectors(t, "pipeline_run.json", &rec)
	html := recordedDDGHTML(t)
	c := withPipelineStubs(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(html)) })

	r := &recorder{}
	var raw []string
	var mu sync.Mutex
	p, err := Enrich(context.Background(), Deps{HTTP: c}, rec.URL, false, func(step string, data map[string]any) {
		r.notify(step, data)
		b, _ := EventJSON(step, data)
		mu.Lock()
		raw = append(raw, string(b))
		mu.Unlock()
	})
	if err != nil || p.Status != rec.Status {
		t.Fatalf("status %q err %v", p.Status, err)
	}
	if len(raw) != len(rec.Messages) {
		t.Fatalf("got %d events, recorded %d:\n%v", len(raw), len(rec.Messages), r.steps())
	}
	// search_progress order inside the batches is concurrent (and Python's is
	// completion order): compare those as a multiset, everything else exactly.
	isProg := func(s string) bool { return strings.Contains(s[:min(len(s), 32)], "search_progress") }
	var gotProg, wantProg []string
	for i := range raw {
		g, w := raw[i], rec.Messages[i]
		if isProg(g) != isProg(w) {
			t.Fatalf("event %d: got %s want %s", i, g, w)
		}
		if isProg(g) {
			gotProg, wantProg = append(gotProg, g), append(wantProg, strings.Replace(w, `{"step": "search_progress", "category": `, `{"step":"search_progress","category":`, 1))
			continue
		}
		if strings.HasPrefix(w, `{"step": "completed"`) {
			var gm, wm map[string]any
			if json.Unmarshal([]byte(g), &gm) != nil || json.Unmarshal([]byte(w), &wm) != nil {
				t.Fatal("completed not json")
			}
			gp, wp := gm["profile"].(map[string]any), wm["profile"].(map[string]any)
			for _, k := range []string{"emails", "skills"} { // Python sets: unordered
				for _, m := range []map[string]any{gp, wp} {
					var s []string
					for _, e := range m[k].([]any) {
						s = append(s, e.(string))
					}
					sort.Strings(s)
					m[k] = s
				}
			}
			if !reflect.DeepEqual(gp, wp) {
				t.Fatalf("completed profile differs:\n got %v\nwant %v", gp, wp)
			}
			continue
		}
		if normJSON(t, g) != normJSON(t, w) {
			t.Fatalf("event %d:\n got %s\nwant %s", i, g, w)
		}
	}
	sort.Strings(gotProg)
	sort.Strings(wantProg)
	for i := range gotProg {
		if normJSON(t, gotProg[i]) != normJSON(t, wantProg[i]) {
			t.Fatalf("progress %s != %s", gotProg[i], wantProg[i])
		}
	}
	// key order of the multi-key payloads is Python's insertion order
	for _, s := range raw {
		if strings.HasPrefix(s, `{"step":"search_done"`) && !strings.HasPrefix(s, `{"step":"search_done","results_count":`) {
			t.Fatalf("search_done key order: %s", s)
		}
	}
}

// normJSON re-renders s with Python's separators removed so a Python
// json.dumps line and Go's compact line compare equal, key order kept.
func normJSON(t *testing.T, s string) string {
	t.Helper()
	var b strings.Builder
	inStr, esc := false, false
	for _, r := range s {
		switch {
		case inStr:
			b.WriteRune(r)
			if esc {
				esc = false
			} else if r == '\\' {
				esc = true
			} else if r == '"' {
				inStr = false
			}
		case r == '"':
			inStr = true
			b.WriteRune(r)
		case r == ' ':
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// fakeRenderer is the browser service's /v1/render.
type fakeRenderer struct {
	mu    sync.Mutex
	calls []browserclient.RenderRequest
	fn    func(ctx context.Context, req browserclient.RenderRequest) (browserclient.RenderResult, error)
}

func (f *fakeRenderer) Render(ctx context.Context, req browserclient.RenderRequest) (browserclient.RenderResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	f.mu.Unlock()
	return f.fn(ctx, req)
}

func renderedPage(body string) browserclient.RenderResult {
	return browserclient.RenderResult{Status: 200, HTML: "<html><head><script>var x='evil@bad.io'</script></head><body>" + body + "</body></html>"}
}

func manyURLs(n int) []string {
	var u []string
	for i := 0; i < n; i++ {
		u = append(u, fmt.Sprintf("https://site%02d.example.org/p", i))
	}
	return u
}

func fastDeep(t *testing.T) {
	t.Helper()
	old := deepDelay
	deepDelay = 0
	t.Cleanup(func() { deepDelay = old })
}

// Without a browser service the deep phase still runs in Go; only JavaScript-wall pages are skipped.
func TestDeepWithoutBrowserSkipsOnlyJSWallPages(t *testing.T) {
	html := recordedDDGHTML(t)
	for name, b := range map[string]Renderer{"nil interface": nil, "nil client": (*browserclient.Client)(nil),
		"unconfigured client": browserclient.New("", "", nil)} {
		t.Run(name, func(t *testing.T) {
			fastDeep(t)
			c := withPipelineStubs(t, func(rw http.ResponseWriter, r *http.Request) { _, _ = rw.Write([]byte(html)) })
			r := &recorder{}
			p, err := Enrich(context.Background(), Deps{HTTP: c, Browser: b, Fetch: jsWall}, "https://www.linkedin.com/in/jane-doe", true, r.notify)
			if err != nil || p.Status != "completed" {
				t.Fatalf("%q %v", p.Status, err)
			}
			if r.count("deep_unavailable") != 0 || r.count("deep_scraping") != 1 {
				t.Fatalf("steps %v", r.steps())
			}
		})
	}
}

// A browser service that is down or misconfigured ends the deep phase with deep_unavailable; a page it cannot
// load is only skipped.
func TestDeepUnavailableWhenBrowserRejects(t *testing.T) {
	for _, e := range []error{browserclient.ErrUnavailable, browserclient.ErrUnauthorized, browserclient.ErrBrowserUnavailable,
		browserclient.ErrEngineUnavailable} {
		fastDeep(t)
		c := withPipelineStubs(t, func(rw http.ResponseWriter, r *http.Request) { _, _ = rw.Write([]byte(ddgHTMLFor(manyURLs(5)))) })
		fr := &fakeRenderer{fn: func(context.Context, browserclient.RenderRequest) (browserclient.RenderResult, error) {
			return browserclient.RenderResult{}, e
		}}
		r := &recorder{}
		p, err := Enrich(context.Background(), Deps{HTTP: c, Browser: fr, Fetch: jsWall}, "https://www.linkedin.com/in/jane-doe", true, r.notify)
		if err != nil || p.Status != "completed" || r.count("deep_unavailable") != 1 || len(fr.calls) != 1 {
			t.Fatalf("%v: status %q err %v calls %d steps %v", e, p.Status, err, len(fr.calls), r.steps())
		}
		for _, ev := range r.ev {
			if ev.step == "deep_unavailable" && ev.data["message"] != "Deep scraping unavailable: browser service not reachable" {
				t.Fatalf("message %v", ev.data)
			}
		}
	}
	fastDeep(t)
	c := withPipelineStubs(t, func(rw http.ResponseWriter, r *http.Request) { _, _ = rw.Write([]byte(ddgHTMLFor(manyURLs(3)))) })
	fr := &fakeRenderer{fn: func(context.Context, browserclient.RenderRequest) (browserclient.RenderResult, error) {
		return browserclient.RenderResult{}, browserclient.ErrNavigation
	}}
	r := &recorder{}
	if _, err := Enrich(context.Background(), Deps{HTTP: c, Browser: fr, Fetch: jsWall}, "https://www.linkedin.com/in/jane-doe", true, r.notify); err != nil {
		t.Fatal(err)
	}
	if r.count("deep_unavailable") != 0 || len(fr.calls) != 3 {
		t.Fatalf("navigation failures skip pages: calls %d steps %v", len(fr.calls), r.steps())
	}
}

func TestDeepBoundedAndCancelled(t *testing.T) {
	t.Run("at most 10 pages, skip list, merge", func(t *testing.T) {
		fastDeep(t)
		urls := append([]string{"https://www.linkedin.com/in/x", "https://google.com/q", "https://duckduckgo.com/x"}, manyURLs(14)...)
		c := withPipelineStubs(t, func(rw http.ResponseWriter, r *http.Request) { _, _ = rw.Write([]byte(ddgHTMLFor(urls))) })
		fr := &fakeRenderer{fn: func(context.Context, browserclient.RenderRequest) (browserclient.RenderResult, error) {
			return renderedPage("<p>" + strings.Repeat("é", 1500) + "</p><p>jane@acme.io not an email x@" + strings.Repeat("a", 400) + ".io</p>"), nil
		}}
		r := &recorder{}
		p, err := Enrich(context.Background(), Deps{HTTP: c, Browser: fr, Fetch: jsWall}, "https://www.linkedin.com/in/jane-doe", true, r.notify)
		if err != nil || p.Status != "completed" {
			t.Fatalf("%q %v", p.Status, err)
		}
		if len(fr.calls) != 10 || r.count("scrape_progress") != 10 {
			t.Fatalf("calls %d progress %d", len(fr.calls), r.count("scrape_progress"))
		}
		seen := map[string]bool{}
		for _, call := range fr.calls {
			u := call.URL
			if !call.IncludeHTML || call.MaxBytes <= 0 || call.Timeout <= 0 || seen[u] || strings.Contains(u, "linkedin.com") || strings.Contains(u, "google.com") || strings.Contains(u, "duckduckgo.com") {
				t.Fatalf("bad call %+v", call)
			}
			seen[u] = true
		}
		if !contains(p.Emails, "jane@acme.io") || contains(p.Emails, "not an email") || len(p.Emails) != 1 {
			t.Fatalf("emails %v", p.Emails)
		}
		scraped := 0
		for _, s := range p.RawSources {
			if s.ScrapedText != "" {
				scraped++
				if n := len([]rune(s.ScrapedText)); n != 1000 || s.ScrapedWordCount != 6 || len(s.ScrapedEmails) != 1 || strings.Contains(s.ScrapedText, "evil") {
					t.Fatalf("scraped fields %d %d %v", n, s.ScrapedWordCount, s.ScrapedEmails)
				}
			}
		}
		if scraped != 10 {
			t.Fatalf("scraped %d", scraped)
		}
		// payload keys of the deep steps
		for _, e := range r.ev {
			if e.step == "deep_scraping" {
				if e.data["total"] != 15 || e.data["message"] != "Deep scraping result pages..." {
					t.Fatalf("deep_scraping %v", e.data)
				}
			}
			if e.step == "scrape_progress" && (e.data["url"] == nil || !strings.HasSuffix(e.data["progress"].(string), "/10")) {
				t.Fatalf("scrape_progress %v", e.data)
			}
		}
	})

	t.Run("60 s budget ends the deep phase, enrichment still completes", func(t *testing.T) {
		fastDeep(t)
		old := deepBudget
		deepBudget = 80 * time.Millisecond
		t.Cleanup(func() { deepBudget = old })
		c := withPipelineStubs(t, func(rw http.ResponseWriter, r *http.Request) { _, _ = rw.Write([]byte(ddgHTMLFor(manyURLs(6)))) })
		fr := &fakeRenderer{fn: func(ctx context.Context, _ browserclient.RenderRequest) (browserclient.RenderResult, error) {
			<-ctx.Done()
			return browserclient.RenderResult{}, ctx.Err()
		}}
		r := &recorder{}
		start := time.Now()
		p, err := Enrich(context.Background(), Deps{HTTP: c, Browser: fr, Fetch: jsWall}, "https://www.linkedin.com/in/jane-doe", true, r.notify)
		if err != nil || p.Status != "completed" || time.Since(start) > 5*time.Second || len(fr.calls) != 1 || r.count("completed") != 1 {
			t.Fatalf("%q %v calls %d", p.Status, err, len(fr.calls))
		}
	})

	t.Run("caller cancel aborts the in-flight page call", func(t *testing.T) {
		fastDeep(t)
		c := withPipelineStubs(t, func(rw http.ResponseWriter, r *http.Request) { _, _ = rw.Write([]byte(ddgHTMLFor(manyURLs(6)))) })
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		fr := &fakeRenderer{fn: func(ctx context.Context, _ browserclient.RenderRequest) (browserclient.RenderResult, error) {
			cancel()
			<-ctx.Done()
			return browserclient.RenderResult{}, ctx.Err()
		}}
		r := &recorder{}
		_, err := Enrich(ctx, Deps{HTTP: c, Browser: fr, Fetch: jsWall}, "https://www.linkedin.com/in/jane-doe", true, r.notify)
		if err == nil || ctx.Err() == nil || len(fr.calls) != 1 || r.count("completed") != 0 {
			t.Fatalf("err %v calls %d steps %v", err, len(fr.calls), r.steps())
		}
	})
}

func TestEnrichHonoursContextCancel(t *testing.T) {
	c := withPipelineStubs(t, func(rw http.ResponseWriter, r *http.Request) { _, _ = rw.Write([]byte(recordedDDGHTML(t))) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &recorder{}
	_, err := Enrich(ctx, Deps{HTTP: c}, "https://www.linkedin.com/in/jane-doe", false, func(step string, d map[string]any) {
		r.notify(step, d)
		if step == "searching" {
			cancel()
		}
	})
	if err != context.Canceled {
		t.Fatalf("err %v", err)
	}
	if r.count("completed") != 0 || r.count("error") != 0 {
		t.Fatalf("steps %v", r.steps())
	}
}

func TestEnrichBadURLFailsLikePython(t *testing.T) {
	r := &recorder{}
	p, err := Enrich(context.Background(), Deps{}, "https://example.com/nope", false, r.notify)
	if err != nil || p.Status != "failed" {
		t.Fatalf("%q %v", p.Status, err)
	}
	if got := r.steps(); !reflect.DeepEqual(got, []string{"error"}) {
		t.Fatalf("steps %v", got)
	}
	if r.ev[0].data["message"] != "Could not parse LinkedIn username from: https://example.com/nope" {
		t.Fatalf("%v", r.ev[0].data)
	}
}

func TestEventJSONOrderAndNoHTMLEscape(t *testing.T) {
	b, err := EventJSON("profile_extracted", map[string]any{"headline": "A & <B>", "name": "N"})
	if err != nil || string(b) != `{"step":"profile_extracted","name":"N","headline":"A & <B>"}` {
		t.Fatalf("%s %v", b, err)
	}
	b, _ = EventJSON("scrape_progress", map[string]any{"progress": "1/2", "url": "u"})
	if string(b) != `{"step":"scrape_progress","url":"u","progress":"1/2"}` {
		t.Fatalf("%s", b)
	}
	// unknown keys follow, sorted
	b, _ = EventJSON("zzz", map[string]any{"b": 1, "a": 2})
	if string(b) != `{"step":"zzz","a":2,"b":1}` {
		t.Fatalf("%s", b)
	}
}

func TestDeepKeepsPageFinishingAtBudgetExpiry(t *testing.T) {
	fastDeep(t)
	old := deepBudget
	deepBudget = 50 * time.Millisecond
	t.Cleanup(func() { deepBudget = old })
	c := withPipelineStubs(t, func(rw http.ResponseWriter, r *http.Request) { _, _ = rw.Write([]byte(ddgHTMLFor(manyURLs(3)))) })
	fc := &fakeRenderer{fn: func(ctx context.Context, _ browserclient.RenderRequest) (browserclient.RenderResult, error) {
		<-ctx.Done() // the answer lands as the budget expires
		return renderedPage("<p>late page late@acme.io</p>"), nil
	}}
	p, err := Enrich(context.Background(), Deps{HTTP: c, Browser: fc, Fetch: jsWall}, "https://www.linkedin.com/in/jane-doe", true, nil)
	if err != nil || len(fc.calls) != 1 || !contains(p.Emails, "late@acme.io") {
		t.Fatalf("err %v calls %d emails %v", err, len(fc.calls), p.Emails)
	}
}
