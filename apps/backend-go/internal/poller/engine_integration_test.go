//go:build integration

package poller_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/automationhook"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/poller"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/pyworker"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/queue"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

const (
	eWS  = "p08t15-a"
	eWS2 = "p08t15-b"
	eW   = "p08t15-w1"
)

func eClean(ctx context.Context, pool *pgxpool.Pool) {
	for _, ws := range []string{eWS, eWS2} {
		_ = tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
			for _, q := range []string{
				`DELETE FROM watch_subscriptions`, `DELETE FROM poll_budget_ledger`, `DELETE FROM signals`,
				`DELETE FROM leads`, `DELETE FROM workspace_credits`,
			} {
				_, _ = tx.Exec(ctx, q)
			}
			_, _ = tx.Exec(ctx, `DELETE FROM jobs WHERE workspace_id = $1 OR payload->>'workspace_id' = $1`, ws)
			_, err := tx.Exec(ctx, `DELETE FROM watch_schedules WHERE workspace_id = $1`, ws)
			return err
		})
	}
}

func eSetup(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	pool := testpg.NewAppPool(t, 40)
	ctx := context.Background()
	eClean(ctx, pool)
	t.Cleanup(func() { eClean(ctx, pool) })
	return pool, ctx
}

type eWatch struct {
	ID, WS, Kind, Target string
	LeadID               *int
	Types                string // JSON list
	Config, Cursor       string // JSON
	Disabled             bool
	Fails                int
	LastErr              *string
}

