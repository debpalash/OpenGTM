//go:build integration

package audiencerefresh_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audiencerefresh"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/deststore"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

const (
	wsA = "ws-p10t12-a"
	wsB = "ws-p10t12-b"
	aud = "p10t12-aud"
)

func setup(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	pool := testpg.NewAppPool(t, 24)
	ctx := context.Background()
	clean := func() {
		for _, w := range []string{wsA, wsB} {
			_ = tenancy.InWorkspace(ctx, pool, w, func(tx pgx.Tx) error {
				for _, q := range []string{
					`DELETE FROM audience_membership_events`, `DELETE FROM audience_members`,
					`DELETE FROM audiences`, `DELETE FROM leads`,
				} {
					if _, err := tx.Exec(ctx, q); err != nil {
						return err
					}
				}
				return nil
			})
		}
		_, _ = pool.Exec(ctx, `DELETE FROM jobs WHERE type = 'audience_destination_sync' AND workspace_id IN ('`+wsA+`','`+wsB+`')`)
	}
	clean()
	t.Cleanup(clean)
	return pool, ctx
}

func mustTx(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ws string, fn func(pgx.Tx) error) {
	t.Helper()
	if err := tenancy.InWorkspace(ctx, pool, ws, fn); err != nil {
		t.Fatal(err)
	}
}

type audSeed struct {
	ID, WS, Filters, Health, LastErr string
	Failures                         int
}

func mkAudience(t *testing.T, ctx context.Context, pool *pgxpool.Pool, a audSeed) {
	t.Helper()
	if a.WS == "" {
		a.WS = wsA
	}
	if a.ID == "" {
		a.ID = aud
	}
	if a.Filters == "" {
		a.Filters = `{}`
	}
	if a.Health == "" {
		a.Health = "unverified"
	}
	var le *string
	if a.LastErr != "" {
		le = &a.LastErr
	}
	mustTx(t, ctx, pool, a.WS, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO audiences (id, workspace_id, name, filters, refresh_health, last_refresh_error, consecutive_refresh_failures)
			VALUES ($1,$2,$1,$3::json,$4,$5,$6)`, a.ID, a.WS, a.Filters, a.Health, le, a.Failures)
		return err
	})
}

func seedMember(t *testing.T, ctx context.Context, pool *pgxpool.Pool, lead int, snap string, token *string) {
	t.Helper()
	mustTx(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO audience_members (workspace_id, audience_id, lead_id, snapshot, refresh_token, joined_at, last_seen_at)
			VALUES ($1,$2,$3,$4::json,$5,'2026-08-01','2026-08-01')`, wsA, aud, lead, snap, token)
		return err
	})
}

type rtErr struct{ msg string }

func (e *rtErr) Error() string  { return e.msg }
func (e *rtErr) PyType() string { return "RuntimeError" }

// scripted is the vectors' lead source: pages over leads with an optional total
// override and a failure on one page.
type scripted struct {
	mu       sync.Mutex
	leads    []json.RawMessage
	total    *int
	failAt   int
	failMsg  string
	calls    []call
	onCall   func(ctx context.Context, page int) error
	filtersS []map[string]any
}

type call struct {
	Filters  map[string]any
	Page     int
	PageSize int
}

func (s *scripted) source(ctx context.Context, _ pgx.Tx, filters map[string]any, page, size int) ([]json.RawMessage, int, error) {
	s.mu.Lock()
	s.calls = append(s.calls, call{filters, page, size})
	s.mu.Unlock()
	if s.onCall != nil {
		if err := s.onCall(ctx, page); err != nil {
			return nil, 0, err
		}
	}
	if s.failAt == page {
		return nil, 0, &rtErr{s.failMsg}
	}
	total := len(s.leads)
	if s.total != nil {
		total = *s.total
	}
	lo := min((page-1)*size, len(s.leads))
	hi := min(lo+size, len(s.leads))
	return s.leads[lo:hi], total, nil
}

func leadsOf(objs ...string) []json.RawMessage {
	out := make([]json.RawMessage, len(objs))
	for i, o := range objs {
		out[i] = json.RawMessage(o)
	}
	return out
}

func genLeads(n, idStart int) []json.RawMessage {
	out := make([]json.RawMessage, n)
	for i := range out {
		id := idStart + i
		out[i] = json.RawMessage(fmt.Sprintf(`{"id": %d, "company": "Co %d", "score": %d}`, id, id, id%100))
	}
	return out
}

type recEmitter struct {
	mu    sync.Mutex
	pages [][]audiencerefresh.MembershipEvent
	err   error
	hook  func(ctx context.Context, evs []audiencerefresh.MembershipEvent)
}

func (r *recEmitter) AudienceEvents(ctx context.Context, tx pgx.Tx, ws string, evs []audiencerefresh.MembershipEvent) (int, error) {
	if r.hook != nil {
		r.hook(ctx, evs)
	}
	r.mu.Lock()
	r.pages = append(r.pages, append([]audiencerefresh.MembershipEvent(nil), evs...))
	r.mu.Unlock()
	return len(evs), r.err
}

type harness struct {
	em       *recEmitter
	executor []string
	ctr      atomic.Int64
	now      time.Time
	pageSize int
	src      *scripted
}

func (h *harness) deps() audiencerefresh.Deps {
	d := audiencerefresh.Deps{
		AudienceEvents: h.em,
		ExecutorFor:    func(destType string) string { h.executor = append(h.executor, destType); return "go" },
		PageSize:       h.pageSize,
		NewID:          func() string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", h.ctr.Add(1)) },
	}
	if !h.now.IsZero() {
		d.Now = func() time.Time { return h.now }
	}
	if h.src != nil {
		d.Leads = h.src.source
	}
	return d
}

func newHarness(src *scripted, pageSize int) *harness {
	return &harness{em: &recEmitter{}, src: src, pageSize: pageSize, now: time.Date(2026, 8, 29, 10, 40, 0, 0, time.UTC)}
}

type memberRow struct {
	ID, Lead         int
	Snap             json.RawMessage
	Joined, LastSeen string
	Token            *string
}

type eventRow struct {
	ID, Lead  int
	Type      string
	Snap      json.RawMessage
	RefreshID *string
}

const isoFmt = `YYYY-MM-DD"T"HH24:MI:SS`

func readMembers(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ws, audience string) []memberRow {
	t.Helper()
	var out []memberRow
	mustTx(t, ctx, pool, ws, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, lead_id, snapshot::text, to_char(joined_at,'`+isoFmt+`'), to_char(last_seen_at,'`+isoFmt+`'), refresh_token
			FROM audience_members WHERE audience_id = $1 ORDER BY id`, audience)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m memberRow
			var s string
			if err := rows.Scan(&m.ID, &m.Lead, &s, &m.Joined, &m.LastSeen, &m.Token); err != nil {
				return err
			}
			m.Snap = json.RawMessage(s)
			out = append(out, m)
		}
		return rows.Err()
	})
	return out
}

func readEvents(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ws, audience string) []eventRow {
	t.Helper()
	var out []eventRow
	mustTx(t, ctx, pool, ws, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, lead_id, event_type, snapshot::text, refresh_id
			FROM audience_membership_events WHERE audience_id = $1 ORDER BY id`, audience)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e eventRow
			var s string
			if err := rows.Scan(&e.ID, &e.Lead, &e.Type, &s, &e.RefreshID); err != nil {
				return err
			}
			e.Snap = json.RawMessage(s)
			out = append(out, e)
		}
		return rows.Err()
	})
	return out
}

type audState struct {
	MemberCount int
	Refreshed   *string
	Health      string
	LastErr     *string
	Failures    int
	Next        *string
}

func readAud(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ws, id string) audState {
	t.Helper()
	var a audState
	mustTx(t, ctx, pool, ws, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT member_count, to_char(refreshed_at,'`+isoFmt+`'), refresh_health, last_refresh_error,
			consecutive_refresh_failures, to_char(next_refresh_at,'`+isoFmt+`') FROM audiences WHERE id = $1`, id).
			Scan(&a.MemberCount, &a.Refreshed, &a.Health, &a.LastErr, &a.Failures, &a.Next)
	})
	return a
}

func jsonEq(t *testing.T, what string, got, want json.RawMessage) {
	t.Helper()
	var g, w any
	dg := json.NewDecoder(bytes.NewReader(got))
	dg.UseNumber()
	dw := json.NewDecoder(bytes.NewReader(want))
	dw.UseNumber()
	if err := dg.Decode(&g); err != nil {
		t.Fatalf("%s: got %s: %v", what, got, err)
	}
	if err := dw.Decode(&w); err != nil {
		t.Fatalf("%s: want %s: %v", what, want, err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("%s = %s, want %s", what, got, want)
	}
}

func clearRuns(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	mustTx(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM destination_runs`)
		return err
	})
	_, _ = pool.Exec(ctx, `DELETE FROM jobs WHERE type = 'audience_destination_sync' AND workspace_id = '`+wsA+`'`)
}

func mkDest(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	mustTx(t, ctx, pool, wsA, func(tx pgx.Tx) error {
		_, err := deststore.CreateDestination(ctx, tx, deststore.CreateInput{
			AudienceID: aud, Name: "hook", Type: "webhook", Enabled: true,
			Config: json.RawMessage(`{"url":"https://hooks.example.test/x","method":"POST"}`), FieldMap: json.RawMessage(`{}`),
		})
		return err
	})
}

// ---- vectors ----

type seqFile struct {
	Scenarios []scenario `json:"scenarios"`
}

type scenario struct {
	Name     string `json:"name"`
	Audience struct {
		ID               string          `json:"id"`
		Name             string          `json:"name"`
		Filters          json.RawMessage `json:"filters"`
		Health           string          `json:"refresh_health"`
		Failures         int             `json:"consecutive_refresh_failures"`
		LastRefreshError *string         `json:"last_refresh_error"`
	} `json:"audience"`
	Existing []struct {
		LeadID   int             `json:"lead_id"`
		Snapshot json.RawMessage `json:"snapshot"`
		Token    *string         `json:"refresh_token"`
	} `json:"existing_members"`
	PageSize int    `json:"page_size"`
	Steps    []step `json:"steps"`
}

type summary struct {
	Count int               `json:"count"`
	First []json.RawMessage `json:"first"`
	Last  []json.RawMessage `json:"last"`
}

type step struct {
	Now   string `json:"now"`
	Op    string `json:"op"`
	Store struct {
		Total      *int   `json:"total"`
		Fail       string `json:"fail"`
		FailAtPage int    `json:"fail_at_page"`
	} `json:"store"`
	LeadsSpec *struct {
		Count    int    `json:"count"`
		IDStart  int    `json:"id_start"`
		Suffix   string `json:"suffix"`
		ScoreMod *int   `json:"score_mod"`
	} `json:"leads_spec"`
	Leads      []json.RawMessage `json:"leads"`
	StoreCalls []struct {
		Filters  map[string]any `json:"filters"`
		Page     int            `json:"page"`
		PageSize int            `json:"page_size"`
		Closed   bool           `json:"closed"`
	} `json:"store_calls"`
	Result *struct {
		Audience  map[string]any `json:"audience"`
		Entered   int            `json:"entered"`
		Exited    int            `json:"exited"`
		Unchanged int            `json:"unchanged"`
		Changed   int            `json:"changed"`
	} `json:"result"`
	Raises *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"raises"`
	AudienceAfter struct {
		MemberCount int     `json:"member_count"`
		RefreshedAt *string `json:"refreshed_at"`
		Health      string  `json:"refresh_health"`
		LastError   *string `json:"last_refresh_error"`
		Failures    int     `json:"consecutive_refresh_failures"`
		NextRefresh *string `json:"next_refresh_at"`
	} `json:"audience_after"`
	Enqueued []json.RawMessage `json:"enqueued_syncs"`
	EmitPage [][]int           `json:"emit_pages"`
	Members  []struct {
		LeadID   int             `json:"lead_id"`
		Snapshot json.RawMessage `json:"snapshot"`
		Joined   string          `json:"joined_at"`
		LastSeen string          `json:"last_seen_at"`
		Token    *string         `json:"refresh_token"`
	} `json:"members_after"`
	Events []struct {
		ID        int             `json:"id"`
		LeadID    int             `json:"lead_id"`
		Type      string          `json:"event_type"`
		Snapshot  json.RawMessage `json:"snapshot"`
		RefreshID string          `json:"refresh_id"`
	} `json:"events_created"`
	MembersSummary *summary `json:"members_summary"`
	EventsSummary  *summary `json:"events_summary"`
}

func specLeads(n, start int, suffix string, mod int) []json.RawMessage {
	out := make([]json.RawMessage, n)
	for i := range out {
		id := start + i
		out[i] = json.RawMessage(fmt.Sprintf(`{"id": %d, "company": "Company %d", "score": %d, "tag": %q}`, id, id, id%mod, suffix))
	}
	return out
}

func TestRefreshMatchesPythonVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/refresh_sequences.json")
	if err != nil {
		t.Fatal(err)
	}
	var f seqFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Scenarios) < 20 {
		t.Fatalf("only %d scenarios", len(f.Scenarios))
	}
	for _, sc := range f.Scenarios {
		t.Run(sc.Name, func(t *testing.T) {
			pool, ctx := setup(t)
			mkAudience(t, ctx, pool, audSeed{Filters: string(sc.Audience.Filters), Health: sc.Audience.Health,
				Failures: sc.Audience.Failures, LastErr: derefStr(sc.Audience.LastRefreshError)})
			for _, m := range sc.Existing {
				seedMember(t, ctx, pool, m.LeadID, string(m.Snapshot), m.Token)
			}
			mkDest(t, ctx, pool)
			h := newHarness(nil, sc.PageSize)
			for i, st := range sc.Steps {
				clearRuns(t, ctx, pool)
				h.em.pages, h.executor = nil, nil
				now, err := time.Parse(time.RFC3339, st.Now)
				if err != nil {
					t.Fatal(err)
				}
				h.now = now
				src := &scripted{leads: st.Leads, total: st.Store.Total, failAt: st.Store.FailAtPage, failMsg: st.Store.Fail}
				if st.LeadsSpec != nil {
					mod := 100
					if st.LeadsSpec.ScoreMod != nil {
						mod = *st.LeadsSpec.ScoreMod
					}
					src.leads = specLeads(st.LeadsSpec.Count, st.LeadsSpec.IDStart, st.LeadsSpec.Suffix, mod)
				}
				h.src = src
				run := audiencerefresh.RefreshObserved
				if st.Op == "refresh" {
					run = audiencerefresh.Refresh
				}
				res, err := run(ctx, pool, wsA, aud, h.deps())
				label := fmt.Sprintf("step %d", i)
				var calls []string
				for _, c := range src.calls {
					calls = append(calls, fmt.Sprintf("%v/%d/%d", c.Filters, c.Page, c.PageSize))
				}
				var wantCalls []string
				for _, c := range st.StoreCalls {
					if !c.Closed {
						wantCalls = append(wantCalls, fmt.Sprintf("%v/%d/%d", c.Filters, c.Page, c.PageSize))
					}
				}
				if !reflect.DeepEqual(calls, wantCalls) {
					t.Errorf("%s store calls = %v, want %v", label, calls, wantCalls)
				}
				if st.Raises != nil {
					var py audiencerefresh.PyTyper
					if err == nil {
						t.Fatalf("%s: want error %q", label, st.Raises.Message)
					}
					if !errors.As(err, &py) || py.PyType() != st.Raises.Type || py.(error).Error() != st.Raises.Message {
						t.Errorf("%s error = %v", label, err)
					}
				} else if err != nil {
					t.Fatalf("%s: %v", label, err)
				} else {
					want := st.Result
					if res.Entered != want.Entered || res.Exited != want.Exited || res.Unchanged != want.Unchanged || res.Changed != want.Changed {
						t.Errorf("%s counts = %+v, want %+v", label, res, want)
					}
					b, _ := json.Marshal(res.Audience)
					var got map[string]any
					_ = json.Unmarshal(b, &got)
					delete(got, "created_at")
					delete(got, "updated_at")
					wantA := want.Audience
					if !reflect.DeepEqual(got, wantA) {
						t.Errorf("%s audience = %v\nwant %v", label, got, wantA)
					}
				}
				a := readAud(t, ctx, pool, wsA, aud)
				w := st.AudienceAfter
				if a.MemberCount != w.MemberCount || !eqStr(a.Refreshed, w.RefreshedAt) || a.Health != w.Health ||
					!eqStr(a.LastErr, w.LastError) || a.Failures != w.Failures || !eqStr(a.Next, w.NextRefresh) {
					t.Errorf("%s audience row = %+v, want %+v", label, a, w)
				}
				if len(h.executor) != len(st.Enqueued) {
					t.Errorf("%s enqueue calls = %d, want %d", label, len(h.executor), len(st.Enqueued))
				}
				members := readMembers(t, ctx, pool, wsA, aud)
				if st.MembersSummary != nil {
					checkSummary(t, label+" members", len(members), st.MembersSummary, func(i int) json.RawMessage {
						m := members[i]
						b, _ := json.Marshal(map[string]any{"lead_id": m.Lead, "snapshot": m.Snap, "joined_at": m.Joined, "last_seen_at": m.LastSeen, "refresh_token": m.Token})
						return b
					}, "lead_id", "snapshot", "joined_at", "last_seen_at", "refresh_token")
				} else {
					if len(members) != len(st.Members) {
						t.Fatalf("%s members = %d, want %d", label, len(members), len(st.Members))
					}
					for j, wm := range st.Members {
						m := members[j]
						if m.Lead != wm.LeadID || m.Joined != wm.Joined || m.LastSeen != wm.LastSeen || !eqStr(m.Token, wm.Token) {
							t.Errorf("%s member %d = %+v, want %+v", label, j, m, wm)
						}
						jsonEq(t, label+" member snapshot", m.Snap, wm.Snapshot)
					}
				}
				// events of this attempt: by refresh id, id order
				token := fmt.Sprintf("00000000-0000-4000-8000-%012d", h.ctr.Load())
				var evs []eventRow
				for _, e := range readEvents(t, ctx, pool, wsA, aud) {
					if e.RefreshID != nil && *e.RefreshID == token {
						evs = append(evs, e)
					}
				}
				if st.EventsSummary != nil {
					checkSummary(t, label+" events", len(evs), st.EventsSummary, func(i int) json.RawMessage {
						e := evs[i]
						b, _ := json.Marshal(map[string]any{"lead_id": e.Lead, "event_type": e.Type, "snapshot": e.Snap, "refresh_id": e.RefreshID})
						return b
					}, "lead_id", "event_type", "snapshot", "refresh_id")
				} else {
					if len(evs) != len(st.Events) {
						t.Fatalf("%s events = %d, want %d", label, len(evs), len(st.Events))
					}
					for j, we := range st.Events {
						e := evs[j]
						if e.Lead != we.LeadID || e.Type != we.Type || *e.RefreshID != we.RefreshID {
							t.Errorf("%s event %d = %+v, want %+v", label, j, e, we)
						}
						jsonEq(t, label+" event snapshot", e.Snap, we.Snapshot)
					}
				}
				// emit pages: sizes and ids map onto this attempt's events in order
				var gotPages [][]int64
				for _, p := range h.em.pages {
					var ids []int64
					for _, e := range p {
						ids = append(ids, e.ID)
					}
					gotPages = append(gotPages, ids)
				}
				if len(gotPages) != len(st.EmitPage) {
					t.Fatalf("%s emit pages = %d, want %d", label, len(gotPages), len(st.EmitPage))
				}
				off := 0
				for pi, wp := range st.EmitPage {
					if len(gotPages[pi]) != len(wp) {
						t.Fatalf("%s emit page %d size = %d, want %d", label, pi, len(gotPages[pi]), len(wp))
					}
					for k := range wp {
						if gotPages[pi][k] != int64(evs[off+k].ID) {
							t.Errorf("%s emit page %d[%d] = %d, want event %d", label, pi, k, gotPages[pi][k], evs[off+k].ID)
						}
					}
					off += len(wp)
				}
			}
		})
	}
}

func checkSummary(t *testing.T, what string, n int, s *summary, row func(int) json.RawMessage, _ ...string) {
	t.Helper()
	if n != s.Count {
		t.Errorf("%s count = %d, want %d", what, n, s.Count)
		return
	}
	for i, w := range s.First {
		cmpRow(t, fmt.Sprintf("%s first[%d]", what, i), row(i), w)
	}
	for i, w := range s.Last {
		cmpRow(t, fmt.Sprintf("%s last[%d]", what, i), row(n-len(s.Last)+i), w)
	}
}

// cmpRow compares the DB-derived row to a fixture row, ignoring the fixture's
// autoincrement id.
func cmpRow(t *testing.T, what string, got, want json.RawMessage) {
	t.Helper()
	var w map[string]json.RawMessage
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatal(err)
	}
	delete(w, "id")
	wb, _ := json.Marshal(w)
	jsonEq(t, what, got, wb)
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func eqStr(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// ---- behaviour ----

func TestRefreshConcurrentSerialized(t *testing.T) {
	pool, ctx := setup(t)
	mkAudience(t, ctx, pool, audSeed{})
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	var firstCalls, secondCalls atomic.Int64
	srcA := &scripted{leads: genLeads(5, 1)}
	srcA.onCall = func(ctx context.Context, page int) error {
		if firstCalls.Add(1) == 1 {
			entered <- struct{}{}
			<-release
		}
		return nil
	}
	srcB := &scripted{leads: genLeads(5, 1)}
	srcB.onCall = func(context.Context, int) error { secondCalls.Add(1); return nil }
	hA, hB := newHarness(srcA, 3), newHarness(srcB, 3)

	var wg sync.WaitGroup
	var resA, resB audiencerefresh.Result
	var errA, errB error
	wg.Add(1)
	go func() { defer wg.Done(); resA, errA = audiencerefresh.Refresh(ctx, pool, wsA, aud, hA.deps()) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first refresh never reached the lead source")
	}
	wg.Add(1)
	go func() { defer wg.Done(); resB, errB = audiencerefresh.Refresh(ctx, pool, wsA, aud, hB.deps()) }()
	time.Sleep(400 * time.Millisecond)
	if secondCalls.Load() != 0 {
		t.Fatal("second refresh ran while the first held the audience lock")
	}
	close(release)
	wg.Wait()
	if errA != nil || errB != nil {
		t.Fatalf("errors: %v / %v", errA, errB)
	}
	if resA.Entered != 5 || resB.Entered != 0 || resB.Unchanged != 5 || resB.Exited != 0 {
		t.Errorf("results A=%+v B=%+v: second must see the first's committed state", resA, resB)
	}
	if n := len(readMembers(t, ctx, pool, wsA, aud)); n != 5 {
		t.Errorf("members = %d", n)
	}
	if n := len(readEvents(t, ctx, pool, wsA, aud)); n != 5 {
		t.Errorf("events = %d (want one entered per lead, as one sequential run)", n)
	}
}

func TestExitsOnlyForUnseenMembers(t *testing.T) {
	pool, ctx := setup(t)
	mkAudience(t, ctx, pool, audSeed{})
	old := "old"
	for i := 1; i <= 5; i++ {
		seedMember(t, ctx, pool, i, fmt.Sprintf(`{"id": %d, "company": "Co %d", "score": %d}`, i, i, i), &old)
	}
	seedMember(t, ctx, pool, 6, `{"id": 6}`, nil)
	h := newHarness(&scripted{leads: genLeads(2, 2)}, 2) // leads 2 and 3 remain
	res, err := audiencerefresh.Refresh(ctx, pool, wsA, aud, h.deps())
	if err != nil {
		t.Fatal(err)
	}
	if res.Exited != 4 || res.Entered != 0 {
		t.Fatalf("result = %+v", res)
	}
	var left []int
	for _, m := range readMembers(t, ctx, pool, wsA, aud) {
		left = append(left, m.Lead)
	}
	if !reflect.DeepEqual(left, []int{2, 3}) {
		t.Errorf("remaining members = %v", left)
	}
	var exited []int
	for _, e := range readEvents(t, ctx, pool, wsA, aud) {
		if e.Type != "exited" {
			t.Errorf("event %+v", e)
		}
		exited = append(exited, e.Lead)
	}
	if !reflect.DeepEqual(exited, []int{1, 4, 5, 6}) {
		t.Errorf("exited events = %v (member id order)", exited)
	}
	if a := readAud(t, ctx, pool, wsA, aud); a.MemberCount != 2 {
		t.Errorf("member_count = %d", a.MemberCount)
	}
}

func TestUnchangedSnapshotNotCountedChanged(t *testing.T) {
	pool, ctx := setup(t)
	mkAudience(t, ctx, pool, audSeed{})
	old := "old"
	// stored: different key order and 1 vs 1.0 must still be equal (Python ==).
	seedMember(t, ctx, pool, 1, `{"score": 10.0, "company": "Co 1", "id": 1, "n": {"a": 1, "b": [1, 2.5]}}`, &old)
	seedMember(t, ctx, pool, 2, `{"id": 2, "company": "Co 2", "score": 20}`, &old)
	seedMember(t, ctx, pool, 3, `{}`, &old)
	h := newHarness(&scripted{leads: leadsOf(
		`{"id": 1, "company": "Co 1", "score": 10, "n": {"b": [1.0, 2.5], "a": 1}}`,
		`{"id": 2, "company": "Co 2", "score": 21}`,
		`{"id": 3}`,
	)}, 0)
	res, err := audiencerefresh.Refresh(ctx, pool, wsA, aud, h.deps())
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed != 2 || res.Unchanged != 1 || res.Entered != 0 || res.Exited != 0 {
		t.Fatalf("result = %+v (lead 1 equal, lead 2 and lead 3 {} vs {\"id\":3} changed)", res)
	}
	for _, m := range readMembers(t, ctx, pool, wsA, aud) {
		if m.Token == nil || *m.Token == "old" || m.LastSeen != "2026-08-29T10:40:00" {
			t.Errorf("member %d not refreshed: %+v", m.Lead, m)
		}
	}
}

func TestEventsPersistedWithRefreshID(t *testing.T) {
	pool, ctx := setup(t)
	mkAudience(t, ctx, pool, audSeed{})
	old := "old"
	seedMember(t, ctx, pool, 9, `{"id": 9, "company": "Gone"}`, &old)
	h := newHarness(&scripted{leads: genLeads(2, 1)}, 0)
	if _, err := audiencerefresh.Refresh(ctx, pool, wsA, aud, h.deps()); err != nil {
		t.Fatal(err)
	}
	evs := readEvents(t, ctx, pool, wsA, aud)
	if len(evs) != 3 {
		t.Fatalf("events = %+v", evs)
	}
	rid := "00000000-0000-4000-8000-000000000001"
	want := []struct {
		lead int
		typ  string
		snap string
	}{
		{1, "entered", `{"id": 1, "company": "Co 1", "score": 1}`},
		{2, "entered", `{"id": 2, "company": "Co 2", "score": 2}`},
		{9, "exited", `{"id": 9, "company": "Gone"}`},
	}
	for i, w := range want {
		e := evs[i]
		if e.Lead != w.lead || e.Type != w.typ || e.RefreshID == nil || *e.RefreshID != rid {
			t.Errorf("event %d = %+v", i, e)
		}
		jsonEq(t, "event snapshot", e.Snap, json.RawMessage(w.snap))
	}
	// key order of the stored snapshot is the source's (json, not jsonb)
	if !strings.HasPrefix(string(evs[0].Snap), `{"id"`) || strings.Index(string(evs[0].Snap), `"company"`) > strings.Index(string(evs[0].Snap), `"score"`) {
		t.Errorf("snapshot key order lost: %s", evs[0].Snap)
	}
}

func TestEventsEmittedAfterCommitInPages(t *testing.T) {
	pool, ctx := setup(t)
	mkAudience(t, ctx, pool, audSeed{})
	h := newHarness(&scripted{leads: genLeads(8, 1)}, 3)
	h.em.hook = func(ctx context.Context, evs []audiencerefresh.MembershipEvent) {
		// the diff is already committed and visible to another connection
		if n := len(readMembers(t, ctx, pool, wsA, aud)); n != 8 {
			t.Errorf("members visible during emit = %d, want 8 (emit after commit)", n)
		}
		if a := readAud(t, ctx, pool, wsA, aud); a.MemberCount != 8 {
			t.Errorf("member_count during emit = %d", a.MemberCount)
		}
	}
	if _, err := audiencerefresh.Refresh(ctx, pool, wsA, aud, h.deps()); err != nil {
		t.Fatal(err)
	}
	var sizes []int
	var last int64
	for _, p := range h.em.pages {
		sizes = append(sizes, len(p))
		for _, e := range p {
			if e.ID <= last || e.Type != "entered" || e.AudienceID != aud {
				t.Errorf("event %+v after %d", e, last)
			}
			last = e.ID
		}
	}
	if !reflect.DeepEqual(sizes, []int{3, 3, 2}) {
		t.Errorf("emit page sizes = %v", sizes)
	}
}

func TestEmitterFailureDoesNotUndoDiff(t *testing.T) {
	pool, ctx := setup(t)
	mkAudience(t, ctx, pool, audSeed{})
	mkDest(t, ctx, pool)
	h := newHarness(&scripted{leads: genLeads(4, 1)}, 0)
	h.em.err = errors.New("emit exploded")
	_, err := audiencerefresh.Refresh(ctx, pool, wsA, aud, h.deps())
	if err == nil || !strings.Contains(err.Error(), "emit exploded") {
		t.Fatalf("err = %v", err)
	}
	if n := len(readMembers(t, ctx, pool, wsA, aud)); n != 4 {
		t.Errorf("members = %d: committed diff must stay", n)
	}
	if n := len(readEvents(t, ctx, pool, wsA, aud)); n != 4 {
		t.Errorf("events = %d", n)
	}
	if a := readAud(t, ctx, pool, wsA, aud); a.MemberCount != 4 || a.Refreshed == nil {
		t.Errorf("audience = %+v", a)
	}
	if len(h.executor) != 0 {
		t.Error("syncs enqueued after a failed emit (Python raises before enqueue)")
	}
}

func TestEnqueueSyncsOnlyOnChange(t *testing.T) {
	pool, ctx := setup(t)
	mkAudience(t, ctx, pool, audSeed{})
	mkDest(t, ctx, pool)
	src := &scripted{leads: genLeads(3, 1)}
	h := newHarness(src, 0)
	runs := func() int {
		n := 0
		mustTx(t, ctx, pool, wsA, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM destination_runs`).Scan(&n)
		})
		return n
	}
	step := func(want int, what string) {
		t.Helper()
		clearRuns(t, ctx, pool)
		if _, err := audiencerefresh.Refresh(ctx, pool, wsA, aud, h.deps()); err != nil {
			t.Fatal(err)
		}
		if got := runs(); got != want {
			t.Errorf("%s: runs = %d, want %d", what, got, want)
		}
	}
	step(1, "first refresh enters members")
	step(0, "identical refresh")
	src.leads = leadsOf(`{"id": 1, "company": "Co 1", "score": 1}`, `{"id": 2, "company": "Co 2", "score": 2}`, `{"id": 3, "company": "Renamed", "score": 3}`)
	step(1, "snapshot change only")
	src.leads = src.leads[:2]
	step(1, "exit only")
	if !reflect.DeepEqual(h.executor, []string{"webhook", "webhook", "webhook"}) {
		t.Errorf("executorFor calls = %v (cohort executor asked once per started sync)", h.executor)
	}
	var executor string
	if err := pool.QueryRow(ctx, `SELECT executor FROM jobs WHERE type = 'audience_destination_sync' AND workspace_id = $1 ORDER BY id DESC LIMIT 1`, wsA).Scan(&executor); err != nil || executor != "go" {
		t.Errorf("job executor = %q, %v", executor, err)
	}
}

