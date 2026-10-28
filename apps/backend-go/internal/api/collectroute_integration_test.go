//go:build integration

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadprogress"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

type collectEnv struct{ mcpEnv }

func newCollectEnv(t *testing.T, mut ...func(*Deps)) collectEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	st := identity.NewStore(pool)
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(pool, nil))
	d := Deps{Pool: pool, Signer: testSigner, Identity: st, Limiter: frozenLimiter(),
		Flags: config.Flags{PGLeadStore: true, Leadgen: config.LeadgenSettings{GoCollectionRoutesEnabled: true, CollectQueryMaxChars: 1000}}}
	for _, m := range mut {
		m(&d)
	}
	RegisterCollect(r, d)
	e := collectEnv{mcpEnv{env: env{h: r.Handler(), st: st}, pool: pool}}
	e.purge(t)
	t.Cleanup(func() {
		e.purge(t)
		collectNewID, collectEnqueue, collectBus = nil, nil, nil
	})
	e.seed(t, "alice", pythonHash, true, "admin", true)
	e.seed(t, "bob", pythonHash, true, "user", false)
	e.seed(t, "erin", pythonHash, true, "user", false)
	alice, bob, erin := e.mustUserByName(t, "alice"), e.mustUserByName(t, "bob"), e.mustUserByName(t, "erin")
	e.workspace(t, "ws-alpha", alice.ID)
	e.workspace(t, "ws-beta", bob.ID)
	ownerExec(t, `UPDATE workspaces SET slug = 'alpha' WHERE id = 'ws-alpha'`)
	e.member(t, "ws-alpha", alice.ID, "owner")
	e.member(t, "ws-alpha", bob.ID, "member") // the viewer
	e.member(t, "ws-alpha", erin.ID, "editor")
	e.member(t, "ws-beta", bob.ID, "owner")
	return e
}

func (e collectEnv) purge(t *testing.T) {
	t.Helper()
	ownerExec(t, `DELETE FROM collection_job_stages WHERE job_id IN (SELECT id FROM collection_jobs WHERE workspace_id IN ('ws-alpha','ws-beta'))`)
	ownerExec(t, `DELETE FROM collection_jobs WHERE workspace_id IN ('ws-alpha','ws-beta')`)
	ownerExec(t, `DELETE FROM jobs WHERE type = 'collect' AND workspace_id IN ('ws-alpha','ws-beta')`)
}

func (e collectEnv) post(who, ws, body string) *httptest.ResponseRecorder {
	hdr := map[string]string{}
	if who != "" {
		hdr = bearer(who)
	}
	if ws != "" {
		hdr["X-Workspace-Id"] = ws
	}
	rec, _ := e.do("POST", "/api/collect", "application/json", body, hdr)
	return rec
}

func ownerCount(t *testing.T, sql string, args ...any) int {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	var n int
	if err := c.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e collectEnv) rows(t *testing.T) (ledger, queue int) {
	t.Helper()
	return ownerCount(t, `SELECT count(*) FROM collection_jobs WHERE workspace_id IN ('ws-alpha','ws-beta')`),
		ownerCount(t, `SELECT count(*) FROM jobs WHERE type='collect' AND workspace_id IN ('ws-alpha','ws-beta')`)
}

// collectActor says who sent a recorded request (the recorder redacts Authorization and X-Workspace-Id).
func collectActor(name string) (who, ws string) {
	switch {
	case name == "collect_no_auth_401":
		return "", ""
	case name == "collect_viewer_403":
		return "bob", "ws-alpha"
	case strings.HasPrefix(name, "collect_rl_"), name == "collect_rate_limited_429":
		return "bob", "ws-beta"
	case name == "collect_trimmed_query", name == "collect_explicit_market_intent":
		return "erin", ""
	}
	return "alice", ""
}

