//go:build integration

package enrichcache_test

import (
	"bytes"
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
		return enrichcache.PutEmailAt(ctx, tx, a, "x@y.co", "smtp", json.RawMessage(`{"status":"valid"}`), time.Hour, base)
	})
	in(t, pool, b, func(tx pgx.Tx) error {
		if _, ok, err := enrichcache.Get(ctx, tx, b, "k", "email", base); err != nil || ok {
			t.Errorf("B sees A entry: ok=%v err=%v", ok, err)
		}
		if _, ok, err := enrichcache.GetEmail(ctx, tx, b, "x@y.co", "smtp", base); err != nil || ok {
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
		if err := enrichcache.PutEmailAt(ctx, tx, ws, " A@B.co ", "reacher", res, enrichcache.EmailTTL("valid"), base); err != nil {
			return err
		}
		got, ok, err := enrichcache.GetEmail(ctx, tx, ws, "A@B.co", "reacher", base.Add(time.Hour))
		if err != nil || !ok {
			t.Fatalf("hit: %v %v", ok, err)
		}
		var m map[string]any
		if err := json.Unmarshal(got, &m); err != nil || m["status"] != "valid" || m["confidence"] != 0.95 {
			t.Errorf("result: %s %v", got, err)
		}
		if _, ok, _ := enrichcache.GetEmail(ctx, tx, ws, "a@b.co", "reacher", base.Add(14*24*time.Hour+time.Second)); ok {
			t.Error("expired email row returned")
		}
		var h string
		if err := tx.QueryRow(ctx, `SELECT email_hash FROM email_verify_cache`).Scan(&h); err != nil || h != enrichcache.EmailKey("a@b.co", "reacher") {
			t.Errorf("stored hash %q err=%v", h, err)
		}
		// The verifier is part of the key: same address, other verifier misses.
		if _, ok, _ := enrichcache.GetEmail(ctx, tx, ws, "a@b.co", "smtp", base); ok {
			t.Error("different verifier hit")
		}
		if err := enrichcache.PutEmailAt(ctx, tx, ws, "a@b.co", "smtp", json.RawMessage(`{"status":"unknown"}`), enrichcache.EmailTTL("unknown"), base); err != nil {
			return err
		}
		if _, ok, _ := enrichcache.GetEmail(ctx, tx, ws, "a@b.co", "smtp", base.Add(23*time.Hour)); !ok {
			t.Error("unknown within 1d missed")
		}
		if _, ok, _ := enrichcache.GetEmail(ctx, tx, ws, "a@b.co", "smtp", base.Add(25*time.Hour)); ok {
			t.Error("unknown after 1d hit")
		}
		// Python skips empty email or verifier on both get and set.
		if err := enrichcache.PutEmailAt(ctx, tx, ws, "a@b.co", "", json.RawMessage(`{"status":"valid"}`), time.Hour, base); err != nil {
			return err
		}
		if _, ok, _ := enrichcache.GetEmail(ctx, tx, ws, "a@b.co", "", base); ok {
			t.Error("empty verifier stored")
		}
		return nil
	})
}

type seqOp map[string]json.RawMessage

func (o seqOp) str(k string) string {
	var s string
	_ = json.Unmarshal(o[k], &s)
	return s
}

func (o seqOp) num(k string) (float64, bool) {
	var f *float64
	if err := json.Unmarshal(o[k], &f); err != nil || f == nil {
		return 0, false
	}
	return *f, true
}

// pyText mirrors Python str() for the scalar values the vectors use; ok=false
// for empty values (not stored) and non-scalars (caller concern, skipped).
func pyText(raw json.RawMessage) (s string, stored, scalar bool) {
	var v any
	dec := json.NewDecoder(bytesReader(raw))
	dec.UseNumber()
	_ = dec.Decode(&v)
	switch x := v.(type) {
	case nil:
		return "", false, true
	case string:
		return x, x != "", true
	case json.Number:
		return x.String(), true, true
	case bool:
		if x {
			return "True", true, true
		}
		return "False", true, true
	case []any:
		return "", len(x) > 0, len(x) == 0
	}
	return "", true, false
}

func TestCacheSequencesMatchPythonVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/key_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Seqs []struct {
			Name  string `json:"name"`
			Steps []struct {
				Op     seqOp           `json:"op"`
				Result json.RawMessage `json:"result"`
			} `json:"steps"`
			RowsLeft int `json:"rows_left"`
		} `json:"cache_sequences"`
		ESeqs []struct {
			Name  string `json:"name"`
			Steps []struct {
				Op     seqOp           `json:"op"`
				Result json.RawMessage `json:"result"`
			} `json:"steps"`
			RowsLeft int `json:"rows_left"`
		} `json:"email_cache_sequences"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	ws := fmt.Sprintf("t11seq_%d", os.Getpid())
	pool := newPool(t, ws)
	ctx := context.Background()
	// The recorder's clock is sticky: an "at" on any op moves it for that op and
	// every later one (sets use the current clock too).
	var clock time.Time
	at := func(o seqOp) time.Time {
		if s, ok := o.num("at"); ok {
			clock = base.Add(time.Duration(s * float64(time.Second)))
		}
		return clock
	}
	for _, s := range v.Seqs {
		t.Run(s.Name, func(t *testing.T) {
			clock = base
			in(t, pool, ws, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `DELETE FROM enrichment_cache`); err != nil {
					return err
				}
				skipped := map[string]bool{}
				for i, st := range s.Steps {
					o := st.Op
					key, field := o.str("key"), o.str("field")
					switch o.str("op") {
					case "set":
						text, stored, scalar := pyText(o["value"])
						if !scalar {
							skipped[key+"/"+field] = true
							continue
						}
						if !stored {
							text = ""
						}
						ttl := enrichcache.TTLFor(field)
						if d, ok := o.num("ttl_days"); ok {
							ttl = time.Duration(d) * 24 * time.Hour
						}
						conf, _ := o.num("confidence")
						if err := enrichcache.PutAt(ctx, tx, ws, key, field, enrichcache.Hit{Value: text, Confidence: conf, Provider: o.str("provider")}, ttl, at(o)); err != nil {
							return err
						}
					case "get":
						if skipped[key+"/"+field] {
							continue
						}
						h, ok, err := enrichcache.Get(ctx, tx, ws, key, field, at(o))
						if err != nil {
							return err
						}
						var want *struct {
							Value      string  `json:"value"`
							Confidence float64 `json:"confidence"`
							Provider   string  `json:"provider"`
						}
						_ = json.Unmarshal(st.Result, &want)
						if (want != nil) != ok || (ok && (h.Value != want.Value || h.Confidence != want.Confidence || h.Provider != want.Provider)) {
							t.Errorf("step %d get %s/%s: got %+v ok=%v want %s", i, key, field, h, ok, st.Result)
						}
					case "purge":
						n, err := enrichcache.Purge(ctx, tx, ws, at(o))
						var want int64
						_ = json.Unmarshal(st.Result, &want)
						if err != nil || n != want {
							t.Errorf("step %d purge: %d err=%v want %d", i, n, err, want)
						}
					}
				}
				if len(skipped) == 0 {
					var n int
					if err := tx.QueryRow(ctx, `SELECT count(*) FROM enrichment_cache`).Scan(&n); err != nil || n != s.RowsLeft {
						t.Errorf("rows_left=%d want %d err=%v", n, s.RowsLeft, err)
					}
				}
				return nil
			})
		})
	}
	for _, s := range v.ESeqs {
		t.Run("email_"+s.Name, func(t *testing.T) {
			clock = base
			in(t, pool, ws, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `DELETE FROM email_verify_cache`); err != nil {
					return err
				}
				for i, st := range s.Steps {
					o := st.Op
					email, verifier := o.str("email"), o.str("verifier")
					switch o.str("op") {
					case "set":
						status := o.str("status")
						if status == "" {
							continue
						}
						ttl := enrichcache.EmailTTL(status)
						if d, ok := o.num("ttl_days"); ok {
							ttl = time.Duration(d) * 24 * time.Hour
						}
						conf, _ := o.num("confidence")
						res, _ := json.Marshal(map[string]any{"status": status, "source": o.str("source"), "detail": o.str("detail"), "confidence": conf})
						if err := enrichcache.PutEmailAt(ctx, tx, ws, email, verifier, res, ttl, at(o)); err != nil {
							return err
						}
					case "get":
						var got json.RawMessage
						var ok bool
						var err error
						got, ok, err = enrichcache.GetEmail(ctx, tx, ws, email, verifier, at(o))
						if err != nil {
							return err
						}
						var want map[string]any
						_ = json.Unmarshal(st.Result, &want)
						if (want != nil) != ok {
							t.Errorf("step %d: ok=%v want %s", i, ok, st.Result)
							continue
						}
						if ok {
							var g map[string]any
							_ = json.Unmarshal(got, &g)
							if g["status"] != want["status"] || g["source"] != want["source"] || g["detail"] != want["detail"] || g["confidence"] != want["confidence"] {
								t.Errorf("step %d: got %s want %s", i, got, st.Result)
							}
						}
					case "purge":
						n, err := enrichcache.PurgeEmail(ctx, tx, ws, at(o))
						var want int64
						_ = json.Unmarshal(st.Result, &want)
						if err != nil || n != want {
							t.Errorf("step %d purge: %d err=%v want %d", i, n, err, want)
						}
					}
				}
				var n int
				if err := tx.QueryRow(ctx, `SELECT count(*) FROM email_verify_cache`).Scan(&n); err != nil || n != s.RowsLeft {
					t.Errorf("rows_left=%d want %d err=%v", n, s.RowsLeft, err)
				}
				return nil
			})
		})
	}
}

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }
