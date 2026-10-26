package billing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stripe/stripe-go/v87/webhook"
)

const testSecret = "whsec_unit_test_only"

func eventJSON(apiVersion string) []byte {
	return []byte(`{"id":"evt_1","object":"event","api_version":"` + apiVersion + `","created":1,` +
		`"type":"checkout.session.completed","data":{"object":{"id":"cs_test_1","object":"checkout.session",` +
		`"payment_status":"paid","metadata":{"workspace_id":"ws-1","credit_usd":"25.0"}}}}`)
}

func sign(payload []byte, secret string, ts time.Time) string {
	return webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: payload, Secret: secret, Timestamp: ts}).Header
}

func TestWebhookAcceptsSDKSignedPayload(t *testing.T) {
	p := eventJSON("2020-08-27")
	ev, err := VerifyWebhook(p, sign(p, testSecret, time.Now()), testSecret)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if ev.Type != "checkout.session.completed" || ev.ID != "evt_1" {
		t.Fatalf("event: %+v", ev)
	}
}

func TestWebhookAcceptsMismatchedAPIVersion(t *testing.T) {
	for _, v := range []string{"2015-01-01", "2099-12-31", "2024-06-20"} {
		p := eventJSON(v)
		if _, err := VerifyWebhook(p, sign(p, testSecret, time.Now()), testSecret); err != nil {
			t.Fatalf("api_version %s rejected: %v", v, err)
		}
	}
}

func TestSignatureRejectsTamperedPayload(t *testing.T) {
	p := eventJSON("2020-08-27")
	h := sign(p, testSecret, time.Now())
	tampered := []byte(strings.Replace(string(p), `"25.0"`, `"2500.0"`, 1))
	if _, err := VerifyWebhook(tampered, h, testSecret); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("want ErrBadSignature, got %v", err)
	}
	if _, err := VerifyWebhook(p, h, "whsec_other"); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("wrong secret: want ErrBadSignature, got %v", err)
	}
}

func TestSignatureRejectsOldTimestamp(t *testing.T) {
	p := eventJSON("2020-08-27")
	h := sign(p, testSecret, time.Now().Add(-10*time.Minute))
	if _, err := VerifyWebhook(p, h, testSecret); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("want ErrBadSignature, got %v", err)
	}
}

func TestSignatureAcceptsSecondV1(t *testing.T) {
	p := eventJSON("2020-08-27")
	good := sign(p, testSecret, time.Now())
	// Insert a bogus v1 ahead of the valid one (secret rotation: Stripe sends several).
	h := strings.Replace(good, ",v1=", ",v1="+strings.Repeat("0", 64)+",v1=", 1)
	if h == good {
		t.Fatal("test setup: no v1 in header")
	}
	if _, err := VerifyWebhook(p, h, testSecret); err != nil {
		t.Fatalf("second v1 rejected: %v", err)
	}
}

func TestSignatureRejectsMissingV1(t *testing.T) {
	p := eventJSON("2020-08-27")
	good := sign(p, testSecret, time.Now())
	ts := strings.SplitN(good, ",", 2)[0]
	for _, h := range []string{"", ts, ts + ",v0=abcd", "garbage"} {
		if _, err := VerifyWebhook(p, h, testSecret); !errors.Is(err, ErrBadSignature) {
			t.Errorf("header %q: want ErrBadSignature, got %v", h, err)
		}
	}
}

func TestSignatureEmptySecretRejected(t *testing.T) {
	p := eventJSON("2020-08-27")
	// A header forged with the empty secret must not verify.
	if _, err := VerifyWebhook(p, sign(p, "", time.Now()), ""); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("empty secret: want ErrBadSignature, got %v", err)
	}
}

func TestSignatureErrorNoEcho(t *testing.T) {
	p := eventJSON("2020-08-27")
	bad := "t=1700000000,v1=deadbeefcafe"
	_, err := VerifyWebhook(p, bad, testSecret)
	if !errors.Is(err, ErrBadSignature) {
		t.Fatalf("want ErrBadSignature, got %v", err)
	}
	for _, leak := range []string{"deadbeefcafe", "1700000000", "ws-1", testSecret, "cs_test_1"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error echoes %q: %v", leak, err)
		}
	}
	// A parse failure of a correctly signed non-JSON body is also ErrBadSignature.
	np := []byte("not json")
	if _, err := VerifyWebhook(np, sign(np, testSecret, time.Now()), testSecret); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("unparseable body: want ErrBadSignature, got %v", err)
	}
}

func TestWebhookUsesRawBody(t *testing.T) {
	// Whitespace and key order that json.Marshal would not reproduce.
	p := []byte("{ \"id\" : \"evt_1\",\n\t\"object\":\"event\", \"type\":\"checkout.session.completed\",\n" +
		"  \"data\":{\"object\":{\"id\":\"cs_1\"}} }\n")
	if _, err := VerifyWebhook(p, sign(p, testSecret, time.Now()), testSecret); err != nil {
		t.Fatalf("raw body rejected: %v", err)
	}
	remarshalled, _ := json.Marshal(json.RawMessage(p))
	if string(remarshalled) == string(p) {
		t.Fatal("test setup: body is already canonical")
	}
}

type captured struct {
	mu     sync.Mutex
	form   url.Values
	header http.Header
	path   string
	calls  int
}

func fakeStripe(t *testing.T, status int, respBody string) (*httptest.Server, *captured) {
	t.Helper()
	c := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		c.mu.Lock()
		c.form, c.header, c.path = r.PostForm, r.Header.Clone(), r.URL.Path
		c.calls++
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(respBody))
	}))
	t.Cleanup(srv.Close)
	return srv, c
}

const okSession = `{"id":"cs_test_123","object":"checkout.session","url":"https://checkout.stripe.com/c/pay/cs_test_123"}`

