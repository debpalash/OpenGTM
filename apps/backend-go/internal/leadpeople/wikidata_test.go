package leadpeople

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestWikidataIdentityVectors(t *testing.T) {
	var cases []struct {
		Company    string           `json:"company"`
		SearchHits []any            `json:"search_hits"`
		Entity     map[string]any   `json:"entity"`
		Calls      []map[string]any `json:"calls"`
		Result     map[string]any   `json:"result"`
	}
	loadVec(t, "wikidata.json", &cases)
	for _, c := range cases {
		var mu sync.Mutex
		calls := []map[string]any{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			rec := map[string]any{}
			for k := range q {
				rec[k] = q.Get(k)
			}
			mu.Lock()
			calls = append(calls, rec)
			mu.Unlock()
			if r.Header.Get("User-Agent") != wikidataUA {
				t.Errorf("User-Agent = %q", r.Header.Get("User-Agent"))
			}
			if q.Get("action") == "wbsearchentities" {
				_ = json.NewEncoder(w).Encode(map[string]any{"search": c.SearchHits})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"entities": map[string]any{q.Get("ids"): c.Entity}})
		}))
		w := WikidataIdentity{Client: srv.Client(), API: srv.URL, Now: func() time.Time { return fixedNow }}
		got, err := w.ResolveIdentity(context.Background(), c.Company)
		srv.Close()
		if err != nil {
			t.Fatalf("%q: %v", c.Company, err)
		}
		sameJSON(t, "result "+c.Company, got, c.Result)
		for _, call := range c.Calls { // httpx sends the int limit as text
			for k, v := range call {
				call[k] = fmt.Sprint(v)
			}
		}
		sameJSON(t, "calls "+c.Company, calls, c.Calls)
	}
}

func TestWikidataHTTPFailureIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("<html>")) }))
	defer srv.Close()
	if _, err := (WikidataIdentity{Client: srv.Client(), API: srv.URL}).ResolveIdentity(context.Background(), "Stripe"); err == nil {
		t.Fatal("want error")
	}
}
