package autopilot

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// MemoryRecord is one remembered autopilot run (autopilot_memory).
type MemoryRecord struct {
	Goal       string  `json:"goal"`
	WorkbookID *string `json:"workbook_id"`
	Outcome    string  `json:"outcome"`
	CreatedAt  float64 `json:"created_at"` // unix seconds
}

// Memory is autopilot_memory: a short, per-workspace record of recent runs that the LLM planner reads back
// (as untrusted text). Record is idempotent on (workspace, workbook id). Best effort: errors are never
// surfaced to the chat.
type Memory interface {
	Recent(ctx context.Context, ws string, limit int) ([]MemoryRecord, error)
	Record(ctx context.Context, ws, goal string, workbookID *string, outcome string) error
}

// MaxPerWorkspace is _MAX_PER_WS.
const MaxPerWorkspace = 50

func newest(rows []MemoryRecord, limit int) []MemoryRecord {
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].CreatedAt > rows[j].CreatedAt })
	return rows[:min(max(limit, 0), len(rows))]
}

// InProcessMemory is Python's in-process store.
type InProcessMemory struct {
	Now func() time.Time

	mu  sync.Mutex
	mem map[string][]MemoryRecord
}

func (m *InProcessMemory) now() float64 {
	if m.Now != nil {
		return float64(m.Now().UnixNano()) / 1e9
	}
	return float64(time.Now().UnixNano()) / 1e9
}

func (m *InProcessMemory) Recent(_ context.Context, ws string, limit int) ([]MemoryRecord, error) {
	if ws == "" {
		return nil, nil
	}
	m.mu.Lock()
	rows := append([]MemoryRecord(nil), m.mem[ws]...)
	m.mu.Unlock()
	return newest(rows, limit), nil
}

func (m *InProcessMemory) Record(_ context.Context, ws, goal string, workbookID *string, outcome string) error {
	if ws == "" {
		return nil
	}
	if outcome == "" {
		outcome = "ok"
	}
	row := MemoryRecord{Goal: goal, WorkbookID: workbookID, Outcome: outcome, CreatedAt: m.now()}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mem == nil {
		m.mem = map[string][]MemoryRecord{}
	}
	bucket := m.mem[ws]
	replaced := false
	if workbookID != nil && *workbookID != "" {
		for i, r := range bucket {
			if r.WorkbookID != nil && *r.WorkbookID == *workbookID {
				bucket[i], replaced = row, true
				break
			}
		}
	}
	if !replaced {
		bucket = append(bucket, row)
	}
	if len(bucket) > MaxPerWorkspace {
		sort.SliceStable(bucket, func(i, j int) bool { return bucket[i].CreatedAt < bucket[j].CreatedAt })
		bucket = append([]MemoryRecord(nil), bucket[len(bucket)-MaxPerWorkspace:]...)
	}
	m.mem[ws] = bucket
	return nil
}

// RedisMemory keeps one hash per workspace (field: the workbook id, or a fresh id for a run without one).
type RedisMemory struct {
	R      *redis.Client
	Prefix string        // "" means "autopilot:memory:"
	TTL    time.Duration // idle expiry of a workspace's memory; 0 means 30 days
	Now    func() time.Time
}

func (m *RedisMemory) key(ws string) string {
	p := m.Prefix
	if p == "" {
		p = "autopilot:memory:"
	}
	return p + ws
}

func (m *RedisMemory) all(ctx context.Context, ws string) (map[string]MemoryRecord, error) {
	raw, err := m.R.HGetAll(ctx, m.key(ws)).Result()
	if err != nil {
		return nil, err
	}
	out := make(map[string]MemoryRecord, len(raw))
	for f, v := range raw {
		var r MemoryRecord
		if json.Unmarshal([]byte(v), &r) == nil {
			out[f] = r
		}
	}
	return out, nil
}

func (m *RedisMemory) Recent(ctx context.Context, ws string, limit int) ([]MemoryRecord, error) {
	if ws == "" {
		return nil, nil
	}
	all, err := m.all(ctx, ws)
	if err != nil {
		return nil, err
	}
	rows := make([]MemoryRecord, 0, len(all))
	for _, r := range all {
		rows = append(rows, r)
	}
	return newest(rows, limit), nil
}

func (m *RedisMemory) Record(ctx context.Context, ws, goal string, workbookID *string, outcome string) error {
	if ws == "" {
		return nil
	}
	if outcome == "" {
		outcome = "ok"
	}
	now := time.Now()
	if m.Now != nil {
		now = m.Now()
	}
	row := MemoryRecord{Goal: goal, WorkbookID: workbookID, Outcome: outcome, CreatedAt: float64(now.UnixNano()) / 1e9}
	field := "run:" + newHex()
	if workbookID != nil && *workbookID != "" {
		field = "wb:" + *workbookID
	}
	b, err := json.Marshal(row)
	if err != nil {
		return err
	}
	ttl := m.TTL
	if ttl <= 0 {
		ttl = 30 * 24 * time.Hour
	}
	k := m.key(ws)
	if _, err := m.R.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.HSet(ctx, k, field, b)
		p.Expire(ctx, k, ttl)
		return nil
	}); err != nil {
		return err
	}
	all, err := m.all(ctx, ws)
	if err != nil || len(all) <= MaxPerWorkspace {
		return err
	}
	type fr struct {
		f string
		t float64
	}
	var rows []fr
	for f, r := range all {
		rows = append(rows, fr{f, r.CreatedAt})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].t < rows[j].t })
	var drop []string
	for _, r := range rows[:len(rows)-MaxPerWorkspace] {
		drop = append(drop, r.f)
	}
	return m.R.HDel(ctx, k, drop...).Err()
}
