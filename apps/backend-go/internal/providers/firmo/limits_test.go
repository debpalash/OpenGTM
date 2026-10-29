package firmo

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

func reply(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
}

// SEC fair access: every request carries the identifying User-Agent (the
// default, or the operator's SEC_EDGAR_USER_AGENT) and requests are spaced so
// the process-wide rate stays under SEC's 10 req/s ceiling, even across
// concurrent enrichments.
func TestSECUserAgentAndRateLimit(t *testing.T) {
	var mu sync.Mutex
	var stamps []time.Time
	var agents []string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		stamps = append(stamps, time.Now())
		agents = append(agents, r.Header.Get("User-Agent"))
		mu.Unlock()
		switch {
		case strings.Contains(r.URL.Host, "efts.sec.gov"):
			return reply(200, `{"hits":{"hits":[{"_source":{"ciks":["1234"],"display_names":["Acme Inc (CIK 0000001234)"]}}]}}`), nil
		case strings.Contains(r.URL.String(), "/submissions/"):
			return reply(200, `{"filings":{"recent":{"form":["D"],"accessionNumber":["0001-23-456"]}}}`), nil
		case strings.Contains(r.URL.String(), "/Archives/"):
			return reply(200, `<edgarSubmission><offeringData><totalAmountSold>5</totalAmountSold></offeringData></edgarSubmission>`), nil
		}
		return reply(200, `{}`), nil // company_tickers.json
	})
	p, _ := providers.Get("sec_edgar")
	run := func(settings map[string]any) {
		out, err := p.Enrich(context.Background(), &http.Client{Transport: rt}, func() (string, error) { return "", nil },
			providers.Input{Lead: map[string]any{"company": "Acme Inc"}, Settings: settings})
		if err != nil || out.Error != "" {
			t.Errorf("Enrich: %v %q", err, out.Error)
		}
	}
	run(nil)
	if len(stamps) != 4 {
		t.Fatalf("made %d requests, want 4 (tickers, full text, submissions, archive)", len(stamps))
	}
	for _, a := range agents {
		if a != secDefaultUA {
			t.Errorf("User-Agent %q want %q", a, secDefaultUA)
		}
	}
	mu.Lock()
	stamps, agents = nil, nil
	mu.Unlock()
	run(map[string]any{"SEC_EDGAR_USER_AGENT": "Ops Team ops@example.com"})
	for _, a := range agents {
		if a != "Ops Team ops@example.com" {
			t.Errorf("configured User-Agent not used: %q", a)
		}
	}

	// Three concurrent enrichments share one limiter: sorted request times are
	// at least ~110 ms apart (125 ms spacing), i.e. well under 10 req/s.
	mu.Lock()
	stamps, agents = nil, nil
	mu.Unlock()
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); run(nil) }()
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(stamps) != 12 {
		t.Fatalf("made %d requests, want 12", len(stamps))
	}
	sort.Slice(stamps, func(i, j int) bool { return stamps[i].Before(stamps[j]) })
	for i := 1; i < len(stamps); i++ {
		if gap := stamps[i].Sub(stamps[i-1]); gap < 110*time.Millisecond {
			t.Errorf("requests %d and %d only %v apart", i-1, i, gap)
		}
	}
	if span := stamps[len(stamps)-1].Sub(stamps[0]); span < 11*110*time.Millisecond {
		t.Errorf("12 requests spanned %v: rate above 10 req/s", span)
	}
}

// A cancelled context interrupts the limiter wait instead of blocking.
func TestSECLimiterHonoursContext(t *testing.T) {
	l := &limiter{interval: time.Hour}
	if err := l.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := l.acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("acquire = %v", err)
	}
}

// Bodies are capped at 1 MiB (the providers' shared limit); a 2 MiB page is
// refused with the cap error, never parsed.
func TestHTMLBodyCap(t *testing.T) {
	big := strings.Repeat("a", 2<<20)
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/robots.txt") {
			return reply(200, "User-agent: *\nAllow: /\n"), nil
		}
		return reply(200, big), nil
	})
	cases := map[string]providers.Input{
		"jsonld_firmographics": {Lead: map[string]any{"website": "https://acme.com"}},
		"tech_stack":           {Lead: map[string]any{"website": "https://acme.com"}, Settings: map[string]any{"TECH_STACK_WEBSITE_FETCH_ENABLED": true}},
	}
	for name, in := range cases {
		p, _ := providers.Get(name)
		out, err := p.Enrich(context.Background(), &http.Client{Transport: rt}, func() (string, error) { return "", nil }, in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if out.Error != "response body exceeds 1048576 bytes" {
			t.Errorf("%s: error %q", name, out.Error)
		}
	}
}

