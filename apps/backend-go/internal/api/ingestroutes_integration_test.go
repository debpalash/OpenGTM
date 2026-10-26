//go:build integration

package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/automationhook"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

// seedIngestToken is the TEST-ONLY plaintext contract_seed.py stores (only its sha256 is in the table).
const seedIngestToken = "wbi_contract_ingest_token_alpha_0001"

type ingestEnv struct {
	*migrateEnv
	ingestOn bool
}

type hookCall struct {
	ws, wb string
	ids    []int64
	inTx   bool
}

type fakeHook struct {
	mu    sync.Mutex
	calls []hookCall
}

func (f *fakeHook) RowsAdded(_ context.Context, tx pgx.Tx, ws, wb string, ids []int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, hookCall{ws, wb, append([]int64(nil), ids...), tx != nil})
	return 0, nil
}

func (f *fakeHook) SignalsInserted(context.Context, pgx.Tx, string, []automationhook.SignalRef) (int, error) {
	return 0, nil
}

func newIngestEnv(t *testing.T, enabled bool) (*ingestEnv, *fakeHook) {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	st := identity.NewStore(pool)
	hook := &fakeHook{}
	prev := ingestHookFor
	ingestHookFor = func(Deps) automationhook.Hook { return hook }
	t.Cleanup(func() { ingestHookFor = prev })
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(pool, nil))
	d := Deps{Pool: pool, Signer: testSigner, Identity: st, Config: config.APIConfig{}, Log: quietLog(),
		Flags: config.Flags{PGLeadStore: true, GoRunRoutesEnabled: true, IngestAPIEnabled: enabled}}
	registerIngest(r, d)
	m := &migrateEnv{leadsEnv{mcpEnv{env: env0(r, st), pool: pool}}}
	m.purge()
	m.seedLeads(t)
	m.seed(t, "erin", pythonHash, true, "user", false)
	m.member(t, "ws-alpha", m.mustUserByName(t, "erin").ID, "editor")
	m.clean()
	t.Cleanup(m.clean)
	t.Cleanup(m.purge)
	m.seedWorkbooks(t)
	e := &ingestEnv{migrateEnv: m, ingestOn: enabled}
	e.cleanIngest()
	t.Cleanup(e.cleanIngest)
	e.seedEnrich(t)
	return e, hook
}

func (e *ingestEnv) cleanIngest() {
	for _, q := range []string{
		`DELETE FROM workbook_ingest_idempotency WHERE workspace_id IN ('ws-alpha','ws-beta')`,
		`DELETE FROM workbook_ingest_tokens WHERE workspace_id IN ('ws-alpha','ws-beta')`,
	} {
		ownerCleanup(q)
	}
}

// seedEnrich mirrors the ENRICH part of contract_seed.py that ingest depends on: the six wb-enrich-1 rows
// (example.com sites drive the registrable-domain collapse), wb-enrich-live, wb-enrich-big, the seed token.
func (e *ingestEnv) seedEnrich(t *testing.T) {
	t.Helper()
	ownerExec(t, `DELETE FROM workbook_rows WHERE workbook_id = 'wb-enrich-1'`)
	sites := []string{"https://globex.example.com", "https://initech.example.com", "", "https://umbrella.example.com",
		"https://hooli.example.com", "https://stark.example.com"}
	companies := []string{"Globex", "Initech", "Soylent", "Umbrella", "Hooli", "Stark Industries"}
	for k := 0; k < 6; k++ {
		d, _ := json.Marshal(map[string]string{"company": companies[k], "website": sites[k]})
		ownerExec(t, `INSERT INTO workbook_rows (id, workbook_id, "position", data, enrichments, workspace_id) VALUES ($1,'wb-enrich-1',$2,$3::json,'{}'::json,'ws-alpha')`, 101+k, k, string(d))
	}
	ins := `INSERT INTO workbooks (id, name, description, status, workspace_id, source_type, source_config, filter_criteria,
		columns_config, total_rows, completed_rows, sync_to_leads, budget_max_usd, budget_spent_usd, created_at, updated_at)
		VALUES ($1,$1,'','idle','ws-alpha','manual','{}'::json,NULL,'[{"id":"company","name":"Company","type":"lead_field","lead_field":"company"}]'::json,0,0,false,0,0,$2::timestamp,$2::timestamp)`
	ownerExec(t, ins, "wb-enrich-live", staleStamp)
	ownerExec(t, ins, "wb-enrich-big", staleStamp)
	ownerExec(t, ins, "wb-alpha-2", staleStamp)
	ownerExec(t, `UPDATE workbooks SET total_rows = 6 WHERE id = 'wb-enrich-1'`)
	e.insertToken(t, "ingest-tok-1", "ws-alpha", "wb-enrich-1", seedIngestToken)
	ownerExec(t, `SELECT setval(pg_get_serial_sequence('workbook_rows','id'), 1250)`)
}

