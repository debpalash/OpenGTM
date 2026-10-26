//go:build integration

package workbook_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/pyworker"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/spend"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

// fakeRunner is a providerrun.Runner stand-in.
type fakeRunner struct {
	mu      sync.Mutex
	calls   []string
	ins     []providers.Input
	keyless map[string]bool
	fn      func(ctx context.Context, provider string, in providers.Input) (providers.Output, error)
}

func (f *fakeRunner) Run(ctx context.Context, _ string, provider string, in providers.Input, _ time.Duration) (providers.Output, error) {
	f.mu.Lock()
	f.calls = append(f.calls, provider)
	f.ins = append(f.ins, in)
	f.mu.Unlock()
	return f.fn(ctx, provider, in)
}

// Configured makes fakeRunner a credential-prechecking runner.
func (f *fakeRunner) Configured(_ context.Context, _, provider string) (bool, error) {
	return !f.keyless[provider], nil
}

func obj(kv ...any) *providers.Obj {
	o := providers.NewObj()
	for i := 0; i+1 < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o
}

// fakePy is a pyworker stand-in.
type fakePy struct {
	mu    sync.Mutex
	calls []pyworker.ProviderCall
	fn    func(ctx context.Context, c pyworker.ProviderCall) (pyworker.ProviderResult, error)
}

func (f *fakePy) Call(ctx context.Context, c pyworker.ProviderCall) (pyworker.ProviderResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
	return f.fn(ctx, c)
}

func okValue(v any) pyworker.ProviderResult {
	b, _ := json.Marshal(map[string]any{"success": true, "fields": map[string]any{"value": v}, "confidence": 1.0, "error": ""})
	return pyworker.ProviderResult{Status: "ok", Output: b}
}

// payloadNoManifests is a v1 payload whose providers are not declarative
// (they run through the provider runner).
func payloadNoManifests(ws, wb string, costs map[string]float64, extra map[string]any) []byte {
	p := map[string]any{"workspace_id": ws, "workbook_id": wb, "retry_passes": 0, "concurrency": 1,
		"go": map[string]any{"version": 1, "manifests": map[string]any{}, "manifest_sha256": map[string]any{}, "costs": costs, "default_concurrency": 4}}
	for k, v := range extra {
		p[k] = v
	}
	b, _ := json.Marshal(p)
	return b
}

func overlay(e *env, wb, col, field string) string {
	return e.str(wsA, fmt.Sprintf(`SELECT enrichments::jsonb->'%s'->>'%s' FROM workbook_rows WHERE workbook_id='%s' ORDER BY id LIMIT 1`, col, field, wb))
}

func TestFormulaColumnCellsMatchPython(t *testing.T) {
	e := setup(t, wsA)
	cols := `[{"id":"f1","type":"formula","name":"F1","formula":"upper({company}) + \"!\""},
	 {"id":"f2","type":"formula","name":"F2","formula":"len({company})"},
	 {"id":"f3","type":"formula","name":"F3","formula":""},
	 {"id":"f4","type":"formula","name":"F4","formula":"default({nothing_here}, \"\")"}]`
	e.seed(wsA, "wb-formula", cols, 1, 10)
	c := e.claim(wsA, payloadNoManifests(wsA, "wb-formula", nil, nil))
	res, err := workbook.Run(e.ctx, e.deps(newSim(t, nil), nil), c)
	if err != nil {
		t.Fatal(err)
	}
	if res.Completed != 2 || res.Errors != 2 {
		t.Fatalf("result %+v", res)
	}
	if v, p := overlay(e, "wb-formula", "f1", "value"), overlay(e, "wb-formula", "f1", "provider"); v != "ACME 0!" || p != "formula" {
		t.Fatalf("f1 %q %q", v, p)
	}
	// Python keeps the python int in the overlay and json.dumps in the table.
	if n := e.one(wsA, `SELECT count(*) FROM workbook_rows WHERE workbook_id='wb-formula' AND json_typeof(enrichments::json->'f2'->'value')='number' AND enrichments::jsonb->'f2'->>'value'='6'`); n != 1 {
		t.Fatalf("f2 overlay is not a JSON number: %s", e.str(wsA, `SELECT enrichments::text FROM workbook_rows WHERE workbook_id='wb-formula'`))
	}
	if v := e.str(wsA, `SELECT value FROM workbook_enrichments WHERE workbook_id='wb-formula' AND column_id='f2'`); v != "6" {
		t.Fatalf("f2 table value %q", v)
	}
	if overlay(e, "wb-formula", "f3", "error") != "no_formula" || overlay(e, "wb-formula", "f4", "error") != "empty_result" {
		t.Fatalf("error cells: %s", e.str(wsA, `SELECT enrichments::text FROM workbook_rows WHERE workbook_id='wb-formula'`))
	}
}

