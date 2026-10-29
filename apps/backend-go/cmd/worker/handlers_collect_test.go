package main

import (
	"context"
	"errors"
	"io"
	"strings"
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

var _ = envMap

func TestCollectHandlersRegistration(t *testing.T) {
	log := telemetry.NewLogger(io.Discard)
	// no pipeline (no browser service): both types are registered and fail permanently with the reason
	got := collectHandlers(nil, log, collectOpts{why: "no browser"})
	for _, typ := range []string{collect.JobTypeCollect, collect.JobTypeBulkEnrich} {
		err := got[typ](context.Background(), &queue.Claim{Type: typ, WorkspaceID: "ws", Payload: []byte(`{"slug":"s"}`)})
		if !queue.IsPermanent(err) || !strings.Contains(err.Error(), "no browser") {
			t.Errorf("%s without a pipeline: %v", typ, err)
		}
	}
	// with the pipeline the Go handler runs: it refuses a claim without a workspace permanently (no DB)
	got = collectHandlers(nil, log, collectOpts{deps: &collect.Deps{}})
	for _, typ := range []string{collect.JobTypeCollect, collect.JobTypeBulkEnrich} {
		err := got[typ](context.Background(), &queue.Claim{Type: typ, Payload: []byte(`{"slug":"s"}`)})
		var pe *queue.PermanentError
		if !errors.As(err, &pe) {
			t.Errorf("%s: err = %v, want the Go handler's permanent refusal", typ, err)
		}
	}
}

// source_health_check probes in process over the worker's leadweb; without one it fails permanently.
func TestSourceHealthHandler(t *testing.T) {
	log := telemetry.NewLogger(io.Discard)
	if nativeProber(nil) != nil {
		t.Fatal("a prober without a leadweb")
	}
	got := leadgenHandlers(config.Config{}, nil, log, leadgenOpts{why: "no browser"})
	if err := got["source_health_check"](context.Background(), &queue.Claim{Type: "source_health_check"}); !queue.IsPermanent(err) {
		t.Fatalf("without a prober: %v", err)
	}
	if got := leadgenHandlers(config.Config{}, nil, log, leadgenOpts{prober: collect.NativeProber{Web: noWeb{}}}); got["source_health_check"] == nil {
		t.Fatal("the in-process probe handler is not registered")
	}
}

func keys(m map[string]runner) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
