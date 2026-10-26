//go:build integration

package outreachstore_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachstore"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

// ---------------------------------------------------------------- helpers

func supNewWS(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	ws := "p09t14-" + hex.EncodeToString(b)
	t.Cleanup(func() {
		ctx := context.Background()
		_ = tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error {
			for _, q := range []string{"outreach_inbound_messages", "outreach_suppressions", "outreach_sends", "outreach_enrollments", "outreach_sequences"} {
				if _, err := tx.Exec(ctx, "DELETE FROM "+q+" WHERE workspace_id=$1", ws); err != nil {
					return err
				}
			}
			return nil
		})
		_, _ = pool.Exec(ctx, `DELETE FROM outreach_inbound_schedules WHERE workspace_id=$1`, ws)
	})
	return ws
}

func supTx(t *testing.T, pool *pgxpool.Pool, ws string, fn func(ctx context.Context, tx pgx.Tx)) {
	t.Helper()
	ctx := context.Background()
	if err := tenancy.InWorkspace(ctx, pool, ws, func(tx pgx.Tx) error { fn(ctx, tx); return nil }); err != nil {
		t.Fatalf("tx: %v", err)
	}
}

func supDefaults() outreachstore.Thresholds {
	return outreachstore.Thresholds{MinSends: 20, BounceRate: 0.05, ComplaintRate: 0.003, SoftBounceMax: 3}
}

// ------------------------------------------------------------ suppressions

func TestSuppressionMatchesGmailVariants(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	ws := supNewWS(t, pool)
	supTx(t, pool, ws, func(ctx context.Context, tx pgx.Tx) {
		if added, err := outreachstore.AddSuppression(ctx, tx, ws, "  John.Doe+promo@Gmail.com ", "manual", "", false); err != nil || !added {
			t.Fatalf("add: %v %v", added, err)
		}
		// stored canonical (lowercase) form
		l, _ := outreachstore.ListSuppressions(ctx, tx, 10)
		if len(l) != 1 || l[0].Email != "john.doe+promo@gmail.com" || l[0].Reason != "manual" || l[0].Locked {
			t.Fatalf("list: %+v", l)
		}
		for _, q := range []string{"john.doe+promo@gmail.com", "JOHN.DOE+PROMO@GMAIL.COM"} {
			if ok, _ := outreachstore.IsSuppressed(ctx, tx, ws, q); !ok {
				t.Errorf("exact %q not suppressed", q)
			}
		}
		// the collapsed key of a variant matches only a stored collapsed row
		if ok, _ := outreachstore.IsSuppressed(ctx, tx, ws, "johndoe@gmail.com"); ok {
			t.Errorf("collapsed form must not match a plus-address row")
		}
		if added, _ := outreachstore.AddSuppression(ctx, tx, ws, "johndoe@googlemail.com", "bounce", "imap", false); !added {
			t.Fatal("add collapsed")
		}
		for _, q := range []string{"j.o.h.n.d.o.e+x@googlemail.com", "johndoe+zzz@googlemail.com"} {
			if ok, _ := outreachstore.IsSuppressed(ctx, tx, ws, q); !ok {
				t.Errorf("variant %q should match collapsed row", q)
			}
		}
		if ok, _ := outreachstore.IsSuppressed(ctx, tx, ws, "johndoe@gmail.com"); ok {
			t.Errorf("googlemail row must not match gmail.com")
		}
		// non-gmail domains never collapse
		if ok, _ := outreachstore.IsSuppressed(ctx, tx, ws, "a.b@example.com"); ok {
			t.Errorf("unexpected")
		}
		if ok, _ := outreachstore.IsSuppressed(ctx, tx, ws, ""); ok {
			t.Errorf("empty is never suppressed")
		}
		if added, err := outreachstore.AddSuppression(ctx, tx, ws, "   ", "manual", "", false); err != nil || added {
			t.Errorf("blank email: added=%v err=%v", added, err)
		}
	})
}

