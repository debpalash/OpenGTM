package llm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/settings"
)

// Errors returned by Complete and ExtractJSON. Python returned "" / {} for all
// of them and left the reason in TokenUsage.Errors; both are kept.
var (
	// ErrNoProvider: no provider has an API key ("No LLM providers configured").
	ErrNoProvider = errors.New("No LLM providers configured")
	// ErrAllFailed: every tried provider returned an error (the last one is wrapped).
	ErrAllFailed = errors.New("all LLM providers failed")
	// ErrEmptyResponse: the providers answered without text and without error.
	ErrEmptyResponse = errors.New("LLM returned an empty response")
	// ErrInvalidJSON: ExtractJSON got text twice and neither parsed as an object.
	ErrInvalidJSON = errors.New("LLM response was not valid JSON")
)

const providerCacheTTL = 60 * time.Second

// Options configures a Client. Settings and HTTP are required.
type Options struct {
	Settings    Settings
	HTTP        *http.Client // egress-guarded client; per-request timeouts are applied by the Client
	Usage       UsageRecorder
	WorkspaceID string // usage rows are written for this workspace (empty: not recorded)
	Config      config.LLMSettings
	Registry    *settings.Registry // chat provider chain (Stream); nil disables ChatChain
	Log         *slog.Logger

	// StreamTimeout is the connect/idle timeout of Stream (httpx timeout=90.0). Default 90s.
	StreamTimeout time.Duration
	// Sleep and Now are test seams.
	Sleep func(ctx context.Context, d time.Duration) error
	Now   func() time.Time
	// Jitter returns a value in [0,1); default math/rand/v2. Only the Anthropic SDK-style retry uses it.
	Jitter func() float64
}

// Client is the provider-agnostic LLM client for one workspace. It is safe for
// concurrent use.
type Client struct {
	o     Options
	usage usageTracker

	mu        sync.Mutex
	cache     []Provider
	cacheAt   time.Time
	cacheInit bool
}

// New returns a Client; it panics on a nil Settings or HTTP (programmer error).
func New(o Options) *Client {
	if o.Settings == nil || o.HTTP == nil {
		panic("llm: Options.Settings and Options.HTTP are required")
	}
	if o.Config.HTTPTimeout <= 0 {
		o.Config.HTTPTimeout = 60 * time.Second
	}
	if o.StreamTimeout <= 0 {
		o.StreamTimeout = 90 * time.Second
	}
	if o.Sleep == nil {
		o.Sleep = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	return &Client{o: o}
}

// Usage returns the token usage accumulated since the last ResetUsage.
func (c *Client) Usage() TokenUsage { return c.usage.snapshot() }

// ResetUsage starts a new usage window (a new job).
func (c *Client) ResetUsage() { c.usage.reset() }

func (c *Client) get(ctx context.Context, key, def string) string {
	if key == "" {
		return def
	}
	v, err := c.o.Settings.Get(ctx, key, def)
	if err != nil {
		// A stored value that cannot be decrypted is never replaced by the default
		// silently for keys; log the key name only.
		c.o.Log.Warn("llm: setting unreadable", "key", key)
		return def
	}
	return v
}

func (c *Client) providerConfig(ctx context.Context, id string) (Provider, bool) {
	spec, ok := providerSpecs[id]
	if !ok {
		return Provider{}, false
	}
	key := c.get(ctx, spec.EnvKey, "")
	if key == "" {
		return Provider{}, false
	}
	return Provider{
		ID:         id,
		APIKey:     key,
		BaseURL:    c.get(ctx, spec.EnvURL, spec.DefaultURL),
		Model:      c.get(ctx, spec.EnvModel, spec.DefaultModel),
		TokenParam: spec.TokenParam,
		Native:     spec.Native,
	}, true
}

// DefaultProvider picks the default provider id: an explicit
// LLM_DEFAULT_PROVIDER, else anthropic when it has a key and YUPCHA_CLOUD is
// truthy, else cerebras (the historical free default).
func (c *Client) DefaultProvider(ctx context.Context) string {
	if v := c.get(ctx, "LLM_DEFAULT_PROVIDER", ""); v != "" {
		return v
	}
	if isTruthy(c.get(ctx, "YUPCHA_CLOUD", "")) {
		if _, ok := c.providerConfig(ctx, AnthropicID); ok {
			return AnthropicID
		}
	}
	return "cerebras"
}

// Providers returns the configured providers in call order: the default first,
// then ProviderOrder. The result is cached for 60 seconds like the Python client.
func (c *Client) Providers(ctx context.Context) []Provider {
	now := c.o.Now()
	c.mu.Lock()
	if c.cacheInit && len(c.cache) > 0 && now.Sub(c.cacheAt) < providerCacheTTL {
		out := append([]Provider(nil), c.cache...)
		c.mu.Unlock()
		return out
	}
	c.mu.Unlock()

	var out []Provider
	seen := map[string]bool{}
	def := c.DefaultProvider(ctx)
	if p, ok := c.providerConfig(ctx, def); ok {
		out = append(out, p)
		seen[def] = true
	}
	for _, id := range ProviderOrder {
		if seen[id] {
			continue
		}
		if p, ok := c.providerConfig(ctx, id); ok {
			out = append(out, p)
			seen[id] = true
		}
	}
	c.mu.Lock()
	c.cache, c.cacheAt, c.cacheInit = append([]Provider(nil), out...), now, true
	c.mu.Unlock()
	return out
}

// AnthropicProvider returns the resolved Anthropic provider iff it leads the
// chain (the bulk/batch path only makes sense when Anthropic serves the run).
func (c *Client) AnthropicProvider(ctx context.Context) (Provider, bool) {
	ps := c.Providers(ctx)
	if len(ps) > 0 && ps[0].Native {
		return ps[0], true
	}
	return Provider{}, false
}

// CompleteRequest is one completion. Zero MaxTokens means 1024; nil Temperature 0.2.
type CompleteRequest struct {
	Prompt      string
	System      string
	MaxTokens   int
	Temperature *float64
}

// Complete sends a chat completion with failover: up to the first three
// configured providers, each retried with backoff (0.5s, 1.37s, 2.74s) while
// the error looks like a rate limit. It returns the trimmed text.
//
// Python returned "" for every failure. The Go error says why: ErrNoProvider,
// ErrAllFailed (wrapping the last provider error) or ErrEmptyResponse. The
// same texts are appended to Usage().Errors.
func (c *Client) Complete(ctx context.Context, req CompleteRequest) (string, error) {
	if req.MaxTokens <= 0 {
		req.MaxTokens = 1024
	}
	temp := 0.2
	if req.Temperature != nil {
		temp = *req.Temperature
	}
	providers := c.Providers(ctx)
	if len(providers) == 0 {
		c.usage.addError(ErrNoProvider.Error())
		return "", ErrNoProvider
	}
	if len(providers) > 3 {
		providers = providers[:3]
	}
	var lastErr error
	for _, prov := range providers {
		for attempt := 0; attempt < 4; attempt++ {
			text, err := c.callProvider(ctx, prov, req, temp)
			if err == nil {
				if text != "" {
					return text, nil
				}
				break // empty without error: next provider
			}
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			lastErr = fmt.Errorf("%s: %w", prov.ID, err)
			c.usage.addError(prov.ID + ": " + truncRunes(err.Error(), 100))
			if isRateLimited(err) && attempt < 3 {
				// 0.5*2^n seconds plus a deterministic 0.37s*n stagger (no RNG), as in Python.
				d := 0.5*math.Pow(2, float64(attempt)) + float64(attempt)*0.37
				if serr := c.o.Sleep(ctx, time.Duration(d*float64(time.Second))); serr != nil {
					return "", serr
				}
				continue
			}
			break
		}
	}
	if lastErr != nil {
		return "", fmt.Errorf("%w: %w", ErrAllFailed, lastErr)
	}
	return "", ErrEmptyResponse
}

// isRateLimited decides whether a provider error is worth retrying on the same
// provider. Python matched the bare substring "rate", which also hit words such
// as "generate" or "moderate"; the Go check needs the word as a phrase.
func isRateLimited(err error) bool {
	m := strings.ToLower(err.Error())
	for _, s := range []string{"429", "rate limit", "rate_limit", "rate-limit", "ratelimit", "rate limited", "quota", "too many"} {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}

const jsonSystemSuffix = "IMPORTANT: Respond with valid JSON only. No markdown, no code fences, no explanation."

// ExtractJSON asks for a JSON object and parses the reply with ParseObject. A
// reply that does not parse is retried once with a stronger instruction.
// MaxTokens 0 means 1024. Python returned {} on failure; Go returns the error
// (ErrInvalidJSON, or the Complete error of the last attempt).
func (c *Client) ExtractJSON(ctx context.Context, prompt, system string, maxTokens int) (map[string]any, error) {
	jsonSystem := jsonSystemSuffix
	if system != "" {
		jsonSystem = system + "\n\n" + jsonSystemSuffix
	}
	lastErr := ErrEmptyResponse
	for attempt := 0; attempt < 2; attempt++ {
		text, err := c.Complete(ctx, CompleteRequest{Prompt: prompt, System: jsonSystem, MaxTokens: maxTokens})
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
			continue
		}
		if obj, ok := ParseObject(text); ok {
			return obj, nil
		}
		lastErr = ErrInvalidJSON
		if attempt == 0 {
			prompt += "\n\nYour previous response was not valid JSON. Return ONLY a JSON object, nothing else."
		}
	}
	return nil, lastErr
}

func (c *Client) record(ctx context.Context, r UsageRecord) {
	if c.o.Usage == nil || c.o.WorkspaceID == "" {
		return
	}
	// Detached from the request's cancellation so a call that finished is still counted.
	if err := c.o.Usage.Record(context.WithoutCancel(ctx), c.o.WorkspaceID, r); err != nil {
		c.o.Log.Debug("llm: usage tracking failed", "err", err)
	}
}

func (c *Client) callProvider(ctx context.Context, p Provider, req CompleteRequest, temp float64) (string, error) {
	if p.Native {
		return c.callAnthropic(ctx, p, req)
	}
	return c.callOpenAI(ctx, p, req, temp)
}
