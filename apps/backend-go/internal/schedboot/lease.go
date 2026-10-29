package schedboot

import (
	"context"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

// LeaseKey is the session-level advisory lock key of the one active scheduler (Go or Python): "OGTM".
const LeaseKey int64 = 0x4f47544d

// Lease is the scheduler leader lock. It keeps one connection out of the pool for as long as it holds
// the lock; if that connection dies Postgres drops the lock, Hold notices on the next pass and competes
// for it again.
type Lease struct {
	pool *pgxpool.Pool
	mu   sync.Mutex
	conn *pgxpool.Conn
}

// NewLease returns an unheld lease on pool.
func NewLease(pool *pgxpool.Pool) *Lease { return &Lease{pool: pool} }

// Hold reports whether this instance is the active scheduler, acquiring the lock when it is free. A held
// lease is verified with a round trip first; a broken connection forfeits it.
func (l *Lease) Hold(ctx context.Context) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn != nil {
		var held bool
		err := l.conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype = 'advisory' AND pid = pg_backend_pid()
		  AND granted AND classid = ($1::bigint >> 32)::oid AND objid = ($1::bigint & 4294967295)::oid AND objsubid = 1)`, LeaseKey).Scan(&held)
		if err == nil && held {
			return true, nil
		}
		l.drop() // connection or lock gone: forfeit, then compete again below
	}
	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return false, err
	}
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, LeaseKey).Scan(&got); err != nil {
		conn.Release()
		return false, err
	}
	if !got {
		conn.Release()
		return false, nil
	}
	l.conn = conn
	return true, nil
}

// Release gives the lock up (shutdown); safe to call when not held.
func (l *Lease) Release(ctx context.Context) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn == nil {
		return
	}
	_, _ = l.conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, LeaseKey)
	l.drop()
}

// drop returns the connection to the pool. A connection that may still own the lock is closed rather than
// reused, which releases any session lock with it.
func (l *Lease) drop() {
	if l.conn == nil {
		return
	}
	_ = l.conn.Conn().Close(context.Background())
	l.conn.Release()
	l.conn = nil
}