func TestHealthyAndDegradedTransitions(t *testing.T) {
	pool, ctx := setup(t)
	mkAudience(t, ctx, pool, audSeed{})
	src := &scripted{leads: genLeads(2, 1), failAt: 1, failMsg: "boom"}
	h := newHarness(src, 0)
	for i := 1; i <= 2; i++ {
		_, err := audiencerefresh.RefreshObserved(ctx, pool, wsA, aud, h.deps())
		if err == nil {
			t.Fatal("want error")
		}
		a := readAud(t, ctx, pool, wsA, aud)
		if a.Health != "degraded" || a.Failures != i || a.LastErr == nil || *a.LastErr != "RuntimeError: boom" {
			t.Errorf("after failure %d: %+v", i, a)
		}
	}
	src.failAt = 0
	res, err := audiencerefresh.RefreshObserved(ctx, pool, wsA, aud, h.deps())
	if err != nil {
		t.Fatal(err)
	}
	a := readAud(t, ctx, pool, wsA, aud)
	if a.Health != "healthy" || a.Failures != 0 || a.LastErr != nil || a.MemberCount != 2 {
		t.Errorf("after recovery: %+v", a)
	}
	if res.Audience.RefreshHealth != "healthy" || res.Audience.LastRefreshError != nil || res.Audience.MemberCount != 2 {
		t.Errorf("result audience = %+v", res.Audience)
	}
	// 1000-character cap counts characters, not bytes
	src.failAt, src.failMsg = 1, strings.Repeat("é", 1200)
	if _, err := audiencerefresh.RefreshObserved(ctx, pool, wsA, aud, h.deps()); err == nil {
		t.Fatal("want error")
	}
	a = readAud(t, ctx, pool, wsA, aud)
	if a.LastErr == nil || len([]rune(*a.LastErr)) != 1000 || !strings.HasPrefix(*a.LastErr, "RuntimeError: éé") {
		t.Errorf("last error = %d runes", len([]rune(derefStr(a.LastErr))))
	}
}

