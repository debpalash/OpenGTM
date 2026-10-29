package collect

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadweb"
)

// panicWeb panics on the calls whose query or URL contains trigger and serves the rest from the
// recorded world.
type panicWeb struct {
	*fakeWeb
	trigger string
	hits    atomic.Int32
}

func (p *panicWeb) Search(ctx context.Context, q string, n int) []leadweb.Hit {
	if strings.Contains(q, p.trigger) {
		p.hits.Add(1)
		panic("boom: " + q)
	}
	return p.fakeWeb.Search(ctx, q, n)
}

func (p *panicWeb) Fetch(ctx context.Context, url string, tier int, timeout time.Duration) (string, bool) {
	if strings.Contains(url, p.trigger) {
		p.hits.Add(1)
		var m map[string]int
		m["x"]++ // a nil-map write: a runtime panic, not a panic(value)
	}
	return p.fakeWeb.Fetch(ctx, url, tier, timeout)
}

func TestRegistryBatchPanicIsolated(t *testing.T) {
	src := fakeSources{enabled: setOf("registry_sources", "indiamart", "justdial"), health: setOf()}
	h := newHarness(t, nil, false, src, nil)
	pw := &panicWeb{fakeWeb: h.web, trigger: "justdial"}
	h.runner.Web = pw
	j := &job{r: h.runner, id: "job1", ws: "ws1", query: "plumbers in mumbai", em: h.em}
	_ = j.searchRegistrySources(context.Background()) // must not crash the process
	if pw.hits.Load() == 0 {
		t.Skip("trigger source not searched by this query")
	}
}

func TestWebsiteScanPanicIsolated(t *testing.T) {
	pw := &panicWeb{fakeWeb: &fakeWeb{w: world{}}, trigger: "bad.example"}
	leads := []*Lead{{Company: "Bad", Website: "https://bad.example"}, {Company: "Good", Website: "https://good.example"}}
	EnrichFromWebsites(context.Background(), pw, leads, 5)
	if pw.hits.Load() == 0 {
		t.Fatal("the panicking site was never fetched")
	}
}

func TestDecisionMakerPanicIsolated(t *testing.T) {
	pw := &panicWeb{fakeWeb: &fakeWeb{w: world{}}, trigger: ""} // every search panics
	leads := []*Lead{{Company: "Acme"}, {Company: "Globex"}}
	EnrichDecisionMakers(context.Background(), pw, noSleep, leads, 2, 3)
	if pw.hits.Load() == 0 {
		t.Fatal("no search was made")
	}
}

func TestProbePanicIsolated(t *testing.T) {
	w := loadWorld(t)
	pw := &panicWeb{fakeWeb: &fakeWeb{w: w}, trigger: "justdial"}
	got, err := NativeProber{Web: pw}.SourceProbe(context.Background(), []string{"indiamart", "justdial"}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got["indiamart"] == nil || *got["indiamart"] != 8 {
		t.Fatalf("the healthy probe must still answer, got %v", got)
	}
	if got["justdial"] != nil {
		t.Fatalf("a crashed probe answers nil (outage), got %v", *got["justdial"])
	}
}