func (e *ingestEnv) insertToken(t *testing.T, id, ws, wb, plain string) {
	t.Helper()
	h := sha256.Sum256([]byte(plain))
	ownerExec(t, `INSERT INTO workbook_ingest_tokens (id, workspace_id, workbook_id, token_hash, prefix, created_by) VALUES ($1,$2,$3,$4,$5,1)`,
		id, ws, wb, hex.EncodeToString(h[:]), plain[:12])
}

func (e *ingestEnv) post(path, body string, hdr map[string]string) (int, string) {
	rec, _ := e.do("POST", path, "application/json", body, hdr)
	return rec.Code, rec.Body.String()
}

func (e *ingestEnv) session(who, ws string, extra ...string) map[string]string {
	h := bearer(who)
	h["X-Workspace-Id"] = ws
	for i := 0; i+1 < len(extra); i += 2 {
		h[extra[i]] = extra[i+1]
	}
	return h
}

func tokHdr(tok string, extra ...string) map[string]string {
	h := map[string]string{"Authorization": "Bearer " + tok}
	for i := 0; i+1 < len(extra); i += 2 {
		h[extra[i]] = extra[i+1]
	}
	return h
}

const ingestBase = "/api/v2/workbooks/"

func (e *ingestEnv) count(t *testing.T, wb string) string {
	return dbString(t, `SELECT count(*)::text FROM workbook_rows WHERE workbook_id = $1`, wb)
}

// ingestFixtureOrder is the recording order of scripts/contract_cases/enrichment.yaml (default profile).
var ingestFixtureOrder = []string{
	"ingest_session", "ingest_session_dupes", "ingest_registrable_domain_collapse", "ingest_session_no_dedupe",
	"ingest_token", "ingest_idempotent_first", "ingest_idempotent_replay", "ingest_idempotent_session",
	"ingest_rows_empty", "ingest_rows_missing", "ingest_too_many", "ingest_max_rows_ok",
	"ingest_no_credentials", "ingest_bad_token", "ingest_token_wrong_workbook", "ingest_viewer", "ingest_foreign",
	"ingest_unknown_workbook", "ingest_token_create_viewer", "ingest_token_create_foreign", "ingest_token_create_unknown",
	"ingest_token_create_no_token", "ingest_token_create", "ingest_token_rotate", "ingest_token_revoked",
	"ingest_token_create_other_workbook",
}

func ingestActor(name string) (hdr map[string]string) {
	switch name {
	case "ingest_token", "ingest_idempotent_first", "ingest_idempotent_replay", "ingest_token_revoked":
		return tokHdr(seedIngestToken)
	case "ingest_bad_token":
		return tokHdr("wbi_not_a_real_token_000000000000")
	case "ingest_token_wrong_workbook":
		return tokHdr(seedIngestToken)
	case "ingest_no_credentials", "ingest_token_create_no_token":
		return map[string]string{}
	case "ingest_viewer", "ingest_token_create_viewer":
		h := bearer("bob")
		h["X-Workspace-Id"] = "ws-alpha"
		return h
	case "ingest_foreign", "ingest_token_create_foreign":
		h := bearer("bob")
		h["X-Workspace-Id"] = "ws-beta"
		return h
	case "ingest_token_rotate":
		h := bearer("erin")
		h["X-Workspace-Id"] = "ws-alpha"
		return h
	}
	h := bearer("alice")
	h["X-Workspace-Id"] = "ws-alpha"
	return h
}

