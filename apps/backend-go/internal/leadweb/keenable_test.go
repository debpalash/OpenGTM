package leadweb

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
)

func keenableCfg(env map[string]string) config.LeadwebSettings {
	cfg, err := config.LeadwebFrom(func(k string) string { return env[k] })
	if err != nil {
		panic(err)
	}
	return cfg
}

func keenableFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "keenable", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestKeenableDisabledByDefaultSendsNoRequest(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1) }))
	defer srv.Close()
	for name, env := range map[string]map[string]string{
		"nothing set":        {},
		"explicit false":     {"KEENABLE_ENABLED": "false"},
		"blank key":          {"KEENABLE_API_KEY": "   "},
		"order lists engine": {"SEARCH_FALLBACK_ORDER": "keenable"},
	} {
		t.Run(name, func(t *testing.T) {
			e := NewKeyedEngines(keenableCfg(env), srv.Client(), nil)
			e.Endpoints = map[string]string{"keenable": srv.URL}
			if got := e.Configured(); len(got) != 0 {
				t.Fatal(got)
			}
			if len(e.Search(context.Background(), "q", 5)) != 0 || len(e.run(context.Background(), "keenable", "q", 5)) != 0 {
				t.Fatal("hits from a disabled engine")
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatal("request sent while disabled")
	}
	if got := keenableCfg(nil).FallbackOrder; got[len(got)-1] != "keenable" {
		t.Fatal("keenable must be last in the default order", got)
	}
}

func TestKeenableEnableModes(t *testing.T) {
	for name, tc := range map[string]struct {
		env    map[string]string
		keyed  bool
		public bool
	}{
		"key":            {map[string]string{"KEENABLE_API_KEY": " kn-test-secret "}, true, false},
		"explicit":       {map[string]string{"KEENABLE_ENABLED": "true"}, false, true},
		"key and flag":   {map[string]string{"KEENABLE_API_KEY": "kn-test-secret", "KEENABLE_ENABLED": "true"}, true, false},
		"key flag false": {map[string]string{"KEENABLE_API_KEY": "kn-test-secret", "KEENABLE_ENABLED": "false"}, true, false},
	} {
		t.Run(name, func(t *testing.T) {
			var gotAuth, gotTitle, gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotAuth, gotTitle, gotPath = r.Header.Get("Authorization"), r.Header.Get("X-Keenable-Title"), r.URL.Path
				_, _ = fmt.Fprint(w, keenableFixture(t, "success.json"))
			}))
			defer srv.Close()
			e := NewKeyedEngines(keenableCfg(tc.env), srv.Client(), nil)
			e.Endpoints = map[string]string{"keenable": srv.URL}
			if got := e.Configured(); !slices.Equal(got, []string{"keenable"}) {
				t.Fatal(got)
			}
			if len(e.Search(context.Background(), "q", 5)) != 2 {
				t.Fatal("no hits")
			}
			if tc.keyed && (gotAuth != "Bearer kn-test-secret" || gotTitle != "") {
				t.Fatal("keyed request must use bearer auth only", gotAuth, gotTitle)
			}
			if tc.public && (gotAuth != "" || gotTitle != "OpenGTM") {
				t.Fatal("keyless request must carry the app title and no credential", gotAuth, gotTitle)
			}
			_ = gotPath
		})
	}
}

