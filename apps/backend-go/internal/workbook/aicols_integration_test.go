//go:build integration

package workbook_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/aicol"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/llm"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

// fakeLLM is the aicol LLM seam (GO_NATIVE_AI_COLUMNS runs never reach a real provider).
type fakeLLM struct {
	mu        sync.Mutex
	prompts   []string
	systems   []string
	complete  func(prompt string) string
	decisions []map[string]any
	batches   []llm.BatchOptions
	batchReqs [][]llm.BatchRequest
	batch     func(reqs []llm.BatchRequest, o llm.BatchOptions) (map[string]string, error)
}

func (f *fakeLLM) Complete(_ context.Context, r llm.CompleteRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prompts, f.systems = append(f.prompts, r.Prompt), append(f.systems, r.System)
	if f.complete == nil {
		return "", llm.ErrEmptyResponse
	}
	return f.complete(r.Prompt), nil
}

func (f *fakeLLM) ExtractJSON(_ context.Context, prompt, _ string, _ int) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prompts = append(f.prompts, prompt)
	if len(f.decisions) == 0 {
		return nil, llm.ErrInvalidJSON
	}
	d := f.decisions[0]
	f.decisions = f.decisions[1:]
	return d, nil
}

func (f *fakeLLM) AnthropicProvider(context.Context) (llm.Provider, bool) {
	return llm.Provider{ID: "anthropic", Model: "claude-sonnet-4-6", Native: true}, f.batch != nil
}

func (f *fakeLLM) AnthropicTurn(context.Context, llm.AnthropicRequest) (*llm.AnthropicResponse, error) {
	return nil, errors.New("no native tools in this test")
}

func (f *fakeLLM) BatchComplete(_ context.Context, reqs []llm.BatchRequest, o llm.BatchOptions) (map[string]string, error) {
	f.mu.Lock()
	f.batches, f.batchReqs = append(f.batches, o), append(f.batchReqs, reqs)
	f.mu.Unlock()
	return f.batch(reqs, o)
}

type noSettings struct{}

func (noSettings) Get(_ context.Context, _, def string) (string, error) { return def, nil }

func nativeDeps(e *env, f *fakeLLM) workbook.Deps {
	d := e.deps(newSim(t0(e), nil), nil)
	d.AICols = &workbook.AIColumns{For: func(context.Context, string) aicol.Deps {
		return aicol.Deps{LLM: f, Settings: noSettings{}}
	}}
	return d
}

func t0(e *env) *testing.T { return e.t }

// With GO_NATIVE_AI_COLUMNS an ai_formula cell runs in process: no Python worker is
// configured, the prompt is resolved and guarded in Go and the cell is written as "ai".
func TestNativeAIFormulaWithoutPyworker(t *testing.T) {
	e := setup(t, wsA)
	e.seed(wsA, "wb-nai", `[{"id":"company","type":"lead_field","name":"Company"},{"id":"a1","type":"ai_formula","name":"A1","prompt":"Describe {company}"}]`, 1, 10)
	f := &fakeLLM{complete: func(string) string { return "a fine company" }}
	d := nativeDeps(e, f) // d.Py is nil
	res, err := workbook.Run(e.ctx, d, e.claim(wsA, payloadNoManifests(wsA, "wb-nai", nil, nil)))
	if err != nil || res.Completed < 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if len(f.prompts) != 1 || f.prompts[0] != "Describe Acme 0" || f.systems[0] != aicol.AIColumnSystem {
		t.Fatalf("llm calls %q %q", f.prompts, f.systems)
	}
	if v, p := overlay(e, "wb-nai", "a1", "value"), overlay(e, "wb-nai", "a1", "provider"); v != "a fine company" || p != "ai" {
		t.Fatalf("cell %q %q", v, p)
	}
}

func TestNativeResearchLegacyLoop(t *testing.T) {
	e := setup(t, wsA)
	e.seed(wsA, "wb-nres", `[{"id":"r1","type":"research","name":"R1","prompt":"Does {company} use k8s?","max_steps":2}]`, 1, 10)
	f := &fakeLLM{decisions: []map[string]any{{"action": "answer", "final": " Yes "}}}
	res, err := workbook.Run(e.ctx, nativeDeps(e, f), e.claim(wsA, payloadNoManifests(wsA, "wb-nres", nil, nil)))
	if err != nil || res.Completed != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if !strings.HasPrefix(f.prompts[0], "Question: Does Acme 0 use k8s?\n\nObservations so far:\n(no observations yet)") {
		t.Fatalf("prompt %q", f.prompts[0])
	}
	if v, p := overlay(e, "wb-nres", "r1", "value"), overlay(e, "wb-nres", "r1", "provider"); v != "Yes" || p != "research" {
		t.Fatalf("cell %q %q", v, p)
	}
}

