//go:build integration

package watchsvc_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/autosvc"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/secrets"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/watchsvc"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

const (
	wsA = "ws-p08t13w-a"
	wsB = "ws-p08t13w-b"
)

var fixedNow = time.Date(2026, 8, 29, 9, 30, 0, 123456000, time.UTC)

type env struct {
	pool *pgxpool.Pool
	ctx  context.Context
	svc  *watchsvc.Service
}

func inWS(t *testing.T, e *env, ws string, fn func(tx pgx.Tx) error) {
	t.Helper()
	if err := tenancy.InWorkspace(e.ctx, e.pool, ws, fn); err != nil {
		t.Fatal(err)
	}
}

func baseCfg() watchsvc.Config {
	return watchsvc.Config{DefaultInterval: "daily", MaxWatchesPerWS: 200, JobChangeMaxContacts: 500,
		PollNowMinIntervalSec: 60, PollNowDailyQuota: 50, AutomationsEnabled: true}
}

func setup(t *testing.T) *env {
	t.Helper()
	pool := testpg.NewAppPool(t, 12)
	ctx := context.Background()
	e := &env{pool: pool, ctx: ctx}
	for _, ws := range []string{wsA, wsB} {
		if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, slug) VALUES ($1::varchar,$1::text,$1::text) ON CONFLICT DO NOTHING`, ws); err != nil {
			t.Fatal(err)
		}
	}
	clean := func() {
		for _, ws := range []string{wsA, wsB} {
			_ = tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
				for _, q := range []string{`DELETE FROM watch_subscriptions`, `DELETE FROM triggers`, `DELETE FROM signals`,
					`DELETE FROM poll_budget_ledger`, `DELETE FROM leads`, `DELETE FROM workspace_settings`} {
					_, _ = tx.Exec(ctx, q)
				}
				_, _ = tx.Exec(ctx, `DELETE FROM jobs WHERE workspace_id = $1`, ws)
				_, _ = tx.Exec(ctx, `DELETE FROM watch_schedules WHERE workspace_id = $1`, ws)
				return nil
			})
		}
	}
	clean()
	t.Cleanup(clean)
	e.svc = &watchsvc.Service{Pool: pool, Cfg: baseCfg(), Now: func() time.Time { return fixedNow },
		ExecFor:  func(string) string { return "go" },
		Secrets:  secrets.NewResolver(nil, secrets.NewPgStore(pool), os.Getenv),
		Outbound: secrets.OutboundPolicy{Prefixes: []string{"HOOK"}}}
	return e
}

func httpErr(t *testing.T, err error, status int, detail string) {
	t.Helper()
	var he *watchsvc.HTTPError
	if !errors.As(err, &he) {
		t.Fatalf("want %d %q, got %v", status, detail, err)
	}
	if he.Status != status || he.Detail != detail {
		t.Fatalf("got %d %q, want %d %q", he.Status, he.Detail, status, detail)
	}
}

func seedLead(t *testing.T, e *env, ws string) int {
	t.Helper()
	var id int
	inWS(t, e, ws, func(tx pgx.Tx) error {
		return tx.QueryRow(e.ctx, `INSERT INTO leads (workspace_id, company) VALUES ($1,'Lead Co') RETURNING id`, ws).Scan(&id)
	})
	return id
}

func jsonObj(t *testing.T, v any) *wbcrud.Obj {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	o, err := wbcrud.ParseObject(b)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func getKey(t *testing.T, w json.Marshaler, k string) any {
	t.Helper()
	b, err := w.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	o, err := wbcrud.ParseObject(b)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := o.Get(k)
	return v
}

func insertWatch(t *testing.T, e *env, ws, id, kind, target string, enabled bool, failures int, lastErr *string, lastPolled *time.Time) {
	t.Helper()
	inWS(t, e, ws, func(tx pgx.Tx) error {
		_, err := tx.Exec(e.ctx, `INSERT INTO watch_subscriptions (id, workspace_id, kind, target, signal_types, "interval", schedule_anchor, enabled, cursor, consecutive_failures, last_error, last_polled_at)
			VALUES ($1,$2,$3,$4,'["company_funded"]'::json,'daily',$5,$6,'{"bootstrapped": false}'::json,$7,$8,$9)`,
			id, ws, kind, target, fixedNow.Add(-48*time.Hour), enabled, failures, lastErr, lastPolled)
		return err
	})
}

func TestCreateWithWebhookRule(t *testing.T) {
	e := setup(t)
	inWS(t, e, wsA, func(tx pgx.Tx) error {
		_, err := tx.Exec(e.ctx, `INSERT INTO workspace_settings (workspace_id, key, value) VALUES ($1,'HOOK','x')`, wsA)
		return err
	})
	url := "https://hooks.example.com/w"
	ref := "HOOK"
	long := strings.Repeat("t", 260)
	c, err := e.svc.Create(e.ctx, wsA, watchsvc.CreateInput{Kind: "feed", Target: long, CreateWebhookRule: true, WebhookURL: &url, WebhookSecretRef: &ref})
	if err != nil {
		t.Fatal(err)
	}
	if c.WebhookRuleID == nil {
		t.Fatal("webhook_rule_id missing")
	}
	var name, tt, tcfg, actions, scope string
	var enabled bool
	inWS(t, e, wsA, func(tx pgx.Tx) error {
		return tx.QueryRow(e.ctx, `SELECT name, trigger_type, trigger_config::text, actions::text, scope_workbook_ids::text, enabled FROM triggers WHERE id=$1`, *c.WebhookRuleID).Scan(&name, &tt, &tcfg, &actions, &scope, &enabled)
	})
	if len([]rune(name)) != 200 || !strings.HasPrefix(name, "watch webhook (feed:ttt") || tt != "on_signal" || !enabled {
		t.Fatalf("rule %q %q %v", name, tt, enabled)
	}
	if tcfg != `{"signal_types":["news"]}` || actions != `[{"type":"webhook","config":{"url":"https://hooks.example.com/w","method":"POST","header_secret_ref":"HOOK"}}]` || strings.ReplaceAll(scope, " ", "") != "[]" {
		t.Fatalf("rule body %s %s %s", tcfg, actions, scope)
	}
	if v := getKey(t, c, "webhook_rule_id"); v != *c.WebhookRuleID {
		t.Fatalf("response webhook_rule_id %v", v)
	}
	// a duplicate (kind, target) is refused BEFORE the rule is created: no orphan
	_, err = e.svc.Create(e.ctx, wsA, watchsvc.CreateInput{Kind: "feed", Target: long, CreateWebhookRule: true, WebhookURL: &url})
	httpErr(t, err, 409, "watch already exists for this kind and target")
	var n int
	inWS(t, e, wsA, func(tx pgx.Tx) error {
		return tx.QueryRow(e.ctx, `SELECT count(*) FROM triggers`).Scan(&n)
	})
	if n != 1 {
		t.Fatalf("rules %d, want 1 (no orphan after a refused duplicate)", n)
	}
	// allow-list applies to the watch webhook url too
	e.svc.Cfg.WebhookAllowlist = []string{".example.com"}
	bad := "https://evil.net/x"
	_, err = e.svc.Create(e.ctx, wsA, watchsvc.CreateInput{Kind: "feed", Target: "other", CreateWebhookRule: true, WebhookURL: &bad})
	httpErr(t, err, 422, "webhook_url: host 'evil.net' not in webhook domain allowlist")
}

func TestPatchEnableResetsFailuresAndBootstraps(t *testing.T) {
	e := setup(t)
	le := "auto_disabled"
	insertWatch(t, e, wsA, "w-1", "funding", "Acme", false, 12, &le, nil)
	on := true
	got, err := e.svc.Patch(e.ctx, wsA, "w-1", watchsvc.PatchInput{Enabled: &on})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Enabled || got.ConsecutiveFailures != 0 || got.LastError != nil || got.NextPollAt == nil {
		t.Fatalf("%+v", got)
	}
	var fk, ex string
	if err := e.pool.QueryRow(e.ctx, `SELECT fire_key, executor FROM jobs WHERE workspace_id=$1 AND type='watch_poll'`, wsA).Scan(&fk, &ex); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(fk, "watch:w-1:") || ex != "go" {
		t.Fatalf("job %q %q", fk, ex)
	}
	var men bool
	var mnext *time.Time
	if err := e.pool.QueryRow(e.ctx, `SELECT enabled, next_poll_at FROM watch_schedules WHERE watch_id='w-1'`).Scan(&men, &mnext); err != nil || !men || mnext == nil || !mnext.Equal(*got.NextPollAt) {
		t.Fatalf("mirror %v %v %v", men, mnext, err)
	}
	// disable clears next_poll_at and the mirror's due time
	off := false
	got, err = e.svc.Patch(e.ctx, wsA, "w-1", watchsvc.PatchInput{Enabled: &off})
	if err != nil || got.Enabled || got.NextPollAt != nil {
		t.Fatalf("%v %+v", err, got)
	}
	if err := e.pool.QueryRow(e.ctx, `SELECT enabled, next_poll_at FROM watch_schedules WHERE watch_id='w-1'`).Scan(&men, &mnext); err != nil || men || mnext != nil {
		t.Fatalf("mirror after disable %v %v %v", men, mnext, err)
	}
	if err := e.svc.Delete(e.ctx, wsA, "w-1"); err != nil {
		t.Fatal(err)
	}
	var c int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM watch_schedules WHERE watch_id='w-1'`).Scan(&c)
	if c != 0 {
		t.Fatal("mirror must go with the watch")
	}
	httpErr(t, e.svc.Delete(e.ctx, wsA, "w-1"), 404, "watch not found")
	// tenant isolation
	insertWatch(t, e, wsB, "w-b", "funding", "B", true, 0, nil, nil)
	_, err = e.svc.Get(e.ctx, wsA, "w-b")
	httpErr(t, err, 404, "watch not found")
	_, err = e.svc.Patch(e.ctx, wsA, "w-b", watchsvc.PatchInput{Enabled: &off})
	httpErr(t, err, 404, "watch not found")
	_, err = e.svc.PollNow(e.ctx, wsA, "w-b")
	httpErr(t, err, 404, "watch not found")
	_, err = e.svc.Signals(e.ctx, wsA, "w-b", watchsvc.SignalsQuery{Limit: 10})
	httpErr(t, err, 404, "watch not found")
}