func TestKeenableRequestShapeAndEndpoints(t *testing.T) {
	var body map[string]any
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "POST" || r.URL.RawQuery != "" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("incorrect request metadata")
		}
		body = nil
		_ = json.NewDecoder(r.Body).Decode(&body)
		raw, _ := json.Marshal(body)
		if strings.Contains(string(raw), "kn-test-secret") {
			t.Error("key in body")
		}
		_, _ = fmt.Fprint(w, keenableFixture(t, "success.json"))
	}))
	defer srv.Close()
	e := NewKeyedEngines(keenableCfg(map[string]string{"KEENABLE_API_KEY": "kn-test-secret"}), srv.Client(), nil)
	e.Endpoints = map[string]string{"keenable": srv.URL}
	for _, tc := range []struct{ n, want int }{{1, 1}, {10, 10}, {500, 50}} {
		hits := e.Search(context.Background(), "acme pricing", tc.n)
		if len(hits) == 0 || len(hits) > 2 || hits[0].Title != "Acme Pricing" || hits[0].Href != "https://acme.example/pricing" || hits[0].Body != "Plans start at $10." {
			t.Fatal("shape", hits)
		}
		if body["query"] != "acme pricing" || int(body["max_results"].(float64)) != tc.want || len(body) != 2 {
			t.Fatal("unexpected body", body)
		}
	}
	for _, q := range []string{"", "  ", strings.Repeat("x", 501), string([]byte{0xff})} {
		if len(e.Search(context.Background(), q, 5)) != 0 {
			t.Fatal("invalid query accepted")
		}
	}
	if len(e.Search(context.Background(), "q", 0)) != 0 || calls != 3 {
		t.Fatal("unexpected request", calls)
	}
	if keenableKeyedEndpoint != "https://api.keenable.ai/v1/search" || keenablePublicEndpoint != "https://api.keenable.ai/v1/search/public" {
		t.Fatal("endpoint constants")
	}
}

func TestKeenableResponsesTable(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		fixture string
		body    string
		want    []string
	}{
		{"success", 200, "success.json", "", []string{"https://acme.example/pricing", "https://acme.example/about"}},
		{"empty", 200, "empty.json", "", nil},
		{"malformed", 200, "malformed.json", "", nil},
		{"not json", 200, "", "<html>", nil},
		{"wrong results type", 200, "", `{"results":"wrong"}`, nil},
		{"401", 401, "401.json", "", nil},
		{"429", 429, "429.json", "", nil},
		{"500", 500, "5xx.json", "", nil},
		{"503", 503, "5xx.json", "", nil},
		{"oversized", 200, "", `{"results":[]}` + strings.Repeat(" ", keenableMaxBody), nil},
		{"non-public filtered", 200, "nonpublic.json", "", []string{"https://acme.example/ok"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := tc.body
			if tc.fixture != "" {
				payload = keenableFixture(t, tc.fixture)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, payload)
			}))
			defer srv.Close()
			for _, env := range []map[string]string{{"KEENABLE_API_KEY": "kn-test-secret"}, {"KEENABLE_ENABLED": "1"}} {
				e := NewKeyedEngines(keenableCfg(env), srv.Client(), nil)
				e.Endpoints = map[string]string{"keenable": srv.URL}
				var got []string
				for _, h := range e.Search(context.Background(), "q", 5) {
					got = append(got, h.Href)
				}
				if !slices.Equal(got, tc.want) {
					t.Fatalf("got %v want %v", got, tc.want)
				}
			}
		})
	}
}

func TestKeenableTimeoutAndRedirects(t *testing.T) {
	started := make(chan struct{}, 1)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer slow.Close()
	cfg := keenableCfg(map[string]string{"KEENABLE_API_KEY": "kn-test-secret"})
	cfg.EngineTimeout = 30 * time.Millisecond
	e := NewKeyedEngines(cfg, slow.Client(), nil)
	e.Endpoints = map[string]string{"keenable": slow.URL}
	t0 := time.Now()
	if len(e.Search(context.Background(), "q", 5)) != 0 || time.Since(t0) > 500*time.Millisecond {
		t.Fatal("deadline not enforced")
	}
	<-started

	var leaked atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		leaked.Add(1)
		_, _ = fmt.Fprint(w, keenableFixture(t, "success.json"))
	}))
	defer target.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer redir.Close()
	e = NewKeyedEngines(keenableCfg(map[string]string{"KEENABLE_API_KEY": "kn-test-secret"}), redir.Client(), nil)
	e.Endpoints = map[string]string{"keenable": redir.URL}
	if len(e.Search(context.Background(), "q", 5)) != 0 || leaked.Load() != 0 {
		t.Fatal("redirect followed")
	}
}

func TestKeenableKeyStaysRedacted(t *testing.T) {
	cfg := keenableCfg(map[string]string{"KEENABLE_API_KEY": "kn-test-secret"})
	raw, _ := json.Marshal(cfg)
	if strings.Contains(string(raw)+fmt.Sprintf("%+v %#v %v", cfg, cfg, NewKeyedEngines(cfg, nil, nil)), "kn-test-secret") {
		t.Fatal("key disclosed by formatting")
	}
}
