package leadpeople

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// Final security Low 9: without an injected client the Wikidata lookup uses the SSRF-guarded egress client, never
// http.DefaultClient.
func TestWikidataWithoutClientIsGuarded(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hit = true; w.Write([]byte(`{}`)) }))
	defer srv.Close()
	w := WikidataIdentity{API: srv.URL}
	if _, err := w.get(context.Background(), url.Values{"action": {"wbsearchentities"}}); err == nil || hit {
		t.Fatalf("unguarded client reached a loopback server: err=%v hit=%v", err, hit)
	}
}
