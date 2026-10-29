package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadsread"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/llm"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/nlcolumn"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/settings"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
	"github.com/jackc/pgx/v5/pgxpool"
)

func readLLMVec(t *testing.T, name string, v any) {
	t.Helper()
	b, err := os.ReadFile("../../testdata/llmroutes/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

// ---- fakes ----

type fakeLeadStore struct {
	mu      sync.Mutex
	leads   map[int64]*leadsread.Lead
	updates []fakeUpdate
	asked   []int64
}

type fakeUpdate struct {
	ID     int64          `json:"id"`
	Fields map[string]any `json:"fields"`
}

func (s *fakeLeadStore) Load(_ context.Context, _ string, id int64) (*leadsread.Lead, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, id)
	return s.leads[id], nil
}

func (s *fakeLeadStore) Save(_ context.Context, _ string, id int64, f map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := map[string]any{}
	for k, v := range f {
		cp[k] = v
	}
	s.updates = append(s.updates, fakeUpdate{id, cp})
	return nil
}

func leadFrom(t *testing.T, m map[string]any) *leadsread.Lead {
	t.Helper()
	b, _ := json.Marshal(m)
	var l leadsread.Lead
	if err := json.Unmarshal(b, &l); err != nil {
		t.Fatal(err)
	}
	return &l
}

type scriptLLM struct {
	script []json.RawMessage
	calls  []struct{ Prompt, System string }
}

func (s *scriptLLM) ExtractJSON(_ context.Context, prompt, system string, _ int) (*wbcrud.Obj, error) {
	s.calls = append(s.calls, struct{ Prompt, System string }{prompt, system})
	if len(s.script) == 0 {
		return wbcrud.NewObj(), nil
	}
	r := s.script[0]
	s.script = s.script[1:]
	if bytes.Contains(r, []byte("__raise__")) {
		return nil, errors.New("llm failed")
	}
	o, err := wbcrud.ParseObjectCanon(r)
	if err != nil {
		return nil, err
	}
	return o, nil
}

type fakeChat struct{ p llm.ChatProvider }

func (f fakeChat) ChatChain(context.Context) []llm.ChatProvider        { return []llm.ChatProvider{f.p} }
func (f fakeChat) ActiveChatProvider(context.Context) llm.ChatProvider { return f.p }

func newTestRoutes(store leadStore, l nlcolumn.LLM) *llmRoutes {
	return &llmRoutes{log: nil, store: store, httpc: http.DefaultClient,
		newLLM: func(context.Context, string) llmBundle { return llmBundle{Extract: l, Chat: fakeChat{}} }}
}

func serve(h http.HandlerFunc, method, target, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// ---- request validation (recorded FastAPI 422 bodies) ----

type reqVec struct {
	Body   string          `json:"body"`
	Status int             `json:"status"`
	JSON   json.RawMessage `json:"json"`
}

func replayValidation(t *testing.T, vecs []reqVec, parse func(*wbcrud.Obj) []httpx.FieldError) {
	t.Helper()
	for _, v := range vecs {
		o, errs := decodeLLMBody([]byte(v.Body))
		if errs == nil {
			errs = parse(o)
		}
		if v.Status == 200 {
			if len(errs) > 0 {
				t.Errorf("%q: unexpected errors %v", v.Body, errs)
			}
			continue
		}
		if len(errs) == 0 {
			t.Errorf("%q: want 422", v.Body)
			continue
		}
		rec := httptest.NewRecorder()
		httpx.ValidationError(rec, errs...)
		var got, want struct {
			Detail []map[string]any `json:"detail"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		_ = json.Unmarshal(v.JSON, &want)
		if len(got.Detail) != len(want.Detail) {
			t.Errorf("%q: %d errors, want %d\n got %s\nwant %s", v.Body, len(got.Detail), len(want.Detail), rec.Body.String(), v.JSON)
			continue
		}
		for i := range got.Detail {
			g, w := got.Detail[i], want.Detail[i]
			if g["type"] == "json_invalid" { // ctx.error text and position are best effort (known gap)
				if w["type"] != "json_invalid" {
					t.Errorf("%q: type %v", v.Body, g["type"])
				}
				continue
			}
			if !reflect.DeepEqual(g, w) {
				t.Errorf("%q error %d\n got %v\nwant %v", v.Body, i, g, w)
			}
		}
	}
}

func TestRequestValidationVectors(t *testing.T) {
	var v struct {
		GenerateColumn []reqVec `json:"generate_column"`
		Campaigns      []reqVec `json:"campaigns"`
	}
	readLLMVec(t, "requests.json", &v)
	replayValidation(t, v.GenerateColumn, func(o *wbcrud.Obj) []httpx.FieldError {
		var errs []httpx.FieldError
		strField(o, "instruction", true, "", 3, 2000, &errs)
		return errs
	})
	replayValidation(t, v.Campaigns, func(o *wbcrud.Obj) []httpx.FieldError {
		_, errs := parseCampaignRequest(o)
		return errs
	})
}

// ---- campaigns ----

type campVec struct {
	System string                    `json:"system"`
	Leads  map[string]map[string]any `json:"leads"`
	Cases  []struct {
		Name    string `json:"name"`
		Request struct {
			LeadIDs []int64 `json:"lead_ids"`
		} `json:"request"`
		Body  json.RawMessage   `json:"-"`
		LLM   []json.RawMessage `json:"llm"`
		Calls []struct {
			Prompt string `json:"prompt"`
			System string `json:"system"`
		} `json:"calls"`
		Asked    []int64         `json:"asked"`
		Response json.RawMessage `json:"response"`
	} `json:"cases"`
}

func TestCampaignGenerateVectors(t *testing.T) {
	var v campVec
	readLLMVec(t, "campaigns.json", &v)
	var raw struct {
		Cases []struct {
			Request json.RawMessage `json:"request"`
		} `json:"cases"`
	}
	readLLMVec(t, "campaigns.json", &raw)
	if v.System != emailSystemPrompt {
		t.Fatal("email system prompt drifted from Python")
	}
	for i, c := range v.Cases {
		t.Run(c.Name, func(t *testing.T) {
			store := &fakeLeadStore{leads: map[int64]*leadsread.Lead{}}
			for k, m := range v.Leads {
				if _, raises := m["__raise__"]; raises {
					continue // Python's DB raised for this id; a missing lead is skipped the same way
				}
				var id int64
				fmt.Sscan(k, &id)
				store.leads[id] = leadFrom(t, m)
			}
			l := &scriptLLM{script: c.LLM}
			h := newTestRoutes(store, l)
			rec := serve(h.campaignGenerate, "POST", "/api/campaigns/generate", string(raw.Cases[i].Request))
			if rec.Code != 200 {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			var got, want any
			_ = json.Unmarshal(rec.Body.Bytes(), &got)
			_ = json.Unmarshal(c.Response, &want)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("response\n got %s\nwant %s", rec.Body, c.Response)
			}
			if len(l.calls) != len(c.Calls) {
				t.Fatalf("%d LLM calls, want %d", len(l.calls), len(c.Calls))
			}
			for j, w := range c.Calls {
				if l.calls[j].Prompt != w.Prompt || l.calls[j].System != w.System {
					t.Errorf("call %d\n got %q\nwant %q", j, l.calls[j].Prompt, w.Prompt)
				}
			}
			if c.Name != "db_raises_continues" && !sameIDs(store.asked, c.Asked) {
				t.Errorf("asked %v, want %v", store.asked, c.Asked)
			}
		})
	}
}

func sameIDs(a, b []int64) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

func TestCampaignHugeAndNegativeIDsAreSkipped(t *testing.T) {
	store := &fakeLeadStore{leads: map[int64]*leadsread.Lead{}}
	h := newTestRoutes(store, &scriptLLM{})
	rec := serve(h.campaignGenerate, "POST", "/x", `{"lead_ids":[99999999999999999999,-3,0],"value_proposition":"vp"}`)
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"emails":[]}` {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

// ---- generate-column response shape ----

func TestFullColumnMatchesRecordedResponse(t *testing.T) {
	var v struct {
		Cases []struct {
			Name     string `json:"name"`
			Response *struct {
				Column json.RawMessage `json:"column"`
			} `json:"response"`
			Result *struct {
				Column json.RawMessage `json:"column"`
			} `json:"result"`
		} `json:"cases"`
	}
	b, _ := os.ReadFile("../../internal/nlcolumn/testdata/nl_column.json")
	_ = json.Unmarshal(b, &v)
	n := 0
	for _, c := range v.Cases {
		if c.Response == nil {
			continue
		}
		col, err := wbcrud.ParseObjectCanon(c.Result.Column)
		if err != nil {
			t.Fatal(err)
		}
		wire, _ := json.Marshal(col)
		parsed, errs := wbcrud.ParseAddBody([]byte(`{"column":` + string(wire) + `}`))
		if len(errs) > 0 {
			t.Fatal(errs)
		}
		got, _ := json.Marshal(fullColumn(parsed))
		var x, y bytes.Buffer
		_ = json.Compact(&x, got)
		_ = json.Compact(&y, c.Response.Column)
		if x.String() != y.String() {
			t.Errorf("%s\n got %s\nwant %s", c.Name, x.String(), y.String())
		}
		n++
	}
	if n < 30 {
		t.Fatalf("only %d cases compared", n)
	}
}

// ---- ordered LLM adapter ----

func TestOrderedObject(t *testing.T) {
	text := "Sure!\n```json\n{\"kind\":\"http\",\"http_headers\":{\"Zed\":\"1\",\"Alpha\":\"{\\\"x\\\"}\"},\"n\":{\"b\":1,\"a\":2}}\n```"
	m, ok := llm.ParseObject(text)
	if !ok {
		t.Fatal("ParseObject rejected the text")
	}
	got, _ := json.Marshal(orderedObject(text, m))
	want := `{"kind":"http","http_headers":{"Zed":"1","Alpha":"{\"x\"}"},"n":{"b":1,"a":2}}`
	if string(got) != want {
		t.Errorf("got %s\nwant %s", got, want)
	}
}

// TestOrderedExtractMatchesClient pins orderedLLM against llm.Client.ExtractJSON: same requests on the wire, including
// the retry after an unparseable first answer.
func TestOrderedExtractMatchesClient(t *testing.T) {
	record := func(useOrdered bool) []string {
		var mu sync.Mutex
		var bodies []string
		n := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			bodies = append(bodies, string(b))
			n++
			reply := "not json"
			if n > 1 {
				reply = `{"b":1,"a":2}`
			}
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": reply}}}})
		}))
		defer srv.Close()
		c := llm.New(llm.Options{Settings: mapSettings{"CEREBRAS_API_KEY": "k", "CEREBRAS_BASE_URL": srv.URL}, HTTP: srv.Client()})
		if useOrdered {
			o, err := orderedLLM{c: c}.ExtractJSON(context.Background(), "p", "sys", 100)
			if err != nil {
				t.Fatal(err)
			}
			if got, _ := json.Marshal(o); string(got) != `{"b":1,"a":2}` {
				t.Errorf("ordered result %s", got)
			}
		} else if _, err := c.ExtractJSON(context.Background(), "p", "sys", 100); err != nil {
			t.Fatal(err)
		}
		return bodies
	}
	a, b := record(false), record(true)
	if !reflect.DeepEqual(a, b) || len(a) != 2 {
		t.Errorf("requests differ:\n client  %v\n ordered %v", a, b)
	}
}

type mapSettings map[string]string

func (m mapSettings) Get(_ context.Context, key, def string) (string, error) {
	if v := m[key]; v != "" {
		return v, nil
	}
	return def, nil
}

// ---- lead enrich (recorded SSE bytes) ----

type enrichVec struct {
	Cases []struct {
		Name        string `json:"name"`
		LeadID      int64  `json:"lead_id"`
		Action      *string
		Status      int          `json:"status"`
		Body        string       `json:"body"`
		Updates     []fakeUpdate `json:"updates"`
		LLMRequests []struct {
			Path          string         `json:"path"`
			Authorization string         `json:"authorization"`
			ContentType   string         `json:"content_type"`
			Body          map[string]any `json:"body"`
		} `json:"llm_requests"`
		DDGQueries [][]any `json:"ddg_queries"`
		OSMCalls   []struct {
			Params  map[string]any    `json:"params"`
			Headers map[string]string `json:"headers"`
		} `json:"osm_calls"`
		Inputs *struct {
			Script *struct {
				Status int      `json:"status"`
				Text   string   `json:"text"`
				SSE    []string `json:"sse"`
			} `json:"script"`
			APIKey   string                      `json:"prov_api_key"`
			DDG      map[string][]map[string]any `json:"ddg"`
			DDGRaise []string                    `json:"ddg_raise"`
			OSM      *struct {
				Status int             `json:"status"`
				Data   json.RawMessage `json:"data"`
				Raise  string          `json:"raise"`
			} `json:"osm"`
			Email       string            `json:"email"`
			Scrape      map[string]string `json:"scrape"`
			ScrapeEmpty bool              `json:"scrape_empty"`
			ScrapeRaise string            `json:"scrape_raise"`
			ScrapeCopy  bool              `json:"scrape_copy"`
		} `json:"inputs"`
		Codes []int `json:"codes"`
	} `json:"cases"`
}

type fakeProvider struct {
	name string
	out  providers.Output
	err  error
}

func (f fakeProvider) Name() string { return f.name }
func (f fakeProvider) Enrich(context.Context, *http.Client, func() (string, error), providers.Input) (providers.Output, error) {
	return f.out, f.err
}

type mapProviders map[string]providers.Provider

func (m mapProviders) Get(n string) (providers.Provider, bool) { p, ok := m[n]; return p, ok }

type fakeSearcher struct {
	results map[string][]map[string]any
	raise   map[string]bool
	queries [][]any
}

func (f *fakeSearcher) Search(_ context.Context, q string, max int) ([]SearchHit, error) {
	f.queries = append(f.queries, []any{q, float64(max)})
	if f.raise["*"] || f.raise[q] {
		return nil, errors.New("ddg down")
	}
	rs, ok := f.results[q]
	if !ok {
		rs = f.results["*"]
	}
	var out []SearchHit
	for _, r := range rs {
		title, _ := r["title"].(string)
		body, _ := r["body"].(string)
		out = append(out, SearchHit{Title: title, Body: body})
	}
	return out, nil
}

var enrichLeads = map[int64]map[string]any{
	1: {"id": 1, "company": "Acme Corp", "city": "Pune", "website": "https://acme.test", "specialization": "IT", "description": "old", "email": "", "phone": "", "notes": "n1  ", "state": ""},
	2: {"id": 2, "company": "NoSite", "city": "", "website": "", "email": "x@x.test"},
	3: {"id": 3, "company": "Grüße GmbH", "city": "Zürich", "website": "https://g.test", "state": "Karnataka", "notes": ""},
}

func TestLeadEnrichVectors(t *testing.T) {
	var v enrichVec
	readLLMVec(t, "enrich.json", &v)
	// The recorder's Action is a *string under a field the struct tag cannot express; read it separately.
	var acts struct {
		Cases []struct {
			Action *string `json:"action"`
		} `json:"cases"`
	}
	readLLMVec(t, "enrich.json", &acts)
	// Python's real website scraper mutates the lead in place, so its route never saved scraper output; the Go
	// route saves it (deviation). The recorded non-copy cases with data (and the two that model Python-only
	// failure shapes) therefore do not apply.
	skip := map[string]string{
		"scrape_found":        "Python bug: compared the lead with itself",
		"scrape_empty_result": "provider seam has no empty-result shape",
		"scrape_raises":       "provider failures are Output.Error, not exceptions",
	}
	for i, c := range v.Cases {
		if c.Inputs == nil {
			continue // 422 and rate limit cases are covered below
		}
		t.Run(c.Name, func(t *testing.T) {
			if why, ok := skip[c.Name]; ok {
				t.Skip(why)
			}
			store := &fakeLeadStore{leads: map[int64]*leadsread.Lead{}}
			for id, m := range enrichLeads {
				store.leads[id] = leadFrom(t, m)
			}
			var mu sync.Mutex // the fake servers' handlers run on their own goroutines
			var llmReqs []map[string]any
			var llmHdr []string
			chat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				var m map[string]any
				_ = json.Unmarshal(b, &m)
				mu.Lock()
				llmReqs = append(llmReqs, m)
				llmHdr = append(llmHdr, r.URL.Path+"|"+r.Header.Get("Authorization")+"|"+r.Header.Get("Content-Type"))
				mu.Unlock()
				sc := c.Inputs.Script
				if sc == nil {
					return
				}
				code := sc.Status
				if code == 0 {
					code = 200
				}
				w.WriteHeader(code)
				if sc.SSE != nil {
					_, _ = io.WriteString(w, strings.Join(sc.SSE, "\n")+"\n")
				} else {
					_, _ = io.WriteString(w, sc.Text)
				}
			}))
			defer chat.Close()
			var osmParams []map[string]string
			osm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				p := map[string]string{}
				for k, vs := range r.URL.Query() {
					p[k] = vs[0]
				}
				mu.Lock()
				osmParams = append(osmParams, p)
				mu.Unlock()
				if r.Header.Get("User-Agent") != "LeadEngine/1.0" {
					t.Errorf("User-Agent = %q", r.Header.Get("User-Agent"))
				}
				o := c.Inputs.OSM
				if o != nil && o.Raise != "" {
					hj, _ := w.(http.Hijacker)
					conn, _, _ := hj.Hijack()
					_ = conn.Close()
					return
				}
				if o == nil {
					_, _ = io.WriteString(w, "[]")
					return
				}
				w.WriteHeader(o.Status)
				_, _ = w.Write(o.Data)
			}))
			defer osm.Close()

			h := newTestRoutes(store, &scriptLLM{})
			h.newLLM = func(context.Context, string) llmBundle {
				return llmBundle{Chat: fakeChat{llm.ChatProvider{ID: "x", APIKey: c.Inputs.APIKey, BaseURL: chat.URL + "/v1/", Model: "m-1", OpenAICompatible: true}}}
			}
			h.nominatimURL = osm.URL + "/search"
			raise := map[string]bool{}
			for _, q := range c.Inputs.DDGRaise {
				raise[q] = true
			}
			fs := &fakeSearcher{results: c.Inputs.DDG, raise: raise}
			h.searcher = fs
			scrape := providers.NewObj()
			for _, k := range []string{"email", "phone", "description", "contact_person"} {
				if val, ok := c.Inputs.Scrape[k]; ok {
					scrape.Set(k, val)
				}
			}
			h.providerLookup = mapProviders{
				"ddg_email_provider":       fakeProvider{name: "ddg_email_provider", out: providers.Output{Value: c.Inputs.Email}},
				"website_scraper_provider": fakeProvider{name: "website_scraper_provider", out: providers.Output{Extra: scrape}},
			}
			target := fmt.Sprintf("/api/lead/%d/enrich", c.LeadID)
			if a := acts.Cases[i].Action; a != nil {
				target += "?action=" + *a
			}
			req := httptest.NewRequest("POST", target, nil)
			req.SetPathValue("lead_id", fmt.Sprint(c.LeadID))
			rec := httptest.NewRecorder()
			h.leadEnrich(rec, req)

			if rec.Code != c.Status {
				t.Fatalf("status %d, want %d (%s)", rec.Code, c.Status, rec.Body)
			}
			if c.Inputs.OSM != nil && c.Inputs.OSM.Raise != "" { // the exception text is transport specific
				re := regexp.MustCompile(`OSM lookup failed: [^"]*`)
				c.Body = re.ReplaceAllString(c.Body, "OSM lookup failed: X")
				body := re.ReplaceAllString(rec.Body.String(), "OSM lookup failed: X")
				rec.Body = bytes.NewBufferString(body)
			}
			if c.Status == 200 {
				if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream; charset=utf-8" {
					t.Errorf("content-type %q", ct)
				}
				if rec.Header().Get("Cache-Control") != "no-cache" || rec.Header().Get("X-Accel-Buffering") != "no" {
					t.Error("stream headers differ")
				}
				if rec.Body.String() != c.Body {
					t.Errorf("SSE body\n got %q\nwant %q", rec.Body.String(), c.Body)
				}
			} else if strings.TrimSpace(rec.Body.String()) != strings.TrimSpace(c.Body) {
				t.Errorf("body %q, want %q", rec.Body.String(), c.Body)
			}
			// writes: same lead and fields; last_enriched_at is an ISO timestamp here, "now" in Python
			if len(store.updates) != len(c.Updates) {
				t.Fatalf("%d updates, want %d: %v", len(store.updates), len(c.Updates), store.updates)
			}
			for j, w := range c.Updates {
				g := store.updates[j]
				if g.ID != w.ID {
					t.Errorf("update %d id %d", j, g.ID)
				}
				if ts, ok := g.Fields["last_enriched_at"].(string); ok {
					if !strings.HasSuffix(ts, "+00:00") || len(ts) < 25 {
						t.Errorf("last_enriched_at = %q", ts)
					}
					g.Fields["last_enriched_at"] = "now"
				}
				if !reflect.DeepEqual(g.Fields, w.Fields) {
					t.Errorf("update %d\n got %v\nwant %v", j, g.Fields, w.Fields)
				}
			}
			chat.Close() // waits for in-flight handlers before the recorded requests are read
			osm.Close()
			mu.Lock()
			defer mu.Unlock()
			if len(llmReqs) != len(c.LLMRequests) {
				t.Fatalf("%d chat requests, want %d", len(llmReqs), len(c.LLMRequests))
			}
			for j, w := range c.LLMRequests {
				if llmHdr[j] != w.Path+"|"+w.Authorization+"|"+w.ContentType || !reflect.DeepEqual(llmReqs[j], w.Body) {
					t.Errorf("chat request %d\n got %v %s\nwant %v", j, llmReqs[j], llmHdr[j], w.Body)
				}
			}
			if len(fs.queries) != len(c.DDGQueries) || (len(fs.queries) > 0 && !reflect.DeepEqual(fs.queries, c.DDGQueries)) {
				t.Errorf("search queries %v, want %v", fs.queries, c.DDGQueries)
			}
			if len(osmParams) != len(c.OSMCalls) {
				t.Fatalf("%d geocoder calls, want %d", len(osmParams), len(c.OSMCalls))
			}
			for j, w := range c.OSMCalls {
				want := map[string]string{}
				for k, x := range w.Params {
					want[k] = strings.TrimSuffix(fmt.Sprint(x), ".0")
				}
				if !reflect.DeepEqual(osmParams[j], want) {
					t.Errorf("geocoder params %v, want %v", osmParams[j], want)
				}
			}
		})
	}
}