func TestPollNowRateAndQuotaAndSingleFlight(t *testing.T) {
	e := setup(t)
	recent := fixedNow.Add(-10 * time.Second)
	insertWatch(t, e, wsA, "w-rate", "funding", "Rate", true, 0, nil, &recent)
	_, err := e.svc.PollNow(e.ctx, wsA, "w-rate")
	httpErr(t, err, 429, "poll-now rate limited")

	insertWatch(t, e, wsA, "w-off", "funding", "Off", false, 0, nil, nil)
	_, err = e.svc.PollNow(e.ctx, wsA, "w-off")
	httpErr(t, err, 409, "watch disabled")

	old := fixedNow.Add(-time.Hour)
	insertWatch(t, e, wsA, "w-1", "funding", "One", true, 0, nil, &old)
	r, err := e.svc.PollNow(e.ctx, wsA, "w-1")
	if err != nil || r.Status != "queued" || r.FireKey != "watch:w-1:pollnow:2026-08-29" || r.JobID != nil {
		t.Fatalf("%+v %v", r, err)
	}
	var payload string
	if err := e.pool.QueryRow(e.ctx, `SELECT payload::text FROM jobs WHERE fire_key=$1 AND status='pending'`, r.FireKey).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if payload != `{"watch_id":"w-1","workspace_id":"`+wsA+`","fire_key":"watch:w-1:pollnow:2026-08-29","poll_now":true}` {
		t.Fatalf("payload %s", payload)
	}
	// second poll the same day while the first is pending: single flight, no second job
	r2, err := e.svc.PollNow(e.ctx, wsA, "w-1")
	if err != nil || r2.Status != "already_queued" || r2.FireKey != r.FireKey || r2.JobID != nil {
		t.Fatalf("%+v %v", r2, err)
	}
	var jobs int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM jobs WHERE workspace_id=$1 AND type='watch_poll'`, wsA).Scan(&jobs)
	if jobs != 1 {
		t.Fatalf("jobs %d", jobs)
	}
	// an active SCHEDULED poll is reused and reported with its id
	insertWatch(t, e, wsA, "w-2", "funding", "Two", true, 0, nil, &old)
	var jid int
	if err := e.pool.QueryRow(e.ctx, `INSERT INTO jobs (type, payload, status, fire_key, workspace_id, executor) VALUES ('watch_poll','{}'::json,'processing','watch:w-2:2026-08-29T12:00:00+00:00',$1,'python') RETURNING id`, wsA).Scan(&jid); err != nil {
		t.Fatal(err)
	}
	r3, err := e.svc.PollNow(e.ctx, wsA, "w-2")
	if err != nil || r3.Status != "already_queued" || r3.JobID == nil || *r3.JobID != int64(jid) || r3.FireKey != "watch:w-2:2026-08-29T12:00:00+00:00" {
		t.Fatalf("%+v %v", r3, err)
	}
	b, _ := json.Marshal(r3)
	if !strings.HasPrefix(string(b), `{"status":"already_queued","job_id":`) {
		t.Fatalf("%s", b)
	}
	// daily quota
	insertWatch(t, e, wsA, "w-3", "funding", "Three", true, 0, nil, &old)
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO poll_budget_ledger (workspace_id, day, n, n_poll_now) VALUES ($1,'2026-08-29',9,5)`, wsA); err != nil {
		// poll_budget_ledger is tenant scoped
		inWS(t, e, wsA, func(tx pgx.Tx) error {
			_, err := tx.Exec(e.ctx, `INSERT INTO poll_budget_ledger (workspace_id, day, n, n_poll_now) VALUES ($1,'2026-08-29',9,5)`, wsA)
			return err
		})
	}
	e.svc.Cfg.PollNowDailyQuota = 5
	_, err = e.svc.PollNow(e.ctx, wsA, "w-3")
	httpErr(t, err, 429, "poll-now daily quota exhausted")
	e.svc.Cfg.PollNowDailyQuota = 7 // ledger 5 + the queued manual poll of w-1
	if r, err := e.svc.PollNow(e.ctx, wsA, "w-3"); err != nil || r.Status != "queued" {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestForeignLeadIDPinRejected(t *testing.T) {
	e := setup(t)
	foreign := seedLead(t, e, wsB)
	own := seedLead(t, e, wsA)
	fid := int32(foreign)
	_, err := e.svc.Create(e.ctx, wsA, watchsvc.CreateInput{Kind: "funding", Target: "X", LeadID: &fid})
	httpErr(t, err, 422, "lead_id "+itoa(foreign)+" not found in this workspace")
	oid := int32(own)
	c, err := e.svc.Create(e.ctx, wsA, watchsvc.CreateInput{Kind: "funding", Target: "X", LeadID: &oid})
	if err != nil || c.LeadID == nil || *c.LeadID != oid {
		t.Fatalf("%+v %v", c, err)
	}
	_, err = e.svc.Patch(e.ctx, wsA, c.ID, watchsvc.PatchInput{LeadID: &fid})
	httpErr(t, err, 422, "lead_id "+itoa(foreign)+" not found in this workspace")
	got, _ := e.svc.Get(e.ctx, wsA, c.ID)
	if got.LeadID == nil || *got.LeadID != oid {
		t.Fatal("rejected patch must not change the pin")
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func seedSignal(t *testing.T, e *env, ws, id string, lead *int, company, typ string, created float64) {
	t.Helper()
	inWS(t, e, ws, func(tx pgx.Tx) error {
		_, err := tx.Exec(e.ctx, `INSERT INTO signals (id, workspace_id, lead_id, company, signal_type, title, created_at) VALUES ($1,$2,$3,$4,$5,$1,$6)`,
			id, ws, lead, company, typ, created)
		return err
	})
}

func sigIDs(t *testing.T, p watchsvc.SignalsPage) []string {
	t.Helper()
	var out []string
	for _, s := range p.Signals {
		out = append(out, s.ID)
	}
	return out
}

func TestSignalsAccountGroupExpansion(t *testing.T) {
	e := setup(t)
	l1, l2, other := seedLead(t, e, wsA), seedLead(t, e, wsA), seedLead(t, e, wsA)
	cfg := `{"workbook_name":"Accounts","accounts":[{"account_id":"a1","lead_id":` + itoa(l1) + `,"company":"Acme Inc"},{"account_id":"a2","company":"  Beta CO "},{"lead_id":` + itoa(l2) + `},"junk",{"company":""}]}`
	inWS(t, e, wsA, func(tx pgx.Tx) error {
		_, err := tx.Exec(e.ctx, `INSERT INTO watch_subscriptions (id, workspace_id, kind, target, signal_types, "interval", enabled, config, cursor)
			VALUES ('w-ag',$1,'account_group','x','["funding","news"]'::json,'daily',true,$2::json,'{}'::json)`, wsA, cfg)
		return err
	})
	seedSignal(t, e, wsA, "s1", &l1, "unrelated", "funding", 100)
	seedSignal(t, e, wsA, "s2", nil, "ACME INC", "funding", 90)
	seedSignal(t, e, wsA, "s3", nil, "beta co", "funding", 80)
	seedSignal(t, e, wsA, "s4", &l2, "", "funding", 70)
	seedSignal(t, e, wsA, "s5", &other, "Gamma", "funding", 60)
	seedSignal(t, e, wsA, "s6", &l1, "x", "pricing_page_change", 50) // type not watched
	seedSignal(t, e, wsA, "s7", nil, "acme inc", "news", 40)
	seedSignal(t, e, wsB, "s8", nil, "acme inc", "funding", 30) // foreign workspace

	p, err := e.svc.Signals(e.ctx, wsA, "w-ag", watchsvc.SignalsQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(sigIDs(t, p), ","); got != "s1,s2,s3,s4,s7" {
		t.Fatalf("expanded feed %s", got)
	}
	// keyset paging
	p1, err := e.svc.Signals(e.ctx, wsA, "w-ag", watchsvc.SignalsQuery{Limit: 2})
	if err != nil || !p1.HasMore || p1.NextCursor == nil || strings.Join(sigIDs(t, p1), ",") != "s1,s2" {
		t.Fatalf("%+v %v", p1, err)
	}
	b, _ := json.Marshal(p1)
	if !strings.Contains(string(b), `"has_more":true,"limit":2,"offset":0}`) {
		t.Fatalf("page JSON %s", b)
	}
	p2, err := e.svc.Signals(e.ctx, wsA, "w-ag", watchsvc.SignalsQuery{Limit: 2, Cursor: *p1.NextCursor})
	if err != nil || strings.Join(sigIDs(t, p2), ",") != "s3,s4" || p2.Offset != nil {
		t.Fatalf("%+v %v", p2, err)
	}
	b, _ = json.Marshal(p2)
	if !strings.Contains(string(b), `"limit":2,"offset":null}`) {
		t.Fatalf("cursor page JSON %s", b)
	}
	// signal_type narrows, offset pages
	pt, _ := e.svc.Signals(e.ctx, wsA, "w-ag", watchsvc.SignalsQuery{Limit: 10, SignalType: "news"})
	if strings.Join(sigIDs(t, pt), ",") != "s7" {
		t.Fatalf("type filter %v", sigIDs(t, pt))
	}
	po, _ := e.svc.Signals(e.ctx, wsA, "w-ag", watchsvc.SignalsQuery{Limit: 2, Offset: 3})
	if strings.Join(sigIDs(t, po), ",") != "s4,s7" {
		t.Fatalf("offset %v", sigIDs(t, po))
	}
	// bad cursor -> 422 with the signal store's message
	_, err = e.svc.Signals(e.ctx, wsA, "w-ag", watchsvc.SignalsQuery{Limit: 2, Cursor: "!!"})
	httpErr(t, err, 422, "invalid signal cursor")

	// an account group with no identity matches nothing (and never decodes the cursor)
	inWS(t, e, wsA, func(tx pgx.Tx) error {
		_, err := tx.Exec(e.ctx, `INSERT INTO watch_subscriptions (id, workspace_id, kind, target, signal_types, enabled, config, cursor)
			VALUES ('w-empty',$1,'account_group','e','["funding"]'::json,true,'{"accounts":[]}'::json,'{}'::json)`, wsA)
		return err
	})
	pe, err := e.svc.Signals(e.ctx, wsA, "w-empty", watchsvc.SignalsQuery{Limit: 5, Cursor: "!!"})
	if err != nil || len(pe.Signals) != 0 || pe.HasMore {
		t.Fatalf("%+v %v", pe, err)
	}

	// a plain watch with a pinned lead filters by lead and its own signal types
	insertWatch(t, e, wsA, "w-pin", "funding", "Pin", true, 0, nil, nil)
	inWS(t, e, wsA, func(tx pgx.Tx) error {
		_, err := tx.Exec(e.ctx, `UPDATE watch_subscriptions SET lead_id=$1 WHERE id='w-pin'`, l1)
		return err
	})
	pp, _ := e.svc.Signals(e.ctx, wsA, "w-pin", watchsvc.SignalsQuery{Limit: 10})
	if got := strings.Join(sigIDs(t, pp), ","); got != "" { // the watch's signal_types ["company_funded"] exclude funding
		t.Fatalf("pinned feed %q", got)
	}
	pp, _ = e.svc.Signals(e.ctx, wsA, "w-pin", watchsvc.SignalsQuery{Limit: 10, SignalType: "funding"})
	if got := strings.Join(sigIDs(t, pp), ","); got != "s1" {
		t.Fatalf("pinned lead feed %s", got)
	}
}

func TestListPagingCursorAndOffset(t *testing.T) {
	e := setup(t)
	for i, id := range []string{"w-a", "w-b", "w-c"} {
		insertWatch(t, e, wsA, id, "funding", "T"+id, true, 0, nil, nil)
		inWS(t, e, wsA, func(tx pgx.Tx) error {
			_, err := tx.Exec(e.ctx, `UPDATE watch_subscriptions SET created_at = $2 WHERE id=$1`, id, fixedNow.Add(time.Duration(i)*time.Minute))
			return err
		})
	}
	p, err := e.svc.List(e.ctx, wsA, 2, 0, "")
	if err != nil || !p.HasMore || p.NextCursor == nil || len(p.Watches) != 2 || p.Watches[0].ID != "w-c" || p.Watches[1].ID != "w-b" {
		t.Fatalf("%+v %v", p, err)
	}
	p2, err := e.svc.List(e.ctx, wsA, 2, 0, *p.NextCursor)
	if err != nil || p2.HasMore || len(p2.Watches) != 1 || p2.Watches[0].ID != "w-a" || p2.Offset != nil {
		t.Fatalf("%+v %v", p2, err)
	}
	p3, err := e.svc.List(e.ctx, wsA, 2, 2, "")
	if err != nil || len(p3.Watches) != 1 || p3.Watches[0].ID != "w-a" || p3.Offset == nil || *p3.Offset != 2 {
		t.Fatalf("%+v %v", p3, err)
	}
	_, err = e.svc.List(e.ctx, wsA, 2, 0, "bm90anNvbg")
	httpErr(t, err, 422, "invalid watch cursor")
	if l, _ := e.svc.List(e.ctx, wsB, 10, 0, ""); len(l.Watches) != 0 {
		t.Fatal("list leaks across workspaces")
	}
}

var _ = autosvc.Iso

func TestPollNowQuotaSerialisedAcrossWatches(t *testing.T) {
	e := setup(t)
	old := fixedNow.Add(-time.Hour)
	for i := 0; i < 6; i++ {
		insertWatch(t, e, wsA, "w-q"+itoa(i), "funding", "Q"+itoa(i), true, 0, nil, &old)
	}
	e.svc.Cfg.PollNowDailyQuota = 2
	res := make(chan error, 6)
	for i := 0; i < 6; i++ {
		go func(i int) { _, err := e.svc.PollNow(e.ctx, wsA, "w-q"+itoa(i)); res <- err }(i)
	}
	ok, limited := 0, 0
	for i := 0; i < 6; i++ {
		var he *watchsvc.HTTPError
		switch err := <-res; {
		case err == nil:
			ok++
		case errors.As(err, &he) && he.Status == 429:
			limited++
		default:
			t.Fatal(err)
		}
	}
	if ok != 2 || limited != 4 {
		t.Fatalf("queued %d limited %d, want 2 and 4", ok, limited)
	}
}
