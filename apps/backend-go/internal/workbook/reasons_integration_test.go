//go:build integration

package workbook_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

// A provider that answers with a rate limit leaves a cell that says so, not a bare no_data, and the cell metadata
// lists what was tried.
func TestRateLimitedProviderGivesASpecificReason(t *testing.T) {
	e := setup(t, wsA)
	e.seed(wsA, "wb-rl", "["+colJSON("c1", "rl1")+"]", 1, 0)
	s := newSim(t, func(_ string, _ int, w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	c := e.claim(wsA, payloadFor(wsA, "wb-rl", map[string]float64{"rl1": 0}, nil))
	if _, err := workbook.Run(e.ctx, e.deps(s, nil), c); err != nil {
		t.Fatal(err)
	}
	msg := e.str(wsA, `SELECT error FROM workbook_enrichments WHERE workbook_id='wb-rl'`)
	if !strings.HasPrefix(msg, "rate_limited:") || !strings.Contains(msg, "rl1") {
		t.Fatalf("error %q", msg)
	}
	meta := e.str(wsA, `SELECT cell_metadata::text FROM workbook_enrichments WHERE workbook_id='wb-rl'`)
	if !strings.Contains(meta, `"attempts"`) || !strings.Contains(meta, "rl1") {
		t.Fatalf("the attempts are not recorded: %q", meta)
	}
}

// Providers skipped because they are cooling down say so in words.
func TestCoolingDownProvidersGiveASpecificReason(t *testing.T) {
	e := setup(t, wsA)
	e.seed(wsA, "wb-cd", "["+colJSON("c1", "cd1")+"]", 1, 0)
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO provider_stats (provider, field, attempts, hits, cooldown_until)
	  VALUES ('cd1', 'founded_year', 5, 0, (now() AT TIME ZONE 'UTC') + interval '5 minutes')
	  ON CONFLICT (provider, field) DO UPDATE SET cooldown_until = EXCLUDED.cooldown_until`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = e.pool.Exec(e.ctx, `DELETE FROM provider_stats WHERE provider='cd1'`) })
	s := newSim(t, nil)
	c := e.claim(wsA, payloadFor(wsA, "wb-cd", map[string]float64{"cd1": 0}, nil))
	if _, err := workbook.Run(e.ctx, e.deps(s, nil), c); err != nil {
		t.Fatal(err)
	}
	msg := e.str(wsA, `SELECT error FROM workbook_enrichments WHERE workbook_id='wb-cd'`)
	if !strings.Contains(msg, "cd1") || !strings.Contains(msg, "cooling down") {
		t.Fatalf("error %q", msg)
	}
}