func TestHTTPColumnCellsMatchPython(t *testing.T) {
	e := setup(t, wsA)
	cols := `[{"id":"h1","type":"http","name":"H1","http_url":"http://127.0.0.1:9/x"},
	 {"id":"h2","type":"http","name":"H2"},
	 {"id":"h3","type":"http","name":"H3","http_url":"http://example.invalid/{missing_col}"}]`
	e.seed(wsA, "wb-http", cols, 1, 10)
	c := e.claim(wsA, payloadNoManifests(wsA, "wb-http", nil, nil))
	s := newSim(t, nil)
	res, err := workbook.Run(e.ctx, e.deps(s, nil), c)
	if err != nil {
		t.Fatal(err)
	}
	if res.Errors != 3 || s.total() != 0 {
		t.Fatalf("result %+v", res)
	}
	if got := overlay(e, "wb-http", "h1", "error"); !strings.HasPrefix(got, "blocked_url: ") {
		t.Fatalf("h1 error %q (loopback must be refused by the egress guard)", got)
	}
	if overlay(e, "wb-http", "h2", "error") != "no_url" {
		t.Fatalf("h2 error %q", overlay(e, "wb-http", "h2", "error"))
	}
	if got := overlay(e, "wb-http", "h3", "error"); got != "unresolved_url_placeholder" {
		t.Fatalf("h3 error %q", got)
	}
}

func TestAIColumnViaPyworkerWritesCell(t *testing.T) {
	e := setup(t, wsA)
	ids := e.seed(wsA, "wb-ai", `[{"id":"a1","type":"ai_formula","name":"A1","prompt":"Describe {company}"}]`, 1, 10)
	py := &fakePy{fn: func(_ context.Context, _ pyworker.ProviderCall) (pyworker.ProviderResult, error) {
		return okValue("a fine company"), nil
	}}
	pub := &recPub{}
	d := e.deps(newSim(t, nil), pub)
	d.Py = py
	c := e.claim(wsA, payloadNoManifests(wsA, "wb-ai", nil, nil))
	res, err := workbook.Run(e.ctx, d, c)
	if err != nil || res.Completed != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if len(py.calls) != 1 || py.calls[0].Provider != "ai_formula" || py.calls[0].WorkspaceID != wsA || py.calls[0].CallID == "" {
		t.Fatalf("pyworker calls %+v", py.calls)
	}
	var in map[string]any
	if err := json.Unmarshal(py.calls[0].Input, &in); err != nil {
		t.Fatal(err)
	}
	if in["prompt"] != "Describe {company}" || in["row"].(map[string]any)["company"] != "Acme 0" || in["columns"] == nil || in["column"] == nil {
		t.Fatalf("rpc input %v", in)
	}
	if py.calls[0].Timeout != 45*time.Second {
		t.Fatalf("timeout %v", py.calls[0].Timeout)
	}
	if v, p := overlay(e, "wb-ai", "a1", "value"), overlay(e, "wb-ai", "a1", "provider"); v != "a fine company" || p != "ai" {
		t.Fatalf("cell %q %q", v, p)
	}
	var last map[string]any
	for _, m := range pub.msgs {
		if m["type"] == "cell_update" {
			last = m
		}
	}
	if last["status"] != "complete" || last["value"] != "a fine company" || last["provider"] != "ai" || last["rowId"] != int64(ids[0]) && last["rowId"] != ids[0] {
		t.Fatalf("broadcast %v", last)
	}
}

