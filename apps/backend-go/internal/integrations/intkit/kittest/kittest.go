// Package kittest replays the Python-recorded vendor transcripts
// (testdata/vendors/<vendor>/*.json) through a fake http.RoundTripper. It never
// opens a socket; every deviation from the recorded request is a test failure.
package kittest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// WS is the workspace id every transcript runs under.
const WS = "ws-transcript"

type Exchange struct {
	Request struct {
		Method  string            `json:"method"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
	} `json:"request"`
	Response struct {
		Status  int               `json:"status"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
	} `json:"response"`
}

type Transcript struct {
	Scenario string `json:"scenario"`
	Call     struct {
		Function string                     `json:"function"`
		Args     map[string]json.RawMessage `json:"args"`
	} `json:"call"`
	Secrets   map[string]string `json:"secrets"`
	Exchanges []Exchange        `json:"http_exchanges"`
	Result    json.RawMessage   `json:"result"`
	File      string            `json:"-"`

	next int
}

// Load reads every transcript of one vendor directory.
func Load(t testing.TB, vendor string) []*Transcript {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "..", "testdata", "vendors", vendor, "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no transcripts for %s: %v", vendor, err)
	}
	var out []*Transcript
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var tr Transcript
		if err := json.Unmarshal(raw, &tr); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		tr.File = filepath.Base(f)
		out = append(out, &tr)
	}
	return out
}

func (tr *Transcript) fresh(s string) string {
	for k, v := range tr.Secrets {
		s = strings.ReplaceAll(s, "{{secret:"+k+"}}", v)
	}
	return s
}

// Redact hides every transcript secret in s.
func (tr *Transcript) Redact(s string) string {
	for _, v := range tr.Secrets {
		if v != "" {
			s = strings.ReplaceAll(s, v, "***")
		}
	}
	return s
}

// Get is the secrets lookup: the transcript's own secret map, any workspace.
func (tr *Transcript) Get(_ context.Context, _ string, key string) (string, error) {
	return tr.Secrets[key], nil
}

// Client returns an http.Client whose transport serves the transcript.
func (tr *Transcript) Client(t testing.TB) *http.Client {
	return &http.Client{Transport: &replay{t: t, tr: tr}}
}

// Done fails when the transcript has requests the code never made.
func (tr *Transcript) Done(t testing.TB) {
	t.Helper()
	if tr.next != len(tr.Exchanges) {
		t.Errorf("%s: made %d of %d recorded requests", tr.File, tr.next, len(tr.Exchanges))
	}
}

// Arg decodes one call argument (an absent argument leaves dst untouched).
func (tr *Transcript) Arg(t testing.TB, name string, dst any) bool {
	t.Helper()
	raw, ok := tr.Call.Args[name]
	if !ok {
		return false
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		t.Fatalf("%s: arg %s: %v", tr.File, name, err)
	}
	return true
}

// ObjArg decodes an argument as an insertion-ordered object (nil when absent).
func (tr *Transcript) ObjArg(t testing.TB, name string) *wbcrud.Obj {
	t.Helper()
	raw, ok := tr.Call.Args[name]
	if !ok {
		return nil
	}
	o, err := wbcrud.ParseObject(raw)
	if err != nil {
		t.Fatalf("%s: arg %s: %v", tr.File, name, err)
	}
	return o
}

// CheckResult compares the result map, key order included.
func (tr *Transcript) CheckResult(t testing.TB, got *wbcrud.Obj) {
	t.Helper()
	want, err := wbcrud.ParseObject(tr.Result)
	if err != nil {
		t.Fatalf("%s: recorded result: %v", tr.File, err)
	}
	g, _ := got.MarshalJSON()
	w, _ := want.MarshalJSON()
	if !bytes.Equal(g, w) {
		t.Errorf("%s: result\n got %s\nwant %s", tr.File, tr.Redact(string(g)), w)
	}
}

type replay struct {
	t  testing.TB
	tr *Transcript
}

func (r *replay) RoundTrip(req *http.Request) (*http.Response, error) {
	tr := r.tr
	if tr.next >= len(tr.Exchanges) {
		r.t.Errorf("%s: unexpected extra request %s %s", tr.File, req.Method, tr.Redact(req.URL.String()))
		return nil, errors.New("unrouted request")
	}
	ex := tr.Exchanges[tr.next]
	tr.next++
	want := ex.Request
	if req.Method != want.Method {
		r.t.Errorf("%s: method %s want %s", tr.File, req.Method, want.Method)
	}
	if got, w := req.URL.String(), tr.fresh(want.URL); got != w {
		r.t.Errorf("%s: url\n got %s\nwant %s", tr.File, tr.Redact(got), want.URL)
	}
	gotH := map[string]string{}
	for k, v := range req.Header {
		gotH[strings.ToLower(k)] = strings.Join(v, ",")
	}
	wantH := map[string]string{}
	for k, v := range want.Headers {
		wantH[strings.ToLower(k)] = tr.fresh(v)
	}
	if len(gotH) != len(wantH) {
		r.t.Errorf("%s: header names differ (got %d, want %d)", tr.File, len(gotH), len(wantH))
	}
	for k, v := range wantH {
		if gotH[k] != v {
			r.t.Errorf("%s: header %s differs", tr.File, k)
		}
	}
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	if string(body) != tr.fresh(want.Body) {
		r.t.Errorf("%s: body\n got %s\nwant %s", tr.File, tr.Redact(string(body)), want.Body)
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

// Static is a secrets lookup keyed by workspace; workspace "" is the global
// fallback (settings/environment), as in Python's get_secret.
type Static map[string]map[string]string

func (s Static) Get(_ context.Context, ws, key string) (string, error) {
	if v := s[ws][key]; v != "" {
		return v, nil
	}
	return s[""][key], nil
}

// RoundTripFunc adapts a function to http.RoundTripper.
type RoundTripFunc func(*http.Request) (*http.Response, error)

func (f RoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Respond builds a canned response.
func Respond(req *http.Request, status int, body string, hdr ...string) *http.Response {
	h := http.Header{}
	for i := 0; i+1 < len(hdr); i += 2 {
		h.Set(hdr[i], hdr[i+1])
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: req}
}