func TestLockedSuppressionNotRemovable(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	ws := supNewWS(t, pool)
	supTx(t, pool, ws, func(ctx context.Context, tx pgx.Tx) {
		_, _ = outreachstore.AddSuppression(ctx, tx, ws, "lock@example.com", "complaint", "imap", true)
		_, _ = outreachstore.AddSuppression(ctx, tx, ws, "free@example.com", "manual", "", false)
		if o, err := outreachstore.RemoveSuppression(ctx, tx, ws, "LOCK@example.com"); err != nil || o != outreachstore.OutcomeLocked {
			t.Errorf("locked: %v %v", o, err)
		}
		if ok, _ := outreachstore.IsSuppressed(ctx, tx, ws, "lock@example.com"); !ok {
			t.Errorf("locked row vanished")
		}
		if o, _ := outreachstore.RemoveSuppression(ctx, tx, ws, "free@example.com"); o != outreachstore.OutcomeOK {
			t.Errorf("free: %v", o)
		}
		if o, _ := outreachstore.RemoveSuppression(ctx, tx, ws, "free@example.com"); o != outreachstore.OutcomeNotFound {
			t.Errorf("again: %v", o)
		}
		if o, _ := outreachstore.RemoveSuppression(ctx, tx, ws, ""); o != outreachstore.OutcomeNotFound {
			t.Errorf("empty: %v", o)
		}
		if string(outreachstore.OutcomeOK) != "ok" || string(outreachstore.OutcomeNotFound) != "not_found" || string(outreachstore.OutcomeLocked) != "locked" {
			t.Errorf("outcome strings")
		}
	})
}

func TestAddSuppressionDoesNotUnlock(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	ws := supNewWS(t, pool)
	supTx(t, pool, ws, func(ctx context.Context, tx pgx.Tx) {
		if added, _ := outreachstore.AddSuppression(ctx, tx, ws, "x@example.com", "unsubscribe", "link", true); !added {
			t.Fatal("first add")
		}
		// unlocked re-add must not downgrade, rewrite or report added
		if added, err := outreachstore.AddSuppression(ctx, tx, ws, "X@example.com", "manual", "api", false); err != nil || added {
			t.Fatalf("second add: %v %v", added, err)
		}
		l, _ := outreachstore.ListSuppressions(ctx, tx, 10)
		if len(l) != 1 || !l[0].Locked || l[0].Reason != "unsubscribe" || l[0].Source != "link" {
			t.Errorf("row changed: %+v", l)
		}
		// the tx is still usable after the conflict (no aborted transaction)
		if ok, err := outreachstore.IsSuppressed(ctx, tx, ws, "x@example.com"); err != nil || !ok {
			t.Errorf("tx after conflict: %v %v", ok, err)
		}
	})
}

func TestSuppressionsTenantIsolation(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	a, b := supNewWS(t, pool), supNewWS(t, pool)
	supTx(t, pool, a, func(ctx context.Context, tx pgx.Tx) {
		_, _ = outreachstore.AddSuppression(ctx, tx, a, "iso@example.com", "complaint", "", true)
		n, _ := outreachstore.RecordInboundProcessed(ctx, tx, outreachstore.InboundIn{WorkspaceID: a, IMAPUID: "1", UIDValidity: "1", SourceMessageID: "<same@x>", Kind: "hard"})
		if !n {
			t.Fatal("a ledger")
		}
	})
	supTx(t, pool, b, func(ctx context.Context, tx pgx.Tx) {
		if ok, _ := outreachstore.IsSuppressed(ctx, tx, a, "iso@example.com"); ok {
			t.Errorf("RLS leak: B sees A's suppression")
		}
		if ok, _ := outreachstore.IsSuppressed(ctx, tx, b, "iso@example.com"); ok {
			t.Errorf("B suppressed by A")
		}
		if l, _ := outreachstore.ListSuppressions(ctx, tx, 50); len(l) != 0 {
			t.Errorf("list leak: %d", len(l))
		}
		if o, _ := outreachstore.RemoveSuppression(ctx, tx, a, "iso@example.com"); o != outreachstore.OutcomeNotFound {
			t.Errorf("remove across tenants: %v", o)
		}
		// same email and same message id in another workspace are distinct rows
		if added, _ := outreachstore.AddSuppression(ctx, tx, b, "iso@example.com", "manual", "", false); !added {
			t.Errorf("b add")
		}
		if n, _ := outreachstore.InboundMessageCount(ctx, tx, a); n != 0 {
			t.Errorf("ledger count leak: %d", n)
		}
		n, _ := outreachstore.RecordInboundProcessed(ctx, tx, outreachstore.InboundIn{WorkspaceID: b, IMAPUID: "1", UIDValidity: "1", SourceMessageID: "<same@x>", Kind: "hard"})
		if !n {
			t.Errorf("same ids in another workspace must be new")
		}
	})
	supTx(t, pool, a, func(ctx context.Context, tx pgx.Tx) {
		l, _ := outreachstore.ListSuppressions(ctx, tx, 50)
		if len(l) != 1 || !l[0].Locked {
			t.Errorf("a row altered: %+v", l)
		}
		if n, _ := outreachstore.InboundMessageCount(ctx, tx, a); n != 1 {
			t.Errorf("a ledger count %d", n)
		}
	})
}

