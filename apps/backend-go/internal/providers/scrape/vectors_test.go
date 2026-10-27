package scrape

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/browserclient"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadweb"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// Vectors are recorded from the real Python providers by
// legacy/python/scripts/record_scrape_providers.py with the I/O seams
// replaced by the scripted answers stored in each scenario.

type page struct {
	Status *int   `json:"status"`
	Ctype  *string `json:"ctype"`
	HTML   string `json:"html"`
	Error  string `json:"error"`
	Raise  bool   `json:"raise"`
}

type mailAns struct {
	Email       string `json:"email"`
	Confidence  string `json:"confidence"`
	Deliverable *bool  `json:"deliverable"`
	CatchAll    bool   `json:"catch_all"`
}

type scenario struct {
	Name     string                     `json:"name"`
	Provider string                     `json:"provider"`
	Input    json.RawMessage            `json:"input"`
	Search   map[string]json.RawMessage `json:"search"`
	Pages    map[string]page            `json:"pages"`
	Mail     struct {
		MX     bool    `json:"mx"`
		Pick   mailAns `json:"pick"`
		Verify mailAns `json:"verify"`
	} `json:"mail"`
	Holehe struct {
		ExistsOn     []string `json:"exists_on"`
		TotalChecked int      `json:"total_checked"`
	} `json:"holehe"`
	StaffSpy struct {
		Rows []browserclient.StaffRow `json:"rows"`
	} `json:"staffspy"`
	Calls  json.RawMessage `json:"calls"`
	Output json.RawMessage `json:"output"`
}

// fakeWorld replays one scenario and logs the calls in the recorder's shape.
type fakeWorld struct {
	sc    scenario
	mu    sync.Mutex
	calls []any
}

func (w *fakeWorld) log(c ...any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = append(w.calls, c)
}

func (w *fakeWorld) hits(q string, n int) ([]Hit, bool) {
	raw, ok := w.sc.Search[q]
	if !ok {
		return nil, true
	}
	if string(raw) == `"raise"` {
		return nil, false
	}
	var hs []struct{ Title, Href, Body string }
	if err := json.Unmarshal(raw, &hs); err != nil {
		panic(err)
	}
	out := []Hit{}
	for i, h := range hs {
		if i >= n {
			break
		}
		out = append(out, Hit{Title: h.Title, Href: h.Href, Body: h.Body})
	}
	return out, true
}

func (w *fakeWorld) Text(_ context.Context, q string, n int) []Hit {
	w.log("text", q, n)
	h, _ := w.hits(q, n)
	return h
}

func (w *fakeWorld) Web(_ context.Context, q string, n int, raise bool) ([]Hit, error) {
	w.log("web", q, n, raise)
	h, ok := w.hits(q, n)
	if !ok {
		if raise {
			return nil, leadweb.ErrSearchFailed
		}
		return nil, nil
	}
	return h, nil
}

func (w *fakeWorld) Fetch(_ context.Context, u string, o leadweb.FetchOptions) leadweb.FetchResult {
	w.log("fetch", u, o.Tier, int(o.Timeout/time.Second))
	p, ok := w.sc.Pages[u]
	if !ok {
		return leadweb.FetchResult{URL: u, StatusCode: 404}
	}
	st := 200
	if p.Status != nil {
		st = *p.Status
	}
	return leadweb.FetchResult{URL: u, StatusCode: st, Text: p.HTML, Error: p.Error}
}

func (w *fakeWorld) RoundTrip(r *http.Request) (*http.Response, error) {
	u := r.URL.String()
	w.log("get", u)
	p, ok := w.sc.Pages[u]
	resp := &http.Response{StatusCode: 404, Header: http.Header{"Content-Type": {"text/html"}},
		Body: io.NopCloser(strings.NewReader("")), Request: r}
	if !ok {
		return resp, nil
	}
	if p.Raise {
		return nil, errors.New("connect failed")
	}
	resp.StatusCode = 200
	if p.Status != nil {
		resp.StatusCode = *p.Status
	}
	resp.Header.Set("Content-Type", "text/html; charset=utf-8")
	if p.Ctype != nil {
		resp.Header.Set("Content-Type", *p.Ctype)
	}
	resp.Body = io.NopCloser(strings.NewReader(p.HTML))
	return resp, nil
}

func mc(a mailAns) MailCheck {
	return MailCheck{Email: a.Email, Confidence: a.Confidence, Deliverable: a.Deliverable, CatchAll: a.CatchAll}
}

func (w *fakeWorld) HasMX(_ context.Context, d string) bool { w.log("mx", d); return w.sc.Mail.MX }

func (w *fakeWorld) PickBest(_ context.Context, c []string) (MailCheck, error) {
	cs := make([]any, len(c))
	for i, s := range c {
		cs[i] = s
	}
	w.log("pick", cs)
	return mc(w.sc.Mail.Pick), nil
}

