package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// The Python client used the official anthropic SDK. These constants are its
// retry policy (max_retries=2, backoff 0.5s doubling to 8s with 25% jitter,
// Retry-After honoured up to 60s); the wire format is recorded in vectors.json.
const (
	anthropicMaxRetries   = 2
	anthropicInitialDelay = 500 * time.Millisecond
	anthropicMaxDelay     = 8 * time.Second
	anthropicMaxRetryWait = 60 * time.Second
)

// AnthropicError is a non-2xx Messages API answer, worded like the SDK's
// APIStatusError so the rate-limit check and stored error texts keep working.
type AnthropicError struct {
	Status int
	Body   string
}

func (e *AnthropicError) Error() string { return fmt.Sprintf("Error code: %d - %s", e.Status, e.Body) }

// AnthropicUsage is the usage block of a Messages response.
type AnthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// AnthropicResponse is a raw Messages API turn. Content keeps every block
// (thinking, tool_use, ...) byte for byte so a caller can append it to the
// history unchanged, as the API requires for thinking blocks.
type AnthropicResponse struct {
	ID         string            `json:"id"`
	Model      string            `json:"model"`
	StopReason string            `json:"stop_reason"`
	Content    []json.RawMessage `json:"content"`
	Usage      AnthropicUsage    `json:"usage"`
}

// Text concatenates the text blocks (thinking blocks are skipped) and trims.
func (r *AnthropicResponse) Text() string {
	var b strings.Builder
	for _, raw := range r.Content {
		var blk struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &blk) == nil && blk.Type == "text" {
			b.WriteString(blk.Text)
		}
	}
	return strings.TrimSpace(b.String())
}

type cacheControl struct {
	Type string `json:"type"`
}

type systemBlock struct {
	Type         string        `json:"type"`
	Text         string        `json:"text"`
	CacheControl *cacheControl `json:"cache_control,omitempty"`
}

// systemBlocks builds the system value with one cache breakpoint: the AI-column
// system prompt is byte-identical across rows, so the first call writes the
// prefix and the rest read it. Empty text means no system prompt.
func (c *Client) systemBlocks(text string) []systemBlock {
	if text == "" {
		return nil
	}
	b := systemBlock{Type: "text", Text: text}
	if c.o.Config.PromptCache {
		b.CacheControl = &cacheControl{Type: "ephemeral"}
	}
	return []systemBlock{b}
}

func anthropicBase(p Provider) string {
	base := strings.TrimSpace(p.BaseURL)
	switch base {
	case "", anthropicDefaultURL, anthropicDefaultURL + "/v1":
		return anthropicDefaultURL
	}
	return strings.TrimRight(base, "/")
}

// anthropicCall performs one Messages-API request with the SDK retry policy and
// returns the 2xx body. stream=true hands back the open body (batch results)
// with the timeout applying to the response headers only.
func (c *Client) anthropicCall(ctx context.Context, p Provider, method, path string, body any, stream bool) (io.ReadCloser, error) {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}
	url := anthropicBase(p) + path
	for attempt := 0; ; attempt++ {
		rc, retryAfter, err := c.anthropicAttempt(ctx, p, method, url, payload, stream)
		if err == nil {
			return rc, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var ae *AnthropicError
		retryable := true // connection errors and timeouts
		if errors.As(err, &ae) {
			retryable = ae.retryable(retryAfter.shouldRetry)
		}
		if !retryable || attempt >= anthropicMaxRetries {
			return nil, err
		}
		if serr := c.o.Sleep(ctx, c.retryDelay(attempt, retryAfter)); serr != nil {
			return nil, serr
		}
	}
}

type retryHints struct {
	afterMS, afterSec string
	date              string
	shouldRetry       string
}

func (e *AnthropicError) retryable(hint string) bool {
	switch hint {
	case "true":
		return true
	case "false":
		return false
	}
	return e.Status == 408 || e.Status == 409 || e.Status == 429 || e.Status >= 500
}

func (c *Client) retryDelay(attempt int, h retryHints) time.Duration {
	if h.afterMS != "" {
		if ms, err := strconv.ParseFloat(strings.TrimSpace(h.afterMS), 64); err == nil && ms > 0 {
			if d := time.Duration(ms * float64(time.Millisecond)); d <= anthropicMaxRetryWait {
				return d
			}
		}
	}
	if h.afterSec != "" {
		if s, err := strconv.ParseFloat(strings.TrimSpace(h.afterSec), 64); err == nil {
			if d := time.Duration(s * float64(time.Second)); d > 0 && d <= anthropicMaxRetryWait {
				return d
			}
		} else if t, err := http.ParseTime(h.afterSec); err == nil {
			if d := t.Sub(c.o.Now()); d > 0 && d <= anthropicMaxRetryWait {
				return d
			}
		}
	}
	d := min(time.Duration(float64(anthropicInitialDelay)*math.Pow(2, float64(attempt))), anthropicMaxDelay)
	jit := rand.Float64
	if c.o.Jitter != nil {
		jit = c.o.Jitter
	}
	return time.Duration(float64(d) * (1 - 0.25*jit()))
}