func (w eWatch) insert(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if w.ID == "" {
		w.ID = eW
	}
	if w.WS == "" {
		w.WS = eWS
	}
	if w.Kind == "" {
		w.Kind = "funding"
	}
	if w.Target == "" {
		w.Target = "Acme"
	}
	if w.Types == "" {
		w.Types = "[]"
		if w.Kind == "funding" {
			w.Types = `["company_funded"]` // the Python vectors' default watch row
		}
	}
	if w.Config == "" {
		w.Config = "{}"
	}
	if w.Cursor == "" {
		w.Cursor = `{"bootstrapped": false}`
	}
	err := tenancy.InWorkspace(ctx, pool, w.WS, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO watch_subscriptions (id, workspace_id, kind, target, lead_id, signal_types, "interval",
			config, schedule_anchor, enabled, consecutive_failures, last_error, cursor)
			VALUES ($1,$2,$3,$4,$5,$6::json,'daily',$7::json,$8,$9,$10,$11,$12::json)`,
			w.ID, w.WS, w.Kind, w.Target, w.LeadID, w.Types, w.Config, vAnchor, !w.Disabled, w.Fails, w.LastErr, w.Cursor); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO watch_schedules (watch_id, workspace_id, next_poll_at, enabled, updated_at)
			VALUES ($1,$2,NULL,true,now()) ON CONFLICT (watch_id) DO UPDATE SET next_poll_at=NULL, enabled=true`, w.ID, w.WS)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func eLead(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ws string, id int, company string) {
	t.Helper()
	if err := tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO leads (id, workspace_id, company) VALUES ($1,$2,$3)`, 8_000_000+id, ws, company)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

const eLeadBase = 8_000_000

// eClaim inserts a processing go job and returns its claim.
func eClaim(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ws string, payload string) *queue.Claim {
	t.Helper()
	c := &queue.Claim{Type: "watch_poll", WorkspaceID: ws, Payload: json.RawMessage(payload), WorkerID: "p08t15", LeaseToken: 1}
	err := pool.QueryRow(ctx, `INSERT INTO jobs (type, payload, status, priority, created_at, retry_count, max_retries, workspace_id,
		executor, worker_id, locked_at, lease_token, started_at, last_heartbeat)
		VALUES ('watch_poll', $1::json, 'processing', 1, now() AT TIME ZONE 'UTC', 0, 3, $2, 'go', 'p08t15',
		now() AT TIME ZONE 'UTC', 1, now() AT TIME ZONE 'UTC', now() AT TIME ZONE 'UTC') RETURNING id, locked_at`,
		payload, ws).Scan(&c.ID, &c.LockedAt)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func ePayload(ws, w string, extra ...string) string {
	s := fmt.Sprintf(`{"watch_id": %q, "workspace_id": %q`, w, ws)
	for _, e := range extra {
		s += ", " + e
	}
	return s + "}"
}

type eCall struct {
	Src, FireKey string
	Lead         int64
	Backfill     bool
}

// eStub is the fetch seam: scripted per-source results and a call log.
type eStub struct {
	mu      sync.Mutex
	calls   []eCall
	results map[string]func() ([]poller.DetectedEvent, map[string]any, error)
	onCall  func(src string)
}

func (s *eStub) fetch(_ context.Context, src string, w poller.PyWatch, lead int64, backfill bool) ([]poller.DetectedEvent, map[string]any, bool, error) {
	s.mu.Lock()
	s.calls = append(s.calls, eCall{src, w.FireKey, lead, backfill})
	f := s.results[src]
	cb := s.onCall
	s.mu.Unlock()
	if cb != nil {
		cb(src)
	}
	if f == nil {
		return []poller.DetectedEvent{}, map[string]any{}, true, nil
	}
	ev, p, err := f()
	return ev, p, true, err
}

func (s *eStub) called() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, c := range s.calls {
		out = append(out, c.Src)
	}
	return out
}

type eHook struct {
	mu    sync.Mutex
	calls [][]automationhook.SignalRef
	fail  bool
}

func (h *eHook) RowsAdded(context.Context, pgx.Tx, string, string, []int64) (int, error) {
	return 0, nil
}

func (h *eHook) SignalsInserted(_ context.Context, _ pgx.Tx, _ string, sigs []automationhook.SignalRef) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.fail {
		return 0, errors.New("simulated crash before cursor commit")
	}
	h.calls = append(h.calls, sigs)
	return len(sigs), nil
}

func (h *eHook) fired() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, c := range h.calls {
		n += len(c)
	}
	return n
}

func eDeps(pool *pgxpool.Pool, stub *eStub, hook automationhook.Hook) poller.HandlerDeps {
	d := poller.HandlerDeps{Enabled: true, PGStore: true, Hook: hook}
	d.Pool = pool
	d.Cfg = cfg
	d.Now = func() time.Time { return vNow }
	if stub != nil {
		d.Fetch = stub.fetch
	}
	return d
}

func ev(typ, natural string) poller.DetectedEvent {
	return poller.DetectedEvent{SignalType: typ, NaturalEventID: natural, Title: "T " + natural, Description: "D " + natural,
		Source: "test", SourceURL: "https://e.example/" + natural, Weight: 7, OccurredAt: 1_700_000_000.5}
}

type eState struct {
	Enabled      bool
	Fails        int
	LastError    *string
	Next         *time.Time
	PolledSet    bool
	Cursor       map[string]any
	ResolvedCIK  *string
	MirrorExists bool
	MirrorEnable bool
	MirrorNext   *time.Time
}

func readState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ws, id string) (eState, bool) {
	t.Helper()
	var s eState
	var raw []byte
	var found bool
	err := tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT enabled, consecutive_failures, last_error, next_poll_at, last_polled_at IS NOT NULL, cursor::text::bytea, resolved_cik
			FROM watch_subscriptions WHERE id=$1`, id).Scan(&s.Enabled, &s.Fails, &s.LastError, &s.Next, &s.PolledSet, &raw, &s.ResolvedCIK)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		if err := json.Unmarshal(raw, &s.Cursor); err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `SELECT enabled, next_poll_at FROM watch_schedules WHERE watch_id=$1`, id).Scan(&s.MirrorEnable, &s.MirrorNext)
		if err == nil {
			s.MirrorExists = true
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, found
}

func mirrorExists(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id string) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM watch_schedules WHERE watch_id=$1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

type eSignal struct {
	ID, Company, Type, Title, Desc, Source, URL string
	Lead, Weight                                int
	Created                                     float64
	Read                                        bool
}

func eSignals(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ws string) []eSignal {
	t.Helper()
	var out []eSignal
	err := tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, company, signal_type, title, description, source, source_url, lead_id, weight, created_at, read
			FROM signals ORDER BY id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s eSignal
			if err := rows.Scan(&s.ID, &s.Company, &s.Type, &s.Title, &s.Desc, &s.Source, &s.URL, &s.Lead, &s.Weight, &s.Created, &s.Read); err != nil {
				return err
			}
			out = append(out, s)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func activeJobs(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ws string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE type='watch_poll' AND status='pending'
		AND payload->>'workspace_id' = $1`, ws).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func ledger(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ws string) (n, pn int, found bool) {
	t.Helper()
	err := tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT n, n_poll_now FROM poll_budget_ledger WHERE workspace_id=$1 AND day='2026-08-29'`, ws).Scan(&n, &pn)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		found = err == nil
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return
}

func run(t *testing.T, ctx context.Context, d poller.HandlerDeps, c *queue.Claim) {
	t.Helper()
	if err := poller.Handle(ctx, d, c); err != nil {
		t.Fatalf("Handle: %v", err)
	}
}

func str(s string) *string { return &s }

var wantNext = time.Date(2026, 8, 30, 8, 0, 0, 0, time.UTC)

// ── Python vectors ──────────────────────────────────────────────────────────

func TestPollMatchesPythonSignalsAndCursor(t *testing.T) {
	pool, ctx := eSetup(t)
	raw, err := readFile("testdata/poll_handler_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Vectors []struct {
			Name  string `json:"name"`
			Watch struct {
				Kind        string         `json:"kind"`
				Target      string         `json:"target"`
				SignalTypes []string       `json:"signal_types"`
				Config      map[string]any `json:"config"`
				Cursor      map[string]any `json:"cursor"`
				Enabled     *bool          `json:"enabled"`
				Fails       int            `json:"consecutive_failures"`
				LastError   *string        `json:"last_error"`
			} `json:"watch"`
			Payload       map[string]any `json:"payload"`
			Budget        int            `json:"budget"`
			SourceResults map[string]any `json:"source_results"`
			Lead          [2]any         `json:"lead"`
			LedgerBefore  []int          `json:"ledger_before"`
			PGStore       bool           `json:"pg_store"`
			Enabled       bool           `json:"enabled"`
			Missing       bool           `json:"missing"`
			SourcesCalled []struct {
				Src      string `json:"src"`
				FireKey  string `json:"fire_key"`
				LeadID   *int64 `json:"lead_id"`
				Backfill bool   `json:"backfill"`
			} `json:"sources_called"`
			Watches []struct {
				Enabled     bool           `json:"enabled"`
				Fails       int            `json:"consecutive_failures"`
				LastError   *string        `json:"last_error"`
				Next        *string        `json:"next_poll_at"`
				PolledSet   bool           `json:"last_polled_set"`
				Cursor      map[string]any `json:"cursor"`
				ResolvedCIK *string        `json:"resolved_cik"`
			} `json:"watches"`
			Mirror []struct {
				Enabled bool    `json:"enabled"`
				Next    *string `json:"next_poll_at"`
			} `json:"mirror"`
			Ledger []struct {
				N        int `json:"n"`
				NPollNow int `json:"n_poll_now"`
			} `json:"ledger"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	if len(file.Vectors) != 24 {
		t.Fatalf("vectors = %d", len(file.Vectors))
	}
	for _, v := range file.Vectors {
		t.Run(v.Name, func(t *testing.T) {
			eClean(ctx, pool)
			if !v.Missing {
				if v.Watch.SignalTypes == nil {
					v.Watch.SignalTypes = []string{"company_funded"}
				}
				w := eWatch{Kind: v.Watch.Kind, Disabled: v.Watch.Enabled != nil && !*v.Watch.Enabled, Fails: v.Watch.Fails, LastErr: v.Watch.LastError}
				if v.Watch.Target != "" {
					w.Target = v.Watch.Target
				}
				if v.Watch.SignalTypes != nil {
					b, _ := json.Marshal(v.Watch.SignalTypes)
					w.Types = string(b)
				}
				if v.Watch.Config != nil {
					b, _ := json.Marshal(v.Watch.Config)
					w.Config = string(b)
				}
				if v.Watch.Cursor != nil {
					b, _ := json.Marshal(v.Watch.Cursor)
					w.Cursor = string(b)
				}
				if id, ok := v.Lead[0].(float64); ok {
					l := eLeadBase + int(id)
					w.LeadID = &l
				}
				w.insert(t, ctx, pool)
				if v.Lead[1] == "ambiguous_lead" {
					eLead(t, ctx, pool, eWS, 1, "Acme Robotics")
					eLead(t, ctx, pool, eWS, 2, "Acme Rockets")
				}
			}
			if v.LedgerBefore != nil {
				_ = tenancy.InWorkspace(ctx, pool, eWS, func(tx pgx.Tx) error {
					_, err := tx.Exec(ctx, `INSERT INTO poll_budget_ledger (workspace_id, day, n, n_poll_now) VALUES ($1,'2026-08-29',$2,$3)`,
						eWS, v.LedgerBefore[0], v.LedgerBefore[1])
					return err
				})
			}
			stub := &eStub{results: map[string]func() ([]poller.DetectedEvent, map[string]any, error){}}
			for src, r := range v.SourceResults {
				switch x := r.(type) {
				case nil:
				case bool: // false: a retryable failure
					stub.results[src] = func() ([]poller.DetectedEvent, map[string]any, error) {
						return nil, nil, errors.New("scripted failure")
					}
				case []any: // (emitted, dupe, collector failure)
					stub.results[src] = func() ([]poller.DetectedEvent, map[string]any, error) {
						return nil, map[string]any{"_collector_failures": []any{"collector"}}, nil
					}
					_ = x
				}
			}
			d := eDeps(pool, stub, nil)
			d.Enabled, d.PGStore = v.Enabled, v.PGStore
			d.Cfg.DailyPollBudget = v.Budget
			if v.Payload["workspace_id"] == "ws-1" {
				v.Payload["workspace_id"] = eWS
			}
			if v.Payload["watch_id"] == "w-1" {
				v.Payload["watch_id"] = eW
			}
			payload, _ := json.Marshal(v.Payload)
			c := eClaim(t, ctx, pool, eWS, string(payload))
			run(t, ctx, d, c)

			if len(stub.calls) != len(v.SourcesCalled) {
				t.Fatalf("sources called %v, want %d", stub.called(), len(v.SourcesCalled))
			}
			for i, want := range v.SourcesCalled {
				got := stub.calls[i]
				fk := strings.ReplaceAll(want.FireKey, "job:5", fmt.Sprintf("job:%d", c.ID))
				var lead int64
				if want.LeadID != nil {
					lead = eLeadBase + *want.LeadID
				}
				if got.Src != want.Src || got.FireKey != fk || got.Lead != lead || got.Backfill != want.Backfill {
					t.Errorf("call %d = %+v, want %+v (fire key %s)", i, got, want, fk)
				}
			}
			for _, want := range v.Watches {
				got, found := readState(t, ctx, pool, eWS, eW)
				if !found {
					t.Fatal("watch row vanished")
				}
				wantErr := want.LastError
				if v.Name == "collector_failure_flag_is_failure" {
					wantErr = str("collector") // the seam can only express the flag through the failure text
				}
				if got.Enabled != want.Enabled || got.Fails != want.Fails || !reflect.DeepEqual(got.LastError, wantErr) ||
					got.PolledSet != want.PolledSet || !reflect.DeepEqual(got.Cursor, want.Cursor) || !reflect.DeepEqual(got.ResolvedCIK, want.ResolvedCIK) {
					t.Errorf("watch = %+v\nwant   %+v", got, want)
				}
				if (got.Next == nil) != (want.Next == nil) || (want.Next != nil && !got.Next.Equal(mustTime(t, *want.Next))) {
					t.Errorf("next_poll_at = %v, want %v", got.Next, want.Next)
				}
				if len(v.Mirror) == 1 {
					m := v.Mirror[0]
					if !got.MirrorExists || got.MirrorEnable != m.Enabled || (got.MirrorNext == nil) != (m.Next == nil) ||
						(m.Next != nil && !got.MirrorNext.Equal(mustTime(t, *m.Next))) {
						t.Errorf("mirror = %+v, want %+v", got, m)
					}
				}
			}
			if len(v.Watches) == 0 || len(v.Mirror) == 0 {
				if mirrorExists(t, ctx, pool, eW) && len(v.Mirror) == 0 {
					t.Errorf("mirror row should be gone")
				}
			}
			n, pn, found := ledger(t, ctx, pool, eWS)
			if found != (len(v.Ledger) == 1) || (found && (n != v.Ledger[0].N || pn != v.Ledger[0].NPollNow)) {
				t.Errorf("ledger = (%d,%d,%v), want %+v", n, pn, found, v.Ledger)
			}
		})
	}
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

// A real hiring source (pyworker reply recorded from Python) through the handler:
// the surge signal, its deterministic id and the merged cursor.
type recordedPy struct{ out json.RawMessage }

func (r recordedPy) Call(context.Context, pyworker.ProviderCall) (pyworker.ProviderResult, error) {
	return pyworker.ProviderResult{Status: "ok", Output: r.out}, nil
}

func TestPollRealHiringSourceEmitsSurge(t *testing.T) {
	pool, ctx := eSetup(t)
	tr, err := readFile("testdata/transcripts/pyworker/hiring_growing.json")
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		Response struct {
			Output json.RawMessage `json:"output"`
		} `json:"response"`
	}
	if err := json.Unmarshal(tr, &rec); err != nil {
		t.Fatal(err)
	}
	eLead(t, ctx, pool, eWS, 5, "Acme")
	eWatch{Kind: "hiring", Types: `["hiring_surge"]`, Cursor: `{"bootstrapped": true}`}.insert(t, ctx, pool)
	d := eDeps(pool, nil, nil)
	d.Py.Client = recordedPy{rec.Response.Output}
	run(t, ctx, d, eClaim(t, ctx, pool, eWS, ePayload(eWS, eW)))
	sigs := eSignals(t, ctx, pool, eWS)
	week := "2026-W35"
	wantID := poller.SignalEventID(eWS, "hiring", poller.StableCompanyID(nil, "Acme"), "hiring_surge", poller.StableCompanyID(nil, "Acme")+":"+week)
	if len(sigs) != 1 || sigs[0].ID != wantID || sigs[0].Lead != eLeadBase+5 || sigs[0].Type != "hiring_surge" || sigs[0].Weight != 7 || sigs[0].Read {
		t.Fatalf("signals = %+v want id %s", sigs, wantID)
	}
	st, _ := readState(t, ctx, pool, eWS, eW)
	if st.Cursor["hiring_band"] != "growing" || st.Cursor["hiring_band_week"] != week || st.Cursor["bootstrapped"] != true || st.Cursor["attempt_count"] != float64(1) {
		t.Errorf("cursor = %v", st.Cursor)
	}
}

// ── scenarios ───────────────────────────────────────────────────────────────

func TestEmissionFieldsLeadRoutingAndResolvedCIK(t *testing.T) {
	pool, ctx := eSetup(t)
	eLead(t, ctx, pool, eWS, 7, "ACME, Inc.")
	eLead(t, ctx, pool, eWS, 3, "Acme Incorporated") // fuzzy only
	eLead(t, ctx, pool, eWS, 2, "acme inc")          // exact, lowest id
	eWatch{Kind: "company", Target: "Acme Inc", Types: `["company_funded"]`, Cursor: `{"bootstrapped": true}`}.insert(t, ctx, pool)
	stub := &eStub{results: map[string]func() ([]poller.DetectedEvent, map[string]any, error){
		"funding": func() ([]poller.DetectedEvent, map[string]any, error) {
			e := ev("company_funded", "n1")
			e.OccurredAt = 0 // falls back to now
			return []poller.DetectedEvent{e}, map[string]any{"_resolved_cik": "0001234567", "sec_last_accession": "x-1"}, nil
		},
	}}
	run(t, ctx, eDeps(pool, stub, nil), eClaim(t, ctx, pool, eWS, ePayload(eWS, eW)))
	sigs := eSignals(t, ctx, pool, eWS)
	if len(sigs) != 1 {
		t.Fatalf("signals = %+v", sigs)
	}
	rc := "0001234567"
	wantID := poller.SignalEventID(eWS, "company", poller.StableCompanyID(&rc, "Acme Inc"), "company_funded", "n1")
	s := sigs[0]
	if s.ID != wantID || s.Lead != eLeadBase+2 || s.Company != "Acme Inc" || s.Title != "T n1" || s.Desc != "D n1" || s.Source != "test" ||
		s.URL != "https://e.example/n1" || s.Weight != 7 || s.Read || s.Created != float64(vNow.UnixNano())/1e9 {
		t.Errorf("signal = %+v", s)
	}
	st, _ := readState(t, ctx, pool, eWS, eW)
	if st.ResolvedCIK == nil || *st.ResolvedCIK != rc || st.Cursor["sec_last_accession"] != "x-1" || st.Cursor["_resolved_cik"] != nil {
		t.Errorf("state = %+v", st)
	}
	// resolved_cik is set only when unset: a second poll offering another value keeps the first.
	stub.results["funding"] = func() ([]poller.DetectedEvent, map[string]any, error) {
		return nil, map[string]any{"_resolved_cik": "9999999999"}, nil
	}
	run(t, ctx, eDeps(pool, stub, nil), eClaim(t, ctx, pool, eWS, ePayload(eWS, eW)))
	st, _ = readState(t, ctx, pool, eWS, eW)
	if *st.ResolvedCIK != rc {
		t.Errorf("resolved_cik overwritten: %v", *st.ResolvedCIK)
	}
}

func TestReEmitAfterCursorRollbackIsNoop(t *testing.T) {
	pool, ctx := eSetup(t)
	eLead(t, ctx, pool, eWS, 1, "Acme")
	eWatch{Kind: "feed", Target: "Acme", Cursor: `{"bootstrapped": true}`}.insert(t, ctx, pool)
	events := func() ([]poller.DetectedEvent, map[string]any, error) {
		return []poller.DetectedEvent{ev("news", "a"), ev("news", "b")}, map[string]any{"feed_seen": []any{"a", "b"}}, nil
	}
	stub := &eStub{results: map[string]func() ([]poller.DetectedEvent, map[string]any, error){"feed": events}}

	// Crash between emit and cursor commit: nothing (signals, cursor, hook) survives.
	hook := &eHook{fail: true}
	err := poller.Handle(ctx, eDeps(pool, stub, hook), eClaim(t, ctx, pool, eWS, ePayload(eWS, eW)))
	if err == nil {
		t.Fatal("expected the simulated crash to surface")
	}
	if n := len(eSignals(t, ctx, pool, eWS)); n != 0 {
		t.Fatalf("signals after rollback = %d", n)
	}
	st, _ := readState(t, ctx, pool, eWS, eW)
	if _, ok := st.Cursor["feed_seen"]; ok {
		t.Fatalf("cursor advanced: %v", st.Cursor)
	}

	// The same signals already committed (a racing Python poll), cursor still behind: re-emit is a no-op.
	hook.fail = false
	if err := tenancy.InWorkspace(ctx, pool, eWS, func(tx pgx.Tx) error {
		_, err := poller.EmitEvents(ctx, tx, nil, poller.WatchRow{ID: eW, WorkspaceID: eWS, Kind: "feed", Target: "Acme"}, eLeadBase+1,
			[]poller.DetectedEvent{ev("news", "a"), ev("news", "b")}, vNow)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	run(t, ctx, eDeps(pool, stub, hook), eClaim(t, ctx, pool, eWS, ePayload(eWS, eW)))
	if n := len(eSignals(t, ctx, pool, eWS)); n != 2 {
		t.Fatalf("signals = %d, want 2", n)
	}
	if hook.fired() != 0 {
		t.Errorf("on_signal fired %d times for rows that already existed", hook.fired())
	}
	st, _ = readState(t, ctx, pool, eWS, eW)
	if _, ok := st.Cursor["feed_seen"]; !ok {
		t.Errorf("cursor not advanced: %v", st.Cursor)
	}
}

func TestSourceFailureDoesNotAdvanceCursor(t *testing.T) {
	pool, ctx := eSetup(t)
	eLead(t, ctx, pool, eWS, 1, "Acme")
	eWatch{Kind: "company", Types: `["company_funded","hiring_surge"]`, Cursor: `{"bootstrapped": true, "hiring_band": "none"}`}.insert(t, ctx, pool)
	stub := &eStub{results: map[string]func() ([]poller.DetectedEvent, map[string]any, error){
		"funding": func() ([]poller.DetectedEvent, map[string]any, error) {
			return []poller.DetectedEvent{ev("company_funded", "f1")}, map[string]any{"sec_last_accession": "a-1"}, nil
		},
		"hiring": func() ([]poller.DetectedEvent, map[string]any, error) { return nil, nil, errors.New("jobspy down") },
	}}
	run(t, ctx, eDeps(pool, stub, nil), eClaim(t, ctx, pool, eWS, ePayload(eWS, eW)))
	st, _ := readState(t, ctx, pool, eWS, eW)
	if st.Cursor["sec_last_accession"] != "a-1" || st.Cursor["hiring_band"] != "none" || st.Fails != 1 || st.LastError != nil {
		t.Errorf("state = %+v", st)
	}
	if n := len(eSignals(t, ctx, pool, eWS)); n != 1 {
		t.Errorf("signals = %d (the healthy source still emits)", n)
	}
	if st.Next == nil || st.Next.Before(vNow) {
		t.Errorf("not rescheduled with backoff: %v", st.Next)
	}
}

func TestBillingRefusalIsFailureNotHealthy(t *testing.T) {
	pool, ctx := eSetup(t)
	eLead(t, ctx, pool, eWS, 1, "Acme")
	eWatch{Kind: "funding", Cursor: `{"bootstrapped": true}`}.insert(t, ctx, pool)
	stub := &eStub{}
	d := eDeps(pool, stub, nil)
	d.Cfg.FundingCostUSD, d.Cfg.BillingEnabled = 0.5, true
	run(t, ctx, d, eClaim(t, ctx, pool, eWS, ePayload(eWS, eW)))
	st, _ := readState(t, ctx, pool, eWS, eW)
	if len(stub.calls) != 0 || st.LastError == nil || *st.LastError != "insufficient_credits" || st.Fails != 1 {
		t.Errorf("calls=%v state=%+v", stub.called(), st)
	}
	if !st.PolledSet || st.Cursor["attempt_count"] != float64(1) {
		t.Errorf("finalise did not run: %+v", st)
	}
	// With credits the same poll fetches and clears the error.
	if err := tenancy.InWorkspace(ctx, pool, eWS, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO workspace_credits (workspace_id, balance_usd) VALUES ($1, 5.0)
			ON CONFLICT (workspace_id) DO UPDATE SET balance_usd = 5.0`, eWS)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	run(t, ctx, d, eClaim(t, ctx, pool, eWS, ePayload(eWS, eW)))
	st, _ = readState(t, ctx, pool, eWS, eW)
	if len(stub.calls) != 1 || st.LastError != nil || st.Fails != 0 {
		t.Errorf("calls=%v state=%+v", stub.called(), st)
	}
}

func TestAmbiguousLeadReschedulesWithoutPoll(t *testing.T) {
	pool, ctx := eSetup(t)
	eLead(t, ctx, pool, eWS, 1, "Acme Robotics")
	eLead(t, ctx, pool, eWS, 2, "Acme Rockets")
	eLead(t, ctx, pool, eWS2, 3, "Acme") // another tenant's exact match is invisible
	eWatch{Kind: "funding"}.insert(t, ctx, pool)
	stub := &eStub{}
	run(t, ctx, eDeps(pool, stub, nil), eClaim(t, ctx, pool, eWS, ePayload(eWS, eW)))
	st, _ := readState(t, ctx, pool, eWS, eW)
	if len(stub.calls) != 0 || st.LastError == nil || *st.LastError != "ambiguous_lead" || st.Fails != 0 || !st.PolledSet ||
		st.Next == nil || !st.Next.Equal(wantNext) || st.Cursor["bootstrapped"] != false {
		t.Errorf("calls=%v state=%+v", stub.called(), st)
	}
	if activeJobs(t, ctx, pool, eWS) != 1 {
		t.Errorf("next poll not enqueued")
	}
}

func TestUnresolvedLeadKeepsCursor(t *testing.T) {
	pool, ctx := eSetup(t)
	eLead(t, ctx, pool, eWS2, 3, "Acme") // only the foreign tenant has the company
	eWatch{Kind: "funding", Cursor: `{"bootstrapped": true}`}.insert(t, ctx, pool)
	stub := &eStub{results: map[string]func() ([]poller.DetectedEvent, map[string]any, error){
		"funding": func() ([]poller.DetectedEvent, map[string]any, error) {
			return []poller.DetectedEvent{ev("company_funded", "u1")}, map[string]any{"_resolved_cik": "0000000042", "sec_last_accession": "z"}, nil
		},
	}}
	run(t, ctx, eDeps(pool, stub, nil), eClaim(t, ctx, pool, eWS, ePayload(eWS, eW)))
	st, _ := readState(t, ctx, pool, eWS, eW)
	if _, ok := st.Cursor["sec_last_accession"]; ok {
		t.Errorf("cursor advanced: %v", st.Cursor)
	}
	if st.LastError == nil || *st.LastError != "unresolved_lead" || st.Fails != 1 {
		t.Errorf("state = %+v", st)
	}
	if st.ResolvedCIK == nil || *st.ResolvedCIK != "0000000042" {
		t.Errorf("resolved_cik is committed with the source like Python: %v", st.ResolvedCIK)
	}
	if n := len(eSignals(t, ctx, pool, eWS)) + len(eSignals(t, ctx, pool, eWS2)); n != 0 {
		t.Errorf("signals = %d", n)
	}
}

func TestBudgetExceededReschedulesWithoutFetch(t *testing.T) {
	pool, ctx := eSetup(t)
	eWatch{Kind: "funding", Fails: 2, LastErr: str("x")}.insert(t, ctx, pool)
	_ = tenancy.InWorkspace(ctx, pool, eWS, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO poll_budget_ledger (workspace_id, day, n, n_poll_now) VALUES ($1,'2026-08-29',3,0)`, eWS)
		return err
	})
	stub := &eStub{}
	d := eDeps(pool, stub, nil)
	d.Cfg.DailyPollBudget = 3
	run(t, ctx, d, eClaim(t, ctx, pool, eWS, ePayload(eWS, eW)))
	st, _ := readState(t, ctx, pool, eWS, eW)
	n, _, _ := ledger(t, ctx, pool, eWS)
	if len(stub.calls) != 0 || st.Fails != 0 || n != 4 || st.Next == nil || !st.Next.Equal(wantNext) || st.PolledSet {
		t.Errorf("calls=%v n=%d state=%+v", stub.called(), n, st)
	}
	if activeJobs(t, ctx, pool, eWS) != 1 {
		t.Errorf("next poll not enqueued")
	}
}

func TestDisabledWatchDropsMirrorAndStops(t *testing.T) {
	pool, ctx := eSetup(t)
	eWatch{Kind: "funding", Disabled: true}.insert(t, ctx, pool)
	stub := &eStub{}
	run(t, ctx, eDeps(pool, stub, nil), eClaim(t, ctx, pool, eWS, ePayload(eWS, eW)))
	if len(stub.calls) != 0 || mirrorExists(t, ctx, pool, eW) || activeJobs(t, ctx, pool, eWS) != 0 {
		t.Errorf("calls=%v mirror=%v", stub.called(), mirrorExists(t, ctx, pool, eW))
	}
	if _, _, found := ledger(t, ctx, pool, eWS); found {
		t.Errorf("a disabled watch must not spend budget")
	}
	// A watch that does not exist behaves the same.
	run(t, ctx, eDeps(pool, stub, nil), eClaim(t, ctx, pool, eWS, ePayload(eWS, "p08t15-nope")))
}

func TestOnSignalFiresOnlyForInsertedRows(t *testing.T) {
	pool, ctx := eSetup(t)
	eLead(t, ctx, pool, eWS, 1, "Acme")
	eWatch{Kind: "funding", Cursor: `{"bootstrapped": true}`}.insert(t, ctx, pool)
	two := func() ([]poller.DetectedEvent, map[string]any, error) {
		return []poller.DetectedEvent{ev("company_funded", "a"), ev("company_funded", "b")}, map[string]any{}, nil
	}
	stub := &eStub{results: map[string]func() ([]poller.DetectedEvent, map[string]any, error){"funding": two}}
	hook := &eHook{}
	run(t, ctx, eDeps(pool, stub, hook), eClaim(t, ctx, pool, eWS, ePayload(eWS, eW)))
	if hook.fired() != 2 {
		t.Fatalf("first poll fired %d", hook.fired())
	}
	for _, r := range hook.calls[0] {
		if r.LeadID == nil || *r.LeadID != eLeadBase+1 || r.Type != "company_funded" || r.PK == "" {
			t.Errorf("ref = %+v", r)
		}
	}
	// Same batch plus one new item: only the new row fires.
	stub.results["funding"] = func() ([]poller.DetectedEvent, map[string]any, error) {
		return []poller.DetectedEvent{ev("company_funded", "a"), ev("company_funded", "b"), ev("company_funded", "c")}, map[string]any{}, nil
	}
	run(t, ctx, eDeps(pool, stub, hook), eClaim(t, ctx, pool, eWS, ePayload(eWS, eW)))
	if hook.fired() != 3 || len(eSignals(t, ctx, pool, eWS)) != 3 {
		t.Errorf("fired %d, signals %d", hook.fired(), len(eSignals(t, ctx, pool, eWS)))
	}
}

func TestJobChangeRoutesPerContactLead(t *testing.T) {
	pool, ctx := eSetup(t)
	eLead(t, ctx, pool, eWS, 14, "Hot Pursuit Labs")
	eLead(t, ctx, pool, eWS, 15, "Quantum Foods")
	eWatch{Kind: "job_change", Target: "My roster", Cursor: `{"bootstrapped": true}`}.insert(t, ctx, pool)
	l14, l15 := int64(eLeadBase+14), int64(eLeadBase+15)
	mk := func(natural string, lead *int64, company string) poller.DetectedEvent {
		e := ev("job_change", natural)
		e.LeadID, e.Company = lead, company
		return e
	}
	stub := &eStub{results: map[string]func() ([]poller.DetectedEvent, map[string]any, error){
		"job_change": func() ([]poller.DetectedEvent, map[string]any, error) {
			return []poller.DetectedEvent{mk("c1", &l14, "Hot Pursuit Labs"), mk("c2", &l15, "Quantum Foods"), mk("c3", nil, "")},
				map[string]any{"job_change_contacts": map[string]any{"k": "v"}}, nil
		},
	}}
	hook := &eHook{}
	run(t, ctx, eDeps(pool, stub, hook), eClaim(t, ctx, pool, eWS, ePayload(eWS, eW)))
	got := map[string]eSignal{}
	for _, s := range eSignals(t, ctx, pool, eWS) {
		got[s.Title] = s
	}
	if len(got) != 3 || got["T c1"].Lead != eLeadBase+14 || got["T c1"].Company != "Hot Pursuit Labs" || got["T c2"].Lead != eLeadBase+15 ||
		got["T c3"].Lead != 0 || got["T c3"].Company != "My roster" {
		t.Errorf("signals = %+v", got)
	}
	routed := 0
	for _, call := range hook.calls {
		for _, r := range call {
			if r.LeadID != nil {
				routed++
			}
		}
	}
	if hook.fired() != 3 || routed != 2 {
		t.Errorf("hook refs %d, routed %d (the unrouted contact carries a nil lead)", hook.fired(), routed)
	}
	st, _ := readState(t, ctx, pool, eWS, eW)
	if st.Cursor["job_change_contacts"] == nil || st.Fails != 0 {
		t.Errorf("state = %+v", st)
	}
}

func TestCollectorFailuresPartialSuccess(t *testing.T) {
	pool, ctx := eSetup(t)
	eWatch{Kind: "account_group", Target: "Group", Config: `{"accounts": []}`, Cursor: `{"bootstrapped": true}`}.insert(t, ctx, pool)
	long := strings.Repeat("é", 300)
	stub := &eStub{results: map[string]func() ([]poller.DetectedEvent, map[string]any, error){
		"account_group": func() ([]poller.DetectedEvent, map[string]any, error) {
			return []poller.DetectedEvent{ev("hiring_surge", "g1")},
				map[string]any{"_collector_failures": []any{long, "second"}, "collector_health": map[string]any{"hiring": "failed"}}, nil
		},
	}}
	run(t, ctx, eDeps(pool, stub, nil), eClaim(t, ctx, pool, eWS, ePayload(eWS, eW)))
	st, _ := readState(t, ctx, pool, eWS, eW)
	if st.LastError == nil || *st.LastError != strings.Repeat("é", 255) {
		t.Errorf("last_error = %v", st.LastError)
	}
	if st.Fails != 1 || st.Cursor["collector_health"] == nil || st.Cursor["_collector_failures"] != nil {
		t.Errorf("state = %+v", st)
	}
	if n := len(eSignals(t, ctx, pool, eWS)); n != 1 {
		t.Errorf("signals = %d: a partial collector failure still emits", n)
	}
}

func TestAutoDisableAtMaxFailures(t *testing.T) {
	pool, ctx := eSetup(t)
	eLead(t, ctx, pool, eWS, 1, "Acme")
	eWatch{Kind: "funding", Fails: 2}.insert(t, ctx, pool)
	stub := &eStub{results: map[string]func() ([]poller.DetectedEvent, map[string]any, error){
		"funding": func() ([]poller.DetectedEvent, map[string]any, error) { return nil, nil, errors.New("boom") },
	}}
	d := eDeps(pool, stub, nil)
	d.Cfg.MaxConsecutiveFailures = 3
	run(t, ctx, d, eClaim(t, ctx, pool, eWS, ePayload(eWS, eW)))
	st, _ := readState(t, ctx, pool, eWS, eW)
	if st.Enabled || st.Fails != 3 || st.LastError == nil || *st.LastError != "auto_disabled" || st.Next != nil ||
		!st.MirrorExists || st.MirrorEnable || st.MirrorNext != nil || activeJobs(t, ctx, pool, eWS) != 0 {
		t.Errorf("state = %+v", st)
	}
}

func TestPollNowCountsQuota(t *testing.T) {
	pool, ctx := eSetup(t)
	eLead(t, ctx, pool, eWS, 1, "Acme")
	eWatch{Kind: "funding"}.insert(t, ctx, pool)
	stub := &eStub{}
	run(t, ctx, eDeps(pool, stub, nil), eClaim(t, ctx, pool, eWS, ePayload(eWS, eW, `"poll_now": true`)))
	run(t, ctx, eDeps(pool, stub, nil), eClaim(t, ctx, pool, eWS, ePayload(eWS, eW)))
	n, pn, _ := ledger(t, ctx, pool, eWS)
	var used int
	if err := tenancy.InWorkspace(ctx, pool, eWS, func(tx pgx.Tx) (err error) {
		used, err = poller.PollNowQuotaUsed(ctx, tx, eWS, vNow)
		return
	}); err != nil {
		t.Fatal(err)
	}
	if n != 2 || pn != 1 || used != 1 {
		t.Errorf("ledger n=%d poll_now=%d quota=%d", n, pn, used)
	}
}

func TestCancelBetweenSources(t *testing.T) {
	pool, ctx := eSetup(t)
	eLead(t, ctx, pool, eWS, 1, "Acme")
	eWatch{Kind: "company", Cursor: `{"bootstrapped": true}`}.insert(t, ctx, pool)
	c := eClaim(t, ctx, pool, eWS, ePayload(eWS, eW))
	stub := &eStub{results: map[string]func() ([]poller.DetectedEvent, map[string]any, error){
		"funding_exec": func() ([]poller.DetectedEvent, map[string]any, error) {
			return []poller.DetectedEvent{ev("company_funded", "k1")}, map[string]any{"sec_last_accession": "k"}, nil
		},
	}}
	stub.onCall = func(src string) {
		if src == "funding_exec" { // the job is cancelled while the first source runs
			if _, err := pool.Exec(ctx, `UPDATE jobs SET status='cancelled' WHERE id=$1`, c.ID); err != nil {
				t.Error(err)
			}
		}
	}
	run(t, ctx, eDeps(pool, stub, nil), c)
	if got := stub.called(); len(got) != 1 || got[0] != "funding_exec" {
		t.Fatalf("sources called = %v", got)
	}
	st, _ := readState(t, ctx, pool, eWS, eW)
	// The running source finished whole (signals + cursor); nothing after it ran.
	if st.Cursor["sec_last_accession"] != "k" || len(eSignals(t, ctx, pool, eWS)) != 1 || st.PolledSet || st.Cursor["attempt_count"] != nil {
		t.Errorf("state = %+v", st)
	}
}

func TestLeaseLossLeavesStateConsistent(t *testing.T) {
	pool, ctx := eSetup(t)
	eLead(t, ctx, pool, eWS, 1, "Acme")
	eWatch{Kind: "company", Cursor: `{"bootstrapped": true}`}.insert(t, ctx, pool)
	c := eClaim(t, ctx, pool, eWS, ePayload(eWS, eW))
	stub := &eStub{results: map[string]func() ([]poller.DetectedEvent, map[string]any, error){
		"funding_exec": func() ([]poller.DetectedEvent, map[string]any, error) {
			return []poller.DetectedEvent{ev("company_funded", "l1")}, map[string]any{"sec_last_accession": "l"}, nil
		},
	}}
	stub.onCall = func(src string) {
		if src == "funding_exec" { // another worker reclaimed the job
			if _, err := pool.Exec(ctx, `UPDATE jobs SET lease_token = lease_token + 1 WHERE id=$1`, c.ID); err != nil {
				t.Error(err)
			}
		}
	}
	run(t, ctx, eDeps(pool, stub, nil), c)
	if got := stub.called(); len(got) != 1 {
		t.Fatalf("sources called = %v", got)
	}
	st, _ := readState(t, ctx, pool, eWS, eW)
	sigs := eSignals(t, ctx, pool, eWS)
	_, advanced := st.Cursor["sec_last_accession"]
	if advanced != (len(sigs) == 1) || st.PolledSet || st.Fails != 0 {
		t.Errorf("inconsistent: cursor advanced=%v signals=%d state=%+v", advanced, len(sigs), st)
	}
}

func TestGoAndPythonPollSameWatchEmitOnce(t *testing.T) {
	pool, ctx := eSetup(t)
	eLead(t, ctx, pool, eWS, 1, "Acme")
	eWatch{Kind: "funding", Cursor: `{"bootstrapped": true}`}.insert(t, ctx, pool)
	rc := func() ([]poller.DetectedEvent, map[string]any, error) {
		return []poller.DetectedEvent{ev("company_funded", "x"), ev("company_funded", "y")}, map[string]any{"sec_last_accession": "s"}, nil
	}
	stub := &eStub{results: map[string]func() ([]poller.DetectedEvent, map[string]any, error){"funding": rc}}
	hook := &eHook{}
	// "Python" already inserted one of the two (same deterministic id, raw SQL as Python's ORM does).
	pyID := poller.SignalEventID(eWS, "funding", poller.StableCompanyID(nil, "Acme"), "company_funded", "x")
	if err := tenancy.InWorkspace(ctx, pool, eWS, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO signals (id, workspace_id, lead_id, company, signal_type, title, description, source, source_url, weight, created_at, read)
			VALUES ($1,$2,$3,'Acme','company_funded','T x','D x','test','https://e.example/x',7,1700000000.5,false)`, pyID, eWS, eLeadBase+1)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		c := eClaim(t, ctx, pool, eWS, ePayload(eWS, eW, fmt.Sprintf(`"fire_key": "race-%d"`, i)))
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := poller.Handle(ctx, eDeps(pool, stub, hook), c); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := len(eSignals(t, ctx, pool, eWS)); n != 2 {
		t.Errorf("signals = %d, want 2", n)
	}
	if hook.fired() != 1 {
		t.Errorf("on_signal fired %d times; only the one row Go inserted may fire", hook.fired())
	}
}

func TestTenantIsolation(t *testing.T) {
	pool, ctx := eSetup(t)
	eLead(t, ctx, pool, eWS2, 9, "Acme")
	eWatch{Kind: "funding", Cursor: `{"bootstrapped": true}`}.insert(t, ctx, pool)
	stub := &eStub{results: map[string]func() ([]poller.DetectedEvent, map[string]any, error){
		"funding": func() ([]poller.DetectedEvent, map[string]any, error) {
			return []poller.DetectedEvent{ev("company_funded", "t1")}, map[string]any{}, nil
		},
	}}
	// A job claiming the watch under another workspace finds nothing and touches nothing.
	run(t, ctx, eDeps(pool, stub, nil), eClaim(t, ctx, pool, eWS2, ePayload(eWS2, eW)))
	st, _ := readState(t, ctx, pool, eWS, eW)
	if len(stub.calls) != 0 || st.PolledSet || !st.MirrorExists || st.Cursor["attempt_count"] != nil {
		t.Errorf("foreign poll reached the watch: calls=%v state=%+v", stub.called(), st)
	}
	if n := len(eSignals(t, ctx, pool, eWS2)); n != 0 {
		t.Errorf("signals leaked into the other tenant: %d", n)
	}
	// The owning workspace resolves leads only from its own rows: the other tenant's "Acme" is not used.
	run(t, ctx, eDeps(pool, stub, nil), eClaim(t, ctx, pool, eWS, ePayload(eWS, eW)))
	st, _ = readState(t, ctx, pool, eWS, eW)
	if len(eSignals(t, ctx, pool, eWS))+len(eSignals(t, ctx, pool, eWS2)) != 0 || st.LastError == nil || *st.LastError != "unresolved_lead" {
		t.Errorf("state = %+v", st)
	}
}

// Python no-ops (flag off, payload gaps) never touch the database.
func TestHandleNoOps(t *testing.T) {
	pool, ctx := eSetup(t)
	eWatch{Kind: "funding"}.insert(t, ctx, pool)
	stub := &eStub{}
	d := eDeps(pool, stub, nil)
	d.Enabled = false
	run(t, ctx, d, eClaim(t, ctx, pool, eWS, ePayload(eWS, eW)))
	d = eDeps(pool, stub, nil)
	for _, p := range []string{`{}`, `{"workspace_id": "x"}`, `{"watch_id": 5, "workspace_id": "x"}`, `not json`} {
		run(t, ctx, d, eClaim(t, ctx, pool, eWS, `{}`))
		run(t, ctx, d, &queue.Claim{ID: 1, Payload: json.RawMessage(p)})
	}
	run(t, ctx, d, nil)
	if len(stub.calls) != 0 {
		t.Errorf("calls = %v", stub.called())
	}
	if _, _, found := ledger(t, ctx, pool, eWS); found {
		t.Errorf("budget spent by a no-op")
	}
}

// No transaction (and so no balance row lock or pooled connection) spans a fetch:
// while a source's fetch is blocked, another debit in the same workspace completes.
func TestNoLockHeldDuringFetch(t *testing.T) {
	pool, ctx := eSetup(t)
	eLead(t, ctx, pool, eWS, 1, "Acme")
	eWatch{Kind: "funding", Cursor: `{"bootstrapped": true}`}.insert(t, ctx, pool)
	if err := tenancy.InWorkspace(ctx, pool, eWS, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO workspace_credits (workspace_id, balance_usd) VALUES ($1, 5.0)
			ON CONFLICT (workspace_id) DO UPDATE SET balance_usd = 5.0`, eWS)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	inFetch, release := make(chan struct{}), make(chan struct{})
	stub := &eStub{}
	stub.onCall = func(string) { close(inFetch); <-release }
	d := eDeps(pool, stub, nil)
	d.Cfg.FundingCostUSD, d.Cfg.BillingEnabled = 0.5, true
	done := make(chan error, 1)
	go func() { done <- poller.Handle(ctx, d, eClaim(t, ctx, pool, eWS, ePayload(eWS, eW))) }()
	<-inFetch
	cfgCopy := d.Cfg
	other := make(chan error, 1)
	go func() {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		other <- tenancy.InWorkspace(cctx, pool, eWS, func(tx pgx.Tx) error {
			ok, err := poller.DebitSource(cctx, tx, cfgCopy, eWS, "funding", "other-poll")
			if err == nil && !ok {
				err = errors.New("refused")
			}
			return err
		})
	}()
	select {
	case err := <-other:
		if err != nil {
			t.Errorf("concurrent debit blocked or failed during the fetch: %v", err)
		}
	case <-time.After(6 * time.Second):
		t.Error("concurrent debit still blocked")
	}
	var idle int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE state='idle in transaction' AND datname=current_database()`).Scan(&idle)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if idle != 0 {
		t.Errorf("%d idle-in-transaction sessions during the fetch", idle)
	}
}

// A cancel that arrives before the first source is honoured.
func TestCancelBeforeFirstSource(t *testing.T) {
	pool, ctx := eSetup(t)
	eLead(t, ctx, pool, eWS, 1, "Acme")
	eWatch{Kind: "funding", Cursor: `{"bootstrapped": true}`}.insert(t, ctx, pool)
	c := eClaim(t, ctx, pool, eWS, ePayload(eWS, eW))
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status='cancelled' WHERE id=$1`, c.ID); err != nil {
		t.Fatal(err)
	}
	stub := &eStub{}
	run(t, ctx, eDeps(pool, stub, nil), c)
	if len(stub.calls) != 0 {
		t.Errorf("sources called: %v", stub.called())
	}
}
