package main

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/collect"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadweb"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/telemetry"
)

type noWeb struct{}

func (noWeb) Search(context.Context, string, int) []leadweb.Hit                { return nil }
func (noWeb) Fetch(context.Context, string, int, time.Duration) (string, bool) { return "", false }

func envMap(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestCollectHandlersRegistration(t *testing.T) {
	log := telemetry.NewLogger(io.Discard)
	deps := &collect.Deps{}
	// flags off: both types claimable (hand-back), no source_health override
	got := collectHandlers(config.Config{}, nil, log, nil, nil, nil, collectOpts{getenv: envMap(nil), deps: deps})
	if got[collect.JobTypeCollect] == nil || got[collect.JobTypeBulkEnrich] == nil || got["source_health_check"] != nil {
		t.Fatalf("flags off: %v", keys(got))
	}
	// collect on: the Go handler refuses a claim without a workspace permanently (no hand-back, no DB)
	got = collectHandlers(config.Config{}, nil, log, nil, nil, nil, collectOpts{deps: deps,
		getenv: envMap(map[string]string{"GO_EXECUTOR_COLLECT": "1", "GO_EXECUTOR_BULK_ENRICH": "1"})})
	for _, typ := range []string{collect.JobTypeCollect, collect.JobTypeBulkEnrich} {
		err := got[typ](context.Background(), &queue.Claim{Type: typ, Payload: []byte(`{"slug":"s"}`)})
		var pe *queue.PermanentError
		if !errors.As(err, &pe) {
			t.Errorf("%s: err = %v, want the Go handler's permanent refusal", typ, err)
		}
	}
	// native probe replaces source_health_check only when its executor is go
	env := envMap(map[string]string{"GO_NATIVE_SOURCE_PROBE": "1"})
	if got = collectHandlers(config.Config{}, nil, log, nil, nil, nil, collectOpts{getenv: env, deps: deps, web: noWeb{}}); got["source_health_check"] != nil {
		t.Error("probe override without the source health executor")
	}
	cfg := config.Config{Executor: config.ExecutorSettings{Enabled: true, SourceHealth: true}}
	if got = collectHandlers(cfg, nil, log, nil, nil, nil, collectOpts{getenv: env, deps: deps, web: noWeb{}}); got["source_health_check"] == nil {
		t.Error("GO_NATIVE_SOURCE_PROBE did not install the in-process probe handler")
	}
}

func keys(m map[string]runner) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