func (c *Client) anthropicAttempt(ctx context.Context, p Provider, method, url string, payload []byte, stream bool) (io.ReadCloser, retryHints, error) {
	var h retryHints
	rctx, cancel := context.WithCancel(ctx)
	var timedOut atomic.Bool
	timer := time.AfterFunc(turnTimeoutFrom(ctx, c.o.Config.HTTPTimeout), func() { timedOut.Store(true); cancel() })
	stop := func() { timer.Stop(); cancel() }

	var rdr io.Reader
	if payload != nil {
		rdr = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(rctx, method, url, rdr)
	if err != nil {
		stop()
		return nil, h, stripURL(err)
	}
	req.Header.Set("x-api-key", p.APIKey)
	req.Header.Set("anthropic-version", anthropicVersion)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.o.HTTP.Do(req)
	transportErr := func(err error) error {
		stop()
		if timedOut.Load() {
			return errors.New("Request timed out.")
		}
		return fmt.Errorf("Connection error: %w", stripURL(err))
	}
	if err != nil {
		return nil, h, transportErr(err)
	}
	if resp.StatusCode/100 != 2 {
		raw, rerr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		resp.Body.Close()
		stop()
		h = retryHints{
			afterMS: resp.Header.Get("retry-after-ms"), afterSec: resp.Header.Get("retry-after"),
			shouldRetry: strings.ToLower(resp.Header.Get("x-should-retry")),
		}
		if rerr != nil {
			return nil, h, transportErr(rerr)
		}
		return nil, h, &AnthropicError{Status: resp.StatusCode, Body: errorBody(raw)}
	}
	if stream {
		timer.Stop() // the deadline guards the headers only; the body is read by the caller
		return &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}, h, nil
	}
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	resp.Body.Close()
	if rerr != nil {
		return nil, h, transportErr(rerr)
	}
	stop()
	return io.NopCloser(bytes.NewReader(raw)), h, nil
}

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error { err := c.ReadCloser.Close(); c.cancel(); return err }

// errorBody renders an error response body compactly (the SDK printed the
// decoded JSON; the text only feeds logs and the rate-limit check).
func errorBody(raw []byte) string {
	var v any
	if json.Unmarshal(raw, &v) == nil {
		if b, err := json.Marshal(v); err == nil {
			return string(b)
		}
	}
	return strings.TrimSpace(string(raw))
}

func (c *Client) anthropicMessages(ctx context.Context, p Provider, body map[string]any) (*AnthropicResponse, error) {
	rc, err := c.anthropicCall(ctx, p, http.MethodPost, "/v1/messages", body, false)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	raw, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	var resp AnthropicResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("invalid JSON in Messages response: %w", err)
	}
	// Usage is read once from the final response (SDK retries never double count).
	c.addAnthropicUsage(ctx, p, resp.Usage)
	return &resp, nil
}

func (c *Client) addAnthropicUsage(ctx context.Context, p Provider, u AnthropicUsage) {
	c.usage.add(u.InputTokens, u.OutputTokens, p.ID, u.CacheCreationInputTokens, u.CacheReadInputTokens)
	c.record(ctx, UsageRecord{Provider: p.ID, Model: p.Model, PromptTokens: u.InputTokens, CompletionTokens: u.OutputTokens})
}

func (c *Client) callAnthropic(ctx context.Context, p Provider, req CompleteRequest) (string, error) {
	body := map[string]any{
		"model":      p.Model,
		"max_tokens": req.MaxTokens,
		"messages":   []orderedObject{{{"role", "user"}, {"content", req.Prompt}}},
		// Adaptive thinking is the recommended mode on Opus 4.8 / 4.7.
		"thinking": map[string]any{"type": "adaptive"},
	}
	if sb := c.systemBlocks(req.System); sb != nil {
		body["system"] = sb
	}
	resp, err := c.anthropicMessages(ctx, p, body)
	if err != nil {
		return "", err
	}
	return resp.Text(), nil
}

