package poller

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers/firmo"
)

// ── shared helpers ──────────────────────────────────────────────────────────

var fixedNow = time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

func fixedDeps() Deps { return Deps{Now: func() time.Time { return fixedNow }} }

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

// norm round-trips v through JSON so Go and vector values compare structurally.
func norm(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func eventVec(e DetectedEvent, now float64) map[string]any {
	m := map[string]any{
		"natural_event_id": e.NaturalEventID, "signal_type": e.SignalType, "title": e.Title,
		"description": e.Description, "source": e.Source, "source_url": e.SourceURL,
		"weight": e.Weight, "occurred_at": e.OccurredAt, "lead_id": nil, "company": e.Company,
	}
	if e.LeadID != nil {
		m["lead_id"] = *e.LeadID
	}
	if e.OccurredAt == now {
		m["occurred_at"] = "now"
	}
	return m
}

func eventsVec(t *testing.T, evs []DetectedEvent) any {
	if evs == nil {
		return nil
	}
	out := []any{}
	now := float64(fixedNow.UnixNano()) / 1e9
	for _, e := range evs {
		out = append(out, eventVec(e, now))
	}
	return norm(t, out)
}

func patchVec(t *testing.T, p map[string]any) any {
	if p == nil {
		return nil
	}
	return norm(t, p)
}

// fixVecNow rewrites the vector's "now" occurred_at markers the way eventVec does.
func eqVec(t *testing.T, what string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		g, _ := json.Marshal(got)
		w, _ := json.Marshal(want)
		t.Errorf("%s\n got %s\nwant %s", what, g, w)
	}
}

func watchFrom(m map[string]any) WatchView {
	w := WatchView{ID: "w1"}
	if m == nil {
		return w
	}
	w.Target, _ = m["target"].(string)
	w.ResolvedCIK, _ = m["resolved_cik"].(string)
	w.Cursor, _ = m["cursor"].(map[string]any)
	return w
}

// ── ParseFeed / parse_epoch vectors ─────────────────────────────────────────

func TestParseFeedVectors(t *testing.T) {
	var v struct {
		ParseFeed []struct {
			Name       string  `json:"name"`
			XML        string  `json:"xml"`
			MaxEntries int     `json:"max_entries"`
			Entries    []any   `json:"entries"`
			_          float64 // keep struct literal-free
		} `json:"parse_feed"`
		ParseEpoch []struct {
			Value string  `json:"value"`
			Epoch float64 `json:"epoch"`
		} `json:"parse_epoch"`
	}
	readJSON(t, "testdata/rss_vectors.json", &v)
	if len(v.ParseFeed) < 28 || len(v.ParseEpoch) < 36 {
		t.Fatalf("vectors truncated: %d/%d", len(v.ParseFeed), len(v.ParseEpoch))
	}
	for _, c := range v.ParseFeed {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			got, _ := ParseFeed([]byte(c.XML), c.MaxEntries)
			out := []any{}
			for _, e := range got {
				out = append(out, map[string]any{"guid": e.GUID, "title": e.Title, "link": e.Link, "summary": e.Summary, "published_epoch": e.PublishedEpoch})
			}
			eqVec(t, "entries", norm(t, out), norm(t, c.Entries))
		})
	}
	for _, c := range v.ParseEpoch {
		if got := parseEpoch(c.Value); got != c.Epoch {
			t.Errorf("parse_epoch(%q) = %v want %v", c.Value, got, c.Epoch)
		}
	}
}

func TestParseFeedMalformedAndHuge(t *testing.T) {
	if _, err := ParseFeed([]byte("<rss><item>"), 10); err == nil {
		t.Error("malformed xml must report an error")
	}
	if got, _ := ParseFeed(nil, 10); len(got) != 0 {
		t.Error("empty document yields no entries")
	}
	// a node bomb is rejected, not materialised
	huge := "<r>" + strings.Repeat("<a/>", maxFeedNodes+10) + "</r>"
	if got, err := ParseFeed([]byte(huge), 10); err == nil || len(got) != 0 {
		t.Errorf("huge tree: %d entries, err %v", len(got), err)
	}
	// a billion-laughs style document never expands recursively
	bomb := `<?xml version="1.0"?><!DOCTYPE r [<!ENTITY a "aaaaaaaaaa"><!ENTITY b "&a;&a;&a;&a;&a;&a;&a;&a;"><!ENTITY c "&b;&b;&b;&b;&b;&b;&b;&b;">]><rss><item><title>&c;</title><guid>1</guid></item></rss>`
	got, _ := ParseFeed([]byte(bomb), 10)
	for _, e := range got {
		if len(e.Title) > 1000 {
			t.Errorf("entity expansion produced %d bytes", len(e.Title))
		}
	}
	// invalid UTF-8 degrades to U+FFFD like httpx's text decoding
	got, err := ParseFeed([]byte("<rss><item><title>a\xffb</title><guid>1</guid></item></rss>"), 10)
	if err != nil || len(got) != 1 || got[0].Title != "a�b" {
		t.Errorf("invalid utf8: %v %+v", err, got)
	}
}

func TestFeedMaxEntries(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("<rss><channel>")
	for i := 0; i < 50; i++ {
		fmt.Fprintf(&sb, "<item><guid>g%d</guid></item>", i)
	}
	sb.WriteString("</channel></rss>")
	for _, n := range []int{1, 7, 50, 100} {
		got, _ := ParseFeed([]byte(sb.String()), n)
		want := n
		if want > 50 {
			want = 50
		}
		if len(got) != want {
			t.Errorf("max %d: %d entries want %d", n, len(got), want)
		}
	}
	// a feed fetch applies deps.FeedMaxEntries
	srv := newReplay(t, &transcript{Exchanges: []tExchange{{Req: &tReq{Method: "GET"}, Resp: &tResp{Status: 200, Headers: map[string]string{"content-type": "application/rss+xml"}, Body: sb.String()}}}}, "feeds.example.com")
	d := srv.deps()
	d.FeedMaxEntries = 5
	res, err := FetchFeed(context.Background(), d, "https://feeds.example.com/news.xml", "", "")
	if err != nil || res.Error != "" || len(res.Entries) != 5 {
		t.Fatalf("fetch: %+v %v", res, err)
	}
}

