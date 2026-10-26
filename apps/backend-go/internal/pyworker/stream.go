package pyworker

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	maxFrameBytes      = 4 << 20   // one NDJSON line
	maxStreamFrames    = 200_000   // frames per stream (defence in depth vs Python caps)
	maxStreamBytes     = 256 << 20 // total bytes per stream
	rpcPrefix          = "/internal/copilot/"
	callIDHeader       = "X-Call-Id"
	readerBufSize      = 64 << 10
	unaryMaxRespBytes  = 4 << 20
	defaultUnaryTimout = 60 * time.Second
)

// ErrTruncated means the stream ended without a terminal frame.
var ErrTruncated = errors.New("pyworker: stream ended without a terminal frame")

// ErrProtocol means the Python side violated the framing contract (oversized
// frame, malformed frame, seq regression, caps exceeded).
var ErrProtocol = errors.New("pyworker: protocol violation")

// ErrTimeout is the Python worker's 408 or a client-side unary deadline.
var ErrTimeout = errors.New("pyworker: python worker timed out")

// ErrClosed is returned by Next after Close.
var ErrClosed = errors.New("pyworker: stream closed")

// Frame is one NDJSON frame. T is the frame type (preroute: sse|none|done|
// error; llm: content|tool_calls|persist|done|error ...). Unknown types are
// passed through untouched; callers ignore what they do not understand.
// Seq is 0 when the frame carries none; when present it must increase.
type Frame struct {
	T        string
	Seq      int
	D        json.RawMessage
	Role     *string
	Content  *string
	ToolData *string
	Cursor   *int
	Message  string
}

type wireFrame struct {
	T        *string         `json:"t"`
	Seq      *int            `json:"seq"`
	D        json.RawMessage `json:"d"`
	Role     *string         `json:"role"`
	Content  *string         `json:"content"`
	ToolData *string         `json:"tool_data"`
	Cursor   *int            `json:"cursor"`
	Message  *string         `json:"message"`
}

func isTerminal(t string) bool { return t == "done" || t == "none" || t == "error" }

// Stream is an open NDJSON response. Not safe for concurrent use except Close.
type Stream struct {
	body     io.ReadCloser
	br       *bufio.Reader
	cancel   context.CancelFunc
	ctx      context.Context
	parent   context.Context
	maxFrame int
	maxN     int
	maxTotal int64
	n        int
	total    int64
	lastSeq  int
	done     bool // terminal frame seen
	err      error
	closed   bool
}

func randomCallID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (c *Client) ready(path string) error {
	if c == nil || c.baseURL == "" || c.token == "" {
		return ErrUnavailable
	}
	if !strings.HasPrefix(path, rpcPrefix) || strings.ContainsAny(path, "?#\\") || strings.Contains(path, "..") {
		return ErrProtocol
	}
	return nil
}

func (c *Client) newReq(ctx context.Context, path string, payload []byte, accept, callID string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return nil, ErrProtocol
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", accept)
	req.Header.Set("Authorization", "Bearer "+c.token.Reveal())
	if callID != "" {
		req.Header.Set(callIDHeader, callID)
	}
	return req, nil
}

// statusErr maps a non-2xx status to a sentinel; the body is never read into
// the error.
func statusErr(st int) error {
	switch st {
	case http.StatusUnauthorized, http.StatusServiceUnavailable:
		return ErrUnauthorized
	case http.StatusNotFound:
		return ErrUnknownProvider
	case http.StatusRequestTimeout:
		return ErrTimeout
	}
	return errors.New("pyworker: python worker error: http " + http.StatusText(st))
}

// Stream POSTs body as JSON to path (under /internal/copilot/) and returns the
// NDJSON response. The deadline is ctx's. Close (or cancelling ctx) aborts the
// HTTP request, which cancels the Python side. No retry: a stream is not
// idempotent.
func (c *Client) Stream(ctx context.Context, path string, body any) (*Stream, error) {
	if err := c.ready(path); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, ErrProtocol
	}
	rctx, cancel := context.WithCancel(ctx)
	req, err := c.newReq(rctx, path, payload, "application/x-ndjson", "")
	if err != nil {
		cancel()
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		cancel()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("pyworker: python worker unreachable") // url.Error text dropped on purpose
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		cancel()
		return nil, statusErr(resp.StatusCode)
	}
	mf, mn, mt := c.maxFrameBytes, c.maxFrames, c.maxTotalBytes
	if mf <= 0 {
		mf = maxFrameBytes
	}
	if mn <= 0 {
		mn = maxStreamFrames
	}
	if mt <= 0 {
		mt = maxStreamBytes
	}
	return &Stream{body: resp.Body, br: bufio.NewReaderSize(resp.Body, readerBufSize), cancel: cancel,
		ctx: rctx, parent: ctx, maxFrame: mf, maxN: mn, maxTotal: mt}, nil
}

