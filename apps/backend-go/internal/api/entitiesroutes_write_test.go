//go:build integration

package api

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

const wsObs = "2026-08-29T10:00:00+00:00"

// seedWriteEntities replays contract_seed_entities_write.py (entities and review pairs 2 and 3) plus the
// base seed's pair 1, in the workspaces the fixtures were recorded in.
func (e entEnv) seedWriteEntities(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	type ent struct{ id, ws, name, dom, site string }
	ents := []ent{
		{"ent-w-1", "ws-alpha", "Zenith Corp", "zenith.example.com", "https://zenith.example.com"},
		{"ent-w-2", "ws-alpha", "Zenith Corporation Ltd", "zenith-corp.example.com", "https://zenith-corp.example.com"},
		{"ent-w-3", "ws-alpha", "Orbit Labs", "orbit.example.com", "https://orbit.example.com"},
		{"ent-w-4", "ws-alpha", "Orbit Laboratories", "orbitlabs.example.com", "https://orbitlabs.example.com"},
		{"ent-beta-1", "ws-beta", "Beta Only Inc", "betaonly.example.com", "https://betaonly.example.com"},
	}
	obs := func(v string) map[string]string {
		return map[string]string{"value": v, "source": "seed", "observed_at": wsObs}
	}
	j := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	for _, en := range ents {
		fields := j(map[string]any{"company": []any{obs(en.name)}, "website": []any{obs(en.site)}, "city": []any{obs("Austin")}})
		keys := j(map[string]any{"domains": []string{en.dom}, "phones": []string{}, "emails": []string{}, "name_variants": []string{en.name}})
		if err := tenancy.InWorkspace(ctx, e.pool, en.ws, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO company_entities (id, workspace_id, canonical_name, primary_domain, primary_phone, primary_email,
				primary_city, identity_keys, fields, sources, source_agreement, corroboration_count, observation_count, first_seen, last_seen)
				VALUES ($1,$2,$3,$4,'','','Austin',$5::json,$6::json,'["seed"]'::json,'{"company": 1.0, "website": 1.0, "city": 1.0}'::json,1,3,
				'2026-08-29T10:00:00','2026-08-29T10:00:00')`, en.id, en.ws, en.name, en.dom, keys, fields)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	pairs := []struct {
		id    int
		ent   string
		cand  string
		score float64
	}{
		{1, "ent-alpha-1", `{"id": "ent-alpha-2", "canonical_name": "Acme Corp"}`, 0.87},
		{2, "ent-w-3", `{"id":"ent-w-4","new_entity_id":"ent-w-4","canonical_name":"Orbit Laboratories"}`, 0.8},
		{3, "ent-w-1", `{"id":"ent-w-2","canonical_name":"Zenith Corporation Ltd"}`, 0.8},
	}
	if err := tenancy.InWorkspace(ctx, e.pool, "ws-alpha", func(tx pgx.Tx) error {
		for _, p := range pairs {
			if _, err := tx.Exec(ctx, `INSERT INTO entity_review_pairs (id, entity_id, candidate, score, reason, status, created_at, workspace_id)
				VALUES ($1,$2,$3::json,$4,'grey_band','pending','2026-08-29T10:00:00','ws-alpha')`, p.id, p.ent, p.cand, p.score); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		for _, ws := range []string{"ws-alpha", "ws-beta"} {
			_ = tenancy.InWorkspace(c, e.pool, ws, func(tx pgx.Tx) error {
				for _, tbl := range []string{"entity_review_pairs", "entity_merge_log", "entity_blocking_keys", "company_identifiers", "company_entities"} {
					_, _ = tx.Exec(c, `DELETE FROM `+tbl+` WHERE workspace_id=$1`, ws)
				}
				return nil
			})
		}
	})
	// ent-alpha-1 is the base seed's entity that review pair 1 points at.
	if err := tenancy.InWorkspace(ctx, e.pool, "ws-alpha", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO company_entities (id, workspace_id, canonical_name, primary_domain, identity_keys, fields, sources,
			source_agreement, corroboration_count, observation_count, first_seen, last_seen)
			VALUES ('ent-alpha-1','ws-alpha','Acme Corporation','acme01.example.com','{}','{}','["seed"]','{}',1,1,'2026-08-29T10:00:00','2026-08-29T10:00:00')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// resetMergeLogSeq makes the next merge log id 1, like the freshly seeded recording database.
func resetMergeLogSeq(t *testing.T) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	owner, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if _, err := owner.Exec(context.Background(), `SELECT setval('entity_merge_log_id_seq', 1, false)`); err != nil {
		t.Fatal(err)
	}
}

// persona creates the contract users: alice owner/admin of ws-alpha, erin editor and bob plain member in
// ws-alpha, bob owner of ws-beta. It returns the auth headers per persona.
func (e entEnv) personas(t *testing.T) (alice, erin, bobAlpha, bobBeta map[string]string) {
	t.Helper()
	e.seed(t, "t17-alice", pythonHash, true, "user", false)
	e.seed(t, "t17-erin", pythonHash, true, "user", false)
	e.seed(t, "t17-bob", pythonHash, true, "user", false)
	a, er, b := e.mustUserByName(t, "t17-alice"), e.mustUserByName(t, "t17-erin"), e.mustUserByName(t, "t17-bob")
	if !e.wsExists("ws-alpha") {
		e.workspace(t, "ws-alpha", a.ID)
	}
	if !e.wsExists("ws-beta") {
		e.workspace(t, "ws-beta", b.ID)
	}
	e.member(t, "ws-alpha", a.ID, "owner")
	e.member(t, "ws-alpha", er.ID, "editor")
	e.member(t, "ws-alpha", b.ID, "member")
	e.member(t, "ws-beta", b.ID, "owner")
	h := func(user, ws string) map[string]string {
		m := bearer(user)
		m["X-Workspace-Id"] = ws
		return m
	}
	return h("t17-alice", "ws-alpha"), h("t17-erin", "ws-alpha"), h("t17-bob", "ws-alpha"), h("t17-bob", "ws-beta")
}

func TestEntityWritesMatchFixtures(t *testing.T) {
	e := newEntEnv(t)
	alice, erin, bobA, bobB := e.personas(t)
	e.seedWriteEntities(t)
	resetMergeLogSeq(t)
	persona := map[string]map[string]string{
		"merge_non_editor": bobA, "merge_other_workspace_header": bobB, "merge_ok": erin,
		"split_non_editor": bobA, "split_other_workspace": bobB,
		"decide_non_editor": bobA, "decide_other_workspace": bobB,
	}
	rp := &replayer{seen: map[string]bool{}, do: func(m, p, b string, h map[string]string) contract.Resp {
		_, got := e.do(m, p, "application/json", b, h)
		return got
	}}
	// Recording order matters: later cases observe the writes of earlier ones.
	for _, n := range []string{"merge_no_token", "merge_non_editor", "merge_missing_field", "merge_wrong_type", "merge_body_not_object",
		"merge_same_entity", "merge_unknown_entity", "merge_other_workspace_header", "merge_ok", "get_kept_after_merge",
		"get_merged_after_merge", "merge_again", "split_non_editor", "split_missing_field", "split_not_int", "split_unknown_log",
		"split_other_workspace", "split_ok", "get_kept_after_split", "get_restored_after_split", "split_again",
		"decide_non_editor", "decide_missing_field", "decide_missing_pair", "decide_other_workspace", "decide_no_candidate",
		"decide_merge", "decide_reject_other_word", "review_merged", "review_rejected"} {
		auth := alice
		if p, ok := persona[n]; ok {
			auth = p
		}
		rp.replay(t, "entities_write/"+n, auth)
	}
	rp.assertAllReplayed(t, "entities_write")
}

func (e entEnv) post(path, body string, h map[string]string) (int, map[string]any) {
	rec, _ := e.do("POST", path, "application/json", body, h)
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	return rec.Code, m
}

func (e entEnv) entityCount(t *testing.T, ws string) int {
	t.Helper()
	var n int
	if err := tenancy.InWorkspace(context.Background(), e.pool, ws, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM company_entities WHERE workspace_id=$1`, ws).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

// A user who is not a member of the header workspace must reach nothing and write nothing.
func TestWritesWithForeignWorkspaceHeaderWriteNothing(t *testing.T) {
	e := newEntEnv(t)
	alice, _, _, _ := e.personas(t)
	e.seedWriteEntities(t)
	foreign := map[string]string{"Authorization": alice["Authorization"], "X-Workspace-Id": "ws-beta"}
	before := e.entityCount(t, "ws-alpha")
	for _, c := range [][2]string{
		{"/api/entities/merge", `{"kept_id":"ent-w-1","merged_id":"ent-w-2"}`},
		{"/api/entities/split", `{"merge_log_id":1}`},
		{"/api/entities/review-queue/decide", `{"pair_id":2,"decision":"merge"}`},
	} {
		code, body := e.post(c[0], c[1], foreign)
		if code != 403 && code != 404 {
			t.Errorf("%s: %d %v", c[0], code, body)
		}
	}
	if e.entityCount(t, "ws-alpha") != before {
		t.Error("entities changed")
	}
}

// A member of ws-beta naming ws-alpha's entities (merge) or pair (decide) gets the same answers as for unknown ids.
func TestWritesCannotReachOtherWorkspaceRows(t *testing.T) {
	e := newEntEnv(t)
	_, _, _, bobB := e.personas(t)
	e.seedWriteEntities(t)
	before := e.entityCount(t, "ws-alpha")
	code, body := e.post("/api/entities/merge", `{"kept_id":"ent-beta-1","merged_id":"ent-w-2"}`, bobB)
	if code != 400 || body["detail"] != "entity_not_found" {
		t.Errorf("merge merged-id of another workspace: %d %v", code, body)
	}
	code, body = e.post("/api/entities/merge", `{"kept_id":"ent-w-1","merged_id":"ent-beta-1"}`, bobB)
	if code != 400 || body["detail"] != "entity_not_found" {
		t.Errorf("merge kept-id of another workspace: %d %v", code, body)
	}
	code, body = e.post("/api/entities/review-queue/decide", `{"pair_id":2,"decision":"merge"}`, bobB)
	if code != 404 || body["detail"] != "Review pair not found" {
		t.Errorf("decide: %d %v", code, body)
	}
	if e.entityCount(t, "ws-alpha") != before || e.entityCount(t, "ws-beta") != 1 {
		t.Error("entities changed")
	}
}

// Deviation: Python answers 500 when provenance is not field -> list; Go answers 400 invalid_entity_fields.
func TestMergeNonListProvenance400(t *testing.T) {
	e := newEntEnv(t)
	alice, _, _, _ := e.personas(t)
	e.seedWriteEntities(t)
	ctx := context.Background()
	_ = tenancy.InWorkspace(ctx, e.pool, "ws-alpha", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE company_entities SET fields='{"name":"x"}' WHERE id='ent-w-2'`)
		return err
	})
	code, body := e.post("/api/entities/merge", `{"kept_id":"ent-w-1","merged_id":"ent-w-2"}`, alice)
	if code != 400 || body["detail"] != "invalid_entity_fields" {
		t.Fatalf("%d %v", code, body)
	}
	if e.entityCount(t, "ws-alpha") != 4 {
		t.Error("entities changed")
	}
}