// replay posts a fixture. Collected job ids are the recorder's counter uuids and queue ids its serial values:
// the id generator returns the recorded job id and the recorded queue id replaces ours after it was checked
// against the queue row.
func (e collectEnv) replay(t *testing.T, name string, fx contract.Fixture) contract.Resp {
	t.Helper()
	var wantBody map[string]any
	_ = json.Unmarshal(fx.Response.Body, &wantBody)
	if id, _ := wantBody["job_id"].(string); id != "" {
		collectNewID = func() string { return id }
	}
	who, ws := collectActor(name)
	var rec *httptest.ResponseRecorder
	if who == "" {
		rec, _ = e.do("POST", "/api/collect", "application/json", string(fx.Request.Body), nil)
	} else {
		rec = e.post(who, ws, string(fx.Request.Body))
	}
	got := contract.Resp{Status: rec.Code, Headers: map[string]string{}, Body: json.RawMessage(rec.Body.Bytes())}
	for _, k := range []string{"Content-Type", "WWW-Authenticate", "Allow"} {
		if v := rec.Header().Get(k); v != "" {
			got.Headers[k] = v
		}
	}
	if rec.Code == 200 {
		var m map[string]any
		dec := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
		dec.UseNumber()
		if err := dec.Decode(&m); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if qid, ok := m["queue_job_id"].(json.Number); ok {
			if n := ownerCount(t, `SELECT count(*) FROM jobs WHERE id = $1::int AND type='collect' AND fire_key = $2`, qid.String(),
				fmt.Sprintf("collect:%s:%s", m["workspace_id"], m["job_id"])); n != 1 {
				t.Errorf("%s: queue_job_id %s is not the queue row of the job", name, qid)
			}
			m["queue_job_id"] = wantBody["queue_job_id"]
			b, _ := json.Marshal(m)
			got.Body = b
		}
	}
	return got
}

func TestCollectMatchesFixtures(t *testing.T) {
	e := newCollectEnv(t)
	replayed := 0
	for _, full := range contractFixtureNames(t, "collect") {
		name := strings.TrimPrefix(full, "collect/")
		if name == "collect_long_query" || strings.HasPrefix(name, "collect_rl_") || name == "collect_rate_limited_429" {
			continue // 1260 chars: Go answers 422 (deviation D2, TestCollectQueryTooLong422); rate limit: TestCollectRateLimit429
		}
		fx := contract.Load(t, full)
		got := e.replay(t, name, fx)
		replayed++
		t.Run(name, func(t *testing.T) { contract.AssertEqual(t, got, fx.Response) })
	}
	if replayed != 23 {
		t.Fatalf("%d collect fixtures replayed", replayed)
	}
}

func TestCollectEmptyQuery400(t *testing.T) {
	e := newCollectEnv(t)
	for _, q := range []string{`""`, `"   "`, `"\u00a0\u2003\t\n"`} { // Python str.strip() removes Unicode whitespace
		rec := e.post("alice", "", `{"query":`+q+`}`)
		if rec.Code != 400 || rec.Body.String() != `{"detail":"query is required"}` {
			t.Errorf("%s: %d %s", q, rec.Code, rec.Body.String())
		}
	}
	if l, q := e.rows(t); l != 0 || q != 0 {
		t.Errorf("rows written: ledger %d queue %d", l, q)
	}
}

func TestCollectClarificationWritesNothing(t *testing.T) {
	e := newCollectEnv(t)
	rec := e.post("alice", "", `{"query":"stripe.com"}`)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// Python key order: ok, the decision's to_dict() order, then message.
	if !strings.HasPrefix(body, `{"ok":false,"query":"stripe.com","intent":`) || !strings.Contains(body, `"clarification_required":true`) ||
		!strings.Contains(body, `,"options":[`) || !strings.HasSuffix(body, `"}`) || strings.LastIndex(body, `"message":`) < strings.Index(body, `"options":`) {
		t.Errorf("body %s", body)
	}
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	if m["message"] != m["reason"] || m["message"] == "" {
		t.Errorf("message %v reason %v", m["message"], m["reason"])
	}
	if l, q := e.rows(t); l != 0 || q != 0 {
		t.Errorf("clarification wrote rows: ledger %d queue %d", l, q)
	}
}

