package providerrun

import (
	"context"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	"github.com/jackc/pgx/v5/pgxpool"
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

func TestCompanyEnrichCacheUsesEmailAndResearchCriteria(t *testing.T) {
	r := Runner{d: Deps{Pool: &pgxpool.Pool{}}}
	for _, tc := range []struct{ provider, field, key, a, b string }{
		{"companyenrich_person", "companyenrich_person", "email", "a@acme.example", "b@acme.example"},
		{"companyenrich_search", "companyenrich_search", "description", "payroll", "payments"},
	} {
		in := providers.Input{Field: tc.field, Lead: map[string]any{"website": "acme.example", tc.key: tc.a}}
		_, a, ok := r.mode(context.Background(), tc.provider, in)
		if !ok {
			t.Fatal("missing cache identity")
		}
		in.Lead[tc.key] = tc.b
		_, b, _ := r.mode(context.Background(), tc.provider, in)
		if a == b {
			t.Errorf("%s shares results for different %s", tc.provider, tc.key)
		}
	}
}
