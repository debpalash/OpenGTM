package personintel

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
)

func TestParseUsernameVectors(t *testing.T) {
	var v struct {
		Cases []struct {
			URL      string  `json:"url"`
			Username *string `json:"username"`
			Error    *string `json:"error"`
		} `json:"cases"`
		Gate []struct {
			URL      string `json:"url"`
			Accepted bool   `json:"accepted"`
		} `json:"linkedin_url_gate"`
	}
	loadVectors(t, "username.json", &v)
	for _, c := range v.Cases {
		got, err := ParseUsername(c.URL)
		if c.Error != nil {
			if err == nil || err.Error() != *c.Error {
				t.Errorf("%q: err=%v want %q", c.URL, err, *c.Error)
			}
			continue
		}
		if err != nil || got != *c.Username {
			t.Errorf("%q: got %q,%v want %q", c.URL, got, err, *c.Username)
		}
	}
	for _, g := range v.Gate {
		if ValidLinkedInURL(g.URL) != g.Accepted {
			t.Errorf("gate %q want %v", g.URL, g.Accepted)
		}
	}
}

func TestBuildQueriesVectors(t *testing.T) {
	var v struct {
		SQ []struct {
			Username string            `json:"username"`
			Queries  map[string]string `json:"queries"`
		} `json:"search_queries"`
	}
	loadVectors(t, "username.json", &v)
	order := []string{"linkedin", "general", "email", "twitter", "github", "blog", "articles"}
	for _, c := range v.SQ {
		q := BuildQueries(c.Username)
		if !eqStrings(q.Keys(), order) {
			t.Fatalf("key order %v", q.Keys())
		}
		for k, want := range c.Queries {
			if got, _ := q.Get(k); got != want {
				t.Errorf("%s: %q want %q", k, got, want)
			}
		}
	}
}

type ddgCase struct {
	Name       string   `json:"name"`
	HTML       string   `json:"html"`
	Status     int      `json:"status"`
	MaxResults int      `json:"max_results"`
	Results    []Result `json:"results"`
}

func withDDG(t *testing.T, h http.HandlerFunc) (*http.Client, func()) {
	t.Helper()
	srv := httptest.NewServer(h)
	old := ddgEndpoint
	ddgEndpoint = srv.URL + "/lite/"
	return stubClient(t, srv), func() { ddgEndpoint = old; srv.Close() }
}

func TestDDGLiteParseVectors(t *testing.T) {
	var v struct{ Cases []ddgCase }
	loadVectors(t, "ddg_lite.json", &v)
	for _, c := range v.Cases {
		t.Run(c.Name, func(t *testing.T) {
			var gotBody, gotCT string
			cl, done := withDDG(t, func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				gotBody, gotCT = string(b), r.Header.Get("Content-Type")
				if r.Method != http.MethodPost {
					t.Errorf("method %s", r.Method)
				}
				w.WriteHeader(c.Status)
				io.WriteString(w, c.HTML)
			})
			defer done()
			got, err := SearchDDGLite(context.Background(), cl, "\"jane\" site:x", c.MaxResults)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) == 0 && len(c.Results) == 0 {
				// ok: nil or empty
			} else if !reflect.DeepEqual(got, c.Results) {
				t.Errorf("got %+v\nwant %+v", got, c.Results)
			}
			form, _ := url.ParseQuery(gotBody)
			if form.Get("q") != "\"jane\" site:x" || form.Get("kl") != "wt-wt" || !strings.HasPrefix(gotCT, "application/x-www-form-urlencoded") {
				t.Errorf("form %q ct %q", gotBody, gotCT)
			}
		})
	}
}

