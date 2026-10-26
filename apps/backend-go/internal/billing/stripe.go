package billing

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/stripe/stripe-go/v87"
	"github.com/stripe/stripe-go/v87/webhook"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
)

// ErrBadSignature is every webhook verification or parse failure. It never
// carries the header, the payload or the SDK's message.
var ErrBadSignature = errors.New("billing: invalid webhook signature")

// ErrCheckoutFailed is any failure creating a Checkout Session. The Stripe
// error message is dropped: it may echo request data and must not reach clients.
var ErrCheckoutFailed = errors.New("billing: stripe checkout session failed")

const (
	httpTimeout = 15 * time.Second
	// Stripe's largest amount for USD is 99,999,999 cents; larger values can
	// only fail upstream and must not overflow the int64 conversion.
	maxUnitAmount = 99_999_999
)

// VerifyWebhook checks the Stripe-Signature header over the raw payload bytes
// with the SDK's default 300 s tolerance (several v1 signatures are accepted).
// IgnoreAPIVersionMismatch is deliberate: Python's construct_event never
// rejects on API version, and a rejection makes Stripe retry the event forever.
// An empty secret never verifies (an HMAC keyed with "" is forgeable).
func VerifyWebhook(payload []byte, header, secret string) (stripe.Event, error) {
	if secret == "" {
		return stripe.Event{}, ErrBadSignature
	}
	ev, err := webhook.ConstructEventWithOptions(payload, header, secret, webhook.ConstructEventOptions{
		IgnoreAPIVersionMismatch: true,
	})
	if err != nil {
		return stripe.Event{}, ErrBadSignature
	}
	return ev, nil
}

// Options tune a Stripe client. APIBase must only be set by tests (the config
// layer drops STRIPE_API_BASE outside APP_ENV=test).
type Options struct {
	APIBase    string
	HTTPClient *http.Client
}

// Stripe is a per-instance Stripe API client; the package-global stripe.Key is
// never touched, so instances (and tests) cannot race.
type Stripe struct {
	client  *stripe.Client
	timeout time.Duration
}

// NewStripe builds a client for secretKey with a 15 s HTTP timeout and no
// automatic retries (each CreateCheckoutSession carries a fresh idempotency key).
func NewStripe(secretKey string, o Options) *Stripe {
	hc := o.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: httpTimeout}
	}
	cfg := &stripe.BackendConfig{
		HTTPClient:        hc,
		MaxNetworkRetries: stripe.Int64(0),
		EnableTelemetry:   stripe.Bool(false),
		// The SDK's default logger prints Stripe error bodies, which can echo
		// request data; the API layer logs a fixed message instead.
		LeveledLogger: &stripe.LeveledLogger{Level: stripe.LevelNull},
	}
	if o.APIBase != "" {
		cfg.URL = stripe.String(o.APIBase)
	}
	client := stripe.NewClient(secretKey, stripe.WithBackends(stripe.NewBackendsWithConfig(cfg)))
	return &Stripe{client: client, timeout: hc.Timeout}
}

// HTTPTimeout reports the per-request timeout of the underlying HTTP client.
func (s *Stripe) HTTPTimeout() time.Duration { return s.timeout }

// CreateCheckoutSession starts a hosted top-up for amountUSD credit.
// Mirrors RealStripeClient.create_checkout_session: unit_amount is
// round-half-even(amount*100) like Python's int(round(...)), and the metadata
// credit_usd is str(float).
func (s *Stripe) CreateCheckoutSession(ctx context.Context, amountUSD float64, workspaceID, successURL, cancelURL string) (id, url string, err error) {
	cents := math.RoundToEven(amountUSD * 100)
	if math.IsNaN(cents) || cents < 0 || cents > maxUnitAmount {
		return "", "", fmt.Errorf("%w: amount out of range", ErrCheckoutFailed)
	}
	credit, err := canonjson.FloatRepr(amountUSD)
	if err != nil {
		return "", "", fmt.Errorf("%w: amount not finite", ErrCheckoutFailed)
	}
	key, err := idempotencyKey()
	if err != nil {
		return "", "", fmt.Errorf("%w: idempotency key", ErrCheckoutFailed)
	}
	params := &stripe.CheckoutSessionCreateParams{
		Mode: stripe.String(string(stripe.CheckoutSessionModePayment)),
		LineItems: []*stripe.CheckoutSessionCreateLineItemParams{{
			PriceData: &stripe.CheckoutSessionCreateLineItemPriceDataParams{
				Currency:    stripe.String("usd"),
				UnitAmount:  stripe.Int64(int64(cents)),
				ProductData: &stripe.CheckoutSessionCreateLineItemPriceDataProductDataParams{Name: stripe.String("OpenGTM enrichment credits")},
			},
			Quantity: stripe.Int64(1),
		}},
		SuccessURL: stripe.String(successURL),
		CancelURL:  stripe.String(cancelURL),
	}
	params.AddMetadata("workspace_id", workspaceID)
	params.AddMetadata("credit_usd", credit)
	params.SetIdempotencyKey(key)
	session, err := s.client.V1CheckoutSessions.Create(ctx, params)
	if err != nil {
		var se *stripe.Error
		if errors.As(err, &se) {
			return "", "", fmt.Errorf("%w (http %d)", ErrCheckoutFailed, se.HTTPStatusCode)
		}
		return "", "", ErrCheckoutFailed
	}
	return session.ID, session.URL, nil
}

// idempotencyKey is a random v4 UUID.
func idempotencyKey() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

// SignedHeader builds a valid Stripe-Signature header for payload using the
// SDK's own test signer. It exists so tests outside this package can sign
// events without importing stripe-go (that import is confined to this package).
func SignedHeader(payload []byte, secret string, ts time.Time) string {
	return webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: payload, Secret: secret, Timestamp: ts}).Header
}