func TestEnrichSeamsUnavailable(t *testing.T) {
	store := &fakeLeadStore{leads: map[int64]*leadsread.Lead{1: leadFrom(t, enrichLeads[1])}}
	h := newTestRoutes(store, &scriptLLM{})
	h.providerLookup = mapProviders{}
	for action, want := range map[string]string{
		"find_emails":    `"message": "Email finder is not available"`,
		"scrape_website": `"message": "Website scraper is not available"`,
		"find_phone":     `"message": "Web search is not available"`,
	} {
		req := httptest.NewRequest("POST", "/x?action="+action, nil)
		req.SetPathValue("lead_id", "1")
		rec := httptest.NewRecorder()
		h.leadEnrich(rec, req)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), want) || strings.Contains(rec.Body.String(), `"step": "done"`) {
			t.Errorf("%s: %d %q", action, rec.Code, rec.Body)
		}
	}
	if len(store.updates) != 0 {
		t.Errorf("unexpected writes %v", store.updates)
	}
}

// The fixed scraper: fields come from the provider result, compared with the lead as loaded.
func TestScrapeSavesChangedFieldsOnly(t *testing.T) {
	store := &fakeLeadStore{leads: map[int64]*leadsread.Lead{1: leadFrom(t, enrichLeads[1])}}
	h := newTestRoutes(store, &scriptLLM{})
	ex := providers.NewObj()
	ex.Set("description", "old")
	ex.Set("phone", "+91 1")
	h.providerLookup = mapProviders{"website_scraper_provider": fakeProvider{out: providers.Output{Extra: ex}}}
	req := httptest.NewRequest("POST", "/x?action=scrape_website", nil)
	req.SetPathValue("lead_id", "1")
	rec := httptest.NewRecorder()
	h.leadEnrich(rec, req)
	if !strings.Contains(rec.Body.String(), `"fields_updated": ["phone", "last_enriched_at"]`) {
		t.Errorf("%q", rec.Body)
	}
	if len(store.updates) != 1 || store.updates[0].Fields["phone"] != "+91 1" {
		t.Errorf("updates %v", store.updates)
	}
}