// An agent column with a paid tool reserves it through the Go spend ledger and settles it
// exactly once (plan 15 WP-AICOLS acceptance); the agent_paid_tools refusal is gone.
func TestNativeAgentPaidToolReservedAndSettledOnce(t *testing.T) {
	e := setup(t, wsA)
	e.inWS(wsA, func(tx pgx.Tx) error { _, err := tx.Exec(e.ctx, `DELETE FROM cell_traces`); return err })
	e.seed(wsA, "wb-nag", `[{"id":"ag","type":"agent","name":"Agent","target_field":"email","tools":["freetool","paidtool"],"policy":{"max_steps":4,"max_cost_usd":0.5}}]`, 1, 10)
	fr := &fakeRunner{fn: func(_ context.Context, p string, _ providers.Input) (providers.Output, error) {
		if p == "paidtool" {
			return providers.Output{Confidence: 0.9, Extra: obj("email", "ceo@acme0.example.com")}, nil
		}
		return providers.Output{}, nil // free tool finds nothing
	}}
	d := nativeDeps(e, &fakeLLM{})
	d.Runner = fr
	res, err := workbook.Run(e.ctx, d, e.claim(wsA, payloadNoManifests(wsA, "wb-nag", map[string]float64{"paidtool": 0.05, "freetool": 0}, nil)))
	if err != nil || res.Completed != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if strings.Join(fr.calls, ",") != "freetool,paidtool" {
		t.Fatalf("calls %v", fr.calls)
	}
	if v, p := overlay(e, "wb-nag", "ag", "value"), overlay(e, "wb-nag", "ag", "provider"); v != "ceo@acme0.example.com" || p != "paidtool" {
		t.Fatalf("cell %q %q", v, p)
	}
	if n := e.one(wsA, `SELECT count(*) FROM workbook_spend_attempts`); n != 1 {
		t.Fatalf("%d attempts", n)
	}
	if st := e.str(wsA, `SELECT status FROM workbook_spend_attempts WHERE provider='paidtool'`); st != "settled" {
		t.Fatalf("attempt %q", st)
	}
	if n := e.one(wsA, `SELECT settled_microusd FROM workbook_spend_attempts WHERE provider='paidtool'`); n != 50000 {
		t.Fatalf("settled %d", n)
	}
	if o := e.str(wsA, `SELECT outcome FROM cell_traces WHERE column_id='ag'`); o != "found" {
		t.Fatalf("trace outcome %q", o)
	}
	var steps []map[string]any
	if err := json.Unmarshal([]byte(e.str(wsA, `SELECT steps::text FROM cell_traces WHERE column_id='ag'`)), &steps); err != nil || len(steps) != 2 ||
		steps[0]["reason"] != "no_value" || steps[1]["reason"] != "goal_met" || steps[1]["cost"] != 0.05 {
		t.Fatalf("steps %v %v", steps, err)
	}
	if s := e.str(wsA, `SELECT total_cost_usd::text FROM cell_traces WHERE column_id='ag'`); s != `{"spent":0.05}` {
		t.Fatalf("cost %s", s)
	}
}

// A paid tool that fails after dispatch leaves its attempt uncertain and stops the agent.
func TestNativeAgentPaidFailureIsUncertain(t *testing.T) {
	e := setup(t, wsA)
	e.seed(wsA, "wb-nagx", `[{"id":"ag","type":"agent","name":"Agent","target_field":"email","tools":["paidtool","paid2"]}]`, 1, 10)
	fr := &fakeRunner{fn: func(_ context.Context, p string, _ providers.Input) (providers.Output, error) {
		return providers.Output{}, errors.New("vendor exploded")
	}}
	d := nativeDeps(e, &fakeLLM{})
	d.Runner = fr
	if _, err := workbook.Run(e.ctx, d, e.claim(wsA, payloadNoManifests(wsA, "wb-nagx", map[string]float64{"paidtool": 0.01, "paid2": 0.02}, nil))); err != nil {
		t.Fatal(err)
	}
	if n := len(fr.calls); n != 1 {
		t.Fatalf("calls %v", fr.calls)
	}
	if st := e.str(wsA, `SELECT status FROM workbook_spend_attempts`); st != "uncertain" {
		t.Fatalf("attempt %q", st)
	}
	if got := overlay(e, "wb-nagx", "ag", "error"); got != "agent_accounting_uncertain" {
		t.Fatalf("error %q", got)
	}
}

