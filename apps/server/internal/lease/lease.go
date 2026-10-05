// Package lease implements scheduler leadership on PostgreSQL, wire-compatible
// with the Python implementation in apps/api/services/scheduler_lease.py.
//
// One row of scheduler_leases per periodic scheduler ("scheduler:<name>"). A
// holder owns it until expires_at on the DATABASE clock, so host clock skew
// cannot create two leaders. The fencing token increases on every change of
// holder and whenever the same holder re-acquires after its lease lapsed; it is
// kept while the lease is live. Writes that must not come from a deposed
// leader are fenced: the transaction that writes also runs Fence, which takes
// FOR SHARE on the row and requires the holder AND token to match, so a
// takeover waits for in-flight fenced commits and a stale token matches
// nothing.
//
// A session-level pg_advisory_lock is deliberately not used: it needs a
// dedicated connection that transaction-pooling proxies break, disappears on a
// connection reset without telling the holder, and carries no token.
package lease

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Prefix namespaces scheduler leases in the shared table.
const Prefix = "scheduler:"

// ErrLost reports that the fenced transaction's lease is no longer held by
// this holder at the token the work started under.
var ErrLost = errors.New("lease: no longer the current holder")

const acquireSQL = `
INSERT INTO scheduler_leases AS l (name, holder, fencing_token, acquired_at, renewed_at, expires_at)
VALUES ($1, $2, 1, now(), now(), now() + make_interval(secs => $3::float8))
ON CONFLICT (name) DO UPDATE SET
    fencing_token = CASE WHEN l.holder = EXCLUDED.holder AND l.expires_at > now()
                         THEN l.fencing_token ELSE l.fencing_token + 1 END,
    acquired_at   = CASE WHEN l.holder = EXCLUDED.holder AND l.expires_at > now()
                         THEN l.acquired_at ELSE now() END,
    holder        = EXCLUDED.holder,
    renewed_at    = now(),
    expires_at    = EXCLUDED.expires_at
WHERE l.holder = EXCLUDED.holder OR l.expires_at <= now()
RETURNING fencing_token`

const releaseSQL = `
UPDATE scheduler_leases SET expires_at = now()
WHERE name = $1 AND holder = $2 AND fencing_token = $3`

const fenceSQL = `
SELECT 1 FROM scheduler_leases
WHERE name = $1 AND holder = $2 AND fencing_token = $3 AND expires_at > clock_timestamp()
FOR SHARE`

const snapshotSQL = `
SELECT name, holder, fencing_token, expires_at, expires_at > now()
FROM scheduler_leases ORDER BY name`

const forceReleaseSQL = `UPDATE scheduler_leases SET expires_at = now() WHERE name = $1`

// HolderID is unique per process start, so a restarted process is a new holder
// (and gets a new fencing token) even on the same host and pid.
func HolderID() string {
	host, _ := os.Hostname()
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s:%d:%s", host, os.Getpid(), hex.EncodeToString(b[:]))
}

// Lease is one named lease held, or not, by this process.
type Lease struct {
	Name   string
	Holder string
	TTL    time.Duration

	pool *pgxpool.Pool

	mu       sync.Mutex
	token    int64
	hasToken bool
	deadline time.Time // monotonic; conservative local expiry
}

// New creates an unheld lease. name may omit the "scheduler:" prefix.
func New(pool *pgxpool.Pool, name, holder string, ttl time.Duration) *Lease {
	if len(name) < len(Prefix) || name[:len(Prefix)] != Prefix {
		name = Prefix + name
	}
	if holder == "" {
		holder = HolderID()
	}
	return &Lease{Name: name, Holder: holder, TTL: ttl, pool: pool}
}

// Token returns the current fencing token, or false when not held.
func (l *Lease) Token() (int64, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.token, l.hasToken
}

// Held reports whether the last successful acquire or renewal is recent enough
// to trust locally. It is a hint; Fence is the guarantee.
func (l *Lease) Held() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.hasToken && time.Now().Before(l.deadline)
}

