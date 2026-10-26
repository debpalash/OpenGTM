//go:build integration

package chatmemory_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/chatmemory"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

func tx(t *testing.T, pool *pgxpool.Pool, fn func(pgx.Tx)) {
	t.Helper()
	ctx := context.Background()
	c, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Rollback(ctx) }()
	fn(c)
	if err := c.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func ns() string { return "ws-t9-" + uuid.NewString()[:8] + ":self-host" }

func mustAdd(t *testing.T, pool *pgxpool.Pool, n, text string, md map[string]any) {
	t.Helper()
	tx(t, pool, func(c pgx.Tx) {
		if err := chatmemory.Add(context.Background(), c, n, text, md); err != nil {
			t.Fatal(err)
		}
	})
}

func search(t *testing.T, pool *pgxpool.Pool, n, q string, limit int) []chatmemory.Memory {
	t.Helper()
	var out []chatmemory.Memory
	tx(t, pool, func(c pgx.Tx) {
		var err error
		out, err = chatmemory.Search(context.Background(), c, n, q, limit)
		if err != nil {
			t.Fatal(err)
		}
	})
	return out
}

type vecRow struct {
	Text     string         `json:"text"`
	Metadata map[string]any `json:"metadata"`
}

func TestSearchOrderingVectors(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	raw, err := os.ReadFile("testdata/memory_search.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Cases []struct {
			Name     string   `json:"name"`
			Rows     []vecRow `json:"rows"`
			Query    string   `json:"query"`
			Limit    int      `json:"limit"`
			Expected []struct {
				Memory   string         `json:"memory"`
				Metadata map[string]any `json:"metadata"`
			} `json:"expected"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			n, other := ns(), ns()
			// decoy in another namespace that would match every query
			mustAdd(t, pool, other, c.Query+" decoy "+c.Query, nil)
			tx(t, pool, func(tx pgx.Tx) {
				for _, r := range c.Rows {
					if err := chatmemory.Add(context.Background(), tx, n, r.Text, r.Metadata); err != nil {
						t.Fatal(err)
					}
				}
			})
			got := search(t, pool, n, c.Query, c.Limit)
			type out struct {
				Memory   string         `json:"memory"`
				Metadata map[string]any `json:"metadata"`
			}
			g := []out{}
			for _, m := range got {
				g = append(g, out{m.Memory, m.Metadata})
			}
			gb, _ := json.Marshal(g)
			wb, _ := json.Marshal(c.Expected)
			if string(gb) != string(wb) {
				t.Fatalf("got %s\nwant %s", gb, wb)
			}
		})
	}
}

func TestSearchCap500(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	n := ns()
	tx(t, pool, func(c pgx.Tx) {
		_ = chatmemory.Add(context.Background(), c, n, "ancient needle", nil)
		for i := 0; i < 500; i++ {
			_ = chatmemory.Add(context.Background(), c, n, fmt.Sprintf("filler row %d", i), nil)
		}
	})
	if got := search(t, pool, n, "needle", 5); len(got) != 0 {
		t.Fatalf("row beyond the 500 cap returned: %v", got)
	}
	if got := search(t, pool, n, "filler", 1000); len(got) != 500 {
		t.Fatalf("got %d, want 500", len(got))
	}
}

func TestNamespaceIsolation(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	a, b := "ws-t9-"+uuid.NewString()[:8], "ws-t9-"+uuid.NewString()[:8]
	editor, other := int32(11), int32(12)
	nAE, nBE, nAO := chatmemory.Namespace(a, &editor), chatmemory.Namespace(b, &editor), chatmemory.Namespace(a, &other)
	mustAdd(t, pool, nAE, "secret alpha roadmap", nil)
	mustAdd(t, pool, nBE, "secret beta roadmap", nil)
	mustAdd(t, pool, nAO, "secret gamma roadmap", nil)
	for _, c := range []struct{ ns, want string }{{nAE, "secret alpha roadmap"}, {nBE, "secret beta roadmap"}, {nAO, "secret gamma roadmap"}} {
		got := search(t, pool, c.ns, "secret roadmap", 10)
		if len(got) != 1 || got[0].Memory != c.want {
			t.Fatalf("ns %s leaked or missed: %v", c.ns, got)
		}
		tx(t, pool, func(tx pgx.Tx) {
			all, err := chatmemory.All(context.Background(), tx, c.ns)
			if err != nil || len(all) != 1 || all[0].Memory != c.want {
				t.Fatalf("All leaked: %v %v", all, err)
			}
		})
	}
	// self-host sentinel of A is distinct from every user of A
	if got := search(t, pool, chatmemory.Namespace(a, nil), "secret", 10); len(got) != 0 {
		t.Fatalf("self-host read user memories: %v", got)
	}
}

func TestCreatedAtFormat(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	n := ns()
	ist := time.FixedZone("IST", 5*3600+1800)
	whole := time.Date(2026, 3, 4, 10, 0, 0, 0, ist)
	frac := time.Date(2026, 3, 4, 10, 0, 0, 123456000, ist)
	tx(t, pool, func(c pgx.Tx) {
		q := gen.New(c)
		for i, ts := range []time.Time{whole, frac} {
			if err := q.MemAdd(context.Background(), gen.MemAddParams{UserID: n, Text: fmt.Sprintf("t%d", i), Metadata: []byte("{}"), CreatedAt: pgtype.Timestamptz{Time: ts, Valid: true}}); err != nil {
				t.Fatal(err)
			}
		}
		all, err := chatmemory.All(context.Background(), c, n)
		if err != nil || len(all) != 2 {
			t.Fatal(all, err)
		}
		if all[0].CreatedAt != "2026-03-04T04:30:00.123456+00:00" || all[1].CreatedAt != "2026-03-04T04:30:00+00:00" {
			t.Fatalf("got %q %q", all[0].CreatedAt, all[1].CreatedAt)
		}
		b, _ := json.Marshal(all[1])
		if string(b) != `{"memory":"t0","metadata":{},"created_at":"2026-03-04T04:30:00+00:00"}` {
			t.Fatal(string(b))
		}
	})
}

func TestAddThenAllNewestFirst(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	n := ns()
	mustAdd(t, pool, n, "first", map[string]any{"k": "v"})
	mustAdd(t, pool, n, "second", nil)
	tx(t, pool, func(c pgx.Tx) {
		all, err := chatmemory.All(context.Background(), c, n)
		if err != nil || len(all) != 2 || all[0].Memory != "second" || all[1].Memory != "first" || all[1].Metadata["k"] != "v" || all[0].Metadata == nil {
			t.Fatalf("%v %v", all, err)
		}
	})
}

func TestSearchEmptyQueryReturnsNothing(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	n := ns()
	mustAdd(t, pool, n, "something useful", nil)
	for _, q := range []string{"", "the and", "ab"} {
		if got := search(t, pool, n, q, 5); len(got) != 0 {
			t.Fatalf("%q -> %v", q, got)
		}
	}
}