// The native batch pre-pass claims one vendor batch per job in jobs.payload, acknowledges
// the vendor id and checkpoints the results; a retried job reuses them instead of paying again.
func TestNativeBatchDurableClaim(t *testing.T) {
	e := setup(t, wsA)
	e.seed(wsA, "wb-nb", `[{"id":"company","type":"lead_field","name":"Company"},{"id":"a1","type":"ai_formula","name":"A1","prompt":"Pitch {company}"}]`, 3, 10)
	f := &fakeLLM{}
	f.batch = func(reqs []llm.BatchRequest, o llm.BatchOptions) (map[string]string, error) {
		if o.BatchID == "" && o.OnSubmitted != nil {
			o.OnSubmitted("msgbatch_1")
		}
		out := map[string]string{}
		for _, r := range reqs {
			out[r.CustomID] = "pitch: " + strings.TrimPrefix(r.Prompt, "Pitch ")
		}
		return out, nil
	}
	d := nativeDeps(e, f)
	d.AIBatch = workbook.BatchConfig{Enabled: true, MinRows: 2}
	c := e.claim(wsA, payloadNoManifests(wsA, "wb-nb", nil, nil))
	res, err := workbook.Run(e.ctx, d, c)
	if err != nil || res.Completed != 3 {
		t.Fatalf("%+v %v", res, err)
	}
	if len(f.batches) != 1 || !f.batches[0].StrictResults || len(f.batchReqs[0]) != 3 || len(f.prompts) != 0 {
		t.Fatalf("batches %d prompts %d", len(f.batches), len(f.prompts))
	}
	if r := f.batchReqs[0][0]; r.System != aicol.AIColumnSystem || r.MaxTokens != 1500 || !strings.HasPrefix(r.Prompt, "Pitch Acme ") {
		t.Fatalf("request %+v", r)
	}
	if v := overlay(e, "wb-nb", "a1", "value"); !strings.HasPrefix(v, "pitch: Acme ") {
		t.Fatalf("cell %q", v)
	}
	payload := e.str(wsA, `SELECT payload::text FROM jobs WHERE id=$1`, c.ID)
	var p map[string]any
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		t.Fatal(err)
	}
	att, _ := p["ai_batch_attempt"].(map[string]any)
	if att["state"] != "accepted" || att["vendor_batch_id"] != "msgbatch_1" || len(att["results"].(map[string]any)) != 3 {
		t.Fatalf("attempt %v", att)
	}
	// A retry of the job (same payload) finds every result checkpointed: no new batch.
	c2 := e.claim(wsA, []byte(payload))
	if _, err := workbook.Run(e.ctx, d, c2); err != nil {
		t.Fatal(err)
	}
	if len(f.batches) != 1 {
		t.Fatalf("retry paid for a second batch: %d", len(f.batches))
	}
	// A retry whose attempt has no vendor id cannot know whether a batch was created.
	delete(att, "vendor_batch_id")
	delete(att, "results")
	att["state"] = "dispatch_claimed"
	b, _ := json.Marshal(p)
	if _, err := workbook.Run(e.ctx, d, e.claim(wsA, b)); !errors.Is(err, workbook.ErrBatchRecovery) {
		t.Fatalf("want batch recovery, got %v", err)
	}
	if len(f.batches) != 1 {
		t.Fatalf("reconcile state dispatched a batch")
	}
}

// Cancelling the job while a paid agent tool is in flight leaves its attempt uncertain (it
// may have reached the vendor) and unwinds the run; nothing settles twice.
func TestNativeAgentCancelMidCallIsUncertain(t *testing.T) {
	e := setup(t, wsA)
	e.seed(wsA, "wb-nagc", `[{"id":"ag","type":"agent","name":"Agent","target_field":"email","tools":["paidtool"]}]`, 1, 10)
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	fr := &fakeRunner{fn: func(c context.Context, _ string, _ providers.Input) (providers.Output, error) {
		cancel()
		<-c.Done()
		return providers.Output{}, c.Err()
	}}
	d := nativeDeps(e, &fakeLLM{})
	d.Runner = fr
	if _, err := workbook.Run(ctx, d, e.claim(wsA, payloadNoManifests(wsA, "wb-nagc", map[string]float64{"paidtool": 0.01}, nil))); err == nil {
		t.Fatal("a cancelled run must unwind")
	}
	if st := e.str(wsA, `SELECT status FROM workbook_spend_attempts`); st != "uncertain" {
		t.Fatalf("attempt %q", st)
	}
}
