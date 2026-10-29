package collect

import (
	"context"
	"testing"
)

// boostEnrich answers jobspy with a fixed hiring_signals JSON.
type boostEnrich struct {
	fakeEnrich
	signals string
}

func (b boostEnrich) Provider(_ context.Context, _, name string, _ *Lead) (ProviderResult, error) {
	return ProviderResult{Success: true, Fields: map[string]any{"hiring_signals": b.signals}}, nil
}

// L3: a hostile or odd score_boost never takes the score outside 0..100 (and never overflows).
func TestHiringBoostClamped(t *testing.T) {
	for _, tc := range []struct {
		signals string
		want    int
	}{
		{`{"score_boost": 1e300}`, 100},
		{`{"score_boost": -1e300}`, 0},
		{`{"score_boost": -500}`, 0},
		{`{"score_boost": 9.3e18}`, 100},
		{`{"score_boost": 10}`, 70},
		{`{"score_boost": "50"}`, 60},
	} {
		h := newHarness(t, nil, false, fakeSources{}, nil)
		h.runner.Enrich = boostEnrich{signals: tc.signals}
		j := &job{r: h.runner, id: "j", ws: "ws", query: "q", ledger: newFakeLedger(0), em: h.em}
		l := NewLead(Lead{Company: "Acme", Score: 60})
		if err := j.hiringStage(context.Background(), []*Lead{l}); err != nil {
			t.Fatal(err)
		}
		if l.Score != tc.want {
			t.Errorf("%s: score %d, want %d", tc.signals, l.Score, tc.want)
		}
	}
}
