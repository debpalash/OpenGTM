//go:build integration

package api

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
)

// newDedupEnv is the lead write env plus erin, an editor member of ws-alpha (the recorded merge persona).
func newDedupEnv(t *testing.T) leadsEnv {
	t.Helper()
	e := newWriteEnv(t)
	e.seed(t, "erin", pythonHash, true, "user", false)
	e.member(t, "ws-alpha", e.mustUserByName(t, "erin").ID, "editor")
	return e
}

// dedupActor says who sent a recorded dedup request; merge fixtures were recorded as erin (editor).
func dedupActor(name string) (who, ws string) {
	switch {
	case strings.HasSuffix(name, "_no_auth"):
		return "", ""
	case name == "dedup_as_viewer", name == "merge_viewer":
		return "bob", "ws-alpha"
	case name == "dedup_beta", name == "get_foreign_after_merge":
		return "bob", "ws-beta"
	case strings.HasPrefix(name, "merge_"):
		return "erin", "ws-alpha"
	}
	return "alice", ""
}

// normDedupRun makes a dedup run order-insensitive: Python's tie ordering depends on PYTHONHASHSEED, so
// compare cluster sets and unordered pair digests instead of list order.
func normDedupRun(t *testing.T, raw []byte) json.RawMessage {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw
	}
	if _, ok := m["pairs"]; !ok {
		return raw
	}
	sortNums := func(v any) {
		if a, ok := v.([]any); ok {
			sort.Slice(a, func(i, j int) bool { return a[i].(float64) < a[j].(float64) })
		}
	}
	if cl, ok := m["clusters"].(map[string]any); ok {
		for _, v := range cl {
			sortNums(v)
		}
	}
	if ps, ok := m["pairs"].([]any); ok {
		for _, p := range ps {
			pm := p.(map[string]any)
			a, b := pm["lead_a_id"].(float64), pm["lead_b_id"].(float64)
			if a > b {
				pm["lead_a_id"], pm["lead_b_id"] = b, a
			}
		}
		sort.Slice(ps, func(i, j int) bool {
			x, y := ps[i].(map[string]any), ps[j].(map[string]any)
			if x["lead_a_id"].(float64) != y["lead_a_id"].(float64) {
				return x["lead_a_id"].(float64) < y["lead_a_id"].(float64)
			}
			return x["lead_b_id"].(float64) < y["lead_b_id"].(float64)
		})
	}
	if ss, ok := m["merge_suggestions"].([]any); ok {
		for _, s := range ss {
			sm := s.(map[string]any)
			sortNums(sm["duplicate_ids"])
			if cs, ok := sm["duplicate_companies"].([]any); ok {
				sort.Slice(cs, func(i, j int) bool { return cs[i].(string) < cs[j].(string) })
			}
		}
		sort.Slice(ss, func(i, j int) bool {
			return ss[i].(map[string]any)["master_id"].(float64) < ss[j].(map[string]any)["master_id"].(float64)
		})
	}
	out, _ := json.Marshal(m)
	return out
}

func (e leadsEnv) replayDedup(t *testing.T, name string) {
	fx := contract.Load(t, "dedup/"+name)
	who, ws := dedupActor(name)
	body := ""
	if len(fx.Request.Body) > 0 && string(fx.Request.Body) != "null" {
		body = string(fx.Request.Body)
	}
	rec := e.send(who, ws, fx.Request.Method, fx.Request.Path, body)
	got := contract.Resp{Status: rec.Code, Headers: map[string]string{}, Body: json.RawMessage(rec.Body.Bytes())}
	for _, k := range []string{"Content-Type", "WWW-Authenticate"} {
		if v := rec.Header().Get(k); v != "" {
			got.Headers[k] = v
		}
	}
	want := fx.Response
	if strings.HasPrefix(name, "dedup_") {
		got.Body = normDedupRun(t, got.Body)
		want.Body = normDedupRun(t, want.Body)
	}
	opts := []contract.Option{}
	if name == "get_merged_master" {
		opts = append(opts, contract.IgnoreShapeOf("$.updated_at"))
	}
	t.Run(name, func(t *testing.T) { contract.AssertEqual(t, got, want, opts...) })
}

var dedupRunFixtures = strings.Fields(`dedup_default dedup_threshold_09 dedup_threshold_05 dedup_threshold_1
dedup_city_mumbai dedup_search_zenith dedup_empty_scope dedup_single_lead dedup_limit_2 dedup_as_viewer dedup_beta
dedup_threshold_low dedup_threshold_high dedup_threshold_bad dedup_limit_zero dedup_limit_5001 dedup_no_auth`)