// ── replay harness (Python-recorded transcripts, never a real host) ─────────

type tReq struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

type tResp struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
	Raises  string            `json:"raises"`
}

type tExchange struct {
	DNS *struct {
		Host    string   `json:"host"`
		Answers []string `json:"answers"`
		Error   string   `json:"error"`
	} `json:"dns"`
	Req  *tReq  `json:"request"`
	Resp *tResp `json:"response"`
}

type transcript struct {
	Scenario  string          `json:"scenario"`
	Input     map[string]any  `json:"input"`
	Exchanges []tExchange     `json:"http_exchanges"`
	Result    json.RawMessage `json:"result"`
	file      string
}

func loadTranscripts(t *testing.T, area string, min int) []*transcript {
	t.Helper()
	files, _ := filepath.Glob("../../testdata/transcripts/" + area + "/*.json")
	if len(files) < min {
		t.Fatalf("%s transcripts: %d files, want >= %d", area, len(files), min)
	}
	var out []*transcript
	for _, f := range files {
		var tr transcript
		readJSON(t, f, &tr)
		tr.file = area + "/" + filepath.Base(f)
		out = append(out, &tr)
	}
	return out
}

type fakeResolver struct {
	mu      sync.Mutex
	answers map[string][]string
	errs    map[string]string
	flip    map[string][]string // answers from the 2nd lookup on (rebinding)
	calls   map[string]int
}

func (f *fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[host]++
	if e, ok := f.errs[host]; ok {
		return nil, errors.New("lookup " + host + ": " + e)
	}
	ans, ok := f.answers[host]
	if fl, ok2 := f.flip[host]; ok2 && f.calls[host] > 1 {
		ans, ok = fl, true
	}
	if !ok {
		ans = []string{"93.184.216.34"}
	}
	var out []netip.Addr
	for _, a := range ans {
		out = append(out, netip.MustParseAddr(a))
	}
	return out, nil
}

type replay struct {
	t      *testing.T
	tr     *transcript
	srv    *httptest.Server
	res    *fakeResolver
	mu     sync.Mutex
	next   int
	raised bool
	// extra response headers the recorder dropped (validators taken from the result)
	extra  map[string]string
	dials  int
	hits   int
	client *http.Client
}

// newReplay serves tr's request exchanges in order over TLS and routes every
// dial to it through the REAL egress guard (fake resolver from the DNS entries).
func newReplay(t *testing.T, tr *transcript, hosts ...string) *replay {
	t.Helper()
	r := &replay{t: t, tr: tr, res: &fakeResolver{answers: map[string][]string{}, errs: map[string]string{}}}
	for _, ex := range tr.Exchanges {
		if ex.DNS == nil {
			continue
		}
		if _, seen := r.res.answers[ex.DNS.Host]; seen {
			continue
		}
		if _, seen := r.res.errs[ex.DNS.Host]; seen {
			continue
		}
		if ex.DNS.Error != "" {
			r.res.errs[ex.DNS.Host] = ex.DNS.Error
		} else {
			r.res.answers[ex.DNS.Host] = ex.DNS.Answers
		}
	}
	r.srv = httptest.NewUnstartedServer(http.HandlerFunc(r.handle))
	r.srv.TLS = &tls.Config{}
	r.srv.StartTLS()
	t.Cleanup(r.srv.Close)
	addr := r.srv.Listener.Addr().String()
	dial := func(ctx context.Context, network, _ string) (net.Conn, error) {
		r.mu.Lock()
		r.dials++
		r.mu.Unlock()
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	r.client = egress.NewClientWith("production", nil, r.res, dial)
	r.client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // test server cert
	return r
}

func (r *replay) deps() SourceDeps {
	return SourceDeps{Deps: fixedDeps(), Client: r.client, Spacer: NewHostSpacer(0)}
}

func (r *replay) nextRequest() *tExchange {
	for r.next < len(r.tr.Exchanges) {
		ex := &r.tr.Exchanges[r.next]
		r.next++
		if ex.Req != nil {
			return ex
		}
	}
	return nil
}

func (r *replay) handle(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.hits++
	ex := r.nextRequest()
	r.mu.Unlock()
	if ex == nil && r.raised { // the transport retries a replayable GET after a dropped connection
		hj, _ := w.(http.Hijacker)
		c, _, _ := hj.Hijack()
		c.Close()
		return
	}
	if ex == nil {
		r.t.Errorf("%s: unrouted request %s %s", r.tr.file, req.Method, req.URL)
		w.WriteHeader(599)
		return
	}
	want := ex.Req
	if req.Method != want.Method {
		r.t.Errorf("%s: method %s want %s", r.tr.file, req.Method, want.Method)
	}
	if want.URL != "" {
		wu := want.URL[strings.Index(want.URL, "://")+3:]
		wantPath := "/"
		if i := strings.Index(wu, "/"); i >= 0 {
			wantPath = wu[i:]
		}
		if req.URL.RequestURI() != wantPath {
			r.t.Errorf("%s: request uri %s want %s", r.tr.file, req.URL.RequestURI(), wantPath)
		}
	}
	for k, v := range want.Headers {
		got := req.Header.Get(k)
		if strings.EqualFold(k, "host") {
			got = req.Host
		}
		if got != v {
			r.t.Errorf("%s: header %s = %q want %q", r.tr.file, k, got, v)
		}
	}
	if ex.Resp.Raises != "" {
		r.mu.Lock()
		r.raised = true
		r.mu.Unlock()
		hj, _ := w.(http.Hijacker)
		c, _, _ := hj.Hijack()
		c.Close()
		return
	}
	for k, v := range r.extra {
		w.Header().Set(k, v)
	}
	for k, v := range ex.Resp.Headers {
		w.Header().Set(k, v)
	}
	w.WriteHeader(ex.Resp.Status)
	_, _ = io.WriteString(w, ex.Resp.Body)
}

func (r *replay) requestsLeft() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, ex := range r.tr.Exchanges[r.next:] {
		if ex.Req != nil {
			n++
		}
	}
	return n
}

