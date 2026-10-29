//go:build integration

package api

import (
	"encoding/json"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
)

// pollAfterEnv serves the CRUD and poll routes on one router so a poll can follow the create or enable
// that queued the watch's scheduled job (the fixtures poll_after_* were recorded that way).
func pollAfterEnv(t *testing.T) watchPollEnv {
	e := newWatchPollEnv(t, config.Flags{PGLeadStore: true, IntentPollerEnabled: true, AutomationsEnabled: true})
	d := Deps{Pool: e.pool, Signer: testSigner, Identity: e.st, Config: config.APIConfig{}, Flags: wcrudFlags("default")}
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(e.pool, nil))
	registerWatchesCRUD(r, d)
	registerWatchesPoll(r, d)
	e.h = r.Handler()
	return e
}

// replayWith sends the named fixture's request and compares it.
func (e watchPollEnv) replayBody(t *testing.T, name, body string, opts ...contract.Option) (json.RawMessage, int) {
	t.Helper()
	fx := contract.Load(t, "watches/"+name)
	hdr := bearer("alice")
	ct := ""
	if body != "" {
		ct = "application/json"
	}
	_, got := e.do(fx.Request.Method, fx.Request.Path, ct, body, hdr)
	contract.AssertEqual(t, got, fx.Response, opts...)
	return got.Body, got.Status
}

func TestPollAfterCreateAlreadyQueuedScheduled(t *testing.T) {
	e := pollAfterEnv(t)
	hdr := bearer("alice")
	_, created := e.do("POST", "/api/watches", "application/json", `{"kind":"company","target":"  Padded Co  "}`, hdr)
	var w struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created.Body, &w); err != nil || created.Status != 200 || w.ID == "" {
		t.Fatalf("create: %d %s", created.Status, created.Body)
	}
	fx := contract.Load(t, "watches/poll_after_create_already_queued_scheduled")
	_, got := e.do("POST", "/api/watches/"+w.ID+"/poll", "", "", hdr)
	contract.AssertEqual(t, got, fx.Response, pollVolatile, contract.IgnorePaths("$.fire_key", "$.job_id"))
}

func TestPollAfterEnableSeededAlreadyQueued(t *testing.T) {
	e := pollAfterEnv(t)
	e.replayBody(t, "patch_enable_disabled_seeded", `{"enabled":true}`, contract.IgnorePaths("$.next_poll_at", "$.created_at", "$.updated_at", "$.last_polled_at"))
	e.replay(t, "poll_after_enable_seeded_already_queued", pollVolatile)
}
