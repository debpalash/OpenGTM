package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
)

func TestAnthropicTurnTimeoutDerivedFromBudget(t *testing.T) {
	base := 60 * time.Second
	if got := AnthropicTurnTimeout(base, 1024); got != base {
		t.Errorf("small turn = %v, want the configured %v", got, base)
	}
	if got := AnthropicTurnTimeout(base, 16000); got < 200*time.Second || got > 280*time.Second {
		t.Errorf("16000-token turn = %v, want a few minutes (below the 300s round cap)", got)
	}
	if got := AnthropicTurnTimeout(400*time.Second, 16000); got != 400*time.Second {
		t.Errorf("operator timeout must win when larger, got %v", got)
	}
}

// A slow large-budget turn is not cut off by the 60s-style base timeout.
func TestAnthropicTurnUsesExplicitTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte(`{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()
	run := func(timeout time.Duration) error {
		c := newTestClient(t, mapSettings{}, nil, func(o *Options) { o.Config = config.LLMSettings{HTTPTimeout: 100 * time.Millisecond} })
		_, err := c.AnthropicTurn(context.Background(), AnthropicRequest{
			Messages: []any{map[string]any{"role": "user", "content": "hi"}}, MaxTokens: 10, Timeout: timeout,
			Provider: &Provider{ID: AnthropicID, APIKey: "k", BaseURL: srv.URL, Model: "m", Native: true},
		})
		return err
	}
	if run(100*time.Millisecond) == nil {
		t.Fatal("control: a short timeout should cut this turn")
	}
	if err := run(5 * time.Second); err != nil {
		t.Fatalf("explicit timeout ignored: %v", err)
	}
}