func TestCheckoutFormEncoding(t *testing.T) {
	srv, c := fakeStripe(t, 200, okSession)
	s := NewStripe("sk_test_unit", Options{APIBase: srv.URL})
	id, link, err := s.CreateCheckoutSession(context.Background(), 25, "ws-1", "https://ok.example/s", "https://ok.example/c")
	if err != nil {
		t.Fatal(err)
	}
	if id != "cs_test_123" || link != "https://checkout.stripe.com/c/pay/cs_test_123" {
		t.Fatalf("id=%q url=%q", id, link)
	}
	want := map[string]string{
		"mode":                                          "payment",
		"line_items[0][price_data][currency]":           "usd",
		"line_items[0][price_data][unit_amount]":        "2500",
		"line_items[0][price_data][product_data][name]": "OpenGTM enrichment credits",
		"line_items[0][quantity]":                       "1",
		"metadata[workspace_id]":                        "ws-1",
		"metadata[credit_usd]":                          "25.0",
		"success_url":                                   "https://ok.example/s",
		"cancel_url":                                    "https://ok.example/c",
	}
	got := map[string]string{}
	for k, v := range c.form {
		if len(v) != 1 {
			t.Errorf("field %s sent %d times", k, len(v))
		}
		got[k] = v[0]
	}
	if len(got) != len(want) {
		var keys []string
		for k := range got {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		t.Errorf("fields: got %v", keys)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: got %q, want %q", k, got[k], v)
		}
	}
	if c.path != "/v1/checkout/sessions" {
		t.Errorf("path %q", c.path)
	}
	if k := c.header.Get("Idempotency-Key"); len(k) < 16 {
		t.Errorf("Idempotency-Key %q", k)
	}
	if a := c.header.Get("Authorization"); a != "Bearer sk_test_unit" {
		t.Errorf("Authorization header not the per-instance key")
	}
}

func TestCheckoutIdempotencyKeyFreshPerCall(t *testing.T) {
	srv, c := fakeStripe(t, 200, okSession)
	s := NewStripe("sk_test_unit", Options{APIBase: srv.URL})
	var keys []string
	for i := 0; i < 2; i++ {
		if _, _, err := s.CreateCheckoutSession(context.Background(), 5, "ws-1", "https://a", "https://b"); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, c.header.Get("Idempotency-Key"))
	}
	if keys[0] == "" || keys[0] == keys[1] {
		t.Fatalf("idempotency keys: %v", keys)
	}
}

func TestCheckoutRoundsHalfEven(t *testing.T) {
	for amount, cents := range map[float64]string{0.125: "12", 0.135: "14", 0.375: "38", 1.005: "100", 25: "2500", 0.01: "1"} {
		srv, c := fakeStripe(t, 200, okSession)
		s := NewStripe("sk_test_unit", Options{APIBase: srv.URL})
		if _, _, err := s.CreateCheckoutSession(context.Background(), amount, "ws-1", "https://a", "https://b"); err != nil {
			t.Fatal(err)
		}
		if got := c.form.Get("line_items[0][price_data][unit_amount]"); got != cents {
			t.Errorf("amount %v: unit_amount %s, want %s", amount, got, cents)
		}
	}
}

func TestCheckoutMetadataCreditUSDIsPythonRepr(t *testing.T) {
	for amount, want := range map[float64]string{25: "25.0", 10.5: "10.5", 0.1: "0.1", 1e16: "1e+16"} {
		srv, c := fakeStripe(t, 200, okSession)
		s := NewStripe("sk_test_unit", Options{APIBase: srv.URL})
		_, _, err := s.CreateCheckoutSession(context.Background(), amount, "ws-1", "https://a", "https://b")
		if amount == 1e16 {
			if err == nil {
				t.Error("absurd amount must be rejected before calling Stripe")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if got := c.form.Get("metadata[credit_usd]"); got != want {
			t.Errorf("amount %v: credit_usd %q, want %q", amount, got, want)
		}
	}
}

func TestCheckoutSDKErrorIsOpaque(t *testing.T) {
	srv, _ := fakeStripe(t, 400, `{"error":{"type":"invalid_request_error","message":"secret-detail sk_live_leak"}}`)
	s := NewStripe("sk_test_unit", Options{APIBase: srv.URL})
	_, _, err := s.CreateCheckoutSession(context.Background(), 5, "ws-1", "https://a", "https://b")
	if !errors.Is(err, ErrCheckoutFailed) {
		t.Fatalf("want ErrCheckoutFailed, got %v", err)
	}
	if strings.Contains(err.Error(), "secret-detail") || strings.Contains(err.Error(), "sk_") {
		t.Fatalf("error leaks Stripe detail: %v", err)
	}
}

func TestCheckoutTimeoutIs15s(t *testing.T) {
	s := NewStripe("sk_test_unit", Options{})
	if s.HTTPTimeout() != 15*time.Second {
		t.Fatalf("timeout %v", s.HTTPTimeout())
	}
}

func TestStripeModuleImportedOnlyByBilling(t *testing.T) {
	out, err := exec.Command("go", "list", "-tags", "integration", "-f",
		`{{.ImportPath}}|{{join .Imports " "}} {{join .TestImports " "}} {{join .XTestImports " "}}`, "../../...").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	found := false
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		pkg, imports, _ := strings.Cut(line, "|")
		if !strings.Contains(imports, "github.com/stripe/stripe-go/") {
			continue
		}
		found = true
		if !strings.HasSuffix(pkg, "/internal/billing") {
			t.Errorf("%s imports stripe-go; only internal/billing may", pkg)
		}
	}
	if !found {
		t.Error("internal/billing does not import stripe-go")
	}
}