func TestDDGLiteMalformedAndRedirectLinks(t *testing.T) {
	// Go-only: real DDG hrefs wrap targets as //duckduckgo.com/l/?uddg=<url>;
	// they are unwrapped (the Python code would drop them as duckduckgo.com).
	html := `<table><tr><td><a class="result-url" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.org%2Fpost%3Fa%3D1&amp;rut=abc">Wrapped</a></td></tr>
<tr><td class="result-snippet">snip</td></tr>
<tr><td><a class="result-url" href="//duckduckgo.com/l/?uddg=javascript%3Aalert(1)">Bad</a></td></tr>
<tr><td><a class="result-url" href="https://ads.duckduckgo.com/y.js?ad=1">Ad</a></td></tr>
<tr><td><a class="result-url" href="https://example.net/ok">Unclosed <b>tag
<tr><td class="result-snippet">after <i>malformed`
	cl, done := withDDG(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, html) })
	defer done()
	got, err := SearchDDGLite(context.Background(), cl, "q", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < 2 || got[0].URL != "https://example.org/post?a=1" || got[0].Title != "Wrapped" || got[0].Snippet != "snip" {
		t.Fatalf("got %+v", got)
	}
	for _, r := range got {
		if strings.HasPrefix(r.URL, "javascript:") || strings.Contains(r.URL, "duckduckgo.com") {
			t.Errorf("leaked %q", r.URL)
		}
	}
	if got[len(got)-1].URL != "https://example.net/ok" {
		t.Errorf("malformed tail: %+v", got)
	}
}

type extractVec struct {
	LinkedIn []struct {
		Name    string          `json:"name"`
		Start   json.RawMessage `json:"start"`
		Results []Result        `json:"results"`
		Profile json.RawMessage `json:"profile"`
	} `json:"linkedin"`
	Social []struct {
		Name  string              `json:"name"`
		Start json.RawMessage     `json:"start"`
		In    map[string][]Result `json:"social_results"`
		Links *Obj                `json:"social_links"`
	} `json:"social"`
	AM []struct {
		Name     string   `json:"name"`
		Username string   `json:"username"`
		Articles []Result `json:"articles_in"`
		General  []Result `json:"general_in"`
		OutA     []Entry  `json:"articles"`
		OutM     []Entry  `json:"mentions"`
	} `json:"articles_mentions"`
	Emails []struct {
		Name       string   `json:"name"`
		Start      []string `json:"start_emails"`
		Results    []Result `json:"results"`
		Sorted     []string `json:"emails_sorted"`
		PrefixKept bool     `json:"start_prefix_kept"`
	} `json:"emails"`
}

func startProfile(t *testing.T, raw json.RawMessage) *Profile {
	p := NewProfile("")
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, p); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func TestExtractVectors(t *testing.T) {
	var v extractVec
	loadVectors(t, "extract.json", &v)
	for _, c := range v.LinkedIn {
		p := startProfile(t, c.Start)
		ExtractLinkedIn(p, c.Results)
		if !reflect.DeepEqual(profileJSON(t, p), normalize(t, c.Profile)) {
			t.Errorf("linkedin/%s: got %v want %s", c.Name, profileJSON(t, p), c.Profile)
		}
	}
	for _, c := range v.Social {
		p := startProfile(t, c.Start)
		ExtractSocial(p, c.In)
		g, _ := json.Marshal(p.SocialLinks)
		w, _ := json.Marshal(c.Links)
		if string(g) != string(w) {
			t.Errorf("social/%s: got %s want %s", c.Name, g, w)
		}
	}
	for _, c := range v.AM {
		p := NewProfile("")
		p.Username = c.Username
		ExtractArticlesAndMentions(p, c.Articles, c.General)
		g, _ := json.Marshal([]any{nz(p.Articles), nz(p.Mentions)})
		w, _ := json.Marshal([]any{nz(c.OutA), nz(c.OutM)})
		if string(g) != string(w) {
			t.Errorf("am/%s:\n got %s\nwant %s", c.Name, g, w)
		}
	}
	for _, c := range v.Emails {
		p := NewProfile("")
		p.Emails = append([]string{}, c.Start...)
		ExtractEmails(p, c.Results)
		if !eqStrings(sorted(p.Emails), sorted(c.Sorted)) {
			t.Errorf("emails/%s: %v want %v", c.Name, p.Emails, c.Sorted)
		}
		if c.PrefixKept && !eqStrings(p.Emails[:len(c.Start)], c.Start) {
			t.Errorf("emails/%s prefix not kept: %v", c.Name, p.Emails)
		}
	}
}

func nz(e []Entry) []Entry {
	if e == nil {
		return []Entry{}
	}
	return e
}