func replayIngest(t *testing.T, e *ingestEnv, name string) {
	t.Helper()
	fx := contract.Load(t, "ingest/"+name)
	hdr := ingestActor(name)
	if k, ok := fx.Request.Headers["Idempotency-Key"]; ok {
		hdr["Idempotency-Key"] = k
	}
	rec, got := e.do(fx.Request.Method, fx.Request.Path, "application/json", string(fx.Request.Body), hdr)
	if name == "ingest_idempotent_replay" && rec.Header().Get("Idempotent-Replay") != "true" {
		t.Errorf("%s: missing Idempotent-Replay header", name)
	}
	var shape []string
	var want struct {
		RowIDs []any `json:"row_ids"`
	}
	_ = json.Unmarshal(fx.Response.Body, &want)
	for i := range want.RowIDs {
		shape = append(shape, fmt.Sprintf("$.row_ids[%d]", i))
	}
	if strings.HasPrefix(name, "ingest_token_create") || name == "ingest_token_rotate" {
		shape = append(shape, "$.id", "$.prefix", "$.created_at", "$.token")
	}
	contract.AssertEqual(t, got, fx.Response, contract.IgnoreShapeOf(shape...))
}

func TestIngestTokenMatchesFixtures(t *testing.T) {
	e, _ := newIngestEnv(t, true)
	if n := len(contractFixtureNames(t, "ingest")); n != len(ingestFixtureOrder)+2 {
		t.Fatalf("found %d ingest fixtures, want %d", n, len(ingestFixtureOrder)+2)
	}
	for _, name := range ingestFixtureOrder {
		t.Run(name, func(t *testing.T) { replayIngest(t, e, name) })
	}
	off, _ := newIngestEnv(t, false)
	for _, name := range []string{"ingest_disabled", "ingest_token_disabled"} {
		t.Run(name, func(t *testing.T) {
			fx := contract.Load(t, "ingest/"+name)
			_, got := off.do(fx.Request.Method, fx.Request.Path, "application/json", string(fx.Request.Body), ingestActor(name))
			contract.AssertEqual(t, got, fx.Response)
		})
	}
}

func TestIngestTokenCreateResponseOnce(t *testing.T) {
	e, _ := newIngestEnv(t, true)
	code, body := e.post(ingestBase+"wb-enrich-live/ingest-token", "", e.session("alice", "ws-alpha"))
	if code != 200 {
		t.Fatalf("create: %d %s", code, body)
	}
	var r struct {
		Token, ID, Prefix, WorkbookID, WorkspaceID string
		CreatedAt                                  string `json:"created_at"`
		Rotated                                    int
	}
	_ = json.Unmarshal([]byte(body), &r)
	if !strings.HasPrefix(r.Token, "wbi_") || len(r.Token) != 4+43 || r.Prefix != r.Token[:12] || r.Rotated != 0 ||
		r.WorkbookID != "wb-enrich-live" || r.WorkspaceID != "ws-alpha" {
		t.Fatalf("bad create response shape: %+v", r)
	}
	// only the sha256 is stored; the plaintext appears nowhere in the table.
	h := sha256.Sum256([]byte(r.Token))
	if dbString(t, `SELECT token_hash FROM workbook_ingest_tokens WHERE id = $1`, r.ID) != hex.EncodeToString(h[:]) {
		t.Error("stored hash is not sha256(plaintext)")
	}
	if dbString(t, `SELECT count(*)::text FROM workbook_ingest_tokens WHERE token_hash = $1 OR prefix = $1`, r.Token) != "0" {
		t.Error("plaintext stored")
	}
}

func TestIngestTokenRotationRevokesPrevious(t *testing.T) {
	e, _ := newIngestEnv(t, true)
	mint := func() string {
		code, body := e.post(ingestBase+"wb-enrich-live/ingest-token", "", e.session("alice", "ws-alpha"))
		if code != 200 {
			t.Fatalf("mint: %d", code)
		}
		var r struct{ Token string }
		_ = json.Unmarshal([]byte(body), &r)
		return r.Token
	}
	a := mint()
	row := `{"rows":[{"company":"Rot %s"}]}`
	if code, body := e.post(ingestBase+"wb-enrich-live/rows/ingest", fmt.Sprintf(row, "A"), tokHdr(a)); code != 200 {
		t.Fatalf("token A: %d %s", code, body)
	}
	b := mint()
	code, body := e.post(ingestBase+"wb-enrich-live/rows/ingest", fmt.Sprintf(row, "A2"), tokHdr(a))
	if code != 401 || body != `{"detail":"ingest token revoked"}` {
		t.Fatalf("revoked token A: %d %s", code, body)
	}
	if code, body := e.post(ingestBase+"wb-enrich-live/rows/ingest", fmt.Sprintf(row, "B"), tokHdr(b)); code != 200 {
		t.Fatalf("token B: %d %s", code, body)
	}
	if n := dbString(t, `SELECT count(*)::text FROM workbook_ingest_tokens WHERE workbook_id='wb-enrich-live' AND revoked_at IS NULL`); n != "1" {
		t.Errorf("active tokens = %s, want 1", n)
	}
	// the revoked token's second ingest wrote nothing.
	if n := dbString(t, `SELECT count(*)::text FROM workbook_rows WHERE workbook_id='wb-enrich-live' AND data->>'company'='Rot A2'`); n != "0" {
		t.Error("revoked token wrote a row")
	}
}

