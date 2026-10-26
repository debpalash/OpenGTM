package contact

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/egress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

const testKey = "ctk_secret_value_123"

func keyOf(k string) func() (string, error) { return func() (string, error) { return k, nil } }

func hunterLead() providers.Input {
	return providers.Input{Lead: map[string]any{"website": "https://acme.com", "contact_person": "Ada Lovelace"}}
}

// The production egress client refuses a host that resolves private, at dial
// time, and neither the error nor the output carries the key.
func TestProviderHonoursEgressGuard(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"data":{"email":"a@b.c","confidence":90}}`))
	}))
	defer srv.Close()
	c := egress.NewClientForEnv("production", nil)

	out, err := (&hunter{base: srv.URL}).Enrich(context.Background(), c, keyOf(testKey), hunterLead())
	if err != nil {
		t.Fatal(err)
	}
	if out.Error == "" || hits.Load() != 0 {
		t.Fatalf("private host reached: hits=%d out=%+v", hits.Load(), out)
	}
	if strings.Contains(out.Error, testKey) {
		t.Fatalf("key leaked in error: %q", out.Error)
	}

	// Reacher: the URL is operator config, the egress guard still applies.
	r := newReacher()
	ro, err := r.Enrich(context.Background(), c, keyOf(testKey), providers.Input{
		Lead:     map[string]any{"email": "ada@acme.com"},
		Settings: map[string]any{"REACHER_ENABLED": "1", "REACHER_URL": srv.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 0 {
		t.Fatal("reacher reached a private host")
	}
	if s, _ := ro.Value.(string); s != "unknown" {
		t.Fatalf("reacher value %v", ro.Value)
	}
	if d, _ := ro.Extra.Get("detail"); d != "error" {
		t.Fatalf("detail %v", d)
	}
}

// A redirect is never followed, even if the caller's client would follow it.
func TestProviderNoRedirects(t *testing.T) {
	var target atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { target.Add(1) }))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/x", http.StatusFound)
	}))
	defer srv.Close()

	out, err := (&hunter{base: srv.URL}).Enrich(context.Background(), &http.Client{}, keyOf(testKey), hunterLead())
	if err != nil {
		t.Fatal(err)
	}
	if target.Load() != 0 {
		t.Fatal("redirect was followed")
	}
	if out.Error == "" {
		t.Fatalf("expected a failure on a 302 with no JSON body: %+v", out)
	}
}

// Response bodies are capped at 1 MiB.
func TestProviderBodyCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"email":"`))
		_, _ = w.Write([]byte(strings.Repeat("a", 2<<20)))
		_, _ = w.Write([]byte(`"}}`))
	}))
	defer srv.Close()
	out, err := (&hunter{base: srv.URL}).Enrich(context.Background(), &http.Client{}, keyOf(testKey), hunterLead())
	if err != nil {
		t.Fatal(err)
	}
	if out.Error == "" || out.Value != nil || !strings.Contains(out.Error, "exceeds") {
		t.Fatalf("oversized body accepted: %+v", out)
	}
	if strings.Contains(out.Error, testKey) {
		t.Fatal("key leaked")
	}
}

// Transport errors embed the request URL; the key is in hunter's query string.
func TestProviderErrorsNeverCarryKey(t *testing.T) {
	c := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, &urlErr{u: r.URL.String()}
	})}
	out, err := (&hunter{}).Enrich(context.Background(), c, keyOf(testKey), hunterLead())
	if err != nil {
		t.Fatal(err)
	}
	if out.Error == "" || strings.Contains(out.Error, testKey) {
		t.Fatalf("error %q", out.Error)
	}
}

type urlErr struct{ u string }

func (e *urlErr) Error() string { return "dial failed for " + e.u }
