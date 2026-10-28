package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/browserclient"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/unisearch"
)

// scrape/vectors.json is recorded from the real UniversalScraper.scrape (fast path against a local server, the
// Playwright path stubbed) together with what the browser service answers for the same page
// (browser_service.textutil.page_meta / html_to_text). See legacy/python/scripts/record_scrape.py.

type scrapeVector struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Service struct {
		Status      int    `json:"status"`
		FinalURL    string `json:"final_url"`
		HTML        string `json:"html"`
		Text        string `json:"text"`
		Title       string `json:"title"`
		Description string `json:"description"`
	} `json:"service"`
	Python       map[string]any `json:"python"`
	PythonRobust map[string]any `json:"python_robust"`
}

const pagesHost = "http://pages.test"

func loadScrapeVectors(t *testing.T) []scrapeVector {
	t.Helper()
	b, err := os.ReadFile("testdata/scrape/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v []scrapeVector
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func subBase(s string) string { return strings.ReplaceAll(s, "{{BASE}}", pagesHost) }

// fakeBrowser is a browser service that replays vectors by page path and records every render request.
type fakeBrowser struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []map[string]any
	// answer builds the render answer; nil answers 500.
	answer func(req map[string]any) (int, any)
}

func newFakeBrowser(t *testing.T, answer func(req map[string]any) (int, any)) *fakeBrowser {
	t.Helper()
	f := &fakeBrowser{answer: answer}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/render" || r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(404)
			return
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.reqs = append(f.reqs, req)
		f.mu.Unlock()
		status, body := f.answer(req)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeBrowser) calls() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.reqs...)
}

func scrapePublicLookup(context.Context, string) ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
}

func routesFor(f *fakeBrowser) *scrapeRoutes {
	return &scrapeRoutes{d: Deps{}, log: Deps{}.logger(), render: browserclient.New(f.srv.URL, "tok", nil),
		guard: urlGuard{lookup: scrapePublicLookup}}
}

func renderBody(v scrapeVector, status int, html, text string) map[string]any {
	return map[string]any{"url": pagesHost + v.Path, "final_url": subBase(v.Service.FinalURL), "status": status, "engine": "x",
		"title": v.Service.Title, "description": v.Service.Description, "html": html, "text": text,
		"html_truncated": false, "text_truncated": false, "challenge": false, "blocked_requests": 0, "elapsed_ms": 1}
}

