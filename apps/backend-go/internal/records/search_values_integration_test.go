//go:build integration

package records

import (
	"context"
	"testing"
)

// Found with the real-backend journey (apps/web/e2e/real): searching "domain" or "name" returned every company,
// because the search cast the whole accepted_fields object to text and so matched its JSON keys.
func TestSearchMatchesFieldValuesNotKeys(t *testing.T) {
	s, ws := recordEnv(t)
	ctx := context.Background()
	mustRecord(t, s, ws, Company, Write{Company: map[string]any{"name": "Stripe", "domain": "stripe.test", "industry": "Payments"}}, "manual", "")
	mustRecord(t, s, ws, Company, Write{Company: map[string]any{"name": "Notion", "domain": "notion.test", "industry": "Software"}}, "manual", "")
	for _, c := range []struct {
		search string
		want   int
	}{
		{"domain", 0},   // a field key, not a value
		{"industry", 0}, // a field key, not a value
		{"stripe", 1},   // name and domain value
		{"payments", 1}, // a non-name value (field search still works)
		{".test", 2},    // a value both share
		{"", 2},         // no search lists everything
	} {
		got, err := s.ListFiltered(ctx, ws, Company, ListOptions{Search: c.search, Limit: 100})
		if err != nil || got.Total != int64(c.want) || len(got.Records) != c.want {
			t.Fatalf("search %q: want %d, got %+v %v", c.search, c.want, got, err)
		}
	}
}