func TestFailureRollsBackDiff(t *testing.T) {
	pool, ctx := setup(t)
	mkAudience(t, ctx, pool, audSeed{})
	old := "old"
	seedMember(t, ctx, pool, 50, `{"id": 50}`, &old)
	src := &scripted{leads: genLeads(6, 1), failAt: 2, failMsg: "page two failed"}
	h := newHarness(src, 3)
	if _, err := audiencerefresh.Refresh(ctx, pool, wsA, aud, h.deps()); err == nil {
		t.Fatal("want error")
	}
	if m := readMembers(t, ctx, pool, wsA, aud); len(m) != 1 || m[0].Lead != 50 || *m[0].Token != "old" {
		t.Errorf("members = %+v", m)
	}
	if e := readEvents(t, ctx, pool, wsA, aud); len(e) != 0 {
		t.Errorf("events = %+v", e)
	}
	if a := readAud(t, ctx, pool, wsA, aud); a.MemberCount != 0 || a.Refreshed != nil || a.Health != "unverified" || a.Failures != 0 {
		t.Errorf("raw refresh touched the audience: %+v", a)
	}
	if len(h.em.pages) != 0 || len(h.executor) != 0 {
		t.Error("emitted or enqueued after failure")
	}
}

func TestContextCancelReleasesLock(t *testing.T) {
	pool, ctx := setup(t)
	mkAudience(t, ctx, pool, audSeed{})
	cctx, cancel := context.WithCancel(ctx)
	in := make(chan struct{})
	src := &scripted{leads: genLeads(3, 1)}
	src.onCall = func(c context.Context, page int) error {
		select {
		case <-in:
		default:
			close(in)
		}
		<-c.Done()
		return c.Err()
	}
	h := newHarness(src, 0)
	done := make(chan error, 1)
	go func() { _, err := audiencerefresh.Refresh(cctx, pool, wsA, aud, h.deps()); done <- err }()
	select {
	case <-in:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh never reached the lead source")
	}
	// lock is held now
	lockErr := tenancy.InWorkspace(ctx, pool, wsA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT 1 FROM audiences WHERE id = $1 FOR UPDATE NOWAIT`, aud)
		return err
	})
	if lockErr == nil {
		t.Fatal("audience row was not locked during refresh")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("refresh did not return after cancel")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := tenancy.InWorkspace(ctx, pool, wsA, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `SELECT 1 FROM audiences WHERE id = $1 FOR UPDATE NOWAIT`, aud)
			return err
		})
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("lock still held after cancel: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// the Timeout dep bounds the call the same way
	src.onCall = func(c context.Context, page int) error { <-c.Done(); return c.Err() }
	d := h.deps()
	d.Timeout = 200 * time.Millisecond
	if _, err := audiencerefresh.Refresh(ctx, pool, wsA, aud, d); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("timeout err = %v", err)
	}
}

func seedLead(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ws, company, city string, score int) {
	t.Helper()
	mustTx(t, ctx, pool, ws, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO leads (workspace_id, company, city, score) VALUES ($1,$2,$3,$4)`, ws, company, city, score)
		return err
	})
}

