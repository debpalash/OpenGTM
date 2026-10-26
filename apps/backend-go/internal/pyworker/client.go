package pyworker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	callPath       = "/internal/provider-call"
	maxTimeout     = 120 * time.Second // mirrors MAX_TIMEOUT_S on the Python side
	defaultTimeout = 10 * time.Second  // DEFAULT_TIMEOUT_S
	clientSlack    = 5 * time.Second
	maxBodyBytes   = 4 << 20
)

// ErrUnavailable means no Python worker is configured (PY_WORKER_URL empty);
// callers hand the job back to the Python executor.
var ErrUnavailable = errors.New("pyworker: python worker unavailable")

// Client calls the Python provider RPC. The token is never logged, returned in
// errors or printed (String redacts it).
type Client struct {
	baseURL string
	token   string
	hc      *http.Client
	slack   time.Duration
}

// New builds a client. hc may be nil; per-call deadlines come from the call's
// Timeout (plus slack), not from hc.Timeout. Redirects are never followed.
func New(baseURL, token string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{}
	} else {
		cp := *hc
		hc = &cp
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"), token: token, hc: hc, slack: clientSlack}
}

// String keeps the token out of any formatted output.
func (c *Client) String() string {
	if c == nil {
		return "pyworker.Client(nil)"
	}
	return "pyworker.Client{url:" + c.baseURL + ", token:[redacted]}"
}

type wireRequest struct {
	CallID      string          `json:"call_id"`
	Provider    string          `json:"provider"`
	WorkspaceID string          `json:"workspace_id"`
	Input       json.RawMessage `json:"input"`
	TimeoutS    float64         `json:"timeout_s"`
}

type wireResponse struct {
	Status  string          `json:"status"`
	Output  json.RawMessage `json:"output"`
	Error   *string         `json:"error"`
	CostUSD float64         `json:"cost_usd"`
}

func errResult(msg string) ProviderResult { return ProviderResult{Status: "error", Error: msg} }

// Call runs one provider on the Python worker. Transport failures and 5xx map
// to Status "error" after one retry with the same CallID (Python dedupes only
// "ok" results, so a retry of a failed call re-runs it). A call that outlives
// Timeout+5s is reported as Status "timeout" and is not retried. Cancelling ctx
// aborts the request (Python kills the child) and returns ctx's error.
func (c *Client) Call(ctx context.Context, call ProviderCall) (ProviderResult, error) {
	if c == nil || c.baseURL == "" {
		return ProviderResult{}, ErrUnavailable
	}
	timeout := call.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	if timeout > maxTimeout {
		timeout = maxTimeout
	}
	input := call.Input
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	body, err := json.Marshal(wireRequest{CallID: call.CallID, Provider: call.Provider,
		WorkspaceID: call.WorkspaceID, Input: input, TimeoutS: timeout.Seconds()})
	if err != nil {
		return errResult("invalid provider call input"), nil
	}
	var last ProviderResult
	for attempt := 0; attempt < 2; attempt++ {
		res, retry, err := c.once(ctx, body, timeout+c.slack)
		if err != nil {
			return ProviderResult{}, err
		}
		if !retry {
			return res, nil
		}
		last = res
	}
	return last, nil
}

// once makes one HTTP attempt. retry reports a retryable failure (connection
// error or 5xx).
func (c *Client) once(ctx context.Context, body []byte, limit time.Duration) (ProviderResult, bool, error) {
	rctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, c.baseURL+callPath, bytes.NewReader(body))
	if err != nil {
		return errResult("python worker request invalid"), false, nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ProviderResult{}, false, ctx.Err()
		}
		if errors.Is(rctx.Err(), context.DeadlineExceeded) {
			return ProviderResult{Status: "timeout", Error: "python worker timed out"}, false, nil
		}
		return errResult("python worker unreachable"), true, nil // url.Error text is dropped on purpose
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return ProviderResult{}, false, ctx.Err()
		}
		if errors.Is(rctx.Err(), context.DeadlineExceeded) {
			return ProviderResult{Status: "timeout", Error: "python worker timed out"}, false, nil
		}
		return errResult("python worker response unreadable"), true, nil
	}
	if len(data) > maxBodyBytes {
		return errResult("python worker response too large"), false, nil
	}
	if resp.StatusCode >= 500 {
		return errResult("python worker error: http " + http.StatusText(resp.StatusCode)), true, nil
	}
	if resp.StatusCode != http.StatusOK {
		return errResult("python worker rejected call: http " + http.StatusText(resp.StatusCode)), false, nil
	}
	var w wireResponse
	if err := json.Unmarshal(data, &w); err != nil || w.Status == "" {
		return errResult("python worker returned an invalid response"), false, nil
	}
	out := ProviderResult{Status: w.Status, CostUSD: w.CostUSD}
	if len(w.Output) > 0 && string(w.Output) != "null" {
		out.Output = w.Output
	}
	if w.Error != nil {
		out.Error = *w.Error
	}
	return out, false, nil
}
