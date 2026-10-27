//go:build integration

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/browserclient"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadweb"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ratelimit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/unisearch"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// history.json is recorded from the real routers/scraper.py functions over an in-memory SQLite session
// (legacy/python/scripts/record_scrape_history.py): the rows POST /api/v2/scrape writes and what
// GET /api/v2/scrapes lists.

type historyVectors struct {
	Steps []struct {
		Name       string           `json:"name"`
		RequestURL string           `json:"request_url"`
		Scrape     map[string]any   `json:"scrape"`
		Response   map[string]any   `json:"response"`
		History    []map[string]any `json:"history"`
		Links      []map[string]any `json:"links"`
		Emails     []map[string]any `json:"emails"`
	} `json:"steps"`
	Listing map[string][]map[string]any `json:"listing"`
}

func loadHistoryVectors(t *testing.T) historyVectors {
	t.Helper()
	b, err := os.ReadFile("testdata/scrape/history.json")
	if err != nil {
		t.Fatal(err)
	}
	var v historyVectors
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func resultFromScrapeMap(m map[string]any) scrapeResult {
	var emails []string
	for _, e := range m["extracted_emails"].([]any) {
		emails = append(emails, e.(string))
	}
	return scrapeResult{URL: m["url"].(string), Title: m["title"].(string), Description: m["description"].(string), Method: m["method"].(string),
		Emails: emails, WordCount: int(m["word_count"].(float64)), Preview: m["preview_text"].(string), HTML: m["html_content"].(string)}
}

// scrapeEnv is the jobs test env (users alice admin / bob member / erin editor, workspaces alpha and beta) with
// the scrape routes mounted over fakes.
func newScrapeEnv(t *testing.T, render pageRenderer, search unifiedSearcher) (jobsEnv, *scrapeRoutes) {
	t.Helper()
	e := newJobsEnv(t, nil)
	ctx := context.Background()
	clean := func() {
		_, _ = e.pool.Exec(ctx, `DELETE FROM email_data WHERE source_link_id IN (SELECT id FROM links WHERE source = 'Scraper V2')`)
		_, _ = e.pool.Exec(ctx, `DELETE FROM links WHERE source = 'Scraper V2'`)
		_, _ = e.pool.Exec(ctx, `DELETE FROM scrape_history`)
	}
	clean()
	t.Cleanup(clean)
	d := Deps{Pool: e.pool, Signer: testSigner, Identity: e.st, Limiter: ratelimit.New(time.Now)}
	h := newScrapeRoutes(d)
	h.render, h.search, h.guard = render, search, urlGuard{lookup: scrapePublicLookup}
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(e.pool, nil))
	h.register(r)
	e.h = r.Handler()
	return e, h
}

