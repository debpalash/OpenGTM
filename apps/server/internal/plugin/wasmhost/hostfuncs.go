package wasmhost

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	extism "github.com/extism/go-sdk"

	"github.com/debpalash/OpenGTM/apps/server/internal/egress"
	"github.com/debpalash/OpenGTM/apps/server/internal/plugin/manifest"
)

// callKey carries per-call state to host functions through the context.
type callKey struct{}

type callState struct {
	plugin  *manifest.Plugin
	host    *Host
	secrets map[string]string

	mu      sync.Mutex
	fetches []FetchEvidence
	logs    int
}

// FetchRequest is the JSON a plugin passes to opengtm_fetch.
type FetchRequest struct {
	Method     string            `json:"method"`
	URL        string            `json:"url"`
	Headers    map[string]string `json:"headers"`
	Body       string            `json:"body"`
	BodyBase64 string            `json:"body_base64"`
}

// FetchEvidence is recorded by the host for every fetch (secrets redacted).
type FetchEvidence struct {
	URL         string    `json:"url"`
	Status      int       `json:"status"`
	FetchedAt   time.Time `json:"fetched_at"`
	ContentType string    `json:"content_type"`
	Bytes       int       `json:"bytes"`
	SHA256      string    `json:"sha256"`
}

// FetchResponse is the JSON opengtm_fetch returns.
type FetchResponse struct {
	Status     int               `json:"status,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	Body       string            `json:"body,omitempty"`
	BodyBase64 string            `json:"body_base64,omitempty"`
	Evidence   *FetchEvidence    `json:"evidence,omitempty"`
	Error      *FetchError       `json:"error,omitempty"`
}

// FetchError is a structured, plugin-visible failure.
type FetchError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func redact(s string, secrets map[string]string) string {
	for _, v := range secrets {
		if len(v) < 4 {
			continue
		}
		s = strings.ReplaceAll(s, v, "REDACTED")
		if q := url.QueryEscape(v); q != v {
			s = strings.ReplaceAll(s, q, "REDACTED")
		}
	}
	return s
}

func (h *Host) hostFunctions() []extism.HostFunction {
	fetch := extism.NewHostFunctionWithStack("opengtm_fetch",
		func(ctx context.Context, p *extism.CurrentPlugin, stack []uint64) {
			reqBytes, err := p.ReadBytes(stack[0])
			var resp FetchResponse
			if err != nil {
				resp.Error = &FetchError{Code: "bad_request", Message: "cannot read request"}
			} else {
				resp = h.fetch(ctx, reqBytes)
			}
			out, _ := json.Marshal(resp)
			off, err := p.WriteBytes(out)
			if err != nil {
				panic(err) // out of guest memory: abort the call
			}
			stack[0] = off
		}, []extism.ValueType{extism.ValueTypePTR}, []extism.ValueType{extism.ValueTypePTR})
	logf := extism.NewHostFunctionWithStack("opengtm_log",
		func(ctx context.Context, p *extism.CurrentPlugin, stack []uint64) {
			level, err1 := p.ReadString(stack[0])
			msg, err2 := p.ReadString(stack[1])
			if err1 != nil || err2 != nil {
				return
			}
			h.log(ctx, level, msg)
		}, []extism.ValueType{extism.ValueTypePTR, extism.ValueTypePTR}, nil)
	return []extism.HostFunction{fetch, logf}
}

func (h *Host) log(ctx context.Context, level, msg string) {
	st, _ := ctx.Value(callKey{}).(*callState)
	if st == nil {
		return
	}
	st.mu.Lock()
	st.logs++
	n := st.logs
	st.mu.Unlock()
	if n > maxLogLines {
		return
	}
	if len(msg) > maxLogBytes {
		msg = msg[:maxLogBytes]
		for !utf8.ValidString(msg) {
			msg = msg[:len(msg)-1]
		}
	}
	lvl := slog.LevelInfo
	switch strings.ToLower(level) {
	case "debug", "trace":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}
	h.opts.Logger.Log(ctx, lvl, redact(msg, st.secrets), "plugin", st.plugin.Name, "source", "wasm")
}

func fetchErr(code, msg string) FetchResponse {
	return FetchResponse{Error: &FetchError{Code: code, Message: msg}}
}

func (h *Host) fetch(ctx context.Context, raw []byte) FetchResponse {
	st, _ := ctx.Value(callKey{}).(*callState)
	if st == nil {
		return fetchErr("internal", "no call context")
	}
	if h.opts.Client == nil {
		return fetchErr("network_unavailable", "this host has no egress client")
	}
	var req FetchRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return fetchErr("bad_request", "request must be JSON {method,url,headers,body}")
	}
	p := st.plugin
	st.mu.Lock()
	if len(st.fetches) >= p.Limits.MaxPages*maxFetchPerPage {
		st.mu.Unlock()
		return fetchErr("too_many_fetches", "fetch budget for this call is exhausted (limits.max_pages)")
	}
	st.mu.Unlock()
	if err := p.Network().Allows(req.URL); err != nil {
		return fetchErr("capability_denied", redact(err.Error(), st.secrets))
	}
	body := []byte(req.Body)
	if req.BodyBase64 != "" {
		b, err := base64.StdEncoding.DecodeString(req.BodyBase64)
		if err != nil {
			return fetchErr("bad_request", "body_base64 is not valid base64")
		}
		body = b
	}
	if len(body) == 0 {
		body = nil
	}
	hdr := http.Header{}
	for k, v := range req.Headers {
		hdr.Set(k, v)
	}
	var sensitive []string
	for k := range req.Headers {
		sensitive = append(sensitive, k) // plugin headers never follow a cross-origin redirect
	}
	resp, err := h.opts.Client.Do(ctx, egress.Request{
		Method:           req.Method,
		URL:              req.URL,
		Header:           hdr,
		Body:             body,
		Robots:           p.Kind == "scraper" || p.Kind == "signal",
		RPS:              p.Limits.RequestsPerSecondPerDomain,
		MaxBodyBytes:     p.Limits.MaxResponseBytes,
		Allow:            p.Network().AllowsURL,
		SensitiveHeaders: sensitive,
	})
	if err != nil {
		return fetchErr(classify(err), redact(err.Error(), st.secrets))
	}
	ev := FetchEvidence{
		URL: redact(resp.Evidence.URL, st.secrets), Status: resp.Status, FetchedAt: resp.Evidence.FetchedAt,
		ContentType: resp.Evidence.ContentType, Bytes: resp.Evidence.Bytes, SHA256: resp.Evidence.SHA256,
	}
	st.mu.Lock()
	st.fetches = append(st.fetches, ev)
	st.mu.Unlock()
	out := FetchResponse{Status: resp.Status, Headers: map[string]string{}, Evidence: &ev}
	for k := range resp.Header {
		out.Headers[strings.ToLower(k)] = resp.Header.Get(k)
	}
	if utf8.Valid(resp.Body) {
		out.Body = string(resp.Body)
	} else {
		out.BodyBase64 = base64.StdEncoding.EncodeToString(resp.Body)
	}
	return out
}

// maxFetchPerPage lets a plugin make a few API calls per declared page.
const maxFetchPerPage = 1

func classify(err error) string {
	var be *egress.BlockedError
	var nd *manifest.ErrNetworkDenied
	var rl *egress.RateLimitError
	switch {
	case errors.As(err, &nd):
		return "capability_denied"
	case errors.As(err, &be):
		return "blocked_url"
	case errors.Is(err, egress.ErrRobotsDisallowed), errors.Is(err, egress.ErrRobotsUnavailable):
		return "robots_disallowed"
	case errors.Is(err, egress.ErrBodyTooLarge):
		return "body_too_large"
	case errors.As(err, &rl):
		return "rate_limited"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	return "fetch_failed"
}