func TestIngestWithSessionNeedsTablesWrite(t *testing.T) {
	e, _ := newIngestEnv(t, true)
	body := `{"rows":[{"company":"Session Co"}]}`
	code, resp := e.post(ingestBase+"wb-enrich-live/rows/ingest", body, e.session("bob", "ws-alpha"))
	if code != 403 || resp != `{"detail":"Insufficient workspace role"}` {
		t.Fatalf("viewer: %d %s", code, resp)
	}
	if n := e.count(t, "wb-enrich-live"); n != "0" {
		t.Errorf("viewer wrote rows: %s", n)
	}
	if code, resp := e.post(ingestBase+"wb-enrich-live/rows/ingest", body, e.session("erin", "ws-alpha")); code != 200 {
		t.Fatalf("editor: %d %s", code, resp)
	}
	// a viewer cannot mint a token either (require_editor gate).
	code, resp = e.post(ingestBase+"wb-enrich-live/ingest-token", "", e.session("bob", "ws-alpha"))
	if code != 403 || resp != `{"detail":"Workspace permission denied: tables.write"}` {
		t.Fatalf("viewer mint: %d %s", code, resp)
	}
}

func TestIngestWithTokenOtherWorkbookRejected(t *testing.T) {
	e, hook := newIngestEnv(t, true)
	code, body := e.post(ingestBase+"wb-enrich-live/rows/ingest", `{"rows":[{"company":"Other"}]}`, tokHdr(seedIngestToken))
	if code != 401 || body != `{"detail":"ingest token not valid for this workbook"}` {
		t.Fatalf("%d %s", code, body)
	}
	if e.count(t, "wb-enrich-live") != "0" || len(hook.calls) != 0 {
		t.Error("rows written or hook fired for a mis-scoped token")
	}
}

func TestIngestMissingCredentials401(t *testing.T) {
	e, _ := newIngestEnv(t, true)
	body := `{"rows":[{"company":"x"}]}`
	for name, hdr := range map[string]map[string]string{
		"none":         {},
		"blank bearer": {"Authorization": "Bearer "},
		"basic":        {"Authorization": "Basic abc"},
	} {
		code, resp := e.post(ingestBase+"wb-enrich-live/rows/ingest", body, hdr)
		if code != 401 || resp != `{"detail":"Missing credentials"}` {
			t.Errorf("%s: %d %s", name, code, resp)
		}
	}
	// X-Ingest-Token is the alternative carrier.
	code, resp := e.post(ingestBase+"wb-enrich-1/rows/ingest", `{"rows":[{"company":"Header Carrier"}]}`, map[string]string{"X-Ingest-Token": seedIngestToken})
	if code != 200 {
		t.Errorf("x-ingest-token: %d %s", code, resp)
	}
	// a bad session JWT is a 401 from the session path, never silently a token attempt.
	code, _ = e.post(ingestBase+"wb-enrich-1/rows/ingest", body, map[string]string{"Authorization": "Bearer not.a.jwt"})
	if code != 401 {
		t.Errorf("bad jwt: %d", code)
	}
}

func TestIngestTooManyRows413(t *testing.T) {
	e, hook := newIngestEnv(t, true)
	mk := func(n int) string {
		var b strings.Builder
		b.WriteString(`{"rows":[`)
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `{"company":"Cap Co %d"}`, i)
		}
		b.WriteString(`]}`)
		return b.String()
	}
	code, resp := e.post(ingestBase+"wb-enrich-live/rows/ingest", mk(501), e.session("alice", "ws-alpha"))
	if code != 413 || resp != `{"detail":"too many rows: 501 > 500 per request"}` {
		t.Fatalf("%d %s", code, resp)
	}
	if e.count(t, "wb-enrich-live") != "0" {
		t.Error("rows inserted by a rejected request")
	}
	if code, resp := e.post(ingestBase+"wb-enrich-live/rows/ingest", mk(500), tokHdr(mustMint(t, e))); code != 200 {
		t.Fatalf("500 rows: %d %.200s", code, resp)
	}
	if e.count(t, "wb-enrich-live") != "500" || len(hook.calls) != 1 || len(hook.calls[0].ids) != 500 {
		t.Errorf("500 rows: count=%s hook=%+v", e.count(t, "wb-enrich-live"), len(hook.calls))
	}
}

