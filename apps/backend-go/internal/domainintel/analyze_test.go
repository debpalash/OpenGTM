package domainintel

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
)

type vectorCase struct {
	Name       string          `json:"name"`
	Domain     string          `json:"domain"`
	Transcript transcript      `json:"transcript"`
	Expected   json.RawMessage `json:"expected"`
	Error      *string         `json:"error"`
	Requests   [][]string      `json:"requests"`
}

type transcript struct {
	RDAP json.RawMessage            `json:"rdap"`
	DoH  map[string]json.RawMessage `json:"doh"`
	CRT  []json.RawMessage          `json:"crt"`
}

type vectorFile struct {
	Clock       string       `json:"clock"`
	Cases       []vectorCase `json:"cases"`
	CleanDomain []struct {
		In  any    `json:"in"`
		Out string `json:"out"`
	} `json:"clean_domain"`
}

func loadVectors(t *testing.T) vectorFile {
	t.Helper()
	raw, err := os.ReadFile("testdata/analyze_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectorFile
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func testNow(t *testing.T) time.Time {
	t.Helper()
	tm, err := time.Parse("2006-01-02T15:04:05", loadVectors(t).Clock)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func resp(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Status: fmt.Sprint(status), Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}
}

// replay answers requests from a recorded transcript, exactly like the Python recorder's fakes.
type replay struct {
	mu   sync.Mutex
	tr   transcript
	crtN int
	log  [][]string
}

func (r *replay) answer(raw json.RawMessage, wantText bool) (*http.Response, error) {
	if string(raw) == `"error"` {
		return nil, errors.New("transcript: connection error")
	}
	var a struct {
		Status int             `json:"status"`
		JSON   json.RawMessage `json:"json"`
		Text   *string         `json:"text"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	body := ""
	switch {
	case a.Text != nil:
		body = *a.Text
	case len(a.JSON) > 0 && string(a.JSON) != "null":
		body = string(a.JSON)
		if body == `"<invalid json>"` {
			body = "<invalid json"
		}
	}
	return resp(a.Status, body), nil
}

func (r *replay) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	u := req.URL
	switch {
	case u.Host == "rdap.org":
		r.log = append(r.log, []string{"GET", "https://rdap.org" + u.EscapedPath()})
		return r.answer(r.tr.RDAP, false)
	case u.Host == "cloudflare-dns.com" && u.Path == "/dns-query":
		q := u.Query()
		r.log = append(r.log, []string{"GET", "https://cloudflare-dns.com/dns-query", q.Get("name"), q.Get("type"), req.Header.Get("Accept")})
		if raw, ok := r.tr.DoH[q.Get("type")+" "+q.Get("name")]; ok {
			return r.answer(raw, false)
		}
		return resp(200, "{}"), nil
	case u.Host == "crt.sh":
		r.log = append(r.log, []string{"GET", "https://crt.sh" + u.RequestURI(), "crt", req.Header.Get("User-Agent")})
		seq := r.tr.CRT
		raw := seq[min(r.crtN, len(seq)-1)]
		r.crtN++
		if string(raw) != `"error"` {
			// httpx: text defaults to json.dumps(json); a null/absent json gives "".
			var a struct {
				Text *string         `json:"text"`
				JSON json.RawMessage `json:"json"`
			}
			_ = json.Unmarshal(raw, &a)
			if a.Text == nil && len(a.JSON) == 0 {
				return resp(statusOf(raw), ""), nil
			}
		}
		return r.answer(raw, true)
	}
	return nil, fmt.Errorf("unexpected host %q", u.Host)
}

func statusOf(raw json.RawMessage) int {
	var a struct {
		Status int `json:"status"`
	}
	_ = json.Unmarshal(raw, &a)
	return a.Status
}

func testAnalyzer(c *http.Client, now time.Time) *analyzer {
	a := newAnalyzer(c)
	a.now = func() time.Time { return now }
	a.sleep = func(context.Context, time.Duration) {}
	return a
}

func compactJSON(t *testing.T, b []byte) string {
	t.Helper()
	var out bytes.Buffer
	if err := json.Compact(&out, b); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	return out.String()
}

func sortedReqs(reqs [][]string) []string {
	out := make([]string, len(reqs))
	for i, r := range reqs {
		b, _ := json.Marshal(r)
		out[i] = string(b)
	}
	sort.Strings(out)
	return out
}

func TestAnalyzeTranscripts(t *testing.T) {
	vec := loadVectors(t)
	if len(vec.Cases) < 40 {
		t.Fatalf("only %d recorded analyses", len(vec.Cases))
	}
	now := testNow(t)
	for _, c := range vec.Cases {
		t.Run(c.Name, func(t *testing.T) {
			rp := &replay{tr: c.Transcript}
			a := testAnalyzer(&http.Client{Transport: rp}, now)
			res, err := a.analyze(context.Background(), c.Domain)
			if c.Error != nil {
				// Python raises (route answers 500); Go returns an error instead of crashing.
				if err == nil || res != nil {
					t.Fatalf("want an error for Python %s, got %v / %v", *c.Error, res, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			b, err := res.MarshalJSON()
			if err != nil {
				t.Fatal(err)
			}
			if got, want := compactJSON(t, b), compactJSON(t, c.Expected); got != want {
				t.Fatalf("result differs\n got: %s\nwant: %s", got, want)
			}
			want, got := sortedReqs(c.Requests), sortedReqs(rp.log)
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("requests differ\n got: %v\nwant: %v", got, want)
			}
		})
	}
}

// TestAnalyzeNeverContactsUserHost: the user domain only ever appears in query values and the RDAP/crt.sh
// request targets, never as a request host; a hostile domain is refused before any request is made.
func TestAnalyzeNeverContactsUserHost(t *testing.T) {
	var mu sync.Mutex
	hosts := map[string]bool{}
	rt := rtFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		hosts[r.URL.Host] = true
		mu.Unlock()
		if r.URL.Scheme != "https" {
			t.Errorf("non-https request %s", r.URL)
		}
		return resp(200, "{}"), nil
	})
	a := testAnalyzer(&http.Client{Transport: rt}, time.Now())
	for _, d := range []string{"victim.example", "169.254.169.254", "xn--bcher-kva.de", "a.b.c.d.e.f.example.org"} {
		if _, err := a.analyze(context.Background(), d); err != nil {
			t.Fatalf("%s: %v", d, err)
		}
	}
	for h := range hosts {
		if h != "cloudflare-dns.com" && h != "rdap.org" && h != "crt.sh" {
			t.Errorf("unexpected request host %q", h)
		}
	}
	var calls atomic.Int32
	rt2 := rtFunc(func(*http.Request) (*http.Response, error) { calls.Add(1); return resp(200, "{}"), nil })
	a2 := testAnalyzer(&http.Client{Transport: rt2}, time.Now())
	long := strings.Repeat("a", 64) + ".com"
	for _, d := range []string{"", "http://evil.test/", "evil.test/path", "evil.test:8080", "EVIL.test", "user@evil.test", "a b.com", "evil.test\n", "[::1]", "localhost", "www.evil.test.", long,
		strings.Repeat("a.", 130) + "com", "xn--ü.com", "evil.test#x", "evil.test?x=1", "a..b.com", "-a.com", "_dmarc.evil.test."} {
		res, err := a2.analyze(context.Background(), d)
		if !errors.Is(err, ErrInvalidDomain) || res != nil {
			t.Errorf("analyze(%q) = %v, %v; want ErrInvalidDomain", d, res, err)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("%d requests were made for invalid domains", calls.Load())
	}
	// A hostile domain value must not alter the DoH query structure: it is rejected, never escaped through.
	if validDomain("evil.test&type=A") || validDomain("evil.test/../x") {
		t.Fatal("validDomain accepted a request-smuggling shape")
	}
}

// TestAnalyzeRedirectPolicy: RDAP (rdap.org is a bootstrap redirector) may follow https redirects to a
// registry; DoH and crt.sh only within their own host; plain-http and over-long chains are refused.
func TestAnalyzeRedirectPolicy(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	redir := func(to string) *http.Response {
		r := resp(302, "")
		r.Header.Set("Location", to)
		return r
	}
	rt := rtFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		seen = append(seen, r.URL.String())
		mu.Unlock()
		switch r.URL.String() {
		case "https://rdap.org/domain/example.com":
			return redir("https://rdap.registry.test/domain/example.com"), nil
		case "https://rdap.registry.test/domain/example.com":
			return resp(200, `{"status":["active"]}`), nil
		}
		if r.URL.Host == "cloudflare-dns.com" {
			return redir("https://elsewhere.test/x"), nil
		}
		if r.URL.Host == "crt.sh" {
			return redir("http://crt.sh/insecure"), nil
		}
		t.Errorf("unexpected %s", r.URL)
		return resp(500, ""), nil
	})
	a := testAnalyzer(&http.Client{Transport: rt}, time.Now())
	res, err := a.analyze(context.Background(), "example.com")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(res)
	if !strings.Contains(string(b), `"status":["active"]`) {
		t.Fatalf("RDAP redirect to the registry was not followed: %s", b)
	}
	for _, u := range seen {
		if strings.Contains(u, "elsewhere.test") || strings.HasPrefix(u, "http://") {
			t.Errorf("followed a forbidden redirect: %s", u)
		}
	}
	// A redirect loop terminates.
	loop := rtFunc(func(r *http.Request) (*http.Response, error) {
		return redir("https://rdap.org/domain/example.com"), nil
	})
	if _, err := testAnalyzer(&http.Client{Transport: loop}, time.Now()).analyze(context.Background(), "example.com"); err != nil {
		t.Fatal(err)
	}
}

// TestAnalyzeRedirectToPrivateHostBlockedByEgress wires the real egress dialer: a redirect from rdap.org to a
// loopback server must be refused at dial time, so the server never sees a request.
func TestAnalyzeRedirectToPrivateHostBlockedByEgress(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, `{"status":["leaked"]}`)
	}))
	defer srv.Close()
	host, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	_ = host
	base := &http.Transport{DialContext: egress.NewDialContext("production", nil), TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // test: the dial guard is what is under test
	rt := rtFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "rdap.org" {
			resp := resp(302, "")
			resp.Header.Set("Location", "https://127.0.0.1:"+port+"/domain/example.com")
			return resp, nil
		}
		return base.RoundTrip(r)
	})
	a := testAnalyzer(&http.Client{Transport: rt}, time.Now())
	res, err := a.analyze(context.Background(), "example.com")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(res)
	if hits.Load() != 0 || strings.Contains(string(b), "leaked") {
		t.Fatalf("redirect reached a loopback server (hits=%d): %s", hits.Load(), b)
	}
}

func TestAnalyzeDeadline(t *testing.T) {
	block := rtFunc(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	a := testAnalyzer(&http.Client{Transport: block}, time.Now())
	a.deadline = 150 * time.Millisecond
	start := time.Now()
	res, err := a.analyze(context.Background(), "example.com")
	if err != nil {
		t.Fatalf("timeouts are swallowed like Python: %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("deadline not enforced: %v", d)
	}
	b, _ := json.Marshal(res)
	if !strings.Contains(string(b), `"legitimacy_score":0`) {
		t.Fatalf("%s", b)
	}
	// A cancelled caller context is an error, not a default result.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := testAnalyzer(&http.Client{Transport: block}, time.Now()).analyze(ctx, "example.com"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if defaultDeadline != 15*time.Second {
		t.Fatalf("overall deadline = %v", defaultDeadline)
	}
}

func TestAnalyzeConcurrencyBounded(t *testing.T) {
	var cur, peak atomic.Int32
	rt := rtFunc(func(r *http.Request) (*http.Response, error) {
		n := cur.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		cur.Add(-1)
		return resp(200, "{}"), nil
	})
	a := testAnalyzer(&http.Client{Transport: rt}, time.Now())
	if _, err := a.analyze(context.Background(), "example.com"); err != nil {
		t.Fatal(err)
	}
	if p := peak.Load(); p > 5 || p < 2 {
		t.Fatalf("peak concurrency %d, want 2..5", p)
	}
}

func TestAnalyzeBodyCaps(t *testing.T) {
	big := strings.Repeat(" ", 16<<20) + "{}"
	var read atomic.Int64
	rt := rtFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(io.TeeReader(strings.NewReader(big), countWriter{&read}))}, nil
	})
	a := testAnalyzer(&http.Client{Transport: rt}, time.Now())
	res, err := a.analyze(context.Background(), "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if read.Load() > 7*(9<<20) {
		t.Fatalf("read %d bytes across 7 requests; bodies are not capped", read.Load())
	}
	b, _ := json.Marshal(res)
	if !strings.Contains(string(b), `"legitimacy_score":0`) {
		t.Fatalf("%s", b)
	}
}

type countWriter struct{ n *atomic.Int64 }

func (c countWriter) Write(p []byte) (int, error) { c.n.Add(int64(len(p))); return len(p), nil }

// TestRDAPDuckTyping pins the Python behaviour for malformed-but-valid JSON shapes: the first Python
// exception aborts the parse and keeps whatever was already stored in the result.
func TestRDAPDuckTyping(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct{ name, body, want string }{
		{"entities null", `{"entities":null,"events":[{"eventAction":"registration","eventDate":"2020-01-01"}]}`,
			`{"registrar":"","registration_date":"","expiration_date":"","domain_age_days":0,"nameservers":[],"status":[]}`},
		{"registrar kept after later abort", `{"entities":[{"roles":["registrar"],"vcardArray":["v",[["fn",{},"text","R"]]]}],"events":[{"eventAction":"registration","eventDate":5}],"status":["x"]}`,
			`{"registrar":"R","registration_date":"","expiration_date":"","domain_age_days":0,"nameservers":[],"status":[]}`},
		{"roles substring", `{"entities":[{"roles":"xregistrarx","vcardArray":["v",[["fn",{},"text","S"]]]}]}`,
			`{"registrar":"S","registration_date":"","expiration_date":"","domain_age_days":0,"nameservers":[],"status":[]}`},
		{"status null", `{"status":null}`, `{"registrar":"","registration_date":"","expiration_date":"","domain_age_days":0,"nameservers":[],"status":null}`},
		{"nameserver not object aborts comprehension", `{"nameservers":[{"ldhName":"a"},3],"status":["z"]}`,
			`{"registrar":"","registration_date":"","expiration_date":"","domain_age_days":0,"nameservers":[],"status":[]}`},
		{"fn value any type", `{"entities":[{"roles":["registrar"],"vcardArray":["v",[["fn",{},"text",42]]]}]}`,
			`{"registrar":42,"registration_date":"","expiration_date":"","domain_age_days":0,"nameservers":[],"status":[]}`},
		{"later registrar overrides", `{"entities":[{"roles":["registrar"],"vcardArray":["v",[["fn",{},"text","A"]]]},{"roles":["registrar"],"vcardArray":["v",[["fn",{},"text","B"]]]}]}`,
			`{"registrar":"B","registration_date":"","expiration_date":"","domain_age_days":0,"nameservers":[],"status":[]}`},
		{"date list slice", `{"events":[{"eventAction":"registration","eventDate":["a","b"]}]}`,
			`{"registrar":"","registration_date":["a","b"],"expiration_date":"","domain_age_days":0,"nameservers":[],"status":[]}`},
		{"date slices by code point", `{"events":[{"eventAction":"expiration","eventDate":"20301234é6789012"}]}`,
			`{"registrar":"","registration_date":"","expiration_date":"20301234é6","domain_age_days":0,"nameservers":[],"status":[]}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			rt := rtFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host == "rdap.org" {
					return resp(200, c.body), nil
				}
				return resp(200, "{}"), nil
			})
			res, err := testAnalyzer(&http.Client{Transport: rt}, now).analyze(context.Background(), "example.com")
			if err != nil {
				t.Fatal(err)
			}
			rd, _ := res.Get("rdap")
			b, _ := json.Marshal(rd)
			if got := compactJSON(t, b); got != c.want {
				t.Fatalf("\n got: %s\nwant: %s", got, c.want)
			}
		})
	}
}
