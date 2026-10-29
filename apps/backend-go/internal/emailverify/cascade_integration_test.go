//go:build integration

package emailverify

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

func pgDeps(t *testing.T, wss ...string) (Deps, *pgxpool.Pool) {
	t.Helper()
	pool := testpg.NewAppPool(t, 2)
	owner, err := pgxpool.New(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	clean := func() {
		_, _ = owner.Exec(context.Background(), `DELETE FROM email_verify_cache WHERE workspace_id = ANY($1)`, wss)
	}
	clean()
	t.Cleanup(func() { clean(); owner.Close() })
	d := NewDeps()
	d.Cache = NewPGCache(pool, nil)
	d.Registry = NewRegistry()
	return d, pool
}

func withReacher(d Deps, v Verifier) Deps {
	d.Reacher = func(context.Context, string) Verifier { return v }
	return d
}

func TestCascadeUsesCache(t *testing.T) {
	ws := fmt.Sprintf("t15cache_%d", os.Getpid())
	d, _ := pgDeps(t, ws)
	r := &scripted{name: "reacher", status: "valid", source: "reacher", conf: 0.95, detail: "rcpt_accepted", avail: true}
	d = withReacher(d, r)
	ctx := context.Background()
	first, err := Cascade(ctx, d, ws, "Someone@Acme.test")
	if err != nil || first.Status != "valid" || r.calls != 1 {
		t.Fatalf("first: %+v err=%v calls=%d", first, err, r.calls)
	}
	second, err := Cascade(ctx, d, ws, "someone@acme.test")
	if err != nil {
		t.Fatal(err)
	}
	if r.calls != 1 {
		t.Fatalf("second call hit the verifier again (calls=%d)", r.calls)
	}
	if second.Status != "valid" || second.Source != "reacher" || second.Confidence != 0.95 || second.Detail != "rcpt_accepted" {
		t.Fatalf("cached result: %+v", second)
	}
	if len(second.Attempts) != 1 || second.Attempts[0].Detail != "cache_hit" {
		t.Fatalf("attempts: %+v", second.Attempts)
	}
}

func TestCascadeCacheIsWorkspaceScoped(t *testing.T) {
	wsA, wsB := fmt.Sprintf("t15a_%d", os.Getpid()), fmt.Sprintf("t15b_%d", os.Getpid())
	d, _ := pgDeps(t, wsA, wsB)
	r := &scripted{name: "reacher", status: "valid", source: "reacher", conf: 0.95, detail: "ok", avail: true}
	d = withReacher(d, r)
	ctx := context.Background()
	if _, err := Cascade(ctx, d, wsA, "a@acme.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := Cascade(ctx, d, wsB, "a@acme.test"); err != nil {
		t.Fatal(err)
	}
	if r.calls != 2 {
		t.Fatalf("workspace B was served workspace A's cache entry (verifier calls=%d, want 2)", r.calls)
	}
	// each workspace now hits its own entry
	if _, err := Cascade(ctx, d, wsA, "a@acme.test"); err != nil || r.calls != 2 {
		t.Fatalf("A second call: err=%v calls=%d", err, r.calls)
	}
}

func TestPGCacheTTL(t *testing.T) {
	ws := fmt.Sprintf("t15ttl_%d", os.Getpid())
	d, pool := pgDeps(t, ws)
	base := time.Unix(1800000000, 0).UTC()
	now := base
	c := NewPGCache(pool, func() time.Time { return now })
	d.Cache = c
	ctx := context.Background()
	if err := c.Put(ctx, ws, "a@b.co", "reacher", CacheEntry{Status: "unknown", Source: "reacher", Detail: "soft"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := c.Get(ctx, ws, "a@b.co", "reacher"); !ok {
		t.Fatal("fresh unknown entry must hit")
	}
	now = base.Add(25 * time.Hour)
	if _, ok, _ := c.Get(ctx, ws, "a@b.co", "reacher"); ok {
		t.Fatal("unknown entries expire after one day")
	}
	if err := c.Put(ctx, ws, "a@b.co", "reacher", CacheEntry{Status: "valid", Source: "reacher", Detail: "ok", Confidence: 0.95}); err != nil {
		t.Fatal(err)
	}
	now = base.Add(25*time.Hour + 13*24*time.Hour)
	e, ok, _ := c.Get(ctx, ws, "a@b.co", "reacher")
	if !ok || e.Status != "valid" || e.Confidence != 0.95 {
		t.Fatalf("definitive entry lives 14 days: %+v ok=%v", e, ok)
	}
}
