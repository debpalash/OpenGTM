package firmo

import (
	"context"
	"net/http"
	"testing"
)

func TestGleifSearch404IsError(t *testing.T) {
	fastLimiters(t)
	c := mixedClient(func(*http.Request) (*http.Response, error) { return reply(404, ""), nil }, "api.gleif.org")
	out := enrichWith(t, "gleif", c, map[string]any{"company": "Acme"})
	if out.Error != "GLEIF returned HTTP 404" {
		t.Fatalf("search 404 reported as %q", out.Error)
	}
}

func TestGleifMissingParentIsOptional(t *testing.T) {
	fastLimiters(t)
	c := mixedClient(func(*http.Request) (*http.Response, error) { return reply(404, ""), nil }, "api.gleif.org")
	rels := map[string]any{"direct-parent": map[string]any{"links": map[string]any{"lei-record": "https://api.gleif.org/api/v1/lei-records/parent"}}}
	parent, err := (gleif{}).parent(context.Background(), c, rels, "direct-parent")
	if err != nil || parent != nil {
		t.Fatalf("missing parent = %v, %v; want nil, nil", parent, err)
	}
}