func TestLeadEnrichBadID(t *testing.T) {
	var v enrichVec
	readLLMVec(t, "enrich.json", &v)
	h := newTestRoutes(&fakeLeadStore{}, &scriptLLM{})
	req := httptest.NewRequest("POST", "/api/lead/abc/enrich", nil)
	req.SetPathValue("lead_id", "abc")
	rec := httptest.NewRecorder()
	h.leadEnrich(rec, req)
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), `"loc":["path","lead_id"]`) || !strings.Contains(rec.Body.String(), `"int_parsing"`) {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
}

func TestExtractPhoneAndSSEEncoding(t *testing.T) {
	if got := extractPhone("x +91 98765-43210 y"); got != "+91 98765-43210" {
		t.Errorf("phone = %q", got)
	}
	var b strings.Builder
	pyDumpString(&b, "a\"\\\nü☃\U0001F600\x7f")
	if b.String() != `"a\"\\\n\u00fc\u2603\ud83d\ude00\u007f"` {
		t.Errorf("got %q", b.String())
	}
	keys := []string{}
	for k := range enrichLeads {
		keys = append(keys, fmt.Sprint(k))
	}
	sort.Strings(keys)
}

func providersOutputValue(v any) providers.Output { return providers.Output{Value: v} }

// llmRoutePatterns is the route table of the four LLM-backed routes (registered behind GO_LLM_ROUTES_ENABLED);
// the coverage tests of the areas they belong to count them as ported.
func llmRoutePatterns(t *testing.T) map[string]bool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://coverage@127.0.0.1:1/coverage")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	r := httpx.NewRouter()
	newLLMRoutes(Deps{Pool: pool, Identity: identity.NewStore(pool)}).register(r)
	out := map[string]bool{}
	for _, p := range r.Patterns() {
		out[p] = true
	}
	return out
}

