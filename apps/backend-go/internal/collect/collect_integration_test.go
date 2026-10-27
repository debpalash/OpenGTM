//go:build integration

package collect

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/browserclient"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/collectledger"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

const (
	itWS  = "ws-wpcollect-a"
	itWS2 = "ws-wpcollect-b"
)

func itSetup(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	ctx := context.Background()
	clean := func() {
		for _, w := range []string{itWS, itWS2} {
			_ = tenancy.InWorkspace(ctx, pool, w, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `DELETE FROM collection_jobs`); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, `DELETE FROM leads`)
				return err
			})
		}
		_, _ = pool.Exec(ctx, `DELETE FROM jobs WHERE workspace_id IN ('ws-wpcollect-a','ws-wpcollect-b')`)
		_, _ = pool.Exec(ctx, `DELETE FROM source_stats WHERE region LIKE 'wpcollect-%'`)
	}
	clean()
	t.Cleanup(clean)
	return pool, ctx
}

func itJob(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ws, id, query string) *queue.Claim {
	t.Helper()
	if err := tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
		return collectledger.Create(ctx, tx, ws, id, query, "market_search", "{}", time.Now().UTC())
	}); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]string{"job_id": id, "query": query, "intent": "market_search",
		"workspace_id": ws, "slug": "s"})
	if _, err := Enqueue(ctx, pool, JobTypeCollect, ws, "go", json.RawMessage(payload), collectledger.CollectFireKey(ws, id)); err != nil {
		t.Fatal(err)
	}
	return &queue.Claim{Type: JobTypeCollect, WorkspaceID: ws, Payload: payload}
}

func itDeps(t *testing.T, pool *pgxpool.Pool, spec jobVector, places any, em *fakeEmitter, enrich Enrichers) Deps {
	var maps map[string]struct {
		Query, City string
		Places      []browserclient.MapPlace
	}
	raw, _ := json.Marshal(places)
	_ = json.Unmarshal(raw, &maps)
	h := newHarness(t, spec.Calls, spec.Spec.LLM == "down", fakeSources{enabled: spec.Spec.Enabled},
		&fakeBrowser{w: loadWorld(t), places: maps})
	if enrich != nil {
		h.runner.Enrich = enrich
	}
	return Deps{Pool: pool, For: func(string) *Runner { return h.runner }, Emit: func(string) Emitter { return em }}
}

func loadJobs(t *testing.T) ([]jobVector, any) {
	var v struct {
		Jobs []jobVector `json:"jobs"`
		Maps any         `json:"maps"`
	}
	load(t, "job.json", &v)
	return v.Jobs, v.Maps
}

func jobNamed(t *testing.T, name string) (jobVector, any) {
	jobs, maps := loadJobs(t)
	for _, j := range jobs {
		if j.Spec.Name == name {
			return j, maps
		}
	}
	t.Fatalf("no job vector %s", name)
	return jobVector{}, nil
}

type pgStage struct {
	Stage, Status                          string
	InputCount, OutputCount, RejectedCount int32
	Details                                any
}

