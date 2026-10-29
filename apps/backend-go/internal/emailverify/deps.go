package emailverify

import (
	"context"
	"crypto/rand"
	"net"
	"sync"
	"time"
)

const (
	// DefaultHeloHost and DefaultMailFrom are the SMTP identity (LEADGEN_SMTP_HELO,
	// LEADGEN_SMTP_MAIL_FROM defaults).
	DefaultHeloHost = "verify.yupcha.com"
	DefaultMailFrom = "verify@yupcha.com"
	// DefaultSMTPBudget bounds one whole probe conversation (dial to QUIT).
	DefaultSMTPBudget = 10 * time.Second
	// cacheTTL and cacheMax bound the per-process MX and catch-all memories.
	// Python keeps both forever and unbounded; an attacker-chosen domain stream
	// must not grow memory without limit, and a transient DNS failure must heal.
	cacheTTL = time.Hour
	cacheMax = 20000
)

// MXResolver is the DNS seam (a *net.Resolver satisfies it).
type MXResolver interface {
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
}

// Deps carries everything the package touches outside memory. Build it with
// NewDeps and override fields; the zero value is not usable. Deps is copied by
// value, the memory caches it points to are shared between copies.
type Deps struct {
	Resolver     MXResolver
	NewSession   func() SMTPSession
	SMTPPort     int  // 0 = 25
	SMTPDisabled bool // LEADGEN_SMTP_VERIFY=0: probing off, classification and MX still work
	HeloHost     string
	MailFrom     string
	SMTPBudget   time.Duration
	RandLocal    func() string
	Now          func() time.Time

	Registry *Registry
	// Reacher resolves the workspace's Reacher verifier (nil = off). It is
	// prepended to the chain per call and never registered globally, so one
	// tenant's configuration cannot leak into another's cascade.
	Reacher func(ctx context.Context, workspaceID string) Verifier
	Cache   VerifyCache
	// Finders is the email-finder waterfall chain, cheapest first.
	Finders []Finder
	// Provenance, when set, stamps the winning provider on the lead (the
	// PROVENANCE_TRACKING_ENABLED path of Python's _record_provenance).
	Provenance func(lead map[string]any, provider string)

	st      *memo
	cascade func(ctx context.Context, d Deps, workspaceID, email string) (Result, error) // test hook
}

// NewDeps returns Deps with defaults, a registry holding the SMTP verifier, the
// system resolver and an empty memory. NewSession is nil until the caller wires
// the egress-guarded dialer (see NewLive); until then every probe is "unreachable".
func NewDeps() Deps {
	d := Deps{Resolver: net.DefaultResolver, st: newMemo(), Registry: NewRegistry()}
	d.Registry.Register(SMTPVerifier(d), false)
	return d
}

// Timeout is the per-probe budget.
func (d Deps) Timeout() time.Duration {
	if d.SMTPBudget > 0 {
		return d.SMTPBudget
	}
	return DefaultSMTPBudget
}

func (d Deps) port() int {
	if d.SMTPPort > 0 {
		return d.SMTPPort
	}
	return 25
}

func (d Deps) helo() string {
	if d.HeloHost != "" {
		return d.HeloHost
	}
	return DefaultHeloHost
}

func (d Deps) mailFrom() string {
	if d.MailFrom != "" {
		return d.MailFrom
	}
	return DefaultMailFrom
}

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d Deps) randLocal() string {
	if d.RandLocal != nil {
		return d.RandLocal()
	}
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

func (d Deps) memo() *memo {
	if d.st != nil {
		return d.st
	}
	return newMemo() // no shared memory: each call starts cold
}

// memo is the process memory of resolved MX hosts and catch-all verdicts
// (_mx_cache, _catchall_cache), bounded and expiring.
type memo struct {
	mu       sync.Mutex
	mx       map[string]memoEntry[string]
	catchAll map[string]memoEntry[bool]
	sems     map[string]chan struct{}
}

type memoEntry[T any] struct {
	v   T
	exp time.Time
}

func newMemo() *memo {
	return &memo{mx: map[string]memoEntry[string]{}, catchAll: map[string]memoEntry[bool]{}, sems: map[string]chan struct{}{}}
}

func memoGet[T any](m map[string]memoEntry[T], k string, now time.Time) (T, bool) {
	e, ok := m[k]
	if !ok || !now.Before(e.exp) {
		var zero T
		return zero, false
	}
	return e.v, true
}

func memoPut[T any](m map[string]memoEntry[T], k string, v T, now time.Time) {
	if len(m) >= cacheMax {
		for key, e := range m { // drop expired first, then arbitrary entries
			if !now.Before(e.exp) {
				delete(m, key)
			}
		}
		for key := range m {
			if len(m) < cacheMax {
				break
			}
			delete(m, key)
		}
	}
	m[k] = memoEntry[T]{v: v, exp: now.Add(cacheTTL)}
}

// maxPerMX bounds concurrent SMTP sessions to one MX host, so N parallel
// cascades for one domain do not hammer the same server; later ones wait and
// then usually hit the catch-all memory.
const maxPerMX = 2

// acquire takes a slot for host (waiting for ctx) and returns its release.
func (m *memo) acquire(ctx context.Context, host string) (func(), error) {
	m.mu.Lock()
	ch, ok := m.sems[host]
	if !ok {
		if len(m.sems) >= cacheMax {
			m.sems = map[string]chan struct{}{} // idle semaphores are cheap to recreate
		}
		ch = make(chan struct{}, maxPerMX)
		m.sems[host] = ch
	}
	m.mu.Unlock()
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