func TestCollectViewer403(t *testing.T) {
	e := newCollectEnv(t)
	rec := e.post("bob", "ws-alpha", `{"query":"IT staffing Pune"}`)
	if rec.Code != 403 || rec.Body.String() != `{"detail":"Workspace permission denied: tables.write"}` {
		t.Errorf("%d %s", rec.Code, rec.Body.String())
	}
	if l, q := e.rows(t); l != 0 || q != 0 {
		t.Errorf("rows written: ledger %d queue %d", l, q)
	}
}

func TestCollectRateLimit429(t *testing.T) {
	e := newCollectEnv(t)
	// validation failures (422) never reach the limited handler body in slowapi: they do not count
	for _, b := range []string{`{}`, `{"query":123}`, `["q"]`} {
		if rec := e.post("bob", "ws-beta", b); rec.Code != 422 {
			t.Fatalf("%s: %d", b, rec.Code)
		}
	}
	for i := 1; i <= 20; i++ {
		name := fmt.Sprintf("collect_rl_%02d", i)
		fx := contract.Load(t, "collect/"+name)
		got := e.replay(t, name, fx)
		contract.AssertEqual(t, got, fx.Response)
	}
	fx := contract.Load(t, "collect/collect_rate_limited_429")
	contract.AssertEqual(t, e.replay(t, "collect_rate_limited_429", fx), fx.Response)
	if n := ownerCount(t, `SELECT count(*) FROM collection_jobs WHERE workspace_id = 'ws-beta'`); n != 20 {
		t.Errorf("%d ledger rows in ws-beta, want 20 (the 429 must write nothing)", n)
	}
	// the bucket is per workspace key: alpha is untouched
	if rec := e.post("alice", "ws-alpha", `{"query":"payroll firms Delhi"}`); rec.Code != 200 {
		t.Errorf("alpha: %d", rec.Code)
	}
}

func TestCollectBodyWorkspaceIgnored(t *testing.T) {
	e := newCollectEnv(t)
	rec := e.post("alice", "", `{"query":"recruiters in Chennai","workspace_id":"ws-beta"}`)
	var m map[string]any
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &m) != nil || m["workspace_id"] != "ws-alpha" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if n := ownerCount(t, `SELECT count(*) FROM collection_jobs WHERE workspace_id='ws-alpha'`); n != 1 {
		t.Errorf("alpha ledger rows %d", n)
	}
	if l, q := ownerCount(t, `SELECT count(*) FROM collection_jobs WHERE workspace_id='ws-beta'`), ownerCount(t, `SELECT count(*) FROM jobs WHERE type='collect' AND workspace_id='ws-beta'`); l != 0 || q != 0 {
		t.Errorf("beta got rows: ledger %d queue %d", l, q)
	}
}

func TestCollectLedgerAndQueueAtomic(t *testing.T) {
	e := newCollectEnv(t)
	collectEnqueue = func(context.Context, pgx.Tx, string, any, string) (int64, error) {
		return 0, errors.New("boom: secret detail")
	}
	rec := e.post("alice", "", `{"query":"IT staffing companies in Pune"}`)
	if rec.Code != 503 || rec.Body.String() != `{"detail":"Collection queue unavailable"}` {
		t.Errorf("%d %s", rec.Code, rec.Body.String())
	}
	if l, q := e.rows(t); l != 0 || q != 0 {
		t.Errorf("enqueue failure left rows: ledger %d queue %d", l, q)
	}
	collectEnqueue = nil
	if rec := e.post("alice", "", `{"query":"IT staffing companies in Pune"}`); rec.Code != 200 {
		t.Fatalf("after recovery: %d", rec.Code)
	}
	if l, q := e.rows(t); l != 1 || q != 1 {
		t.Errorf("rows: ledger %d queue %d", l, q)
	}
}

