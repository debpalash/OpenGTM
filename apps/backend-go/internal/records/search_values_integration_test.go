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
	mustRecord(t, s, ws, Company, Write{Company: map[string]any{"name": "Acme", "domain": "acme.example", "industry": "50%_off"}}, "manual", "")
	for _, c := range []struct {
		search string
		want   int
	}{
		{"domain", 0},   // a field key, not a value
		{"industry", 0}, // a field key, not a value
		{"stripe", 1},   // name and domain value
		{"payments", 1}, // a non-name value (field search still works)
		{".test", 2},    // a value both share
		{"", 3},         // no search lists everything
		{"50%_o", 1},    // wildcard characters match themselves in a stored value
		{"%", 1},        // a literal percent sign: only the one value that holds it
		{"_", 1},        // a literal underscore, not "any character"
		{"str%e", 0},    // would match "stripe" as a wildcard
		{"s_ripe", 0},   // would match "stripe" as a wildcard
		{`\`, 0},        // the escape character itself
	} {
		got, err := s.ListFiltered(ctx, ws, Company, ListOptions{Search: c.search, Limit: 100})
		if err != nil || got.Total != int64(c.want) || len(got.Records) != c.want {
			t.Fatalf("search %q: want %d, got %+v %v", c.search, c.want, got, err)
		}
	}
}
