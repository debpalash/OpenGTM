//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

// arunFrozen is the instant the fixtures were recorded at (the manual-run fire key embeds it).
var arunFrozen = time.Date(2099, 1, 1, 0, 0, 0, 1000, time.UTC)

type arunEnv struct{ mcpEnv }

// newArunEnv registers the preview/run/history routes only, over the automations seed the fixtures
// were recorded against (testdata/autorun/seed.json: legacy/python/scripts/contract_seed.py with
// CONTRACT_SEED_AUTOMATIONS=yes, dumped for workbooks, rows, triggers, runs and action results).
func newArunEnv(t *testing.T, automations bool, getenv func(string) string) arunEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	st := identity.NewStore(pool)
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(pool, nil))
	d := Deps{Pool: pool, Signer: testSigner, Identity: st, Getenv: getenv,
		Flags: config.Flags{PGLeadStore: true, AutomationsEnabled: automations,
			Automations: config.AutomationsSettings{MaxRowsPerEval: 500, GoExecutorTriggerEval: true}}}
	registerAutomationsRun(r, d)
	e := arunEnv{mcpEnv{env: env{h: r.Handler(), st: st}, pool: pool}}
	old := autoRunNow
	autoRunNow = func() time.Time { return arunFrozen }
	e.purge()
	t.Cleanup(func() { autoRunNow = old; e.purge() })
	e.seedAutomations(t)
	return e
}

func (e arunEnv) purge() {
	ctx := context.Background()
	for _, ws := range []string{"ws-alpha", "ws-beta"} {
		_ = tenancy.InWorkspace(ctx, e.pool, ws, func(tx pgx.Tx) error {
			for _, tbl := range []string{"trigger_action_results", "trigger_runs", "trigger_cap_reservations", "triggers", "workbook_rows", "workbooks"} {
				_, _ = tx.Exec(ctx, "DELETE FROM "+tbl)
			}
			_, _ = tx.Exec(ctx, `DELETE FROM jobs WHERE workspace_id = $1`, ws)
			return nil
		})
	}
}