func scrapeAsMap(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func sortedStrings(v any) []string {
	var out []string
	for _, e := range v.([]any) {
		out = append(out, e.(string))
	}
	sort.Strings(out)
	return out
}

func TestScrapeMatchesRecordedPython(t *testing.T) {
	vectors := loadScrapeVectors(t)
	if len(vectors) < 10 {
		t.Fatalf("vectors truncated: %d", len(vectors))
	}
	for _, v := range vectors {
		t.Run(v.Name, func(t *testing.T) {
			f := newFakeBrowser(t, func(req map[string]any) (int, any) {
				return 200, renderBody(v, v.Service.Status, v.Service.HTML, v.Service.Text)
			})
			res, err := routesFor(f).scrape(context.Background(), pagesHost+v.Path)
			if err != nil {
				t.Fatal(err)
			}
			want := v.Python
			wantCalls := 1
			if want["method"] == "robust-stub" { // Python fell back to Playwright
				want = v.PythonRobust
				wantCalls = 2
			}
			got := scrapeAsMap(t, publicScrapeJSON(res))
			wantSub := map[string]any{}
			b, _ := json.Marshal(want)
			if err := json.Unmarshal([]byte(subBase(string(b))), &wantSub); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(sortedStrings(got["extracted_emails"]), sortedStrings(wantSub["extracted_emails"])) {
				t.Errorf("emails %v, Python %v", got["extracted_emails"], wantSub["extracted_emails"])
			}
			for _, k := range []string{"extracted_emails", "title"} {
				delete(got, k)
				delete(wantSub, k)
			}
			if !reflect.DeepEqual(got, wantSub) {
				gb, _ := json.MarshalIndent(got, "", " ")
				wb, _ := json.MarshalIndent(wantSub, "", " ")
				t.Errorf("result differs\n got: %s\nwant: %s", gb, wb)
			}
			// title: Python kept the raw <title> string; the service strips it (deviation, documented)
			if gt, wt := scrapeAsMap(t, publicScrapeJSON(res))["title"].(string), subBase(want["title"].(string)); gt != strings.TrimSpace(wt) {
				t.Errorf("title %q, Python %q", gt, wt)
			}
			calls := f.calls()
			if len(calls) != wantCalls {
				t.Fatalf("%d browser calls, want %d", len(calls), wantCalls)
			}
			if calls[0]["engine"] != "impersonate" {
				t.Errorf("first call engine %v", calls[0]["engine"])
			}
			if wantCalls == 2 {
				if calls[1]["engine"] != "browser" || calls[1]["scroll"] != true || calls[1]["max_scrolls"] != float64(5) {
					t.Errorf("fallback call %+v", calls[1])
				}
			}
		})
	}
}

func TestScrapeJSONKeyOrder(t *testing.T) {
	res := scrapeResult{URL: "u", Title: "t", Method: "fast"}
	order := func(v any) []string {
		b, _ := json.Marshal(v)
		var keys []string
		dec := json.NewDecoder(strings.NewReader(string(b)))
		dec.Token()
		for dec.More() {
			k, _ := dec.Token()
			keys = append(keys, k.(string))
			var skip json.RawMessage
			_ = dec.Decode(&skip)
		}
		return keys
	}
	if got, want := order(publicScrapeJSON(res)), []string{"status", "url", "title", "description", "method", "extracted_emails", "word_count", "preview_text", "html_content"}; !reflect.DeepEqual(got, want) {
		t.Errorf("public scrape keys %v", got)
	}
	if got, want := order(scrapeResponseJSON(res)), []string{"status", "url", "title", "description", "extracted_emails", "word_count", "preview_text", "html_content", "method"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ScrapeResponse keys %v", got)
	}
	if b, _ := json.Marshal(publicScrapeJSON(res)); !strings.Contains(string(b), `"extracted_emails":[]`) {
		t.Errorf("empty emails must be [] not null: %s", b)
	}
}

func TestURLGuard(t *testing.T) {
	private := func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("10.0.0.5")}, nil
	}
	mapped := func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("::ffff:127.0.0.1")}, nil
	}
	nxdomain := func(context.Context, string) ([]netip.Addr, error) { return nil, errors.New("no such host") }
	cases := []struct {
		name string
		g    urlGuard
		url  string
		want string // "" = allowed, else substring of the reason
	}{
		{"public", urlGuard{lookup: scrapePublicLookup}, "https://example.com/x", ""},
		{"surrounding space is stripped", urlGuard{lookup: scrapePublicLookup}, "  https://example.com/x \n", ""},
		{"file scheme", urlGuard{lookup: scrapePublicLookup}, "file:///etc/passwd", "scheme"},
		{"ftp scheme", urlGuard{lookup: scrapePublicLookup}, "ftp://example.com/", "scheme"},
		{"no host", urlGuard{lookup: scrapePublicLookup}, "http:///x", "no host"},
		{"localhost", urlGuard{lookup: scrapePublicLookup}, "http://localhost/", "blocked"},
		{"loopback literal", urlGuard{lookup: scrapePublicLookup}, "http://127.0.0.1/", "private"},
		{"decimal ip", urlGuard{lookup: scrapePublicLookup}, "http://2130706433/", "private"},
		{"octal ip", urlGuard{lookup: scrapePublicLookup}, "http://0177.0.0.1/", "private"},
		{"metadata ip", urlGuard{lookup: scrapePublicLookup}, "http://169.254.169.254/latest", "private"},
		{"ipv6 loopback", urlGuard{lookup: scrapePublicLookup}, "http://[::1]/", "private"},
		{"any resolved address private", urlGuard{lookup: private}, "https://rebind.example/", "resolves to a private"},
		{"ipv4-mapped private", urlGuard{lookup: mapped}, "https://mapped.example/", "resolves to a private"},
		{"unresolvable", urlGuard{lookup: nxdomain}, "https://nope.invalid/", "cannot resolve"},
		{"allow-list ignored outside test", urlGuard{appEnv: "production", allow: []string{"intranet.test"}, lookup: private}, "http://intranet.test/", "resolves to a private"},
		{"allow-list honoured in test", urlGuard{appEnv: "test", allow: []string{"intranet.test"}, lookup: private}, "http://intranet.test/", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.g.check(context.Background(), c.url)
			switch {
			case c.want == "" && err != nil:
				t.Errorf("blocked: %v", err)
			case c.want != "" && err == nil:
				t.Errorf("allowed, want block (%s)", c.want)
			case c.want != "":
				if !errors.Is(err, errURLBlocked) || !strings.Contains(err.Error(), c.want) {
					t.Errorf("error %q, want it to contain %q and match errURLBlocked", err, c.want)
				}
			}
		})
	}
}

