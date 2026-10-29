// Package progress publishes workbook run progress to Redis pub/sub. It ports
// enrichment._broadcast: JSON on channel "workbook:<id>", best-effort, never
// fatal to the run.
package progress

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

const publishTimeout = 2 * time.Second

// Publisher sends progress messages. Implementations must be safe for
// concurrent use and must not return errors that abort a run.
type Publisher interface {
	Publish(ctx context.Context, workbookID string, msg map[string]any)
}

// Channel is the pub/sub channel the WebSocket relay subscribes to.
func Channel(workbookID string) string { return "workbook:" + workbookID }

// Redis publishes to a Redis server.
type Redis struct {
	c   *redis.Client
	log *slog.Logger
}

// NewRedis connects lazily from a redis:// URL.
func NewRedis(url string, log *slog.Logger) (*Redis, error) {
	opt, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("progress: parse REDIS_URL: %w", err)
	}
	if log == nil {
		log = slog.Default()
	}
	return &Redis{c: redis.NewClient(opt), log: log}, nil
}

// Close releases the connection pool.
func (r *Redis) Close() error { return r.c.Close() }

// Marshal encodes msg like json.dumps(message): a single JSON object. Keys are
// sorted and HTML is not escaped; subscribers parse it, so byte order is not
// part of the contract (the key set is).
func Marshal(msg map[string]any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(msg); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// Publish sends msg; failures are logged and swallowed (Python logs a warning).
// A cancelled job context must not suppress the final status message, so the
// publish uses its own short deadline detached from ctx cancellation.
func (r *Redis) Publish(ctx context.Context, workbookID string, msg map[string]any) {
	if r == nil {
		return
	}
	b, err := Marshal(msg)
	if err != nil {
		r.log.Warn("redis broadcast failed", "err", err)
		return
	}
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), publishTimeout)
	defer cancel()
	if err := r.c.Publish(pctx, Channel(workbookID), b).Err(); err != nil {
		r.log.Warn("redis broadcast failed", "err", err)
	}
}

// Nop drops every message.
type Nop struct{}

// Publish implements Publisher.
func (Nop) Publish(context.Context, string, map[string]any) {}
