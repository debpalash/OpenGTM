// Package leadprogress is the Go port of services/leadgen/progress.py: a
// workspace-isolated progress bus. Events are published on the Redis channel
// leadgen:progress:<workspace> (live SSE relay, see leadevents) and pushed
// onto a bounded list leadgen:progress:<workspace>:recent (reconnect backfill).
package leadprogress

import (
	"context"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	recentCap    = 500
	redisBackoff = time.Second // Python _forget_redis: skip Redis for 1s after a failure
)

// Channel is the pub/sub channel of one workspace.
func Channel(workspaceID string) string { return "leadgen:progress:" + workspaceID }

// RecentKey is the bounded history list of one workspace.
func RecentKey(workspaceID string) string { return Channel(workspaceID) + ":recent" }

// Bus publishes progress events. A nil client behaves like Python without
// REDIS_URL: events only land in the in-process fallback deque (which Recent
// reads back, tenant filtered).
type Bus struct {
	rdb *redis.Client
	now func() time.Time

	mu         sync.Mutex
	local      []map[string]any // Python's deque(maxlen=500)
	retryAfter time.Time
}

// NewBus returns a bus on rdb (nil allowed: no-op Redis).
func NewBus(rdb *redis.Client) *Bus { return &Bus{rdb: rdb, now: time.Now} }

type redisOp struct {
	cmd  string // publish | lpush | ltrim
	key  string
	arg  string
	from int64
	to   int64
}

// buildEvent mirrors ProgressBus.emit: {"type", "ts", **data} (a data key
// "type"/"ts" keeps its first position but takes the data value), then
// workspace_id forced to the server-side value when there is one.
func buildEvent(workspaceID, eventType string, ts float64, data Obj) Obj {
	ev := Obj{{"type", eventType}, {"ts", ts}}
	for _, f := range data {
		ev = ev.set(f.Key, f.Val)
	}
	if workspaceID != "" {
		ev = ev.set("workspace_id", workspaceID)
	}
	return ev
}

// buildOps is the pipeline for an event; empty when unscoped (an unscoped
// event is never put on a shared channel).
func buildOps(workspaceID string, ev Obj) []redisOp {
	if workspaceID == "" {
		return nil
	}
	enc := string(dumps(ev))
	return []redisOp{
		{cmd: "publish", key: Channel(workspaceID), arg: enc},
		{cmd: "lpush", key: RecentKey(workspaceID), arg: enc},
		{cmd: "ltrim", key: RecentKey(workspaceID), from: 0, to: recentCap - 1},
	}
}

// Emit publishes one event for workspaceID and returns it. data keys are
// ordered sorted (use EmitOrdered when Python's insertion order matters). A
// non-empty workspaceID overrides data["workspace_id"]; an empty one means the
// event stays local and is never published.
func (b *Bus) Emit(ctx context.Context, workspaceID, eventType string, data map[string]any) map[string]any {
	return b.EmitOrdered(ctx, workspaceID, eventType, objFromMap(data))
}

// EmitOrdered is Emit with the data keys in the caller's order.
func (b *Bus) EmitOrdered(ctx context.Context, workspaceID, eventType string, data Obj) map[string]any {
	ts := float64(b.now().UnixNano()) / 1e9
	ev := buildEvent(workspaceID, eventType, ts, data)
	m := ev.toMap()

	b.mu.Lock()
	b.local = append(b.local, m)
	if len(b.local) > recentCap {
		b.local = append(b.local[:0:0], b.local[len(b.local)-recentCap:]...)
	}
	b.mu.Unlock()

	if ops := buildOps(workspaceID, ev); ops != nil {
		b.run(ctx, ops)
	}
	return m
}

// redisUsable reports whether Redis may be tried now (client set, not in the
// post-failure backoff window).
func (b *Bus) redisUsable() bool {
	if b == nil || b.rdb == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.now().Before(b.retryAfter)
}

func (b *Bus) forgetRedis() {
	b.mu.Lock()
	b.retryAfter = b.now().Add(redisBackoff)
	b.mu.Unlock()
}

func (b *Bus) run(ctx context.Context, ops []redisOp) {
	if !b.redisUsable() {
		return
	}
	pipe := b.rdb.Pipeline() // non-transactional, like Python
	for _, op := range ops {
		switch op.cmd {
		case "publish":
			pipe.Publish(ctx, op.key, op.arg)
		case "lpush":
			pipe.LPush(ctx, op.key, op.arg)
		case "ltrim":
			pipe.LTrim(ctx, op.key, op.from, op.to)
		}
	}
	if _, err := pipe.Exec(ctx); err != nil {
		b.forgetRedis() // fail-soft: the in-process deque still has the event
	}
}
