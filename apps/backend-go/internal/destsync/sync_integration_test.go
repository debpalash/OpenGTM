//go:build integration

package destsync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/adsdrv"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/destdeliver"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/deststore"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/warehouse"
)

const (
	wsVec  = "ws-vec" // the workspace the Python transcripts ran in
	wsOth  = "ws-other"
	audID  = "aud-1"
	destID = "dest-1"
)

// ---------------------------------------------------------------- scripted ports

type entry struct {
	Success    bool   `json:"success"`
	Summary    string `json:"summary"`
	Error      string `json:"error"`
	ExternalID string `json:"external_id"`
	Raise      string `json:"raise"`
}

// fakePorts implements Deliverer, AdSyncer and WarehouseSyncer from a script and records
// every call in the transcript shape of sync_flows.json.
type fakePorts struct {
	mu     sync.Mutex
	script map[string][]entry
	calls  []map[string]any
	n      int
	before func(kind string)   // runs first inside the call (outside the engine's transactions)
	after  func(n int)         // runs after call n (0-based) returned
	skip   map[string]struct{} // unused
}

func newFake(script map[string][]entry) *fakePorts { return &fakePorts{script: script} }

func (f *fakePorts) pop(kind string) entry {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := f.script[kind]
	if len(q) == 0 {
		return entry{Success: true}
	}
	e := q[0]
	f.script[kind] = q[1:]
	return e
}

func roundTrip(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		panic(err)
	}
	return out
}

func (f *fakePorts) record(call map[string]any) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, roundTrip(call).(map[string]any))
	f.n++
	return f.n - 1
}

func (f *fakePorts) done(i int) {
	if f.after != nil {
		f.after(i)
	}
}

func (f *fakePorts) Deliver(ctx context.Context, dest destdeliver.Destination, leadID int, snapshot map[string]any, idem string) destdeliver.Result {
	if f.before != nil {
		f.before("deliver")
	}
	i := f.record(map[string]any{"kind": "deliver", "destination_id": dest.ID, "lead_id": leadID, "snapshot": snapshot, "idempotency_key": idem})
	e := f.pop("deliver")
	defer f.done(i)
	if e.Raise != "" { // Python's _deliver raised; the engine's catch is the Deliverer's contract here
		return destdeliver.Result{Error: trunc(e.Raise, 500)}
	}
	return destdeliver.Result{Success: e.Success, Summary: e.Summary, Error: e.Error, ExternalID: e.ExternalID}
}

func (f *fakePorts) SyncBatch(ctx context.Context, ws, destType string, cfg map[string]any, snaps []map[string]any, op string) (adsdrv.Result, error) {
	if f.before != nil {
		f.before("ad")
	}
	i := f.record(map[string]any{"kind": "ad_batch", "workspace_id": ws, "type": destType, "config": cfg, "operation": op, "snapshots": snaps})
	e := f.pop("ad")
	defer f.done(i)
	if e.Raise != "" {
		return adsdrv.Result{}, errors.New(e.Raise)
	}
	return adsdrv.Result{Success: e.Success, Summary: e.Summary, Error: e.Error, ExternalID: e.ExternalID}, nil
}

func (f *fakePorts) Sync(ctx context.Context, ws string, dest warehouse.DestRef, b *warehouse.Buffer) (warehouse.Result, error) {
	if f.before != nil {
		f.before("warehouse")
	}
	body, _ := io.ReadAll(b.Reader())
	sum := sha256.Sum256(body)
	lines := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
	i := f.record(map[string]any{"kind": "warehouse_export", "workspace_id": ws, "manifest": b.Manifest(), "row_count": b.RowCount(),
		"body_sha256": hex.EncodeToString(sum[:]), "lines": lines})
	e := f.pop("warehouse")
	defer f.done(i)
	if e.Raise != "" {
		return warehouse.Result{}, errors.New(e.Raise)
	}
	return warehouse.Result{Success: e.Success, Summary: e.Summary, Error: e.Error, ExternalID: e.ExternalID}, nil
}

