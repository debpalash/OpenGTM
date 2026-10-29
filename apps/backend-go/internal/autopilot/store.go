package autopilot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// PlanStore is autopilot_plan_store: a drafted plan kept server-side under (workspace, plan_id) with a
// single-use nonce, so the approval gate renders and executes exactly the stored plan, at most once.
// A user is "" for a self-host (no user) chat.
type PlanStore interface {
	// Put stores a plan (its JSON) and returns (plan_id, nonce).
	Put(ctx context.Context, ws, user string, plan []byte) (planID, nonce string, err error)
	// Peek returns the stored plan for the gate text (nil when unknown, expired or another workspace's).
	Peek(ctx context.Context, ws, planID string) ([]byte, error)
	// Consume atomically burns the nonce and returns the plan; nil when the id is unknown or expired, the
	// nonce, workspace or user differ, or it was already consumed.
	Consume(ctx context.Context, ws, user, planID, nonce string) ([]byte, error)
}

// DefaultPlanTTL is AUTOPILOT_PLAN_TTL_SEC's default.
const DefaultPlanTTL = time.Hour

// PlanTTL reads AUTOPILOT_PLAN_TTL_SEC (a bad value is the default; at least one second).
func PlanTTL(ctx context.Context, s Settings) time.Duration {
	n, err := strconv.ParseInt(strip(setting(ctx, s, "AUTOPILOT_PLAN_TTL_SEC", "3600")), 10, 64)
	if err != nil {
		return DefaultPlanTTL
	}
	return time.Duration(max(n, 1)) * time.Second
}

func newHex() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return hex.EncodeToString(b[:])
}

// ---- in-process store (Python's behaviour; one API process) ----

type memRow struct {
	ws, user, nonce string
	plan            []byte
	consumed        bool
	created         time.Time
}

// MemoryPlanStore keeps plans in this process (Python's store). Use RedisPlanStore when several API
// processes serve the chat: an approval may reach a different process than the draft.
type MemoryPlanStore struct {
	TTL   func(ctx context.Context) time.Duration // nil: DefaultPlanTTL
	NewID func() string                           // nil: uuid4().hex
	Now   func() time.Time                        // nil: time.Now

	mu   sync.Mutex
	rows map[string]*memRow
}

func (m *MemoryPlanStore) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *MemoryPlanStore) id() string {
	if m.NewID != nil {
		return m.NewID()
	}
	return newHex()
}

func (m *MemoryPlanStore) sweepLocked(ctx context.Context, now time.Time) {
	ttl := DefaultPlanTTL
	if m.TTL != nil {
		ttl = m.TTL(ctx)
	}
	for id, r := range m.rows {
		if now.Sub(r.created) > ttl {
			delete(m.rows, id)
		}
	}
}

func (m *MemoryPlanStore) Put(ctx context.Context, ws, user string, plan []byte) (string, string, error) {
	planID, nonce := m.id(), m.id()
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rows == nil {
		m.rows = map[string]*memRow{}
	}
	m.sweepLocked(ctx, now)
	m.rows[planID] = &memRow{ws: ws, user: user, nonce: nonce, plan: append([]byte(nil), plan...), created: now}
	return planID, nonce, nil
}

func (m *MemoryPlanStore) Peek(ctx context.Context, ws, planID string) ([]byte, error) {
	if planID == "" {
		return nil, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweepLocked(ctx, m.now())
	r := m.rows[planID]
	if r == nil || r.ws != ws {
		return nil, nil
	}
	return append([]byte(nil), r.plan...), nil
}

func (m *MemoryPlanStore) Consume(ctx context.Context, ws, user, planID, nonce string) ([]byte, error) {
	if planID == "" || nonce == "" {
		return nil, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweepLocked(ctx, m.now())
	r := m.rows[planID]
	if r == nil || r.consumed || r.nonce != nonce || r.ws != ws || r.user != user {
		return nil, nil
	}
	r.consumed = true
	return append([]byte(nil), r.plan...), nil
}

// ---- Redis store (shared by every API process) ----

// RedisPlanStore keeps each plan in one hash with the TTL as its expiry; Consume is one Lua script, so two
// concurrent approvals of the same plan cannot both get it.
type RedisPlanStore struct {
	R      *redis.Client
	TTL    func(ctx context.Context) time.Duration // nil: DefaultPlanTTL
	Prefix string                                  // key prefix; "" means "autopilot:plan:"
	NewID  func() string
}

func (s *RedisPlanStore) key(id string) string {
	p := s.Prefix
	if p == "" {
		p = "autopilot:plan:"
	}
	return p + id
}

func (s *RedisPlanStore) Put(ctx context.Context, ws, user string, plan []byte) (string, string, error) {
	id, nonce := newHex(), newHex()
	if s.NewID != nil {
		id, nonce = s.NewID(), s.NewID()
	}
	ttl := DefaultPlanTTL
	if s.TTL != nil {
		ttl = s.TTL(ctx)
	}
	k := s.key(id)
	_, err := s.R.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.HSet(ctx, k, "ws", ws, "user", user, "nonce", nonce, "consumed", "0", "plan", plan)
		p.Expire(ctx, k, ttl)
		return nil
	})
	if err != nil {
		return "", "", err
	}
	return id, nonce, nil
}

func (s *RedisPlanStore) Peek(ctx context.Context, ws, planID string) ([]byte, error) {
	if planID == "" {
		return nil, nil
	}
	v, err := s.R.HMGet(ctx, s.key(planID), "ws", "plan").Result()
	if err != nil {
		return nil, err
	}
	w, _ := v[0].(string)
	p, _ := v[1].(string)
	if v[0] == nil || v[1] == nil || w != ws {
		return nil, nil
	}
	return []byte(p), nil
}

var consumeScript = redis.NewScript(`
local v = redis.call('HMGET', KEYS[1], 'ws', 'user', 'nonce', 'consumed', 'plan')
if not v[5] then return false end
if v[4] ~= '0' then return false end
if v[3] ~= ARGV[3] then return false end
if v[1] ~= ARGV[1] then return false end
if v[2] ~= ARGV[2] then return false end
redis.call('HSET', KEYS[1], 'consumed', '1')
return v[5]
`)

func (s *RedisPlanStore) Consume(ctx context.Context, ws, user, planID, nonce string) ([]byte, error) {
	if planID == "" || nonce == "" {
		return nil, nil
	}
	v, err := consumeScript.Run(ctx, s.R, []string{s.key(planID)}, ws, user, nonce).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p, _ := v.(string)
	return []byte(p), nil
}
