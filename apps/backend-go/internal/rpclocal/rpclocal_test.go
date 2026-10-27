package rpclocal

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/emailverify"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadweb"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers/scrape"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/pyworker"
)

type fakePy struct{ calls []pyworker.ProviderCall }

func (f *fakePy) Call(_ context.Context, c pyworker.ProviderCall) (pyworker.ProviderResult, error) {
	f.calls = append(f.calls, c)
	return pyworker.ProviderResult{Status: "ok", Output: json.RawMessage(`{"from":"python"}`)}, nil
}

type fakeSearch struct {
	n     atomic.Int32
	ws    atomic.Value
	block bool
}

func (s *fakeSearch) Text(ctx context.Context, q string, n int) []scrape.Hit {
	s.n.Add(1)
	s.ws.Store(providers.WorkspaceFrom(ctx))
	if s.block {
		<-ctx.Done()
		return nil
	}
	return []scrape.Hit{{Title: "T", Href: "https://a.example", Body: "b"}}
}

func (s *fakeSearch) Web(ctx context.Context, q string, n int, raise bool) ([]scrape.Hit, error) {
	return s.Text(ctx, q, n), nil
}

type fakeFetch struct{ pages map[string]string }

func (f fakeFetch) Fetch(_ context.Context, u string, _ leadweb.FetchOptions) leadweb.FetchResult {
	if h, ok := f.pages[u]; ok {
		return leadweb.FetchResult{URL: u, StatusCode: 200, Text: h}
	}
	return leadweb.FetchResult{URL: u, StatusCode: 404}
}

func only(names ...string) func(string) bool {
	return func(n string) bool {
		for _, x := range names {
			if x == n {
				return true
			}
		}
		return false
	}
}

func call(p, id string, in string) pyworker.ProviderCall {
	return pyworker.ProviderCall{CallID: id, Provider: p, WorkspaceID: "ws1", Input: json.RawMessage(in), Timeout: time.Second}
}

func TestUnlistedNamesGoToPython(t *testing.T) {
	py := &fakePy{}
	c := New(Deps{Py: py, Local: only("jobspy")})
	res, err := c.Call(context.Background(), call("ddgs_search", "a", `{"query":"x"}`))
	if err != nil || string(res.Output) != `{"from":"python"}` || len(py.calls) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := New(Deps{}).Call(context.Background(), call("jobspy", "a", `{}`)); !errors.Is(err, pyworker.ErrUnavailable) {
		t.Fatalf("no worker: %v", err)
	}
	// reacher keeps its operator-client path even when listed.
	if New(Deps{Local: only("reacher")}).Serves("reacher") {
		t.Fatal("reacher served")
	}
}

func TestLocalRPCAndProvider(t *testing.T) {
	s := &fakeSearch{}
	scrape.Configure(scrape.Deps{Search: s, Sleep: func(context.Context, time.Duration) error { return nil }})
	t.Cleanup(func() { scrape.Configure(scrape.Deps{}) })
	py := &fakePy{}
	c := New(Deps{Py: py, Local: only("ddgs_search", "jobspy_signals", "jobspy")})

	res, err := c.Call(context.Background(), call("ddgs_search", "c1", `{"query":"acme","max_results":3}`))
	if err != nil || res.Status != "ok" || len(py.calls) != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	want := `{"provider":"ddgs_search","success":true,"fields":{"results":[{"title":"T","snippet":"b","url":"https://a.example"}]},"confidence":1.0,"error":"","billing_evidence":null,"license":"unknown"}`
	if string(res.Output) != want {
		t.Errorf("output %s", res.Output)
	}
	if s.ws.Load() != "ws1" {
		t.Errorf("workspace salt %v", s.ws.Load())
	}
	// dedupe: the same (workspace, call_id) is answered from the first run.
	n := s.n.Load()
	if _, err := c.Call(context.Background(), call("ddgs_search", "c1", `{"query":"acme"}`)); err != nil || s.n.Load() != n {
		t.Errorf("dedupe re-ran: %d -> %d", n, s.n.Load())
	}
	// an error result is never cached.
	bad := call("ddgs_search", "c2", `{"query":"  "}`)
	r1, _ := c.Call(context.Background(), bad)
	if r1.Status != "error" || r1.Error != "query required" || !strings.Contains(string(r1.Output), `"success":false`) {
		t.Fatalf("%+v", r1)
	}
	// the registry provider by its own name (jobspy: no jobs -> error status with output).
	s2 := &fakeSearch{}
	scrape.Configure(scrape.Deps{Search: s2, Sleep: func(context.Context, time.Duration) error { return nil }})
	r2, err := c.Call(context.Background(), call("jobspy", "c3", `{"company":"Acme"}`))
	if err != nil || r2.Status != "ok" || !strings.Contains(string(r2.Output), `"provider":"jobspy","success":true`) {
		t.Fatalf("%+v %v", r2, err)
	}
	r3, _ := c.Call(context.Background(), call("jobspy", "c4", `{}`))
	if r3.Status != "error" || r3.Error != "no_company_name" || r3.CostUSD != 0 {
		t.Fatalf("%+v", r3)
	}
	if r, _ := c.Call(context.Background(), call("jobspy", "c5", `[1]`)); r.Status != "error" || r.HTTPStatus != http.StatusUnprocessableEntity {
		t.Fatalf("non-object input: %+v", r)
	}
}

