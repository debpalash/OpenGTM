package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/billing"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

const (
	maxWebhookBody       = 1 << 20
	stripeNotConfigured  = "Stripe is not configured: set STRIPE_SECRET_KEY (and STRIPE_WEBHOOK_SECRET) in the environment."
	billingDisabled      = "Billing is disabled on this deployment."
	checkoutFailed       = "Failed to create Stripe checkout session."
	defaultSuccessURL    = "https://app.yupcha.com/billing?status=success"
	defaultCancelURL     = "https://app.yupcha.com/billing?status=cancel"
	ledgerEntriesOnRoute = 50
)

// pyFloat marshals like Python's repr(float): 25.0, not 25.
type pyFloat float64

func (f pyFloat) MarshalJSON() ([]byte, error) {
	s, err := canonjson.FloatRepr(float64(f))
	return []byte(s), err
}

// RegisterBilling registers /api/billing: balance and checkout behind the
// workspace middleware, and the Stripe webhook (unauthenticated, signature
// verified, tenant taken from the signed event metadata).
func RegisterBilling(r *httpx.Router, d Deps) {
	b := billingRoutes{d: d}
	if key := d.Config.StripeSecretKey; key != "" {
		var o billing.Options
		if strings.EqualFold(strings.TrimSpace(d.Config.AppEnv), "test") {
			o.APIBase = d.Config.StripeAPIBase // tests only; production always talks to api.stripe.com
		}
		b.stripe = billing.NewStripe(key, o)
	}
	mws := []httpx.Middleware{identity.Authenticate(d.Signer, d.Identity), identity.Workspace(d.Identity)}
	r.Handle("GET /api/billing/balance", http.HandlerFunc(b.balance), mws...)
	r.Handle("POST /api/billing/checkout", http.HandlerFunc(b.checkout), mws...)
	r.Handle("POST /api/billing/webhook", http.HandlerFunc(b.webhook))
}

type billingRoutes struct {
	d      Deps
	stripe *billing.Stripe // nil when STRIPE_SECRET_KEY is unset
}

func (b billingRoutes) log() *slog.Logger {
	if b.d.Log != nil {
		return b.d.Log
	}
	return slog.Default()
}

func (b billingRoutes) internalError(w http.ResponseWriter, what string) {
	b.log().Error("billing: " + what + " failed")
	httpx.Detail(w, http.StatusInternalServerError, "Internal Server Error")
}

// ---- GET /balance ----

type ledgerEntryJSON struct {
	ID          int32   `json:"id"`
	WorkspaceID string  `json:"workspace_id"`
	AmountUSD   pyFloat `json:"amount_usd"`
	Reason      string  `json:"reason"`
	RunID       *string `json:"run_id"`
	CreatedAt   *string `json:"created_at"`
}

type balanceJSON struct {
	BillingEnabled bool              `json:"billing_enabled"`
	WorkspaceID    string            `json:"workspace_id"`
	BalanceUSD     pyFloat           `json:"balance_usd"`
	Entries        []ledgerEntryJSON `json:"entries"`
}

// naiveISO renders a naive timestamp like datetime.isoformat().
func naiveISO(t time.Time) string {
	s := t.Format("2006-01-02T15:04:05")
	if us := t.Nanosecond() / 1000; us != 0 {
		s += fmt.Sprintf(".%06d", us)
	}
	return s
}