func TestCollectQueueRowShape(t *testing.T) {
	e := newCollectEnv(t)
	collectNewID = func() string { return "00000000000040008000000000005000" }
	rec := e.post("alice", "", `{"query":"  IT staffing companies in Pune  "}`)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var qid int
	var typ, executor, fireKey, ws, status, payload string
	var prio, maxRetries, retryCount int
	ctx := context.Background()
	c, err := pgx.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	err = c.QueryRow(ctx, `SELECT id, type, executor, fire_key, workspace_id, status, priority, max_retries, retry_count, payload::text
		FROM jobs WHERE type='collect' AND workspace_id='ws-alpha'`).Scan(&qid, &typ, &executor, &fireKey, &ws, &status, &prio, &maxRetries, &retryCount, &payload)
	if err != nil {
		t.Fatal(err)
	}
	// the Python-enqueued row (jobs/queue_after_collect.json): type, executor, fire key, retries, status
	if typ != "collect" || executor != "go" || fireKey != "collect:ws-alpha:00000000000040008000000000005000" ||
		ws != "ws-alpha" || status != "pending" || prio != 1 || maxRetries != 3 || retryCount != 0 {
		t.Errorf("row: %s %s %s %s %s %d %d %d", typ, executor, fireKey, ws, status, prio, maxRetries, retryCount)
	}
	if !strings.Contains(rec.Body.String(), fmt.Sprintf(`"queue_job_id":%d`, qid)) {
		t.Errorf("response %s lacks queue id %d", rec.Body.String(), qid)
	}
	// payload keys and their ORDER are the Python dict's: job_id, query, intent, workspace_id, slug
	dec := json.NewDecoder(strings.NewReader(payload))
	if _, err := dec.Token(); err != nil {
		t.Fatal(err)
	}
	var keys []string
	vals := map[string]any{}
	for dec.More() {
		k, _ := dec.Token()
		var v any
		if err := dec.Decode(&v); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k.(string))
		vals[k.(string)] = v
	}
	if strings.Join(keys, ",") != "job_id,query,intent,workspace_id,slug" {
		t.Errorf("payload key order %v", keys)
	}
	want := map[string]any{"job_id": "00000000000040008000000000005000", "query": "IT staffing companies in Pune", "intent": "market_search", "workspace_id": "ws-alpha", "slug": "alpha"}
	for k, v := range want {
		if vals[k] != v {
			t.Errorf("payload %s = %v, want %v", k, vals[k], v)
		}
	}
	// the ledger row: id, stripped query, intent, intent_details = json.dumps(decision.to_dict())
	var lq, lintent, ldetails, lstatus, lws string
	err = c.QueryRow(ctx, `SELECT query, intent, intent_details, status, workspace_id FROM collection_jobs WHERE id='00000000000040008000000000005000'`).
		Scan(&lq, &lintent, &ldetails, &lstatus, &lws)
	if err != nil {
		t.Fatal(err)
	}
	wantDetails := `{"query": "IT staffing companies in Pune", "intent": "market_search", "domain": "", "entity": "", "clarification_kind": "", "clarification_required": false, "reason": "", "options": []}`
	if lq != "IT staffing companies in Pune" || lintent != "market_search" || ldetails != wantDetails || lstatus != "pending" || lws != "ws-alpha" {
		t.Errorf("ledger: %q %q %q %q %q", lq, lintent, ldetails, lstatus, lws)
	}
}

func TestCollectJobIDIsUUID4Hex(t *testing.T) {
	e := newCollectEnv(t)
	rec := e.post("alice", "", `{"query":"IT staffing companies in Pune"}`)
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	id, _ := m["job_id"].(string)
	if !regexp.MustCompile(`^[0-9a-f]{12}4[0-9a-f]{3}[89ab][0-9a-f]{15}$`).MatchString(id) {
		t.Errorf("job_id %q is not a uuid4 hex", id)
	}
}

