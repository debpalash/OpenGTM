//go:build integration

package workbook_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

// jobRow reads the job columns a hand-back must set.
func (e *env) jobRow(id int64) (executor, status, errText string, retries int, workerNull bool) {
	e.t.Helper()
	var worker *string
	var er *string
	if err := e.pool.QueryRow(e.ctx, `SELECT executor, status, error, retry_count, worker_id FROM jobs WHERE id=$1`, id).
		Scan(&executor, &status, &er, &retries, &worker); err != nil {
		e.t.Fatal(err)
	}
	if er != nil {
		errText = *er
	}
	return executor, status, errText, retries, worker == nil
}

func mutate(raw []byte, fn func(p map[string]any)) []byte {
	var p map[string]any
	_ = json.Unmarshal(raw, &p)
	fn(p)
	out, _ := json.Marshal(p)
	return out
}

// Every deterministic refusal hands the job back to Python: executor python,
// pending, no retry spent, workbook not failed, no provider call.
func TestRefusalsHandBackToPython(t *testing.T) {
	good := func(wb string) []byte { return payloadFor(wsA, wb, map[string]float64{"free1": 0}, nil) }
	cases := []struct {
		name    string
		columns string
		payload func(wb string) []byte
		srv     workbook.ManifestSource
		reason  string
	}{
		{"payload v2", "[" + colJSON("c1", "free1") + "]", func(wb string) []byte {
			return mutate(good(wb), func(p map[string]any) { p["go"].(map[string]any)["version"] = 2 })
		}, nil, "go.version"},
		{"missing go block", "[" + colJSON("c1", "free1") + "]", func(wb string) []byte {
			return mutate(good(wb), func(p map[string]any) { delete(p, "go") })
		}, nil, "missing go block"},
		{"unsupported column", `[{"id":"c1","type":"ai_formula","name":"c1","prompt":"x"}]`, good, nil, "not supported"},
		{"edited column (refs)", `[{"id":"c1","type":"waterfall","target_field":"founded_year","waterfall":["free1"],"prompt":"{Other}"}]`, good, nil, "references other columns"},
		{"unknown provider", "[" + colJSON("c1", "ghost") + "]", good, nil, "unknown_provider"},
		{"manifest hash mismatch", "[" + colJSON("c1", "free1") + "]", good,
			manifestSrv{sha: map[string]string{"free1": "0000"}}, "manifest_mismatch"},
		{"manifest not served", "[" + colJSON("c1", "free1") + "]", good,
			manifestSrv{missing: map[string]bool{"free1": true}}, "manifest_mismatch"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t, wsA)
			wb := "wb-hb" + string(rune('a'+i))
			e.seed(wsA, wb, tc.columns, 1, 0)
			e.inWS(wsA, func(tx pgx.Tx) error {
				_, err := tx.Exec(e.ctx, `UPDATE workbooks SET status='running' WHERE id=$1`, wb)
				return err
			})
			s := newSim(t, nil)
			d := e.deps(s, nil)
			if tc.srv != nil {
				d.Manifests = tc.srv
			}
			c := e.claim(wsA, tc.payload(wb))
			_, err := workbook.Run(e.ctx, d, c)
			if !errors.Is(err, workbook.ErrHandedBack) || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("want hand-back containing %q, got %v", tc.reason, err)
			}
			exec, status, msg, retries, noWorker := e.jobRow(c.ID)
			if exec != "python" || status != "pending" || retries != 0 || !noWorker || !strings.HasPrefix(msg, "handed back to python: ") {
				t.Fatalf("job row: %s %s %q retries=%d workerNull=%v", exec, status, msg, retries, noWorker)
			}
			if st := e.str(wsA, `SELECT status FROM workbooks WHERE id=$1`, wb); st != "running" {
				t.Fatalf("workbook was changed to %q; a hand-back must not fail it", st)
			}
			if s.total() != 0 || e.one(wsA, `SELECT count(*) FROM workbook_enrichments`) != 0 {
				t.Fatal("work happened before a refusal")
			}
		})
	}
}

// S2: a payload whose manifest points at an attacker URL is never executed
// (the served copy is), and when the hash does not match the job is handed back.
func TestTamperedPayloadManifestNeverExecuted(t *testing.T) {
	e := setup(t, wsA)
	e.seed(wsA, "wb-tamper", "["+colJSON("c1", "free1")+"]", 1, 0)
	evil := strings.ReplaceAll(manifestJSON("free1"), "sim.provider.example", "evil.example")
	raw := mutate(payloadFor(wsA, "wb-tamper", map[string]float64{"free1": 0}, nil), func(p map[string]any) {
		p["go"].(map[string]any)["manifests"].(map[string]any)["free1"] = json.RawMessage(evil)
	})
	s := newSim(t, nil)

	// (a) The hash still matches the served manifest: the SERVED copy runs, not the payload's.
	c := e.claim(wsA, raw)
	res, err := workbook.Run(e.ctx, e.deps(s, nil), c)
	if err != nil || res.Completed != 1 || s.count("free1") != 1 {
		t.Fatalf("served manifest must run: %+v calls=%d err=%v", res, s.count("free1"), err)
	}

	// (b) The payload's hash differs from the served one: handed back, no call.
	e2 := e
	e2.seed(wsA, "wb-tamper2", "["+colJSON("c1", "free1")+"]", 1, 0)
	raw2 := mutate(payloadFor(wsA, "wb-tamper2", map[string]float64{"free1": 0}, nil), func(p map[string]any) {
		g := p["go"].(map[string]any)
		g["manifests"].(map[string]any)["free1"] = json.RawMessage(evil)
		g["manifest_sha256"].(map[string]any)["free1"] = "feedface"
	})
	s2 := newSim(t, nil)
	c2 := e2.claim(wsA, raw2)
	_, err = workbook.Run(e2.ctx, e2.deps(s2, nil), c2)
	if !errors.Is(err, workbook.ErrHandedBack) || !strings.Contains(err.Error(), "manifest_mismatch") || s2.total() != 0 {
		t.Fatalf("tampered hash must hand back: %v calls=%d", err, s2.total())
	}
	exec, status, _, retries, _ := e2.jobRow(c2.ID)
	if exec != "python" || status != "pending" || retries != 0 {
		t.Fatalf("%s %s %d", exec, status, retries)
	}
}
