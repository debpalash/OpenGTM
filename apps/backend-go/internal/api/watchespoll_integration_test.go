//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/contract"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

var watchTables = []string{"watch_subscriptions", "watch_schedules", "poll_budget_ledger", "signals"}

type watchPollEnv struct{ leadsEnv }

// newWatchPollEnv is the leads environment (alice/bob, ws-alpha/ws-beta, leads 1-18) plus erin (editor in
// ws-alpha) and the watch seed of contract_seed.py (CONTRACT_SEED_AUTOMATIONS=yes), dumped by
// legacy/python/scripts/dump_watch_seed.py into testdata/watches/seed.json. flags picks the profile:
// default (poller + PG lead store on), off (poller off) or nopg (PG lead store off).
func newWatchPollEnv(t *testing.T, flags config.Flags) watchPollEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 40)
	st := identity.NewStore(pool)
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(pool, nil))
	flags.Automations.GoRoutesEnabled = true
	flags.Automations.PollerPollNowDailyQuota = 50
	flags.Automations.PollerPollNowMinIntervalSec = 60
	flags.Automations.PollerDefaultInterval = "daily"
	flags.Automations.PollerMaxWatchesPerWS = 200
	flags.Automations.PollerJobChangeMaxContacts = 500
	d := Deps{Pool: pool, Signer: testSigner, Identity: st, Config: config.APIConfig{}, Flags: flags}
	registerWatchesPoll(r, d)
	e := watchPollEnv{leadsEnv{mcpEnv{env: env{h: r.Handler(), st: st}, pool: pool}}}
	e.purgeWatches(t)
	e.purge()
	t.Cleanup(e.purge)
	e.seedLeads(t)
	ownerExec(t, `SELECT setval(pg_get_serial_sequence('leads','id'),
		GREATEST(1000, (SELECT COALESCE(max(id), 0) FROM leads), (SELECT last_value FROM leads_id_seq)))`)
	e.seed(t, "erin", pythonHash, true, "user", false)
	e.member(t, "ws-alpha", e.mustUserByName(t, "erin").ID, "editor")
	e.seedWatches(t)
	// registered last, so it runs before the workspace cleanups (FKs from the watch rows)
	t.Cleanup(func() { e.purgeWatches(t) })
	return e
}

func (e watchPollEnv) purgeWatches(t *testing.T) {
	t.Helper()
	ownerExec(t, `DELETE FROM jobs WHERE type = 'watch_poll'`)
	for _, tb := range append([]string{"watch_schedules"}, watchTables...) {
		ownerExec(t, "DELETE FROM "+tb+" WHERE workspace_id IN ('ws-alpha','ws-beta')")
	}
}