func readJob(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ws, id string) (*collectledger.JobDetail, []pgStage) {
	t.Helper()
	var d *collectledger.JobDetail
	if err := tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
		var err error
		d, err = collectledger.Detail(ctx, tx, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var out []pgStage
	for _, s := range d.Stages {
		var det any
		_ = json.Unmarshal(s.Details, &det)
		out = append(out, pgStage{s.Stage, s.Status, s.InputCount, s.OutputCount, s.RejectedCount, det})
	}
	return d, out
}

func countLeads(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ws, where string, args ...any) int {
	t.Helper()
	var n int
	if err := tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM leads WHERE `+where, args...).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

// A whole collect job on Postgres over the recorded fake world produces the recorded stage rows,
// leads and progress, and finishes the ledger row as done.
func TestCollectJobOnPostgres(t *testing.T) {
	pool, ctx := itSetup(t)
	spec, maps := jobNamed(t, "location_all_sources")
	em := &fakeEmitter{}
	d := itDeps(t, pool, spec, maps, em, nil)
	// Existing workspace leads take part in cross-job dedup.
	for _, c := range spec.Spec.Existing {
		if err := (PgStore{pool, itWS}).Upsert(ctx, NewLead(Lead{Company: c, City: "Old"})); err != nil {
			t.Fatal(err)
		}
	}
	c := itJob(t, ctx, pool, itWS, "job1", spec.Spec.Query)
	if err := HandleCollect(ctx, d, c); err != nil {
		t.Fatal(err)
	}
	job, stages := readJob(t, ctx, pool, itWS, "job1")
	if job.Status != "done" || int(job.LeadsFound) != len(spec.Stored) || job.Attempts != 1 || job.StartedAt == "" || job.CompletedAt == "" {
		t.Fatalf("job row: %+v", job)
	}
	if len(stages) != len(spec.Stages) {
		t.Fatalf("%d stages, want %d", len(stages), len(spec.Stages))
	}
	for i, s := range stages {
		w := spec.Stages[i]
		if s.Stage != w["stage"] || s.Status != w["status"] || float64(s.InputCount) != w["input_count"] ||
			float64(s.OutputCount) != w["output_count"] || float64(s.RejectedCount) != w["rejected_count"] ||
			!reflect.DeepEqual(jsonNorm(s.Details), jsonNorm(w["details"])) {
			t.Errorf("stage %d: %+v want %v", i, s, w)
		}
	}
	if n := countLeads(t, ctx, pool, itWS, `collection_job_id = 'job1'`); n != len(spec.Stored) {
		t.Errorf("%d stored leads, want %d", n, len(spec.Stored))
	}
	for _, w := range spec.Stored {
		var got map[string]any
		if err := tenancy.InWorkspace(ctx, pool, itWS, func(tx pgx.Tx) error {
			var raw string
			err := tx.QueryRow(ctx, `SELECT row_to_json(l)::text FROM (SELECT company, website, email, phone, city,
				score, score_tier, source, source_url, decision_makers, collection_job_id FROM leads
				WHERE company = $1 AND city = $2) l`, w["company"], w["city"]).Scan(&raw)
			if err == nil {
				err = json.Unmarshal([]byte(raw), &got)
			}
			return err
		}); err != nil {
			t.Fatalf("%v: %v", w["company"], err)
		}
		for k, v := range got {
			if !reflect.DeepEqual(v, w[k]) {
				t.Errorf("%v.%s = %#v want %#v", w["company"], k, v, w[k])
			}
		}
	}
	compareEvents(t, "pg", em.events, spec.Events)
	var runs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM source_stats WHERE region = 'india' AND source = 'google_maps'`).Scan(&runs); err != nil || runs != 1 {
		t.Errorf("source_stats google_maps/india rows = %d %v", runs, err)
	}
}

// cancelOnProvider cancels the job (as DELETE-free POST /api/jobs/{id}/cancel does) the first time a
// provider stage runs, i.e. during enrichment, after the last legacy cancel check.
type cancelOnProvider struct {
	fakeEnrich
	once func()
}

func (c cancelOnProvider) Provider(ctx context.Context, ws, name string, l *Lead) (ProviderResult, error) {
	c.once()
	return c.fakeEnrich.Provider(ctx, ws, name, l)
}

