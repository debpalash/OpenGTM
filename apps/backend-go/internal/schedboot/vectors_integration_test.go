//go:build integration

package schedboot_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/schedboot"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

// The vectors are recorded from the Python bootstrap_* functions (testdata/gen_vectors.py.txt) against
// a real Postgres: the job rows and mirror rows each leaves behind for a fixed seed, under an executor
// configuration, plus the return value of the first and of a second run.

type recordedJob struct {
	Type        string         `json:"type"`
	Executor    string         `json:"executor"`
	Status      string         `json:"status"`
	Payload     map[string]any `json:"payload"`
	Priority    int            `json:"priority"`
	MaxRetries  int            `json:"max_retries"`
	RetryCount  int            `json:"retry_count"`
	NextRunAt   *int           `json:"next_run_at"`
	FireKey     *string        `json:"fire_key"`
	WorkspaceID *string        `json:"workspace_id"`
}

type recordedCase struct {
	First   any                         `json:"returned_first"`
	Second  any                         `json:"returned_second"`
	Jobs    []recordedJob               `json:"jobs"`
	Mirrors map[string][]map[string]any `json:"mirrors"`
	// second_run_* record that Python's second run changed nothing.
	SameJobs    bool `json:"second_run_same_jobs"`
	SameMirrors bool `json:"second_run_same_mirrors"`
}

type recordedConfig struct {
	Cases map[string]recordedCase `json:"cases"`
}