// The parsed text is cut to 300 000 characters (jsonld) and 200 000 (tech
// stack) before parsing, so markup past the cut is invisible, as in Python.
func TestPageTextTruncatedBeforeParse(t *testing.T) {
	ld := `<script type="application/ld+json">{"@type":"Organization","name":"Late"}</script>`
	serve := func(page string) *http.Client {
		return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, "/robots.txt") {
				return reply(404, ""), nil
			}
			return reply(200, page), nil
		})}
	}
	enrich := func(name string, page string, in providers.Input) providers.Output {
		p, _ := providers.Get(name)
		out, err := p.Enrich(context.Background(), serve(page), func() (string, error) { return "", nil }, in)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	site := map[string]any{"website": "https://acme.com"}
	if out := enrich("jsonld_firmographics", strings.Repeat("x", 299_990)+ld, providers.Input{Lead: site}); out.Error != "no_structured_data" {
		t.Errorf("block straddling the 300k cut was parsed: %+v", out)
	}
	if out := enrich("jsonld_firmographics", strings.Repeat("x", 100)+ld, providers.Input{Lead: site}); out.Error != "" {
		t.Errorf("block inside the cut not parsed: %+v", out)
	}
	ts := providers.Input{Lead: site, Settings: map[string]any{"TECH_STACK_WEBSITE_FETCH_ENABLED": true}}
	if out := enrich("tech_stack", strings.Repeat("x", 200_000)+"wp-content/", ts); out.Error != "No technologies detected" {
		t.Errorf("signal past the 200k cut was seen: %+v", out)
	}
	if out := enrich("tech_stack", strings.Repeat("x", 199_000)+"wp-content/", ts); out.Error != "" {
		t.Errorf("signal inside the cut missed: %+v", out)
	}
}

// Every outbound request goes through the caller's egress client: a lead
// website on a loopback address is refused before any request (URL check) and
// a hostname-less redirect target is refused at the hop; the loopback server is
// never reached.
func TestFirmoProvidersHonourEgressGuard(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(200)
	}))
	defer srv.Close()
	client := egress.NewClientForEnv("production", nil)
	noKey := func() (string, error) { return "", nil }
	for _, name := range []string{"jsonld_firmographics", "tech_stack"} {
		p, _ := providers.Get(name)
		out, err := p.Enrich(context.Background(), client, noKey, providers.Input{
			Lead: map[string]any{"website": srv.URL}, Settings: map[string]any{"TECH_STACK_WEBSITE_FETCH_ENABLED": true}})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(out.Error, "private/blocked") {
			t.Errorf("%s: error %q, want the url guard's message", name, out.Error)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("loopback server reached %d times", n)
	}
}

// The GLEIF parent links come from the vendor response, so they are untrusted
// URLs: one that points at the loopback is refused by the dial-time guard.
func TestGleifParentLinkIsEgressGuarded(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(200)
	}))
	defer srv.Close()
	fastLimiters(t)
	guarded := egress.NewClientForEnv("production", nil).Transport
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "api.gleif.org" {
			if strings.Contains(r.URL.Path, "fuzzycompletions") {
				return reply(200, `{"data":[{"relationships":{"lei-records":{"data":{"id":"L1"}}},"attributes":{"value":"Acme"}}]}`), nil
			}
			return reply(200, `{"data":{"id":"L1","attributes":{"entity":{"legalName":{"name":"ACME"}}},"relationships":{"direct-parent":{"links":{"lei-record":"`+srv.URL+`/p"}}}}}`), nil
		}
		return guarded.RoundTrip(r)
	})
	p, _ := providers.Get("gleif")
	out, err := p.Enrich(context.Background(), &http.Client{Transport: rt}, func() (string, error) { return "", nil },
		providers.Input{Lead: map[string]any{"company": "Acme"}})
	if err != nil || out.Error != "" {
		t.Fatalf("Enrich: %v %q", err, out.Error)
	}
	if _, ok := out.Extra.Get("parent_lei"); ok {
		t.Error("parent record fetched from a loopback link")
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("loopback server reached %d times", n)
	}
}