// Close aborts the request and releases the connection. Idempotent.
func (s *Stream) Close() {
	if s == nil || s.closed {
		return
	}
	s.closed = true
	s.cancel()
	_ = s.body.Close()
}

func (s *Stream) fail(err error) (Frame, error) {
	s.err = err
	s.cancel()
	_ = s.body.Close()
	return Frame{}, err
}

// readLine returns the next line without the newline, bounded by maxFrame.
func (s *Stream) readLine() ([]byte, error) {
	var acc []byte
	for {
		chunk, err := s.br.ReadSlice('\n')
		s.total += int64(len(chunk))
		if s.total > s.maxTotal {
			return nil, ErrProtocol
		}
		if len(acc)+len(chunk) > s.maxFrame+1 { // +1: trailing newline
			return nil, ErrProtocol
		}
		switch {
		case err == nil:
			if acc == nil {
				return bytes.TrimRight(chunk[:len(chunk)-1], "\r"), nil
			}
			acc = append(acc, chunk[:len(chunk)-1]...)
			return bytes.TrimRight(acc, "\r"), nil
		case errors.Is(err, bufio.ErrBufferFull):
			acc = append(acc, chunk...)
		default: // io.EOF or read error; a partial line is truncation
			return nil, err
		}
	}
}

// Next returns the next frame. After a terminal frame (done|none|error) has
// been returned, the following call returns io.EOF. A connection that ends
// before a terminal frame yields ErrTruncated. Errors are sticky.
func (s *Stream) Next() (Frame, error) {
	if s.err != nil {
		return Frame{}, s.err
	}
	if s.closed {
		return Frame{}, ErrClosed
	}
	if s.done {
		s.err = io.EOF
		s.Close()
		return Frame{}, io.EOF
	}
	for {
		line, err := s.readLine()
		if err != nil {
			if s.parent.Err() != nil {
				return s.fail(s.parent.Err())
			}
			if errors.Is(err, ErrProtocol) {
				return s.fail(ErrProtocol)
			}
			return s.fail(ErrTruncated)
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		s.n++
		if s.n > s.maxN {
			return s.fail(ErrProtocol)
		}
		var w wireFrame
		if json.Unmarshal(line, &w) != nil || w.T == nil || *w.T == "" {
			return s.fail(ErrProtocol)
		}
		f := Frame{T: *w.T, D: w.D, Role: w.Role, Content: w.Content, ToolData: w.ToolData, Cursor: w.Cursor}
		if w.Message != nil {
			f.Message = *w.Message
		}
		if w.Seq != nil {
			if *w.Seq <= s.lastSeq {
				return s.fail(ErrProtocol)
			}
			s.lastSeq = *w.Seq
			f.Seq = *w.Seq
		}
		if isTerminal(f.T) {
			s.done = true
		}
		return f, nil
	}
}

// Unary POSTs body and decodes the JSON answer into out (nil discards it).
// One retry, with the same X-Call-Id, only when the connection fails before any
// response header; never after a response started. 408 and a client-side
// deadline map to ErrTimeout.
func (c *Client) Unary(ctx context.Context, path string, body, out any, timeout time.Duration) error {
	if err := c.ready(path); err != nil {
		return err
	}
	if timeout <= 0 {
		timeout = defaultUnaryTimout
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return ErrProtocol
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	callID := randomCallID()
	var resp *http.Response
	for attempt := 0; ; attempt++ {
		req, err := c.newReq(rctx, path, payload, "application/json", callID)
		if err != nil {
			return err
		}
		resp, err = c.hc.Do(req)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if rctx.Err() != nil {
			return ErrTimeout
		}
		if attempt == 1 {
			return errors.New("pyworker: python worker unreachable")
		}
		t := time.NewTimer(c.backoff)
		select {
		case <-rctx.Done():
			t.Stop()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return ErrTimeout
		case <-t.C:
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return statusErr(resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, unaryMaxRespBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if rctx.Err() != nil {
			return ErrTimeout
		}
		return errors.New("pyworker: python worker response unreadable")
	}
	if len(data) > unaryMaxRespBytes {
		return ErrProtocol
	}
	if out == nil {
		return nil
	}
	if json.Unmarshal(data, out) != nil {
		return ErrProtocol
	}
	return nil
}