func TestScrapeHistoryStoreMatchesRecordedPython(t *testing.T) {
	v := loadHistoryVectors(t)
	e, h := newScrapeEnv(t, scrapeOKRender(), &fakeUnified{})
	ctx := context.Background()
	for _, s := range v.Steps {
		res := resultFromScrapeMap(s.Scrape)
		if err := h.store.Save(ctx, s.RequestURL, res); err != nil {
			t.Fatalf("%s: %v", s.Name, err)
		}
		type hist struct {
			URL, Title, Method    string
			WordCount, EmailCount int
		}
		var gotHist []map[string]any
		rows, err := e.pool.Query(ctx, `SELECT url, title, method, word_count, email_count FROM scrape_history ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var u, ti, m string
			var wc, ec int
			if err := rows.Scan(&u, &ti, &m, &wc, &ec); err != nil {
				t.Fatal(err)
			}
			gotHist = append(gotHist, map[string]any{"url": u, "title": ti, "method": m, "word_count": float64(wc), "email_count": float64(ec)})
		}
		rows.Close()
		if !reflect.DeepEqual(gotHist, s.History) {
			t.Errorf("%s: history\n got %v\nwant %v", s.Name, gotHist, s.History)
		}
		var gotLinks []map[string]any
		rows, _ = e.pool.Query(ctx, `SELECT url, status, source FROM links WHERE source = 'Scraper V2' ORDER BY id`)
		for rows.Next() {
			var u, st, src string
			_ = rows.Scan(&u, &st, &src)
			gotLinks = append(gotLinks, map[string]any{"url": u, "status": st, "source": src})
		}
		rows.Close()
		if !reflect.DeepEqual(gotLinks, s.Links) {
			t.Errorf("%s: links\n got %v\nwant %v", s.Name, gotLinks, s.Links)
		}
		var gotEmails []map[string]any
		rows, _ = e.pool.Query(ctx, `SELECT e.name, e.email, l.url FROM email_data e JOIN links l ON l.id = e.source_link_id WHERE l.source = 'Scraper V2' ORDER BY e.id`)
		linkIdx := map[string]float64{}
		for i, l := range s.Links {
			linkIdx[l["url"].(string)] = float64(i + 1)
		}
		for rows.Next() {
			var n, em, lu string
			_ = rows.Scan(&n, &em, &lu)
			gotEmails = append(gotEmails, map[string]any{"name": n, "email": em, "source_link_id": linkIdx[lu]})
		}
		rows.Close()
		if !reflect.DeepEqual(gotEmails, s.Emails) {
			t.Errorf("%s: emails\n got %v\nwant %v", s.Name, gotEmails, s.Emails)
		}
		// the response body is the ScrapeResponse model
		b, _ := json.Marshal(scrapeResponseJSON(res))
		var got map[string]any
		_ = json.Unmarshal(b, &got)
		if !reflect.DeepEqual(got, s.Response) {
			t.Errorf("%s: response\n got %v\nwant %v", s.Name, got, s.Response)
		}
	}
	// listing: newest first, skip/limit
	for key, want := range v.Listing {
		var skip, limit int
		_, _ = fmtSscan(key, &skip, &limit)
		rows, err := h.store.List(ctx, skip, limit)
		if err != nil {
			t.Fatal(err)
		}
		var got []map[string]any
		for _, r := range rows {
			b, _ := json.Marshal(r)
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			ca, _ := m["created_at"].(string)
			delete(m, "created_at")
			m["created_at_shape"] = float64(len(ca))
			got = append(got, m)
		}
		if len(want) == 0 {
			want = nil
		}
		if len(got) == 0 {
			got = nil
		}
		// ids differ between SQLite and Postgres sequences: compare everything but id, and the order by position
		strip := func(ms []map[string]any) []map[string]any {
			var out []map[string]any
			for _, m := range ms {
				c := map[string]any{}
				for k, v := range m {
					if k != "id" {
						c[k] = v
					}
				}
				out = append(out, c)
			}
			return out
		}
		if !reflect.DeepEqual(strip(got), strip(want)) {
			t.Errorf("listing %s:\n got %v\nwant %v", key, got, want)
		}
	}
}

func fmtSscan(key string, a, b *int) (int, error) {
	parts := strings.SplitN(key, "_", 2)
	var err error
	*a, err = atoi(parts[0])
	if err != nil {
		return 0, err
	}
	*b, err = atoi(parts[1])
	return 2, err
}

func atoi(s string) (int, error) {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n, nil
}

func TestScrapeRoutesAuthAndEndToEnd(t *testing.T) {
	render := scrapeOKRender()
	fs := &fakeUnified{out: unisearch.Unified{Query: "go", Total: 1, Results: []unisearch.Result{{ID: "a", Title: "T", Source: "arxiv", FileType: "pdf"}}}}
	e, _ := newScrapeEnv(t, render, fs)
	json := "application/json"
	body := `{"url":"https://example.com/start"}`

	type tc struct {
		name, method, path, who, ws, body string
		code                              int
	}
	for _, c := range []tc{
		{"scrape anonymous", "POST", "/api/v2/scrape", "", "", body, 401},
		{"scrape workspace admin only is not enough", "POST", "/api/v2/scrape", "bob", "ws-beta", body, 403},
		{"scrape global admin", "POST", "/api/v2/scrape", "alice", "", body, 200},
		{"scrapes anonymous", "GET", "/api/v2/scrapes", "", "", "", 401},
		{"scrapes member", "GET", "/api/v2/scrapes", "erin", "ws-alpha", "", 403},
		{"scrapes admin", "GET", "/api/v2/scrapes", "alice", "", "", 200},
		{"scraper/scrape anonymous", "POST", "/api/v2/scraper/scrape", "", "", body, 401},
		{"scraper/scrape viewer", "POST", "/api/v2/scraper/scrape", "bob", "ws-alpha", body, 403},
		{"scraper/scrape editor", "POST", "/api/v2/scraper/scrape", "erin", "ws-alpha", body, 200},
		{"scraper/scrape without workspace", "POST", "/api/v2/scraper/scrape", "erin", "", body, 200},
		{"unified anonymous", "GET", "/api/v2/search/unified?q=go", "", "", "", 401},
		{"unified viewer reads", "GET", "/api/v2/search/unified?q=go", "bob", "ws-alpha", "", 200},
	} {
		hdr := map[string]string{}
		if c.who != "" {
			hdr = bearer(c.who)
		}
		if c.ws != "" {
			hdr["X-Workspace-Id"] = c.ws
		}
		ct := ""
		if c.body != "" {
			ct = json
		}
		rec, _ := e.do(c.method, c.path, ct, c.body, hdr)
		if rec.Code != c.code {
			t.Errorf("%s: %d, want %d (%s)", c.name, rec.Code, c.code, rec.Body)
		}
	}

	// the admin scrape persisted exactly one history row, listed back newest first
	rec, _ := e.do("GET", "/api/v2/scrapes?limit=5", "", "", bearer("alice"))
	var rows []map[string]any
	if err := jsonUnmarshal(rec.Body.Bytes(), &rows); err != nil || len(rows) != 1 {
		t.Fatalf("scrapes: %v %s", err, rec.Body)
	}
	if rows[0]["url"] != "https://example.com/final" || rows[0]["method"] != "fast" || rows[0]["email_count"] != float64(1) {
		t.Errorf("history row %v", rows[0])
	}
	for _, k := range []string{"id", "url", "title", "method", "word_count", "email_count", "created_at"} {
		if _, ok := rows[0][k]; !ok {
			t.Errorf("history row lacks %s", k)
		}
	}
	// GET /api/sources and /api/trending are dropped (D4): the Go router does not know them
	for _, p := range []string{"/api/sources", "/api/trending"} {
		if rec, _ := e.do("GET", p, "", "", bearer("alice")); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", p, rec.Code)
		}
	}
}

func TestSystemStatsFromGoCounters(t *testing.T) {
	st := leadweb.SystemStats{}
	st.ProxyPool = leadweb.PoolStats{Total: 3, HTTP: 2, Socks5: 1, Blocked: 1, DomainAssignments: 4}
	st.RateLimiter = leadweb.LimiterStats{TrackedDomains: 2, TrippedDomains: []string{"slow.example"}}
	st.Browser = &browserclient.Stats{Version: "1", Active: 1, Capacity: 4}
	e := newStatsEnv(t, st)

	rec, _ := e.statsGet("alice", "")
	if rec.Code != 200 {
		t.Fatalf("status %d %s", rec.Code, rec.Body)
	}
	var got struct {
		ProxyPool   map[string]int `json:"proxy_pool"`
		RateLimiter struct {
			Tracked int      `json:"tracked_domains"`
			Tripped []string `json:"tripped_domains"`
		} `json:"rate_limiter"`
		Jobs    map[string]int `json:"jobs"`
		Browser map[string]any `json:"browser"`
	}
	if err := jsonUnmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ProxyPool["total"] != 3 || got.ProxyPool["domain_assignments"] != 4 || got.RateLimiter.Tracked != 2 || len(got.RateLimiter.Tripped) != 1 ||
		got.Browser["capacity"] != float64(4) || got.Jobs["total"] != 7 {
		t.Errorf("stats %s", rec.Body)
	}
	// key order: Python's proxy_pool, rate_limiter, jobs; the browser key is the one addition, last
	body := rec.Body.String()
	if !(strings.Index(body, `"proxy_pool"`) < strings.Index(body, `"rate_limiter"`) && strings.Index(body, `"rate_limiter"`) < strings.Index(body, `"jobs"`) &&
		strings.Index(body, `"jobs"`) < strings.Index(body, `"browser"`)) {
		t.Errorf("key order: %s", body)
	}
	// a non-admin member never sees tripped domains (process-global limiter state)
	rec, _ = e.statsGet("bob", "ws-beta")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"tripped_domains":[]`) {
		t.Errorf("non-admin: %d %s", rec.Code, rec.Body)
	}
	_ = wbcrud.NewObj
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