func TestTimeout(t *testing.T) {
	scrape.Configure(scrape.Deps{Search: &fakeSearch{block: true}})
	t.Cleanup(func() { scrape.Configure(scrape.Deps{}) })
	c := New(Deps{Local: only("ddgs_search")})
	cl := call("ddgs_search", "t1", `{"query":"x"}`)
	cl.Timeout = 30 * time.Millisecond
	res, err := c.Call(context.Background(), cl)
	if err != nil || res.Status != "timeout" || res.Error != "provider timed out" {
		t.Fatalf("%+v %v", res, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Call(ctx, call("ddgs_search", "t2", `{"query":"x"}`)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
}

// keyedEcho is a test provider that leaks its key into the output. The extra
// scrub value "key=" is shorter than six characters and is ignored, as Python.
type keyedEcho struct{}

func (keyedEcho) Name() string     { return "rpclocal_test_keyed" }
func (keyedEcho) KeyEnv() []string { return []string{"TEST_KEY"} }
func (keyedEcho) Enrich(_ context.Context, _ *http.Client, key func() (string, error), _ providers.Input) (providers.Output, error) {
	k, err := key()
	if err != nil {
		return providers.Output{}, err
	}
	f := providers.NewObj()
	f.Set("echo", "key="+k)
	f.Set("nested", []any{"x" + k})
	return providers.Output{Extra: f}, nil
}

func init() { providers.Register(keyedEcho{}) }

type secrets map[string]string

func (s secrets) Get(_ context.Context, ws, key string) (string, string, error) {
	if key == "FAIL" {
		return "", "", errors.New("vault down")
	}
	return s[ws+"/"+key], "test", nil
}

func TestScrubAndCredentials(t *testing.T) {
	c := New(Deps{Local: only("rpclocal_test_keyed"), Secrets: secrets{"ws1/TEST_KEY": "supersecret-123"},
		Scrub: func(context.Context, string) []string { return []string{"key="} }})
	res, err := c.Call(context.Background(), call("rpclocal_test_keyed", "s1", `{}`))
	if err != nil || res.Status != "ok" {
		t.Fatalf("%+v %v", res, err)
	}
	if strings.Contains(string(res.Output), "supersecret") || !strings.Contains(string(res.Output), `"echo":"key=[redacted]"`) ||
		!strings.Contains(string(res.Output), `"nested":["x[redacted]"]`) {
		t.Fatalf("not scrubbed: %s", res.Output)
	}
}

// The email waterfall's website step runs without the Python worker once
// website_scraper is listed.
func TestEmailWaterfallWithoutPyworker(t *testing.T) {
	scrape.Configure(scrape.Deps{Fetch: fakeFetch{pages: map[string]string{
		"https://acme.example": `<html><body><a href="mailto:sales@acme.example">mail</a></body></html>`}}})
	t.Cleanup(func() { scrape.Configure(scrape.Deps{}) })
	c := New(Deps{Local: only("website_scraper")}) // no Py at all
	d := emailverify.NewDeps()
	d.Finders = []emailverify.Finder{&emailverify.WebsiteFinder{Py: c, Timeout: 5 * time.Second}}
	lead := map[string]any{"company": "Acme", "website": "https://acme.example/about"}
	res, err := emailverify.Waterfall(context.Background(), d, "ws1", lead)
	if err != nil || !res.Found || res.Email != "sales@acme.example" || res.Provider != "website" || lead["email"] != "sales@acme.example" {
		t.Fatalf("%+v %v %v", res, err, lead)
	}
}
