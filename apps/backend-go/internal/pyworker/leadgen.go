package pyworker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

const (
	sourceListPath   = "/internal/leadgen/source-list"
	sourceProbePath  = "/internal/leadgen/source-probe"
	maxLeadgenBody   = 4 << 20
	errLeadgenReject = "pyworker: python worker rejected the request"
)

// leadgenDo makes one bounded leadgen RPC call: a hard deadline, no retry, no redirect, a capped
// response body, and errors that never carry the token or the upstream text.
func (c *Client) leadgenDo(ctx context.Context, method, path string, body []byte, timeout time.Duration) ([]byte, error) {
	if c == nil || c.baseURL == "" || c.token == "" {
		return nil, ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rd)
	if err != nil {
		return nil, errors.New("pyworker: request invalid")
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token.Reveal())
	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("pyworker: python worker unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(errLeadgenReject)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxLeadgenBody+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("pyworker: python worker response unreadable")
	}
	if len(data) > maxLeadgenBody {
		return nil, errors.New("pyworker: python worker response too large")
	}
	return data, nil
}

// SourceList is GET /internal/leadgen/source-list: the names of every registry source (enabled or
// not, so disabled ones keep being probed and can recover).
func (c *Client) SourceList(ctx context.Context, timeout time.Duration) ([]string, error) {
	data, err := c.leadgenDo(ctx, http.MethodGet, sourceListPath, nil, timeout)
	if err != nil {
		return nil, err
	}
	var out struct {
		Sources []struct {
			Name string `json:"name"`
		} `json:"sources"`
	}
	if json.Unmarshal(data, &out) != nil {
		return nil, ErrProtocol
	}
	names := make([]string, 0, len(out.Sources))
	for _, s := range out.Sources {
		if s.Name != "" {
			names = append(names, s.Name)
		}
	}
	return names, nil
}

// SourceProbe is POST /internal/leadgen/source-probe for one batch. A source the worker did not
// finish (or whose probe raised) answers nil. The call's deadline is timeout plus the client slack,
// so the worker's own timeout_s fires first.
func (c *Client) SourceProbe(ctx context.Context, names []string, timeout time.Duration) (map[string]*int, error) {
	body, err := json.Marshal(map[string]any{"sources": names, "timeout_s": timeout.Seconds()})
	if err != nil {
		return nil, ErrProtocol
	}
	data, err := c.leadgenDo(ctx, http.MethodPost, sourceProbePath, body, timeout+c.slack)
	if err != nil {
		return nil, err
	}
	var out struct {
		Yields map[string]*int `json:"yields"`
	}
	if json.Unmarshal(data, &out) != nil || out.Yields == nil {
		return nil, ErrProtocol
	}
	return out.Yields, nil
}