// --------------------------------------------------------- schedule mirror

func TestScheduleMirrorsNonRLS(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	ws := supNewWS(t, pool)
	ctx := context.Background()
	if s, err := outreachstore.GetInboundSchedule(ctx, pool, ws); err != nil || s != nil {
		t.Fatalf("absent: %+v %v", s, err)
	}
	next := time.Date(2026, 8, 29, 10, 40, 0, 0, time.UTC)
	fails, uv, lu := 2, "77", "9"
	if err := outreachstore.UpsertInboundSchedule(ctx, pool, ws, outreachstore.InboundScheduleUpsert{
		NextPollAt: &next, Enabled: true, ConsecutiveFailures: &fails, UIDValidity: &uv, LastUID: &lu,
	}); err != nil {
		t.Fatal(err)
	}
	s, err := outreachstore.GetInboundSchedule(ctx, pool, ws)
	if err != nil || s == nil || !s.Enabled || s.NextPollAt == nil || !s.NextPollAt.Equal(next) ||
		s.ConsecutiveFailures != 2 || s.UIDValidity != "77" || s.LastUID != "9" {
		t.Fatalf("get: %+v %v", s, err)
	}
	// unset optional fields keep their values; next_poll_at and enabled always overwrite
	if err := outreachstore.UpsertInboundSchedule(ctx, pool, ws, outreachstore.InboundScheduleUpsert{NextPollAt: nil, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	s, _ = outreachstore.GetInboundSchedule(ctx, pool, ws)
	if s.NextPollAt != nil || !s.Enabled || s.ConsecutiveFailures != 2 || s.UIDValidity != "77" || s.LastUID != "9" {
		t.Fatalf("partial upsert: %+v", s)
	}
	zero := 0
	_ = outreachstore.UpsertInboundSchedule(ctx, pool, ws, outreachstore.InboundScheduleUpsert{NextPollAt: &next, Enabled: true, ConsecutiveFailures: &zero})
	if s, _ = outreachstore.GetInboundSchedule(ctx, pool, ws); s.ConsecutiveFailures != 0 {
		t.Fatalf("explicit zero: %+v", s)
	}
	if err := outreachstore.DisableInboundSchedule(ctx, pool, ws); err != nil {
		t.Fatal(err)
	}
	s, _ = outreachstore.GetInboundSchedule(ctx, pool, ws)
	if s.Enabled || s.NextPollAt != nil || s.UIDValidity != "77" {
		t.Fatalf("disable: %+v", s)
	}
	// disabling an absent row is a no-op, and the table has no RLS (readable with no GUC)
	if err := outreachstore.DisableInboundSchedule(ctx, pool, "p09t14-absent"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outreach_inbound_schedules WHERE workspace_id=$1`, ws).Scan(&n); err != nil || n != 1 {
		t.Fatalf("no-GUC read: %d %v", n, err)
	}
}

// --------------------------------------------------------- vector harness

type supVec struct {
	Clock     string        `json:"clock"`
	Scenarios []supScenario `json:"scenarios"`
}

type supScenario struct {
	Name     string         `json:"name"`
	Settings map[string]any `json:"settings"`
	Setup    struct {
		Sequences    []map[string]any `json:"sequences"`
		Enrollments  []map[string]any `json:"enrollments"`
		Sends        []map[string]any `json:"sends"`
		Suppressions []map[string]any `json:"suppressions"`
	} `json:"setup"`
	Steps []struct {
		Op     map[string]any `json:"op"`
		Result any            `json:"result"`
		State  map[string]any `json:"state"`
	} `json:"steps"`
}

func supLoad(t *testing.T, name string) supVec {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var v supVec
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func supNum(v any) int { // JSON numbers decode as float64
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return 0
}

func supStr(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return fmt.Sprintf("%d", int(x))
	}
	return fmt.Sprint(v)
}

func supThresholds(s map[string]any) outreachstore.Thresholds {
	th := supDefaults()
	if v, ok := s["OUTREACH_SOFT_BOUNCE_MAX"]; ok {
		th.SoftBounceMax = supNum(v)
	}
	if v, ok := s["OUTREACH_CIRCUIT_MIN_SENDS"]; ok {
		th.MinSends = supNum(v)
	}
	if v, ok := s["OUTREACH_BOUNCE_PAUSE_RATE"]; ok {
		th.BounceRate = v.(float64)
	}
	if v, ok := s["OUTREACH_COMPLAINT_PAUSE_RATE"]; ok {
		th.ComplaintRate = v.(float64)
	}
	return th
}

// supRun seeds one scenario (vector ids are remapped to the real serial ids and
// to a per-run sequence prefix), runs its ops and compares every step's state.
type supRun struct {
	t      *testing.T
	pool   *pgxpool.Pool
	ws     string
	clock  time.Time
	prefix string
	enrol  map[int]int
	sends  map[int]int
	rEnrol map[int]int
	rSends map[int]int
}

func (r *supRun) seq(v any) string {
	if v == nil {
		return ""
	}
	return r.prefix + v.(string)
}

func (r *supRun) enrolID(v any) *int {
	if v == nil {
		return nil
	}
	id, ok := r.enrol[supNum(v)]
	if !ok {
		id = 900000000 + supNum(v)
	}
	return &id
}

func (r *supRun) sendID(v any) *int32 {
	if v == nil {
		return nil
	}
	id, ok := r.sends[supNum(v)]
	if !ok {
		id = 900000000 + supNum(v)
	}
	x := int32(id)
	return &x
}

func supMaybe(m map[string]any, k string, def any) any {
	if v, ok := m[k]; ok && v != nil {
		return v
	}
	return def
}

func (r *supRun) seed(sc supScenario) {
	t, ctx := r.t, context.Background()
	supTx(t, r.pool, r.ws, func(ctx context.Context, tx pgx.Tx) {
		for _, s := range sc.Setup.Sequences {
			_, err := tx.Exec(ctx, `INSERT INTO outreach_sequences (id, workspace_id, name, steps, status, bounce_count, complaint_count, auto_paused)
			  VALUES ($1,$2,'v','[]',$3,$4,$5,$6)`, r.prefix+s["id"].(string), r.ws,
				supMaybe(s, "status", "active"), supNum(supMaybe(s, "bounce_count", 0.0)), supNum(supMaybe(s, "complaint_count", 0.0)), supMaybe(s, "auto_paused", false))
			if err != nil {
				t.Fatalf("seed seq: %v", err)
			}
		}
		for _, e := range sc.Setup.Enrollments {
			var id int
			err := tx.QueryRow(ctx, `INSERT INTO outreach_enrollments (workspace_id, sequence_id, lead_id, to_email_snapshot, current_step, status, next_send_at, sent_count, soft_bounce_count)
			  VALUES ($1,$2,$7,$3,$4,$5,$6,0,0) RETURNING id`, r.ws, r.seq(e["sequence_id"]), e["to_email"], supNum(e["current_step"]), e["status"], r.clock, 1000+supNum(e["id"])).Scan(&id)
			if err != nil {
				t.Fatalf("seed enrollment: %v", err)
			}
			r.enrol[supNum(e["id"])], r.rEnrol[id] = id, supNum(e["id"])
		}
		for _, s := range sc.Setup.Sends {
			var enr any
			if s["enrollment_id"] != nil {
				enr = *r.enrolID(s["enrollment_id"])
			}
			var id int
			err := tx.QueryRow(ctx, `INSERT INTO outreach_sends (workspace_id, sequence_id, enrollment_id, lead_id, step_number, to_email, subject, status, message_id, idempotency_key, charged_usd)
			  VALUES ($1,$2,$3,$4,0,$5,'',$6,$7,$8,0) RETURNING id`, r.ws, r.seq(s["sequence_id"]), enr, supNum(s["lead_id"]), s["to_email"], s["status"],
				s["message_id"], r.ws+":"+s["idempotency_key"].(string)).Scan(&id)
			if err != nil {
				t.Fatalf("seed send: %v", err)
			}
			r.sends[supNum(s["id"])], r.rSends[id] = id, supNum(s["id"])
		}
		for _, s := range sc.Setup.Suppressions {
			if _, err := outreachstore.AddSuppression(ctx, tx, r.ws, s["email"].(string), s["reason"].(string), supStr(s["source"]), s["locked"] == true); err != nil {
				t.Fatalf("seed suppression: %v", err)
			}
		}
	})
	_ = ctx
}

func (r *supRun) iso(ts *time.Time) any {
	if ts == nil {
		return nil
	}
	return r.clock.Format("2006-01-02T15:04:05.000000") + "+00:00"
}

// state renders the observable tables in the vector shape. Timestamps the code
// stamps with now() (bounced_at) are normalised to the vector clock.
func (r *supRun) state() map[string]any {
	t := r.t
	out := map[string]any{}
	supTx(t, r.pool, r.ws, func(ctx context.Context, tx pgx.Tx) {
		var seqs, enrs, sends, supp, ledger []any

		rows, err := tx.Query(ctx, `SELECT id, COALESCE(auto_paused,false), COALESCE(bounce_count,0), COALESCE(complaint_count,0), status
		  FROM outreach_sequences WHERE workspace_id=$1 ORDER BY id`, r.ws)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id, status string
			var ap bool
			var bc, cc int
			_ = rows.Scan(&id, &ap, &bc, &cc, &status)
			seqs = append(seqs, map[string]any{"id": strings.TrimPrefix(id, r.prefix), "auto_paused": ap, "bounce_count": float64(bc), "complaint_count": float64(cc), "status": status})
		}
		rows.Close()

		rows, err = tx.Query(ctx, `SELECT id, sequence_id, current_step, COALESCE(error,''), last_sent_at, next_send_at, sent_count, soft_bounce_count, status
		  FROM outreach_enrollments WHERE workspace_id=$1`, r.ws)
		if err != nil {
			t.Fatal(err)
		}
		type erow struct {
			id  int
			row map[string]any
		}
		var er []erow
		for rows.Next() {
			var id, cs, sc, sb int
			var seq, e, st string
			var ls, ns *time.Time
			_ = rows.Scan(&id, &seq, &cs, &e, &ls, &ns, &sc, &sb, &st)
			er = append(er, erow{r.rEnrol[id], map[string]any{"id": float64(r.rEnrol[id]), "sequence_id": strings.TrimPrefix(seq, r.prefix), "current_step": float64(cs),
				"error": e, "last_sent_at": r.iso(ls), "next_send_at": r.iso(ns), "sent_count": float64(sc), "soft_bounce_count": float64(sb), "status": st}})
		}
		rows.Close()
		sort.Slice(er, func(i, j int) bool { return er[i].id < er[j].id })
		for _, e := range er {
			enrs = append(enrs, e.row)
		}

		rows, err = tx.Query(ctx, `SELECT id, sequence_id, enrollment_id, lead_id, step_number, to_email, COALESCE(subject,''), status, skip_reason, message_id,
		  idempotency_key, COALESCE(charged_usd,0), COALESCE(error,''), sent_at, bounced_at FROM outreach_sends WHERE workspace_id=$1`, r.ws)
		if err != nil {
			t.Fatal(err)
		}
		var sr []erow
		for rows.Next() {
			var id, lead, step int
			var enr *int
			var seq, to, subj, st, idem, e, mid string
			var skip *string
			var ch float64
			var sa, ba *time.Time
			_ = rows.Scan(&id, &seq, &enr, &lead, &step, &to, &subj, &st, &skip, &mid, &idem, &ch, &e, &sa, &ba)
			var en any
			if enr == nil {
				continue // the vector state lists enrollment-linked sends only (the rest is breaker filler)
			}
			en = float64(r.rEnrol[*enr])
			var sk any
			if skip != nil {
				sk = *skip
			}
			sr = append(sr, erow{r.rSends[id], map[string]any{"id": float64(r.rSends[id]), "sequence_id": strings.TrimPrefix(seq, r.prefix), "enrollment_id": en, "lead_id": float64(lead),
				"step_number": float64(step), "to_email": to, "subject": subj, "status": st, "skip_reason": sk, "message_id": mid,
				"idempotency_key": strings.TrimPrefix(idem, r.ws+":"), "charged_usd": ch, "error": e, "sent_at": r.iso(sa), "bounced_at": r.iso(ba)}})
		}
		rows.Close()
		sort.Slice(sr, func(i, j int) bool { return sr[i].id < sr[j].id })
		for _, e := range sr {
			sends = append(sends, e.row)
		}

		rows, err = tx.Query(ctx, `SELECT email, locked, reason, source FROM outreach_suppressions WHERE workspace_id=$1 ORDER BY email`, r.ws)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var em, rs, src string
			var lk bool
			_ = rows.Scan(&em, &lk, &rs, &src)
			supp = append(supp, map[string]any{"email": em, "locked": lk, "reason": rs, "source": src})
		}
		rows.Close()

		rows, err = tx.Query(ctx, `SELECT uidvalidity, imap_uid, source_message_id, matched_send_id, kind, COALESCE(recipient,''), COALESCE(diagnostic,'')
		  FROM outreach_inbound_messages WHERE workspace_id=$1 ORDER BY uidvalidity, imap_uid`, r.ws)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var uv, uid, mid, kind, rec, diag string
			var m *int
			_ = rows.Scan(&uv, &uid, &mid, &m, &kind, &rec, &diag)
			var ms any
			if m != nil {
				ms = float64(r.rSends[*m])
			}
			ledger = append(ledger, map[string]any{"uidvalidity": uv, "imap_uid": uid, "source_message_id": mid, "matched_send_id": ms, "kind": kind, "recipient": rec, "diagnostic": diag})
		}
		rows.Close()

		out["sequences"], out["enrollments"], out["sends"], out["suppressions"], out["ledger"] = nz(seqs), nz(enrs), nz(sends), nz(supp), nz(ledger)
	})
	return out
}

func nz(a []any) []any {
	if a == nil {
		return []any{}
	}
	return a
}

func supSort(m map[string]any) {
	for _, k := range []string{"suppressions", "ledger"} {
		a := m[k].([]any)
		sort.Slice(a, func(i, j int) bool {
			x, y := a[i].(map[string]any), a[j].(map[string]any)
			if k == "suppressions" {
				return x["email"].(string) < y["email"].(string)
			}
			kx, ky := x["uidvalidity"].(string)+"/"+x["imap_uid"].(string), y["uidvalidity"].(string)+"/"+y["imap_uid"].(string)
			return kx < ky
		})
	}
}

func supRunScenario(t *testing.T, pool *pgxpool.Pool, v supVec, sc supScenario) {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	clock, _ := time.Parse(time.RFC3339Nano, v.Clock)
	r := &supRun{t: t, pool: pool, ws: supNewWS(t, pool), clock: clock, prefix: "s" + hex.EncodeToString(b) + "-",
		enrol: map[int]int{}, sends: map[int]int{}, rEnrol: map[int]int{}, rSends: map[int]int{}}
	r.seed(sc)
	th := supThresholds(sc.Settings)
	for i, st := range sc.Steps {
		op := st.Op
		var result any
		supTx(t, pool, r.ws, func(ctx context.Context, tx pgx.Tx) {
			switch op["op"] {
			case "apply_bounce":
				err := outreachstore.ApplyBounce(ctx, tx, outreachstore.BounceApply{
					WorkspaceID: r.ws, SendID: r.sendID(op["send_id"]), SequenceID: r.seq(op["sequence_id"]), EnrollmentID: r.enrolID(op["enrollment_id"]),
					ToEmail: supStr(op["to_email"]), Kind: supStr(op["kind"]), Diagnostic: supStr(op["diagnostic"]), Source: supStr(op["source"]),
				}, th)
				if err != nil {
					t.Fatalf("step %d apply: %v", i, err)
				}
			case "record_bounce":
				tripped, err := outreachstore.RecordBounceAndMaybePause(ctx, tx, r.ws, r.seq(op["seq_id"]), op["complaint"] == true, th)
				if err != nil {
					t.Fatalf("step %d breaker: %v", i, err)
				}
				result = tripped
			case "record_inbound":
				created, err := outreachstore.RecordInboundProcessed(ctx, tx, outreachstore.InboundIn{
					WorkspaceID: r.ws, IMAPUID: supStr(op["imap_uid"]), UIDValidity: supStr(op["uidvalidity"]), UIDValidityNone: op["uidvalidity"] == nil, SourceMessageID: supStr(op["source_message_id"]),
					MatchedSendID: r.sendID(op["matched_send_id"]), Kind: supStr(op["kind"]), Recipient: supStr(op["recipient"]), Diagnostic: supStr(op["diagnostic"]),
				})
				if err != nil {
					t.Fatalf("step %d inbound: %v", i, err)
				}
				result = created
			default:
				t.Fatalf("unknown op %v", op["op"])
			}
		})
		if !reflect.DeepEqual(result, st.Result) {
			t.Errorf("%s step %d: result %v, want %v", sc.Name, i, result, st.Result)
		}
		got, want := r.state(), st.State
		supSort(got)
		supSort(want)
		for _, k := range []string{"sequences", "enrollments", "sends", "suppressions", "ledger"} {
			if !reflect.DeepEqual(got[k], want[k]) {
				g, _ := json.Marshal(got[k])
				w, _ := json.Marshal(want[k])
				t.Errorf("%s step %d: %s differ\n got  %s\n want %s", sc.Name, i, k, g, w)
			}
		}
	}
}

func TestApplyBounceMatchesVectors(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	v := supLoad(t, "apply_bounce.json")
	if len(v.Scenarios) < 19 {
		t.Fatalf("vectors: %d", len(v.Scenarios))
	}
	for _, sc := range v.Scenarios {
		sc := sc
		t.Run(sc.Name, func(t *testing.T) { supRunScenario(t, pool, v, sc) })
	}
}

func TestBreakerThresholdsMatchVectors(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	v := supLoad(t, "breaker.json")
	if len(v.Scenarios) < 20 {
		t.Fatalf("vectors: %d", len(v.Scenarios))
	}
	for _, sc := range v.Scenarios {
		sc := sc
		t.Run(sc.Name, func(t *testing.T) { supRunScenario(t, pool, v, sc) })
	}
}

func TestRecordInboundIdempotentBothKeys(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	v := supLoad(t, "inbound_ledger.json")
	if len(v.Scenarios) < 19 {
		t.Fatalf("vectors: %d", len(v.Scenarios))
	}
	for _, sc := range v.Scenarios {
		sc := sc
		t.Run(sc.Name, func(t *testing.T) { supRunScenario(t, pool, v, sc) })
	}
	// a conflict must not abort the caller's transaction
	ws := supNewWS(t, pool)
	supTx(t, pool, ws, func(ctx context.Context, tx pgx.Tx) {
		in := outreachstore.InboundIn{WorkspaceID: ws, IMAPUID: "5", UIDValidity: "1", SourceMessageID: "<k@x>", Kind: "soft"}
		if c, err := outreachstore.RecordInboundProcessed(ctx, tx, in); !c || err != nil {
			t.Fatalf("first: %v %v", c, err)
		}
		if c, err := outreachstore.RecordInboundProcessed(ctx, tx, in); c || err != nil {
			t.Fatalf("replay: %v %v", c, err)
		}
		if n, err := outreachstore.InboundMessageCount(ctx, tx, ws); n != 1 || err != nil {
			t.Fatalf("count after conflict: %d %v", n, err)
		}
	})
}

func TestBreakerNeedsMinSends(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	ws := supNewWS(t, pool)
	seq := "s" + ws[len(ws)-8:] + "-brk"
	supTx(t, pool, ws, func(ctx context.Context, tx pgx.Tx) {
		if _, err := tx.Exec(ctx, `INSERT INTO outreach_sequences (id, workspace_id, name, steps, status, auto_paused) VALUES ($1,$2,'b','[]','active',false)`, seq, ws); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 19; i++ {
			if _, err := tx.Exec(ctx, `INSERT INTO outreach_sends (workspace_id, sequence_id, step_number, to_email, status, idempotency_key)
			  VALUES ($1,$2,0,'a@example.com','sent',$3)`, ws, seq, fmt.Sprintf("k%d", i)); err != nil {
				t.Fatal(err)
			}
		}
		th := supDefaults()
		// 19 sends < 20: counters move, breaker stays quiet even at 100% bounce
		for i := 0; i < 3; i++ {
			if tr, err := outreachstore.RecordBounceAndMaybePause(ctx, tx, ws, seq, false, th); tr || err != nil {
				t.Fatalf("below min: %v %v", tr, err)
			}
		}
		s, _ := outreachstore.GetSequence(ctx, tx, ws, seq)
		if s.BounceCount != 3 || s.AutoPaused || (s.Status == nil || *s.Status != "active") {
			t.Fatalf("seq: %+v", s)
		}
		// the 20th send arrives: the next bounce evaluates and trips
		if _, err := tx.Exec(ctx, `INSERT INTO outreach_sends (workspace_id, sequence_id, step_number, to_email, status, idempotency_key)
		  VALUES ($1,$2,0,'a@example.com','sent','k19')`, ws, seq); err != nil {
			t.Fatal(err)
		}
		if tr, _ := outreachstore.RecordBounceAndMaybePause(ctx, tx, ws, seq, false, th); !tr {
			t.Fatalf("expected trip at min sends")
		}
	})
}

func TestAutoPausedSequenceStopsDue(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	ws := supNewWS(t, pool)
	seq := "s" + ws[len(ws)-8:] + "-pau"
	supTx(t, pool, ws, func(ctx context.Context, tx pgx.Tx) {
		if _, err := tx.Exec(ctx, `INSERT INTO outreach_sequences (id, workspace_id, name, steps, status, auto_paused) VALUES ($1,$2,'p','[]','active',false)`, seq, ws); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO outreach_sends (workspace_id, sequence_id, step_number, to_email, status, idempotency_key)
		  VALUES ($1,$2,0,'a@example.com','sent','k0')`, ws, seq); err != nil {
			t.Fatal(err)
		}
		th := outreachstore.Thresholds{MinSends: 0, BounceRate: 0.05, ComplaintRate: 0.003, SoftBounceMax: 3}
		tr, err := outreachstore.RecordBounceAndMaybePause(ctx, tx, ws, seq, true, th)
		if err != nil || !tr {
			t.Fatalf("trip: %v %v", tr, err)
		}
		s, _ := outreachstore.GetSequence(ctx, tx, ws, seq)
		if s == nil || s.Status == nil || *s.Status != "paused" || !s.AutoPaused || s.ComplaintCount != 1 {
			t.Fatalf("seq after trip: %+v", s)
		}
		// an already auto-paused sequence never trips again (counters still move)
		if tr, _ := outreachstore.RecordBounceAndMaybePause(ctx, tx, ws, seq, false, th); tr {
			t.Errorf("re-trip")
		}
		s, _ = outreachstore.GetSequence(ctx, tx, ws, seq)
		if s.BounceCount != 1 || s.Status == nil || *s.Status != "paused" {
			t.Errorf("counters: %+v", s)
		}
	})
}
