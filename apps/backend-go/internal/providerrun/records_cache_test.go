package providerrun

import (
	"context"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/credentials"
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

func TestComplianceVerdictsAreNeverCached(t *testing.T) {
	idx := credentials.ConnectorIndex(func(string) (string, bool) { return "", false }, nil)
	r := Runner{d: Deps{Pool: &pgxpool.Pool{}, Manifests: idx}}
	lead := map[string]any{"email": "jane.smith@example.co.uk", "company": "Example Trading Ltd", "website": "https://example.co.uk"}
	if m, _, ok := r.mode(context.Background(), "precleared_compliance", providers.Input{Field: "compliance", Lead: lead}); ok || m != cacheNone {
		t.Fatal("a per-recipient compliance verdict must not be served from the company-keyed cache")
	}
	lead["contact_person"] = "Jane Smith"
	if m, _, ok := r.mode(context.Background(), "precleared_compliance", providers.Input{Field: "compliance", Lead: lead}); ok || m != cacheNone {
		t.Fatal("a compliance verdict must not be served from the record cache either")
	}
	// Other declarative providers keep the cache.
	if _, _, ok := r.mode(context.Background(), "leadmagic_email", providers.Input{Field: "email", Lead: lead}); !ok {
		t.Fatal("leadmagic_email lost its cache")
	}
}