func (e watchPollEnv) seedWatches(t *testing.T) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "watches", "seed.json"))
	if err != nil {
		t.Fatal(err)
	}
	var seed map[string]map[string]struct {
		Columns []string          `json:"columns"`
		Rows    []json.RawMessage `json:"rows"`
	}
	if err := json.Unmarshal(raw, &seed); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for ws, tables := range seed {
		for _, tb := range watchTables {
			tt := tables[tb]
			b, _ := json.Marshal(tt.Rows)
			cols := strings.Join(tt.Columns, ", ")
			err := tenancy.InWorkspace(ctx, e.pool, ws, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %[1]s (%[2]s) SELECT %[2]s FROM json_populate_recordset(null::%[1]s, $1::json)`, tb, cols), string(b))
				return err
			})
			if err != nil {
				t.Fatalf("seed %s %s: %v", ws, tb, err)
			}
		}
	}
	// the recorded beta quota row is dated the day it was seeded: re-date it to today (UTC) so the quota is exhausted now
	ownerExec(t, `UPDATE poll_budget_ledger SET day = (now() AT TIME ZONE 'UTC')::date WHERE workspace_id = 'ws-beta'`)
}

// watchActor says who sent a recorded request (the recorder redacts Authorization and X-Workspace-Id).
func watchActor(name string) (who, ws string) {
	switch name {
	case "poll_no_auth", "signals_no_auth":
		return "", ""
	case "poll_daily_quota_exhausted", "signals_beta_own":
		return "bob", "ws-beta" // bob's recorded active workspace; Go resolves the default workspace differently
	case "poll_member_forbidden", "signals_member_read_ok", "off_watches_poll_member_403_first":
		return "bob", "ws-alpha"
	case "poll_editor_ok":
		return "erin", ""
	}
	return "alice", ""
}

func (e watchPollEnv) call(method, who, ws, path string) (*httptest.ResponseRecorder, contract.Resp) {
	hdr := map[string]string{}
	if who != "" {
		hdr = bearer(who)
	}
	if ws != "" {
		hdr["X-Workspace-Id"] = ws
	}
	return e.do(method, path, "", "", hdr)
}

func (e watchPollEnv) replay(t *testing.T, name string, opts ...contract.Option) {
	t.Helper()
	fx := contract.Load(t, "watches/"+name)
	who, ws := watchActor(name)
	_, got := e.call(fx.Request.Method, who, ws, fx.Request.Path)
	contract.AssertEqual(t, got, fx.Response, opts...)
}

var pollVolatile = contract.IgnorePaths("$.fire_key", "$.job_id")

// The poll fixtures were recorded in this order against one database (poll_queued enqueues the manual poll that
// poll_already_queued_manual then finds).
var pollOrder = []string{"poll_queued", "poll_already_queued_manual", "poll_disabled", "poll_auto_disabled", "poll_rate_limited",
	"poll_daily_quota_exhausted", "poll_unknown", "poll_foreign_ws", "poll_member_forbidden", "poll_editor_ok", "poll_no_auth"}

func TestPollNowMatchesFixtures(t *testing.T) {
	e := newWatchPollEnv(t, config.Flags{PGLeadStore: true, IntentPollerEnabled: true})
	for _, name := range pollOrder {
		t.Run(name, func(t *testing.T) { e.replay(t, name, pollVolatile) })
	}
}

func TestPollNowGatesMatchFixtures(t *testing.T) {
	// one pool (and test lock) per profile: each profile is its own subtest
	t.Run("off", func(t *testing.T) {
		off := newWatchPollEnv(t, config.Flags{PGLeadStore: true})
		for _, name := range []string{"off_watches_poll", "off_watches_poll_member_403_first", "off_watches_signals"} {
			t.Run(name, func(t *testing.T) { off.replay(t, name) })
		}
	})
	t.Run("nopg", func(t *testing.T) {
		nopg := newWatchPollEnv(t, config.Flags{IntentPollerEnabled: true})
		for _, name := range []string{"nopg_poll", "nopg_signals"} {
			t.Run(name, func(t *testing.T) { nopg.replay(t, name) })
		}
	})
}

func TestPollNowDisabled409(t *testing.T) {
	e := newWatchPollEnv(t, config.Flags{PGLeadStore: true, IntentPollerEnabled: true})
	for _, name := range []string{"poll_disabled", "poll_auto_disabled"} {
		e.replay(t, name)
	}
	if n := e.jobCount(t, "watch-a-07"); n != 0 {
		t.Fatalf("disabled watch enqueued %d jobs", n)
	}
}

func TestPollNowRateLimited429(t *testing.T) {
	e := newWatchPollEnv(t, config.Flags{PGLeadStore: true, IntentPollerEnabled: true})
	e.replay(t, "poll_rate_limited")
}

func TestPollNowQuota429(t *testing.T) {
	e := newWatchPollEnv(t, config.Flags{PGLeadStore: true, IntentPollerEnabled: true})
	e.replay(t, "poll_daily_quota_exhausted")
}

func (e watchPollEnv) jobCount(t *testing.T, watch string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM jobs WHERE type='watch_poll' AND payload->>'watch_id' = $1 AND status IN ('pending','processing')`, watch).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPollNowReusesScheduledJob(t *testing.T) {
	e := newWatchPollEnv(t, config.Flags{PGLeadStore: true, IntentPollerEnabled: true})
	var jid int64
	err := tenancy.InWorkspace(context.Background(), e.pool, "ws-alpha", func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `INSERT INTO jobs (type, payload, status, fire_key, workspace_id, executor)
			VALUES ('watch_poll','{"watch_id":"watch-a-02"}'::json,'pending','watch:watch-a-02:2099-01-01T00:00:00+00:00','ws-alpha','python') RETURNING id`).Scan(&jid)
	})
	if err != nil {
		t.Fatal(err)
	}
	_, got := e.call("POST", "alice", "", "/api/watches/watch-a-02/poll")
	var body map[string]any
	_ = json.Unmarshal(got.Body, &body)
	if got.Status != 200 || body["status"] != "already_queued" || int64(body["job_id"].(float64)) != jid ||
		body["fire_key"] != "watch:watch-a-02:2099-01-01T00:00:00+00:00" {
		t.Fatalf("%d %s", got.Status, got.Body)
	}
	if n := e.jobCount(t, "watch-a-02"); n != 1 {
		t.Fatalf("jobs %d", n)
	}
}

