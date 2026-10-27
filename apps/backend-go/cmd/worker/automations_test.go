package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
)

func TestTriggerEvalDeadline600(t *testing.T) {
	if got := jobLimit(config.Config{JobTimeout: 1234 * time.Second}, "trigger_eval"); got != 600*time.Second {
		t.Fatalf("trigger_eval limit %s", got)
	}
}

func TestWatchPollDeadline600(t *testing.T) {
	if got := jobLimit(config.Config{JobTimeout: 1234 * time.Second}, "watch_poll"); got != 600*time.Second {
		t.Fatalf("watch_poll limit %s", got)
	}
}

func TestUnknownJobTypeFailsPermanentlyAfterAutomationWiring(t *testing.T) {
	handlers := map[string]runner{"trigger_eval": func(context.Context, *queue.Claim) error { return nil }}
	err := dispatch(handlers)(context.Background(), &queue.Claim{Type: "mystery"})
	if err == nil || !queue.IsPermanent(err) || !strings.Contains(err.Error(), "mystery") {
		t.Fatalf("unknown type: %v", err)
	}
}
