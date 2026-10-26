package main

import (
	"context"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"log/slog"
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

func TestUnknownJobTypeStillHandedBackAfterAutomationWiring(t *testing.T) {
	handlers := map[string]runner{"trigger_eval": func(context.Context, *queue.Claim) error { return nil }}
	err := dispatch(handlers)(context.Background(), &queue.Claim{Type: "mystery"})
	if err == nil || !queue.IsPermanent(err) || !strings.Contains(err.Error(), "mystery") {
		t.Fatalf("unknown type: %v", err)
	}
}

// push_crm must not fall back to env-only tokens when no secret resolver exists.
func TestPushCRMFailsClosedWithoutResolver(t *testing.T) {
	var buf strings.Builder
	log := slog.New(slog.NewTextHandler(&buf, nil))
	push := crmPushGuard(nil, true, log)
	if push == nil {
		t.Fatal("no guard: push_crm would use env-only tokens")
	}
	res := push(context.Background(), "ws", nil, nil, "", 0)
	if res.Success || !strings.Contains(res.Error, "CREDENTIALS_SOURCE=local") {
		t.Fatalf("result %+v", res)
	}
	if !strings.Contains(buf.String(), "CREDENTIALS_SOURCE=local") {
		t.Fatalf("startup warning missing: %q", buf.String())
	}
	if crmPushGuard(nil, false, log) != nil {
		t.Fatal("flag off: no guard needed")
	}
	if crmPushGuard(&secrets.Resolver{}, true, log) != nil {
		t.Fatal("resolver present: no guard")
	}
}