func trunc(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// ---------------------------------------------------------------- harness

type harness struct {
	t          *testing.T
	ctx        context.Context
	pool       *pgxpool.Pool
	owner      *pgxpool.Pool
	fake       *fakePorts
	eng        *Engine
	logs       *bytes.Buffer
	leaseCalls int
	lease      func(n int) bool
}

func cleanWS(ctx context.Context, pool *pgxpool.Pool) {
	for _, w := range []string{wsVec, wsOth} {
		_ = tenancy.InWorkspace(ctx, pool, w, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM audiences`) // cascades members, destinations, runs, deliveries
			return err
		})
	}
}

func newHarness(t *testing.T, script map[string][]entry, tune func(*Deps)) *harness {
	t.Helper()
	pool := testpg.NewAppPool(t, 24)
	ctx := context.Background()
	owner, err := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	cleanWS(ctx, pool)
	t.Cleanup(func() { cleanWS(ctx, pool) })
	h := &harness{t: t, ctx: ctx, pool: pool, owner: owner, fake: newFake(script), logs: &bytes.Buffer{}}
	d := Deps{
		Pool: pool, Deliverer: h.fake, Ads: h.fake, Warehouse: h.fake,
		LeaseEvery: -1, Log: slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Lease: func(context.Context, *queue.Claim) (bool, error) {
			h.leaseCalls++
			if h.lease != nil {
				return h.lease(h.leaseCalls), nil
			}
			return true, nil
		},
	}
	if tune != nil {
		tune(&d)
	}
	h.eng = New(d)
	return h
}

func (h *harness) tx(ws string, fn func(pgx.Tx) error) {
	h.t.Helper()
	if err := tenancy.InWorkspace(h.ctx, h.pool, ws, fn); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) exec(ws, sql string, args ...any) {
	h.t.Helper()
	h.tx(ws, func(tx pgx.Tx) error { _, err := tx.Exec(h.ctx, sql, args...); return err })
}

type destSeed struct {
	ID, Type, Config, FieldMap, Health string
	Disabled                           bool
}

func (h *harness) seedAudience(ws string) {
	h.exec(ws, `INSERT INTO audiences (id, workspace_id, name, filters) VALUES ($1,$2,'a','{}'::json) ON CONFLICT DO NOTHING`, audID, ws)
}

func (h *harness) seedDest(ws string, d destSeed) {
	if d.ID == "" {
		d.ID = destID
	}
	if d.Config == "" {
		d.Config = `{"url":"https://hooks.example.test/x"}`
	}
	if d.FieldMap == "" {
		d.FieldMap = `{}`
	}
	if d.Health == "" {
		d.Health = "unverified"
	}
	if d.Type == "" {
		d.Type = "webhook"
	}
	h.seedAudience(ws)
	h.exec(ws, `INSERT INTO audience_destinations (id, workspace_id, audience_id, name, destination_type, enabled, config, field_map, health_status)
		VALUES ($1,$2,$3,$1,$4,$5,$6::json,$7::json,$8)`, d.ID, ws, audID, d.Type, !d.Disabled, d.Config, d.FieldMap, d.Health)
}

func (h *harness) seedRun(ws, id, status string, startedAt string) {
	var st any
	if startedAt != "" {
		st = startedAt
	}
	h.exec(ws, `INSERT INTO destination_runs (id, workspace_id, destination_id, status, started_at, created_at)
		VALUES ($1,$2,$3,$4,$5::timestamp,'2026-08-02 09:00:00')`, id, ws, destID, status, st)
}

// seedMembers inserts members 1..n with the given snapshot builder.
func (h *harness) seedMembers(ws string, snaps ...map[string]any) {
	for i, s := range snaps {
		b, _ := json.Marshal(s)
		h.exec(ws, `INSERT INTO audience_members (workspace_id, audience_id, lead_id, snapshot) VALUES ($1,$2,$3,$4::json)`, ws, audID, i+1, string(b))
	}
}

func snap(i int) map[string]any {
	return map[string]any{"id": i, "company": fmt.Sprintf("Co %d", i), "email": fmt.Sprintf("u%d@x.test", i),
		"phone": fmt.Sprintf("+1 555 010 %04d", i), "city": "Austin", "score": i}
}

func snaps(n int) []map[string]any {
	out := make([]map[string]any, n)
	for i := range out {
		out[i] = snap(i + 1)
	}
	return out
}

func claim(ws, run string) *queue.Claim {
	p, _ := json.Marshal(map[string]any{"workspace_id": ws, "run_id": run})
	return &queue.Claim{ID: 7, Type: deststore.JobType, WorkspaceID: ws, Payload: p, WorkerID: "w", LockedAt: time.Now(), LeaseToken: 1}
}

type runRow struct {
	Status                                string
	Error                                 *string
	Attempted, Succeeded, Failed, Skipped int
	Started, Finished                     *time.Time
}

func (h *harness) run(ws, id string) *runRow {
	h.t.Helper()
	var r runRow
	found := false
	h.tx(ws, func(tx pgx.Tx) error {
		err := tx.QueryRow(h.ctx, `SELECT status, error, attempted, succeeded, failed, skipped, started_at, finished_at FROM destination_runs WHERE id=$1`, id).
			Scan(&r.Status, &r.Error, &r.Attempted, &r.Succeeded, &r.Failed, &r.Skipped, &r.Started, &r.Finished)
		if err == nil {
			found = true
			return nil
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	if !found {
		return nil
	}
	return &r
}

type destRow struct {
	Health      string
	LastError   *string
	LastSuccess *time.Time
}

func (h *harness) dest(ws string) *destRow {
	h.t.Helper()
	var d destRow
	found := false
	h.tx(ws, func(tx pgx.Tx) error {
		err := tx.QueryRow(h.ctx, `SELECT health_status, last_error, last_success_at FROM audience_destinations WHERE id=$1`, destID).Scan(&d.Health, &d.LastError, &d.LastSuccess)
		if err == nil {
			found = true
			return nil
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	if !found {
		return nil
	}
	return &d
}

type delRow struct {
	ID                                  int
	RunID                               string
	Lead                                int
	Operation, Key, Fingerprint, Status string
	Payload                             string
	Attempts                            int
	ExternalID, Summary, Error          *string
	Delivered                           *time.Time
}

func (h *harness) deliveries(ws string) []delRow {
	h.t.Helper()
	var out []delRow
	h.tx(ws, func(tx pgx.Tx) error {
		rows, err := tx.Query(h.ctx, `SELECT id, run_id, lead_id, operation, idempotency_key, payload_fingerprint, payload::text, status, attempts, external_id, summary, error, delivered_at
			FROM destination_deliveries ORDER BY id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d delRow
			if err := rows.Scan(&d.ID, &d.RunID, &d.Lead, &d.Operation, &d.Key, &d.Fingerprint, &d.Payload, &d.Status, &d.Attempts, &d.ExternalID, &d.Summary, &d.Error, &d.Delivered); err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	return out
}

func (h *harness) setRunStatus(ws, id, status, errText string) {
	h.exec(ws, `UPDATE destination_runs SET status=$2, error=$3 WHERE id=$1`, id, status, errText)
}

func (h *harness) handle(ws, run string) error {
	return h.eng.Handle(h.ctx, claim(ws, run))
}

func (h *harness) mustHandle(ws, run string) {
	h.t.Helper()
	if err := h.handle(ws, run); err != nil {
		h.t.Fatalf("Handle: %v", err)
	}
}

func countKind(f *fakePorts, kind string) int {
	n := 0
	for _, c := range f.calls {
		if c["kind"] == kind {
			n++
		}
	}
	return n
}

func str(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// ---------------------------------------------------------------- Python transcripts

type tInput struct {
	Destination struct {
		ID       string         `json:"id"`
		Type     string         `json:"type"`
		Enabled  bool           `json:"enabled"`
		Config   map[string]any `json:"config"`
		FieldMap map[string]any `json:"field_map"`
		Health   string         `json:"health_status"`
	} `json:"destination"`
	DestinationMissing bool `json:"destination_missing"`
	Members            []struct {
		LeadID   int             `json:"lead_id"`
		Snapshot json.RawMessage `json:"snapshot"`
	} `json:"members"`
	Prior []struct {
		ID          int             `json:"id"`
		RunID       string          `json:"run_id"`
		LeadID      int             `json:"lead_id"`
		Operation   string          `json:"operation"`
		Key         string          `json:"idempotency_key"`
		Fingerprint string          `json:"payload_fingerprint"`
		Payload     json.RawMessage `json:"payload"`
		Status      string          `json:"status"`
		Attempts    int             `json:"attempts"`
		ExternalID  *string         `json:"external_id"`
		Summary     *string         `json:"summary"`
		Error       *string         `json:"error"`
		DeliveredAt bool            `json:"delivered_at_set"`
	} `json:"prior_deliveries"`
	RunStatus       string             `json:"run_status"`
	RunStartedAtSet bool               `json:"run_started_at_set"`
	Payload         map[string]any     `json:"payload"`
	Script          map[string][]entry `json:"script"`
	MemberPageSize  int                `json:"member_page_size"`
	AdBatchSize     int                `json:"ad_batch_size"`
	Cancel          struct {
		AtCheck   *int `json:"at_check"`
		AfterCall *int `json:"after_call"`
	} `json:"cancel"`
}

type tRun struct {
	Status                                string  `json:"status"`
	Error                                 *string `json:"error"`
	Attempted, Succeeded, Failed, Skipped int
	StartedAt                             *string `json:"started_at"`
	FinishedAt                            *string `json:"finished_at"`
}

type tDelivery struct {
	RunID       string          `json:"run_id"`
	LeadID      int             `json:"lead_id"`
	Operation   string          `json:"operation"`
	Key         string          `json:"idempotency_key"`
	Fingerprint string          `json:"payload_fingerprint"`
	Payload     json.RawMessage `json:"payload"`
	Status      string          `json:"status"`
	Attempts    int             `json:"attempts"`
	ExternalID  *string         `json:"external_id"`
	Summary     *string         `json:"summary"`
	Error       *string         `json:"error"`
	DeliveredAt *string         `json:"delivered_at"`
}

type tOutput struct {
	Raises *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"raises"`
	Run  *tRun `json:"run"`
	Dest *struct {
		Health      string  `json:"health_status"`
		LastError   *string `json:"last_error"`
		LastSuccess *string `json:"last_success_at"`
	} `json:"destination"`
	Deliveries []tDelivery      `json:"deliveries"`
	Calls      []map[string]any `json:"calls"`
	Checks     int              `json:"cancellation_checks"`
	Fired      *string          `json:"cancel_fired"`
}

type scenario struct {
	Name   string  `json:"name"`
	Input  tInput  `json:"input"`
	Output tOutput `json:"output"`
}

func loadScenarios(t *testing.T) []scenario {
	t.Helper()
	raw, err := os.ReadFile("testdata/sync_flows.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Scenarios []scenario `json:"scenarios"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Scenarios) < 100 {
		t.Fatalf("only %d scenarios", len(f.Scenarios))
	}
	return f.Scenarios
}

func jsonEq(a, b json.RawMessage) bool {
	var x, y any
	if len(a) == 0 {
		a = json.RawMessage(`{}`)
	}
	if len(b) == 0 {
		b = json.RawMessage(`{}`)
	}
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

func jstr(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestSyncMatchesPythonTranscripts(t *testing.T) {
	scs := loadScenarios(t)
	skipped := 0
	for _, sc := range scs {
		sc := sc
		if sc.Input.DestinationMissing {
			// destination_runs.destination_id is ON DELETE CASCADE: a run whose destination is
			// gone cannot exist on Postgres (the Python in-memory run could). Covered by
			// TestDisabledDestinationCancelsRun for the same handler branch.
			skipped++
			continue
		}
		t.Run(sc.Name, func(t *testing.T) {
			in := sc.Input
			ws := wsVec
			fired := ""
			h := newHarness(t, in.Script, func(d *Deps) {
				d.MemberPageSize = in.MemberPageSize
				d.AdBatchSize = in.AdBatchSize
			})
			h.seedDest(ws, destSeed{Type: in.Destination.Type, Config: jstr(in.Destination.Config), FieldMap: jstr(in.Destination.FieldMap),
				Health: in.Destination.Health, Disabled: !in.Destination.Enabled})
			started := ""
			if in.RunStartedAtSet {
				started = "2026-08-02 09:00:01"
			}
			h.seedRun(ws, "run-1", in.RunStatus, started)
			needOld := false
			for _, p := range in.Prior {
				if p.RunID == "run-0" {
					needOld = true
				}
			}
			if needOld {
				h.seedRun(ws, "run-0", "completed", "")
			}
			h.seedAudience(ws)
			for i, m := range in.Members {
				h.exec(ws, `INSERT INTO audience_members (workspace_id, audience_id, lead_id, snapshot) VALUES ($1,$2,$3,$4::json)`,
					ws, audID, m.LeadID, string(m.Snapshot))
				_ = i
			}
			sort.Slice(in.Prior, func(i, j int) bool { return in.Prior[i].ID < in.Prior[j].ID })
			for _, p := range in.Prior {
				var delivered any
				if p.DeliveredAt {
					delivered = "2026-08-01 09:05:00"
				}
				h.exec(ws, `INSERT INTO destination_deliveries (workspace_id, run_id, destination_id, lead_id, operation, idempotency_key, payload_fingerprint,
					payload, status, attempts, external_id, summary, error, delivered_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8::json,$9,$10,$11,$12,$13,$14::timestamp)`,
					ws, p.RunID, destID, p.LeadID, p.Operation, p.Key, p.Fingerprint, string(p.Payload), p.Status, p.Attempts, p.ExternalID, p.Summary, p.Error, delivered)
			}
			if in.Cancel.AtCheck != nil {
				at := *in.Cancel.AtCheck
				h.eng.checkHook = func(n int) {
					if n == at && fired == "" {
						fired = fmt.Sprintf("check_%d", n)
						h.setRunStatus(ws, "run-1", "cancelling", "Cancellation requested")
					}
				}
			}
			if in.Cancel.AfterCall != nil {
				after := *in.Cancel.AfterCall
				h.fake.after = func(n int) {
					if n == after && fired == "" {
						fired = fmt.Sprintf("after_call_%d", n)
						h.setRunStatus(ws, "run-1", "cancelling", "Cancellation requested")
					}
				}
			}

			cws, _ := in.Payload["workspace_id"].(string)
			pl, _ := json.Marshal(in.Payload)
			err := h.eng.Handle(h.ctx, &queue.Claim{ID: 7, Type: deststore.JobType, WorkspaceID: cws, Payload: pl, WorkerID: "w", LockedAt: time.Now(), LeaseToken: 1})
			out := sc.Output
			if out.Raises != nil {
				if err == nil || err.Error() != out.Raises.Message {
					t.Fatalf("error = %v, want %q", err, out.Raises.Message)
				}
			} else if err != nil {
				t.Fatalf("Handle: %v", err)
			}

			// run
			rr := h.run(ws, "run-1")
			er := out.Run
			if rr.Status != er.Status || str(rr.Error) != str(er.Error) || rr.Attempted != er.Attempted || rr.Succeeded != er.Succeeded ||
				rr.Failed != er.Failed || rr.Skipped != er.Skipped {
				t.Errorf("run = %+v err=%s\n want %+v err=%s", *rr, str(rr.Error), *er, str(er.Error))
			}
			switch {
			case er.StartedAt == nil && rr.Started != nil:
				t.Errorf("started_at set, want NULL")
			case er.StartedAt != nil && rr.Started == nil:
				t.Errorf("started_at NULL, want set")
			case in.RunStartedAtSet && rr.Started != nil && rr.Started.Format("2006-01-02T15:04:05") != "2026-08-02T09:00:01":
				t.Errorf("started_at changed: %v", rr.Started)
			}
			if (er.FinishedAt == nil) != (rr.Finished == nil) {
				t.Errorf("finished_at set-ness: got %v want %v", rr.Finished, er.FinishedAt)
			}
			// destination
			dr := h.dest(ws)
			if dr.Health != out.Dest.Health || str(dr.LastError) != str(out.Dest.LastError) || (dr.LastSuccess == nil) != (out.Dest.LastSuccess == nil) {
				t.Errorf("destination = %+v/%s/%v want %+v/%s/%v", dr.Health, str(dr.LastError), dr.LastSuccess, out.Dest.Health, str(out.Dest.LastError), out.Dest.LastSuccess)
			}
			// deliveries by idempotency key
			got := h.deliveries(ws)
			if len(got) != len(out.Deliveries) {
				t.Fatalf("%d deliveries, want %d: %+v", len(got), len(out.Deliveries), got)
			}
			byKey := map[string]delRow{}
			for _, d := range got {
				byKey[d.Key] = d
			}
			for _, w := range out.Deliveries {
				g, ok := byKey[w.Key]
				if !ok {
					t.Errorf("missing delivery %s", w.Key)
					continue
				}
				if g.RunID != w.RunID || g.Lead != w.LeadID || g.Operation != w.Operation || g.Status != w.Status || g.Attempts != w.Attempts ||
					str(g.ExternalID) != str(w.ExternalID) || str(g.Summary) != str(w.Summary) || str(g.Error) != str(w.Error) ||
					(g.Delivered == nil) != (w.DeliveredAt == nil) || !jsonEq(json.RawMessage(g.Payload), w.Payload) {
					t.Errorf("delivery %s:\n got  %+v summary=%s error=%s ext=%s\n want %+v summary=%s error=%s ext=%s", w.Key, g, str(g.Summary), str(g.Error), str(g.ExternalID), w,
						str(w.Summary), str(w.Error), str(w.ExternalID))
				}
				if w.Fingerprint != "" && g.Fingerprint != w.Fingerprint {
					t.Errorf("delivery %s fingerprint %s want %s", w.Key, g.Fingerprint, w.Fingerprint)
				}
			}
			// requests made
			if len(h.fake.calls) != len(out.Calls) {
				t.Fatalf("%d external calls, want %d:\n got  %s\n want %s", len(h.fake.calls), len(out.Calls), jstr(h.fake.calls), jstr(out.Calls))
			}
			for i := range out.Calls {
				if !reflect.DeepEqual(roundTrip(h.fake.calls[i]), roundTrip(out.Calls[i])) {
					t.Errorf("call %d:\n got  %s\n want %s", i, jstr(h.fake.calls[i]), jstr(out.Calls[i]))
				}
			}
			if out.Raises == nil && h.eng.checks != out.Checks {
				t.Errorf("%d cancellation checks, want %d", h.eng.checks, out.Checks)
			}
			if (out.Fired == nil) != (fired == "") || (out.Fired != nil && *out.Fired != fired) {
				t.Errorf("cancel fired %q, want %v", fired, out.Fired)
			}
		})
	}
	t.Logf("%d scenarios, %d skipped (destination row gone: unreachable under the FK)", len(scs), skipped)
}

// ---------------------------------------------------------------- queue failure reconcile

func TestFailureReconcileMatchesPythonVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/failure_reconcile.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Cases []struct {
			Name      string         `json:"name"`
			Payload   map[string]any `json:"payload"`
			Error     any            `json:"error"`
			WillRetry bool           `json:"will_retry"`
			Raises    *struct {
				Message string `json:"message"`
			} `json:"raises"`
			Before, After struct {
				Runs []struct {
					ID                                    string  `json:"id"`
					Status                                string  `json:"status"`
					Error                                 *string `json:"error"`
					Attempted, Succeeded, Failed, Skipped int
					FinishedAt                            *string `json:"finished_at"`
				} `json:"runs"`
				Deliveries []struct {
					ID       int     `json:"id"`
					RunID    string  `json:"run_id"`
					Status   string  `json:"status"`
					Attempts int     `json:"attempts"`
					Error    *string `json:"error"`
				} `json:"deliveries"`
				Dest *struct {
					Health      string  `json:"health_status"`
					LastError   *string `json:"last_error"`
					LastSuccess *string `json:"last_success_at"`
				} `json:"destination"`
			}
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Cases) < 30 {
		t.Fatalf("%d cases", len(f.Cases))
	}
	for _, c := range f.Cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			h := newHarness(t, nil, nil)
			ws := wsVec
			dest := c.Before.Dest
			if dest == nil {
				t.Skip("destination row absent: unreachable under the FK")
			}
			h.seedDest(ws, destSeed{Health: dest.Health})
			if dest.LastError != nil {
				h.exec(ws, `UPDATE audience_destinations SET last_error=$1 WHERE id=$2`, *dest.LastError, destID)
			}
			for _, r := range c.Before.Runs {
				h.exec(ws, `INSERT INTO destination_runs (id, workspace_id, destination_id, status, error, attempted, succeeded, failed, skipped)
					VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, r.ID, ws, destID, r.Status, r.Error, r.Attempted, r.Succeeded, r.Failed, r.Skipped)
			}
			for _, d := range c.Before.Deliveries {
				h.exec(ws, `INSERT INTO destination_deliveries (workspace_id, run_id, destination_id, lead_id, operation, idempotency_key, payload_fingerprint, status, attempts, error)
					VALUES ($1,$2,$3,$4,'upsert',$5,'fp',$6,$7,$8)`, ws, d.RunID, destID, d.ID, fmt.Sprintf("k-%d", d.ID), d.Status, d.Attempts, d.Error)
			}
			pl, _ := json.Marshal(c.Payload)
			errText, _ := c.Error.(string)
			err := h.eng.OnFailure(h.ctx, h.pool, &queue.Claim{ID: 7, Type: deststore.JobType, Payload: pl}, errText, c.WillRetry)
			if c.Raises != nil {
				if err == nil || err.Error() != c.Raises.Message {
					t.Fatalf("error = %v, want %q", err, c.Raises.Message)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range c.After.Runs {
				r := h.run(ws, w.ID)
				if r.Status != w.Status || str(r.Error) != str(w.Error) || r.Attempted != w.Attempted || r.Succeeded != w.Succeeded || r.Failed != w.Failed || r.Skipped != w.Skipped ||
					(r.Finished == nil) != (w.FinishedAt == nil) {
					t.Errorf("run %s = %+v/%s want %+v/%s", w.ID, *r, str(r.Error), w, str(w.Error))
				}
			}
			got := h.deliveries(ws)
			sort.Slice(got, func(i, j int) bool { return got[i].Lead < got[j].Lead })
			if len(got) != len(c.After.Deliveries) {
				t.Fatalf("%d deliveries want %d", len(got), len(c.After.Deliveries))
			}
			for i, w := range c.After.Deliveries {
				g := got[i]
				if g.Lead != w.ID || g.Status != w.Status || g.Attempts != w.Attempts || str(g.Error) != str(w.Error) {
					t.Errorf("delivery %d = %s/%d/%s want %s/%d/%s", w.ID, g.Status, g.Attempts, str(g.Error), w.Status, w.Attempts, str(w.Error))
				}
			}
			d := h.dest(ws)
			wd := c.After.Dest
			if d.Health != wd.Health || str(d.LastError) != str(wd.LastError) || (d.LastSuccess == nil) != (wd.LastSuccess == nil) {
				t.Errorf("destination = %s/%s/%v want %+v", d.Health, str(d.LastError), d.LastSuccess, *wd)
			}
		})
	}
}

// ---------------------------------------------------------------- named behaviours

func TestIdempotentRedeliverySkipsSuccess(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.seedDest(wsVec, destSeed{})
	h.seedMembers(wsVec, snaps(3)...)
	h.seedRun(wsVec, "run-1", "pending", "")
	h.mustHandle(wsVec, "run-1")
	if n := countKind(h.fake, "deliver"); n != 3 {
		t.Fatalf("first run delivered %d", n)
	}
	// the same job again (queue redelivery of a finished run): nothing happens
	h.mustHandle(wsVec, "run-1")
	// a later manual run of the unchanged audience: every member is already delivered
	h.seedRun(wsVec, "run-2", "pending", "")
	h.mustHandle(wsVec, "run-2")
	if n := countKind(h.fake, "deliver"); n != 3 {
		t.Fatalf("redelivered successes: %d calls", n)
	}
	r := h.run(wsVec, "run-2")
	if r.Status != "completed" || r.Skipped != 3 || r.Attempted != 0 || r.Succeeded != 0 {
		t.Fatalf("run-2 = %+v", *r)
	}
	// a job retried mid-run (run still running, member 1 already settled success in this run)
	h2 := newHarness(t, nil, nil)
	h2.seedDest(wsVec, destSeed{})
	h2.seedMembers(wsVec, snaps(3)...)
	h2.seedRun(wsVec, "run-1", "running", "2026-08-02 09:00:01")
	fp := destdeliver.Fingerprint(snap(1))
	h2.exec(wsVec, `INSERT INTO destination_deliveries (workspace_id, run_id, destination_id, lead_id, operation, idempotency_key, payload_fingerprint, status, attempts, delivered_at)
		VALUES ($1,'run-1',$2,1,'upsert',$3,$4,'success',1, now() at time zone 'UTC')`, wsVec, destID, destdeliver.IdempotencyKey(destID, 1, fp, false), fp)
	h2.mustHandle(wsVec, "run-1")
	if n := countKind(h2.fake, "deliver"); n != 2 {
		t.Fatalf("retry delivered %d members, want the 2 unfinished", n)
	}
}

func TestFailedPriorRetriedAndRehomed(t *testing.T) {
	h := newHarness(t, map[string][]entry{"deliver": {{Success: true, Summary: "ok"}}}, nil)
	h.seedDest(wsVec, destSeed{})
	h.seedMembers(wsVec, snap(1))
	h.seedRun(wsVec, "run-0", "completed_with_errors", "")
	h.seedRun(wsVec, "run-1", "pending", "")
	fp := destdeliver.Fingerprint(snap(1))
	key := destdeliver.IdempotencyKey(destID, 1, fp, false)
	h.exec(wsVec, `INSERT INTO destination_deliveries (workspace_id, run_id, destination_id, lead_id, operation, idempotency_key, payload_fingerprint, status, attempts, error)
		VALUES ($1,'run-0',$2,1,'upsert',$3,$4,'failed',2,'HTTP 500')`, wsVec, destID, key, fp)
	h.mustHandle(wsVec, "run-1")
	ds := h.deliveries(wsVec)
	if len(ds) != 1 || ds[0].RunID != "run-1" || ds[0].Status != "success" || ds[0].Attempts != 3 || ds[0].Error != nil {
		t.Fatalf("deliveries = %+v", ds)
	}
}

// Deliver / SyncBatch / Sync run with no database transaction open on any session of the
// application role, for every kind of destination.
func TestNoTransactionHeldDuringDeliver(t *testing.T) {
	for _, typ := range []string{"webhook", "meta_ads", "warehouse_http"} {
		typ := typ
		t.Run(typ, func(t *testing.T) {
			h := newHarness(t, nil, nil)
			cfg := `{"url":"https://hooks.example.test/x"}`
			if typ == "meta_ads" {
				cfg = `{"custom_audience_id":"1","consent_attested":true,"consent_source":"form"}`
			}
			h.seedDest(wsVec, destSeed{Type: typ, Config: cfg})
			h.seedMembers(wsVec, snaps(3)...)
			h.seedRun(wsVec, "run-1", "pending", "")
			seen := 0
			h.fake.before = func(kind string) {
				seen++
				var open int
				err := h.owner.QueryRow(h.ctx, `SELECT count(*) FROM pg_stat_activity
					WHERE datname = current_database() AND usename = $1 AND pid <> pg_backend_pid()
					  AND (xact_start IS NOT NULL OR state LIKE 'idle in transaction%')`, testpg.AppLoginRole).Scan(&open)
				if err != nil {
					t.Errorf("pg_stat_activity: %v", err)
				}
				if open != 0 {
					t.Errorf("%s call: %d application sessions hold a transaction open", kind, open)
				}
			}
			h.mustHandle(wsVec, "run-1")
			if seen == 0 {
				t.Fatal("nothing was delivered")
			}
		})
	}
}

func adCfg() string {
	return `{"custom_audience_id":"1","consent_attested":true,"consent_source":"form"}`
}

func TestAdBatchSettlesAllRowsTogether(t *testing.T) {
	for _, ok := range []bool{true, false} {
		script := map[string][]entry{"ad": {{Success: ok, Summary: "added 3 hashed users", Error: map[bool]string{true: "", false: "platform said no"}[ok], ExternalID: "sess-1"}}}
		h := newHarness(t, script, nil)
		h.seedDest(wsVec, destSeed{Type: "meta_ads", Config: adCfg()})
		h.seedMembers(wsVec, snaps(3)...)
		h.seedRun(wsVec, "run-1", "pending", "")
		h.mustHandle(wsVec, "run-1")
		if n := countKind(h.fake, "ad_batch"); n != 1 {
			t.Fatalf("%d batches", n)
		}
		want := map[bool]string{true: "success", false: "failed"}[ok]
		for _, d := range h.deliveries(wsVec) {
			if d.Status != want || d.Attempts != 1 || str(d.Summary) != "added 3 hashed users" || str(d.ExternalID) != "sess-1" || (d.Delivered != nil) != ok {
				t.Errorf("ok=%v: row %+v summary=%s", ok, d, str(d.Summary))
			}
			if !ok && str(d.Error) != "platform said no" {
				t.Errorf("error %s", str(d.Error))
			}
		}
		r := h.run(wsVec, "run-1")
		if ok && (r.Succeeded != 3 || r.Failed != 0 || r.Attempted != 3) || !ok && (r.Failed != 3 || r.Succeeded != 0 || r.Status != "completed_with_errors") {
			t.Errorf("ok=%v run %+v", ok, *r)
		}
	}
}

func TestAdBatchSizeBoundary(t *testing.T) {
	for _, tc := range []struct{ members, batches int }{{5000, 1}, {5001, 2}} {
		tc := tc
		t.Run(fmt.Sprint(tc.members), func(t *testing.T) {
			h := newHarness(t, nil, nil) // default AdBatchSize is the Python constant 5000
			h.seedDest(wsVec, destSeed{Type: "google_ads", Config: `{"customer_id":"1234567890","user_list_id":"55"}`})
			h.seedAudience(wsVec)
			h.exec(wsVec, `INSERT INTO audience_members (workspace_id, audience_id, lead_id, snapshot)
				SELECT $1, $2, g, json_build_object('id', g, 'email', 'u' || g || '@x.test') FROM generate_series(1, $3::int) g`, wsVec, audID, tc.members)
			h.seedRun(wsVec, "run-1", "pending", "")
			h.mustHandle(wsVec, "run-1")
			var sizes []int
			for _, c := range h.fake.calls {
				if c["kind"] == "ad_batch" && c["operation"] == "add" {
					sizes = append(sizes, len(c["snapshots"].([]any)))
				}
			}
			if len(sizes) != tc.batches || sizes[0] != 5000 || (tc.batches == 2 && sizes[1] != 1) {
				t.Fatalf("batch sizes %v", sizes)
			}
			if r := h.run(wsVec, "run-1"); r.Succeeded != tc.members || r.Attempted != tc.members || r.Status != "completed" {
				t.Fatalf("run %+v", *r)
			}
		})
	}
}

func TestAdUnsupportedIdentifiersSkipped(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.seedDest(wsVec, destSeed{Type: "linkedin_ads", Config: `{"account_id":"1","list_id":"2","consent_attested":true,"consent_source":"form"}`})
	h.seedMembers(wsVec,
		map[string]any{"id": 1, "company": "NoId"},
		map[string]any{"id": 2, "phone": "+1 555 010 0002"}, // LinkedIn takes email only
		map[string]any{"id": 3, "email": "ok@x.test"})
	h.seedRun(wsVec, "run-1", "pending", "")
	h.mustHandle(wsVec, "run-1")
	skipped := 0
	for _, d := range h.deliveries(wsVec) {
		if d.Status == "skipped" {
			skipped++
			if str(d.Summary) != "No valid email or phone identifier" || d.Payload != "{}" && !jsonEq(json.RawMessage(d.Payload), json.RawMessage(`{}`)) || d.Attempts != 0 {
				t.Errorf("skipped row %+v", d)
			}
		}
	}
	if skipped != 2 {
		t.Fatalf("%d skipped", skipped)
	}
	r := h.run(wsVec, "run-1")
	if r.Skipped != 2 || r.Succeeded != 1 || r.Status != "completed" {
		t.Fatalf("run %+v", *r)
	}
	if n := len(h.fake.calls[0]["snapshots"].([]any)); n != 1 {
		t.Fatalf("batch carried %d snapshots, want only the supported member", n)
	}
}

func TestAdRemovalReconciliationHashesOnly(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.seedDest(wsVec, destSeed{Type: "meta_ads", Config: adCfg()})
	h.seedMembers(wsVec, snaps(2)...)
	h.seedRun(wsVec, "run-1", "pending", "")
	h.mustHandle(wsVec, "run-1")
	// member 2 leaves the audience
	h.exec(wsVec, `DELETE FROM audience_members WHERE lead_id = 2`)
	h.seedRun(wsVec, "run-2", "pending", "")
	h.mustHandle(wsVec, "run-2")

	var remove map[string]any
	for _, c := range h.fake.calls {
		if c["operation"] == "remove" {
			remove = c
		}
	}
	if remove == nil {
		t.Fatalf("no removal batch: %s", jstr(h.fake.calls))
	}
	want := adsdrv.HashedIdentifiers(snap(2))
	got := remove["snapshots"].([]any)
	if len(got) != 1 || !reflect.DeepEqual(got[0], roundTrip(want)) {
		t.Fatalf("removal snapshots %s, want hashed %v", jstr(got), want)
	}
	for _, d := range h.deliveries(wsVec) {
		for _, raw := range []string{"u2@x.test", "u1@x.test", "555"} {
			if strings.Contains(d.Payload, raw) || strings.Contains(d.Key, raw) {
				t.Errorf("raw identifier %q stored in delivery %+v", raw, d)
			}
		}
	}
	if strings.Contains(jstr(remove), "@x.test") {
		t.Errorf("raw identifier sent in the removal request: %s", jstr(remove))
	}
	var rm delRow
	for _, d := range h.deliveries(wsVec) {
		if d.Operation == "remove" {
			rm = d
		}
	}
	if rm.Status != "success" || rm.Lead != 2 || rm.RunID != "run-2" || !strings.Contains(rm.Key, ":remove:") {
		t.Fatalf("remove row %+v", rm)
	}
	if r := h.run(wsVec, "run-2"); r.Succeeded != 1+0 && r.Attempted != 1 {
		t.Fatalf("run-2 %+v", *r)
	}
}

func TestAdRemovalIdemSkipsAfterSuccess(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.seedDest(wsVec, destSeed{Type: "meta_ads", Config: adCfg()})
	h.seedMembers(wsVec, snaps(2)...)
	h.seedRun(wsVec, "run-1", "pending", "")
	h.mustHandle(wsVec, "run-1")
	h.exec(wsVec, `DELETE FROM audience_members WHERE lead_id = 2`)
	h.seedRun(wsVec, "run-2", "pending", "")
	h.mustHandle(wsVec, "run-2")
	before := countKind(h.fake, "ad_batch")
	h.seedRun(wsVec, "run-3", "pending", "")
	h.mustHandle(wsVec, "run-3")
	if n := countKind(h.fake, "ad_batch"); n != before {
		t.Fatalf("removal repeated: %d batches, was %d", n, before)
	}
}

func TestWarehouseFlushSettlesInFlightRows(t *testing.T) {
	h := newHarness(t, map[string][]entry{"warehouse": {{Success: false, Error: "HTTP 502"}}}, nil)
	h.seedDest(wsVec, destSeed{Type: "warehouse_http", Config: `{"url":"https://w.example.test/x","header_secret_ref":"R","dataset":"ds"}`})
	h.seedMembers(wsVec, snaps(4)...)
	h.seedRun(wsVec, "run-1", "pending", "")
	h.mustHandle(wsVec, "run-1")
	if n := countKind(h.fake, "warehouse_export"); n != 1 {
		t.Fatalf("%d exports", n)
	}
	for _, d := range h.deliveries(wsVec) {
		if d.Status != "failed" || d.Attempts != 1 || str(d.Error) != "HTTP 502" {
			t.Errorf("row %+v", d)
		}
	}
	r := h.run(wsVec, "run-1")
	if r.Failed != 4 || r.Attempted != 4 || r.Succeeded != 0 || r.Status != "completed_with_errors" {
		t.Fatalf("counters by affected rows: %+v", *r)
	}
}

func TestWarehouseNoRowsNoRequest(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.seedDest(wsVec, destSeed{Type: "warehouse_http", Config: `{"url":"https://w.example.test/x","header_secret_ref":"R","dataset":"ds"}`})
	h.seedRun(wsVec, "run-1", "pending", "")
	h.mustHandle(wsVec, "run-1") // empty audience
	if len(h.fake.calls) != 0 {
		t.Fatalf("export sent for zero rows: %s", jstr(h.fake.calls))
	}
	if r := h.run(wsVec, "run-1"); r.Status != "completed" || r.Attempted != 0 {
		t.Fatalf("run %+v", *r)
	}
	// every member already delivered: the buffer stays empty too
	h.seedMembers(wsVec, snaps(2)...)
	h.seedRun(wsVec, "run-2", "pending", "")
	h.mustHandle(wsVec, "run-2")
	h.seedRun(wsVec, "run-3", "pending", "")
	n := len(h.fake.calls)
	h.mustHandle(wsVec, "run-3")
	if len(h.fake.calls) != n {
		t.Fatalf("export sent with nothing new to deliver")
	}
}

func TestCancelBeforeMemberAndBeforeBatch(t *testing.T) {
	// before the first member
	h := newHarness(t, nil, nil)
	h.seedDest(wsVec, destSeed{})
	h.seedMembers(wsVec, snaps(3)...)
	h.seedRun(wsVec, "run-1", "pending", "")
	h.eng.checkHook = func(n int) {
		if n == 0 {
			h.setRunStatus(wsVec, "run-1", "cancelling", "Cancellation requested")
		}
	}
	h.mustHandle(wsVec, "run-1")
	if len(h.fake.calls) != 0 {
		t.Fatalf("delivered after cancel: %d calls", len(h.fake.calls))
	}
	if r := h.run(wsVec, "run-1"); r.Status != "cancelled" || str(r.Error) != "Cancellation completed" || r.Finished == nil {
		t.Fatalf("run %+v", *r)
	}

	// before the ad batch (the check after the last member)
	h2 := newHarness(t, nil, nil)
	h2.seedDest(wsVec, destSeed{Type: "meta_ads", Config: adCfg()})
	h2.seedMembers(wsVec, snaps(2)...)
	h2.seedRun(wsVec, "run-1", "pending", "")
	h2.eng.checkHook = func(n int) {
		if n == 2 { // checks 0,1 are the members; 2 is the batch
			h2.setRunStatus(wsVec, "run-1", "cancelling", "Cancellation requested")
		}
	}
	h2.mustHandle(wsVec, "run-1")
	if len(h2.fake.calls) != 0 {
		t.Fatalf("batch sent after cancel: %s", jstr(h2.fake.calls))
	}
	for _, d := range h2.deliveries(wsVec) {
		if d.Status != "cancelled" || str(d.Error) != "Run cancelled before delivery" || d.Attempts != 0 {
			t.Errorf("row %+v", d)
		}
	}
	if r := h2.run(wsVec, "run-1"); r.Status != "cancelled" || r.Attempted != 0 {
		t.Fatalf("run %+v", *r)
	}
}

func TestCancelDuringFinalRequestKeepsResultsAndCancels(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.seedDest(wsVec, destSeed{Type: "warehouse_http", Config: `{"url":"https://w.example.test/x","header_secret_ref":"R","dataset":"ds"}`})
	h.seedMembers(wsVec, snaps(3)...)
	h.seedRun(wsVec, "run-1", "pending", "")
	h.fake.after = func(int) { h.setRunStatus(wsVec, "run-1", "cancelling", "Cancellation requested") }
	h.mustHandle(wsVec, "run-1")
	r := h.run(wsVec, "run-1")
	if r.Status != "cancelled" || r.Succeeded != 3 || r.Attempted != 3 || str(r.Error) != "Cancellation completed" {
		t.Fatalf("run %+v", *r)
	}
	for _, d := range h.deliveries(wsVec) {
		if d.Status != "success" || d.Delivered == nil {
			t.Errorf("completed effect erased: %+v", d)
		}
	}
	if d := h.dest(wsVec); d.LastSuccess == nil {
		t.Error("last_success_at not moved although deliveries succeeded")
	}
}

func TestCancelledNeverOverwrittenByCompleted(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.seedDest(wsVec, destSeed{})
	h.seedMembers(wsVec, snaps(2)...)
	h.seedRun(wsVec, "run-1", "pending", "")
	h.eng.checkHook = func(n int) {
		if n == 2 { // the final check, after both members were delivered
			h.setRunStatus(wsVec, "run-1", "cancelling", "Cancellation requested")
		}
	}
	h.mustHandle(wsVec, "run-1")
	r := h.run(wsVec, "run-1")
	if r.Status != "cancelled" || r.Succeeded != 2 {
		t.Fatalf("run %+v", *r)
	}
	if d := h.dest(wsVec); d.Health != "unverified" {
		t.Fatalf("health updated by a cancelled run: %+v", *d)
	}
	// a cancelled run stays cancelled when its job is delivered again
	h.mustHandle(wsVec, "run-1")
	if r := h.run(wsVec, "run-1"); r.Status != "cancelled" {
		t.Fatalf("terminal run rewritten: %+v", *r)
	}
}

func TestDisabledDestinationCancelsRun(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.seedDest(wsVec, destSeed{Disabled: true})
	h.seedMembers(wsVec, snaps(2)...)
	h.seedRun(wsVec, "run-1", "pending", "")
	h.mustHandle(wsVec, "run-1")
	r := h.run(wsVec, "run-1")
	if r.Status != "cancelled" || str(r.Error) != "destination missing or disabled" || r.Finished == nil || r.Started != nil {
		t.Fatalf("run %+v", *r)
	}
	if len(h.fake.calls) != 0 {
		t.Fatal("a disabled destination was delivered to")
	}
}

func TestEmptyAudienceMarksSuccess(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.seedDest(wsVec, destSeed{})
	h.seedAudience(wsVec)
	h.seedRun(wsVec, "run-1", "pending", "")
	h.mustHandle(wsVec, "run-1")
	r, d := h.run(wsVec, "run-1"), h.dest(wsVec)
	if r.Status != "completed" || d.Health != "healthy" || d.LastSuccess == nil || d.LastError != nil {
		t.Fatalf("run %+v dest %+v", *r, *d)
	}
}

func TestHealthAndLastSuccess(t *testing.T) {
	// all fail: degraded, N deliveries failed, last_success_at untouched
	h := newHarness(t, map[string][]entry{"deliver": {{Error: "e1"}, {Error: "e2"}, {Success: true}}}, nil)
	h.seedDest(wsVec, destSeed{})
	h.seedMembers(wsVec, snaps(3)...)
	h.seedRun(wsVec, "run-1", "pending", "")
	h.mustHandle(wsVec, "run-1")
	r, d := h.run(wsVec, "run-1"), h.dest(wsVec)
	if r.Status != "completed_with_errors" || r.Failed != 2 || r.Succeeded != 1 || d.Health != "degraded" || str(d.LastError) != "2 deliveries failed" || d.LastSuccess == nil {
		t.Fatalf("run %+v dest %+v %s", *r, *d, str(d.LastError))
	}
	// only failures: last_success_at keeps its old value (NULL here)
	h2 := newHarness(t, map[string][]entry{"deliver": {{Error: "e"}}}, nil)
	h2.seedDest(wsVec, destSeed{})
	h2.seedMembers(wsVec, snap(1))
	h2.seedRun(wsVec, "run-1", "pending", "")
	h2.mustHandle(wsVec, "run-1")
	if d := h2.dest(wsVec); d.Health != "degraded" || d.LastSuccess != nil || str(d.LastError) != "1 deliveries failed" {
		t.Fatalf("dest %+v", *d)
	}
	// the retry run succeeds: healthy again, error cleared
	h2.seedRun(wsVec, "run-2", "pending", "")
	h2.mustHandle(wsVec, "run-2")
	if d := h2.dest(wsVec); d.Health != "healthy" || d.LastError != nil || d.LastSuccess == nil {
		t.Fatalf("dest after recovery %+v", *d)
	}
}

func TestQueueRetryResetsInFlight(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.seedDest(wsVec, destSeed{})
	h.seedRun(wsVec, "run-1", "running", "2026-08-02 09:00:01")
	for i, st := range []string{"success", "in_flight", "pending", "failed"} {
		h.exec(wsVec, `INSERT INTO destination_deliveries (workspace_id, run_id, destination_id, lead_id, operation, idempotency_key, payload_fingerprint, status, attempts)
			VALUES ($1,'run-1',$2,$3,'upsert',$4,'fp',$5,1)`, wsVec, destID, i+1, fmt.Sprintf("k%d", i), st)
	}
	if err := h.eng.OnFailure(h.ctx, h.pool, claim(wsVec, "run-1"), "worker killed", true); err != nil {
		t.Fatal(err)
	}
	r := h.run(wsVec, "run-1")
	if r.Status != "pending" || str(r.Error) != "Queue retry scheduled: worker killed" || r.Finished != nil {
		t.Fatalf("run %+v", *r)
	}
	got := map[int]string{}
	for _, d := range h.deliveries(wsVec) {
		got[d.Lead] = d.Status
	}
	if got[1] != "success" || got[2] != "pending" || got[3] != "pending" || got[4] != "failed" {
		t.Fatalf("statuses %v", got)
	}
	// the retried job then finishes the run
	h.mustHandle(wsVec, "run-1")
	if r := h.run(wsVec, "run-1"); r.Status != "completed" && r.Status != "completed_with_errors" {
		t.Fatalf("retry did not finish: %+v", *r)
	}
}

func TestTerminalFailureFailsPending(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.seedDest(wsVec, destSeed{})
	h.seedRun(wsVec, "run-1", "running", "2026-08-02 09:00:01")
	for i, st := range []string{"success", "in_flight", "pending"} {
		h.exec(wsVec, `INSERT INTO destination_deliveries (workspace_id, run_id, destination_id, lead_id, operation, idempotency_key, payload_fingerprint, status, attempts)
			VALUES ($1,'run-1',$2,$3,'upsert',$4,'fp',$5,1)`, wsVec, destID, i+1, fmt.Sprintf("k%d", i), st)
	}
	msg := "POST failed: Authorization: Bearer sk_live_TOPSECRET0123 " + strings.Repeat("z", 2000)
	if err := h.eng.OnFailure(h.ctx, h.pool, claim(wsVec, "run-1"), msg, false); err != nil {
		t.Fatal(err)
	}
	r, d := h.run(wsVec, "run-1"), h.dest(wsVec)
	if r.Status != "failed" || !strings.HasPrefix(str(r.Error), "Final failure: ") || r.Finished == nil || r.Succeeded != 1 || r.Failed != 2 {
		t.Fatalf("run %+v %s", *r, str(r.Error))
	}
	if strings.Contains(str(r.Error), "TOPSECRET") || len([]rune(str(r.Error))) > len("Final failure: ")+1000 {
		t.Fatalf("error not redacted/truncated: %d chars, secret=%v", len([]rune(str(r.Error))), strings.Contains(str(r.Error), "TOPSECRET"))
	}
	if d.Health != "degraded" || !strings.HasPrefix(str(d.LastError), "Final failure: ") {
		t.Fatalf("dest %+v", *d)
	}
	for _, x := range h.deliveries(wsVec) {
		if x.Lead > 1 && (x.Status != "failed" || !strings.HasPrefix(str(x.Error), "Final failure: ")) {
			t.Errorf("open delivery not failed: %+v", x)
		}
	}
}

func TestLeaseLossStopsBeforeNextDelivery(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.seedDest(wsVec, destSeed{})
	h.seedMembers(wsVec, snaps(3)...)
	h.seedRun(wsVec, "run-1", "pending", "")
	h.lease = func(n int) bool { return n < 2 } // the second check finds the lease gone
	err := h.handle(wsVec, "run-1")
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("err = %v, want ErrLeaseLost", err)
	}
	if n := countKind(h.fake, "deliver"); n != 1 {
		t.Fatalf("%d deliveries after the lease was lost, want 1", n)
	}
	ds := h.deliveries(wsVec)
	if len(ds) != 1 || ds[0].Status != "success" {
		t.Fatalf("ledger touched past the lease loss: %+v", ds)
	}
	if r := h.run(wsVec, "run-1"); r.Status != "running" {
		t.Fatalf("a lost lease must not finish the run: %+v", *r)
	}
}

func TestTenantIsolation(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.seedDest(wsVec, destSeed{})
	h.seedMembers(wsVec, snaps(2)...)
	h.seedRun(wsVec, "run-1", "pending", "")

	// a job of another workspace naming this run touches nothing
	h.seedAudience(wsOth)
	h.mustHandle(wsOth, "run-1")
	if r := h.run(wsVec, "run-1"); r.Status != "pending" || len(h.fake.calls) != 0 {
		t.Fatalf("foreign job acted on the run: %+v calls=%d", *r, len(h.fake.calls))
	}
	if err := h.eng.OnFailure(h.ctx, h.pool, claim(wsOth, "run-1"), "boom", false); err != nil {
		t.Fatal(err)
	}
	if r := h.run(wsVec, "run-1"); r.Status != "pending" {
		t.Fatalf("foreign failure reconcile acted on the run: %+v", *r)
	}

	// a payload that names a workspace other than the job row's is refused outright
	p, _ := json.Marshal(map[string]any{"workspace_id": wsVec, "run_id": "run-1"})
	err := h.eng.Handle(h.ctx, &queue.Claim{ID: 8, Type: deststore.JobType, WorkspaceID: wsOth, Payload: p})
	if !errors.Is(err, ErrWorkspaceMismatch) || !queue.IsPermanent(err) {
		t.Fatalf("mismatched payload: %v", err)
	}
	err = h.eng.OnFailure(h.ctx, h.pool, &queue.Claim{ID: 8, Type: deststore.JobType, WorkspaceID: wsOth, Payload: p}, "boom", false)
	if !errors.Is(err, ErrWorkspaceMismatch) {
		t.Fatalf("mismatched failure payload: %v", err)
	}
	if r := h.run(wsVec, "run-1"); r.Status != "pending" {
		t.Fatalf("run touched: %+v", *r)
	}
	// and the right workspace does run it
	h.mustHandle(wsVec, "run-1")
	if r := h.run(wsVec, "run-1"); r.Status != "completed" {
		t.Fatalf("run %+v", *r)
	}
}

// Credentials in a provider's error text never reach the ledger or the logs, and the
// stored text is the redacted one cut to 1000.
func TestSecretsRedactedFromLedgerAndLogs(t *testing.T) {
	leak := "Authorization: Bearer sk_live_TOPSECRET0123 " + strings.Repeat("y", 1500)
	h := newHarness(t, map[string][]entry{"deliver": {{Error: leak}}, "ad": {{Raise: leak}}}, nil)
	h.seedDest(wsVec, destSeed{})
	h.seedMembers(wsVec, snap(1))
	h.seedRun(wsVec, "run-1", "pending", "")
	h.mustHandle(wsVec, "run-1")
	for _, d := range h.deliveries(wsVec) {
		if strings.Contains(str(d.Error), "TOPSECRET") || len([]rune(str(d.Error))) > 1000 {
			t.Errorf("ledger error leaks or overruns: %d chars", len([]rune(str(d.Error))))
		}
	}
	h2 := newHarness(t, map[string][]entry{"ad": {{Raise: leak}}}, nil)
	h2.seedDest(wsVec, destSeed{Type: "meta_ads", Config: adCfg()})
	h2.seedMembers(wsVec, snap(1))
	h2.seedRun(wsVec, "run-1", "pending", "")
	h2.mustHandle(wsVec, "run-1")
	for _, d := range h2.deliveries(wsVec) {
		if strings.Contains(str(d.Error), "TOPSECRET") || len([]rune(str(d.Error))) > 500 {
			t.Errorf("ad batch error: %d chars leak=%v", len([]rune(str(d.Error))), strings.Contains(str(d.Error), "TOPSECRET"))
		}
	}
	for _, hh := range []*harness{h, h2} {
		if strings.Contains(hh.logs.String(), "TOPSECRET") || strings.Contains(hh.logs.String(), "u1@x.test") {
			t.Errorf("logs hold a secret or identifier: %s", hh.logs.String())
		}
	}
}

// Member pages of 2 stream the whole audience in id order, each page its own transaction.
func TestMemberPagingStreamsAllMembers(t *testing.T) {
	h := newHarness(t, nil, func(d *Deps) { d.MemberPageSize = 2 })
	h.seedDest(wsVec, destSeed{})
	h.seedMembers(wsVec, snaps(5)...)
	h.seedRun(wsVec, "run-1", "pending", "")
	h.mustHandle(wsVec, "run-1")
	var leads []int
	for _, c := range h.fake.calls {
		leads = append(leads, int(c["lead_id"].(float64)))
	}
	if !reflect.DeepEqual(leads, []int{1, 2, 3, 4, 5}) {
		t.Fatalf("delivery order %v", leads)
	}
}
