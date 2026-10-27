package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maxResponseBytes = 16 << 20

// orderedObject marshals its fields in insertion order (Python dict order), so
// request bodies keep the key order the providers have always received.
type orderedObject []kv

type kv struct {
	K string
	V any
}

func (o orderedObject) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, f := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(f.K)
		v, err := json.Marshal(f.V)
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// httpStatusError is a non-200 provider answer: "HTTP 429: message".
type httpStatusError struct {
	Status int
	Msg    string
}

func (e *httpStatusError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Msg) }

// timeoutError reports an exceeded LLM_HTTP_TIMEOUT.
type timeoutError struct{ d time.Duration }

func (e *timeoutError) Error() string { return fmt.Sprintf("request timed out after %s", e.d) }

func (c *Client) callOpenAI(ctx context.Context, p Provider, req CompleteRequest, temp float64) (string, error) {
	msgs := make([]orderedObject, 0, 2)
	if req.System != "" {
		msgs = append(msgs, orderedObject{{"role", "system"}, {"content", req.System}})
	}
	msgs = append(msgs, orderedObject{{"role", "user"}, {"content", req.Prompt}})
	body, err := json.Marshal(orderedObject{
		{"model", p.Model}, {"messages", msgs}, {p.TokenParam, req.MaxTokens}, {"temperature", temp},
	})
	if err != nil {
		return "", err
	}

	cctx, cancel := context.WithTimeout(ctx, c.o.Config.HTTPTimeout)
	defer cancel()
	hreq, err := http.NewRequestWithContext(cctx, http.MethodPost, p.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", stripURL(err)
	}
	hreq.Header.Set("Authorization", "Bearer "+p.APIKey)
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := c.o.HTTP.Do(hreq)
	if err != nil {
		return "", c.wrapTransportErr(ctx, cctx, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return "", c.wrapTransportErr(ctx, cctx, err)
	}

	var data struct {
		Error   json.RawMessage `json:"error"`
		Choices []struct {
			Message struct {
				Content   any `json:"content"`
				Reasoning any `json:"reasoning"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     float64 `json:"prompt_tokens"`
			CompletionTokens float64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	jsonErr := json.Unmarshal(raw, &data)

	if resp.StatusCode != http.StatusOK {
		// Python parsed the body first, so a non-JSON error page surfaced as an
		// unrelated decode error; the status line is reported here.
		msg := ""
		if jsonErr != nil {
			msg = strings.TrimSpace(string(raw))
		} else if len(data.Error) > 0 {
			var obj map[string]any
			var s string
			switch {
			case json.Unmarshal(data.Error, &obj) == nil && obj != nil:
				if m, ok := obj["message"].(string); ok {
					msg = m
				}
			case json.Unmarshal(data.Error, &s) == nil:
				msg = s
			default:
				msg = string(data.Error)
			}
		}
		return "", &httpStatusError{Status: resp.StatusCode, Msg: truncRunes(msg, 100)}
	}
	if jsonErr != nil {
		return "", fmt.Errorf("invalid JSON in provider response: %w", jsonErr)
	}

	prompt, completion := int(data.Usage.PromptTokens), int(data.Usage.CompletionTokens)
	c.usage.add(prompt, completion, p.ID, 0, 0)
	c.record(ctx, UsageRecord{
		Provider: p.ID, Model: p.Model, PromptTokens: prompt, CompletionTokens: completion,
		RateLimit:     headerInt(resp.Header, "X-RateLimit-Limit", "x-ratelimit-limit-requests"),
		RateRemaining: headerInt(resp.Header, "X-RateLimit-Remaining", "x-ratelimit-remaining-requests"),
		RateReset:     headerStr(resp.Header, "X-RateLimit-Reset", "x-ratelimit-reset-requests"),
	})

	if len(data.Choices) == 0 {
		return "", nil
	}
	m := data.Choices[0].Message
	if s, ok := m.Content.(string); ok && s != "" {
		return strings.TrimSpace(s), nil
	}
	if s, ok := m.Reasoning.(string); ok {
		return strings.TrimSpace(s), nil
	}
	return "", nil
}

// wrapTransportErr maps a failed request to a short error: an exceeded
// LLM_HTTP_TIMEOUT becomes a timeoutError, anything else keeps its text.
func (c *Client) wrapTransportErr(parent, req context.Context, err error) error {
	if parent.Err() == nil && errors.Is(req.Err(), context.DeadlineExceeded) {
		return &timeoutError{c.o.Config.HTTPTimeout}
	}
	return stripURL(err)
}

// urlFreeError is a transport error whose request URL has been reduced to its host: the path, query and
// userinfo of an operator-set base URL must not end up in usage errors or logs.
type urlFreeError struct {
	op, host string
	err      error
}

func (e *urlFreeError) Error() string { return e.op + " " + e.host + ": " + e.err.Error() }
func (e *urlFreeError) Unwrap() error { return e.err }

// stripURL replaces the *url.Error of a failed request with host-only text (errors.Is/As still reach the cause).
func stripURL(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	host := "invalid-url"
	if u, perr := url.Parse(ue.URL); perr == nil && u.Host != "" {
		host = u.Host
	}
	return &urlFreeError{op: ue.Op, host: host, err: ue.Err}
}

func headerStr(h http.Header, names ...string) string {
	for _, n := range names {
		if v := h.Get(n); v != "" {
			return v
		}
	}
	return ""
}

// headerInt is the first non-empty header as an int, 0 when absent or not a
// number (Python's int() raised on garbage and discarded the usage row).
func headerInt(h http.Header, names ...string) int {
	n, err := strconv.Atoi(strings.TrimSpace(headerStr(h, names...)))
	if err != nil {
		return 0
	}
	return n
}