func TestExtractEmailsDedupeAndJunk(t *testing.T) {
	p := NewProfile("")
	ExtractEmails(p, []Result{
		{Snippet: "a@acme.io a@acme.io A@acme.io", ScrapedText: "a@acme.io b@acme.io"},
		{Snippet: "x@example.com y@SENTRY.io z@wixpress.com me@yourname.dev"},
	})
	want := []string{"a@acme.io", "A@acme.io", "b@acme.io"}
	if !eqStrings(sorted(p.Emails), sorted(want)) {
		t.Fatalf("got %v", p.Emails)
	}
	ExtractEmails(p, nil)
	if len(p.Emails) != 3 {
		t.Fatal("idempotent")
	}
}

func TestProfileJSONKeyOrder(t *testing.T) {
	var v struct {
		Keys     []string       `json:"person_profile_keys"`
		Defaults map[string]any `json:"person_profile_defaults"`
	}
	loadVectors(t, "response_keys.json", &v)
	b, err := json.Marshal(NewProfile(""))
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.Token()
	var keys []string
	for dec.More() {
		k, _ := dec.Token()
		keys = append(keys, k.(string))
		var skip json.RawMessage
		dec.Decode(&skip)
	}
	if !eqStrings(keys, v.Keys) {
		t.Fatalf("keys %v", keys)
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	if !reflect.DeepEqual(m, v.Defaults) {
		t.Fatalf("defaults %v", m)
	}
	// a Profile decoded from null-ish fields still marshals [] / {}
	var p Profile
	b, _ = json.Marshal(&p)
	if !strings.Contains(string(b), `"emails":[]`) || !strings.Contains(string(b), `"social_links":{}`) {
		t.Fatalf("nil normalisation: %s", b)
	}
}

type ghCase struct {
	Name    string          `json:"name"`
	Social  *Obj            `json:"social_links"`
	User    json.RawMessage `json:"user_payload"`
	Repos   json.RawMessage `json:"repos_payload"`
	Options struct {
		Start       json.RawMessage `json:"start"`
		UserStatus  int             `json:"user_status"`
		ReposStatus int             `json:"repos_status"`
		RaiseOn     string          `json:"raise_on"`
	} `json:"options"`
	Calls []struct {
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
	} `json:"calls"`
	Profile json.RawMessage `json:"profile"`
}

func TestGitHubEnrichVectors(t *testing.T) {
	var v struct{ Cases []ghCase }
	loadVectors(t, "github.json", &v)
	for _, c := range v.Cases {
		t.Run(c.Name, func(t *testing.T) {
			var mu sync.Mutex
			var calls []string
			var hdrs []http.Header
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				// net/http transparently retries an idempotent GET whose
				// connection was cut; record each request once.
				if n := len(calls); n == 0 || calls[n-1] != r.URL.RequestURI() {
					calls = append(calls, r.URL.RequestURI())
					hdrs = append(hdrs, r.Header.Clone())
				}
				mu.Unlock()
				isRepos := strings.HasSuffix(r.URL.Path, "/repos")
				if c.Options.RaiseOn != "" && strings.Contains(r.URL.Path, c.Options.RaiseOn) {
					hj, _ := w.(http.Hijacker)
					conn, _, _ := hj.Hijack()
					conn.Close()
					return
				}
				status, body := c.Options.UserStatus, c.User
				if isRepos {
					status, body = c.Options.ReposStatus, c.Repos
				}
				if status == 0 {
					status = 200
				}
				w.WriteHeader(status)
				w.Write(body)
			}))
			defer srv.Close()
			old := githubAPI
			githubAPI = srv.URL
			defer func() { githubAPI = old }()

			p := startProfile(t, c.Options.Start)
			p.SocialLinks = c.Social
			if p.SocialLinks == nil {
				p.SocialLinks = &Obj{}
			}
			if err := EnrichFromGitHub(context.Background(), stubClient(t, srv), p); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(profileJSON(t, p), normalize(t, c.Profile)) {
				t.Errorf("got %v\nwant %s", profileJSON(t, p), c.Profile)
			}
			srv.Close() // drain in-flight handlers before reading calls
			mu.Lock()
			defer mu.Unlock()
			if len(calls) != len(c.Calls) {
				t.Fatalf("calls %v want %d", calls, len(c.Calls))
			}
			for i, want := range c.Calls {
				wu, _ := url.Parse(want.URL)
				if calls[i] != wu.RequestURI() {
					t.Errorf("call %d %q want %q", i, calls[i], wu.RequestURI())
				}
				for k, hv := range want.Headers {
					if hdrs[i].Get(k) != hv {
						t.Errorf("call %d header %s=%q want %q", i, k, hdrs[i].Get(k), hv)
					}
				}
			}
		})
	}
}