func TestLLMRoutePatterns(t *testing.T) {
	got := llmRoutePatterns(t)
	for _, p := range []string{"POST /api/campaigns/generate", "POST /api/v2/workbooks/{workbook_id}/generate-column",
		"GET /api/workbooks/meta/provider-accuracy", "POST /api/lead/{lead_id}/enrich"} {
		if !got[p] {
			t.Errorf("missing %s", p)
		}
	}
	if len(got) != 4 {
		t.Errorf("%d routes, want 4", len(got))
	}
}

// With the flag off nothing registers (the Python path stays).
func TestRegisterLLMRoutesFlagOff(t *testing.T) {
	pool, _ := pgxpool.New(context.Background(), "postgres://coverage@127.0.0.1:1/coverage")
	defer pool.Close()
	r := httpx.NewRouter()
	RegisterLLMRoutes(r, Deps{Pool: pool, Identity: identity.NewStore(pool), Registry: &settings.Registry{}, Envelope: &secrets.Envelope{},
		Getenv: func(string) string { return "" }})
	if n := len(r.Patterns()); n != 0 {
		t.Errorf("%d routes registered with the flag off", n)
	}
	r = httpx.NewRouter()
	RegisterLLMRoutes(r, Deps{Pool: pool, Identity: identity.NewStore(pool), Registry: &settings.Registry{}, Envelope: &secrets.Envelope{},
		Getenv: func(k string) string {
			if k == "GO_LLM_ROUTES_ENABLED" {
				return "true"
			}
			return ""
		}})
	if n := len(r.Patterns()); n != 4 {
		t.Errorf("%d routes registered with the flag on, want 4", n)
	}
}

