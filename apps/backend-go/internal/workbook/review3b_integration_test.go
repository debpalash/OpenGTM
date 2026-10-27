//go:build integration

package workbook_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/llm"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

// L9: a native agent cell has the same overall deadline as the RPC path (120 s, shortened
// here); a tool that never answers ends the cell as agent_timeout instead of holding the run.
func TestNativeAgentCellDeadline(t *testing.T) {
	defer workbook.SetAgentTimeout(300 * time.Millisecond)()
	e := setup(t, wsA)
	e.seed(wsA, "wb-nagt", `[{"id":"ag","type":"agent","name":"Agent","target_field":"email","tools":["slow1","slow2","slow3"]}]`, 1, 10)
	fr := &fakeRunner{fn: func(c context.Context, _ string, _ providers.Input) (providers.Output, error) {
		<-c.Done()
		return providers.Output{}, c.Err()
	}}
	d := nativeDeps(e, &fakeLLM{})
	d.Runner = fr
	start := time.Now()
	if _, err := workbook.Run(e.ctx, d, e.claim(wsA, payloadNoManifests(wsA, "wb-nagt", map[string]float64{"slow1": 0, "slow2": 0, "slow3": 0}, nil))); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("run took %s", el)
	}
	if got := overlay(e, "wb-nagt", "ag", "error"); got != "agent_timeout" {
		t.Fatalf("error %q", got)
	}
}

// M3: the batch claim persists its request set, so a retry after a crash in the middle of
// writing the results (some cells complete, the rest pending) reuses the identical set and
// applies the paid, checkpointed results under FillMissing — even when fewer cells than
// MinRows remain or a row's values changed since — instead of failing every retry.
func TestNativeBatchRetryAfterPartialWrite(t *testing.T) {
	e := setup(t, wsA)
	ids := e.seed(wsA, "wb-nbp", `[{"id":"company","type":"lead_field","name":"Company"},{"id":"a1","type":"ai_formula","name":"A1","prompt":"Pitch {company}"}]`, 3, 10)
	f := &fakeLLM{}
	f.batch = func(reqs []llm.BatchRequest, o llm.BatchOptions) (map[string]string, error) {
		if o.BatchID == "" && o.OnSubmitted != nil {
			o.OnSubmitted("msgbatch_p")
		}
		out := map[string]string{}
		for _, r := range reqs {
			out[r.CustomID] = "pitch: " + strings.TrimPrefix(r.Prompt, "Pitch ")
		}
		return out, nil
	}
	d := nativeDeps(e, f)
	d.AIBatch = workbook.BatchConfig{Enabled: true, MinRows: 2}
	c := e.claim(wsA, payloadNoManifests(wsA, "wb-nbp", nil, map[string]any{"fill_missing": true}))
	if res, err := workbook.Run(e.ctx, d, c); err != nil || res.Completed != 3 {
		t.Fatalf("%+v %v", res, err)
	}
	payload := e.str(wsA, `SELECT payload::text FROM jobs WHERE id=$1`, c.ID)
	cell := func(id int32) string {
		return e.str(wsA, `SELECT coalesce(enrichments::jsonb->'a1'->>'value','') FROM workbook_rows WHERE id=$1`, id)
	}
	// The crash: only the first cell was written; the other two are pending again, and a
	// pending row's value changed after the batch was paid for.
	e.inWS(wsA, func(tx pgx.Tx) error {
		if _, err := tx.Exec(e.ctx, `UPDATE workbook_rows SET enrichments='{}'::json WHERE id = ANY($1)`, []int32{ids[1], ids[2]}); err != nil {
			return err
		}
		_, err := tx.Exec(e.ctx, `UPDATE workbook_rows SET data='{"company":"Renamed","website":"x.example"}'::json WHERE id=$1`, ids[2])
		return err
	})
	res, err := workbook.Run(e.ctx, d, e.claim(wsA, []byte(payload)))
	if err != nil || res.Completed != 2 {
		t.Fatalf("retry: %+v %v", res, err)
	}
	if len(f.batches) != 1 || len(f.prompts) != 0 {
		t.Fatalf("retry paid again: batches %d prompts %d", len(f.batches), len(f.prompts))
	}
	if cell(ids[1]) != "pitch: Acme 1" || cell(ids[2]) != "pitch: Acme 2" {
		t.Fatalf("cells %q %q", cell(ids[1]), cell(ids[2]))
	}
	// Fewer pending cells than MinRows: the stored set still applies.
	e.inWS(wsA, func(tx pgx.Tx) error {
		_, err := tx.Exec(e.ctx, `UPDATE workbook_rows SET enrichments='{}'::json WHERE id=$1`, ids[0])
		return err
	})
	var p map[string]any
	_ = json.Unmarshal([]byte(payload), &p)
	att, _ := p["ai_batch_attempt"].(map[string]any)
	if reqs, _ := att["requests"].([]any); len(reqs) != 3 {
		t.Fatalf("stored requests %v", att["requests"])
	}
	if res, err := workbook.Run(e.ctx, d, e.claim(wsA, []byte(payload))); err != nil || res.Completed != 1 {
		t.Fatalf("retry 2: %+v %v", res, err)
	}
	if len(f.batches) != 1 || len(f.prompts) != 0 || cell(ids[0]) != "pitch: Acme 0" {
		t.Fatalf("retry 2 paid again or lost the result: %d %d %q", len(f.batches), len(f.prompts), cell(ids[0]))
	}
}