func loadVectors(t *testing.T) map[string]recordedConfig {
	t.Helper()
	raw, err := os.ReadFile("testdata/bootstrap_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]recordedConfig
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// now is the whole-second wall clock at start: enqueued jobs are due at the database clock, so the offsets are real-time.
var now = time.Now().UTC().Truncate(time.Second)

func naive(off int) string {
	return now.Add(time.Duration(off) * time.Second).Format("2006-01-02 15:04:05")
}
func aware(off int) string {
	return now.Add(time.Duration(off) * time.Second).Format(time.RFC3339)
}

// opt is a nullable offset.
func opt(f func(int) string, off *int) any {
	if off == nil {
		return nil
	}
	return f(*off)
}
func ip(i int) *int { return &i }

// iso is datetime.isoformat() of an aware UTC instant; day is a UTC date, days from today.
func iso(off int) string {
	return now.Add(time.Duration(off)*time.Second).Format("2006-01-02T15:04:05") + "+00:00"
}
func day(d int) string { return now.AddDate(0, 0, d).Format("2006-01-02") }

func owner(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	p, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

var resetTables = []string{"jobs", "audience_schedules", "audiences", "retention_schedules", "retention_policies",
	"playbook_schedules", "research_playbooks", "outreach_schedules", "outreach_inbound_schedules", "outreach_sequences",
	"outreach_enrollments", "outreach_sends"}

// seed runs stmts as the table owner (RLS and FKs off), after emptying every table the bootstraps touch.
func seed(t *testing.T, own *pgxpool.Pool, stmts ...[]any) {
	t.Helper()
	ctx := context.Background()
	tx, err := own.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SET LOCAL session_replication_role = replica"); err != nil {
		t.Fatal(err)
	}
	for _, tb := range resetTables {
		if _, err := tx.Exec(ctx, "DELETE FROM "+tb); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s[0].(string), s[1:]...); err != nil {
			t.Fatalf("seed %q: %v", s[0], err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func q(sql string, args ...any) []any { return append([]any{sql}, args...) }

const insJob = `INSERT INTO jobs (type, payload, status, priority, created_at, next_run_at, retry_count, max_retries, fire_key, executor)
 VALUES ($1, '{}', $2, 1, $3::timestamp, $3::timestamp, 0, 3, $4, 'python')`

// ---- normalisation, mirroring the recorder ----

var (
	isoRE  = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?\+00:00`)
	dateRE = regexp.MustCompile(`(?:^|:)(\d{4}-\d{2}-\d{2})$`)
)

// relOffset rounds v - now to the nearest 10 s, like the recorder.
func relOffset(v time.Time) int {
	s := v.Sub(now).Seconds() / 10
	r := int(s + 0.5)
	if s < 0 {
		r = int(s - 0.5)
	}
	return r * 10
}

func normKey(k string) string {
	k = isoRE.ReplaceAllStringFunc(k, func(m string) string {
		ts, err := time.Parse(time.RFC3339Nano, m)
		if err != nil {
			return m
		}
		return fmt.Sprintf("{t:%d}", relOffset(ts))
	})
	if m := dateRE.FindStringSubmatch(k); m != nil {
		d, _ := time.Parse("2006-01-02", m[1])
		days := int(d.Sub(time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)).Hours() / 24)
		k = k[:len(k)-len(m[1])] + fmt.Sprintf("{d:%d}", days)
	}
	return k
}

func normPayload(p map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range p {
		if s, ok := v.(string); ok && k == "fire_key" {
			v = normKey(s)
		}
		out[k] = v
	}
	return out
}

// jobIDs is the set of job ids currently in the table (the rows seeded before a bootstrap pass).
func jobIDs(t *testing.T, own *pgxpool.Pool) map[int64]bool {
	t.Helper()
	rows, err := own.Query(context.Background(), `SELECT id FROM jobs`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	ids := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids[id] = true
	}
	return ids
}

// dbClock is the database's wall clock, as a naive UTC time like the jobs timestamps.
func dbClock(t *testing.T, own *pgxpool.Pool) time.Time {
	t.Helper()
	var c time.Time
	if err := own.QueryRow(context.Background(), `SELECT clock_timestamp() AT TIME ZONE 'UTC'`).Scan(&c); err != nil {
		t.Fatal(err)
	}
	return c
}

// dumpJobs normalises next_run_at to the recorder's offsets from the test's fixed clock. A row the pass enqueued
// (not in seeded) is either stamped from that fixed clock (the Deps clock) or by the database at the moment of
// the pass (queue.EnqueueOnce: now()). The latter falls in [passStart, the dump's database time]: it is the pass's
// own clock, so it is normalised to offset 0 whatever the wall time between process start and the pass.
func dumpJobs(t *testing.T, own *pgxpool.Pool, seeded map[int64]bool, passStart time.Time) []recordedJob {
	t.Helper()
	passEnd := dbClock(t, own)
	rows, err := own.Query(context.Background(), `SELECT id, type, executor, status, payload::text, priority, max_retries, retry_count,
		next_run_at AT TIME ZONE 'UTC', fire_key, workspace_id FROM jobs ORDER BY type, fire_key`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []recordedJob
	for rows.Next() {
		var j recordedJob
		var payload string
		var next *time.Time
		var id int64
		if err := rows.Scan(&id, &j.Type, &j.Executor, &j.Status, &payload, &j.Priority, &j.MaxRetries, &j.RetryCount, &next, &j.FireKey, &j.WorkspaceID); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(payload), &j.Payload); err != nil {
			t.Fatal(err)
		}
		j.Payload = normPayload(j.Payload)
		if next != nil {
			if !seeded[id] && !next.Before(passStart) && !next.After(passEnd) {
				j.NextRunAt = ip(0)
			} else {
				j.NextRunAt = ip(relOffset(*next))
			}
		}
		if j.FireKey != nil {
			k := normKey(*j.FireKey)
			j.FireKey = &k
		}
		out = append(out, j)
	}
	return out
}

func dumpMirror(t *testing.T, own *pgxpool.Pool, table, cols, order string) []map[string]any {
	t.Helper()
	names := strings.Split(cols, ", ")
	rows, err := own.Query(context.Background(), fmt.Sprintf("SELECT %s FROM %s ORDER BY %s", cols, table, order))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]any{}
		for i, v := range vals {
			if ts, ok := v.(time.Time); ok {
				v = float64(relOffset(ts.UTC()))
			}
			if n, ok := v.(int32); ok {
				v = float64(n)
			}
			m[names[i]] = v
		}
		out = append(out, m)
	}
	return out
}

// recJobs / recMirror canonicalise the recorded rows for comparison.
func (c recordedCase) jobsCanon() []string { return canonJobs(c.Jobs) }

func canonJobs(js []recordedJob) []string {
	var out []string
	for _, j := range js {
		b, _ := json.Marshal(j)
		out = append(out, string(b))
	}
	sort.Strings(out)
	return out
}

func canonMirror(rows []map[string]any) []string {
	var out []string
	for _, r := range rows {
		b, _ := json.Marshal(r) // map keys marshal sorted
		out = append(out, string(b))
	}
	sort.Strings(out)
	return out
}

func asInt(v any) int {
	switch x := v.(type) {
	case bool:
		if x {
			return 1
		}
		return 0
	case float64:
		return int(x)
	case int:
		return x
	}
	return -999
}

// ---- configurations ----

func cfgFor(name string) (config.Config, func(context.Context, string) (bool, error)) {
	var c config.Config
	c.Flags.AutomationsEnabled = name != "python_flags_off"
	c.Flags.Outreach = config.OutreachSettings{InboundPollEnabled: name != "python_flags_off", InboundPollInterval: 900,
		InboundMaxConsecutiveFailures: 10, TickInterval: 900, TickMaxEnqueue: 200}
	if name == "go_cohort_wsA" {
		c.Executor = config.ExecutorSettings{Enabled: true, SignalScan: true, Workspaces: map[string]bool{"wsA": true}}
		c.Flags.Governance.GoExecutorRetention = true
		c.Flags.Governance.GoExecutorPlaybooks = true
		c.Flags.Audiences.GoExecutorAudienceRefresh = true
		c.Flags.Outreach.GoExecutorSend = true
		c.Flags.Outreach.GoExecutorInbound = true
	}
	imap := name != "python_no_imap"
	return c, func(context.Context, string) (bool, error) { return imap, nil }
}

func deps(pool *pgxpool.Pool, name string) schedboot.Deps {
	c, imap := cfgFor(name)
	return schedboot.Deps{Pool: pool, Cfg: c, Now: func() time.Time { return now }, IMAPConfigured: imap}
}

type scenario struct {
	key    string // case key in the vector file
	seed   [][]any
	run    func(d schedboot.Deps, ctx context.Context) (any, error)
	tables [][3]string // table, columns, order
	// adjust edits the recorded case into what Go must do (documented deviations from Python).
	adjust func(config string, c *recordedCase)
}

func scenarios() []scenario {
	var aud, ret, pb, out, inb [][]any
	_ = aud
	// audiences
	audRow := func(id, ws string, enabled bool) []any {
		return q(`INSERT INTO audiences (id, workspace_id, name, filters, refresh_enabled) VALUES ($1,$2,$1,'{}',$3)`, id, ws, enabled)
	}
	audMir := func(id, ws string, off *int, enabled bool) []any {
		return q(`INSERT INTO audience_schedules (audience_id, workspace_id, next_refresh_at, enabled) VALUES ($1,$2,$3::timestamp,$4)`, id, ws, opt(naive, off), enabled)
	}
	aud = [][]any{
		audRow("a1", "wsA", true), audMir("a1", "wsA", ip(-3600), true),
		audRow("a2", "wsB", true), audMir("a2", "wsB", nil, true),
		audRow("a3", "wsA", true), audMir("a3", "wsA", ip(3600), true),
		audRow("a4", "wsA", true), audMir("a4", "wsA", ip(-3600), false),
		audRow("a5", "wsA", false), audMir("a5", "wsA", ip(-3600), true),
		audMir("a6", "wsA", ip(-3600), true),
		audRow("a7", "wsB", true), audMir("a7", "wsB", ip(-3600), true),
		q(insJob, "audience_refresh", "pending", naive(0), "audience_refresh:a7:"+iso(-7200)),
		audRow("a_b", "wsA", true), audMir("a_b", "wsA", ip(-7200), true),
		audRow("axb", "wsA", true), audMir("axb", "wsA", ip(-7200), true),
		q(insJob, "audience_refresh", "pending", naive(0), "audience_refresh:axb:"+iso(-7200)),
	}
	// retention
	pol := func(ws string, enabled, hold bool) []any {
		return q(`INSERT INTO retention_policies (workspace_id, enabled, legal_hold, retention_days) VALUES ($1,$2,$3,'{}')`, ws, enabled, hold)
	}
	rsch := func(ws string, off *int, enabled bool) []any {
		return q(`INSERT INTO retention_schedules (workspace_id, enabled, next_run_at) VALUES ($1,$2,$3::timestamp)`, ws, enabled, opt(naive, off))
	}
	ret = [][]any{
		pol("wsA", true, false), rsch("wsA", ip(-86400), true),
		pol("wsB", true, true), rsch("wsB", ip(-86400), true),
		pol("wsC", true, false), rsch("wsC", ip(86400), true),
		pol("wsD", false, false), rsch("wsD", ip(-86400), true),
		pol("wsE", true, false), rsch("wsE", ip(-86400), true),
		q(insJob, "retention_enforce", "pending", naive(0), "retention:wsE:"+day(-3)),
		pol("wsF", true, false), rsch("wsF", nil, true),
		pol("wsG", true, false), rsch("wsG", ip(-86400), false),
		rsch("wsH", ip(-86400), true),
		pol("w_I", true, false), rsch("w_I", ip(-86400), true),
		pol("wsJ", true, false), rsch("wsJ", ip(-86400), true),
		q(insJob, "retention_enforce", "pending", naive(0), "retention:wsJ:"+day(-3)),
	}
	// playbooks
	pbRow := func(id, ws string, enabled bool, aud any) []any {
		return q(`INSERT INTO research_playbooks (id, workspace_id, name, description, prompt_template, output_format, max_steps,
		  cell_budget_usd, version, enabled, schedule_audience_id) VALUES ($1,$2,$1,'','p','text',3,0.1,1,$3,$4)`, id, ws, enabled, aud)
	}
	pbMir := func(id, ws string, off *int, enabled bool) []any {
		return q(`INSERT INTO playbook_schedules (playbook_id, workspace_id, enabled, next_run_at) VALUES ($1,$2,$3,$4::timestamp)`, id, ws, enabled, opt(naive, off))
	}
	pb = [][]any{
		q(`INSERT INTO audiences (id, workspace_id, name, filters) VALUES ('aud1','wsA','aud1','{}'), ('aud2','wsB','aud2','{}')`),
		pbRow("p1", "wsA", true, "aud1"), pbMir("p1", "wsA", ip(-3600), true),
		pbRow("p2", "wsB", true, nil), pbMir("p2", "wsB", ip(-3600), true),
		pbRow("p3", "wsA", true, "aud1"), pbMir("p3", "wsA", ip(-3600), true),
		q(insJob, "research_playbook_schedule", "processing", naive(0), "playbook_schedule:p3:"+iso(-7200)),
		pbRow("p4", "wsA", true, "aud1"), pbMir("p4", "wsA", nil, true),
		pbRow("p5", "wsB", false, "aud1"), pbMir("p5", "wsB", ip(-3600), true),
		pbRow("p6", "wsA", true, "aud1"), pbMir("p6", "wsA", ip(3600), true),
		pbRow("p7", "wsA", true, "aud1"), pbMir("p7", "wsA", ip(-3600), false),
		pbMir("p8", "wsA", ip(-3600), true),
	}
	// outreach ticker
	seq := func(id, ws, status string) []any {
		return q(`INSERT INTO outreach_sequences (id, workspace_id, name, steps, status) VALUES ($1,$2,$1,'[]',$3)`, id, ws, status)
	}
	osch := func(id, ws string, off *int, enabled bool) []any {
		return q(`INSERT INTO outreach_schedules (sequence_id, workspace_id, next_tick_at, enabled) VALUES ($1,$2,$3::timestamptz,$4)`, id, ws, opt(aware, off), enabled)
	}
	enr := func(id int, seqID, ws string, lead int) []any {
		return q(`INSERT INTO outreach_enrollments (id, workspace_id, sequence_id, lead_id, to_email_snapshot, current_step, status, next_send_at)
		  VALUES ($1,$2,$3,$4,'x@example.test',0,'pending',$5::timestamptz)`, id, ws, seqID, lead, aware(-60))
	}
	out = [][]any{
		seq("s1", "wsA", "active"), osch("s1", "wsA", ip(-600), true),
		seq("s2", "wsB", "paused"), osch("s2", "wsB", ip(-600), true),
		seq("s3", "wsA", "active"), osch("s3", "wsA", ip(600), true),
		seq("s4", "wsA", "active"), osch("s4", "wsA", ip(-600), false),
		osch("s5", "wsA", ip(-600), true),
		seq("s6", "wsB", "active"), osch("s6", "wsB", nil, true),
		enr(100, "s1", "wsA", 7), enr(101, "s6", "wsB", 8),
	}
	// outreach inbound
	imir := func(ws string, off *int, enabled bool, fails int) []any {
		return q(`INSERT INTO outreach_inbound_schedules (workspace_id, next_poll_at, enabled, consecutive_failures) VALUES ($1,$2::timestamptz,$3,$4)`,
			ws, opt(aware, off), enabled, fails)
	}
	inb = [][]any{
		imir("wsA", ip(-600), true, 3), imir("wsB", nil, true, 0), imir("wsC", ip(600), true, 0), imir("wsD", ip(-600), false, 0),
		seq("sq", "wsE", "active"), osch("sq", "wsE", ip(600), true),
		seq("sq2", "wsF", "active"), osch("sq2", "wsF", ip(600), false),
		imir("wsG", ip(-600), true, 0),
	}
	// signal_scan seeds
	scanJob := func(status string) [][]any {
		return [][]any{q(insJob, "signal_scan", status, naive(0), "signal_scan:old")}
	}
	scan := func(key string, seed [][]any) scenario {
		return scenario{key: "signal_scan/" + key, seed: seed,
			run: func(d schedboot.Deps, ctx context.Context) (any, error) { return d.SignalScan(ctx) }}
	}
	return []scenario{
		scan("empty", nil), scan("pending_exists", scanJob("pending")), scan("processing_exists", scanJob("processing")),
		scan("only_completed", scanJob("completed")),
		{key: "audiences/mixed", seed: aud, run: func(d schedboot.Deps, ctx context.Context) (any, error) { return d.Audiences(ctx) },
			tables: [][3]string{{"audience_schedules", "audience_id, workspace_id, next_refresh_at, enabled", "audience_id"}},
			adjust: func(cfgName string, c *recordedCase) {
				// Python's active-job check is a LIKE with '_' as a wildcard: the pending job of audience
				// "axb" suppressed the refresh of "a_b". Go matches the id exactly, so a_b is enqueued.
				c.First = float64(asInt(c.First) + 1)
				exec := "python"
				if cfgName == "go_cohort_wsA" {
					exec = "go"
				}
				c.Jobs = append(c.Jobs, recordedJob{Type: "audience_refresh", Executor: exec, Status: "pending",
					Payload:  map[string]any{"audience_id": "a_b", "workspace_id": "wsA", "fire_key": "audience_refresh:a_b:{t:-7200}"},
					Priority: 1, MaxRetries: 3, NextRunAt: ip(0), FireKey: sp("audience_refresh:a_b:{t:-7200}"), WorkspaceID: sp("wsA")})
			}},
		{key: "retention/mixed", seed: ret, run: func(d schedboot.Deps, ctx context.Context) (any, error) { return d.Retention(ctx) },
			tables: [][3]string{{"retention_schedules", "workspace_id, enabled, next_run_at", "workspace_id"}}},
		{key: "playbooks/mixed", seed: pb, run: func(d schedboot.Deps, ctx context.Context) (any, error) { return d.Playbooks(ctx) },
			tables: [][3]string{{"playbook_schedules", "playbook_id, workspace_id, enabled, next_run_at", "playbook_id"}}},
		{key: "outreach/mixed", seed: out, run: func(d schedboot.Deps, ctx context.Context) (any, error) { return d.Outreach(ctx) },
			tables: [][3]string{{"outreach_schedules", "sequence_id, workspace_id, next_tick_at, enabled", "sequence_id"}}},
		{key: "outreach_inbound/mixed", seed: inb, run: func(d schedboot.Deps, ctx context.Context) (any, error) { return d.OutreachInbound(ctx) },
			tables: [][3]string{{"outreach_inbound_schedules", "workspace_id, next_poll_at, enabled, consecutive_failures", "workspace_id"}}},
	}
}

func sp(s string) *string { return &s }

// adjustWorkspace applies Go's one deliberate row difference: jobs.workspace_id is set from the payload
// (Python wrote NULL), so the worker's payload-vs-row workspace check is not vacuous (S-L29).
func adjustWorkspace(c *recordedCase) {
	for i := range c.Jobs {
		j := &c.Jobs[i]
		if j.Type == "send" && j.FireKey == nil {
			// plan 09: Go's EnqueueSend single-flights on the idempotency key as the fire key
			// (Python left fire_key NULL and deduplicated by a payload lookup).
			k := j.Payload["idempotency_key"].(string)
			j.FireKey = &k
			j.Payload["fire_key"] = k
		}
		if j.WorkspaceID == nil && j.Type != "signal_scan" && len(j.Payload) > 0 {
			if ws, ok := j.Payload["workspace_id"].(string); ok {
				j.WorkspaceID = &ws
			}
		}
	}
}

func TestBootstrapsMatchPythonRecordings(t *testing.T) {
	vec := loadVectors(t)
	pool := testpg.NewAppPool(t, 16)
	own := owner(t)
	ctx := context.Background()
	for _, cfgName := range []string{"python", "go_cohort_wsA", "python_no_imap", "python_flags_off"} {
		for _, sc := range scenarios() {
			rec, ok := vec[cfgName].Cases[sc.key]
			if !ok {
				continue
			}
			t.Run(cfgName+"/"+sc.key, func(t *testing.T) {
				seed(t, own, sc.seed...)
				seeded := jobIDs(t, own)
				passStart := dbClock(t, own)
				d := deps(pool, cfgName)
				first, err := sc.run(d, ctx)
				if err != nil {
					t.Fatalf("first run: %v", err)
				}
				jobs1 := dumpJobs(t, own, seeded, passStart)
				mir1 := map[string][]map[string]any{}
				for _, tb := range sc.tables {
					mir1[tb[0]] = dumpMirror(t, own, tb[0], tb[1], tb[2])
				}
				second, err := sc.run(d, ctx)
				if err != nil {
					t.Fatalf("second run: %v", err)
				}
				jobs2 := dumpJobs(t, own, seeded, passStart)

				want := rec
				adjustWorkspace(&want)
				if sc.adjust != nil {
					sc.adjust(cfgName, &want)
				}
				if got, exp := asInt(first), asInt(want.First); got != exp {
					t.Errorf("first run returned %v, Python %v", first, want.First)
				}
				if got, exp := asInt(second), asInt(want.Second); got != exp {
					t.Errorf("second run returned %v, Python %v", second, want.Second)
				}
				if g, w := canonJobs(jobs1), want.jobsCanon(); !reflect.DeepEqual(g, w) {
					t.Errorf("job rows differ\n go:     %s\n python: %s", strings.Join(g, "\n         "), strings.Join(w, "\n         "))
				}
				for tb, rows := range mir1 {
					exp := canonMirror(normMirror(want.Mirrors[tb]))
					if g := canonMirror(rows); !reflect.DeepEqual(g, exp) {
						t.Errorf("%s differs\n go:     %v\n python: %v", tb, g, exp)
					}
				}
				if !reflect.DeepEqual(canonJobs(jobs1), canonJobs(jobs2)) {
					t.Errorf("second run changed the job rows (not idempotent)")
				}
			})
		}
	}
}

// normMirror converts the recorded ints (JSON numbers) to float64 like the dump.
func normMirror(rows []map[string]any) []map[string]any { return rows }

var _ = strconv.Itoa
var _ pgx.Tx
