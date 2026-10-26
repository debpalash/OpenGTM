// Package providers is the Go port of the HTTP enrichment providers. A Provider
// is one vendor adapter (a port of one Python providers/*.py module): it turns a
// lead into an Output over the caller's egress-guarded HTTP client.
//
// Contract shared with providerrun (Task 18), the firmographic providers
// (Task 14) and emailverify (Task 15):
//
//   - Enrich returns a non-nil error only for infrastructure failures: the
//     context was cancelled or the key lookup failed. Every vendor failure
//     (HTTP status, malformed JSON, timeout, missing key) is a normal Output
//     with Error set, with the exact string Python puts in EnrichmentResult.error.
//   - Output.Extra is the Python EnrichmentResult.fields dict in insertion
//     order; Output.Value is fields[Input.Field] (nil if absent). Success is
//     Output.Error == "". CostUSD is always 0 here (cost is vendorcatalog's job).
//   - The credential is resolved lazily through the key callback, so a provider
//     that needs none, or fails before needing it, never triggers a fetch. An
//     empty key means "not configured" and yields the provider's Python message.
//     Providers with several env vars implement Keyed; the callback then returns
//     JoinKeys of the values in KeyEnv order.
//   - The *http.Client must be the egress client (internal/egress); providers
//     never follow redirects, cap bodies at MaxBody and never put a key in an
//     error string or log.
package providers

import (
	"context"
	"net/http"
	"strings"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// Obj is the insertion-ordered JSON object used for provider fields.
type Obj = wbcrud.Obj

// Input is one provider call. Lead is the lead dict (company, website,
// contact_person, linkedin_url, email, phone, ...). Settings carries the
// non-secret global configuration a provider reads (for example REACHER_URL).
type Input struct {
	Lead      map[string]any
	Field     string
	Operation string
	Settings  map[string]any
}

// Output is one provider result.
type Output struct {
	Value      any
	Confidence float64
	Extra      *Obj
	CostUSD    float64
	Error      string
}

// Provider is one vendor adapter.
type Provider interface {
	Name() string
	Enrich(ctx context.Context, c *http.Client, key func() (string, error), in Input) (Output, error)
}

// Keyed is implemented by providers that need credentials: KeyEnv lists the
// env var names (credentials.Client.Get(ctx, workspaceID, envVar)) in the order
// their values are passed through the key callback.
type Keyed interface {
	KeyEnv() []string
}

const keySep = "\x1f"

// JoinKeys packs several credential values into the single key callback result.
func JoinKeys(vals ...string) string { return strings.Join(vals, keySep) }

// SplitKeys is the inverse of JoinKeys.
func SplitKeys(s string) []string { return strings.Split(s, keySep) }
