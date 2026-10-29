package workbook

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/rowtmpl"
)

// M3: a row value cannot choose the webhook host or bend the template's authority.
func TestSendWebhookURLTemplateIsLiteralInAuthority(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	rs := &runState{deps: Deps{Client: srv.Client(), URLCheck: func(string) error { return nil }}}
	cfg := func(u string) (map[string]any, string) { return map[string]any{"url": u, "method": "GET"}, u }
	ctx := context.Background()
	for _, tc := range []struct {
		name, tmpl string
		row        map[string]any
	}{
		{"placeholder in host", "https://{sub}.example.com/hook", map[string]any{"sub": "evil.com/"}},
		{"placeholder first", "{website}/hook", map[string]any{"website": srv.URL}},
		{"placeholder in port", "http://127.0.0.1:{p}/x", map[string]any{"p": "1"}},
	} {
		c, _ := cfg(tc.tmpl)
		res, err := rs.sendWebhook(ctx, c, colgraph.Col{Vals: map[string]any{}}, tc.row)
		if err != nil || res.Success || !strings.Contains(res.Error, "literal") {
			t.Errorf("%s: %+v %v", tc.name, res, err)
		}
	}
	if hits.Load() != 0 {
		t.Fatal("a refused template reached the network")
	}
	// substituted path values are percent-encoded and stay in the path
	var path atomic.Value
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { path.Store(r.URL.EscapedPath()) }))
	defer srv2.Close()
	rs.deps.Client = srv2.Client()
	c, _ := cfg(srv2.URL + "/h/{id}")
	res, err := rs.sendWebhook(ctx, c, colgraph.Col{Vals: map[string]any{}}, map[string]any{"id": "../evil/x?y"})
	if err != nil || !res.Success {
		t.Fatalf("%+v %v", res, err)
	}
	if got, _ := path.Load().(string); got != "/h/..%2Fevil%2Fx%3Fy" {
		t.Fatalf("path %q", got)
	}
}

type errStore struct{}

func (errStore) SequenceExists(context.Context, string, string) (bool, error) {
	return false, errors.New(`ERROR: relation "outreach_sequences" secret detail (SQLSTATE 42P01)`)
}
func (errStore) IsSuppressed(context.Context, string, string) (bool, error) { return false, nil }
func (errStore) Enroll(context.Context, string, string, int64, string, string) (bool, error) {
	return false, nil
}

// L5/L7: a cell never stores a webhook response body, a raw request error or a raw database error.
func TestOutputErrorsAreShortAndSanitized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte("echo " + r.Header.Get("Authorization")))
	}))
	defer srv.Close()
	rs := &runState{deps: Deps{Client: srv.Client(), URLCheck: func(string) error { return nil }}}
	cfgOrdered, _ := rowtmplDecode(`{"headers":{"Authorization":"Bearer s3cret"}}`)
	res, err := rs.sendWebhook(context.Background(), map[string]any{"url": srv.URL, "method": "GET", "headers": map[string]any{"Authorization": "Bearer s3cret"}}, cfgOrdered, map[string]any{})
	if err != nil || res.Success || res.Error != "HTTP 500" {
		t.Fatalf("%+v %v", res, err)
	}
	// an unparseable resolved URL does not leak through the request error
	res, _ = rs.sendWebhook(context.Background(), map[string]any{"url": "http://h.example.com/%zz", "method": "GET"}, colgraph.Col{Vals: map[string]any{}}, map[string]any{})
	if strings.Contains(res.Error, "%zz") {
		t.Fatalf("raw url in error: %q", res.Error)
	}
	out := enrollSequence(context.Background(), nil, errStore{}, map[string]any{"sequence_id": "s"}, 1, map[string]any{"email": "a@b.co"}, "ws")
	if out.Error != "sequencer_error" {
		t.Fatalf("sequencer error %q", out.Error)
	}
}

func rowtmplDecode(s string) (colgraph.Col, error) {
	v, err := rowtmpl.DecodeOrdered([]byte(s))
	c, _ := v.(colgraph.Col)
	return c, err
}
