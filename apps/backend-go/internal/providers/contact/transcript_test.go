package contact

import (
	"bytes"
	"context"
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

// Transcripts are recorded from the real Python providers (Task 1). The vendor
// bodies are hand-written; the request and the result sides are Python's.

type exchange struct {
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

type timeoutErr struct{ msg string }

func (e timeoutErr) Error() string   { return e.msg }
func (e timeoutErr) Timeout() bool   { return true }
func (e timeoutErr) Temporary() bool { return true }

// replay is a fake RoundTripper that serves a transcript in order and records
// every deviation as a test failure.
type replay struct {
	t     *testing.T
	tr    *transcript
	next  int
	fresh func(string) string
}

func (r *replay) RoundTrip(req *http.Request) (*http.Response, error) {
	if r.next >= len(r.tr.Exchanges) {
		r.t.Errorf("%s: unexpected extra request %s %s", r.tr.file, req.Method, redact(req.URL.String(), r.tr.Secrets))
		return nil, errors.New("unrouted request")
	}
	ex := r.tr.Exchanges[r.next]
	r.next++
	if ex.Request == nil {
		r.t.Fatalf("%s: exchange %d has no request", r.tr.file, r.next-1)
	}
	want := ex.Request
	if req.Method != want.Method {
		r.t.Errorf("%s: method %s want %s", r.tr.file, req.Method, want.Method)
	}
	if got, w := req.URL.String(), r.fresh(want.URL); got != w {
		r.t.Errorf("%s: url\n got %s\nwant %s", r.tr.file, redact(got, r.tr.Secrets), want.URL)
	}
	gotH := map[string]string{}
	for k, v := range req.Header {
		gotH[strings.ToLower(k)] = strings.Join(v, ",")
	}
	wantH := map[string]string{}
	for k, v := range want.Headers {
		wantH[strings.ToLower(k)] = r.fresh(v)
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
	if string(body) != r.fresh(want.Body) {
		r.t.Errorf("%s: body\n got %s\nwant %s", r.tr.file, redact(string(body), r.tr.Secrets), want.Body)
	}
	if ex.Response.Raises != "" {
		return nil, timeoutErr{msg: ex.Response.Message}
	}
	h := http.Header{}
	for k, v := range ex.Response.Headers {
		h.Set(k, v)
	}
	return &http.Response{
		StatusCode: ex.Response.Status,
		Status:     http.StatusText(ex.Response.Status),
		Header:     h,
		Body:       io.NopCloser(strings.NewReader(ex.Response.Body)),
		Request:    req,
	}, nil
}

func redact(s string, secrets map[string]string) string {
	for _, v := range secrets {
		if v != "" {
			s = strings.ReplaceAll(s, v, "***")
		}
	}
	return s
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
	for _, n := range contactNames {
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

// contactNames are the provider names this package owns (transcript "provider").
var contactNames = []string{"hunter_io", "apollo_io", "prospeo", "snovio", "people_data_labs", "debounce", "abstract_api", "numverify", "reacher"}

func (tr *transcript) fresh(s string) string {
	for k, v := range tr.Secrets {
		s = strings.ReplaceAll(s, "{{secret:"+k+"}}", v)
	}
	return s
}

// keyFunc resolves the transcript secrets for the provider's declared env vars.
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

func (tr *transcript) provider(t *testing.T) providers.Provider {
	if tr.Provider == "reacher" {
		return newReacher() // fresh circuit breaker per transcript
	}
	p, ok := providers.Get(tr.Provider)
	if !ok {
		t.Fatalf("provider %q is not registered", tr.Provider)
	}
	return p
}

func resultJSON(o providers.Output, fields bool) []byte {
	if !fields {
		b, _ := o.Extra.MarshalJSON()
		return b
	}
	extra := []byte("{}")
	if o.Extra != nil {
		extra, _ = o.Extra.MarshalJSON()
	}
	return extra
}

func compact(t *testing.T, raw []byte) string {
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestProviderTranscripts(t *testing.T) {
	trs := loadTranscripts(t)
	if len(trs) < 150 {
		t.Fatalf("expected the full contact transcript set, got %d", len(trs))
	}
	for _, tr := range trs {
		tr := tr
		t.Run(tr.file, func(t *testing.T) {
			p := tr.provider(t)
			rp := &replay{t: t, tr: tr, fresh: tr.fresh}
			client := &http.Client{Transport: rp}
			in := providers.Input{Lead: tr.Input, Settings: tr.Config}
			if tr.Provider == "reacher" {
				checkReacher(t, tr, p, client, in)
			} else {
				out, err := p.Enrich(context.Background(), client, tr.keyFunc(p), in)
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
				if got, w := string(resultJSON(out, true)), compact(t, want.Fields); got != w {
					t.Errorf("fields\n got %s\nwant %s", got, w)
				}
			}
			if rp.next != len(tr.Exchanges) {
				t.Errorf("made %d requests, transcript has %d", rp.next, len(tr.Exchanges))
			}
		})
	}
}

// checkReacher compares the verifier-shaped result: Output.Extra carries
// {email,status,source,confidence,detail,is_available}.
func checkReacher(t *testing.T, tr *transcript, p providers.Provider, client *http.Client, in providers.Input) {
	var emails []any
	if es, ok := tr.Input["emails"].([]any); ok {
		emails = es
	} else {
		emails = []any{tr.Input["email"]}
	}
	var calls []json.RawMessage
	for _, e := range emails {
		in.Lead = map[string]any{"email": e}
		out, err := p.Enrich(context.Background(), client, tr.keyFunc(p), in)
		if err != nil {
			t.Fatalf("Enrich: %v", err)
		}
		if out.Error != "" {
			t.Errorf("reacher never reports Output.Error, got %q", out.Error)
		}
		calls = append(calls, resultJSON(out, false))
	}
	var got string
	if len(calls) == 1 && tr.Input["emails"] == nil {
		got = string(calls[0])
	} else {
		parts := make([]string, len(calls))
		for i, c := range calls {
			parts[i] = string(c)
		}
		got = `{"calls":[` + strings.Join(parts, ",") + `]}`
	}
	if want := compact(t, tr.Result); got != want {
		t.Errorf("result\n got %s\nwant %s", got, want)
	}
}

// Python returns the missing-key message as a normal result, never an error,
// and no HTTP request (nor a key fetch for keyless setups) is made.
func TestMissingKeyErrorMatchesPython(t *testing.T) {
	seen := map[string]bool{}
	for _, tr := range loadTranscripts(t) {
		if tr.Scenario != "no_credentials" || tr.Provider == "reacher" {
			continue
		}
		seen[tr.Provider] = true
		p := tr.provider(t)
		rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
			t.Errorf("%s: request made without a key", tr.Provider)
			return nil, errors.New("no network")
		})
		out, err := p.Enrich(context.Background(), &http.Client{Transport: rt},
			func() (string, error) { return "", nil }, providers.Input{Lead: tr.Input, Settings: tr.Config})
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
	if len(seen) != 8 {
		t.Errorf("no_credentials transcripts for %d providers, want 8", len(seen))
	}
}

// A failing key lookup is an error from Enrich (infrastructure), without the
// key or any request.
func TestKeyLookupFailureIsError(t *testing.T) {
	p, _ := providers.Get("hunter_io")
	boom := errors.New("credentials: transport error")
	out, err := p.Enrich(context.Background(), &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("request made")
		return nil, errors.New("x")
	})}, func() (string, error) { return "", boom },
		providers.Input{Lead: map[string]any{"website": "acme.com"}})
	if !errors.Is(err, boom) || out.Error != "" {
		t.Fatalf("got out=%+v err=%v", out, err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