func TestEmptyFiltersMatchAllLeads(t *testing.T) {
	pool, ctx := setup(t)
	mkAudience(t, ctx, pool, audSeed{})
	for i := 0; i < 7; i++ {
		seedLead(t, ctx, pool, wsA, fmt.Sprintf("Co %d", i), []string{"Pune", "Oslo"}[i%2], i)
	}
	h := newHarness(nil, 3) // real source, pages of 3
	res, err := audiencerefresh.Refresh(ctx, pool, wsA, aud, h.deps())
	if err != nil {
		t.Fatal(err)
	}
	if res.Entered != 7 || res.Audience.MemberCount != 7 {
		t.Fatalf("result = %+v", res)
	}
	ms := readMembers(t, ctx, pool, wsA, aud)
	if len(ms) != 7 {
		t.Fatalf("members = %d", len(ms))
	}
	var snap map[string]any
	if err := json.Unmarshal(ms[0].Snap, &snap); err != nil {
		t.Fatal(err)
	}
	if _, ok := snap["company"]; !ok || snap["id"] == nil || snap["workspace_id"] != wsA {
		t.Errorf("snapshot is not the lead row: %v", snap)
	}
	// a filtered audience through the real source
	mkAudience(t, ctx, pool, audSeed{ID: aud + "-pune", Filters: `{"city": "Pune"}`})
	res, err = audiencerefresh.Refresh(ctx, pool, wsA, aud+"-pune", h.deps())
	if err != nil || res.Entered != 4 {
		t.Fatalf("filtered = %+v, %v", res, err)
	}
}