func TestScrapeBlockedURLNeverReachesBrowser(t *testing.T) {
	f := newFakeBrowser(t, func(map[string]any) (int, any) { return 200, map[string]any{} })
	h := routesFor(f)
	for _, u := range []string{"http://127.0.0.1/", "file:///etc/passwd", "http://localhost:8080/admin", "http://169.254.169.254/"} {
		if _, err := h.scrape(context.Background(), u); !errors.Is(err, errURLBlocked) {
			t.Errorf("%s: err = %v", u, err)
		}
	}
	if n := len(f.calls()); n != 0 {
		t.Errorf("browser service called %d times for blocked URLs", n)
	}
}

func TestScrapeServiceBlockedRedirectIsNotRetried(t *testing.T) {
	f := newFakeBrowser(t, func(map[string]any) (int, any) {
		return 403, map[string]any{"error": map[string]any{"code": "egress_blocked", "message": "x"}}
	})
	_, err := routesFor(f).scrape(context.Background(), "https://example.com/redirects-inside")
	if !errors.Is(err, errURLBlocked) {
		t.Fatalf("err = %v, want a blocked-URL error (400 upstream of here)", err)
	}
	if n := len(f.calls()); n != 1 {
		t.Errorf("%d browser calls; a blocked destination must not be retried through the browser engine", n)
	}
}

func TestScrapeNeverSendsTheOperatorLinkedInCookie(t *testing.T) {
	v := loadScrapeVectors(t)[0]
	for _, c := range []struct {
		url        string
		wantCookie bool
	}{
		{"https://www.linkedin.com/in/someone", false},
		{"https://linkedin.com/company/x", false},
		{"https://evil.example/?next=linkedin.com", false},
		{"https://notlinkedin.com/", false},
		{"https://linkedin.com.evil.example/", false},
	} {
		f := newFakeBrowser(t, func(req map[string]any) (int, any) {
			return 200, renderBody(v, 403, "<html>wall</html>", "wall") // non-200: forces the browser engine
		})
		h := routesFor(f)
		if _, err := h.scrape(context.Background(), c.url); err != nil {
			t.Fatal(err)
		}
		calls := f.calls()
		if len(calls) != 2 {
			t.Fatalf("%s: %d calls", c.url, len(calls))
		}
		if _, has := calls[0]["cookies"]; has {
			t.Errorf("%s: the impersonate call carries cookies", c.url)
		}
		_, has := calls[1]["cookies"]
		if has != c.wantCookie {
			t.Errorf("%s: browser call cookie = %v, want %v", c.url, has, c.wantCookie)
		}
	}
}