func TestPollNowPayloadHasPollNow(t *testing.T) {
	e := newWatchPollEnv(t, config.Flags{PGLeadStore: true, IntentPollerEnabled: true})
	_, got := e.call("POST", "alice", "", "/api/watches/watch-a-01/poll")
	if got.Status != 200 || !strings.Contains(string(got.Body), `"queued"`) {
		t.Fatalf("%d %s", got.Status, got.Body)
	}
	var payload string
	if err := e.pool.QueryRow(context.Background(), `SELECT payload::text FROM jobs WHERE type='watch_poll' AND payload->>'watch_id'='watch-a-01'`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var p map[string]any
	if err := json.Unmarshal([]byte(payload), &p); err != nil || p["poll_now"] != true || p["workspace_id"] != "ws-alpha" ||
		!strings.HasPrefix(fmt.Sprint(p["fire_key"]), "watch:watch-a-01:pollnow:") {
		t.Fatalf("payload %s", payload)
	}
}

func TestPollNowConcurrentEnqueuesOnce(t *testing.T) {
	e := newWatchPollEnv(t, config.Flags{PGLeadStore: true, IntentPollerEnabled: true})
	const n = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	statuses := map[string]int{}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, got := e.call("POST", "alice", "", "/api/watches/watch-a-01/poll")
			var body map[string]any
			_ = json.Unmarshal(got.Body, &body)
			mu.Lock()
			statuses[fmt.Sprintf("%d/%v", got.Status, body["status"])]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if statuses["200/queued"] != 1 || statuses["200/already_queued"] != n-1 {
		t.Fatalf("statuses %v", statuses)
	}
	if c := e.jobCount(t, "watch-a-01"); c != 1 {
		t.Fatalf("active jobs %d", c)
	}
}

func TestSignalsMatchFixtures(t *testing.T) {
	e := newWatchPollEnv(t, config.Flags{PGLeadStore: true, IntentPollerEnabled: true})
	files, err := filepath.Glob(filepath.Join("..", "..", "testdata", "contract", "watches", "signals_*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 35 {
		t.Fatalf("expected 35 signals fixtures, found %d", len(files))
	}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".json")
		t.Run(name, func(t *testing.T) { e.replay(t, name) })
	}
}

func TestSignalsAccountGroupExpansion(t *testing.T) {
	e := newWatchPollEnv(t, config.Flags{PGLeadStore: true, IntentPollerEnabled: true})
	e.replay(t, "signals_account_group_expansion")
	e.replay(t, "signals_filter_type_group")
	// ws-beta's signals never leak into an alpha group expansion
	_, got := e.call("GET", "alice", "", "/api/watches/watch-a-06/signals?limit=200")
	if strings.Contains(string(got.Body), "sig-p-b-01") {
		t.Fatal("foreign workspace signal in expansion")
	}
}

func TestSignalsInvalidCursor422(t *testing.T) {
	e := newWatchPollEnv(t, config.Flags{PGLeadStore: true, IntentPollerEnabled: true})
	for _, name := range []string{"signals_cursor_not_base64", "signals_cursor_not_json", "signals_cursor_wrong_shape", "signals_cursor_too_long"} {
		e.replay(t, name)
	}
}

func TestSignalsLimitBounds422(t *testing.T) {
	e := newWatchPollEnv(t, config.Flags{PGLeadStore: true, IntentPollerEnabled: true})
	for _, name := range []string{"signals_limit_0", "signals_limit_201", "signals_offset_negative", "signals_limit_200"} {
		e.replay(t, name)
	}
}

func TestSignalsForeignWatch404(t *testing.T) {
	e := newWatchPollEnv(t, config.Flags{PGLeadStore: true, IntentPollerEnabled: true})
	e.replay(t, "signals_foreign_watch")
	e.replay(t, "signals_unknown_watch")
	e.replay(t, "poll_foreign_ws")
}

func TestSignalsReadOnlyMemberAllowed(t *testing.T) {
	e := newWatchPollEnv(t, config.Flags{PGLeadStore: true, IntentPollerEnabled: true})
	e.replay(t, "signals_member_read_ok")
	// the same member may not poll (signals.write)
	e.replay(t, "poll_member_forbidden")
}
