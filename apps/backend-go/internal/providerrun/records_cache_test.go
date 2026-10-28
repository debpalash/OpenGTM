package providerrun

import (
	"context"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	"github.com/jackc/pgx/v5/pgxpool"
	"testing"
)

func TestRecordCacheSeparatesPeopleInputsAndSettings(t *testing.T) {
	r := Runner{d: Deps{Pool: &pgxpool.Pool{}}}
	in := providers.Input{Field: "score_tier", Lead: map[string]any{"company_id": "same-company", "person_id": "ana", "website": "https://same.test", "score": 90}}
	if m, _, ok := r.mode(context.Background(), "lead_scorer", in); ok || m != cacheNone {
		t.Fatal("scoring must use current input, without stale domain cache")
	}
	in.Field = "email"
	_, a, ok := r.mode(context.Background(), "test_provider", in)
	if !ok {
		t.Fatal("missing record cache")
	}
	in.Lead["person_id"] = "bo"
	_, b, _ := r.mode(context.Background(), "test_provider", in)
	if a == b {
		t.Fatal("different people share cache key")
	}
	in.Settings = map[string]any{"region": "EU"}
	_, c, _ := r.mode(context.Background(), "test_provider", in)
	if b == c {
		t.Fatal("provider configuration omitted from key")
	}
}