func TestAITimeoutIsCellErrorAndNotRetried(t *testing.T) {
	e := setup(t, wsA)
	e.seed(wsA, "wb-aito", `[{"id":"a1","type":"ai_formula","name":"A1","prompt":"x"}]`, 1, 10)
	py := &fakePy{fn: func(context.Context, pyworker.ProviderCall) (pyworker.ProviderResult, error) {
		return pyworker.ProviderResult{Status: "timeout", Error: "provider timed out"}, nil
	}}
	d := e.deps(newSim(t, nil), nil)
	d.Py = py
	c := e.claim(wsA, payloadNoManifests(wsA, "wb-aito", nil, map[string]any{"retry_passes": 2}))
	if _, err := workbook.Run(e.ctx, d, c); err != nil {
		t.Fatal(err)
	}
	if overlay(e, "wb-aito", "a1", "error") != "ai_timeout" {
		t.Fatalf("error %q", overlay(e, "wb-aito", "a1", "error"))
	}
	if len(py.calls) != 1 {
		t.Fatalf("a timed-out RPC was retried: %d calls", len(py.calls))
	}
}

func TestPyworkerUnavailableHandsBackJob(t *testing.T) {
	e := setup(t, wsA)
	e.seed(wsA, "wb-nopy", `[{"id":"a1","type":"ai_formula","name":"A1","prompt":"x"}]`, 1, 10)
	c := e.claim(wsA, payloadNoManifests(wsA, "wb-nopy", nil, nil))
	d := e.deps(newSim(t, nil), nil) // Py is nil
	if _, err := workbook.Run(e.ctx, d, c); !errors.Is(err, workbook.ErrHandedBack) {
		t.Fatalf("want hand back, got %v", err)
	}
	if n := e.one(wsA, `SELECT count(*) FROM workbook_enrichments`); n != 0 {
		t.Fatal("cells written before the hand back")
	}
}

func TestOutputColumnHandsBack(t *testing.T) {
	e := setup(t, wsA)
	cols := `[{"id":"f1","type":"formula","name":"F1","formula":"upper({company})"},{"id":"o1","type":"output","name":"O1","destination":"webhook"}]`
	e.seed(wsA, "wb-out", cols, 1, 10)
	c := e.claim(wsA, payloadNoManifests(wsA, "wb-out", nil, nil))
	d := e.deps(newSim(t, nil), nil)
	d.Py = &fakePy{fn: func(context.Context, pyworker.ProviderCall) (pyworker.ProviderResult, error) {
		t.Error("python called for a job that must be handed back")
		return pyworker.ProviderResult{}, nil
	}}
	if _, err := workbook.Run(e.ctx, d, c); !errors.Is(err, workbook.ErrHandedBack) {
		t.Fatalf("want hand back, got %v", err)
	}
	if n := e.one(wsA, `SELECT count(*) FROM workbook_enrichments`); n != 0 {
		t.Fatal("the formula cell ran although the job contains an output column")
	}
}

// A lead-linked row: structured fields go to the lead, scalars that are not the
// target go too, the target stays a cell value only.
func TestStructuredFieldsWriteBack(t *testing.T) {
	e := setup(t, wsA)
	ids := e.seed(wsA, "wb-struct", `[{"id":"c1","type":"enrichment","name":"C1","target_field":"contact_title","waterfall":["rp"]}]`, 1, 10)
	var leadID int32
	e.inWS(wsA, func(tx pgx.Tx) error {
		if _, err := tx.Exec(e.ctx, `DELETE FROM leads`); err != nil { // (company, city) is unique per workspace
			return err
		}
		if err := tx.QueryRow(e.ctx, `INSERT INTO leads (workspace_id, company) VALUES ($1,'Acme 0') RETURNING id`, wsA).Scan(&leadID); err != nil {
			return err
		}
		_, err := tx.Exec(e.ctx, `UPDATE workbook_rows SET lead_id=$1 WHERE id=$2`, leadID, ids[0])
		return err
	})
	fr := &fakeRunner{fn: func(context.Context, string, providers.Input) (providers.Output, error) {
		return providers.Output{Confidence: 0.9, Value: "CTO", Extra: obj(
			"contact_title", "CTO",
			"decision_makers", `[{"name":"Ada"}]`,
			"secondary_emails", "a@x.com",
			"city", "Pune",
			"bogus_field", "zz")}, nil
	}}
	d := e.deps(newSim(t, nil), nil)
	d.Runner = fr
	c := e.claim(wsA, payloadNoManifests(wsA, "wb-struct", map[string]float64{"rp": 0}, nil))
	res, err := workbook.Run(e.ctx, d, c)
	if err != nil || res.Completed != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	get := func(col string) string {
		return e.str(wsA, fmt.Sprintf(`SELECT %s FROM leads WHERE id=%d`, col, leadID))
	}
	if get("decision_makers") != `[{"name":"Ada"}]` || get("secondary_emails") != "a@x.com" || get("city") != "Pune" {
		t.Fatalf("write-back missing: %q %q %q", get("decision_makers"), get("secondary_emails"), get("city"))
	}
	t.Cleanup(func() {
		e.inWS(wsA, func(tx pgx.Tx) error { _, err := tx.Exec(e.ctx, `DELETE FROM leads`); return err })
	})
	if get("contact_title") != "" {
		t.Fatalf("the target field must stay a cell value, lead has %q", get("contact_title"))
	}
	if overlay(e, "wb-struct", "c1", "value") != "CTO" {
		t.Fatal("target cell value missing")
	}
}