func TestScrapeBrowserNotConfigured(t *testing.T) {
	h := &scrapeRoutes{log: Deps{}.logger(), guard: urlGuard{lookup: scrapePublicLookup}}
	if _, err := h.scrape(context.Background(), "https://example.com/"); !errors.Is(err, browserclient.ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
	h.render = browserclient.New("", "", nil) // a typed client without URL or token
	if _, err := h.scrape(context.Background(), "https://example.com/"); !errors.Is(err, browserclient.ErrUnavailable) {
		t.Fatalf("unconfigured client: err = %v", err)
	}
}

// ── handlers ────────────────────────────────────────────────────────────────

type scrapeFakeRender struct {
	res   browserclient.RenderResult
	err   error
	calls int
}

func (f *scrapeFakeRender) Render(context.Context, browserclient.RenderRequest) (browserclient.RenderResult, error) {
	f.calls++
	return f.res, f.err
}

type scrapeMemStore struct {
	saved []struct {
		url string
		res scrapeResult
	}
	err error
}

func (m *scrapeMemStore) Save(_ context.Context, u string, res scrapeResult) error {
	if m.err != nil {
		return m.err
	}
	m.saved = append(m.saved, struct {
		url string
		res scrapeResult
	}{u, res})
	return nil
}
func (m *scrapeMemStore) List(context.Context, int, int) ([]historyRow, error) { return nil, nil }

func scrapeOKRender() *scrapeFakeRender {
	return &scrapeFakeRender{res: browserclient.RenderResult{Status: 200, FinalURL: "https://example.com/final", Title: "T", Description: "D",
		HTML: "<html>h</html>", Text: "hello world mail me@example.com"}}
}

func scrapePost(h http.HandlerFunc, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/x", strings.NewReader(body))
	req.RemoteAddr = "203.0.113.9:1234"
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func TestPublicScrapeHandler(t *testing.T) {
	newH := func(r pageRenderer) *scrapeRoutes {
		return &scrapeRoutes{d: Deps{Limiter: frozenLimiter()}, log: Deps{}.logger(), render: r, guard: urlGuard{lookup: scrapePublicLookup}}
	}
	type tc struct {
		name, body string
		render     pageRenderer
		code       int
		contains   string
	}
	blocked := &scrapeFakeRender{err: &browserclient.Error{Code: "egress_blocked"}}
	for _, c := range []tc{
		{"ok", `{"url":" https://example.com/ "}`, scrapeOKRender(), 200, `"status":"success","url":"https://example.com/final"`},
		{"no body", ``, scrapeOKRender(), 422, `"loc":["body"]`},
		{"not json", `{`, scrapeOKRender(), 422, `json_invalid`},
		{"not an object", `[1]`, scrapeOKRender(), 422, `model_attributes_type`},
		{"missing url", `{}`, scrapeOKRender(), 400, `{"detail":"url is required"}`},
		{"blank url", `{"url":"   "}`, scrapeOKRender(), 400, `url is required`},
		{"non-string url", `{"url":5}`, scrapeOKRender(), 400, `url is required`},
		{"private url", `{"url":"http://10.1.2.3/"}`, scrapeOKRender(), 400, `url not allowed: ip '10.1.2.3' is private/blocked`},
		{"blocked by service", `{"url":"https://example.com/"}`, blocked, 400, `url not allowed`},
		{"fetch error is a 200 error body", `{"url":"https://example.com/"}`, &scrapeFakeRender{err: &browserclient.Error{Code: "navigation_failed"}}, 200,
			`{"url":"https://example.com/","status":"error","error":"the page could not be loaded"}`},
		{"service down is a 200 error body", `{"url":"https://example.com/"}`, &scrapeFakeRender{err: browserclient.ErrUnavailable}, 200, `"status":"error","error":"browser service is not available"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := scrapePost(newH(c.render).publicScrape, c.body)
			if rec.Code != c.code || !strings.Contains(rec.Body.String(), c.contains) {
				t.Errorf("status %d body %s; want %d containing %s", rec.Code, rec.Body, c.code, c.contains)
			}
			if strings.Contains(rec.Body.String(), "egress_blocked") || strings.Contains(rec.Body.String(), "navigation_failed") {
				t.Errorf("an internal error code leaked: %s", rec.Body)
			}
		})
	}
}

func TestPublicScrapeRateLimit(t *testing.T) {
	h := &scrapeRoutes{d: Deps{Limiter: frozenLimiter()}, log: Deps{}.logger(), render: scrapeOKRender(), guard: urlGuard{lookup: scrapePublicLookup}}
	// 30/minute: invalid bodies (422) do not count, a 400 does
	for i := 0; i < 50; i++ {
		if rec := scrapePost(h.publicScrape, `{`); rec.Code != 422 {
			t.Fatalf("422 expected, got %d", rec.Code)
		}
	}
	var last int
	for i := 0; i < 31; i++ {
		last = scrapePost(h.publicScrape, `{"url":"https://example.com/"}`).Code
	}
	if last != http.StatusTooManyRequests {
		t.Errorf("31st request: %d, want 429", last)
	}
}

func TestAdminScrapeHandler(t *testing.T) {
	st := &scrapeMemStore{}
	rr := scrapeOKRender()
	h := &scrapeRoutes{d: Deps{Limiter: frozenLimiter()}, log: Deps{}.logger(), render: rr, store: st, guard: urlGuard{lookup: scrapePublicLookup}}
	if rec := scrapePost(h.adminScrape, `{}`); rec.Code != 422 || !strings.Contains(rec.Body.String(), `"type":"missing","loc":["body","url"]`) {
		t.Errorf("missing url: %d %s", rec.Code, rec.Body)
	}
	if rec := scrapePost(h.adminScrape, `{"url":7}`); rec.Code != 422 || !strings.Contains(rec.Body.String(), "string_type") {
		t.Errorf("non-string url: %d %s", rec.Code, rec.Body)
	}
	rec := scrapePost(h.adminScrape, `{"url":"https://example.com/start"}`)
	if rec.Code != 200 || !strings.HasPrefix(rec.Body.String(), `{"status":"success","url":"https://example.com/final"`) || !strings.HasSuffix(rec.Body.String(), `"method":"fast"}`) {
		t.Errorf("ok: %d %s", rec.Code, rec.Body)
	}
	if len(st.saved) != 1 || st.saved[0].url != "https://example.com/start" || st.saved[0].res.URL != "https://example.com/final" ||
		!reflect.DeepEqual(st.saved[0].res.Emails, []string{"me@example.com"}) {
		t.Errorf("saved %+v", st.saved)
	}
	if rec := scrapePost(h.adminScrape, `{"url":"http://192.168.1.1/"}`); rec.Code != 400 || !strings.Contains(rec.Body.String(), `"detail":"URL not allowed: `) {
		t.Errorf("private: %d %s", rec.Code, rec.Body)
	}
	for _, c := range []struct {
		err  error
		code int
	}{{browserclient.ErrUnavailable, 503}, {&browserclient.Error{Code: "busy"}, 503}, {&browserclient.Error{Code: "timeout"}, 500}, {errors.New("secret internals"), 500}} {
		bad := &scrapeRoutes{log: Deps{}.logger(), render: &scrapeFakeRender{err: c.err}, store: st, guard: urlGuard{lookup: scrapePublicLookup}}
		rec := scrapePost(bad.adminScrape, `{"url":"https://example.com/"}`)
		if rec.Code != c.code || strings.Contains(rec.Body.String(), "secret") {
			t.Errorf("%v: %d %s", c.err, rec.Code, rec.Body)
		}
	}
	n := len(st.saved)
	st.err = errors.New("db down with password hunter2")
	if rec := scrapePost(h.adminScrape, `{"url":"https://example.com/again"}`); rec.Code != 500 || strings.Contains(rec.Body.String(), "hunter2") {
		t.Errorf("store failure: %d %s", rec.Code, rec.Body)
	}
	if len(st.saved) != n {
		t.Error("a failed save recorded a row")
	}
}

// ── unified search handler ──────────────────────────────────────────────────

type fakeUnified struct {
	calls []struct {
		q                string
		names            []string
		per              int
		sortBy, fileType string
	}
	out unisearch.Unified
}

func (f *fakeUnified) SearchUnified(_ context.Context, q string, names []string, per int, sortBy, ft string) unisearch.Unified {
	f.calls = append(f.calls, struct {
		q                string
		names            []string
		per              int
		sortBy, fileType string
	}{q, names, per, sortBy, ft})
	return f.out
}

func scrapeGet(h http.HandlerFunc, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", target, nil)
	req.RemoteAddr = "203.0.113.9:1234"
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func TestUnifiedHandler(t *testing.T) {
	snippet := "snip"
	year := 2020
	fs := &fakeUnified{out: unisearch.Unified{Query: "go", Total: 2, Results: []unisearch.Result{
		{ID: "a1", Title: "T1", URL: "u1", Source: "arxiv", FileType: "pdf", Year: &year, Snippet: &snippet},
		{ID: "b1", Title: "T2", URL: "u2", Source: "doaj", FileType: "other"},
	}}}
	h := &scrapeRoutes{d: Deps{Limiter: frozenLimiter()}, log: Deps{}.logger(), search: fs}

	rec := scrapeGet(h.unified, "/api/v2/search/unified")
	if rec.Code != 200 || rec.Body.String() != `{"query":"","total":0,"results":[]}` || len(fs.calls) != 0 {
		t.Errorf("empty q: %d %s (searched: %d)", rec.Code, rec.Body, len(fs.calls))
	}
	rec = scrapeGet(h.unified, "/api/v2/search/unified?q=go&sources=arxiv,%20doaj,,&limit=40&sort=date&file_type=pdf")
	want := `{"query":"go","total":2,"results":[` +
		`{"id":"a1","title":"T1","url":"u1","source":"arxiv","download_url":null,"thumbnail":null,"author":null,"year":2020,"file_type":"pdf","file_size":null,"snippet":"snip","description":"snip"},` +
		`{"id":"b1","title":"T2","url":"u2","source":"doaj","download_url":null,"thumbnail":null,"author":null,"year":null,"file_type":"other","file_size":null,"snippet":null,"description":null}]}`
	if rec.Code != 200 || rec.Body.String() != want {
		t.Errorf("body:\n got %s\nwant %s", rec.Body, want)
	}
	c := fs.calls[0]
	if c.q != "go" || !reflect.DeepEqual(c.names, []string{"arxiv", "doaj"}) || c.per != 20 || c.sortBy != "date" || c.fileType != "pdf" {
		t.Errorf("searcher call %+v", c)
	}

	// limit_per_source = max(5, limit // max(len(sources), 1)), capped at 100
	for _, k := range []struct {
		target string
		want   int
	}{
		{"?q=x", 20}, {"?q=x&limit=3", 5}, {"?q=x&sources=a,b,c&limit=30", 10}, {"?q=x&sources=a,b,c&limit=-9", 5}, {"?q=x&limit=100000", 100},
	} {
		fs.calls = nil
		scrapeGet(h.unified, "/api/v2/search/unified"+k.target)
		if len(fs.calls) != 1 || fs.calls[0].per != k.want {
			t.Errorf("%s: per-source %v, want %d", k.target, fs.calls, k.want)
		}
		if fs.calls[0].sortBy != "relevance" {
			t.Errorf("default sort %q", fs.calls[0].sortBy)
		}
	}

	rec = scrapeGet(h.unified, "/api/v2/search/unified?q=x&limit=abc")
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), `"loc":["query","limit"]`) {
		t.Errorf("bad limit: %d %s", rec.Code, rec.Body)
	}
}

func TestUnifiedRateLimit(t *testing.T) {
	fs := &fakeUnified{}
	h := &scrapeRoutes{d: Deps{Limiter: frozenLimiter()}, log: Deps{}.logger(), search: fs}
	for i := 0; i < 40; i++ { // 422s do not count
		scrapeGet(h.unified, "/api/v2/search/unified?limit=x")
	}
	var last int
	for i := 0; i < 61; i++ {
		last = scrapeGet(h.unified, "/api/v2/search/unified?q=go").Code
	}
	if last != http.StatusTooManyRequests {
		t.Errorf("61st request: %d, want 429", last)
	}
}

// scrapeRoutePatterns is every route of the scrape group (registered regardless of the flag).
func scrapeRoutePatterns(t *testing.T) map[string]bool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://coverage@127.0.0.1:1/coverage")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	r := httpx.NewRouter()
	newScrapeRoutes(Deps{Pool: pool, Identity: identity.NewStore(pool)}).register(r)
	out := map[string]bool{}
	for _, p := range r.Patterns() {
		out[p] = true
	}
	return out
}

func TestScrapeRoutesRegisterOnlyBehindTheFlag(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://coverage@127.0.0.1:1/coverage")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	patterns := func(env map[string]string) []string {
		r := httpx.NewRouter()
		RegisterScrapeRoutes(r, Deps{Pool: pool, Identity: identity.NewStore(pool), Getenv: func(k string) string { return env[k] }})
		return r.Patterns()
	}
	for _, env := range []map[string]string{nil, {"GO_SCRAPE_ROUTES_ENABLED": "false"}, {"GO_SCRAPE_ROUTES_ENABLED": "garbage"}} {
		if got := patterns(env); len(got) != 0 {
			t.Errorf("%v: registered %v with the flag off", env, got)
		}
	}
	got := patterns(map[string]string{"GO_SCRAPE_ROUTES_ENABLED": "true"})
	sort.Strings(got)
	want := []string{"GET /api/v2/scrapes", "GET /api/v2/search/unified", "POST /api/v2/scrape", "POST /api/v2/scraper/scrape"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("patterns %v, want %v", got, want)
	}
	// controller ruling D4: these two are dropped and stay unrouted
	for _, dropped := range []string{"GET /api/sources", "GET /api/trending"} {
		for _, p := range got {
			if p == dropped {
				t.Errorf("%s must not be ported", dropped)
			}
		}
	}
}