func TestFetchRefusesPrivateHost(t *testing.T) {
	var hit atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit.Add(1); io.WriteString(w, "<html>") }))
	defer srv.Close()
	// default egress client, no allowlist: loopback must be refused
	c := egress.NewClientForEnv("production", nil)
	oldD, oldG := ddgEndpoint, githubAPI
	ddgEndpoint, githubAPI = srv.URL+"/lite/", srv.URL
	defer func() { ddgEndpoint, githubAPI = oldD, oldG }()
	if _, err := SearchDDGLite(context.Background(), c, "q", 5); err == nil {
		t.Error("ddg: want error for private host")
	}
	p := NewProfile("")
	p.SocialLinks.Set("github", "https://github.com/jane")
	_ = EnrichFromGitHub(context.Background(), c, p)
	if hit.Load() != 0 {
		t.Fatalf("private host was contacted %d times", hit.Load())
	}
	if len(p.Emails)+len(p.Skills) != 0 {
		t.Error("profile mutated")
	}
}

func TestRedirectToPrivateNotFollowed(t *testing.T) {
	var hit atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit.Add(1) }))
	defer target.Close()
	cl, done := withDDG(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	})
	defer done()
	got, err := SearchDDGLite(context.Background(), cl, "q", 5)
	if err != nil || len(got) != 0 || hit.Load() != 0 {
		t.Fatalf("got %v %v hits=%d", got, err, hit.Load())
	}
}

func TestBodyCap(t *testing.T) {
	big := strings.Repeat("a", (1<<20)+10)
	cl, done := withDDG(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, big) })
	defer done()
	if _, err := SearchDDGLite(context.Background(), cl, "q", 5); err == nil {
		t.Fatal("want body cap error")
	}
	// github: oversized body is ignored, profile untouched, no error leak of body
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, big) }))
	defer srv.Close()
	old := githubAPI
	githubAPI = srv.URL
	defer func() { githubAPI = old }()
	p := NewProfile("")
	p.SocialLinks.Set("github", "https://github.com/jane")
	_ = EnrichFromGitHub(context.Background(), stubClient(t, srv), p)
	if p.Summary != "" || len(p.Skills) != 0 {
		t.Fatalf("mutated: %+v", p)
	}
}

func TestRunSearchesBoundedParallelism(t *testing.T) {
	var cur, peak atomic.Int32
	cl, done := withDDG(t, func(w http.ResponseWriter, r *http.Request) {
		n := cur.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		cur.Add(-1)
		io.WriteString(w, `<a class="result-url" href="https://example.org/`+r.FormValue("q")+`">t</a>`)
	})
	defer done()
	q := &Obj{}
	for i := 0; i < 20; i++ {
		q.Set(string(rune('a'+i)), string(rune('a'+i)))
	}
	res := RunSearches(context.Background(), cl, q, 20, nil)
	if len(res) != 20 || res[0].Category != "a" || res[19].Category != "t" || len(res[3].Results) != 1 {
		t.Fatalf("res order/shape: %d", len(res))
	}
	if peak.Load() > 8 || peak.Load() < 2 {
		t.Fatalf("peak parallelism %d", peak.Load())
	}
}

func TestRunSearchesCancel(t *testing.T) {
	cl, done := withDDG(t, func(w http.ResponseWriter, r *http.Request) { time.Sleep(2 * time.Second) })
	defer done()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	res := RunSearches(ctx, cl, BuildQueries("jane"), 20, nil)
	if time.Since(start) > time.Second || len(res) != 7 {
		t.Fatalf("not cancelled: %v %d", time.Since(start), len(res))
	}
}
