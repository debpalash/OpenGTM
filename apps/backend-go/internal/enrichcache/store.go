package enrichcache

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
)

// Hit is a cached provider value. Value is opaque text (Python stores
// str(value)); it is kept as a jsonb string so the bytes, including the key
// order of any JSON text inside it, round-trip exactly.
type Hit struct {
	Value      string
	Confidence float64
	Provider   string
}

// naive converts to the UTC wall clock the timestamp-without-time-zone
// columns hold.
func naive(t time.Time) pgtype.Timestamp {
	return pgtype.Timestamp{Time: t.UTC(), Valid: true}
}

// Get returns a fresh entry (expires_at >= now). Expired rows are never
// returned. Run inside tenancy.InWorkspace for workspaceID.
func Get(ctx context.Context, tx pgx.Tx, workspaceID, key, field string, now time.Time) (Hit, bool, error) {
	if key == "" {
		return Hit{}, false, nil
	}
	row, err := gen.New(tx).EnrichCacheGet(ctx, gen.EnrichCacheGetParams{
		WorkspaceID: workspaceID, CacheKey: key, FieldName: field, ExpiresAt: naive(now),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Hit{}, false, nil
	}
	if err != nil {
		return Hit{}, false, err
	}
	var v string
	if err := json.Unmarshal(row.Value, &v); err != nil {
		return Hit{}, false, nil // not a value this cache wrote; treat as a miss
	}
	h := Hit{Value: v, Confidence: row.Confidence.Float64, Provider: row.Provider.String}
	if h.Provider == "" {
		h.Provider = "cache"
	}
	return h, true, nil
}

// Put stores or overwrites an entry (an expired row is replaced, the TTL
// restarts). An empty key or empty value is a no-op, like Python.
func Put(ctx context.Context, tx pgx.Tx, workspaceID, key, field string, h Hit, ttl time.Duration) error {
	return PutAt(ctx, tx, workspaceID, key, field, h, ttl, time.Now())
}

// PutAt is Put with an explicit clock.
func PutAt(ctx context.Context, tx pgx.Tx, workspaceID, key, field string, h Hit, ttl time.Duration, now time.Time) error {
	if key == "" || h.Value == "" {
		return nil
	}
	val, err := json.Marshal(h.Value)
	if err != nil {
		return err
	}
	return gen.New(tx).EnrichCachePut(ctx, gen.EnrichCachePutParams{
		WorkspaceID: workspaceID, CacheKey: key, FieldName: field, Value: val,
		Provider:   pgtype.Text{String: h.Provider, Valid: true},
		Confidence: pgtype.Float8{Float64: h.Confidence, Valid: true},
		CachedAt:   naive(now), ExpiresAt: naive(now.Add(ttl)),
	})
}

// Purge deletes this workspace's expired provider-cache rows.
func Purge(ctx context.Context, tx pgx.Tx, workspaceID string, now time.Time) (int64, error) {
	return gen.New(tx).EnrichCachePurge(ctx, gen.EnrichCachePurgeParams{WorkspaceID: workspaceID, ExpiresAt: naive(now)})
}

// GetEmail returns the cached verification result for an address (hash of the
// stripped, lower-cased email). Only fresh rows are returned.
func GetEmail(ctx context.Context, tx pgx.Tx, workspaceID, email string, now time.Time) (json.RawMessage, bool, error) {
	if email == "" {
		return nil, false, nil
	}
	return GetEmailKey(ctx, tx, workspaceID, EmailHash(email), now)
}

// PutEmail stores or overwrites the verification result for an address.
func PutEmail(ctx context.Context, tx pgx.Tx, workspaceID, email string, result json.RawMessage, ttl time.Duration) error {
	return PutEmailAt(ctx, tx, workspaceID, email, result, ttl, time.Now())
}

// PutEmailAt is PutEmail with an explicit clock.
func PutEmailAt(ctx context.Context, tx pgx.Tx, workspaceID, email string, result json.RawMessage, ttl time.Duration, now time.Time) error {
	if email == "" {
		return nil
	}
	return PutEmailKeyAt(ctx, tx, workspaceID, EmailHash(email), result, ttl, now)
}

// GetEmailKey reads by a full cache key, e.g. EmailKey(email, verifier).
func GetEmailKey(ctx context.Context, tx pgx.Tx, workspaceID, key string, now time.Time) (json.RawMessage, bool, error) {
	if key == "" {
		return nil, false, nil
	}
	res, err := gen.New(tx).EmailVerifyCacheGet(ctx, gen.EmailVerifyCacheGetParams{
		WorkspaceID: workspaceID, EmailHash: key, ExpiresAt: naive(now),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return json.RawMessage(res), true, nil
}

// PutEmailKey writes by a full cache key.
func PutEmailKey(ctx context.Context, tx pgx.Tx, workspaceID, key string, result json.RawMessage, ttl time.Duration) error {
	return PutEmailKeyAt(ctx, tx, workspaceID, key, result, ttl, time.Now())
}

// PutEmailKeyAt is PutEmailKey with an explicit clock. result must be valid JSON.
func PutEmailKeyAt(ctx context.Context, tx pgx.Tx, workspaceID, key string, result json.RawMessage, ttl time.Duration, now time.Time) error {
	if key == "" || len(result) == 0 {
		return nil
	}
	return gen.New(tx).EmailVerifyCachePut(ctx, gen.EmailVerifyCachePutParams{
		WorkspaceID: workspaceID, EmailHash: key, Result: result,
		CachedAt: naive(now), ExpiresAt: naive(now.Add(ttl)),
	})
}

// PurgeEmail deletes this workspace's expired email-verify rows.
func PurgeEmail(ctx context.Context, tx pgx.Tx, workspaceID string, now time.Time) (int64, error) {
	return gen.New(tx).EmailVerifyCachePurge(ctx, gen.EmailVerifyCachePurgeParams{WorkspaceID: workspaceID, ExpiresAt: naive(now)})
}
