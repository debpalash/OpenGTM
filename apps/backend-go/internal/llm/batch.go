package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// BatchRequest is one entry of an Anthropic Message Batch.
type BatchRequest struct {
	CustomID  string
	Prompt    string
	System    string
	MaxTokens int // 0 means 1024
}

// BatchOptions tunes BatchComplete. Zero values: PollInterval 5s, MaxWait 24h.
type BatchOptions struct {
	Provider       *Provider // default: AnthropicProvider
	PollInterval   time.Duration
	MaxWait        time.Duration // counted in poll intervals, as in Python
	ShouldStop     func() bool   // polled while waiting; true cancels the batch
	BatchID        string        // resume an already submitted batch
	OnSubmitted    func(batchID string)
	StrictResults  bool // errors reading results (or a batch still processing at MaxWait) are returned
	OnResult       func(customID, text string)
	KnownResultIDs []string // results already accounted for in usage (resume)
}

// BatchComplete runs many completions as ONE Message Batch (about 50% cost,
// asynchronous) and returns {custom_id: text} for the succeeded, non-empty
// results; failed ids are absent so the caller can fall back per row. The shared
// system prompt carries one cache breakpoint per request. Results are keyed by
// custom_id, never by position.
func (c *Client) BatchComplete(ctx context.Context, reqs []BatchRequest, o BatchOptions) (map[string]string, error) {
	results := map[string]string{}
	if len(reqs) == 0 {
		return results, nil
	}
	var p Provider
	if o.Provider != nil {
		p = *o.Provider
	} else {
		var ok bool
		if p, ok = c.AnthropicProvider(ctx); !ok {
			return nil, errors.New("anthropic provider not configured for batch run")
		}
	}
	if o.PollInterval <= 0 {
		o.PollInterval = 5 * time.Second
	}
	if o.MaxWait <= 0 {
		o.MaxWait = 24 * time.Hour
	}

	batchID := o.BatchID
	if batchID == "" {
		items := make([]orderedObject, 0, len(reqs))
		for _, r := range reqs {
			mt := r.MaxTokens
			if mt <= 0 {
				mt = 1024
			}
			params := orderedObject{
				{"model", p.Model}, {"max_tokens", mt},
				{"messages", []orderedObject{{{"role", "user"}, {"content", r.Prompt}}}},
				{"thinking", map[string]any{"type": "adaptive"}},
			}
			if sb := c.systemBlocks(r.System); sb != nil {
				params = append(params, kv{"system", sb})
			}
			items = append(items, orderedObject{{"custom_id", r.CustomID}, {"params", params}})
		}
		var created struct {
			ID string `json:"id"`
		}
		if err := c.anthropicJSON(ctx, p, http.MethodPost, "/v1/messages/batches", map[string]any{"requests": items}, &created); err != nil {
			return nil, err
		}
		batchID = created.ID
		if o.OnSubmitted != nil {
			o.OnSubmitted(batchID)
		}
	}

	path := "/v1/messages/batches/" + url.PathEscape(batchID)
	var waited time.Duration
	for {
		if o.ShouldStop != nil && o.ShouldStop() {
			// Best effort, like Python; stopped mid-run means nothing is mapped back.
			_ = c.anthropicJSON(context.WithoutCancel(ctx), p, http.MethodPost, path+"/cancel", nil, nil)
			return results, nil
		}
		var st struct {
			ProcessingStatus string `json:"processing_status"`
		}
		if err := c.anthropicJSON(ctx, p, http.MethodGet, path, nil, &st); err != nil {
			return nil, err
		}
		if st.ProcessingStatus == "ended" {
			break
		}
		if waited >= o.MaxWait {
			if o.StrictResults {
				return nil, errors.New("Batch is still processing; retrieve the existing batch before retrying cells")
			}
			c.o.Log.Warn("llm: anthropic batch exceeded max wait; abandoning", "batch", batchID)
			break
		}
		if err := c.o.Sleep(ctx, o.PollInterval); err != nil {
			return nil, err
		}
		waited += o.PollInterval
	}

	accounted := map[string]bool{}
	for _, id := range o.KnownResultIDs {
		accounted[id] = true
	}
	if err := c.readBatchResults(ctx, p, path+"/results", accounted, results, o); err != nil {
		if o.StrictResults {
			return nil, err
		}
		c.o.Log.Warn("llm: anthropic batch results read failed", "batch", batchID, "err", err)
	}
	return results, nil
}

func (c *Client) readBatchResults(ctx context.Context, p Provider, path string, accounted map[string]bool, results map[string]string, o BatchOptions) error {
	rc, err := c.anthropicCall(ctx, p, http.MethodGet, path, nil, true)
	if err != nil {
		return err
	}
	defer rc.Close()
	br := bufio.NewReaderSize(rc, 1<<20)
	for {
		line, rerr := br.ReadBytes('\n')
		if s := strings.TrimSpace(string(line)); s != "" {
			var item struct {
				CustomID string `json:"custom_id"`
				Result   struct {
					Type    string             `json:"type"`
					Message *AnthropicResponse `json:"message"`
				} `json:"result"`
			}
			if err := json.Unmarshal([]byte(s), &item); err != nil {
				return fmt.Errorf("invalid batch result line: %w", err)
			}
			if item.Result.Type == "succeeded" && item.Result.Message != nil {
				msg := item.Result.Message
				if !accounted[item.CustomID] {
					c.addAnthropicUsage(ctx, p, msg.Usage)
					accounted[item.CustomID] = true
				}
				if text := msg.Text(); text != "" {
					if o.OnResult != nil {
						o.OnResult(item.CustomID, text)
					}
					results[item.CustomID] = text
				}
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return nil
			}
			return rerr
		}
	}
}

// anthropicJSON is a Messages-API request whose JSON answer is decoded into out (nil: discard).
func (c *Client) anthropicJSON(ctx context.Context, p Provider, method, path string, body, out any) error {
	rc, err := c.anthropicCall(ctx, p, method, path, body, false)
	if err != nil {
		return err
	}
	defer rc.Close()
	raw, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("invalid JSON in Messages API response: %w", err)
	}
	return nil
}