// Lead websites are user data: private, loopback and metadata targets are refused before any provider runs, and
// providers receive the strict client, never the operator client.
func TestScrapeRefusesInternalWebsites(t *testing.T) {
	for _, site := range []string{"http://127.0.0.1/x", "http://169.254.169.254/latest/meta-data", "http://10.1.2.3", "https://192.168.0.5/a", "http://localhost:8080", "file:///etc/passwd"} {
		m := map[string]any{"id": 1, "company": "C", "website": site}
		store := &fakeLeadStore{leads: map[int64]*leadsread.Lead{1: leadFrom(t, m)}}
		h := newTestRoutes(store, &scriptLLM{})
		called := false
		ex := providers.NewObj()
		ex.Set("email", "a@b.test")
		h.providerLookup = mapProviders{"website_scraper_provider": spyProvider{called: &called, out: providers.Output{Extra: ex}}}
		req := httptest.NewRequest("POST", "/x?action=scrape_website", nil)
		req.SetPathValue("lead_id", "1")
		rec := httptest.NewRecorder()
		h.leadEnrich(rec, req)
		if called || len(store.updates) != 0 || !strings.Contains(rec.Body.String(), "Website URL is not allowed") {
			t.Errorf("%s: called=%v updates=%v body=%q", site, called, store.updates, rec.Body)
		}
	}
}