// Acquire acquires or renews. It returns true when this holder leads and false
// when another holder's lease is still live. Database errors are returned.
func (l *Lease) Acquire(ctx context.Context) (bool, error) {
	started := time.Now()
	var token int64
	err := l.pool.QueryRow(ctx, acquireSQL, l.Name, l.Holder, l.TTL.Seconds()).Scan(&token)
	l.mu.Lock()
	defer l.mu.Unlock()
	if errors.Is(err, pgx.ErrNoRows) {
		if l.hasToken {
			slog.Warn("lost lease", "lease", l.Name, "holder", l.Holder)
		}
		l.hasToken, l.deadline = false, time.Time{}
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lease: acquire %s: %w", l.Name, err)
	}
	switch {
	case !l.hasToken:
		slog.Info("acquired lease", "lease", l.Name, "token", token, "holder", l.Holder)
	case token != l.token:
		slog.Warn("re-acquired lease with a new token", "lease", l.Name, "token", token, "was", l.token)
	}
	l.token, l.hasToken = token, true
	// Trust it for strictly less than its remaining life: the request may have
	// been slow, and the database fence is the backstop.
	l.deadline = started.Add(l.TTL * 8 / 10)
	return true, nil
}

// Release hands the lease over now (graceful shutdown). Idempotent.
func (l *Lease) Release(ctx context.Context) error {
	l.mu.Lock()
	token, had := l.token, l.hasToken
	l.hasToken, l.deadline = false, time.Time{}
	l.mu.Unlock()
	if !had {
		return nil
	}
	if _, err := l.pool.Exec(ctx, releaseSQL, l.Name, l.Holder, token); err != nil {
		return fmt.Errorf("lease: release %s: %w", l.Name, err)
	}
	slog.Info("released lease", "lease", l.Name, "token", token)
	return nil
}

// Fence fails with ErrLost unless this holder still owns the lease at token.
// Call it inside the transaction that must be fenced, just before commit.
func (l *Lease) Fence(ctx context.Context, tx pgx.Tx, token int64) error {
	var one int
	err := tx.QueryRow(ctx, fenceSQL, l.Name, l.Holder, token).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s token %d", ErrLost, l.Name, token)
	}
	if err != nil {
		return fmt.Errorf("lease: fence %s: %w", l.Name, err)
	}
	return nil
}

// WithFence runs fn in a transaction and fences it against the token held when
// the call started. If the lease is lost, nothing fn wrote is committed.
func (l *Lease) WithFence(ctx context.Context, fn func(pgx.Tx) error) error {
	token, ok := l.Token()
	if !ok {
		return fmt.Errorf("%w: %s not held", ErrLost, l.Name)
	}
	return pgx.BeginFunc(ctx, l.pool, func(tx pgx.Tx) error {
		if err := fn(tx); err != nil {
			return err
		}
		return l.Fence(ctx, tx, token)
	})
}

// Run keeps the lease alive until ctx ends: it acquires or renews every
// `every` (default TTL/3) and calls onChange(held) when leadership changes.
// On exit it releases the lease so a peer leads at once.
func (l *Lease) Run(ctx context.Context, every time.Duration, onChange func(held bool)) {
	if every <= 0 {
		every = l.TTL / 3
	}
	last := false
	step := func() {
		held, err := l.Acquire(ctx)
		if err != nil {
			slog.Error("lease heartbeat failed", "lease", l.Name, "err", err)
			held = l.Held()
		}
		if held != last {
			last = held
			if onChange != nil {
				onChange(held)
			}
		}
	}
	step()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			rel, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			_ = l.Release(rel)
			cancel()
			if last && onChange != nil {
				onChange(false)
			}
			return
		case <-t.C:
			step()
		}
	}
}

// Row is one lease as stored.
type Row struct {
	Name      string
	Holder    string
	Token     int64
	ExpiresAt time.Time
	Live      bool
}

// Snapshot lists every lease (operators, diagnostics).
func Snapshot(ctx context.Context, pool *pgxpool.Pool) ([]Row, error) {
	rows, err := pool.Query(ctx, snapshotSQL)
	if err != nil {
		return nil, fmt.Errorf("lease: snapshot: %w", err)
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.Name, &r.Holder, &r.Token, &r.ExpiresAt, &r.Live); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ForceRelease expires a lease regardless of its holder, for an operator who
// knows the holder is gone and does not want to wait out the TTL. The token is
// kept, so the next acquirer still gets a higher one and the old holder, if it
// is somehow alive, is fenced out. It reports whether a live lease was expired.
func ForceRelease(ctx context.Context, pool *pgxpool.Pool, name string) (bool, error) {
	if len(name) < len(Prefix) || name[:len(Prefix)] != Prefix {
		name = Prefix + name
	}
	tag, err := pool.Exec(ctx, forceReleaseSQL, name)
	if err != nil {
		return false, fmt.Errorf("lease: force release %s: %w", name, err)
	}
	return tag.RowsAffected() > 0, nil
}
