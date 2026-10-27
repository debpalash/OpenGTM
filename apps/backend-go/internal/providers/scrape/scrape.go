// Package scrape ports the Python-only enrichment providers (plan 15
// WP-PROVIDERS, inventory B1-B19): the providers that searched the web,
// scraped pages or ran a scraping library inside the Python worker. Page and
// search work goes through internal/leadweb and the browser service; parsing,
// ranking and result shaping run here.
//
// Every provider is a providers.Provider registered under its Python name.
// They are gated (providers.LocalGated): providerrun and rpclocal run one in
// process only when GO_LOCAL_PROVIDERS lists it; otherwise the call keeps going to the
// Python worker. A direct caller (an LLM route action) may call Enrich itself.
//
// The I/O seams (Deps) are process-wide and set once at start-up by
// Configure; a provider whose seam is missing answers a normal error Output.
// User and lead supplied URLs are only fetched through the leadweb fetcher
// (egress URL guard first, the browser service re-checks every hop) or the
// caller's strict egress client, never the operator client.
package scrape

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/browserclient"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadweb"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// Hit is one search result.
type Hit = leadweb.Hit

// Search is the web search seam. Text is the legacy resilient search
// (proxy_client.get_ddgs); Web is the enrichment web_search DDGS stand-in
// (SearXNG first), which reports a failure only when raise is set.
type Search interface {
	Text(ctx context.Context, query string, max int) []Hit
	Web(ctx context.Context, query string, max int, raise bool) ([]Hit, error)
}

// Fetcher is the legacy StealthClient (leadweb.Fetcher).
type Fetcher interface {
	Fetch(ctx context.Context, rawURL string, o leadweb.FetchOptions) leadweb.FetchResult
}

// MailCheck is the slice of an email_verify result the providers read.
type MailCheck struct {
	Email       string
	Confidence  string
	Deliverable *bool
	CatchAll    bool
}

// Mail is the SMTP seam (emailverify): MX lookup, pick_best_email and
// verify_email.
type Mail interface {
	HasMX(ctx context.Context, domain string) bool
	PickBest(ctx context.Context, candidates []string) (MailCheck, error)
	Verify(ctx context.Context, email string) (MailCheck, error)
}

// Browser is the part of the browser-service client the providers call
// directly (holehe, staffspy, the person_intel_deep page render).
type Browser interface {
	Render(ctx context.Context, req browserclient.RenderRequest) (browserclient.RenderResult, error)
	Holehe(ctx context.Context, req browserclient.HoleheRequest) (browserclient.HoleheResult, error)
	StaffSpy(ctx context.Context, req browserclient.StaffSpyRequest) (browserclient.StaffSpyResult, error)
}

// Deps are the process-wide seams.
type Deps struct {
	Search  Search
	Fetch   Fetcher
	Mail    Mail
	Browser Browser
	// StaffSpyEnabled is STAFFSPY_ENABLED (default off: LinkedIn ToS).
	StaffSpyEnabled bool
	// LinkedInCookies resolves the calling workspace's own LinkedIn session for
	// staffspy (li_at and JSESSIONID) from its workspace secrets — never an
	// operator / env value (credential rule). The session goes to the browser
	// service for that one request and is never stored or returned. nil, no
	// workspace or no li_at leaves staffspy off.
	LinkedInCookies func(ctx context.Context, ws string) ([]browserclient.Cookie, error)
	// Sleep is the legacy pacing sleep; nil sleeps for real (ctx-aware).
	Sleep func(ctx context.Context, d time.Duration) error
}

var cur atomic.Pointer[Deps]

// Configure installs the process-wide seams (call once at start-up; tests may
// call it again).
func Configure(d Deps) { cur.Store(&d) }

func current() *Deps {
	if d := cur.Load(); d != nil {
		return d
	}
	return &Deps{}
}

// gated marks the providers of this package (providers.LocalGated).
type gated struct{}

func (gated) LocalGated() {}

func sleep(ctx context.Context, d time.Duration) error {
	if f := current().Sleep; f != nil {
		return f(ctx, d)
	}
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// errNoWeb is the Output error of a provider whose seam is not wired.
const errNoWeb = "browser service not configured"

var errNoSeam = errors.New(errNoWeb)

// ---- results ----------------------------------------------------------------

// result builds the Output of one EnrichmentResult: Extra is fields in
// insertion order, Value is fields[in.Field], Error is "" on success.
func result(in providers.Input, fields *providers.Obj, conf float64, errMsg string) providers.Output {
	if fields == nil {
		fields = providers.NewObj()
	}
	out := providers.Output{Extra: fields, Confidence: conf, Error: errMsg}
	if v, ok := fields.Get(in.Field); ok {
		out.Value = v
	}
	return out
}

func fail(in providers.Input, msg string) providers.Output { return result(in, nil, 0, msg) }

// ok is a success with the given fields (alternating key, value).
func ok(in providers.Input, conf float64, kv ...any) providers.Output {
	f := providers.NewObj()
	for i := 0; i+1 < len(kv); i += 2 {
		f.Set(kv[i].(string), kv[i+1])
	}
	return result(in, f, conf, "")
}

// ResultName is the EnrichmentResult.provider a provider reports (the Python
// result's provider field). Most report their own name; a few report "" on a
// failure, exactly as the Python class did.
type ResultName interface {
	ResultProvider(out providers.Output) string
}
