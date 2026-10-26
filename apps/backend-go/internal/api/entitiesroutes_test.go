//go:build integration

package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

type entEnv struct{ mcpEnv }

func newEntEnv(t *testing.T) entEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 4)
	st := identity.NewStore(pool)
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(pool, nil))
	RegisterEntities(r, Deps{Pool: pool, Signer: testSigner, Identity: st})
	return entEnv{mcpEnv{env: env{h: r.Handler(), st: st}, pool: pool}}
}

// ent inserts an entity (and optionally nothing else) in ws and removes it with the test.
func (e entEnv) ent(t *testing.T, ws, id, name string, corroboration int) {
	t.Helper()
	ctx := context.Background()
	err := tenancy.InWorkspace(ctx, e.pool, ws, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO company_entities (id, workspace_id, canonical_name, primary_domain, identity_keys, fields,
			sources, source_agreement, corroboration_count, observation_count, first_seen, last_seen)
			VALUES ($1,$2,$3,$4,$5::json,$6::json,'["seed"]'::json,'{}'::json,$7,1,'2026-08-29T10:00:00','2026-08-29T10:00:00')`,
			id, ws, name, id+".example.com", `{"domain": "`+id+`.example.com"}`, `{"name": "`+name+`"}`, corroboration)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_ = tenancy.InWorkspace(c, e.pool, ws, func(tx pgx.Tx) error {
			_, _ = tx.Exec(c, `DELETE FROM entity_review_pairs WHERE entity_id=$1 OR workspace_id=$2`, id, ws)
			_, _ = tx.Exec(c, `DELETE FROM company_entities WHERE id=$1`, id)
			return nil
		})
	})
}

func (e entEnv) pair(t *testing.T, ws, entity, status string, score float64, cand string) int32 {
	t.Helper()
	ctx := context.Background()
	var id int32
	err := tenancy.InWorkspace(ctx, e.pool, ws, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO entity_review_pairs (entity_id, candidate, score, reason, status, created_at, workspace_id)
			VALUES ($1,$2::json,$3,'similar name',$4,'2026-08-29T10:00:00',$5) RETURNING id`,
			entity, cand, score, status, ws).Scan(&id)
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (e entEnv) get(path string, hdr map[string]string) contract.Resp {
	_, r := e.do("GET", path, "", "", hdr)
	return r
}

func TestCompanyJSONMatchesToAPI(t *testing.T) {
	e := newEntEnv(t)
	_, alice := e.admin(t, "ws-alpha", "t16-alice")
	_, bob := e.admin(t, "ws-beta", "t16-bob")
	e.ent(t, "ws-alpha", "ent-alpha-1", "Acme Corporation", 1)
	e.ent(t, "ws-alpha", "ent-alpha-2", "Acme Corp", 1)
	// The recorded review pair has id 1; pin it so the fixture compares byte for byte.
	ctx := context.Background()
	if err := tenancy.InWorkspace(ctx, e.pool, "ws-alpha", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO entity_review_pairs (id, entity_id, candidate, score, reason, status, created_at, workspace_id)
			VALUES (1,'ent-alpha-1','{"id": "ent-alpha-2", "canonical_name": "Acme Corp"}'::json,0.87,'similar name','pending','2026-08-29T10:00:00','ws-alpha')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	rp := &replayer{seen: map[string]bool{}, do: func(m, p, b string, h map[string]string) contract.Resp {
		_, got := e.do(m, p, "application/json", b, h)
		return got
	}}
	for _, n := range []string{"list_default", "list_min_corroboration_2", "list_limit_1", "list_min_corroboration_0",
		"list_limit_1001", "list_limit_text", "list_no_token", "get_ok", "get_missing", "review_default",
		"review_status_rejected", "review_limit_0", "review_limit_1001"} {
		rp.replay(t, "entities/"+n, alice)
	}
	for _, n := range []string{"list_other_workspace", "get_other_workspace", "review_other_workspace"} {
		rp.replay(t, "entities/"+n, bob)
	}
	rp.assertAllReplayed(t, "entities")
}

func TestListMinCorroborationAndLimitBounds422(t *testing.T) {
	e := newEntEnv(t)
	_, h := e.admin(t, "t16-ws-bounds", "t16-bounds")
	for _, tc := range []struct{ path, typ, msg string }{
		{"/api/entities/company?min_corroboration=0", "greater_than_equal", "Input should be greater than or equal to 1"},
		{"/api/entities/company?limit=1001", "less_than_equal", "Input should be less than or equal to 1000"},
		{"/api/entities/review-queue?limit=1001", "less_than_equal", "Input should be less than or equal to 1000"},
		{"/api/entities/company?limit=1.5", "int_parsing", "Input should be a valid integer, unable to parse string as an integer"},
	} {
		r := e.get(tc.path, h)
		var b struct{ Detail []struct{ Type, Msg string } }
		if r.Status != 422 || json.Unmarshal(r.Body, &b) != nil || len(b.Detail) != 1 || b.Detail[0].Type != tc.typ || b.Detail[0].Msg != tc.msg {
			t.Errorf("%s: %d %s", tc.path, r.Status, r.Body)
		}
	}
	for _, p := range []string{"/api/entities/company?min_corroboration=1&limit=1000", "/api/entities/company?limit=0"} {
		if r := e.get(p, h); r.Status != 200 {
			t.Errorf("%s: %d %s", p, r.Status, r.Body)
		}
	}
}

func TestListOrdersByCorroborationDesc(t *testing.T) {
	e := newEntEnv(t)
	_, h := e.admin(t, "t16-ws-order", "t16-order")
	e.ent(t, "t16-ws-order", "t16-o-low", "Low", 1)
	e.ent(t, "t16-ws-order", "t16-o-high", "High", 3)
	r := e.get("/api/entities/company", h)
	var b struct{ Entities []struct{ ID string } }
	_ = json.Unmarshal(r.Body, &b)
	if len(b.Entities) != 2 || b.Entities[0].ID != "t16-o-high" {
		t.Fatalf("%s", r.Body)
	}
	r = e.get("/api/entities/company?min_corroboration=2", h)
	_ = json.Unmarshal(r.Body, &b)
	if len(b.Entities) != 1 || b.Entities[0].ID != "t16-o-high" {
		t.Fatalf("%s", r.Body)
	}
}

func TestGetOtherWorkspace404(t *testing.T) {
	e := newEntEnv(t)
	_, a := e.admin(t, "t16-ws-a", "t16-a")
	_, b := e.admin(t, "t16-ws-b", "t16-b")
	e.ent(t, "t16-ws-a", "t16-ent-a", "A Co", 1)
	if r := e.get("/api/entities/company/t16-ent-a", a); r.Status != 200 {
		t.Fatalf("own: %d %s", r.Status, r.Body)
	}
	r := e.get("/api/entities/company/t16-ent-a", b)
	if r.Status != 404 || string(r.Body) == "" {
		t.Fatalf("other workspace: %d %s", r.Status, r.Body)
	}
	var d struct{ Detail string }
	_ = json.Unmarshal(r.Body, &d)
	if d.Detail != "Entity not found" {
		t.Fatalf("detail %q", d.Detail)
	}
}

func TestReviewQueueJoinsTenantEntitiesOnly(t *testing.T) {
	e := newEntEnv(t)
	_, a := e.admin(t, "t16-ws-ja", "t16-ja")
	_, b := e.admin(t, "t16-ws-jb", "t16-jb")
	e.ent(t, "t16-ws-ja", "t16-ent-ja", "JA", 1)
	e.ent(t, "t16-ws-jb", "t16-ent-jb", "JB", 1)
	ownA := e.pair(t, "t16-ws-ja", "t16-ent-ja", "pending", 0.8, `{"new_entity_id":"x"}`)
	ownB := e.pair(t, "t16-ws-jb", "t16-ent-jb", "pending", 0.7, `{}`)
	// A pair stamped with workspace B but pointing at A's entity must never surface for either tenant.
	e.pair(t, "t16-ws-jb", "t16-ent-ja", "pending", 0.99, `{}`)
	ids := func(h map[string]string) []int32 {
		var out struct{ Pairs []struct{ ID int32 } }
		r := e.get("/api/entities/review-queue", h)
		if r.Status != 200 || json.Unmarshal(r.Body, &out) != nil {
			t.Fatalf("%d %s", r.Status, r.Body)
		}
		var got []int32
		for _, p := range out.Pairs {
			got = append(got, p.ID)
		}
		return got
	}
	if g := ids(a); len(g) != 1 || g[0] != ownA {
		t.Errorf("A sees %v, want [%d]", g, ownA)
	}
	if g := ids(b); len(g) != 1 || g[0] != ownB {
		t.Errorf("B sees %v, want [%d]", g, ownB)
	}
}

func TestReviewQueueStatusFilter(t *testing.T) {
	e := newEntEnv(t)
	_, h := e.admin(t, "t16-ws-st", "t16-st")
	e.ent(t, "t16-ws-st", "t16-ent-st", "ST", 1)
	e.pair(t, "t16-ws-st", "t16-ent-st", "pending", 0.71, `{}`)
	top := e.pair(t, "t16-ws-st", "t16-ent-st", "pending", 0.84, `{}`)
	rej := e.pair(t, "t16-ws-st", "t16-ent-st", "rejected", 0.8, `{}`)
	var out struct{ Pairs []struct{ ID int32 } }
	r := e.get("/api/entities/review-queue", h)
	_ = json.Unmarshal(r.Body, &out)
	if len(out.Pairs) != 2 || out.Pairs[0].ID != top {
		t.Fatalf("pending ordered by score desc: %s", r.Body)
	}
	r = e.get("/api/entities/review-queue?status=rejected", h)
	_ = json.Unmarshal(r.Body, &out)
	if len(out.Pairs) != 1 || out.Pairs[0].ID != rej {
		t.Fatalf("rejected: %s", r.Body)
	}
	r = e.get("/api/entities/review-queue?limit=1", h)
	_ = json.Unmarshal(r.Body, &out)
	if len(out.Pairs) != 1 || out.Pairs[0].ID != top {
		t.Fatalf("limit: %s", r.Body)
	}
}