// AnthropicRequest is one native Messages turn (see Client.AnthropicTurn).
type AnthropicRequest struct {
	Messages     []any // Anthropic-native turns; content may already be blocks
	System       any   // string (cache-breakpointed) or a ready list of system blocks; nil for none
	Tools        []any
	ToolChoice   any
	OutputFormat any // becomes output_config.format; incompatible with a forced tool_choice
	MaxTokens    int // 0 means 1024
	Provider     *Provider
	// Timeout bounds one HTTP attempt of this turn. Zero derives it from MaxTokens (AnthropicTurnTimeout): a
	// non-streaming turn with a large token budget and adaptive thinking legitimately takes minutes.
	Timeout time.Duration
}

type turnTimeoutKey struct{}

func turnTimeoutFrom(ctx context.Context, def time.Duration) time.Duration {
	if d, ok := ctx.Value(turnTimeoutKey{}).(time.Duration); ok && d > 0 {
		return d
	}
	return def
}

// AnthropicTurnTimeout is the per-attempt timeout of a non-streaming turn that may produce maxTokens tokens: the
// configured LLM_HTTP_TIMEOUT floor, or 30s plus 13ms per token (about 77 tokens/s worst case), whichever is longer.
// 16000 tokens give 238s.
func AnthropicTurnTimeout(base time.Duration, maxTokens int) time.Duration {
	return max(base, 30*time.Second+time.Duration(maxTokens)*13*time.Millisecond)
}

// AnthropicTurn performs one native Messages-API turn with tools, tool_choice
// and structured output and returns the raw response (leadgen/llm.py
// anthropic_tool_call). Without Provider the Anthropic default provider is used.
func (c *Client) AnthropicTurn(ctx context.Context, r AnthropicRequest) (*AnthropicResponse, error) {
	var p Provider
	if r.Provider != nil {
		p = *r.Provider
	} else {
		var ok bool
		if p, ok = c.AnthropicProvider(ctx); !ok {
			return nil, errors.New("anthropic provider not configured")
		}
	}
	if r.MaxTokens <= 0 {
		r.MaxTokens = 1024
	}
	body := map[string]any{
		"model":      p.Model,
		"max_tokens": r.MaxTokens,
		"messages":   r.Messages,
		"thinking":   map[string]any{"type": "adaptive"},
	}
	switch s := r.System.(type) {
	case nil:
	case string:
		if sb := c.systemBlocks(s); sb != nil {
			body["system"] = sb
		}
	default:
		body["system"] = s
	}
	if len(r.Tools) > 0 {
		body["tools"] = r.Tools
	}
	if r.ToolChoice != nil {
		body["tool_choice"] = r.ToolChoice
	}
	if r.OutputFormat != nil {
		body["output_config"] = map[string]any{"format": r.OutputFormat}
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = AnthropicTurnTimeout(c.o.Config.HTTPTimeout, r.MaxTokens)
	}
	return c.anthropicMessages(context.WithValue(ctx, turnTimeoutKey{}, timeout), p, body)
}

// Per-MTok prices (USD input, output). An unknown model is priced at the most
// expensive tier so a budget trips early and never runs away.
var anthropicPrices = []struct {
	Model   string
	In, Out float64
}{
	{"claude-opus-4-8", 5, 25}, {"claude-opus-4-7", 5, 25}, {"claude-opus-4-6", 5, 25}, {"claude-opus-4-5", 5, 25},
	{"claude-sonnet-4-6", 3, 15}, {"claude-haiku-4-5", 1, 5}, {"claude-fable-5", 10, 50}, {"claude-mythos-5", 10, 50},
}

// PriceForModel returns (input, output) USD per million tokens; exact id first,
// then a known prefix (date-suffixed ids), else the most expensive tier.
func PriceForModel(model string) (in, out float64) {
	m := strings.TrimSpace(model)
	for _, p := range anthropicPrices {
		if m == p.Model {
			return p.In, p.Out
		}
	}
	for _, p := range anthropicPrices {
		if strings.HasPrefix(m, p.Model) {
			return p.In, p.Out
		}
	}
	return 10, 50
}

// AnthropicCostUSD prices one turn: input and output at the model rate, cache
// writes at 1.25x and cache reads at 0.1x of the input rate. An empty model
// means AnthropicDefaultModel.
func AnthropicCostUSD(u AnthropicUsage, model string) float64 {
	if model == "" {
		model = AnthropicDefaultModel
	}
	in, out := PriceForModel(model)
	return (float64(u.InputTokens)*in +
		float64(u.OutputTokens)*out +
		float64(u.CacheCreationInputTokens)*in*1.25 +
		float64(u.CacheReadInputTokens)*in*0.1) / 1_000_000.0
}
