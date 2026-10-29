package leadprogress

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
)

// Recent returns up to limit of the workspace's newest events, oldest first.
//
// Matches Python recent(): limit is clamped to [0, 500] (0 -> empty); Redis is
// read via LRANGE 0 limit-1 and reversed; an empty list, a Redis error, or ANY
// entry that is not valid JSON abandons Redis (1s backoff) and the in-process
// deque is returned instead (tenant filtered, last limit). Redis errors are
// never surfaced. Deliberate deviations: an empty workspaceID returns nothing
// (Python would return every tenant's local events), and valid-JSON entries
// that are not objects are dropped from the typed result (Emit never writes
// them).
func (b *Bus) Recent(ctx context.Context, workspaceID string, limit int) ([]map[string]any, error) {
	entries := b.recent(ctx, workspaceID, limit)
	rows := make([]map[string]any, len(entries))
	for i, e := range entries {
		rows[i] = e.m
	}
	return rows, nil
}

// RecentEvent is one recent event twice: decoded (for filtering) and as the JSON text that was stored
// (Redis: the exact LPUSHed string, Python's or Go's; in-process deque: the text Emit would publish).
// Callers that must keep the stored key order, which a decoded map loses, use Raw.
type RecentEvent struct {
	Raw   string
	Event map[string]any
}

// RecentEvents is Recent with the stored text kept. Oldest first.
func (b *Bus) RecentEvents(ctx context.Context, workspaceID string, limit int) []RecentEvent {
	entries := b.recent(ctx, workspaceID, limit)
	out := make([]RecentEvent, len(entries))
	for i, e := range entries {
		out[i] = RecentEvent{Raw: e.raw, Event: e.m}
	}
	return out
}

type recentEntry struct {
	raw string
	m   map[string]any
}

func (b *Bus) recent(ctx context.Context, workspaceID string, limit int) []recentEntry {
	if limit < 0 {
		limit = 0
	}
	if limit > recentCap {
		limit = recentCap
	}
	if limit == 0 || workspaceID == "" {
		return []recentEntry{}
	}
	if b.redisUsable() {
		raw, err := b.rdb.LRange(ctx, RecentKey(workspaceID), 0, int64(limit-1)).Result()
		if err != nil {
			b.forgetRedis()
		} else if len(raw) > 0 {
			rows := make([]recentEntry, 0, len(raw))
			for _, item := range raw {
				// An invalid entry is skipped (Python falls back to the local deque for the whole call); bad data
				// is no reason to back Redis off (audit F10).
				v, derr := decode(item)
				if derr != nil {
					continue
				}
				if m, isObj := v.(map[string]any); isObj {
					rows = append(rows, recentEntry{raw: item, m: m})
				}
			}
			for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
				rows[i], rows[j] = rows[j], rows[i]
			}
			return rows
		}
	}
	return b.localRecent(workspaceID, limit)
}

func (b *Bus) localRecent(workspaceID string, limit int) []recentEntry {
	b.mu.Lock()
	defer b.mu.Unlock()
	aligned := len(b.localEnc) == len(b.local) // tests may append to b.local directly
	var out []recentEntry
	for i, ev := range b.local {
		if ws, _ := ev["workspace_id"].(string); ws == workspaceID {
			e := recentEntry{m: ev}
			if aligned {
				e.raw = b.localEnc[i]
			} else {
				e.raw = string(dumps(ev))
			}
			out = append(out, e)
		}
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	if out == nil {
		out = []recentEntry{}
	}
	return out
}

// decode is json.loads: numbers stay json.Number, NaN/Infinity are not
// accepted by encoding/json (Python accepts them) -> treated as invalid.
func decode(s string) (any, error) {
	dec := json.NewDecoder(bytes.NewReader([]byte(s)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, io.ErrUnexpectedEOF
	}
	return v, nil
}