func TestTenantIsolation(t *testing.T) {
	pool, ctx := setup(t)
	mkAudience(t, ctx, pool, audSeed{})
	mkAudience(t, ctx, pool, audSeed{ID: aud + "-b", WS: wsB})
	seedLead(t, ctx, pool, wsA, "A1", "Pune", 1)
	seedLead(t, ctx, pool, wsB, "B1", "Pune", 1)
	seedLead(t, ctx, pool, wsB, "B2", "Pune", 2)
	h := newHarness(nil, 0)
	if _, err := audiencerefresh.Refresh(ctx, pool, wsB, aud, h.deps()); !errors.Is(err, audiencerefresh.ErrNotFound) {
		t.Errorf("other tenant's audience: err = %v, want ErrNotFound", err)
	}
	if _, err := audiencerefresh.Refresh(ctx, pool, wsA, aud+"-b", h.deps()); !errors.Is(err, audiencerefresh.ErrNotFound) {
		t.Errorf("foreign audience id: err = %v, want ErrNotFound", err)
	}
	if n := len(readMembers(t, ctx, pool, wsA, aud)) + len(readMembers(t, ctx, pool, wsB, aud+"-b")); n != 0 {
		t.Errorf("failed lookups created %d members", n)
	}
	resA, err := audiencerefresh.Refresh(ctx, pool, wsA, aud, h.deps())
	if err != nil || resA.Entered != 1 {
		t.Fatalf("A = %+v, %v", resA, err)
	}
	resB, err := audiencerefresh.Refresh(ctx, pool, wsB, aud+"-b", h.deps())
	if err != nil || resB.Entered != 2 {
		t.Fatalf("B = %+v, %v", resB, err)
	}
	if ms := readMembers(t, ctx, pool, wsA, aud); len(ms) != 1 || !strings.Contains(string(ms[0].Snap), `"A1"`) {
		t.Errorf("A members = %+v", ms)
	}
	if _, err := audiencerefresh.Refresh(ctx, pool, "", aud, h.deps()); err == nil {
		t.Error("empty workspace must be refused")
	}
}