func mustMint(t *testing.T, e *ingestEnv) string {
	t.Helper()
	code, body := e.post(ingestBase+"wb-enrich-live/ingest-token", "", e.session("alice", "ws-alpha"))
	if code != 200 {
		t.Fatalf("mint %d", code)
	}
	var r struct{ Token string }
	_ = json.Unmarshal([]byte(body), &r)
	return r.Token
}

func TestIngestMapsAndDedupes(t *testing.T) {
	e, hook := newIngestEnv(t, true)
	hdr := e.session("alice", "ws-alpha")
	body := `{"rows":[
		{"Company":"Acme","WEBSITE":"https://www.acme.io/x","Zed":1,"alpha key":2},
		{"company":"Acme Again","website":"http://acme.io"},
		{"company":"Dup Name Inc"},{"company":"dup name llc"},
		{"company":""},{"company":null},{}]}`
	code, resp := e.post(ingestBase+"wb-enrich-live/rows/ingest", body, hdr)
	if code != 200 {
		t.Fatalf("%d %s", code, resp)
	}
	var r struct {
		Added             int      `json:"added"`
		SkippedDuplicates int      `json:"skipped_duplicates"`
		SkippedEmpty      int      `json:"skipped_empty"`
		TotalRows         int      `json:"total_rows"`
		RowIDs            []int64  `json:"row_ids"`
		UnmappedKeys      []string `json:"unmapped_keys"`
	}
	_ = json.Unmarshal([]byte(resp), &r)
	if r.Added != 2 || r.SkippedDuplicates != 2 || r.SkippedEmpty != 3 || r.TotalRows != 2 || len(r.RowIDs) != 2 {
		t.Fatalf("counts: %s", resp)
	}
	if strings.Join(r.UnmappedKeys, "|") != "Zed|alpha key" { // sorted, original spelling kept
		t.Errorf("unmapped_keys = %v", r.UnmappedKeys)
	}
	if got := dbString(t, `SELECT data::text FROM workbook_rows WHERE id = $1`, r.RowIDs[0]); got != `{"company": "Acme", "website": "https://www.acme.io/x", "Zed": 1, "alpha key": 2}` {
		t.Errorf("stored row = %s", got)
	}
	if dbString(t, `SELECT total_rows::text FROM workbooks WHERE id='wb-enrich-live'`) != "2" {
		t.Error("workbooks.total_rows not updated")
	}
	if len(hook.calls) != 1 || !hook.calls[0].inTx || hook.calls[0].ws != "ws-alpha" || hook.calls[0].wb != "wb-enrich-live" ||
		fmt.Sprint(hook.calls[0].ids) != fmt.Sprint(r.RowIDs) {
		t.Errorf("RowsAdded hook calls = %+v, row ids %v", hook.calls, r.RowIDs)
	}
	// dedupe=false keeps repeats; nothing added -> no hook call.
	code, resp = e.post(ingestBase+"wb-enrich-live/rows/ingest", `{"rows":[{"company":"Acme"}],"dedupe":false}`, hdr)
	if code != 200 || !strings.Contains(resp, `"added":1`) {
		t.Errorf("dedupe=false: %d %s", code, resp)
	}
	n := len(hook.calls)
	e.post(ingestBase+"wb-enrich-live/rows/ingest", `{"rows":[{"company":""}]}`, hdr)
	if len(hook.calls) != n {
		t.Error("hook fired for an ingest that added nothing")
	}
}

func TestIngestIdempotencyReplayAndRace(t *testing.T) {
	e, hook := newIngestEnv(t, true)
	hdr := e.session("alice", "ws-alpha", "Idempotency-Key", "k-1")
	body := `{"rows":[{"company":"Once Only"}]}`
	_, first := e.post(ingestBase+"wb-enrich-live/rows/ingest", body, hdr)
	rec, _ := e.do("POST", ingestBase+"wb-enrich-live/rows/ingest", "application/json", body, hdr)
	if rec.Body.String() != first || rec.Header().Get("Idempotent-Replay") != "true" {
		t.Errorf("replay = %q (%q), first %q", rec.Body.String(), rec.Header().Get("Idempotent-Replay"), first)
	}
	// concurrent callers with one key insert once.
	var wg sync.WaitGroup
	race := e.session("alice", "ws-alpha", "Idempotency-Key", "k-race")
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.post(ingestBase+"wb-enrich-live/rows/ingest", `{"rows":[{"company":"Racer"}],"dedupe":false}`, race)
		}()
	}
	wg.Wait()
	if n := dbString(t, `SELECT count(*)::text FROM workbook_rows WHERE workbook_id='wb-enrich-live' AND data->>'company'='Racer'`); n != "1" {
		t.Errorf("racing idempotent ingests inserted %s rows", n)
	}
	if len(hook.calls) != 2 {
		t.Errorf("hook calls = %d, want 2 (one per applied ingest)", len(hook.calls))
	}
}

