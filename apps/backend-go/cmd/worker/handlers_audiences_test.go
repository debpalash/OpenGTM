package main

import (
	"context"
	"io"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audiencerefresh"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/telemetry"
)

// Per-type wall-clock limits: Python JOB_TIMEOUTS (audience_refresh 900, audience_destination_sync 1800).
func TestWorkerAudienceTimeoutsPerType(t *testing.T) {
	for typ, secs := range map[string]int{"audience_refresh": 900, "audience_destination_sync": 1800} {
		d, ok := audienceLimit(typ)
		if !ok || int(d.Seconds()) != secs {
			t.Errorf("%s: %v %v, want %ds", typ, d, ok, secs)
		}
		if got := jobLimit(config.Config{JobTimeout: 1234}, typ); int(got.Seconds()) != secs {
			t.Errorf("jobLimit(%s) = %v, want %ds", typ, got, secs)
		}
	}
	if _, ok := audienceLimit("send"); ok {
		t.Error("send is not an audience job")
	}
}

// Both types always have a handler (plan 15 cutover: no flags), and without an emitter audience_refresh fails
// permanently instead of dropping membership events.
func TestAudienceHandlersAlwaysRegistered(t *testing.T) {
	log := telemetry.NewLogger(io.Discard)
	got := audienceHandlers(config.Config{}, nil, log, nil, nil, audienceOpts{})
	for _, k := range []string{"audience_refresh", "audience_destination_sync"} {
		if got[k] == nil {
			t.Fatalf("%s not registered", k)
		}
	}
	if err := got["audience_refresh"](context.Background(), &queue.Claim{Type: "audience_refresh", WorkspaceID: "ws"}); !queue.IsPermanent(err) {
		t.Fatalf("audience_refresh without an emitter: %v", err)
	}
}

// The tenant comes from the claim only: an audience job without jobs.workspace_id fails permanently.
func TestAudienceJobsRefuseEmptyJobWorkspace(t *testing.T) {
	log := telemetry.NewLogger(io.Discard)
	got := audienceHandlers(config.Config{}, nil, log, nil, fakeEmitter{}, audienceOpts{})
	for typ, payload := range map[string]string{
		"audience_refresh":          `{"workspace_id":"ws","audience_id":"a"}`,
		"audience_destination_sync": `{"workspace_id":"ws","run_id":"r"}`,
	} {
		if err := got[typ](context.Background(), &queue.Claim{Type: typ, Payload: []byte(payload)}); !queue.IsPermanent(err) {
			t.Errorf("%s: %v, want a permanent refusal", typ, err)
		}
	}
}

type fakeResolver struct{}

func (fakeResolver) Get(context.Context, string, string) (string, string, error) { return "", "", nil }

type srcFake struct{ source string }

func (s srcFake) Get(context.Context, string, string) (string, string, error) {
	return "tok", s.source, nil
}

// S-L25: AD_TOKENS_WORKSPACE_ONLY off keeps the global/env fallback; on, a global-only token reads as unset
// (delivery fails "missing workspace secret ...").
func TestAdTokenGetterWorkspaceOnly(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		only   bool
		source string
		want   string
	}{{false, "global", "tok"}, {false, "env", "tok"}, {true, "global", ""}, {true, "env", ""}, {true, "workspace", "tok"}} {
		got, err := adTokenGetter(srcFake{tc.source}, tc.only)(ctx, "ws", "META_ACCESS_TOKEN")
		if err != nil || got != tc.want {
			t.Errorf("only=%v %s: %q %v", tc.only, tc.source, got, err)
		}
	}
}

type fakeEmitter struct{}

func (fakeEmitter) AudienceEvents(context.Context, pgx.Tx, string, []audiencerefresh.MembershipEvent) (int, error) {
	return 0, nil
}