func TestCollectQueryTooLong422(t *testing.T) {
	e := newCollectEnv(t)
	long := strings.Repeat("a", 1001)
	rec := e.post("alice", "", `{"query":"`+long+`"}`)
	var m struct {
		Detail []struct {
			Type string         `json:"type"`
			Loc  []any          `json:"loc"`
			Msg  string         `json:"msg"`
			In   string         `json:"input"`
			Ctx  map[string]any `json:"ctx"`
		} `json:"detail"`
	}
	if rec.Code != 422 || json.Unmarshal(rec.Body.Bytes(), &m) != nil || len(m.Detail) != 1 {
		t.Fatalf("%d %.200s", rec.Code, rec.Body.String())
	}
	d := m.Detail[0]
	if d.Type != "string_too_long" || d.Msg != "String should have at most 1000 characters" || d.In != long ||
		fmt.Sprint(d.Loc) != "[body query]" || fmt.Sprint(d.Ctx["max_length"]) != "1000" {
		t.Errorf("detail %+v", d)
	}
	if l, q := e.rows(t); l != 0 || q != 0 {
		t.Errorf("rows written: ledger %d queue %d", l, q)
	}
	// the limit counts characters (before strip, as pydantic): 1000 two-byte runes are accepted
	if rec := e.post("alice", "", `{"query":"`+strings.Repeat("é", 1000)+`"}`); rec.Code != 200 {
		t.Errorf("1000 runes: %d %.200s", rec.Code, rec.Body.String())
	}
}

func TestCollectNulByte422(t *testing.T) {
	e := newCollectEnv(t)
	for body, loc := range map[string]string{`{"query":"a\u0000b"}`: `"query"`, `{"query":"x","intent":"a\u0000"}`: `"intent"`} {
		rec := e.post("alice", "", body)
		if rec.Code != 422 || !strings.Contains(rec.Body.String(), `"type":"value_error","loc":["body",`+loc+`],"msg":"Value error, string contains a NUL byte"`) {
			t.Errorf("%s: %d %s", body, rec.Code, rec.Body.String())
		}
	}
	if l, q := e.rows(t); l != 0 || q != 0 {
		t.Errorf("rows written: ledger %d queue %d", l, q)
	}
}

func TestCollectNonStringQuery422(t *testing.T) {
	e := newCollectEnv(t)
	cases := map[string]string{
		`{"query":["a"]}`:                   `{"detail":[{"type":"string_type","loc":["body","query"],"msg":"Input should be a valid string","input":["a"]}]}`,
		`{"query":"x","intent":5}`:          `{"detail":[{"type":"string_type","loc":["body","intent"],"msg":"Input should be a valid string","input":5}]}`,
		`{"query":"x","workspace_id":null}`: `{"detail":[{"type":"string_type","loc":["body","workspace_id"],"msg":"Input should be a valid string","input":null}]}`,
		`{"query":true,"intent":1.5}`:       `{"detail":[{"type":"string_type","loc":["body","query"],"msg":"Input should be a valid string","input":true},{"type":"string_type","loc":["body","intent"],"msg":"Input should be a valid string","input":1.5}]}`,
		`{"query":12345678901234567890123}`: `{"detail":[{"type":"string_type","loc":["body","query"],"msg":"Input should be a valid string","input":12345678901234567890123}]}`,
		`{"query":"x"`:                      "",
		``:                                  `{"detail":[{"type":"missing","loc":["body"],"msg":"Field required","input":null}]}`,
	}
	for body, want := range cases {
		rec := e.post("alice", "", body)
		if rec.Code != 422 {
			t.Errorf("%s: %d %s", body, rec.Code, rec.Body.String())
			continue
		}
		if want != "" && rec.Body.String() != want {
			t.Errorf("%s:\n got %s\nwant %s", body, rec.Body.String(), want)
		}
		if want == "" && !strings.Contains(rec.Body.String(), `"type":"json_invalid"`) {
			t.Errorf("%s: %s", body, rec.Body.String())
		}
	}
	if l, q := e.rows(t); l != 0 || q != 0 {
		t.Errorf("rows written: ledger %d queue %d", l, q)
	}
	big := `{"query":"x","pad":"` + strings.Repeat("p", 70<<10) + `"}`
	if rec := e.post("alice", "", big); rec.Code != 413 || rec.Body.String() != `{"detail":"Request body too large"}` {
		t.Errorf("oversize body: %d %.100s", rec.Code, rec.Body.String())
	}
}

