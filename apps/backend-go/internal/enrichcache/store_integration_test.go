//go:build integration

package enrichcache_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/enrichcache"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

var base = time.Unix(1800000000, 0).UTC()

func newPool(t *testing.T, ws ...string) *pgxpool.Pool {
	t.Helper()
	pool := testpg.NewAppPool(t, 1)
	owner, err := pgxpool.New(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	clean := func() {
		for _, tbl := range []string{"enrichment_cache", "email_verify_cache"} {
			_, _ = owner.Exec(context.Background(), `DELETE FROM `+tbl+` WHERE workspace_id = ANY($1)`, ws)
		}
	}
	clean()
	t.Cleanup(func() { clean(); owner.Close() })
	return pool
}

func in(t *testing.T, pool *pgxpool.Pool, ws string, fn func(tx pgx.Tx) error) {
	t.Helper()
	if err := tenancy.InWorkspace(context.Background(), pool, ws, fn); err != nil {
		t.Fatal(err)
	}
}

func TestCacheTTLExpiry(t *testing.T) {
	ws := fmt.Sprintf("t11ttl_%d", os.Getpid())
	pool := newPool(t, ws)
	ctx := context.Background()
	in(t, pool, ws, func(tx pgx.Tx) error {
		h := enrichcache.Hit{Value: "a@b.co", Confidence: 0.9, Provider: "hunter_io"}
		if err := enrichcache.PutAt(ctx, tx, ws, "k1", "email", h, enrichcache.TTLFor("email"), base); err != nil {
			return err
		}
		for _, c := range []struct {
			at  time.Duration
			hit bool
		}{{time.Second, true}, {30*24*time.Hour - time.Second, true}, {30 * 24 * time.Hour, true}, {30*24*time.Hour + time.Second, false}} {
			got, ok, err := enrichcache.Get(ctx, tx, ws, "k1", "email", base.Add(c.at))
			if err != nil {
				return err
			}
			if ok != c.hit || (ok && got != h) {
				t.Errorf("at +%v: ok=%v got=%+v", c.at, ok, got)
			}
		}
		// Provider default and empty-value no-op, like Python.
		if err := enrichcache.PutAt(ctx, tx, ws, "k2", "phone", enrichcache.Hit{Value: "v"}, time.Hour, base); err != nil {
			return err
		}
		got, ok, _ := enrichcache.Get(ctx, tx, ws, "k2", "phone", base)
		if !ok || got.Provider != "cache" || got.Confidence != 0 {
			t.Errorf("defaults: %+v %v", got, ok)
		}
		_ = enrichcache.PutAt(ctx, tx, ws, "k3", "phone", enrichcache.Hit{Value: ""}, time.Hour, base)
		_ = enrichcache.PutAt(ctx, tx, ws, "", "phone", enrichcache.Hit{Value: "v"}, time.Hour, base)
		if _, ok, _ := enrichcache.Get(ctx, tx, ws, "k3", "phone", base); ok {
			t.Error("empty value stored")
		}
		if _, ok, _ := enrichcache.Get(ctx, tx, ws, "", "phone", base); ok {
			t.Error("empty key hit")
		}
		return nil
	})
}

func TestCacheWorkspaceIsolation(t *testing.T) {
	a, b := fmt.Sprintf("t11a_%d", os.Getpid()), fmt.Sprintf("t11b_%d", os.Getpid())
	pool := newPool(t, a, b) // pool of 1: the same connection serves both tenants in turn
	ctx := context.Background()
	in(t, pool, a, func(tx pgx.Tx) error {
		if err := enrichcache.PutAt(ctx, tx, a, "k", "email", enrichcache.Hit{Value: "a-secret"}, time.Hour, base); err != nil {
			return err
		}
		return enrichcache.PutEmailAt(ctx, tx, a, "x@y.co", json.RawMessage(`{"status":"valid"}`), time.Hour, base)
	})
	in(t, pool, b, func(tx pgx.Tx) error {
		if _, ok, err := enrichcache.Get(ctx, tx, b, "k", "email", base); err != nil || ok {
			t.Errorf("B sees A entry: ok=%v err=%v", ok, err)
		}
		if _, ok, err := enrichcache.GetEmail(ctx, tx, b, "x@y.co", base); err != nil || ok {
			t.Errorf("B sees A email entry: ok=%v err=%v", ok, err)
		}
		// B naming A's workspace id is blocked by RLS (read: nothing, write: error).
		if _, ok, _ := enrichcache.Get(ctx, tx, a, "k", "email", base); ok {
			t.Error("B read A rows by naming A")
		}
		return nil
	})
	err := tenancy.InWorkspace(ctx, pool, b, func(tx pgx.Tx) error {
		return enrichcache.PutAt(ctx, tx, a, "k", "email", enrichcache.Hit{Value: "forged"}, time.Hour, base)
	})
	if err == nil {
		t.Error("cross-workspace write succeeded")
	}
	in(t, pool, a, func(tx pgx.Tx) error {
		got, ok, err := enrichcache.Get(ctx, tx, a, "k", "email", base)
		if err != nil || !ok || got.Value != "a-secret" {
			t.Errorf("A lost its entry: %+v %v %v", got, ok, err)
		}
		return nil
	})
}

func TestCachePutOverwrites(t *testing.T) {
	ws := fmt.Sprintf("t11ow_%d", os.Getpid())
	pool := newPool(t, ws)
	ctx := context.Background()
	in(t, pool, ws, func(tx pgx.Tx) error {
		old := enrichcache.Hit{Value: "old", Confidence: 0.1, Provider: "a"}
		if err := enrichcache.PutAt(ctx, tx, ws, "k", "email", old, time.Hour, base); err != nil {
			return err
		}
		// Expired row is invisible, then overwritten by a fresh Put (TTL restarts).
		if _, ok, _ := enrichcache.Get(ctx, tx, ws, "k", "email", base.Add(2*time.Hour)); ok {
			t.Error("expired row returned")
		}
		nw := enrichcache.Hit{Value: "new", Confidence: 0.9, Provider: "b"}
		later := base.Add(2 * time.Hour)
		if err := enrichcache.PutAt(ctx, tx, ws, "k", "email", nw, 48*time.Hour, later); err != nil {
			return err
		}
		got, ok, _ := enrichcache.Get(ctx, tx, ws, "k", "email", later.Add(24*time.Hour))
		if !ok || got != nw {
			t.Errorf("overwrite: %+v %v", got, ok)
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM enrichment_cache`).Scan(&n); err != nil || n != 1 {
			t.Errorf("rows=%d err=%v", n, err)
		}
		// Purge removes expired rows only.
		if c, err := enrichcache.Purge(ctx, tx, ws, later.Add(72*time.Hour)); err != nil || c != 1 {
			t.Errorf("purge=%d err=%v", c, err)
		}
		return nil
	})
}

func TestCacheValueRoundTripKeyOrder(t *testing.T) {
	ws := fmt.Sprintf("t11ko_%d", os.Getpid())
	pool := newPool(t, ws)
	ctx := context.Background()
	// jsonb would reorder keys of an object; values are stored as text, so the
	// exact bytes (key order, spacing, unicode) survive.
	vals := []string{`{"zeta":1,"alpha":{"b":2,"a":1},"mid":[3,1,2]}`, `{ "b" : 1,  "a" : 2 }`, "日本語 é 😀", `"quoted"`, "0"}
	in(t, pool, ws, func(tx pgx.Tx) error {
		for i, v := range vals {
			f := "f" + strconv.Itoa(i)
			if err := enrichcache.PutAt(ctx, tx, ws, "k", f, enrichcache.Hit{Value: v, Provider: "p"}, time.Hour, base); err != nil {
				return err
			}
			got, ok, err := enrichcache.Get(ctx, tx, ws, "k", f, base)
			if err != nil || !ok || got.Value != v {
				t.Errorf("round trip %q: got %q ok=%v err=%v", v, got.Value, ok, err)
			}
		}
		return nil
	})
}

func TestEmailCacheRoundTripAndExpiry(t *testing.T) {
	ws := fmt.Sprintf("t11em_%d", os.Getpid())
	pool := newPool(t, ws)
	ctx := context.Background()
	res := json.RawMessage(`{"status":"valid","source":"reacher","detail":"rcpt_accepted","confidence":0.95}`)
	in(t, pool, ws, func(tx pgx.Tx) error {
		if err := enrichcache.PutEmailAt(ctx, tx, ws, " A@B.co ", res, enrichcache.EmailTTL("valid"), base); err != nil {
			return err
		}
		got, ok, err := enrichcache.GetEmail(ctx, tx, ws, "a@b.co", base.Add(time.Hour))
		if err != nil || !ok {
			t.Fatalf("hit: %v %v", ok, err)
		}
		var m map[string]any
		if err := json.Unmarshal(got, &m); err != nil || m["status"] != "valid" || m["confidence"] != 0.95 {
			t.Errorf("result: %s %v", got, err)
		}
		if _, ok, _ := enrichcache.GetEmail(ctx, tx, ws, "a@b.co", base.Add(14*24*time.Hour+time.Second)); ok {
			t.Error("expired email row returned")
		}
		var h string
		if err := tx.QueryRow(ctx, `SELECT email_hash FROM email_verify_cache`).Scan(&h); err != nil || h != enrichcache.EmailHash("a@b.co") {
			t.Errorf("stored hash %q err=%v", h, err)
		}
		// Verifier-scoped key (Python email_key) variant.
		k := enrichcache.EmailKey("a@b.co", "smtp")
		if err := enrichcache.PutEmailKeyAt(ctx, tx, ws, k, json.RawMessage(`{"status":"unknown"}`), enrichcache.EmailTTL("unknown"), base); err != nil {
			return err
		}
		if _, ok, _ := enrichcache.GetEmailKey(ctx, tx, ws, k, base.Add(23*time.Hour)); !ok {
			t.Error("unknown within 1d missed")
		}
		if _, ok, _ := enrichcache.GetEmailKey(ctx, tx, ws, k, base.Add(25*time.Hour)); ok {
			t.Error("unknown after 1d hit")
		}
		return nil
	})
}