func (w *fakeWorld) Verify(_ context.Context, e string) (MailCheck, error) {
	w.log("verify", e)
	return mc(w.sc.Mail.Verify), nil
}

func (w *fakeWorld) Render(context.Context, browserclient.RenderRequest) (browserclient.RenderResult, error) {
	return browserclient.RenderResult{}, errors.New("render not scripted")
}

func (w *fakeWorld) Holehe(_ context.Context, r browserclient.HoleheRequest) (browserclient.HoleheResult, error) {
	w.log("holehe", r.Email)
	return browserclient.HoleheResult{Email: r.Email, ExistsOn: w.sc.Holehe.ExistsOn, TotalChecked: w.sc.Holehe.TotalChecked}, nil
}

func (w *fakeWorld) StaffSpy(_ context.Context, r browserclient.StaffSpyRequest) (browserclient.StaffSpyResult, error) {
	w.log("staffspy", r.CompanyName, r.Location, r.MaxResults)
	return browserclient.StaffSpyResult{CompanyName: r.CompanyName, Staff: w.sc.StaffSpy.Rows}, nil
}

func noSleep(context.Context, time.Duration) error { return nil }

// deviations are the documented fixes over the recorded Python output.
var deviations = map[string]func(o *wbcrud.Obj){
	// decision_maker: Python never returned the linkedin_url it found (it
	// compared the lead with itself after mutating it in place).
	"dm_found": func(o *wbcrud.Obj) {
		f, _ := o.Get("fields")
		old := f.(*wbcrud.Obj)
		nf := wbcrud.NewObj()
		for _, k := range old.Keys() {
			v, _ := old.Get(k)
			if k == "decision_makers" {
				nf.Set("linkedin_url", "https://in.linkedin.com/in/priya")
			}
			nf.Set(k, v)
		}
		o.Set("fields", nf)
	},
}

func loadScenarios(t *testing.T) []scenario {
	t.Helper()
	raw, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var scs []scenario
	if err := json.Unmarshal(raw, &scs); err != nil {
		t.Fatal(err)
	}
	return scs
}

func canonJSON(t *testing.T, raw []byte) string {
	t.Helper()
	var v any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// runScenario runs one recorded scenario against the Go port.
func runScenario(t *testing.T, sc scenario) (*providers.Obj, []any) {
	t.Helper()
	w := &fakeWorld{sc: sc}
	Configure(Deps{Search: w, Fetch: w, Mail: w, Browser: w, StaffSpyEnabled: true, Sleep: noSleep,
		LinkedInCookies: func(context.Context, string) ([]browserclient.Cookie, error) {
			return []browserclient.Cookie{{Name: "li_at", Value: "x", Domain: ".linkedin.com"}}, nil
		}})
	client := &http.Client{Transport: w}
	ctx := providers.WithWorkspace(context.Background(), "ws-vectors") // staffspy needs the calling workspace
	if rpc, ok := RPCs()[sc.Provider]; ok {
		out, err := rpc(ctx, client, sc.Input)
		if err != nil {
			t.Fatal(err)
		}
		return out, w.calls
	}
	p, ok := providers.Get(sc.Provider)
	if !ok {
		t.Fatalf("%s not registered", sc.Provider)
	}
	lead, okd := decodeJSON(string(sc.Input))
	if !okd {
		t.Fatal("bad input")
	}
	out, err := p.Enrich(ctx, client, func() (string, error) { return "", nil }, providers.Input{Lead: lead.(map[string]any)})
	if err != nil {
		t.Fatal(err)
	}
	return OutputDict(ResultProviderOf(p, out), out), w.calls
}

func TestProviderVectors(t *testing.T) {
	nowYear = func() int { return 2026 }
	t.Cleanup(func() { Configure(Deps{}) })
	scs := loadScenarios(t)
	if len(scs) < 70 {
		t.Fatalf("only %d vectors", len(scs))
	}
	for _, sc := range scs {
		t.Run(sc.Name, func(t *testing.T) {
			got, calls := runScenario(t, sc)
			want, err := wbcrud.ParseObject(sc.Output)
			if err != nil {
				t.Fatal(err)
			}
			if fix, ok := deviations[sc.Name]; ok {
				fix(want)
			}
			wb, _ := want.MarshalJSON()
			gb, _ := got.MarshalJSON()
			if string(wb) != string(gb) {
				t.Errorf("output\n got %s\nwant %s", gb, wb)
			}
			cb, _ := json.Marshal(calls)
			if calls == nil {
				cb = []byte("[]")
			}
			if g, w := canonJSON(t, cb), canonJSON(t, sc.Calls); g != w {
				t.Errorf("calls\n got %s\nwant %s", g, w)
			}
		})
	}
}