func TestCancelMidRPCLeavesUncertain(t *testing.T) {
	e := setup(t, wsA)
	e.seed(wsA, "wb-rpccancel", `[{"id":"c1","type":"waterfall","name":"C1","target_field":"founded_year","waterfall":["paidpy"]}]`, 1, 10)
	held := make(chan struct{})
	fr := &fakeRunner{fn: func(ctx context.Context, _ string, _ providers.Input) (providers.Output, error) {
		close(held)
		<-ctx.Done()
		return providers.Output{}, ctx.Err()
	}}
	d := e.deps(newSim(t, nil), nil)
	d.Runner = fr
	c := e.claim(wsA, payloadNoManifests(wsA, "wb-rpccancel", map[string]float64{"paidpy": 0.05}, nil))
	ctx, stop := e.watch(c)
	defer stop()
	go func() {
		<-held
		_, _ = e.pool.Exec(e.ctx, `UPDATE jobs SET status='cancelled' WHERE id=$1`, c.ID)
	}()
	if _, err := workbook.Run(ctx, d, c); err == nil {
		t.Fatal("expected the run to unwind")
	}
	if st := e.str(wsA, `SELECT status FROM workbook_spend_attempts WHERE provider='paidpy'`); st != "uncertain" {
		t.Fatalf("dispatched paid call is %q, want uncertain", st)
	}
	if n := e.one(wsA, `SELECT count(*) FROM workbook_enrichments`); n != 0 {
		t.Fatalf("cell written after cancel: %d", n)
	}
}

// A late RPC answer must not overwrite state once the job is cancelled.
func TestNoOverwriteOfCancelled(t *testing.T) {
	e := setup(t, wsA)
	e.seed(wsA, "wb-nooverwrite", `[{"id":"a1","type":"ai_formula","name":"A1","prompt":"x"}]`, 1, 10)
	held := make(chan struct{})
	py := &fakePy{fn: func(ctx context.Context, _ pyworker.ProviderCall) (pyworker.ProviderResult, error) {
		close(held)
		<-ctx.Done()
		return okValue("too late"), nil // answers after cancellation anyway
	}}
	d := e.deps(newSim(t, nil), nil)
	d.Py = py
	c := e.claim(wsA, payloadNoManifests(wsA, "wb-nooverwrite", nil, nil))
	ctx, stop := e.watch(c)
	defer stop()
	go func() {
		<-held
		_, _ = e.pool.Exec(e.ctx, `UPDATE jobs SET status='cancelled' WHERE id=$1`, c.ID)
	}()
	if _, err := workbook.Run(ctx, d, c); err == nil {
		t.Fatal("expected the run to unwind")
	}
	if n := e.one(wsA, `SELECT count(*) FROM workbook_enrichments`); n != 0 {
		t.Fatalf("a cancelled job wrote %d cells", n)
	}
	if st := e.str(wsA, `SELECT status FROM workbooks WHERE id='wb-nooverwrite'`); st == "complete" {
		t.Fatalf("cancelled run left the workbook %q", st)
	}
}

