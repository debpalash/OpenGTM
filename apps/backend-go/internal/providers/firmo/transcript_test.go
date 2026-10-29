package firmo

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// Transcripts are recorded from the real Python providers (Task 1): request
// construction, response handling and result are Python's; vendor bodies are
// hand-written fixtures of each vendor's documented shape.

type exchange struct {
	DNS *struct {
		Host    string   `json:"host"`
		Answers []string `json:"answers"`
		Error   string   `json:"error"`
	} `json:"dns"`
	Request *struct {
		Method  string            `json:"method"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
	} `json:"request"`
	Response struct {
		Status  int               `json:"status"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
		Raises  string            `json:"raises"`
		Message string            `json:"message"`
	} `json:"response"`
}

type transcript struct {
	Provider  string            `json:"provider"`
	Scenario  string            `json:"scenario"`
	Input     map[string]any    `json:"input"`
	Secrets   map[string]string `json:"secrets"`
	Config    map[string]any    `json:"config"`
	Exchanges []exchange        `json:"http_exchanges"`
	Result    json.RawMessage   `json:"result"`
	file      string
}

var firmoNames = []string{"ipinfo", "gleif", "companies_house", "mca_registry", "wikidata", "sec_edgar",
	"jsonld_firmographics", "tech_stack", "ats_hiring", "google_maps"}

type timeoutErr struct{ msg string }

func (e timeoutErr) Error() string   { return e.msg }
func (e timeoutErr) Timeout() bool   { return true }
func (e timeoutErr) Temporary() bool { return true }

// replay serves a transcript in order. DNS exchanges are consumed by the
// ipinfo resolver hook; for the other providers (whose Python resolved the host
// in url_guard) the egress guard does that at dial time, so DNS entries are
// skipped.
type replay struct {
	t    *testing.T
	tr   *transcript
	next int
}

func (r *replay) skipDNS() {
	for r.next < len(r.tr.Exchanges) && r.tr.Exchanges[r.next].DNS != nil {
		r.next++
	}
}

func (r *replay) lookup(_ context.Context, host string) (string, error) {
	if r.next >= len(r.tr.Exchanges) || r.tr.Exchanges[r.next].DNS == nil {
		r.t.Errorf("%s: unexpected DNS lookup of %s", r.tr.file, host)
		return "", errors.New("unexpected lookup")
	}
	d := r.tr.Exchanges[r.next].DNS
	r.next++
	if d.Host != host {
		r.t.Errorf("%s: DNS host %q want %q", r.tr.file, host, d.Host)
	}
	if d.Error != "" || len(d.Answers) == 0 {
		return "", errors.New("lookup failed: " + d.Error)
	}
	return d.Answers[0], nil
}

func (tr *transcript) fresh(s string) string {
	for k, v := range tr.Secrets {
		s = strings.ReplaceAll(s, "{{b64secret:"+k+"}}", base64.StdEncoding.EncodeToString([]byte(v+":")))
		s = strings.ReplaceAll(s, "{{secret:"+k+"}}", v)
	}
	return s
}

func redact(s string, secrets map[string]string) string {
	for _, v := range secrets {
		if v != "" {
			s = strings.ReplaceAll(s, v, "***")
			s = strings.ReplaceAll(s, base64.StdEncoding.EncodeToString([]byte(v+":")), "***")
		}
	}
	return s
}

func (r *replay) RoundTrip(req *http.Request) (*http.Response, error) {
	r.skipDNS()
	if r.next >= len(r.tr.Exchanges) {
		r.t.Errorf("%s: unexpected extra request %s %s", r.tr.file, req.Method, redact(req.URL.String(), r.tr.Secrets))
		return nil, errors.New("unrouted request")
	}
	ex := r.tr.Exchanges[r.next]
	r.next++
	want := ex.Request
	if want == nil {
		r.t.Fatalf("%s: exchange %d has no request", r.tr.file, r.next-1)
	}
	if req.Method != want.Method {
		r.t.Errorf("%s: method %s want %s", r.tr.file, req.Method, want.Method)
	}
	if got, w := req.URL.String(), r.tr.fresh(want.URL); got != w {
		r.t.Errorf("%s: url\n got %s\nwant %s", r.tr.file, redact(got, r.tr.Secrets), want.URL)
	}
	gotH := map[string]string{}
	for k, v := range req.Header {
		gotH[strings.ToLower(k)] = strings.Join(v, ",")
	}
	wantH := map[string]string{}
	for k, v := range want.Headers {
		wantH[strings.ToLower(k)] = r.tr.fresh(v)
	}
	if len(gotH) != len(wantH) {
		r.t.Errorf("%s: header names got %v want %v", r.tr.file, sortedKeys(gotH), sortedKeys(wantH))
	}
	for k, v := range wantH {
		if gotH[k] != v {
			r.t.Errorf("%s: header %s differs", r.tr.file, k)
		}
	}
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	if string(body) != r.tr.fresh(want.Body) {
		r.t.Errorf("%s: body\n got %s\nwant %s", r.tr.file, redact(string(body), r.tr.Secrets), want.Body)
	}
	if ex.Response.Raises != "" {
		return nil, timeoutErr{msg: ex.Response.Message}
	}
	h := http.Header{}
	for k, v := range ex.Response.Headers {
		h.Add(k, v)
	}
	return &http.Response{StatusCode: ex.Response.Status, Status: http.StatusText(ex.Response.Status), Header: h,
		Body: io.NopCloser(strings.NewReader(ex.Response.Body)), Request: req}, nil
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func loadTranscripts(t *testing.T) []*transcript {
	t.Helper()
	files, err := filepath.Glob("../../../testdata/providers/*/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("no transcripts found: %v", err)
	}
	want := map[string]bool{}
	for _, n := range firmoNames {
		want[n] = true
	}
	var out []*transcript
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var tr transcript
		if err := json.Unmarshal(raw, &tr); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if !want[tr.Provider] {
			continue
		}
		tr.file = filepath.Base(filepath.Dir(f)) + "/" + filepath.Base(f)
		out = append(out, &tr)
	}
	return out
}