func TestDedupRunMatchesPython(t *testing.T) {
	e := newDedupEnv(t)
	for _, n := range dedupRunFixtures {
		e.replayDedup(t, n)
	}
}

func TestDedupBounds422(t *testing.T) {
	e := newDedupEnv(t)
	for _, q := range []string{"threshold=0.49", "threshold=1.01", "threshold=nan", "threshold=inf", "threshold=x", "limit=0", "limit=5001", "limit=-1", "limit=x"} {
		if rec := e.send("alice", "ws-alpha", "POST", "/api/leads/dedup?"+q, ""); rec.Code != 422 {
			t.Errorf("%s: %d %s", q, rec.Code, rec.Body.String())
		}
	}
	for _, q := range []string{"threshold=0.5", "threshold=1", "limit=1", "limit=5000"} {
		if rec := e.send("alice", "ws-alpha", "POST", "/api/leads/dedup?"+q, ""); rec.Code != 200 {
			t.Errorf("%s: %d %s", q, rec.Code, rec.Body.String())
		}
	}
}

func TestDedupEmptyShape(t *testing.T) {
	e := newDedupEnv(t)
	rec := e.send("alice", "ws-alpha", "POST", "/api/leads/dedup?city=Nowhere", "")
	want := `{"stats":{"total_leads":0,"duplicates_found":0},"pairs":[],"clusters":{},"scanned":0}`
	if rec.Code != 200 || rec.Body.String() != want {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestDedupFiltersScopeScanned(t *testing.T) {
	e := newDedupEnv(t)
	// "__all__" is unset like "" (_clean); limit bounds scanned; scanned is the trailing key.
	rec := e.send("alice", "ws-alpha", "POST", "/api/leads/dedup?city=__all__&limit=2", "")
	if rec.Code != 200 || !strings.HasSuffix(rec.Body.String(), `,"scanned":2}`) {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	rec = e.send("alice", "ws-alpha", "POST", "/api/leads/dedup?city=Mumbai", "")
	var m struct {
		Scanned int `json:"scanned"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	if rec.Code != 200 || m.Scanned == 0 || m.Scanned >= 15 {
		t.Fatalf("city filter: %d %s", rec.Code, rec.Body.String())
	}
	// A beta lead never shows up in alpha's scan.
	rec = e.send("bob", "ws-beta", "POST", "/api/leads/dedup", "")
	if strings.Contains(rec.Body.String(), `"ws-alpha"`) || rec.Code != 200 {
		t.Fatalf("beta: %d %s", rec.Code, rec.Body.String())
	}
}

func TestMergeSuccessMatchesPython(t *testing.T) {
	e := newDedupEnv(t)
	for _, n := range []string{"merge_ok", "get_merged_master", "get_merged_duplicate", "get_foreign_after_merge", "merge_again"} {
		e.replayDedup(t, n)
	}
	rec := e.send("alice", "ws-alpha", "POST", "/api/leads/dedup/merge", `{"master_id":10,"duplicate_ids":[11]}`)
	if !strings.HasPrefix(rec.Body.String(), `{"status":"merged","master_id":10,"duplicates_deleted":`) {
		t.Errorf("key order: %s", rec.Body.String())
	}
}

func TestMergeErrorsInPythonOrder(t *testing.T) {
	e := newDedupEnv(t)
	for _, n := range strings.Fields(`merge_missing_both merge_missing_duplicates merge_empty_duplicates merge_master_zero
merge_master_not_found merge_master_foreign merge_duplicate_is_master merge_duplicates_none_valid merge_duplicate_foreign_only`) {
		e.replayDedup(t, n)
	}
	// Python checks the master (404) before the "other than master" rule (400).
	rec := e.send("alice", "ws-alpha", "POST", "/api/leads/dedup/merge", `{"master_id":9999,"duplicate_ids":[9999]}`)
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), "Master lead 9999 not found") {
		t.Errorf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestMergeIgnoresDuplicateIdsRepeatedAndMaster(t *testing.T) {
	e := newDedupEnv(t)
	rec := e.send("alice", "ws-alpha", "POST", "/api/leads/dedup/merge", `{"master_id":6,"duplicate_ids":[6,7,7,6,7]}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"duplicates_deleted":1,`) {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if leadRow(t, e, "ws-alpha", 6, "") == nil || leadRow(t, e, "ws-alpha", 7, "") != nil {
		t.Error("master must survive and the duplicate be deleted")
	}
}

func TestMergeNonIntegerIds400(t *testing.T) {
	e := newDedupEnv(t)
	for _, b := range []string{
		`{"master_id":"6","duplicate_ids":[7]}`, `{"master_id":6.5,"duplicate_ids":[7]}`, `{"master_id":true,"duplicate_ids":[7]}`,
		`{"master_id":[6],"duplicate_ids":[7]}`, `{"master_id":6,"duplicate_ids":[7,"8"]}`, `{"master_id":6,"duplicate_ids":7}`,
		`{"master_id":6,"duplicate_ids":"78"}`, `{"master_id":6,"duplicate_ids":[null]}`, `{"master_id":6,"duplicate_ids":[1.0]}`,
	} {
		rec := e.send("alice", "ws-alpha", "POST", "/api/leads/dedup/merge", b)
		if rec.Code != 400 || rec.Body.String() != `{"detail":"master_id and duplicate_ids required"}` {
			t.Errorf("%s: %d %s", b, rec.Code, rec.Body.String())
		}
	}
	if leadRow(t, e, "ws-alpha", 7, "") == nil {
		t.Error("a rejected merge deleted a lead")
	}
	for _, b := range []string{`[1]`, ``, `{bad`, `5`} {
		if rec := e.send("alice", "ws-alpha", "POST", "/api/leads/dedup/merge", b); rec.Code != 422 {
			t.Errorf("%q: %d %s", b, rec.Code, rec.Body.String())
		}
	}
}

func TestMergeNonNumericScoreOrSource422NoWrite(t *testing.T) {
	e := newDedupEnv(t)
	ctx := context.Background()
	owner, err := pgx.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(ctx)
	for _, set := range []string{"score = NULL", "source = NULL"} {
		if _, err := owner.Exec(ctx, "UPDATE leads SET "+set+" WHERE id = 7"); err != nil {
			t.Fatal(err)
		}
		before := leadRow(t, e, "ws-alpha", 7, "")
		rec := e.send("alice", "ws-alpha", "POST", "/api/leads/dedup/merge", `{"master_id":7,"duplicate_ids":[6,8]}`)
		if rec.Code != 422 {
			t.Errorf("%s: %d %s", set, rec.Code, rec.Body.String())
		}
		if leadRow(t, e, "ws-alpha", 6, "") == nil || leadRow(t, e, "ws-alpha", 8, "") == nil {
			t.Errorf("%s: 422 still deleted a duplicate", set)
		}
		if a, b := leadRow(t, e, "ws-alpha", 7, ""), before; !reflect.DeepEqual(a, b) {
			t.Errorf("%s: 422 changed the master", set)
		}
	}
}

func TestMergeIsAtomic(t *testing.T) {
	e := newDedupEnv(t)
	ctx := context.Background()
	owner, err := pgx.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE OR REPLACE FUNCTION t13_fail_delete() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF OLD.id = 8 THEN RAISE EXCEPTION 'forced'; END IF; RETURN OLD; END $$`,
		`CREATE TRIGGER t13_fail BEFORE DELETE ON leads FOR EACH ROW EXECUTE FUNCTION t13_fail_delete()`,
	} {
		if _, err := owner.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		defer owner.Close(ctx)
		_, _ = owner.Exec(ctx, `DROP TRIGGER IF EXISTS t13_fail ON leads`)
		_, _ = owner.Exec(ctx, `DROP FUNCTION IF EXISTS t13_fail_delete()`)
	})
	before := leadRow(t, e, "ws-alpha", 6, "")
	rec := e.send("alice", "ws-alpha", "POST", "/api/leads/dedup/merge", `{"master_id":6,"duplicate_ids":[7,8,9]}`)
	if rec.Code != 500 || rec.Body.String() != `{"detail":"Internal Server Error"}` {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	after := leadRow(t, e, "ws-alpha", 6, "")
	b, _ := json.Marshal(before)
	a, _ := json.Marshal(after)
	if string(a) != string(b) {
		t.Errorf("master changed despite rollback:\n%s\n%s", b, a)
	}
	for _, id := range []int{7, 8, 9} {
		if leadRow(t, e, "ws-alpha", id, "") == nil {
			t.Errorf("duplicate %d deleted despite rollback", id)
		}
	}
}

func TestMergeCrossWorkspaceIds404(t *testing.T) {
	e := newDedupEnv(t)
	for _, b := range []string{`{"master_id":17,"duplicate_ids":[7]}`, `{"master_id":6,"duplicate_ids":[17]}`} {
		if rec := e.send("alice", "ws-alpha", "POST", "/api/leads/dedup/merge", b); rec.Code != 404 {
			t.Errorf("%s: %d %s", b, rec.Code, rec.Body.String())
		}
	}
	if leadRow(t, e, "ws-beta", 17, "") == nil || leadRow(t, e, "ws-alpha", 6, "") == nil || leadRow(t, e, "ws-alpha", 7, "") == nil {
		t.Error("a lead was touched")
	}
}

func TestMergeRequiresEditor(t *testing.T) {
	e := newDedupEnv(t)
	for _, c := range []struct {
		who, ws string
		code    int
	}{{"bob", "ws-alpha", 403}, {"", "", 401}} {
		rec := e.send(c.who, c.ws, "POST", "/api/leads/dedup/merge", `{"master_id":6,"duplicate_ids":[7]}`)
		if c.code == 403 && !strings.Contains(rec.Body.String(), "Workspace permission denied: tables.write") {
			t.Errorf("403 body: %s", rec.Body.String())
		}
		if rec.Code != c.code {
			t.Errorf("%s: %d %s", c.who, rec.Code, rec.Body.String())
		}
	}
	if leadRow(t, e, "ws-alpha", 7, "") == nil {
		t.Error("forbidden merge deleted a lead")
	}
}

func TestMergeAllowsEditor(t *testing.T) {
	e := newDedupEnv(t)
	rec := e.send("erin", "ws-alpha", "POST", "/api/leads/dedup/merge", `{"master_id":6,"duplicate_ids":[7]}`)
	if rec.Code != 200 || leadRow(t, e, "ws-alpha", 7, "") != nil {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestDedupRunAllowsViewer(t *testing.T) {
	e := newDedupEnv(t)
	if rec := e.send("bob", "ws-alpha", "POST", "/api/leads/dedup", ""); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

// A master with an empty city merged with a same-company duplicate that has a city would collide
// with that duplicate on uq_leads_ws_company_city when the master is updated first (Python 500s).
func TestMergeCompanyCityCollisionWithDuplicate(t *testing.T) {
	e := newDedupEnv(t)
	e.exec(t, "ws-alpha", `INSERT INTO leads (id, workspace_id, company, city, score, status, source) VALUES
		(9101, 'ws-alpha', 'Acme Collide', '', 1, 'new', 'x'), (9102, 'ws-alpha', 'Acme Collide', 'Pune', 2, 'new', 'x')`)
	rec := e.send("alice", "ws-alpha", "POST", "/api/leads/dedup/merge", `{"master_id":9101,"duplicate_ids":[9102]}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"duplicates_deleted":1,`) {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	m := leadRow(t, e, "ws-alpha", 9101, "")
	if m == nil || m["city"] != "Pune" || leadRow(t, e, "ws-alpha", 9102, "") != nil {
		t.Errorf("master %v, duplicate must be gone", m)
	}
}

// A unique collision with a third lead that is not part of the merge is a 409, not a 500, and
// nothing is written (the transaction rolls back, the duplicate survives).
func TestMergeCollisionWithThirdLead409(t *testing.T) {
	e := newDedupEnv(t)
	e.exec(t, "ws-alpha", `INSERT INTO leads (id, workspace_id, company, city, score, status, source) VALUES
		(9111, 'ws-alpha', 'Beta Collide', '', 1, 'new', 'x'), (9112, 'ws-alpha', 'Other Collide', 'Pune', 2, 'new', 'x'),
		(9113, 'ws-alpha', 'Beta Collide', 'Pune', 3, 'new', 'x')`)
	rec := e.send("alice", "ws-alpha", "POST", "/api/leads/dedup/merge", `{"master_id":9111,"duplicate_ids":[9112]}`)
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "already exists") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if m := leadRow(t, e, "ws-alpha", 9111, ""); m == nil || m["city"] != "" || leadRow(t, e, "ws-alpha", 9113, "") == nil || leadRow(t, e, "ws-alpha", 9112, "") == nil {
		t.Errorf("nothing may be written: %v", m)
	}
}
