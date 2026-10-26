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
	if limit < 0 {
		limit = 0
	}
	if limit > recentCap {
		limit = recentCap
	}
	if limit == 0 || workspaceID == "" {
		return []map[string]any{}, nil
	}
	if b.redisUsable() {
		raw, err := b.rdb.LRange(ctx, RecentKey(workspaceID), 0, int64(limit-1)).Result()
		if err != nil {
			b.forgetRedis()
		} else if len(raw) > 0 {
			rows := make([]map[string]any, 0, len(raw))
			ok := true
			for _, item := range raw {
				v, derr := decode(item)
				if derr != nil {
					ok = false
					break
				}
				if m, isObj := v.(map[string]any); isObj {
					rows = append(rows, m)
				}
			}
			if ok {
				for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
					rows[i], rows[j] = rows[j], rows[i]
				}
				return rows, nil
			}
			b.forgetRedis()
		}
	}
	return b.localRecent(workspaceID, limit), nil
}

func (b *Bus) localRecent(workspaceID string, limit int) []map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for _, ev := range b.local {
		if ws, _ := ev["workspace_id"].(string); ws == workspaceID {
			out = append(out, ev)
		}
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	if out == nil {
		out = []map[string]any{}
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