// ── FetchFeed transcripts ───────────────────────────────────────────────────

func TestFetchFeedTranscripts(t *testing.T) {
	for _, tr := range loadTranscripts(t, "rss", 30) {
		tr := tr
		t.Run(tr.file, func(t *testing.T) {
			rp := newReplay(t, tr)
			rp.extra = recordedValidators(tr)
			d := rp.deps()
			s := func(k string) string { v, _ := tr.Input[k].(string); return v }
			if mx, ok := tr.Input["max_entries"].(float64); ok {
				d.FeedMaxEntries = int(mx)
			}
			res, err := FetchFeed(context.Background(), d, s("url"), s("etag"), s("last_modified"))
			if err != nil {
				t.Fatal(err)
			}
			var want struct {
				Status       int      `json:"status"`
				NotModified  bool     `json:"not_modified"`
				ETag         *string  `json:"etag"`
				LastModified *string  `json:"last_modified"`
				Error        *string  `json:"error"`
				Entries      []any    `json:"entries"`
				_            struct{} `json:"-"`
			}
			if err := json.Unmarshal(tr.Result, &want); err != nil {
				t.Fatal(err)
			}
			switch tr.Scenario {
			case "oversize_body_1_1_mib":
				// Deviation D4: Python parses the whole 1.1 MiB body; Go refuses it.
				if res.Error == "" || !strings.Contains(res.Error, "exceeds") || res.Status != 0 || len(res.Entries) != 0 {
					t.Errorf("oversize must be refused: %+v", res)
				}
				return
			case "timeout", "connect_error":
				if res.Error == "" || res.Status != 0 || len(res.Entries) != 0 {
					t.Errorf("transport failure: %+v", res)
				}
				return
			}
			if res.Status != want.Status || res.NotModified != want.NotModified {
				t.Errorf("status/not_modified %d/%v want %d/%v", res.Status, res.NotModified, want.Status, want.NotModified)
			}
			wantErr := ""
			if want.Error != nil {
				wantErr = *want.Error
			}
			if tr.Scenario == "blocked_dns_error" {
				if !strings.HasPrefix(res.Error, "blocked_url: cannot resolve host") {
					t.Errorf("dns failure: %q", res.Error)
				}
				return
			}
			if res.Error != wantErr {
				t.Errorf("error %q want %q", res.Error, wantErr)
			}
			if !reflect.DeepEqual(res.ETag, want.ETag) || !reflect.DeepEqual(res.LastModified, want.LastModified) {
				t.Errorf("validators %v/%v want %v/%v", deref(res.ETag), deref(res.LastModified), deref(want.ETag), deref(want.LastModified))
			}
			got := []any{}
			for _, e := range res.Entries {
				got = append(got, map[string]any{"guid": e.GUID, "title": e.Title, "link": e.Link, "summary": e.Summary, "published_epoch": e.PublishedEpoch})
			}
			if want.Entries == nil {
				want.Entries = []any{}
			}
			eqVec(t, "entries", norm(t, got), norm(t, want.Entries))
			if n := rp.requestsLeft(); n != 0 {
				t.Errorf("%d recorded requests were never made", n)
			}
		})
	}
}

// recordedValidators: the Python recorder keeps only some response headers, but
// the recorded RESULT carries the validators the real response sent; replay them.
func recordedValidators(tr *transcript) map[string]string {
	var r struct {
		ETag         *string `json:"etag"`
		LastModified *string `json:"last_modified"`
	}
	_ = json.Unmarshal(tr.Result, &r)
	out := map[string]string{}
	if r.ETag != nil && *r.ETag != tr.Input["etag"] {
		out["ETag"] = *r.ETag
	}
	if r.LastModified != nil && *r.LastModified != tr.Input["last_modified"] {
		out["Last-Modified"] = *r.LastModified
	}
	return out
}