func (b billingRoutes) balance(w http.ResponseWriter, r *http.Request) {
	c, ok := identity.WorkspaceFrom(r.Context())
	if !ok {
		b.internalError(w, "context")
		return
	}
	out := balanceJSON{BillingEnabled: b.d.Config.BillingEnabled, WorkspaceID: c.WorkspaceID, Entries: []ledgerEntryJSON{}}
	err := c.Tx(r.Context(), func(tx pgx.Tx) error {
		bal, err := billing.Balance(r.Context(), tx, c.WorkspaceID)
		if err != nil {
			return err
		}
		out.BalanceUSD = pyFloat(bal)
		entries, err := billing.RecentEntries(r.Context(), tx, c.WorkspaceID, ledgerEntriesOnRoute)
		if err != nil {
			return err
		}
		for _, e := range entries {
			j := ledgerEntryJSON{ID: e.ID, WorkspaceID: e.WorkspaceID, AmountUSD: pyFloat(e.AmountUSD), Reason: e.Reason, RunID: e.RunID}
			if e.CreatedAt != nil {
				s := naiveISO(*e.CreatedAt)
				j.CreatedAt = &s
			}
			out.Entries = append(out.Entries, j)
		}
		return nil
	})
	if err != nil {
		b.internalError(w, "balance")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// ---- POST /checkout ----

type checkoutBody struct {
	amount     float64
	successURL string
	cancelURL  string
}

// pyEcho renders a decoded JSON value as Python would echo it in a validation
// error's input: floats via repr, integers verbatim.
func pyEcho(v any) any {
	n, ok := v.(json.Number)
	if !ok {
		return v
	}
	s := n.String()
	if !strings.ContainsAny(s, ".eE") {
		return n
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsInf(f, 0) {
		return n
	}
	return pyFloat(f)
}

// pyFloatFromString mirrors pydantic's lax float parsing of a string.
func pyFloatFromString(s string) (float64, bool) {
	t := strings.TrimSpace(s)
	if strings.Contains(t, "_") {
		// Underscores are accepted only between digits ("1_0").
		for i := 0; i < len(t); i++ {
			if t[i] == '_' && (i == 0 || i == len(t)-1 || t[i-1] < '0' || t[i-1] > '9' || t[i+1] < '0' || t[i+1] > '9') {
				return 0, false
			}
		}
		t = strings.ReplaceAll(t, "_", "")
	}
	if t == "" || strings.ContainsAny(t, "xXpP") { // ParseFloat also reads hex floats; pydantic does not
		return 0, false
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return 0, false
	}
	return f, true
}

// parseCheckout validates the body like pydantic's CheckoutRequest.
func parseCheckout(obj map[string]any) (checkoutBody, []mcpFieldErr) {
	out := checkoutBody{successURL: defaultSuccessURL, cancelURL: defaultCancelURL}
	var errs []mcpFieldErr
	loc := []any{"body", "amount_usd"}
	v, present := obj["amount_usd"]
	if !present {
		errs = append(errs, mcpFieldErr{Type: "missing", Loc: loc, Msg: "Field required", Input: obj})
	} else {
		f, valid := 0.0, true
		switch x := v.(type) {
		case json.Number:
			var err error
			if f, err = strconv.ParseFloat(x.String(), 64); err != nil && !errors.Is(err, strconv.ErrRange) {
				valid = false
			}
		case string:
			if f, valid = pyFloatFromString(x); !valid {
				errs = append(errs, mcpFieldErr{Type: "float_parsing", Loc: loc, Msg: "Input should be a valid number, unable to parse string as a number", Input: x})
			}
		case bool:
			if x {
				f = 1
			}
		default:
			valid = false
		}
		switch _, isStr := v.(string); {
		case !valid && !isStr:
			errs = append(errs, mcpFieldErr{Type: "float_type", Loc: loc, Msg: "Input should be a valid number", Input: pyEcho(v)})
		case valid && !(f > 0): // also NaN
			errs = append(errs, mcpFieldErr{Type: "greater_than", Loc: loc, Msg: "Input should be greater than 0", Input: pyEcho(v), Ctx: map[string]any{"gt": pyFloat(0)}})
		case valid:
			out.amount = f
		}
	}
	if s, ok := scimStrField(obj, "success_url", &errs); ok {
		out.successURL = s
	}
	if s, ok := scimStrField(obj, "cancel_url", &errs); ok {
		out.cancelURL = s
	}
	return out, errs
}

func (b billingRoutes) checkout(w http.ResponseWriter, r *http.Request) {
	obj, errs, ok := scimDecodeObject(w, r)
	if !ok {
		return
	}
	var body checkoutBody
	if errs == nil {
		body, errs = parseCheckout(obj)
	}
	if len(errs) > 0 {
		httpx.WriteJSON(w, http.StatusUnprocessableEntity, map[string]any{"detail": errs})
		return
	}
	c, ok := identity.WorkspaceFrom(r.Context())
	if !ok {
		b.internalError(w, "context")
		return
	}
	if !b.d.Config.BillingEnabled {
		httpx.Detail(w, http.StatusBadRequest, billingDisabled)
		return
	}
	if b.stripe == nil {
		httpx.Detail(w, http.StatusServiceUnavailable, stripeNotConfigured)
		return
	}
	id, link, err := b.stripe.CreateCheckoutSession(r.Context(), body.amount, c.WorkspaceID, body.successURL, body.cancelURL)
	if err != nil {
		// The SDK error text is dropped on purpose: it can echo request data.
		b.log().Error("billing: stripe checkout failed", "workspace_id", c.WorkspaceID)
		httpx.Detail(w, http.StatusBadGateway, checkoutFailed)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"session_id": id, "url": link})
}

// ---- POST /webhook ----

type webhookOK struct {
	Status           string  `json:"status"`
	CreditedUSD      pyFloat `json:"credited_usd"`
	BalanceUSD       pyFloat `json:"balance_usd"`
	IdempotentReplay bool    `json:"idempotent_replay"`
}

func (b billingRoutes) webhook(w http.ResponseWriter, r *http.Request) {
	if !b.d.Config.BillingEnabled {
		// Don't fail Stripe's retries against a disabled deployment.
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ignored", "reason": "billing_disabled"})
		return
	}
	payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if b.stripe == nil {
		httpx.Detail(w, http.StatusServiceUnavailable, stripeNotConfigured)
		return
	}
	const badSig = "Invalid webhook signature"
	if err != nil {
		httpx.Detail(w, http.StatusBadRequest, badSig)
		return
	}
	// The raw bytes are verified as received; re-marshalled JSON would not match.
	ev, err := billing.VerifyWebhook(payload, r.Header.Get("Stripe-Signature"), b.d.Config.StripeWebhookSecret)
	if err != nil {
		b.log().Warn("billing: stripe webhook verification failed")
		httpx.Detail(w, http.StatusBadRequest, badSig)
		return
	}
	if ev.Type != "checkout.session.completed" {
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ignored", "type": string(ev.Type)})
		return
	}
	obj := map[string]any{}
	if ev.Data != nil && len(ev.Data.Raw) > 0 {
		dec := json.NewDecoder(strings.NewReader(string(ev.Data.Raw)))
		dec.UseNumber()
		_ = dec.Decode(&obj)
	}
	if ps, present := obj["payment_status"]; present && ps != nil && ps != "paid" {
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ignored", "reason": "unpaid"})
		return
	}
	workspaceID, sessionID, credit, valid := checkoutFromEvent(obj)
	if !valid {
		b.log().Warn("billing: stripe webhook has a malformed checkout session")
		httpx.Detail(w, http.StatusBadRequest, "Malformed checkout session")
		return
	}
	// The signed metadata is the only tenant signal (never request headers):
	// bind it before touching the RLS-protected ledger.
	var res billing.DebitResult
	err = tenancy.InWorkspace(r.Context(), b.d.Pool, workspaceID, func(tx pgx.Tx) error {
		var e error
		res, e = billing.Credit(r.Context(), tx, workspaceID, credit, "stripe_topup", "stripe:"+sessionID)
		return e
	})
	if err != nil {
		b.internalError(w, "credit")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, webhookOK{Status: "ok", CreditedUSD: pyFloat(res.ChargedUSD), BalanceUSD: pyFloat(res.BalanceUSD), IdempotentReplay: res.IdempotentReplay})
}

// checkoutFromEvent extracts (workspace_id, session id, credit_usd) from a
// checkout.session object; valid is false for anything the Python route
// rejects as "Malformed checkout session" (and for a non-finite credit).
func checkoutFromEvent(obj map[string]any) (workspaceID, sessionID string, credit float64, valid bool) {
	meta, _ := obj["metadata"].(map[string]any)
	workspaceID, _ = meta["workspace_id"].(string)
	sessionID, _ = obj["id"].(string)
	switch x := meta["credit_usd"].(type) {
	case string:
		credit, _ = pyFloatFromString(x)
	case json.Number:
		credit, _ = strconv.ParseFloat(x.String(), 64)
	}
	if workspaceID == "" || sessionID == "" || !(credit > 0) || math.IsInf(credit, 0) {
		return "", "", 0, false
	}
	return workspaceID, sessionID, credit, true
}
