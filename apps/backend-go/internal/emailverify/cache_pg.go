package emailverify

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/enrichcache"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// PGCache is the Postgres VerifyCache: email_verify_cache under FORCE RLS, one
// workspace transaction per operation (tenancy.InWorkspace), so a workspace can
// neither read nor overwrite another's entries. Definitive verdicts live 14
// days, anything else one day (enrichcache.EmailTTL).
type PGCache struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewPGCache returns the cache; now may be nil (time.Now).
func NewPGCache(pool *pgxpool.Pool, now func() time.Time) *PGCache {
	if now == nil {
		now = time.Now
	}
	return &PGCache{pool: pool, now: now}
}

// Get returns a fresh entry for (email, verifier) in workspaceID.
func (c *PGCache) Get(ctx context.Context, workspaceID, email, verifier string) (CacheEntry, bool, error) {
	var (
		raw json.RawMessage
		hit bool
	)
	err := tenancy.InWorkspace(ctx, c.pool, workspaceID, func(tx pgx.Tx) error {
		var err error
		raw, hit, err = enrichcache.GetEmail(ctx, tx, workspaceID, email, verifier, c.now())
		return err
	})
	if err != nil || !hit {
		return CacheEntry{}, false, err
	}
	var e CacheEntry
	if json.Unmarshal(raw, &e) != nil || e.Status == "" {
		return CacheEntry{}, false, nil // not a verdict this cache wrote: a miss
	}
	return e, true, nil
}

// Put stores or overwrites the verdict; an empty status is a no-op like Python.
func (c *PGCache) Put(ctx context.Context, workspaceID, email, verifier string, e CacheEntry) error {
	if e.Status == "" {
		return nil
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return tenancy.InWorkspace(ctx, c.pool, workspaceID, func(tx pgx.Tx) error {
		return enrichcache.PutEmailAt(ctx, tx, workspaceID, email, verifier, raw, enrichcache.EmailTTL(e.Status), c.now())
	})
}
