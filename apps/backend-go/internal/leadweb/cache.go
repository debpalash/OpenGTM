package leadweb

import (
	"container/list"
	"crypto/sha1" //nolint:gosec // cache key digest, as legacy (not a security boundary)
	"encoding/hex"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/text/cases"
)

// Hit is one search result in the ddgs shape every legacy caller consumed:
// {"title","href","body"} (news adds date and source).
type Hit struct {
	Title  string `json:"title"`
	Href   string `json:"href"`
	Body   string `json:"body"`
	Date   string `json:"date,omitempty"`
	Source string `json:"source,omitempty"`
}

// ── search result cache (leadgen/search_cache.py _SearchCache) ──

// Cache is a process-local LRU with a TTL per entry (empty results get the
// shorter empty TTL). Safe for concurrent use. Like legacy it is per process:
// each api / worker replica has its own (plan 15 R6).
type Cache struct {
	mu       sync.Mutex
	ttl      time.Duration
	emptyTTL time.Duration
	max      int
	ll       *list.List
	items    map[string]*list.Element
	now      func() time.Time
}

type cacheEntry struct {
	key     string
	expires time.Time
	hits    []Hit
}

// NewCache returns an empty cache holding at most max entries.
func NewCache(ttl, emptyTTL time.Duration, maxEntries int) *Cache {
	return &Cache{ttl: ttl, emptyTTL: emptyTTL, max: max(1, maxEntries), ll: list.New(), items: map[string]*list.Element{},
		now: time.Now}
}

// CacheKey is legacy make_key: sha1 of "salt|method|normalised query|max|backends"
// where the query is stripped, case-folded and whitespace-collapsed. salt is
// the workspace id ("" = shared entry); maxResults < 0 renders as Python None.
func CacheKey(salt, method, query string, maxResults int, backends string) string {
	norm := strings.Join(strings.FieldsFunc(cases.Fold().String(pyStrip(query)), pyIsSpace), " ")
	n := "None"
	if maxResults >= 0 {
		n = strconv.Itoa(maxResults)
	}
	sum := sha1.Sum([]byte(salt + "|" + method + "|" + norm + "|" + n + "|" + backends)) //nolint:gosec
	return hex.EncodeToString(sum[:])
}

// Get returns a copy of a live entry and marks it recently used.
func (c *Cache) Get(key string) ([]Hit, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return nil, false
	}
	e := el.Value.(*cacheEntry)
	if c.now().After(e.expires) {
		c.ll.Remove(el)
		delete(c.items, key)
		return nil, false
	}
	c.ll.MoveToFront(el)
	return slices.Clone(e.hits), true
}

// Put stores hits (empty selects the empty TTL) and evicts the least
// recently used entries above the cap.
func (c *Cache) Put(key string, hits []Hit, empty bool) {
	ttl := c.ttl
	if empty {
		ttl = c.emptyTTL
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e := &cacheEntry{key: key, expires: c.now().Add(ttl), hits: slices.Clone(hits)}
	if el, ok := c.items[key]; ok {
		el.Value = e
		c.ll.MoveToFront(el)
	} else {
		c.items[key] = c.ll.PushFront(e)
	}
	for c.ll.Len() > c.max {
		old := c.ll.Back()
		c.ll.Remove(old)
		delete(c.items, old.Value.(*cacheEntry).key)
	}
}

// Len is the number of stored entries (expired ones included until read).
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

// ── per-host adaptive backoff (search_cache.py _HostBackoff) ──

// rlPatterns are legacy _RL_PATTERNS: rate-limit / block signals.
var rlPatterns = []string{"ratelimit", "rate limit", "429", "403", "202", "too many", "blocked", "captcha", "timeout",
	"timed out"}

// IsBlockSignal is legacy is_block_signal over "<ExceptionType> <message>".
func IsBlockSignal(text string) bool {
	return containsAny(cases.Fold().String(text), rlPatterns)
}

// Backoff is per-host exponential backoff with jitter, capped. A disabled
// Backoff (SEARCH_BACKOFF_ENABLED=false) never blocks and records nothing.
type Backoff struct {
	mu      sync.Mutex
	enabled bool
	base    time.Duration
	cap     time.Duration
	state   map[string]backoffState
	now     func() time.Time
	jitter  func(max float64) float64 // uniform in [0, max]
}

type backoffState struct {
	until       time.Time
	consecutive int
}

// NewBackoff returns a Backoff with the legacy delay formula.
func NewBackoff(enabled bool, base, cap time.Duration) *Backoff {
	return &Backoff{enabled: enabled, base: base, cap: cap, state: map[string]backoffState{}, now: time.Now,
		jitter: func(m float64) float64 { return rand.Float64() * m }} //nolint:gosec // jitter
}

// Blocked reports whether host is cooling down.
func (b *Backoff) Blocked(host string) bool {
	if !b.enabled {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	st, ok := b.state[host]
	return ok && b.now().Before(st.until)
}

// Record grows host's backoff when the failure was a block signal and
// reports whether it did.
func (b *Backoff) Record(host string, block bool) bool {
	if !b.enabled || !block {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.state[host]
	st.consecutive++
	exp := min(st.consecutive-1, 30)
	capS := b.cap.Seconds()
	baseDelay := math.Min(b.base.Seconds()*math.Pow(2, float64(exp)), capS)
	delay := math.Min(baseDelay+b.jitter(0.3*baseDelay), capS)
	st.until = b.now().Add(time.Duration(delay * float64(time.Second)))
	b.state[host] = st
	return true
}

// RecordSuccess clears host's backoff.
func (b *Backoff) RecordSuccess(host string) {
	if !b.enabled {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.state, host)
}

// Remaining is the time left in host's backoff.
func (b *Backoff) Remaining(host string) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	st, ok := b.state[host]
	if !ok {
		return 0
	}
	return max(0, st.until.Sub(b.now()))
}

// Consecutive is the current block streak for host.
func (b *Backoff) Consecutive(host string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state[host].consecutive
}

func (b *Backoff) String() string { return fmt.Sprintf("Backoff(hosts=%d)", len(b.state)) }
