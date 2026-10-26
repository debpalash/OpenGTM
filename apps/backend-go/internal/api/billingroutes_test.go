//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stripe/stripe-go/v87/webhook"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

const whSecret = "whsec_route_test_only"

func billingCfg(enabled bool, key, apiBase string) config.APIConfig {
	return config.APIConfig{AppEnv: "test", BillingEnabled: enabled, StripeSecretKey: key, StripeWebhookSecret: whSecret, StripeAPIBase: apiBase}
}

func newBillingEnv(t *testing.T, cfg config.APIConfig) mcpEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	st := identity.NewStore(pool)
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(pool, nil))
	RegisterBilling(r, Deps{Pool: pool, Signer: testSigner, Identity: st, Config: cfg})
	return mcpEnv{env: env{h: r.Handler(), st: st}, pool: pool}
}

func (e mcpEnv) cleanCredits(t *testing.T, ws ...string) {
	t.Helper()
	clean := func() {
		for _, w := range ws {
			_ = tenancy.InWorkspace(context.Background(), e.pool, w, func(tx pgx.Tx) error {
				_, _ = tx.Exec(context.Background(), `DELETE FROM credit_ledger_entries WHERE workspace_id=$1`, w)
				_, _ = tx.Exec(context.Background(), `DELETE FROM workspace_credits WHERE workspace_id=$1`, w)
				return nil
			})
		}
	}
	clean()
	t.Cleanup(clean)
}