func (e arunEnv) seedAutomations(t *testing.T) {
	t.Helper()
	for _, u := range []struct {
		name  string
		admin bool
	}{{"alice", true}, {"bob", false}, {"erin", false}} {
		role := "user"
		if u.admin {
			role = "admin"
		}
		e.seed(t, u.name, pythonHash, true, role, u.admin)
	}
	alice, bob, erin := e.mustUserByName(t, "alice"), e.mustUserByName(t, "bob"), e.mustUserByName(t, "erin")
	e.workspace(t, "ws-alpha", alice.ID)
	e.workspace(t, "ws-beta", bob.ID)
	e.member(t, "ws-alpha", alice.ID, "owner")
	e.member(t, "ws-alpha", bob.ID, "member")
	e.member(t, "ws-alpha", erin.ID, "editor")
	e.member(t, "ws-beta", bob.ID, "owner")
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "autorun", "seed.json"))
	if err != nil {
		t.Fatal(err)
	}
	var seed map[string]map[string]struct {
		Columns []string        `json:"columns"`
		Rows    json.RawMessage `json:"rows"`
	}
	if err := json.Unmarshal(raw, &seed); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for ws, tables := range seed {
		err := tenancy.InWorkspace(ctx, e.pool, ws, func(tx pgx.Tx) error {
			for _, tbl := range []string{"workbooks", "workbook_rows", "triggers", "trigger_runs", "trigger_action_results"} {
				d := tables[tbl]
				cols := strings.Join(d.Columns, ", ")
				if _, err := tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %[1]s (%[2]s) SELECT %[2]s FROM json_populate_recordset(null::%[1]s, $1::json)`, tbl, cols), string(d.Rows)); err != nil {
					return fmt.Errorf("%s %s: %w", ws, tbl, err)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// arunActor says who sent a recorded request (fixtures keep Authorization and X-Workspace-Id redacted;
// the case file legacy/python/scripts/contract_cases/automations.yaml names the actor: bob, erin or alice).
func arunActor(name string) string {
	switch name {
	case "run_detail_beta_own", "preview_member_read_ok", "run_member_forbidden", "off_automations_run_member_403_first":
		return "bob"
	case "run_editor_forbidden":
		return "erin"
	}
	if strings.HasSuffix(name, "_no_auth") {
		return ""
	}
	return "alice"
}

func (e arunEnv) call(who, ws, method, path, body string) contract.Resp {
	hdr := map[string]string{}
	if who != "" {
		hdr = bearer(who)
		if ws != "" {
			hdr["X-Workspace-Id"] = ws
		}
	}
	ctype := ""
	if body != "" {
		ctype = "application/json"
	}
	_, resp := e.do(method, path, ctype, body, hdr)
	return resp
}

func (e arunEnv) replay(t *testing.T, name string, opts ...contract.Option) {
	t.Helper()
	fx := contract.Load(t, "automations/"+name)
	ws := ""
	if _, ok := fx.Request.Headers["X-Workspace-Id"]; ok {
		ws = "ws-alpha"
	}
	body := ""
	if fx.Request.Body != nil && strings.TrimSpace(string(fx.Request.Body)) != "null" {
		body = string(fx.Request.Body)
	}
	got := e.call(arunActor(name), ws, fx.Request.Method, fx.Request.Path, body)
	t.Run(name, func(t *testing.T) { contract.AssertEqual(t, got, fx.Response, opts...) })
}

// arunPython500 are recorded Python crashes that Go answers as defined results (D4): non-numeric
// row ids are dropped, a negative history limit is a 422. They have their own tests.
var arunPython500 = map[string]bool{"preview_ids_nonnumeric": true, "preview_ids_mixed_nonnumeric": true,
	"run_ids_nonnumeric": true, "runs_limit_negative": true}

func arunNames(t *testing.T, prefixes ...string) []string {
	t.Helper()
	var out []string
	for _, n := range contractFixtureNames(t, "automations") {
		n = strings.TrimPrefix(n, "automations/")
		if arunPython500[n] {
			continue
		}
		for _, p := range prefixes {
			if strings.HasPrefix(n, p) {
				out = append(out, n)
				break
			}
		}
	}
	return out
}

func TestPreviewMatchesFixtures(t *testing.T) {
	e := newArunEnv(t, true, nil)
	names := arunNames(t, "preview_")
	if len(names) < 20 {
		t.Fatalf("expected the recorded preview fixtures, got %d", len(names))
	}
	for _, n := range names {
		e.replay(t, n)
	}
}

// spyTransport counts every outbound HTTP request made through the default client.
type spyTransport struct{ n atomic.Int64 }

func (s *spyTransport) RoundTrip(*http.Request) (*http.Response, error) {
	s.n.Add(1)
	return nil, fmt.Errorf("outbound call from preview")
}

// arunWorldState is a checksum of every table a preview must never touch.
func (e arunEnv) worldState(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	var parts []string
	for _, ws := range []string{"ws-alpha", "ws-beta"} {
		if err := tenancy.InWorkspace(ctx, e.pool, ws, func(tx pgx.Tx) error {
			for _, tbl := range []string{"triggers", "trigger_runs", "trigger_action_results", "trigger_cap_reservations",
				"workbook_rows", "workbooks", "credit_ledger_entries", "workspace_credits", "scheduled_triggers"} {
				var s string
				if err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT count(*)::text || ':' || coalesce(md5(string_agg(x::text, '|' ORDER BY x::text)), '') FROM %s x`, tbl)).Scan(&s); err != nil {
					return err
				}
				parts = append(parts, ws+"/"+tbl+"="+s)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	var jobs string
	if err := e.pool.QueryRow(ctx, `SELECT count(*)::text FROM jobs`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	return strings.Join(parts, "\n") + "\njobs=" + jobs
}

func TestPreviewMakesNoOutboundCallOrWrite(t *testing.T) {
	e := newArunEnv(t, true, nil)
	spy := &spyTransport{}
	oldT, oldC := http.DefaultTransport, http.DefaultClient.Transport
	http.DefaultTransport, http.DefaultClient.Transport = spy, spy
	t.Cleanup(func() { http.DefaultTransport, http.DefaultClient.Transport = oldT, oldC })

	before := e.worldState(t)
	// trg-a-01 is a webhook rule with a secret ref, trg-a-07 a CRM push, trg-a-05 a paid re-enrich.
	for _, id := range []string{"trg-a-01", "trg-a-07", "trg-a-05", "trg-a-09", "trg-a-03"} {
		r := e.call("alice", "", "POST", "/api/automations/triggers/"+id+"/preview", `{"limit":100}`)
		if r.Status != 200 {
			t.Fatalf("%s: %d %s", id, r.Status, r.Body)
		}
		if strings.Contains(string(r.Body), "ctk_automation_hook_secret") {
			t.Fatalf("%s: preview echoed a secret", id)
		}
	}
	if n := spy.n.Load(); n != 0 {
		t.Fatalf("preview made %d outbound calls", n)
	}
	if after := e.worldState(t); after != before {
		t.Fatalf("preview changed the database:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	// The preview transaction is read-only: a write inside it is refused by Postgres, not just absent.
	err := tenancy.InWorkspace(context.Background(), e.pool, "ws-alpha", func(tx pgx.Tx) error {
		if _, err := tx.Exec(context.Background(), "SET TRANSACTION READ ONLY"); err != nil {
			return err
		}
		_, err := tx.Exec(context.Background(), `DELETE FROM trigger_runs`)
		return err
	})
	if err == nil {
		t.Fatal("expected a read-only transaction to refuse writes")
	}
}

func arunMatched(t *testing.T, r contract.Resp) (matched int, sample []map[string]any) {
	t.Helper()
	if r.Status != 200 {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
	var b struct {
		Matched int              `json:"matched"`
		Sample  []map[string]any `json:"sample"`
	}
	if err := json.Unmarshal(r.Body, &b); err != nil {
		t.Fatal(err)
	}
	return b.Matched, b.Sample
}

func TestPreviewLimitClamp(t *testing.T) {
	e := newArunEnv(t, true, nil)
	for _, c := range []struct {
		body string
		want int
	}{{`{}`, 8}, {`{"limit":0}`, 8}, {`{"limit":-5}`, 1}, {`{"limit":1}`, 1}, {`{"limit":3}`, 3}, {`{"limit":500}`, 8}, {`{"limit":"2"}`, 2}} {
		r := e.call("alice", "", "POST", "/api/automations/triggers/trg-a-01/preview", c.body)
		matched, sample := arunMatched(t, r)
		if matched != 8 || len(sample) != c.want {
			t.Errorf("%s: matched %d sample %d, want 8 / %d", c.body, matched, len(sample), c.want)
		}
	}
	// 100 is the ceiling even when more rows match: trg-a-02 matches 11 rows, a limit of 100 returns all 11.
	r := e.call("alice", "", "POST", "/api/automations/triggers/trg-a-05/preview", `{"limit":9999}`)
	if _, sample := arunMatched(t, r); len(sample) > 100 {
		t.Fatalf("sample %d exceeds the 100 ceiling", len(sample))
	}
}

func TestPreviewNonNumericIDsNoCrash(t *testing.T) {
	e := newArunEnv(t, true, nil)
	for _, body := range []string{`{"row_ids":["abc"]}`, `{"row_ids":["3001","x"]}`, `{"row_ids":["", " ", "1e3", "3001.5", "-1", "99999999999999999999"]}`} {
		r := e.call("alice", "", "POST", "/api/automations/triggers/trg-a-01/preview", body)
		if r.Status != 200 {
			t.Fatalf("%s: %d %s (Python 500; Go answers 200 with the bad ids dropped)", body, r.Status, r.Body)
		}
	}
	r := e.call("alice", "", "POST", "/api/automations/triggers/trg-a-01/preview", `{"row_ids":["3001","x"]}`)
	if _, sample := arunMatched(t, r); len(sample) != 1 || sample[0]["row_id"] != "3001" {
		t.Fatalf("only the valid id remains: %s", r.Body)
	}
	r = e.call("alice", "", "POST", "/api/automations/triggers/trg-a-01/run", `{"row_ids":["abc"]}`)
	if r.Status != 200 {
		t.Fatalf("run with non-numeric id: %d %s", r.Status, r.Body)
	}
}

func TestPreviewForeignRowsDropped(t *testing.T) {
	e := newArunEnv(t, true, nil)
	// 3101 belongs to ws-alpha? no: wb-auto-beta rows 3101/3102 are ws-beta's.
	r := e.call("alice", "", "POST", "/api/automations/triggers/trg-a-01/preview", `{"row_ids":["3101","3102"]}`)
	matched, sample := arunMatched(t, r)
	for _, s := range sample {
		if s["workbook_id"] == "wb-auto-beta" {
			t.Fatalf("foreign row leaked: %s", r.Body)
		}
	}
	_ = matched
	r = e.call("alice", "", "POST", "/api/automations/triggers/trg-a-01/preview", `{"row_ids":["3001","3101"]}`)
	if _, sample := arunMatched(t, r); len(sample) != 1 || sample[0]["row_id"] != "3001" {
		t.Fatalf("own row kept, foreign dropped: %s", r.Body)
	}
	// A foreign trigger is 404, never 403.
	r = e.call("alice", "", "POST", "/api/automations/triggers/trg-b-01/preview", `{}`)
	if r.Status != 404 || strings.TrimSpace(string(r.Body)) != `{"detail":"trigger not found"}` {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
}

func TestPreviewTemplateErrorShape(t *testing.T) {
	e := newArunEnv(t, true, nil)
	// wb-auto-2 has no `website` column: the webhook body of trg-a-01 cannot resolve there.
	r := e.call("alice", "", "POST", "/api/automations/triggers/trg-a-01/preview", `{"row_ids":["3011"]}`)
	_, sample := arunMatched(t, r)
	if len(sample) != 1 {
		t.Fatalf("%s", r.Body)
	}
	acts := sample[0]["actions"].([]any)
	prev := acts[0].(map[string]any)["resolved_payload_preview"].(map[string]any)
	if len(prev) != 1 || prev["error"] != "preview failed: Unknown column reference: website. Fix the template before sending." {
		t.Fatalf("error shape: %v", prev)
	}
}

func TestRunEnqueuesTriggerEval(t *testing.T) {
	cohort := func(k string) string {
		switch k {
		case "GO_EXECUTOR_ENABLED":
			return "true"
		case "GO_EXECUTOR_WORKSPACES":
			return "ws-alpha"
		}
		return ""
	}
	for _, c := range []struct {
		name    string
		getenv  func(string) string
		wantExe string
	}{{"cohort", cohort, "go"}, {"off", func(string) string { return "" }, "python"}} {
		t.Run(c.name, func(t *testing.T) {
			e := newArunEnv(t, true, c.getenv)
			r := e.call("alice", "", "POST", "/api/automations/triggers/trg-a-01/run", `{"row_ids":["3001","3002","nope","3101"]}`)
			if r.Status != 200 {
				t.Fatalf("%d %s", r.Status, r.Body)
			}
			var res struct {
				JobID   int64  `json:"job_id"`
				FireKey string `json:"fire_key"`
				DryRun  bool   `json:"dry_run"`
			}
			if err := json.Unmarshal(r.Body, &res); err != nil {
				t.Fatal(err)
			}
			if res.FireKey != "manual:trg-a-01:2099-01-01T00:00:00.000001+00:00" || res.DryRun {
				t.Fatalf("%s", r.Body)
			}
			var typ, exe, payload, ws string
			if err := e.pool.QueryRow(context.Background(), `SELECT type, executor, payload::text, workspace_id FROM jobs WHERE id=$1`, res.JobID).Scan(&typ, &exe, &payload, &ws); err != nil {
				t.Fatal(err)
			}
			if typ != "trigger_eval" || exe != c.wantExe || ws != "ws-alpha" {
				t.Fatalf("job %s %s %s", typ, exe, ws)
			}
			var p map[string]any
			if err := json.Unmarshal([]byte(payload), &p); err != nil {
				t.Fatal(err)
			}
			targets, _ := p["targets"].([]any)
			if p["trigger_id"] != "trg-a-01" || p["workspace_id"] != "ws-alpha" || p["fire_source"] != "manual" ||
				p["fire_key"] != res.FireKey || p["dry_run"] != false || len(targets) != 2 {
				t.Fatalf("payload %s", payload)
			}
			t0 := targets[0].(map[string]any)
			if t0["workbook_id"] != "wb-auto-1" || t0["row_id"] != "3001" {
				t.Fatalf("targets %v", targets)
			}
			// the job never touched the run tables: the worker creates the run row
			var n int
			_ = tenancy.InWorkspace(context.Background(), e.pool, "ws-alpha", func(tx pgx.Tx) error {
				return tx.QueryRow(context.Background(), `SELECT count(*) FROM trigger_runs WHERE job_id=$1`, int32(res.JobID)).Scan(&n)
			})
			if n != 0 {
				t.Fatalf("manual run wrote %d run rows", n)
			}
		})
	}
}

func TestRunDryRunFlag(t *testing.T) {
	e := newArunEnv(t, true, nil)
	for _, c := range []struct {
		body string
		want bool
	}{{`{}`, false}, {`{"dry_run":true}`, true}, {`{"dry_run":"true"}`, true}, {`{"dry_run":"no"}`, false}, {`{"dry_run":0}`, false}} {
		r := e.call("alice", "", "POST", "/api/automations/triggers/trg-a-01/run", c.body)
		if r.Status != 200 {
			t.Fatalf("%s: %d %s", c.body, r.Status, r.Body)
		}
		var res struct {
			JobID  int64 `json:"job_id"`
			DryRun bool  `json:"dry_run"`
		}
		_ = json.Unmarshal(r.Body, &res)
		var payload string
		if err := e.pool.QueryRow(context.Background(), `SELECT payload::text FROM jobs WHERE id=$1`, res.JobID).Scan(&payload); err != nil {
			t.Fatal(err)
		}
		var p map[string]any
		_ = json.Unmarshal([]byte(payload), &p)
		if res.DryRun != c.want || p["dry_run"] != c.want {
			t.Errorf("%s: response %v payload %v want %v", c.body, res.DryRun, p["dry_run"], c.want)
		}
	}
	// a text that is not a bool is a 422 (recorded)
	if r := e.call("alice", "", "POST", "/api/automations/triggers/trg-a-01/run", `{"dry_run":"maybe"}`); r.Status != 422 {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
}

func TestRunRequiresAdmin(t *testing.T) {
	e := newArunEnv(t, true, nil)
	want := `{"detail":"Workspace permission denied: automations.manage"}`
	for _, c := range []struct{ who, ws string }{{"bob", "ws-alpha"}, {"erin", ""}} {
		before := e.worldState(t)
		r := e.call(c.who, c.ws, "POST", "/api/automations/triggers/trg-a-01/run", `{}`)
		if r.Status != 403 || strings.TrimSpace(string(r.Body)) != want {
			t.Fatalf("%s: %d %s", c.who, r.Status, r.Body)
		}
		// 403 comes before body validation (422) and nothing was enqueued
		r = e.call(c.who, c.ws, "POST", "/api/automations/triggers/trg-a-01/run", `{"dry_run":"maybe"}`)
		if r.Status != 403 {
			t.Fatalf("%s invalid body: %d", c.who, r.Status)
		}
		if e.worldState(t) != before {
			t.Fatalf("%s: a forbidden run changed state", c.who)
		}
	}
	// replayed recorded gates
	for _, n := range arunNames(t, "run_member_forbidden", "run_editor_forbidden", "run_no_auth", "run_no_body", "run_unknown", "run_foreign_ws") {
		e.replay(t, n)
	}
}

func TestAutomationRunMatchesFixtures(t *testing.T) {
	e := newArunEnv(t, true, nil)
	n := 0
	for _, name := range arunNames(t, "run_") {
		if strings.HasPrefix(name, "run_detail_") {
			continue
		}
		e.replay(t, name, contract.IgnorePaths("$.job_id"))
		n++
	}
	if n < 15 {
		t.Fatalf("expected the recorded run fixtures, got %d", n)
	}
}

func TestRunsLimitBounds422(t *testing.T) {
	e := newArunEnv(t, true, nil)
	for _, n := range arunNames(t, "runs_") {
		e.replay(t, n)
	}
	for _, q := range []string{"201", "abc", "-1"} {
		r := e.call("alice", "", "GET", "/api/automations/triggers/trg-a-01/runs?limit="+q, "")
		if r.Status != 422 {
			t.Errorf("limit=%s: %d %s", q, r.Status, r.Body)
		}
	}
	for _, q := range []string{"0", "1", "200"} {
		if r := e.call("alice", "", "GET", "/api/automations/triggers/trg-a-01/runs?limit="+q, ""); r.Status != 200 {
			t.Errorf("limit=%s: %d %s", q, r.Status, r.Body)
		}
	}
}

func TestRunDetailMatchesFixture(t *testing.T) {
	e := newArunEnv(t, true, nil)
	names := arunNames(t, "run_detail_")
	if len(names) < 8 {
		t.Fatalf("expected the recorded run detail fixtures, got %d", len(names))
	}
	for _, n := range names {
		e.replay(t, n)
	}
}

func TestRunDetailForeign404(t *testing.T) {
	e := newArunEnv(t, true, nil)
	want := `{"detail":"run not found"}`
	r := e.call("alice", "", "GET", "/api/automations/runs/run-b-01-01", "")
	if r.Status != 404 || strings.TrimSpace(string(r.Body)) != want {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
	r = e.call("bob", "", "GET", "/api/automations/runs/run-a-01-01", "")
	if r.Status != 404 && r.Status != 403 {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
}

func TestDisabledFlag404(t *testing.T) {
	e := newArunEnv(t, false, nil)
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/api/automations/triggers/trg-a-01/preview", `{}`},
		{"POST", "/api/automations/triggers/trg-a-01/run", `{}`},
		{"GET", "/api/automations/triggers/trg-a-01/runs", ""},
		{"GET", "/api/automations/runs/run-a-01-01", ""},
	} {
		r := e.call("alice", "", c.method, c.path, c.body)
		if r.Status != 404 || strings.TrimSpace(string(r.Body)) != `{"detail":"automations disabled"}` {
			t.Errorf("%s %s: %d %s", c.method, c.path, r.Status, r.Body)
		}
	}
	for _, n := range arunNames(t, "off_automations_") {
		e.replay(t, n)
	}
}