// Fix: a cancel during enrichment no longer stores the leads or flips the job to done.
func TestCollectCancelDuringEnrichment(t *testing.T) {
	pool, ctx := itSetup(t)
	spec, maps := jobNamed(t, "location_all_sources")
	done := false
	cancel := func() {
		if done {
			return
		}
		done = true
		if err := tenancy.InWorkspace(ctx, pool, itWS, func(tx pgx.Tx) error {
			_, err := collectledger.Cancel(ctx, tx, "jobc", time.Now().UTC())
			return err
		}); err != nil {
			t.Error(err)
		}
	}
	em := &fakeEmitter{}
	d := itDeps(t, pool, spec, maps, em, cancelOnProvider{once: cancel})
	c := itJob(t, ctx, pool, itWS, "jobc", spec.Spec.Query)
	if err := HandleCollect(ctx, d, c); err != nil {
		t.Fatal(err)
	}
	job, stages := readJob(t, ctx, pool, itWS, "jobc")
	if job.Status != "cancelled" || job.Error != "Cancelled by user" || job.LeadsFound != 0 {
		t.Fatalf("job: %+v", job)
	}
	for _, s := range stages {
		if s.Stage == "store" {
			t.Error("store stage ran after cancel")
		}
	}
	if n := countLeads(t, ctx, pool, itWS, `collection_job_id = 'jobc'`); n != 0 {
		t.Errorf("%d leads stored after cancel", n)
	}
	// A cancelled job's next delivery is a no-op.
	if err := HandleCollect(ctx, d, c); err != nil {
		t.Fatal(err)
	}
	if j, _ := readJob(t, ctx, pool, itWS, "jobc"); j.Attempts != 1 {
		t.Errorf("cancelled job ran again: attempts %d", j.Attempts)
	}
}

// The intent gate fails the job without running sources; the queue job succeeds (no retry).
func TestCollectBlockedIntent(t *testing.T) {
	pool, ctx := itSetup(t)
	spec, maps := jobNamed(t, "blocked_intent")
	em := &fakeEmitter{}
	d := itDeps(t, pool, spec, maps, em, nil)
	c := itJob(t, ctx, pool, itWS, "jobb", spec.Spec.Query)
	if err := HandleCollect(ctx, d, c); err != nil {
		t.Fatal(err)
	}
	job, stages := readJob(t, ctx, pool, itWS, "jobb")
	if job.Status != "failed" || !strings.HasPrefix(job.Error, "collection_intent_blocked: ") || len(stages) != 0 {
		t.Fatalf("job: %+v stages %v", job, stages)
	}
}

func TestCollectMissingLedgerRow(t *testing.T) {
	pool, ctx := itSetup(t)
	payload, _ := json.Marshal(map[string]string{"job_id": "ghost", "query": "q", "workspace_id": itWS, "slug": "s"})
	err := HandleCollect(ctx, Deps{Pool: pool}, &queue.Claim{WorkspaceID: itWS, Payload: payload})
	if err == nil || err.Error() != "collection job ghost not found in workspace ledger" {
		t.Fatalf("err = %v", err)
	}
}

// reconcile_collect_job_failure: retry requeues, the final failure fails, a cancelled or missing job
// is left alone; the tenant is the failed row's workspace.
func TestCollectFailureHook(t *testing.T) {
	pool, ctx := itSetup(t)
	hook := failureHook(pool)
	itJob(t, ctx, pool, itWS, "jf", "q")
	PgLedger{pool, itWS}.MarkRunning(ctx, "jf")
	notice := func(retry bool) queue.FailureNotice {
		return queue.FailureNotice{JobType: JobTypeCollect, WorkspaceID: itWS, WillRetry: retry, Reason: "job timed out",
			Payload: []byte(`{"job_id":"jf","workspace_id":"ws-wpcollect-a","slug":"s"}`)}
	}
	if err := hook(ctx, notice(true)); err != nil {
		t.Fatal(err)
	}
	j, _ := readJob(t, ctx, pool, itWS, "jf")
	if j.Status != "pending" || j.Error != "Queue retry scheduled: job timed out" || j.CompletedAt != "" {
		t.Fatalf("retry: %+v", j)
	}
	if err := hook(ctx, notice(false)); err != nil {
		t.Fatal(err)
	}
	j, _ = readJob(t, ctx, pool, itWS, "jf")
	if j.Status != "failed" || j.Error != "Final failure: job timed out" || j.CompletedAt == "" {
		t.Fatalf("final: %+v", j)
	}
	// Another workspace's row with this payload never touches ws-a.
	n := notice(true)
	n.WorkspaceID = itWS2
	if err := hook(ctx, n); err == nil {
		t.Error("cross-workspace notice accepted")
	}
	if j2, _ := readJob(t, ctx, pool, itWS, "jf"); j2.Status != "failed" {
		t.Errorf("status changed by a foreign notice: %s", j2.Status)
	}
	// Cancelled jobs stay cancelled.
	itJob(t, ctx, pool, itWS, "jx", "q")
	_ = tenancy.InWorkspace(ctx, pool, itWS, func(tx pgx.Tx) error {
		_, err := collectledger.Cancel(ctx, tx, "jx", time.Now().UTC())
		return err
	})
	n = notice(true)
	n.Payload = []byte(`{"job_id":"jx","workspace_id":"ws-wpcollect-a","slug":"s"}`)
	if err := hook(ctx, n); err != nil {
		t.Fatal(err)
	}
	if j, _ := readJob(t, ctx, pool, itWS, "jx"); j.Status != "cancelled" {
		t.Errorf("cancelled job reconciled to %s", j.Status)
	}
}