// Paid calls that go through the runner keep the Python attempt key and contract.
func TestSpendAttemptKeysMatchPythonForPyworkerCalls(t *testing.T) {
	e := setup(t, wsA)
	ids := e.seed(wsA, "wb-keys", `[{"id":"c1","type":"waterfall","name":"C1","target_field":"founded_year","waterfall":["paidpy"]}]`, 1, 10)
	fr := &fakeRunner{fn: func(context.Context, string, providers.Input) (providers.Output, error) {
		return providers.Output{Confidence: 0.7, Extra: obj("founded_year", "1999")}, nil
	}}
	d := e.deps(newSim(t, nil), nil)
	d.Runner = fr
	c := e.claim(wsA, payloadNoManifests(wsA, "wb-keys", map[string]float64{"paidpy": 0.05}, nil))
	res, err := workbook.Run(e.ctx, d, c)
	if err != nil || res.Completed != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	rowIdentity := fmt.Sprintf("row:%d", ids[0])
	wantKey := spend.AttemptKey(wsA, "wb-keys", c.ID, rowIdentity, "c1", "paidpy")
	if got := e.str(wsA, `SELECT attempt_key FROM workbook_spend_attempts WHERE provider='paidpy'`); got != wantKey {
		t.Fatalf("attempt key %q want %q", got, wantKey)
	}
	// A non-declarative provider's contract hashes the whole hydrated row (Python
	// _operation_contract), including the identity keys but not the contract-inputs key.
	var colCfg map[string]any
	_ = json.Unmarshal([]byte(`{"id":"c1","type":"waterfall","name":"C1","target_field":"founded_year","waterfall":["paidpy"]}`), &colCfg)
	_, want, err := spend.ContractHash(spend.Reservation{WorkbookID: "wb-keys", RunID: fmt.Sprintf("job:%d", c.ID), RowIdentity: rowIdentity,
		ColumnID: "c1", Provider: "paidpy", ExposureMicroUSD: 50000, CellLimitMicroUSD: 50000,
		CostBasis: map[string]any{"kind": "catalog_estimate", "provider": "paidpy"},
		Operation: map[string]any{"inputs": map[string]any{"company": "Acme 0", "website": "acme0.example.com",
			"id": int64(ids[0]), "__row_id": int64(ids[0]), "__lead_id": nil}, "column": colCfg}})
	if err != nil {
		t.Fatal(err)
	}
	if got := e.str(wsA, `SELECT contract_hash FROM workbook_spend_attempts WHERE provider='paidpy'`); got != want {
		t.Fatalf("contract hash %q want %q", got, want)
	}
}

func TestRedisCellEventsMatchPython(t *testing.T) {
	e := setup(t, wsA)
	cols := `[{"id":"f1","type":"formula","name":"F1","formula":"upper({company})"},{"id":"a1","type":"ai_formula","name":"A1","prompt":""}]`
	ids := e.seed(wsA, "wb-ev", cols, 1, 10)
	pub := &recPub{}
	d := e.deps(newSim(t, nil), pub)
	d.Py = &fakePy{fn: func(context.Context, pyworker.ProviderCall) (pyworker.ProviderResult, error) {
		t.Error("an empty prompt must not call python")
		return pyworker.ProviderResult{}, nil
	}}
	c := e.claim(wsA, payloadNoManifests(wsA, "wb-ev", nil, nil))
	if _, err := workbook.Run(e.ctx, d, c); err != nil {
		t.Fatal(err)
	}
	var seq []string
	for _, m := range pub.msgs {
		if m["type"] != "cell_update" {
			continue
		}
		seq = append(seq, fmt.Sprintf("%v:%v", m["colId"], m["status"]))
		switch m["status"] {
		case "running":
			if keys(m) != "colId,leadId,rowId,status,type,value" {
				t.Fatalf("running keys %s", keys(m))
			}
		case "complete":
			if keys(m) != "colId,error,leadId,provenance,provider,research,rowId,status,type,value,verify_status" ||
				m["value"] != "ACME 0" || m["provider"] != "formula" || m["rowId"] != int64(ids[0]) && m["rowId"] != ids[0] {
				t.Fatalf("complete event %v", m)
			}
		}
	}
	// no_prompt: running is broadcast, the error cell is written without a result event (Python).
	if strings.Join(seq, ",") != "f1:running,f1:complete,a1:running" {
		t.Fatalf("event sequence %v", seq)
	}
	if overlay(e, "wb-ev", "a1", "error") != "no_prompt" {
		t.Fatal("no_prompt cell missing")
	}
}