func (tr *transcript) keyFunc(p providers.Provider) func() (string, error) {
	return func() (string, error) {
		kp, ok := p.(providers.Keyed)
		if !ok {
			return "", nil
		}
		var vals []string
		for _, env := range kp.KeyEnv() {
			vals = append(vals, tr.Secrets[env])
		}
		if len(vals) == 1 {
			return vals[0], nil
		}
		return providers.JoinKeys(vals...), nil
	}
}

func compact(t *testing.T, raw []byte) string {
	t.Helper()
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// fastLimiters removes the self rate limiters' waits (the Python recorder does
// the same); TestSECUserAgentAndRateLimit exercises them for real.
func fastLimiters(t *testing.T) {
	t.Helper()
	g, s := gleifLimiter.interval, secLimiter.interval
	gleifLimiter.interval, secLimiter.interval = 0, 0
	t.Cleanup(func() { gleifLimiter.interval, secLimiter.interval = g, s })
}

// TestFirmoProviderTranscripts replays every Python-recorded transcript of the
// ten providers: request method, full URL (query order), headers and body, and
// the result (success, error, confidence, fields JSON incl. key order).
func TestFirmoProviderTranscripts(t *testing.T) {
	fastLimiters(t)
	trs := loadTranscripts(t)
	if len(trs) < 150 {
		t.Fatalf("expected the full firmographic transcript set, got %d", len(trs))
	}
	seen := map[string]int{}
	for _, tr := range trs {
		tr := tr
		seen[tr.Provider]++
		t.Run(tr.file, func(t *testing.T) {
			p, ok := providers.Get(tr.Provider)
			if !ok {
				t.Fatalf("provider %q is not registered", tr.Provider)
			}
			rp := &replay{t: t, tr: tr}
			oldLookup := lookupIPv4
			lookupIPv4 = rp.lookup
			defer func() { lookupIPv4 = oldLookup }()
			settings := map[string]any{}
			for k, v := range tr.Config {
				settings[k] = v
			}
			if tr.Provider == "tech_stack" { // the recorder enables the fetch gate
				settings["TECH_STACK_WEBSITE_FETCH_ENABLED"] = true
			}
			if tr.Provider == "mca_registry" { // Python's module-level key was the fake one
				old := mcaDefaultKey
				mcaDefaultKey = "ctk_datagov_0001"
				defer func() { mcaDefaultKey = old }()
			}
			in := providers.Input{Lead: tr.Input, Settings: settings}
			out, err := p.Enrich(context.Background(), &http.Client{Transport: rp}, tr.keyFunc(p), in)
			if err != nil {
				t.Fatalf("Enrich: %v", err)
			}
			var want struct {
				Success    bool            `json:"success"`
				Fields     json.RawMessage `json:"fields"`
				Confidence float64         `json:"confidence"`
				Error      string          `json:"error"`
			}
			if err := json.Unmarshal(tr.Result, &want); err != nil {
				t.Fatal(err)
			}
			if (out.Error == "") != want.Success {
				t.Errorf("success: error=%q want success=%v", out.Error, want.Success)
			}
			if out.Error != want.Error {
				t.Errorf("error %q want %q", out.Error, want.Error)
			}
			if out.Confidence != want.Confidence {
				t.Errorf("confidence %v want %v", out.Confidence, want.Confidence)
			}
			extra := []byte("{}")
			if out.Extra != nil {
				extra, _ = out.Extra.MarshalJSON()
			}
			if got, w := string(extra), compact(t, want.Fields); got != w {
				t.Errorf("fields\n got %s\nwant %s", got, w)
			}
			rp.skipDNS()
			if rp.next != len(tr.Exchanges) {
				t.Errorf("consumed %d of %d exchanges", rp.next, len(tr.Exchanges))
			}
		})
	}
	for _, n := range firmoNames {
		if seen[n] == 0 {
			t.Errorf("no transcripts for %s", n)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// No key, no request: the Python providers return their missing-key message as
// a normal result and make no network call.
func TestMissingKeyErrorMatchesPython(t *testing.T) {
	seen := map[string]bool{}
	for _, tr := range loadTranscripts(t) {
		if tr.Scenario != "no_credentials" || tr.Provider == "mca_registry" {
			continue
		}
		seen[tr.Provider] = true
		p, _ := providers.Get(tr.Provider)
		rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
			t.Errorf("%s: request made without a key", tr.Provider)
			return nil, errors.New("no network")
		})
		old := lookupIPv4
		lookupIPv4 = func(context.Context, string) (string, error) {
			t.Errorf("%s: DNS lookup without a key", tr.Provider)
			return "", errors.New("no dns")
		}
		out, err := p.Enrich(context.Background(), &http.Client{Transport: rt},
			func() (string, error) { return "", nil }, providers.Input{Lead: tr.Input, Settings: tr.Config})
		lookupIPv4 = old
		if err != nil {
			t.Fatalf("%s: %v", tr.Provider, err)
		}
		var want struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(tr.Result, &want)
		if out.Error != want.Error || out.Error == "" {
			t.Errorf("%s: error %q want %q", tr.Provider, out.Error, want.Error)
		}
	}
	for _, n := range []string{"ipinfo", "companies_house", "google_maps"} {
		if !seen[n] {
			t.Errorf("no no_credentials transcript for %s", n)
		}
	}
}

// A failing key lookup is an error from Enrich (infrastructure), with no
// request and without the key anywhere.
func TestKeyLookupFailureIsError(t *testing.T) {
	boom := errors.New("credentials: transport error")
	for _, n := range []string{"ipinfo", "companies_house", "google_maps"} {
		p, _ := providers.Get(n)
		out, err := p.Enrich(context.Background(), &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Error("request made")
			return nil, errors.New("x")
		})}, func() (string, error) { return "", boom },
			providers.Input{Lead: map[string]any{"company": "Acme", "website": "acme.com"}})
		if !errors.Is(err, boom) || out.Error != "" {
			t.Errorf("%s: got out=%+v err=%v", n, out, err)
		}
	}
}