func TestCollectProgressEventAfterCommit(t *testing.T) {
	rdb := testRedis(t)
	ctx := context.Background()
	t.Cleanup(func() { rdb.Del(ctx, leadprogress.RecentKey("ws-alpha")) })
	e := newCollectEnv(t, func(d *Deps) { d.Redis = rdb })
	collectBus = func(Deps) *leadprogress.Bus { return leadprogress.NewBus(rdb) }
	realEnqueue := collectEnqueueDefault
	// hold the transaction open after the enqueue: an event emitted before the commit would reach the
	// subscriber while the ledger row is still invisible to other connections
	collectEnqueue = func(ctx context.Context, tx pgx.Tx, ws string, payload any, fireKey string) (int64, error) {
		id, err := realEnqueue(ctx, tx, ws, payload, fireKey)
		time.Sleep(300 * time.Millisecond)
		return id, err
	}
	sub := rdb.Subscribe(ctx, leadprogress.Channel("ws-alpha"))
	defer sub.Close()
	if _, err := sub.Receive(ctx); err != nil {
		t.Fatal(err)
	}
	type seen struct {
		raw  string
		rows int
	}
	got := make(chan seen, 4)
	go func() {
		for msg := range sub.Channel() {
			got <- seen{msg.Payload, ownerCountNoT(`SELECT count(*) FROM collection_jobs WHERE workspace_id='ws-alpha'`)}
		}
	}()
	rec := e.post("alice", "", `{"query":"IT staffing companies in Pune"}`)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	select {
	case s := <-got:
		if s.rows != 1 {
			t.Errorf("event reached subscribers before the ledger row was committed (rows visible: %d)", s.rows)
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(s.raw), &ev); err != nil {
			t.Fatal(err)
		}
		if ev["type"] != "job_created" || ev["job_id"] != resp["job_id"] || ev["query"] != "IT staffing companies in Pune" ||
			ev["workspace_id"] != "ws-alpha" || ev["message"] != "Collection queued: IT staffing companies in Pune" {
			t.Errorf("event %s", s.raw)
		}
		ij, iq, im := strings.Index(s.raw, `"job_id"`), strings.Index(s.raw, `"query"`), strings.Index(s.raw, `"message"`)
		if !(ij >= 0 && ij < iq && iq < im) { // Python data order: job_id, query, workspace_id, message
			t.Errorf("event key order: %s", s.raw)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no job_created event")
	}

	// a failed enqueue publishes nothing; a Redis outage never fails the request
	collectEnqueue = func(context.Context, pgx.Tx, string, any, string) (int64, error) { return 0, errors.New("down") }
	if rec := e.post("alice", "", `{"query":"payroll firms Delhi"}`); rec.Code != 503 {
		t.Fatalf("%d", rec.Code)
	}
	select {
	case s := <-got:
		t.Errorf("event for a rolled-back job: %s", s.raw)
	case <-time.After(300 * time.Millisecond):
	}
	collectEnqueue = nil
	dead := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 200 * time.Millisecond})
	defer dead.Close()
	collectBus = func(Deps) *leadprogress.Bus { return leadprogress.NewBus(dead) }
	if rec := e.post("alice", "", `{"query":"payroll firms Delhi"}`); rec.Code != 200 {
		t.Errorf("redis down: %d %s", rec.Code, rec.Body.String())
	}
}

func ownerCountNoT(sql string) int {
	ctx := context.Background()
	c, err := pgx.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		return -1
	}
	defer c.Close(ctx)
	var n int
	if err := c.QueryRow(ctx, sql).Scan(&n); err != nil {
		return -1
	}
	return n
}