// Free providers behind the runner write exactly one provider_stats row per
// call: the runner owns the ledger, the worker must not add its own.
func TestRunnerPathWritesNoLedgerRowOfItsOwn(t *testing.T) {
	e := setup(t, wsA)
	e.seed(wsA, "wb-ledger", `[{"id":"c1","type":"waterfall","name":"C1","target_field":"founded_year","waterfall":["freepy"]}]`, 1, 10)
	fr := &fakeRunner{fn: func(context.Context, string, providers.Input) (providers.Output, error) {
		return providers.Output{Confidence: 0.7, Extra: obj("founded_year", "1999")}, nil
	}}
	d := e.deps(newSim(t, nil), nil)
	d.Runner = fr
	c := e.claim(wsA, payloadNoManifests(wsA, "wb-ledger", map[string]float64{"freepy": 0}, nil))
	if res, err := workbook.Run(e.ctx, d, c); err != nil || res.Completed != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if n := e.one(wsA, `SELECT count(*) FROM provider_stats WHERE provider='freepy'`); n != 0 {
		t.Fatalf("worker wrote %d ledger rows although the runner owns the ledger", n)
	}
	_ = http.StatusOK
}

// A keyless paid provider is skipped as "not configured" before any hold; the
// next provider runs (Python never created an attempt for a missing key).
func TestKeylessPaidProviderIsSkippedWithoutAttempt(t *testing.T) {
	e := setup(t, wsA)
	e.seed(wsA, "wb-keyless", `[{"id":"c1","type":"waterfall","name":"C1","target_field":"founded_year","waterfall":["paidnokey","freepy"]}]`, 1, 10)
	fr := &fakeRunner{keyless: map[string]bool{"paidnokey": true}, fn: func(_ context.Context, p string, _ providers.Input) (providers.Output, error) {
		if p == "paidnokey" {
			return providers.Output{Error: "not configured"}, nil
		}
		return providers.Output{Confidence: 0.7, Extra: obj("founded_year", "1999")}, nil
	}}
	d := e.deps(newSim(t, nil), nil)
	d.Runner = fr
	c := e.claim(wsA, payloadNoManifests(wsA, "wb-keyless", map[string]float64{"paidnokey": 0.05, "freepy": 0}, nil))
	res, err := workbook.Run(e.ctx, d, c)
	if err != nil || res.Completed != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if n := e.one(wsA, `SELECT count(*) FROM workbook_spend_attempts`); n != 0 {
		t.Fatalf("a keyless provider created %d attempts", n)
	}
	if strings.Join(fr.calls, ",") != "freepy" {
		t.Fatalf("provider calls %v", fr.calls)
	}
}

// A provider the runner does not know cannot reach a vendor: the attempt
// settles at 0 and the waterfall moves on.
func TestUnknownPaidProviderSettlesAtZero(t *testing.T) {
	e := setup(t, wsA)
	e.seed(wsA, "wb-unkpaid", `[{"id":"c1","type":"waterfall","name":"C1","target_field":"founded_year","waterfall":["ghostpaid","freepy"]}]`, 1, 10)
	fr := &fakeRunner{fn: func(_ context.Context, p string, _ providers.Input) (providers.Output, error) {
		if p == "ghostpaid" {
			return providers.Output{}, pyworker.ErrUnknownProvider
		}
		return providers.Output{Confidence: 0.7, Extra: obj("founded_year", "1999")}, nil
	}}
	d := e.deps(newSim(t, nil), nil)
	d.Runner = fr
	c := e.claim(wsA, payloadNoManifests(wsA, "wb-unkpaid", map[string]float64{"ghostpaid": 0.05, "freepy": 0}, nil))
	res, err := workbook.Run(e.ctx, d, c)
	if err != nil || res.Completed != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if st := e.str(wsA, `SELECT status FROM workbook_spend_attempts WHERE provider='ghostpaid'`); st != "settled" {
		t.Fatalf("unknown provider attempt is %q, want settled", st)
	}
	if n := e.one(wsA, `SELECT coalesce(sum(settled_microusd),0) FROM workbook_spend_attempts WHERE provider='ghostpaid'`); n != 0 {
		t.Fatalf("charged %d", n)
	}
}