func TestIngestDisabledFlagResponse(t *testing.T) {
	e, hook := newIngestEnv(t, false)
	for _, hdr := range []map[string]string{e.session("alice", "ws-alpha"), tokHdr(seedIngestToken), {}} {
		code, resp := e.post(ingestBase+"wb-enrich-1/rows/ingest", `{"rows":[{"company":"x"}]}`, hdr)
		if hdr["Authorization"] == "" {
			if code != 401 && code != 404 {
				t.Errorf("no creds: %d", code)
			}
			continue
		}
		if code != 404 || resp != `{"detail":"ingest api disabled"}` {
			t.Errorf("rows: %d %s", code, resp)
		}
	}
	code, resp := e.post(ingestBase+"wb-enrich-1/ingest-token", "", e.session("alice", "ws-alpha"))
	if code != 404 || resp != `{"detail":"ingest api disabled"}` {
		t.Errorf("token: %d %s", code, resp)
	}
	if e.count(t, "wb-enrich-1") != "6" || len(hook.calls) != 0 ||
		dbString(t, `SELECT count(*)::text FROM workbook_ingest_tokens`) != "1" {
		t.Error("disabled flag still changed state")
	}
}

func TestIngestTenantIsolation(t *testing.T) {
	e, _ := newIngestEnv(t, true)
	// a ws-beta workbook with its own token.
	e.insertToken(t, "beta-tok", "ws-beta", "wb-beta-1", "wbi_beta_tenant_token_0000000000001")
	// beta's token cannot reach alpha's workbook, alpha's cannot reach beta's.
	for _, c := range []struct{ tok, wb string }{{"wbi_beta_tenant_token_0000000000001", "wb-enrich-live"}, {seedIngestToken, "wb-beta-1"}} {
		code, resp := e.post(ingestBase+c.wb+"/rows/ingest", `{"rows":[{"company":"Leak"}]}`, tokHdr(c.tok))
		if code != 401 || resp != `{"detail":"ingest token not valid for this workbook"}` {
			t.Errorf("%s: %d %s", c.wb, code, resp)
		}
	}
	// beta's token writes into beta only, stamped with beta's workspace.
	if code, resp := e.post(ingestBase+"wb-beta-1/rows/ingest", `{"rows":[{"company":"Beta Co"}]}`, tokHdr("wbi_beta_tenant_token_0000000000001")); code != 200 {
		t.Fatalf("beta ingest: %d %s", code, resp)
	}
	if dbString(t, `SELECT workspace_id FROM workbook_rows WHERE workbook_id='wb-beta-1'`) != "ws-beta" {
		t.Error("row not stamped with the token's workspace")
	}
	// a session in ws-beta sees alpha's workbook as missing, and can mint nothing for it.
	if code, resp := e.post(ingestBase+"wb-enrich-1/rows/ingest", `{"rows":[{"company":"Leak"}]}`, e.session("bob", "ws-beta")); code != 404 || resp != `{"detail":"Workbook not found"}` {
		t.Errorf("session foreign: %d %s", code, resp)
	}
	if code, _ := e.post(ingestBase+"wb-enrich-1/ingest-token", "", e.session("bob", "ws-beta")); code != 404 {
		t.Errorf("mint foreign: %d", code)
	}
	if dbString(t, `SELECT count(*)::text FROM workbook_rows WHERE data->>'company'='Leak'`) != "0" {
		t.Error("cross-tenant row written")
	}
	if dbString(t, `SELECT count(*)::text FROM workbook_ingest_tokens WHERE workbook_id='wb-enrich-1'`) != "1" {
		t.Error("cross-tenant token minted")
	}
}

func ownerCleanup(q string) {
	c, err := pgx.Connect(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		return
	}
	_, _ = c.Exec(context.Background(), q)
	_ = c.Close(context.Background())
}