// A transport failure's text never carries the key (it is in the URL of some
// providers, in a header of others).
func TestProviderErrorsNeverCarryKey(t *testing.T) {
	const secret = "ctk_super_secret_key_123"
	fail := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("dial " + r.URL.String() + " " + r.Header.Get("Authorization") + " " + r.Header.Get("X-Goog-Api-Key") + " refused")
	})
	old := lookupIPv4
	lookupIPv4 = func(context.Context, string) (string, error) { return "93.184.216.34", nil }
	defer func() { lookupIPv4 = old }()
	lead := map[string]any{"company": "Acme", "website": "acme.com"}
	for _, n := range []string{"ipinfo", "companies_house", "google_maps", "mca_registry"} {
		p, _ := providers.Get(n)
		out, err := p.Enrich(context.Background(), &http.Client{Transport: fail}, func() (string, error) { return secret, nil },
			providers.Input{Lead: lead})
		if err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		for _, v := range []string{secret, base64.StdEncoding.EncodeToString([]byte(secret + ":"))} {
			if strings.Contains(out.Error, v) {
				t.Errorf("%s: error carries the key: %q", n, out.Error)
			}
		}
	}
}

func TestRegistered(t *testing.T) {
	for _, n := range firmoNames {
		p, ok := providers.Get(n)
		if !ok || p.Name() != n {
			t.Errorf("%s not registered under its own name", n)
		}
	}
	for n, env := range map[string]string{"ipinfo": "IPINFO_TOKEN", "companies_house": "COMPANIES_HOUSE_API_KEY",
		"google_maps": "GOOGLE_MAPS_API_KEY", "mca_registry": "DATA_GOV_IN_KEY"} {
		p, _ := providers.Get(n)
		k, ok := p.(providers.Keyed)
		if !ok || len(k.KeyEnv()) != 1 || k.KeyEnv()[0] != env {
			t.Errorf("%s: KeyEnv %v want [%s]", n, k, env)
		}
	}
}

func inputOf(lead map[string]any) providers.Input { return providers.Input{Lead: lead} }
