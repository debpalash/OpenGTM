package workbook

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/colgraph"
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