// Fix: complete_job and fail_job no longer overwrite a cancelled job; fail_job requeues while
// attempts remain.
func TestLedgerGuards(t *testing.T) {
	pool, ctx := itSetup(t)
	l := PgLedger{pool, itWS}
	itJob(t, ctx, pool, itWS, "jg", "q")
	_ = l.MarkRunning(ctx, "jg")
	if err := l.FailJob(ctx, "jg", "boom"); err != nil {
		t.Fatal(err)
	}
	j, _ := readJob(t, ctx, pool, itWS, "jg")
	if j.Status != "pending" || j.Error != "boom" || j.CompletedAt != "" {
		t.Fatalf("fail_job with attempts left: %+v", j)
	}
	_ = l.MarkRunning(ctx, "jg")
	_ = tenancy.InWorkspace(ctx, pool, itWS, func(tx pgx.Tx) error {
		_, err := collectledger.Cancel(ctx, tx, "jg", time.Now().UTC())
		return err
	})
	_ = l.CompleteJob(ctx, "jg", 9)
	_ = l.FailJob(ctx, "jg", "late")
	if j, _ := readJob(t, ctx, pool, itWS, "jg"); j.Status != "cancelled" || j.LeadsFound != 0 || j.Error != "Cancelled by user" {
		t.Fatalf("cancelled job overwritten: %+v", j)
	}
	if st, _ := l.Status(ctx, "missing"); st != "" {
		t.Errorf("missing status %q", st)
	}
	// RLS: another workspace cannot see the job.
	if st, _ := (PgLedger{pool, itWS2}).Status(ctx, "jg"); st != "" {
		t.Errorf("cross-workspace status %q", st)
	}
}

