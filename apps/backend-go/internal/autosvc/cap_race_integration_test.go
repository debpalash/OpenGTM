//go:build integration

package autosvc_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/autosvc"
)

// F21: ten parallel creates at cap-1 yield exactly one success.
func TestCreateRuleCapDoesNotRace(t *testing.T) {
	e := setup(t)
	e.svc.Cfg.MaxRulesPerWS = 3
	mk := func(i int) autosvc.RuleInput {
		in := ruleJSON(t, "on_row_added", "", `[{"type":"re_enrich","config":{"column_ids":["x"]}}]`, "")
		in.Actions = nil
		in.Name = fmt.Sprintf("r%d", i)
		return in
	}
	for i := 0; i < 2; i++ {
		if _, err := e.svc.Create(e.ctx, wsA, mk(i)); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := e.svc.Create(e.ctx, wsA, mk(100+i)); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("successes at cap-1: %d, want 1", ok)
	}
}