func (e mcpEnv) balanceOf(t *testing.T, ws string) (bal float64, entries int) {
	t.Helper()
	err := tenancy.InWorkspace(context.Background(), e.pool, ws, func(tx pgx.Tx) error {
		_ = tx.QueryRow(context.Background(), `SELECT COALESCE(max(balance_usd),0) FROM workspace_credits WHERE workspace_id=$1`, ws).Scan(&bal)
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM credit_ledger_entries WHERE workspace_id=$1`, ws).Scan(&entries)
	})
	if err != nil {
		t.Fatal(err)
	}
	return bal, entries
}

func completedEvent(ws, session, credit, payStatus string) []byte {
	ps := ""
	if payStatus != "" {
		ps = `"payment_status":"` + payStatus + `",`
	}
	return []byte(`{"id":"evt_` + session + `","object":"event","api_version":"2019-02-11","created":1,` +
		`"type":"checkout.session.completed","data":{"object":{"id":"` + session + `","object":"checkout.session",` + ps +
		`"metadata":{"workspace_id":"` + ws + `","credit_usd":"` + credit + `"}}}}`)
}

func signedHdr(payload []byte) map[string]string {
	return map[string]string{"Stripe-Signature": webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{
		Payload: payload, Secret: whSecret, Timestamp: time.Now()}).Header}
}

func (e mcpEnv) hook(body []byte, hdr map[string]string) (int, map[string]any) {
	rec, _ := e.do("POST", "/api/billing/webhook", "application/json", string(body), hdr)
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	return rec.Code, m
}

// ---- balance ----

func TestBalanceMatchesFixture(t *testing.T) {
	e := newBillingEnv(t, billingCfg(true, "", ""))
	_, hdr := e.admin(t, "t9-bal-ws", "t9-bal-adm")
	e.cleanCredits(t, "t9-bal-ws")

	fx := contract.Load(t, "billing/balance_empty_enabled")
	_, got := e.do("GET", "/api/billing/balance", "", "", hdr)
	contract.AssertEqual(t, got, fx.Response, contract.IgnorePaths("$.workspace_id"))

	// Rows equivalent to the ones the fixture recorder inserted (ids differ).
	err := tenancy.InWorkspace(context.Background(), e.pool, "t9-bal-ws", func(tx pgx.Tx) error {
		ctx := context.Background()
		if _, err := tx.Exec(ctx, `INSERT INTO workspace_credits (workspace_id, balance_usd) VALUES ('t9-bal-ws', 24.87655)`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO credit_ledger_entries (workspace_id, amount_usd, reason, run_id, idempotency_key, created_at) VALUES
			('t9-bal-ws', 25.0, 'stripe_topup', NULL, 'stripe:t9-bal-1', '2026-08-29 10:40:00'),
			('t9-bal-ws', -0.12345, 'run_debit', 'run-fixture-1', 'run:t9-bal-1', '2026-08-29 10:41:00')`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	fx = contract.Load(t, "billing/balance_with_entries")
	_, got = e.do("GET", "/api/billing/balance", "", "", hdr)
	contract.AssertEqual(t, got, fx.Response, contract.IgnorePaths("$.workspace_id",
		"$.entries[0].id", "$.entries[1].id", "$.entries[0].workspace_id", "$.entries[1].workspace_id"))
	var m struct {
		Entries []struct{ ID int }
	}
	_ = json.Unmarshal(got.Body, &m)
	if len(m.Entries) != 2 || m.Entries[0].ID <= m.Entries[1].ID {
		t.Fatalf("entries not newest-first: %s", got.Body)
	}
}

func TestBalanceDisabledAndNoToken(t *testing.T) {
	e := newBillingEnv(t, billingCfg(false, "", ""))
	_, hdr := e.admin(t, "t9-bal2-ws", "t9-bal2-adm")
	e.cleanCredits(t, "t9-bal2-ws")
	fx := contract.Load(t, "billing/balance_empty_disabled")
	_, got := e.do("GET", "/api/billing/balance", "", "", hdr)
	contract.AssertEqual(t, got, fx.Response, contract.IgnorePaths("$.workspace_id"))
	fx = contract.Load(t, "billing/balance_no_token")
	_, got = e.do("GET", "/api/billing/balance", "", "", nil)
	contract.AssertEqual(t, got, fx.Response)
}

func TestBalanceTenantIsolation(t *testing.T) {
	e := newBillingEnv(t, billingCfg(true, "", ""))
	_, hdrA := e.admin(t, "t9-iso-a", "t9-iso-adm-a")
	_, hdrB := e.admin(t, "t9-iso-b", "t9-iso-adm-b")
	e.cleanCredits(t, "t9-iso-a", "t9-iso-b")
	code, _ := e.hook(completedEvent("t9-iso-a", "cs_iso_1", "40.0", "paid"), signedHdr(completedEvent("t9-iso-a", "cs_iso_1", "40.0", "paid")))
	if code != 200 {
		t.Fatalf("webhook %d", code)
	}
	_, a := e.do("GET", "/api/billing/balance", "", "", hdrA)
	_, b := e.do("GET", "/api/billing/balance", "", "", hdrB)
	if !strings.Contains(string(a.Body), `"balance_usd":40.0`) {
		t.Fatalf("A: %s", a.Body)
	}
	if !strings.Contains(string(b.Body), `"balance_usd":0.0`) || !strings.Contains(string(b.Body), `"entries":[]`) {
		t.Fatalf("B sees A's ledger: %s", b.Body)
	}
	// The pool role is subject to FORCE RLS: without a workspace GUC nothing is visible.
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM credit_ledger_entries`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("unscoped read saw %d rows (err %v)", n, err)
	}
}

// ---- checkout ----

func TestCheckoutAmountZero422(t *testing.T) {
	e := newBillingEnv(t, billingCfg(false, "", ""))
	_, hdr := e.admin(t, "t9-co-ws", "t9-co-adm")
	for _, name := range []string{"checkout_amount_zero", "checkout_amount_negative", "checkout_amount_missing",
		"checkout_amount_not_number", "checkout_amount_null", "checkout_url_not_string"} {
		fx := contract.Load(t, "billing/"+name)
		_, got := e.do("POST", "/api/billing/checkout", "application/json", string(fx.Request.Body), hdr)
		t.Run(name, func(t *testing.T) { contract.AssertEqual(t, got, fx.Response) })
	}
}

func TestCheckoutBillingDisabled400(t *testing.T) {
	e := newBillingEnv(t, billingCfg(false, "", ""))
	_, hdr := e.admin(t, "t9-co2-ws", "t9-co2-adm")
	for _, name := range []string{"checkout_disabled", "checkout_amount_numeric_string"} {
		fx := contract.Load(t, "billing/"+name)
		_, got := e.do("POST", "/api/billing/checkout", "application/json", string(fx.Request.Body), hdr)
		contract.AssertEqual(t, got, fx.Response)
	}
}

func TestCheckoutUnconfigured503(t *testing.T) {
	e := newBillingEnv(t, billingCfg(true, "", ""))
	_, hdr := e.admin(t, "t9-co3-ws", "t9-co3-adm")
	fx := contract.Load(t, "billing/checkout_unconfigured")
	_, got := e.do("POST", "/api/billing/checkout", "application/json", string(fx.Request.Body), hdr)
	contract.AssertEqual(t, got, fx.Response)
}

func TestCheckoutRequiresAuth(t *testing.T) {
	e := newBillingEnv(t, billingCfg(true, "sk_test_x", ""))
	rec, _ := e.do("POST", "/api/billing/checkout", "application/json", `{"amount_usd":5}`, nil)
	if rec.Code != 401 {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestCheckoutSuccessUsesWorkspaceAndDefaults(t *testing.T) {
	var form map[string][]string
	stripe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form = r.PostForm
		_, _ = w.Write([]byte(`{"id":"cs_test_77","object":"checkout.session","url":"https://checkout.stripe.com/c/pay/cs_test_77"}`))
	}))
	defer stripe.Close()
	e := newBillingEnv(t, billingCfg(true, "sk_test_x", stripe.URL))
	_, hdr := e.admin(t, "t9-co4-ws", "t9-co4-adm")
	rec, got := e.do("POST", "/api/billing/checkout", "application/json", `{"amount_usd":25}`, hdr)
	if rec.Code != 200 || string(got.Body) != `{"session_id":"cs_test_77","url":"https://checkout.stripe.com/c/pay/cs_test_77"}` {
		t.Fatalf("%d %s", rec.Code, got.Body)
	}
	if form["metadata[workspace_id]"][0] != "t9-co4-ws" || form["metadata[credit_usd]"][0] != "25.0" ||
		form["success_url"][0] != "https://app.yupcha.com/billing?status=success" ||
		form["cancel_url"][0] != "https://app.yupcha.com/billing?status=cancel" {
		t.Fatalf("form %v", form)
	}
}

func TestCheckoutStripeErrorIs502Generic(t *testing.T) {
	stripe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(402)
		_, _ = w.Write([]byte(`{"error":{"type":"card_error","message":"LEAKY-STRIPE-MESSAGE sk_live_abc"}}`))
	}))
	defer stripe.Close()
	e := newBillingEnv(t, billingCfg(true, "sk_test_x", stripe.URL))
	_, hdr := e.admin(t, "t9-co5-ws", "t9-co5-adm")
	rec, _ := e.do("POST", "/api/billing/checkout", "application/json", `{"amount_usd":25}`, hdr)
	if rec.Code != 502 {
		t.Fatalf("status %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "LEAKY") || strings.Contains(rec.Body.String(), "sk_") {
		t.Fatalf("stripe detail reached client: %s", rec.Body)
	}
}

func TestStripeAPIBaseIgnoredOutsideTestEnv(t *testing.T) {
	called := false
	stripe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer stripe.Close()
	cfg := billingCfg(true, "sk_test_x", stripe.URL)
	cfg.AppEnv = "production"
	e := newBillingEnv(t, cfg)
	_, hdr := e.admin(t, "t9-co6-ws", "t9-co6-adm")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest("POST", "/api/billing/checkout", strings.NewReader(`{"amount_usd":25}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	e.h.ServeHTTP(httptest.NewRecorder(), req)
	if called {
		t.Fatal("STRIPE_API_BASE override honoured outside APP_ENV=test")
	}
}

// ---- webhook ----

func TestWebhookDisabledAndUnconfigured(t *testing.T) {
	e := newBillingEnv(t, billingCfg(false, "", ""))
	fx := contract.Load(t, "billing/webhook_disabled")
	_, got := e.do("POST", "/api/billing/webhook", "application/json", string(fx.Request.Body), map[string]string{"Stripe-Signature": "t=1,v1=00"})
	contract.AssertEqual(t, got, fx.Response)

	e2 := newBillingEnv(t, billingCfg(true, "", ""))
	fx = contract.Load(t, "billing/webhook_unconfigured")
	_, got = e2.do("POST", "/api/billing/webhook", "application/json", string(fx.Request.Body), map[string]string{"Stripe-Signature": "t=1,v1=00"})
	contract.AssertEqual(t, got, fx.Response)
}

func TestWebhookBadSignatureMatchesFixtures(t *testing.T) {
	e := newBillingEnv(t, billingCfg(true, "sk_test_x", ""))
	fx := contract.Load(t, "billing/webhook_bad_signature")
	_, got := e.do("POST", "/api/billing/webhook", "application/json", string(fx.Request.Body), map[string]string{"Stripe-Signature": "t=1,v1=00"})
	contract.AssertEqual(t, got, fx.Response)
	fx = contract.Load(t, "billing/webhook_no_signature")
	_, got = e.do("POST", "/api/billing/webhook", "application/json", string(fx.Request.Body), nil)
	contract.AssertEqual(t, got, fx.Response)
}

func TestWebhookCreditsMetadataWorkspaceOnly(t *testing.T) {
	e := newBillingEnv(t, billingCfg(true, "sk_test_x", ""))
	_, hdrB := e.admin(t, "t9-wh-b", "t9-wh-adm-b")
	e.workspace(t, "t9-wh-a", e.mustUserByName(t, "t9-wh-adm-b").ID)
	e.cleanCredits(t, "t9-wh-a", "t9-wh-b")
	p := completedEvent("t9-wh-a", "cs_wh_1", "12.5", "paid")
	h := signedHdr(p)
	h["X-Workspace-Id"] = "t9-wh-b" // caller-controlled headers must not steer the credit
	h["Authorization"] = hdrB["Authorization"]
	code, m := e.hook(p, h)
	if code != 200 || m["status"] != "ok" || m["credited_usd"] != 12.5 || m["balance_usd"] != 12.5 || m["idempotent_replay"] != false {
		t.Fatalf("%d %v", code, m)
	}
	if bal, n := e.balanceOf(t, "t9-wh-a"); bal != 12.5 || n != 1 {
		t.Fatalf("A: %v %d", bal, n)
	}
	if bal, n := e.balanceOf(t, "t9-wh-b"); bal != 0 || n != 0 {
		t.Fatalf("B credited: %v %d", bal, n)
	}
}

func TestWebhookRedeliveryDoesNotDoubleCredit(t *testing.T) {
	e := newBillingEnv(t, billingCfg(true, "sk_test_x", ""))
	e.admin(t, "t9-wh2-ws", "t9-wh2-adm")
	e.cleanCredits(t, "t9-wh2-ws")
	p := completedEvent("t9-wh2-ws", "cs_wh_2", "30", "paid")
	_, first := e.hook(p, signedHdr(p))
	code, second := e.hook(p, signedHdr(p))
	if first["idempotent_replay"] != false || code != 200 || second["idempotent_replay"] != true ||
		second["credited_usd"] != 0.0 || second["balance_usd"] != 30.0 {
		t.Fatalf("first %v second %v", first, second)
	}
	if bal, n := e.balanceOf(t, "t9-wh2-ws"); bal != 30 || n != 1 {
		t.Fatalf("balance %v entries %d", bal, n)
	}
	rec, _ := e.do("POST", "/api/billing/webhook", "application/json", string(p), signedHdr(p))
	if !strings.Contains(rec.Body.String(), `"credited_usd":0.0`) || !strings.Contains(rec.Body.String(), `"balance_usd":30.0`) {
		t.Fatalf("python-style float repr expected: %s", rec.Body)
	}
}

func TestWebhookDisabledAndUnpaidAndMalformed(t *testing.T) {
	e := newBillingEnv(t, billingCfg(true, "sk_test_x", ""))
	e.admin(t, "t9-wh3-ws", "t9-wh3-adm")
	e.cleanCredits(t, "t9-wh3-ws")

	other := []byte(`{"id":"evt_o","object":"event","type":"invoice.paid","data":{"object":{"id":"in_1"}}}`)
	if code, m := e.hook(other, signedHdr(other)); code != 200 || m["status"] != "ignored" || m["type"] != "invoice.paid" {
		t.Fatalf("other type: %d %v", code, m)
	}
	unpaid := completedEvent("t9-wh3-ws", "cs_u", "5", "unpaid")
	if code, m := e.hook(unpaid, signedHdr(unpaid)); code != 200 || m["status"] != "ignored" || m["reason"] != "unpaid" {
		t.Fatalf("unpaid: %d %v", code, m)
	}
	// payment_status absent is credited (Python: not in (None, "paid") is false).
	absent := completedEvent("t9-wh3-ws", "cs_abs", "5", "")
	if code, m := e.hook(absent, signedHdr(absent)); code != 200 || m["status"] != "ok" {
		t.Fatalf("absent payment_status: %d %v", code, m)
	}
	for name, p := range map[string][]byte{
		"zero credit":   completedEvent("t9-wh3-ws", "cs_m1", "0", "paid"),
		"negative":      completedEvent("t9-wh3-ws", "cs_m2", "-3", "paid"),
		"not a number":  completedEvent("t9-wh3-ws", "cs_m3", "abc", "paid"),
		"nan":           completedEvent("t9-wh3-ws", "cs_m4", "nan", "paid"),
		"no workspace":  completedEvent("", "cs_m5", "5", "paid"),
		"no metadata":   []byte(`{"id":"evt_m","object":"event","type":"checkout.session.completed","data":{"object":{"id":"cs_m6","payment_status":"paid"}}}`),
		"no session id": []byte(`{"id":"evt_m","object":"event","type":"checkout.session.completed","data":{"object":{"payment_status":"paid","metadata":{"workspace_id":"t9-wh3-ws","credit_usd":"5"}}}}`),
	} {
		code, m := e.hook(p, signedHdr(p))
		if code != 400 || m["detail"] != "Malformed checkout session" {
			t.Errorf("%s: %d %v", name, code, m)
		}
	}
	if bal, n := e.balanceOf(t, "t9-wh3-ws"); bal != 5 || n != 1 {
		t.Fatalf("only the absent-status event may credit: %v %d", bal, n)
	}
}

func TestWebhookUsesRawBody(t *testing.T) {
	e := newBillingEnv(t, billingCfg(true, "sk_test_x", ""))
	e.admin(t, "t9-wh4-ws", "t9-wh4-adm")
	e.cleanCredits(t, "t9-wh4-ws")
	p := []byte("{ \"id\" : \"evt_raw\",\n \"object\":\"event\", \"type\":\"checkout.session.completed\",\n" +
		" \"data\":{ \"object\":{\"id\":\"cs_raw\", \"payment_status\":\"paid\",\n\t\"metadata\":{\"credit_usd\":\"2.5\",\"workspace_id\":\"t9-wh4-ws\"}}} }\n")
	if code, m := e.hook(p, signedHdr(p)); code != 200 || m["status"] != "ok" {
		t.Fatalf("%d %v", code, m)
	}
}

func TestWebhookMismatchedAPIVersionCredits(t *testing.T) {
	e := newBillingEnv(t, billingCfg(true, "sk_test_x", ""))
	e.admin(t, "t9-wh5-ws", "t9-wh5-adm")
	e.cleanCredits(t, "t9-wh5-ws")
	p := []byte(strings.Replace(string(completedEvent("t9-wh5-ws", "cs_ver", "9", "paid")), "2019-02-11", "2099-01-01", 1))
	if code, m := e.hook(p, signedHdr(p)); code != 200 || m["status"] != "ok" {
		t.Fatalf("%d %v", code, m)
	}
}

func TestWebhookTamperedAndStaleRejected(t *testing.T) {
	e := newBillingEnv(t, billingCfg(true, "sk_test_x", ""))
	e.admin(t, "t9-wh6-ws", "t9-wh6-adm")
	e.cleanCredits(t, "t9-wh6-ws")
	p := completedEvent("t9-wh6-ws", "cs_t", "5", "paid")
	h := signedHdr(p)
	tampered := []byte(strings.Replace(string(p), `"5"`, `"5000"`, 1))
	if code, m := e.hook(tampered, h); code != 400 || m["detail"] != "Invalid webhook signature" {
		t.Fatalf("tampered: %d %v", code, m)
	}
	stale := map[string]string{"Stripe-Signature": webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{
		Payload: p, Secret: whSecret, Timestamp: time.Now().Add(-time.Hour)}).Header}
	if code, _ := e.hook(p, stale); code != 400 {
		t.Fatalf("stale: %d", code)
	}
	if bal, n := e.balanceOf(t, "t9-wh6-ws"); bal != 0 || n != 0 {
		t.Fatalf("credited: %v %d", bal, n)
	}
}

func TestWebhookConcurrentRedeliveryCreditsOnce(t *testing.T) {
	e := newBillingEnv(t, billingCfg(true, "sk_test_x", ""))
	e.admin(t, "t9-wh7-ws", "t9-wh7-adm")
	e.cleanCredits(t, "t9-wh7-ws")
	p := completedEvent("t9-wh7-ws", "cs_conc", "4", "paid")
	done := make(chan string, 8)
	for i := 0; i < 8; i++ {
		go func() {
			_, m := e.hook(p, signedHdr(p))
			done <- fmt.Sprint(m["idempotent_replay"])
		}()
	}
	fresh := 0
	for i := 0; i < 8; i++ {
		if <-done == "false" {
			fresh++
		}
	}
	if bal, n := e.balanceOf(t, "t9-wh7-ws"); fresh != 1 || bal != 4 || n != 1 {
		t.Fatalf("fresh=%d balance=%v entries=%d", fresh, bal, n)
	}
}

func TestWebhookNeedsNoAuth(t *testing.T) {
	e := newBillingEnv(t, billingCfg(true, "sk_test_x", ""))
	e.admin(t, "t9-wh8-ws", "t9-wh8-adm")
	e.cleanCredits(t, "t9-wh8-ws")
	p := completedEvent("t9-wh8-ws", "cs_noauth", "1", "paid")
	if code, m := e.hook(p, signedHdr(p)); code != 200 || m["status"] != "ok" {
		t.Fatalf("%d %v", code, m)
	}
}