func deref(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func TestFetchFeedRedirectRefused(t *testing.T) {
	tr := &transcript{Exchanges: []tExchange{{Req: &tReq{Method: "GET"}, Resp: &tResp{Status: 302, Headers: map[string]string{"Location": "https://169.254.169.254/latest/meta-data"}}}}}
	rp := newReplay(t, tr)
	res, _ := FetchFeed(context.Background(), rp.deps(), "https://feeds.example.com/news.xml", "", "")
	if res.Error != "http_302" || res.Status != 302 {
		t.Errorf("%+v", res)
	}
	if rp.hits != 1 {
		t.Errorf("redirect was chased: %d requests", rp.hits)
	}
}

func TestFeedBlockedHostsRefused(t *testing.T) {
	for _, u := range []string{
		"http://127.0.0.1/feed.xml", "https://localhost/feed.xml", "http://169.254.169.254/latest/meta-data/",
		"http://[::1]/f", "http://[::ffff:127.0.0.1]/f", "http://0x7f.1/f", "http://2130706433/f", "file:///etc/passwd", "ftp://x.example.com/f",
		"http://metadata.google.internal/", "http://100.64.0.1/f", "http://10.0.0.5:8080/f",
	} {
		rp := newReplay(t, &transcript{})
		res, err := FetchFeed(context.Background(), rp.deps(), u, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(res.Error, "blocked_url") && !strings.Contains(res.Error, "blocked") {
			t.Errorf("%s: not refused: %+v", u, res)
		}
		if rp.hits != 0 || rp.dials != 0 {
			t.Errorf("%s: the network was touched (%d hits, %d dials)", u, rp.hits, rp.dials)
		}
	}
	// a hostname that resolves private is refused at dial time, before any connection
	rp := newReplay(t, &transcript{})
	rp.res.answers["feeds.example.com"] = []string{"192.168.1.9"}
	res, _ := FetchFeed(context.Background(), rp.deps(), "https://feeds.example.com/news.xml", "", "")
	if res.Error != "blocked_url: host resolves to private ip 192.168.1.9" || rp.dials != 0 {
		t.Errorf("%+v dials=%d", res, rp.dials)
	}
	// ANY private record blocks the whole fetch (mixed answers)
	rp = newReplay(t, &transcript{})
	rp.res.answers["feeds.example.com"] = []string{"93.184.216.34", "10.0.0.1"}
	if res, _ = FetchFeed(context.Background(), rp.deps(), "https://feeds.example.com/news.xml", "", ""); !strings.HasPrefix(res.Error, "blocked_url") || rp.dials != 0 {
		t.Errorf("mixed answers: %+v", res)
	}
	// rebinding: the name resolves public at first, private at the dial
	rp = newReplay(t, &transcript{})
	rp.res.answers["feeds.example.com"] = []string{"93.184.216.34"}
	rp.res.flip = map[string][]string{"feeds.example.com": {"127.0.0.1"}}
	rp.res.LookupNetIP(context.Background(), "ip", "feeds.example.com") // the "check" lookup
	if res, _ = FetchFeed(context.Background(), rp.deps(), "https://feeds.example.com/news.xml", "", ""); !strings.HasPrefix(res.Error, "blocked_url") || rp.dials != 0 {
		t.Errorf("rebinding: %+v dials=%d", res, rp.dials)
	}
}

func TestFeedBlockedNeverLeaksOtherHeaders(t *testing.T) {
	// the feed request carries only the poller headers, never cookies or auth
	var got http.Header
	tr := &transcript{Exchanges: []tExchange{{Req: &tReq{Method: "GET"}, Resp: &tResp{Status: 200, Body: "<rss/>"}}}}
	rp := newReplay(t, tr)
	rt := rp.client.Transport
	rp.client.Transport = roundTripFn(func(r *http.Request) (*http.Response, error) { got = r.Header.Clone(); return rt.RoundTrip(r) })
	if _, err := FetchFeed(context.Background(), rp.deps(), "https://feeds.example.com/x", `"e"`, "lm"); err != nil {
		t.Fatal(err)
	}
	for k := range got {
		switch k {
		case "User-Agent", "Accept", "If-None-Match", "If-Modified-Since":
		default:
			t.Errorf("unexpected request header %s", k)
		}
	}
}

type roundTripFn func(*http.Request) (*http.Response, error)

func (f roundTripFn) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// ── feed cursor keys (detector vectors) ─────────────────────────────────────

type detVec struct {
	Detector string         `json:"detector"`
	Name     string         `json:"name"`
	Watch    map[string]any `json:"watch"`
	Backfill bool           `json:"backfill"`
	Events   any            `json:"events"`
	Patch    any            `json:"patch"`
	// feed
	MaxEntries  int            `json:"max_entries"`
	FetchResult *vecFeedResult `json:"fetch_result"`
	FetcherArgs map[string]any `json:"fetcher_args"`
	// funding
	WantFunding    bool        `json:"want_funding"`
	WantExec       bool        `json:"want_exec"`
	Filings        []vecFiling `json:"filings"`
	ProviderRaises *string     `json:"provider_raises"`
	SincePassed    *string     `json:"since_passed"`
	// web tech
	Website        *string          `json:"website"`
	Flag           bool             `json:"flag"`
	ProviderResult json.RawMessage  `json:"provider_result"`
	ProviderCalls  []map[string]any `json:"provider_calls"`
}

type vecFeedResult struct {
	Status       int     `json:"status"`
	NotModified  bool    `json:"not_modified"`
	ETag         *string `json:"etag"`
	LastModified *string `json:"last_modified"`
	Error        *string `json:"error"`
	Entries      []struct {
		GUID           string  `json:"guid"`
		Title          string  `json:"title"`
		Link           string  `json:"link"`
		Summary        string  `json:"summary"`
		PublishedEpoch float64 `json:"published_epoch"`
	} `json:"entries"`
}

type vecFiling struct {
	CIK            string              `json:"cik"`
	Accession      string              `json:"accession"`
	Form           string              `json:"form"`
	FilingDate     string              `json:"filing_date"`
	Fields         map[string]any      `json:"fields"`
	RelatedPersons []map[string]string `json:"related_persons"`
}

func detectorVectors(t *testing.T, kind string) []detVec {
	var f struct {
		Vectors []detVec `json:"vectors"`
	}
	readJSON(t, "testdata/detector_vectors.json", &f)
	var out []detVec
	for _, v := range f.Vectors {
		if v.Detector == kind {
			out = append(out, v)
		}
	}
	return out
}

func TestFeedCursorKeys(t *testing.T) {
	vs := detectorVectors(t, "feed")
	if len(vs) < 19 {
		t.Fatalf("%d feed vectors", len(vs))
	}
	for _, v := range vs {
		v := v
		t.Run(v.Name, func(t *testing.T) {
			var gotArgs map[string]any
			d := SourceDeps{Deps: fixedDeps(), FeedMaxEntries: v.MaxEntries}
			d.FeedFetch = func(_ context.Context, _ SourceDeps, u, etag, lm string) (FeedResult, error) {
				gotArgs = map[string]any{"url": u, "etag": nilIfEmpty(etag), "last_modified": nilIfEmpty(lm)}
				fr := FeedResult{Status: v.FetchResult.Status, NotModified: v.FetchResult.NotModified, ETag: v.FetchResult.ETag, LastModified: v.FetchResult.LastModified}
				if v.FetchResult.Error != nil {
					fr.Error = *v.FetchResult.Error
				}
				for _, e := range v.FetchResult.Entries {
					fr.Entries = append(fr.Entries, FeedEntry{GUID: e.GUID, Title: e.Title, Link: e.Link, Summary: e.Summary, PublishedEpoch: e.PublishedEpoch})
				}
				return fr, nil
			}
			evs, patch, err := FetchFeedSource(context.Background(), d, watchFrom(v.Watch), v.Backfill)
			if v.Events == nil {
				if err == nil || evs != nil || patch != nil {
					t.Errorf("failure must return (nil, nil, err): %v %v %v", evs, patch, err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			eqVec(t, "fetcher args", gotArgs, v.FetcherArgs)
			eqVec(t, "events", eventsVec(t, evs), norm(t, v.Events))
			eqVec(t, "patch", patchVec(t, patch), norm(t, v.Patch))
		})
	}
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ── funding / exec ──────────────────────────────────────────────────────────

func TestFundingExecDetectorVectors(t *testing.T) {
	vs := detectorVectors(t, "funding_exec")
	if len(vs) < 26 {
		t.Fatalf("%d funding vectors", len(vs))
	}
	for _, v := range vs {
		v := v
		t.Run(v.Name, func(t *testing.T) {
			var gotSince *string
			d := SourceDeps{Deps: fixedDeps()}
			d.FormD = func(_ context.Context, _, since string) ([]firmo.SecFiling, error) {
				s := since
				gotSince = &s
				if v.ProviderRaises != nil {
					return nil, errors.New(*v.ProviderRaises)
				}
				var out []firmo.SecFiling
				for _, f := range v.Filings {
					out = append(out, firmo.SecFiling{CIK: f.CIK, Accession: strings.ReplaceAll(f.Accession, "-", ""), Form: f.Form, FilingDate: f.FilingDate, Fields: f.Fields, RelatedPersons: f.RelatedPersons})
				}
				return out, nil
			}
			// the vectors' accessions are dashed; keep them as recorded
			orig := d.FormD
			d.FormD = func(ctx context.Context, tgt, since string) ([]firmo.SecFiling, error) {
				out, err := orig(ctx, tgt, since)
				for i := range out {
					out[i].Accession = v.Filings[i].Accession
				}
				return out, err
			}
			evs, patch, err := FetchFundingAndExec(context.Background(), d, watchFrom(v.Watch), v.WantFunding, v.WantExec, v.Backfill)
			if v.Events == nil {
				if err == nil || evs != nil || patch != nil {
					t.Errorf("failure must return (nil, nil, err): %v %v %v", evs, patch, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if v.SincePassed != nil && (gotSince == nil || *gotSince != *v.SincePassed) {
				t.Errorf("since passed %v want %s", gotSince, *v.SincePassed)
			}
			eqVec(t, "events", eventsVec(t, evs), norm(t, v.Events))
			eqVec(t, "patch", patchVec(t, patch), norm(t, v.Patch))
		})
	}
}

func secTranscriptRun(t *testing.T, tr *transcript) ([]DetectedEvent, map[string]any, error) {
	t.Helper()
	rp := newReplay(t, tr)
	d := rp.deps()
	w := WatchFrom(tr.Input)
	evs, patch, err := FetchFundingAndExec(context.Background(), d, w, tr.Input["want_funding"] == true, tr.Input["want_exec"] == true, tr.Input["backfill"] == true)
	if n := rp.requestsLeft(); n != 0 {
		t.Errorf("%s: %d recorded requests never made", tr.file, n)
	}
	return evs, patch, err
}

// WatchFrom builds a WatchView from a transcript/vector input.
func WatchFrom(in map[string]any) WatchView { return watchFrom(in) }

func TestFundingExecTranscripts(t *testing.T) {
	for _, tr := range loadTranscripts(t, "sec_funding", 21) {
		tr := tr
		t.Run(tr.file, func(t *testing.T) {
			evs, patch, err := secTranscriptRun(t, tr)
			if err != nil {
				t.Fatal(err)
			}
			var want struct {
				Events any `json:"events"`
				Patch  any `json:"patch"`
			}
			if err := json.Unmarshal(tr.Result, &want); err != nil {
				t.Fatal(err)
			}
			eqVec(t, "events", eventsVec(t, evs), norm(t, want.Events))
			eqVec(t, "patch", patchVec(t, patch), norm(t, want.Patch))
		})
	}
}

func TestFundingExecSharedWatermark(t *testing.T) {
	// one filing batch feeds both detectors; the watermark is the newest accession
	// and a funding-only poll must still record the executives it saw
	var tr *transcript
	for _, c := range loadTranscripts(t, "sec_funding", 21) {
		if c.Scenario == "two_new_executives" {
			tr = c
		}
	}
	if tr == nil {
		t.Fatal("two_new_executives transcript missing")
	}
	evs, patch, err := secTranscriptRun(t, tr)
	if err != nil {
		t.Fatal(err)
	}
	var funded, execs int
	for _, e := range evs {
		switch e.SignalType {
		case "company_funded":
			funded++
		case "executive_hired":
			execs++
		}
	}
	if funded != 1 || execs < 1 {
		t.Errorf("funded=%d execs=%d", funded, execs)
	}
	known, _ := patch["sec_known_execs"].([]string)
	if len(known) != execs {
		t.Errorf("sec_known_execs %v vs %d exec events", known, execs)
	}
	// want_exec only: the funding events vanish but the watermark still advances
	d := SourceDeps{Deps: fixedDeps(), FormD: func(context.Context, string, string) ([]firmo.SecFiling, error) {
		return []firmo.SecFiling{
			{CIK: "0000000001", Accession: "0000000001-24-000002", FilingDate: "2024-05-01", Fields: map[string]any{"funding_amount": int64(10)}, RelatedPersons: []map[string]string{{"name": "A B", "title": "CEO"}}},
			{CIK: "0000000001", Accession: "0000000001-24-000001", FilingDate: "2024-04-01", RelatedPersons: []map[string]string{{"name": "C D", "title": "Director"}}},
		}, nil
	}}
	w := WatchView{Target: "sec_cik:1", Cursor: map[string]any{"bootstrapped": true}}
	evs, patch, err = FetchFundingAndExec(context.Background(), d, w, false, true, false)
	if err != nil || len(evs) != 2 || patch["sec_last_accession"] != "0000000001-24-000002" {
		t.Fatalf("exec only: %d events, patch %v, %v", len(evs), patch, err)
	}
	for _, e := range evs {
		if e.SignalType != "executive_hired" {
			t.Errorf("funding leaked into exec-only poll: %+v", e)
		}
	}
	if k := patch["sec_known_execs"].([]string); len(k) != 2 {
		t.Errorf("known execs %v", k)
	}
}

func TestCIKResolvedSideChannel(t *testing.T) {
	filing := func(cik string) []firmo.SecFiling {
		return []firmo.SecFiling{{CIK: cik, Accession: "0000000001-24-000001", FilingDate: "2024-05-01"}}
	}
	run := func(resolved string, fs []firmo.SecFiling) map[string]any {
		d := SourceDeps{Deps: fixedDeps(), FormD: func(context.Context, string, string) ([]firmo.SecFiling, error) { return fs, nil }}
		_, patch, err := FetchFundingAndExec(context.Background(), d, WatchView{Target: "Stripe", ResolvedCIK: resolved, Cursor: map[string]any{"bootstrapped": true}}, true, true, false)
		if err != nil {
			t.Fatal(err)
		}
		return patch
	}
	if p := run("", filing("0000000001")); p["_resolved_cik"] != "0000000001" {
		t.Errorf("unresolved watch: %v", p)
	}
	if p := run("0000000001", filing("0000000001")); p["_resolved_cik"] != nil {
		t.Errorf("already resolved must not re-emit the side channel: %v", p)
	}
	if p := run("", nil); p["_resolved_cik"] != nil {
		t.Errorf("no filings, nothing to resolve from: %v", p)
	}
}

// ── web tech ────────────────────────────────────────────────────────────────

func TestWebTechTranscript(t *testing.T) {
	for _, tr := range loadTranscripts(t, "webtech", 14) {
		tr := tr
		t.Run(tr.file, func(t *testing.T) {
			rp := newReplay(t, tr)
			d := rp.deps()
			d.Cfg.WebsiteTech = tr.Input["flag"] == true
			website, _ := tr.Input["website"].(string)
			evs, patch, err := FetchWebTech(context.Background(), d, WatchFrom(tr.Input), website, tr.Input["backfill"] == true)
			var want struct {
				Events any `json:"events"`
				Patch  any `json:"patch"`
			}
			if err := json.Unmarshal(tr.Result, &want); err != nil {
				t.Fatal(err)
			}
			if want.Events == nil {
				if err == nil || evs != nil || patch != nil {
					t.Errorf("failure must return (nil, nil, err): %v %v %v", evs, patch, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			eqVec(t, "events", eventsVec(t, evs), norm(t, want.Events))
			eqVec(t, "patch", patchVec(t, patch), norm(t, want.Patch))
		})
	}
}

type stubTech struct {
	out   providers.Output
	err   error
	calls []map[string]any
}

func (s *stubTech) Name() string { return "tech_stack" }
func (s *stubTech) Enrich(_ context.Context, _ *http.Client, _ func() (string, error), in providers.Input) (providers.Output, error) {
	s.calls = append(s.calls, map[string]any{"company": in.Lead["company"], "website": in.Lead["website"]})
	return s.out, s.err
}

func TestWebTechDetectorVectors(t *testing.T) {
	vs := detectorVectors(t, "web_tech")
	if len(vs) < 23 {
		t.Fatalf("%d web_tech vectors", len(vs))
	}
	for _, v := range vs {
		v := v
		t.Run(v.Name, func(t *testing.T) {
			stub := &stubTech{}
			var pr *struct {
				Success bool           `json:"success"`
				Fields  map[string]any `json:"fields"`
				Error   *string        `json:"error"`
			}
			if string(v.ProviderResult) != "null" && len(v.ProviderResult) > 0 {
				pr = &struct {
					Success bool           `json:"success"`
					Fields  map[string]any `json:"fields"`
					Error   *string        `json:"error"`
				}{}
				if err := json.Unmarshal(v.ProviderResult, pr); err != nil {
					t.Fatal(err)
				}
			}
			switch {
			case v.ProviderRaises != nil:
				stub.err = errors.New(*v.ProviderRaises)
			case pr == nil:
				stub.err = errors.New("provider returned None")
			default:
				o := providers.Output{}
				if !pr.Success {
					o.Error = "unsuccessful"
					if pr.Error != nil {
						o.Error = *pr.Error
					}
				}
				if pr.Fields != nil {
					o.Extra = providers.NewObj()
					for k, x := range pr.Fields {
						o.Extra.Set(k, x)
					}
				}
				stub.out = o
			}
			d := SourceDeps{Deps: fixedDeps(), TechStack: stub}
			d.Cfg.WebsiteTech = v.Flag
			website := ""
			if v.Website != nil {
				website = *v.Website
			}
			evs, patch, err := FetchWebTech(context.Background(), d, watchFrom(v.Watch), website, v.Backfill)
			if v.Events == nil {
				if err == nil || evs != nil || patch != nil {
					t.Errorf("failure must return (nil, nil, err): %v %v %v", evs, patch, err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			eqVec(t, "events", eventsVec(t, evs), norm(t, v.Events))
			eqVec(t, "patch", patchVec(t, patch), norm(t, v.Patch))
			want := v.ProviderCalls
			if want == nil {
				want = []map[string]any{}
			}
			got := stub.calls
			if got == nil {
				got = []map[string]any{}
			}
			eqVec(t, "provider calls", norm(t, got), norm(t, want))
		})
	}
}

// ── bootstrap / backfill, failure, rate limit ───────────────────────────────

func TestBackfillFlag(t *testing.T) {
	filings := func(context.Context, string, string) ([]firmo.SecFiling, error) {
		return []firmo.SecFiling{{CIK: "0000000001", Accession: "0000000001-24-000001", FilingDate: "2024-05-01",
			Fields: map[string]any{"funding_amount": int64(5)}, RelatedPersons: []map[string]string{{"name": "A", "title": "CEO"}}}}, nil
	}
	feed := func(context.Context, SourceDeps, string, string, string) (FeedResult, error) {
		return FeedResult{Status: 200, Entries: []FeedEntry{{GUID: "g1", PublishedEpoch: 100}}}, nil
	}
	for _, c := range []struct {
		bootstrapped, backfill bool
		wantEvents             bool
	}{{false, false, false}, {false, true, true}, {true, false, true}, {true, true, true}} {
		cur := map[string]any{}
		if c.bootstrapped {
			cur["bootstrapped"] = true
		}
		w := WatchView{Target: "https://f.example.com/x", Cursor: cur}
		d := SourceDeps{Deps: fixedDeps(), FormD: filings, FeedFetch: feed}
		evs, patch, err := FetchFundingAndExec(context.Background(), d, w, true, true, c.backfill)
		if err != nil || (len(evs) > 0) != c.wantEvents {
			t.Errorf("funding %+v: %d events, %v", c, len(evs), err)
		}
		if patch["sec_last_accession"] == nil || len(patch["sec_known_execs"].([]string)) != 1 {
			t.Errorf("funding %+v: state must be recorded even when suppressed: %v", c, patch)
		}
		evs, patch, err = FetchFeedSource(context.Background(), d, w, c.backfill)
		if err != nil || (len(evs) > 0) != c.wantEvents {
			t.Errorf("feed %+v: %d events, %v", c, len(evs), err)
		}
		if g := patch["feed_seen_guids"].([]string); len(g) != 1 {
			t.Errorf("feed %+v: seen %v", c, g)
		}
	}
}

func TestFetchFailureReturnsNoPatch(t *testing.T) {
	boom := errors.New("boom")
	d := SourceDeps{Deps: fixedDeps(),
		FormD: func(context.Context, string, string) ([]firmo.SecFiling, error) { return nil, boom },
		FeedFetch: func(context.Context, SourceDeps, string, string, string) (FeedResult, error) {
			return FeedResult{Error: "http_500", Status: 500}, nil
		},
		TechStack: &stubTech{err: boom},
	}
	d.Cfg.WebsiteTech = true
	w := WatchView{Target: "Acme", Cursor: map[string]any{"bootstrapped": true, "sec_last_accession": "x", "feed_etag": "e"}}
	if evs, p, err := FetchFundingAndExec(context.Background(), d, w, true, true, false); err == nil || evs != nil || p != nil {
		t.Errorf("funding: %v %v %v", evs, p, err)
	}
	if evs, p, err := FetchFeedSource(context.Background(), d, w, false); err == nil || evs != nil || p != nil {
		t.Errorf("feed: %v %v %v", evs, p, err)
	}
	if evs, p, err := FetchWebTech(context.Background(), d, w, "https://a.example.com", false); err == nil || evs != nil || p != nil {
		t.Errorf("web tech: %v %v %v", evs, p, err)
	}
	// a billing/ctx cancellation is an error too, never a partial patch
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d.FeedFetch = nil
	rp := newReplay(t, &transcript{})
	d.Client = rp.client
	if evs, p, err := FetchFeedSource(ctx, d, w, false); err == nil || evs != nil || p != nil {
		t.Errorf("cancelled feed: %v %v %v", evs, p, err)
	}
}

func TestPerHostRateLimit(t *testing.T) {
	var mu sync.Mutex
	clock := time.Unix(1000, 0)
	var sleeps []time.Duration
	sp := &HostSpacer{
		Interval: time.Second,
		Now:      func() time.Time { mu.Lock(); defer mu.Unlock(); return clock },
		Sleep: func(_ context.Context, d time.Duration) error {
			mu.Lock()
			defer mu.Unlock()
			sleeps = append(sleeps, d)
			clock = clock.Add(d)
			return nil
		},
	}
	ctx := context.Background()
	for _, h := range []string{"a.example.com", "a.example.com", "b.example.com", "a.example.com"} {
		if err := sp.Wait(ctx, h); err != nil {
			t.Fatal(err)
		}
	}
	// 1st a: no wait; 2nd a: 1s; b: none (other host); 3rd a: the clock already moved 1s
	if len(sleeps) != 2 || sleeps[0] != time.Second || math.Abs(sleeps[1].Seconds()-1) > 1e-9 {
		t.Errorf("sleeps %v", sleeps)
	}
	// a cancelled context aborts the wait
	sp2 := NewHostSpacer(time.Hour)
	_ = sp2.Wait(ctx, "x")
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := sp2.Wait(cctx, "x"); err == nil {
		t.Error("cancelled wait must fail")
	}
	// FetchFeed waits through the spacer: two fetches of one host sleep once
	var slept int
	sp3 := &HostSpacer{Interval: time.Second, Now: func() time.Time { return time.Unix(5, 0) }, Sleep: func(context.Context, time.Duration) error { slept++; return nil }}
	tr := &transcript{Exchanges: []tExchange{
		{Req: &tReq{Method: "GET"}, Resp: &tResp{Status: 200, Body: "<rss/>"}},
		{Req: &tReq{Method: "GET"}, Resp: &tResp{Status: 200, Body: "<rss/>"}},
	}}
	rp := newReplay(t, tr)
	d := rp.deps()
	d.Spacer = sp3
	for i := 0; i < 2; i++ {
		if _, err := FetchFeed(ctx, d, "https://feeds.example.com/x", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if slept != 1 {
		t.Errorf("slept %d times, want 1", slept)
	}
}

func TestEpochFromDate(t *testing.T) {
	d := SourceDeps{Deps: fixedDeps()}
	now := float64(fixedNow.Unix())
	for in, want := range map[string]float64{
		"2024-05-01": 1714521600, "2024-5-1": 1714521600, " 2024-05-01 ": 1714521600, "": now, "not-a-date": now,
		"2024-02-30": now, "2024-13-01": now, "2024-05-01x": now, "0000-01-01": now,
	} {
		if got := d.epochFromDate(in); got != want {
			t.Errorf("epochFromDate(%q) = %v want %v", in, got, want)
		}
	}
}

func TestFormatAmount(t *testing.T) {
	for in, want := range map[any]string{int64(5000000): "5,000,000", float64(1250000.5): "1,250,000", "3000000": "3,000,000", json.Number("123456789012345678901"): "123,456,789,012,345,678,901", int64(999): "999", int64(1000): "1,000"} {
		if got, err := formatAmount(in); err != nil || got != want {
			t.Errorf("formatAmount(%v) = %q, %v want %q", in, got, err, want)
		}
	}
	for _, bad := range []any{math.NaN(), "12.5", []any{}} {
		if _, err := formatAmount(bad); err == nil {
			t.Errorf("formatAmount(%v) must fail", bad)
		}
	}
}

func TestParseFeedEntityExpansionBounded(t *testing.T) {
	probe := func(refs int) []byte {
		ent := strings.Repeat("x", 4000)
		return []byte(`<?xml version="1.0"?><!DOCTYPE r [<!ENTITY a "` + ent + `">]><rss><item><title>` +
			strings.Repeat("&a;", refs) + `</title><guid>1</guid></item></rss>`)
	}
	start := time.Now()
	for _, refs := range []int{20000, 340000} { // 80 MB and the 1.36 GB reference probe
		got, err := ParseFeed(probe(refs), 10)
		if err == nil || len(got) != 0 {
			t.Fatalf("%d refs: want a malformed-feed error, got %d entries, err %v", refs, len(got), err)
		}
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("rejection took %s", d)
	}
	// a small legitimate expansion still works
	got, err := ParseFeed(probe(3), 10)
	if err != nil || len(got) != 1 || len(got[0].Title) != 12000 {
		t.Errorf("small expansion: %v %d", err, len(got))
	}
}

func TestParseFeedTextAndDepthCaps(t *testing.T) {
	// many CDATA tokens in one node: linear time, text capped per node
	big := "<rss><item><title>" + strings.Repeat("<![CDATA[x]]>", 80000) + "</title><guid>1</guid></item></rss>"
	start := time.Now()
	got, err := ParseFeed([]byte(big), 10)
	if err != nil || len(got) != 1 || len(got[0].Title) > maxNodeText {
		t.Fatalf("cdata: %v %d", err, len(got))
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("cdata parse took %s", time.Since(start))
	}
	deep := strings.Repeat("<a>", maxFeedDepth+5) + strings.Repeat("</a>", maxFeedDepth+5)
	if _, err := ParseFeed([]byte(deep), 10); err == nil {
		t.Error("over-deep document must be rejected")
	}
}

func TestParseFeedMultiDirectiveExpansionBounded(t *testing.T) {
	ent := strings.Repeat("x", 4000)
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0"?>`)
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&sb, `<!DOCTYPE r [<!ENTITY a%d "%s">]>`, i, ent)
	}
	sb.WriteString("<rss><item><title>")
	for i := 0; i < 400; i++ {
		sb.WriteString(strings.Repeat(fmt.Sprintf("&a%d;", i), 100))
	}
	sb.WriteString("</title><guid>1</guid></item></rss>")
	start := time.Now()
	got, err := ParseFeed([]byte(sb.String()), 10)
	if err == nil || len(got) != 0 || time.Since(start) > 3*time.Second {
		t.Fatalf("multi-directive probe: %d entries, err %v, %s", len(got), err, time.Since(start))
	}
	// one DOCTYPE only; more than 64 declarations in total is refused
	two := `<!DOCTYPE r [<!ENTITY a "1">]><!DOCTYPE r [<!ENTITY b "2">]><rss><item><guid>1</guid></item></rss>`
	if _, err := ParseFeed([]byte(two), 10); err == nil {
		t.Error("second DOCTYPE must be rejected")
	}
	var decls strings.Builder
	decls.WriteString("<!DOCTYPE r [")
	for i := 0; i < 65; i++ {
		fmt.Fprintf(&decls, `<!ENTITY e%d "v">`, i)
	}
	decls.WriteString("]><rss><item><guid>1</guid></item></rss>")
	if _, err := ParseFeed([]byte(decls.String()), 10); err == nil {
		t.Error("65 entity declarations must be rejected")
	}
}
