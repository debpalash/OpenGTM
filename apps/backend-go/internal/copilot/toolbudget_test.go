package copilot

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/ssestream"
)

// The per-result cap alone lets a model-steered loop push many MiB into the next prompt; the turn also has an
// aggregate budget, past which results are replaced by a small marker.
func TestRunSafeCapsTotalToolResultBytes(t *testing.T) {
	old := maxTurnToolBytes
	maxTurnToolBytes = 3000
	defer func() { maxTurnToolBytes = old }()
	big := `{"rows": "` + strings.Repeat("a", 1000) + `"}`
	tn := newTurn(newFakeLLM(t), func(context.Context, string, map[string]any, string, *int32) (string, bool, error) {
		return big, true, nil
	})
	tn.Seen = map[string]string{}
	tn.NewTicker = func(time.Duration) (<-chan time.Time, func()) { return make(chan time.Time), func() {} }
	rec := newRec()
	w, _ := ssestream.New(rec, nil)
	var calls []parsedCall
	for _, id := range []string{"1", "2", "3", "4", "5"} {
		calls = append(calls, parsedCall{ID: id, Name: "search_leads", ArgsJSON: []byte(`{"q": "` + id + `"}`), Args: map[string]any{"q": id}})
	}
	msgs, err := tn.runSafe(context.Background(), w, calls)
	if err != nil {
		t.Fatal(err)
	}
	total, capped := 0, 0
	for _, m := range msgs {
		c := unfence(t, m["content"]) // the budget counts the raw results; the fence is fixed overhead
		total += len(c)
		if strings.Contains(c, "tool results for this turn exceed") {
			capped++
		}
	}
	if capped == 0 {
		t.Fatal("no result was capped")
	}
	if capped == len(msgs) {
		t.Fatal("everything was capped, the budget should admit the first results")
	}
	if total > 3000+capped*200 {
		t.Errorf("total %d exceeds the budget", total)
	}
}
