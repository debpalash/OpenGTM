//go:build integration

package watchsvc_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/watchsvc"
)

// F21: ten parallel creates at cap-1 yield exactly one success.
func TestCreateWatchCapDoesNotRace(t *testing.T) {
	e := setup(t)
	e.svc.Cfg.MaxWatchesPerWS = 3
	mk := func(i int) watchsvc.CreateInput {
		return watchsvc.CreateInput{Kind: "feed", Target: fmt.Sprintf("https://feeds.example.com/%d.xml", i)}
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
