//go:build integration

package workbook_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

func TestPaidMissIsAccountingUncertain(t *testing.T) {
	e := setup(t, wsA)
	e.seed(wsA, "wb-miss", "["+colJSON("c1", "p1", "p2")+"]", 1, 10)
	s := newSim(t, func(_ string, _ int, w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"v":null}`) })
	c := e.claim(wsA, payloadFor(wsA, "wb-miss", map[string]float64{"p1": 0.05, "p2": 0.05}, nil))
	res, err := workbook.Run(e.ctx, e.deps(s, nil), c)
	if err != nil || res.Errors != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if s.count("p2") != 0 {
		t.Fatal("waterfall must stop at an uncertain paid attempt")
	}
	if st := e.str(wsA, `SELECT status FROM workbook_spend_attempts WHERE provider='p1'`); st != "uncertain" {
		t.Fatalf("attempt %s", st)
	}
	if msg := e.str(wsA, `SELECT error FROM workbook_enrichments`); msg != "accounting_uncertain" {
		t.Fatalf("error %q", msg)
	}
	if got := e.spentUSD(wsA, "wb-miss"); got != 0 {
		t.Fatalf("an unsettled attempt must not be charged: %v", got)
	}
}

func TestReservedTimeoutIsAccountingUncertain(t *testing.T) {
	e := setup(t, wsA)
	e.seed(wsA, "wb-to", "["+colJSON("c1", "p1")+"]", 1, 10)
	s := newSim(t, func(_ string, _ int, w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	})
	c := e.claim(wsA, payloadFor(wsA, "wb-to", map[string]float64{"p1": 0.05}, map[string]any{"provider_timeout": 0.15}))
	res, err := workbook.Run(e.ctx, e.deps(s, nil), c)
	if err != nil || res.Errors != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if st := e.str(wsA, `SELECT status FROM workbook_spend_attempts WHERE provider='p1'`); st != "uncertain" {
		t.Fatalf("attempt %s", st)
	}
	if msg := e.str(wsA, `SELECT error FROM workbook_enrichments`); msg != "accounting_uncertain" {
		t.Fatalf("error %q", msg)
	}
	if n := e.one(wsA, `SELECT count(*) FROM provider_stats WHERE provider='p1' AND cooldown_until IS NOT NULL AND attempts=1 AND hits=0`); n != 1 {
		t.Fatal("timeout must bench the provider (circuit breaker)")
	}
}

func TestFreeTimeoutFallsThroughAndBenchesProvider(t *testing.T) {
	e := setup(t, wsA)
	e.seed(wsA, "wb-fto", "["+colJSON("c1", "slow", "ok")+"]", 1, 0)
	s := newSim(t, func(name string, _ int, w http.ResponseWriter, r *http.Request) {
		if name == "slow" {
			select {
			case <-r.Context().Done():
			case <-time.After(3 * time.Second):
			}
			return
		}
		fmt.Fprint(w, `{"v":"5"}`)
	})
	c := e.claim(wsA, payloadFor(wsA, "wb-fto", map[string]float64{"slow": 0, "ok": 0}, map[string]any{"provider_timeout": 0.15}))
	res, err := workbook.Run(e.ctx, e.deps(s, nil), c)
	if err != nil || res.Completed != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if n := e.one(wsA, `SELECT count(*) FROM provider_stats WHERE provider='slow' AND cooldown_until IS NOT NULL`); n != 1 {
		t.Fatal("slow provider not benched")
	}
}

func TestProviderPriceUnknownAndUnknownProvider(t *testing.T) {
	e := setup(t, wsA)
	e.seed(wsA, "wb-pu", "["+colJSON("c1", "nocost")+","+colJSON("c2", "ghost")+"]", 1, 0)
	raw := payloadFor(wsA, "wb-pu", map[string]float64{"nocost": 0}, nil)
	var p map[string]any
	_ = json.Unmarshal(raw, &p)
	delete(p["go"].(map[string]any)["costs"].(map[string]any), "nocost")
	raw, _ = json.Marshal(p)
	s := newSim(t, nil)
	c := e.claim(wsA, raw)
	res, err := workbook.Run(e.ctx, e.deps(s, nil), c)
	if err != nil || res.Errors != 2 || s.total() != 0 {
		t.Fatalf("%+v calls=%d err=%v", res, s.total(), err)
	}
	if msg := e.str(wsA, `SELECT error FROM workbook_enrichments WHERE column_id='c1'`); msg != "provider_price_unknown" {
		t.Fatalf("c1: %q", msg)
	}
	if msg := e.str(wsA, `SELECT error FROM workbook_enrichments WHERE column_id='c2'`); msg != "providers_unavailable: ghost (unknown_provider)" {
		t.Fatalf("c2: %q", msg)
	}
	if v := e.str(wsA, `SELECT enrichments::jsonb->'c2'->'skipped_providers'->0->>'reason' FROM workbook_rows`); v != "unknown_provider" {
		t.Fatalf("overlay skipped_providers: %q", v)
	}
}

func TestRetryPassRecoversTransientFailure(t *testing.T) {
	e := setup(t, wsA)
	e.seed(wsA, "wb-retry", "["+colJSON("c1", "free1")+"]", 1, 0)
	s := newSim(t, func(_ string, n int, w http.ResponseWriter, _ *http.Request) {
		if n == 1 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, `{"v":"9"}`)
	})
	c := e.claim(wsA, payloadFor(wsA, "wb-retry", map[string]float64{"free1": 0}, map[string]any{"retry_passes": 1}))
	res, err := workbook.Run(e.ctx, e.deps(s, nil), c)
	if err != nil {
		t.Fatal(err)
	}
	// Python: errors counted on pass one, then moved to completed by the retry.
	if res.Completed != 1 || res.Errors != 0 || res.Total != 1 || s.count("free1") != 2 {
		t.Fatalf("%+v calls=%d", res, s.count("free1"))
	}
	if st := e.str(wsA, `SELECT status FROM workbooks WHERE id='wb-retry'`); st != "complete" {
		t.Fatalf("status %s", st)
	}
}

func TestHandlerPersistsReceiptForOwningLease(t *testing.T) {
	e := setup(t, wsA)
	e.seed(wsA, "wb-rc", "["+colJSON("c1", "free1")+"]", 2, 0)
	s := newSim(t, nil)
	c := e.claim(wsA, payloadFor(wsA, "wb-rc", map[string]float64{"free1": 0}, nil))
	if err := workbook.Handler(e.deps(s, nil))(e.ctx, c); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	var raw []byte
	if err := e.pool.QueryRow(e.ctx, `SELECT payload::text FROM jobs WHERE id=$1`, c.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(raw, &got)
	rc, _ := got["execution_result"].(map[string]any)
	if keys(rc) != "completed,errors,recorded_at,retry_count,rows,stopped,total" ||
		rc["completed"] != 2.0 || rc["errors"] != 0.0 || rc["total"] != 2.0 || rc["rows"] != 2.0 || rc["stopped"] != false {
		t.Fatalf("receipt %v", rc)
	}
	if got["workbook_id"] != "wb-rc" || got["go"] == nil {
		t.Fatalf("payload was not preserved: %v", got)
	}
	// A reclaimed lease must not be able to write the old owner's receipt.
	if _, err := e.pool.Exec(e.ctx, `UPDATE jobs SET lease_token = lease_token + 1 WHERE id=$1`, c.ID); err != nil {
		t.Fatal(err)
	}
	ok, err := workbook.PersistReceipt(e.ctx, e.deps(s, nil), c, workbook.Result{Completed: 99})
	if err != nil || ok {
		t.Fatalf("stale lease persisted a receipt: %v %v", ok, err)
	}
}

func TestRefusesNonV1Payload(t *testing.T) {
	e := setup(t, wsA)
	e.seed(wsA, "wb-v2", "["+colJSON("c1", "free1")+"]", 1, 0)
	raw := payloadFor(wsA, "wb-v2", map[string]float64{"free1": 0}, nil)
	var p map[string]any
	_ = json.Unmarshal(raw, &p)
	p["go"].(map[string]any)["version"] = 2
	raw, _ = json.Marshal(p)
	s := newSim(t, nil)
	_, err := workbook.Run(e.ctx, e.deps(s, nil), e.claim(wsA, raw))
	if err == nil || s.total() != 0 {
		t.Fatalf("v2 payload must fail the job before any work: %v", err)
	}
	if n := e.one(wsA, `SELECT count(*) FROM workbook_enrichments`); n != 0 {
		t.Fatal("cells written")
	}
}

func TestRefusalFailsWorkbookButKeepsPauseAndCancelLeavesRunning(t *testing.T) {
	e := setup(t, wsA)
	set := func(wb, status string) {
		e.inWS(wsA, func(tx pgx.Tx) error {
			_, err := tx.Exec(e.ctx, `UPDATE workbooks SET status=$2 WHERE id=$1`, wb, status)
			return err
		})
	}
	bad := func(wb string) []byte {
		raw := payloadFor(wsA, wb, map[string]float64{"free1": 0}, nil)
		var p map[string]any
		_ = json.Unmarshal(raw, &p)
		p["go"].(map[string]any)["version"] = 2
		raw, _ = json.Marshal(p)
		return raw
	}
	s := newSim(t, nil)

	// Terminal refusal while /run left the workbook `running`: it must not stay so.
	e.seed(wsA, "wb-rf1", "["+colJSON("c1", "free1")+"]", 1, 0)
	set("wb-rf1", "running")
	if _, err := workbook.Run(e.ctx, e.deps(s, nil), e.claim(wsA, bad("wb-rf1"))); err == nil {
		t.Fatal("expected refusal")
	}
	if st := e.str(wsA, `SELECT status FROM workbooks WHERE id='wb-rf1'`); st != "failed" {
		t.Fatalf("refused run left workbook %q", st)
	}

	// A user pause wins over the failure write.
	e.seed(wsA, "wb-rf2", "["+colJSON("c1", "free1")+"]", 1, 0)
	set("wb-rf2", "paused")
	if _, err := e.pool.Exec(e.ctx, `DELETE FROM jobs`); err != nil {
		t.Fatal(err)
	}
	if _, err := workbook.Run(e.ctx, e.deps(s, nil), e.claim(wsA, bad("wb-rf2"))); err == nil {
		t.Fatal("expected refusal")
	}
	if st := e.str(wsA, `SELECT status FROM workbooks WHERE id='wb-rf2'`); st != "paused" {
		t.Fatalf("pause overwritten: %q", st)
	}

	// Context cancellation (shutdown) never writes a status: the reclaimed run resumes.
	e.seed(wsA, "wb-rf3", "["+colJSON("c1", "free1")+"]", 2, 0)
	set("wb-rf3", "running")
	if _, err := e.pool.Exec(e.ctx, `DELETE FROM jobs`); err != nil {
		t.Fatal(err)
	}
	held := make(chan struct{})
	s2 := newSim(t, func(_ string, _ int, w http.ResponseWriter, r *http.Request) {
		close(held)
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(e.ctx)
	go func() { <-held; cancel() }()
	if _, err := workbook.Run(ctx, e.deps(s2, nil), e.claim(wsA, payloadFor(wsA, "wb-rf3", map[string]float64{"free1": 0}, nil))); err == nil {
		t.Fatal("expected cancellation error")
	}
	if st := e.str(wsA, `SELECT status FROM workbooks WHERE id='wb-rf3'`); st != "running" {
		t.Fatalf("shutdown wrote status %q", st)
	}
}