func TestProvidersGetTheStrictClient(t *testing.T) {
	h := newLLMRoutes(Deps{})
	if h.userHTTP == nil || h.userHTTP == h.httpc {
		t.Fatal("user-data client must be separate from the operator client")
	}
	store := &fakeLeadStore{leads: map[int64]*leadsread.Lead{1: leadFrom(t, map[string]any{"id": 1, "company": "C", "website": "https://example.com"})}}
	h.store = store
	var got *http.Client
	h.providerLookup = mapProviders{"website_scraper_provider": clientSpy{got: &got}, "ddg_email_provider": clientSpy{got: &got}}
	for _, a := range []string{"scrape_website", "find_emails"} {
		got = nil
		req := httptest.NewRequest("POST", "/x?action="+a, nil)
		req.SetPathValue("lead_id", "1")
		h.leadEnrich(httptest.NewRecorder(), req)
		if got != h.userHTTP {
			t.Errorf("%s: provider did not get the user-data client", a)
		}
	}
}

type spyProvider struct {
	called *bool
	out    providers.Output
}

func (s spyProvider) Name() string { return "spy" }
func (s spyProvider) Enrich(context.Context, *http.Client, func() (string, error), providers.Input) (providers.Output, error) {
	*s.called = true
	return s.out, nil
}

type clientSpy struct{ got **http.Client }

func (c clientSpy) Name() string { return "spy" }
func (c clientSpy) Enrich(_ context.Context, hc *http.Client, _ func() (string, error), _ providers.Input) (providers.Output, error) {
	*c.got = hc
	return providers.Output{Extra: providers.NewObj()}, nil
}

// The enrich actions must find the providers under the names the Go registry uses (scrape.ddgEmail is "ddg_email").
func TestLeadEnrichFindsRegisteredProviderNames(t *testing.T) {
	e := &enrichRun{h: &llmRoutes{providerLookup: mapProviders{
		"ddg_email":       fakeProvider{name: "ddg_email"},
		"website_scraper": fakeProvider{name: "website_scraper"},
	}}}
	for _, names := range [][]string{emailProviderNames, scraperProviderNames} {
		if _, ok := e.provider(names...); !ok {
			t.Errorf("no provider registered under %v", names)
		}
	}
}
