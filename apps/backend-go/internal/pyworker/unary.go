package pyworker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"time"
)

const (
	systemStatsPath  = "/internal/leadgen/system-stats"
	maxUnaryBody     = 1 << 20
	errUnaryRejected = "pyworker: python worker rejected the request"
)

// SystemStats calls the unary POST /internal/leadgen/system-stats RPC (an empty JSON object body: the endpoint takes
// no workspace or user input) and returns the raw JSON answer. The call has one attempt and a hard deadline of
// timeout (a 3 s budget in the stats route); there is no retry and no redirect. Errors never carry the token or the
// upstream response text. ErrUnavailable when no worker is configured.
func (c *Client) SystemStats(ctx context.Context, timeout time.Duration) ([]byte, error) {
	if c == nil || c.baseURL == "" || c.token == "" {
		return nil, ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+systemStatsPath, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, errors.New("pyworker: request invalid")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token.Reveal())
	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("pyworker: python worker unreachable") // url.Error text is dropped on purpose
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(errUnaryRejected)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxUnaryBody+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("pyworker: python worker response unreadable")
	}
	if len(data) > maxUnaryBody {
		return nil, errors.New("pyworker: python worker response too large")
	}
	return data, nil
}