func TestUpsertAndSourceStats(t *testing.T) {
	pool, ctx := itSetup(t)
	s := PgStore{pool, itWS}
	a := NewLead(Lead{Company: "Acme", City: "Pune", Email: "a@acme.com", Score: 50, CreatedAt: "2020-01-01T00:00:00+00:00"})
	if err := s.Upsert(ctx, a); err != nil {
		t.Fatal(err)
	}
	b := NewLead(Lead{Company: "Acme", City: "Pune", Phone: "123", Score: 70})
	if err := s.Upsert(ctx, b); err != nil || b.ID != a.ID {
		t.Fatalf("upsert overwrite: %v %d %d", err, a.ID, b.ID)
	}
	var email, phone, created string
	var score int32
	_ = tenancy.InWorkspace(ctx, pool, itWS, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT email, phone, created_at, score FROM leads WHERE id = $1`, a.ID).Scan(&email, &phone, &created, &score)
	})
	// A re-found company fills empty columns only; the score is user-owned (review 3a L1).
	if email != "a@acme.com" || phone != "123" || created != "2020-01-01T00:00:00+00:00" || score != 50 {
		t.Errorf("upsert: %q %q %q %d", email, phone, created, score)
	}
	names, err := s.ExistingCompanies(ctx)
	if err != nil || !names["acme"] || len(names) != 1 {
		t.Errorf("existing: %v %v", names, err)
	}
	if other, _ := (PgStore{pool, itWS2}).ExistingCompanies(ctx); len(other) != 0 {
		t.Errorf("cross-workspace companies: %v", other)
	}
	region := "wpcollect-r"
	for i := 0; i < 6; i++ {
		if err := s.RecordSourceRun(ctx, region, []SourceRun{{"web_search", SourceCounters{4, 2, 1}}, {"x", SourceCounters{}}}); err != nil {
			t.Fatal(err)
		}
	}
	var runs, withOut, emitted int
	var rel float64
	_ = pool.QueryRow(ctx, `SELECT runs, runs_with_output, emitted, reliability FROM source_stats WHERE source='web_search' AND region=$1`, region).
		Scan(&runs, &withOut, &emitted, &rel)
	if runs != 6 || withOut != 6 || emitted != 24 || rel < 0.6874 || rel > 0.6876 {
		t.Errorf("source_stats: %d %d %d %v", runs, withOut, emitted, rel)
	}
	m, err := s.Reliability(ctx, region)
	if err != nil || len(m) != 2 || m["x"] != 0 {
		t.Errorf("reliability map %v %v", m, err)
	}
}

// bulk_enrich writes changed fields with an ISO last_enriched_at (fixes: legacy wrote nothing,
// and "now" as text).
func TestBulkEnrichOnPostgres(t *testing.T) {
	pool, ctx := itSetup(t)
	s := PgStore{pool, itWS}
	w := loadWorld(t)
	// a company whose email search the world answers: find_email_via_search(company, city, domain)
	var company, want string
	for q, hits := range w.Searches {
		if !strings.HasSuffix(q, " email contact") {
			continue
		}
		f := &fakeWeb{w: w}
		if e := FindEmailViaSearch(ctx, f, strings.TrimSuffix(q, " email contact"), "", ""); e != "" {
			company, want = strings.TrimSuffix(q, " email contact"), e
			_ = hits
			break
		}
	}
	if company == "" {
		t.Skip("world has no company-email search")
	}
	l1 := NewLead(Lead{Company: company})
	l2 := NewLead(Lead{Company: "Has Mail", Email: "x@y.com"})
	for _, l := range []*Lead{l1, l2} {
		if err := s.Upsert(ctx, l); err != nil {
			t.Fatal(err)
		}
	}
	h := newHarness(t, nil, false, fakeSources{}, nil)
	em := &fakeEmitter{}
	d := Deps{Pool: pool, For: func(string) *Runner { return h.runner }, Emit: func(string) Emitter { return em }}
	payload, _ := json.Marshal(map[string]any{"job_id": "b1", "ids": []int64{l1.ID, l2.ID, 999999999, 99999999999},
		"action": "find_emails", "workspace_id": itWS, "slug": "s"})
	if err := HandleBulkEnrich(ctx, d, &queue.Claim{WorkspaceID: itWS, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	var email, last, conf, prov string
	_ = tenancy.InWorkspace(ctx, pool, itWS, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT email, last_enriched_at, email_confidence, email_provider FROM leads WHERE id = $1`,
			l1.ID).Scan(&email, &last, &conf, &prov)
	})
	if email != want || !strings.HasSuffix(last, "+00:00") {
		t.Errorf("find_emails wrote %q / %q, want %q / iso", email, last, want)
	}
	// Review 3a L4: the email is stored with its confidence and source, so outreach can tell a
	// searched address from an unverified info@ guess.
	if conf != "verified" || prov != "email_finder" {
		t.Errorf("find_emails confidence/source = %q / %q, want verified / email_finder", conf, prov)
	}
	types := []string{}
	for _, e := range em.events {
		types = append(types, e["type"].(string))
	}
	if strings.Join(types, ",") != "bulk_enrich_started,bulk_enrich_progress,bulk_enrich_progress,bulk_enrich_done" ||
		em.events[3]["updated"] != float64(1) {
		t.Errorf("events: %v", em.events)
	}
}
