//go:build integration

package outreachstore_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachstore"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

const (
	wsA = "ws-p09t12-a"
	wsB = "ws-p09t12-b"
)

type env struct {
	t    *testing.T
	ctx  context.Context
	pool *pgxpool.Pool
}

func setup(t *testing.T) *env {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	e := &env{t: t, ctx: context.Background(), pool: pool}
	clean := func() {
		for _, w := range []string{wsA, wsB} {
			_ = tenancy.InWorkspace(e.ctx, pool, w, func(tx pgx.Tx) error {
				_, _ = tx.Exec(e.ctx, `DELETE FROM outreach_sends`)
				_, _ = tx.Exec(e.ctx, `DELETE FROM outreach_enrollments`)
				_, err := tx.Exec(e.ctx, `DELETE FROM outreach_sequences`)
				return err
			})
		}
		_, _ = pool.Exec(e.ctx, `DELETE FROM outreach_schedules WHERE workspace_id IN ($1,$2)`, wsA, wsB)
	}
	clean()
	t.Cleanup(clean)
	return e
}

func (e *env) in(ws string, fn func(tx pgx.Tx)) {
	e.t.Helper()
	err := tenancy.InWorkspace(e.ctx, e.pool, ws, func(tx pgx.Tx) error {
		fn(tx)
		return nil
	})
	if err != nil {
		e.t.Fatalf("InWorkspace: %v", err)
	}
}

func (e *env) newSeq(ws, name string) outreachstore.Seq {
	e.t.Helper()
	var s outreachstore.Seq
	e.in(ws, func(tx pgx.Tx) {
		var err error
		s, err = outreachstore.CreateSequence(e.ctx, tx, ws, outreachstore.NewSeqInput(name))
		if err != nil {
			e.t.Fatalf("CreateSequence: %v", err)
		}
	})
	return s
}

func (e *env) enroll(ws, seq string, lead int, email string) int {
	e.t.Helper()
	var id int
	e.in(ws, func(tx pgx.Tx) {
		var created bool
		var err error
		id, created, err = outreachstore.Enroll(e.ctx, tx, ws, seq, lead, email, "web form", time.Time{})
		if err != nil || !created {
			e.t.Fatalf("Enroll: id=%d created=%v err=%v", id, created, err)
		}
	})
	return id
}

func (e *env) exec(ws, sql string, args ...any) {
	e.t.Helper()
	e.in(ws, func(tx pgx.Tx) {
		if _, err := tx.Exec(e.ctx, sql, args...); err != nil {
			e.t.Fatalf("exec %q: %v", sql, err)
		}
	})
}

func (e *env) count(ws, sql string, args ...any) int {
	e.t.Helper()
	var n int
	e.in(ws, func(tx pgx.Tx) {
		if err := tx.QueryRow(e.ctx, sql, args...).Scan(&n); err != nil {
			e.t.Fatalf("count %q: %v", sql, err)
		}
	})
	return n
}

var isoRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d{6})?\+00:00$`)

func TestSequenceCRUDMatchesPythonShape(t *testing.T) {
	e := setup(t)
	var seq outreachstore.Seq
	e.in(wsA, func(tx pgx.Tx) {
		in := outreachstore.NewSeqInput("Welcome")
		in.Steps = json.RawMessage(`[{"step": 1, "subject": "Hi {{name}}", "body": "x", "delay_days": 0}]`)
		var err error
		seq, err = outreachstore.CreateSequence(e.ctx, tx, wsA, in)
		if err != nil {
			t.Fatal(err)
		}
	})
	if len(seq.ID) != 36 || seq.ID[14] != '4' {
		t.Fatalf("id is not a uuid4: %q", seq.ID)
	}
	b, _ := json.Marshal(seq)
	var ordered []string
	dec := json.NewDecoder(bytes.NewReader(b))
	_, _ = dec.Token()
	for dec.More() {
		k, _ := dec.Token()
		ordered = append(ordered, k.(string))
		var skip json.RawMessage
		_ = dec.Decode(&skip)
	}
	want := []string{"id", "workspace_id", "name", "description", "steps", "status", "daily_limit",
		"send_window_start", "send_window_end", "send_window_tz", "consent_basis", "bounce_count",
		"complaint_count", "auto_paused", "created_at", "updated_at"}
	if len(ordered) != len(want) {
		t.Fatalf("keys %v", ordered)
	}
	for i := range want {
		if ordered[i] != want[i] {
			t.Fatalf("key order %v want %v", ordered, want)
		}
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if m["status"] != "draft" || m["daily_limit"] != float64(50) || m["send_window_start"] != float64(9) ||
		m["send_window_end"] != float64(18) || m["send_window_tz"] != "UTC" || m["description"] != "" ||
		m["consent_basis"] != "" || m["bounce_count"] != float64(0) || m["complaint_count"] != float64(0) ||
		m["auto_paused"] != false || m["workspace_id"] != wsA || m["name"] != "Welcome" {
		t.Fatalf("defaults: %s", b)
	}
	if string(seq.Steps) != `[{"step": 1, "subject": "Hi {{name}}", "body": "x", "delay_days": 0}]` {
		t.Fatalf("steps not stored verbatim: %s", seq.Steps)
	}
	if seq.CreatedAt == nil || !isoRe.MatchString(*seq.CreatedAt) || seq.UpdatedAt == nil || !isoRe.MatchString(*seq.UpdatedAt) {
		t.Fatalf("iso timestamps: %v %v", seq.CreatedAt, seq.UpdatedAt)
	}

	second := e.newSeq(wsA, "Second")
	e.in(wsA, func(tx pgx.Tx) {
		list, err := outreachstore.ListSequences(e.ctx, tx, wsA)
		if err != nil || len(list) != 2 || list[0].ID != second.ID || list[1].ID != seq.ID {
			t.Fatalf("list newest first: %v %v", list, err)
		}
		if string(list[0].Steps) != `[]` {
			t.Fatalf("empty steps: %s", list[0].Steps)
		}
		got, err := outreachstore.GetSequence(e.ctx, tx, wsA, seq.ID)
		if err != nil || got == nil || got.Name != "Welcome" {
			t.Fatalf("get: %v %v", got, err)
		}
		miss, err := outreachstore.GetSequence(e.ctx, tx, wsA, "nope")
		if err != nil || miss != nil {
			t.Fatalf("get missing: %v %v", miss, err)
		}
		ok, err := outreachstore.SequenceExists(e.ctx, tx, wsA, seq.ID)
		if err != nil || !ok {
			t.Fatalf("exists: %v %v", ok, err)
		}
		ok, _ = outreachstore.SequenceExists(e.ctx, tx, wsA, "nope")
		if ok {
			t.Fatal("exists for unknown id")
		}
	})
}

func TestUpdateOnlyGivenKeys(t *testing.T) {
	e := setup(t)
	seq := e.newSeq(wsA, "Orig")
	time.Sleep(5 * time.Millisecond)
	e.in(wsA, func(tx pgx.Tx) {
		got, err := outreachstore.UpdateSequence(e.ctx, tx, wsA, seq.ID, map[string]any{
			"name":          "Renamed",
			"daily_limit":   float64(10), // decoded-JSON number
			"steps":         json.RawMessage(`[{"step": 1}]`),
			"not_a_column":  "ignored",
			"workspace_id":  "ws-evil",
			"bounce_count":  99,
			"consent_basis": "legitimate_interest",
		})
		if err != nil || got == nil {
			t.Fatalf("update: %v %v", got, err)
		}
		if got.Name != "Renamed" || *got.DailyLimit != 10 || string(got.Steps) != `[{"step": 1}]` ||
			got.ConsentBasis != "legitimate_interest" {
			t.Fatalf("changed fields: %+v", got)
		}
		if got.WorkspaceID != wsA || got.BounceCount != 0 {
			t.Fatalf("non-updatable fields changed: %+v", got)
		}
		if *got.SendWindowStart != 9 || *got.SendWindowEnd != 18 || *got.SendWindowTZ != "UTC" || *got.Status != "draft" {
			t.Fatalf("untouched fields changed: %+v", got)
		}
		if *got.UpdatedAt == *seq.UpdatedAt {
			t.Fatalf("updated_at not bumped")
		}
		before, _ := outreachstore.GetSequence(e.ctx, tx, wsA, seq.ID)
		same, err := outreachstore.UpdateSequence(e.ctx, tx, wsA, seq.ID, map[string]any{})
		if err != nil || same == nil || *same.UpdatedAt != *before.UpdatedAt {
			t.Fatalf("empty update must not touch the row: %+v %v", same, err)
		}
		miss, err := outreachstore.UpdateSequence(e.ctx, tx, wsA, "nope", map[string]any{"name": "x"})
		if err != nil || miss != nil {
			t.Fatalf("missing: %v %v", miss, err)
		}
		st, err := outreachstore.SetSequenceStatus(e.ctx, tx, wsA, seq.ID, "paused")
		if err != nil || st == nil || *st.Status != "paused" || st.Name != "Renamed" {
			t.Fatalf("set status: %+v %v", st, err)
		}
		if _, err := outreachstore.UpdateSequence(e.ctx, tx, wsA, seq.ID, map[string]any{"daily_limit": "many"}); err == nil {
			t.Fatal("expected type error")
		}
	})
}

func TestDeleteCascadesLikePython(t *testing.T) {
	e := setup(t)
	a := e.newSeq(wsA, "A")
	b := e.newSeq(wsA, "B")
	ea := e.enroll(wsA, a.ID, 1, "a@example.com")
	eb := e.enroll(wsA, b.ID, 2, "b@example.com")
	for i, x := range []struct {
		seq string
		enr int
	}{{a.ID, ea}, {b.ID, eb}} {
		e.exec(wsA, `INSERT INTO outreach_sends (workspace_id, sequence_id, enrollment_id, step_number, to_email, status, idempotency_key)
			VALUES ($1,$2,$3,0,'x@example.com','sent',$4)`, wsA, x.seq, x.enr, "k"+string(rune('0'+i)))
	}
	e.in(wsA, func(tx pgx.Tx) {
		if err := outreachstore.UpsertSchedule(e.ctx, tx, wsA, a.ID, time.Now(), true); err != nil {
			t.Fatal(err)
		}
		if err := outreachstore.UpsertSchedule(e.ctx, tx, wsA, b.ID, time.Now(), true); err != nil {
			t.Fatal(err)
		}
	})
	e.in(wsA, func(tx pgx.Tx) {
		ok, err := outreachstore.DeleteSequence(e.ctx, tx, wsA, a.ID)
		if err != nil || !ok {
			t.Fatalf("delete: %v %v", ok, err)
		}
		ok, err = outreachstore.DeleteSequence(e.ctx, tx, wsA, a.ID)
		if err != nil || ok {
			t.Fatalf("second delete: %v %v", ok, err)
		}
	})
	if n := e.count(wsA, `SELECT count(*) FROM outreach_sequences WHERE id=$1`, a.ID); n != 0 {
		t.Fatalf("sequence left: %d", n)
	}
	if n := e.count(wsA, `SELECT count(*) FROM outreach_enrollments WHERE sequence_id=$1`, a.ID); n != 0 {
		t.Fatalf("enrollments left: %d", n)
	}
	if n := e.count(wsA, `SELECT count(*) FROM outreach_sends WHERE sequence_id=$1`, a.ID); n != 0 {
		t.Fatalf("sends left: %d", n)
	}
	if n := e.count(wsA, `SELECT count(*) FROM outreach_schedules WHERE sequence_id=$1`, a.ID); n != 0 {
		t.Fatalf("schedule left: %d", n)
	}
	if e.count(wsA, `SELECT count(*) FROM outreach_sequences WHERE id=$1`, b.ID) != 1 ||
		e.count(wsA, `SELECT count(*) FROM outreach_enrollments WHERE sequence_id=$1`, b.ID) != 1 ||
		e.count(wsA, `SELECT count(*) FROM outreach_sends WHERE sequence_id=$1`, b.ID) != 1 ||
		e.count(wsA, `SELECT count(*) FROM outreach_schedules WHERE sequence_id=$1`, b.ID) != 1 {
		t.Fatal("other sequence was touched")
	}
}

func TestEnrollIdempotent(t *testing.T) {
	e := setup(t)
	seq := e.newSeq(wsA, "S")
	consent := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	e.in(wsA, func(tx pgx.Tx) {
		id, created, err := outreachstore.Enroll(e.ctx, tx, wsA, seq.ID, 7, "  Lead@Example.COM ", "web form", consent)
		if err != nil || !created || id == 0 {
			t.Fatalf("first: %d %v %v", id, created, err)
		}
		id2, created2, err := outreachstore.Enroll(e.ctx, tx, wsA, seq.ID, 7, "lead@example.com", "other", time.Time{})
		if err != nil || created2 || id2 != 0 {
			t.Fatalf("dup: %d %v %v", id2, created2, err)
		}
		// the transaction must survive the conflict
		en, err := outreachstore.GetEnrollment(e.ctx, tx, id)
		if err != nil || en == nil {
			t.Fatalf("get after conflict: %v %v", en, err)
		}
		if en.ToEmailSnapshot != "lead@example.com" || en.Status != "pending" || en.CurrentStep != 0 ||
			en.SentCount != 0 || en.SoftBounceCount != 0 || en.ConsentSource != "web form" ||
			en.ConsentAt == nil || !en.ConsentAt.Equal(consent) || en.SequenceID != seq.ID || en.LeadID != 7 {
			t.Fatalf("enrollment: %+v", en)
		}
		if en.NextSendAt == nil || time.Since(*en.NextSendAt) > time.Minute || en.NextSendAt.After(time.Now().Add(time.Minute)) {
			t.Fatalf("next_send_at: %v", en.NextSendAt)
		}
		// another lead and an unconsented enroll are fine
		id3, created3, err := outreachstore.Enroll(e.ctx, tx, wsA, seq.ID, 8, "x@example.com", "", time.Time{})
		if err != nil || !created3 || id3 == id {
			t.Fatalf("second lead: %d %v %v", id3, created3, err)
		}
		en3, _ := outreachstore.GetEnrollment(e.ctx, tx, id3)
		if en3.ConsentAt != nil || en3.ConsentSource != "" {
			t.Fatalf("no consent: %+v", en3)
		}
		if _, _, err := outreachstore.Enroll(e.ctx, tx, wsA, seq.ID, 9, "   ", "", time.Time{}); !errors.Is(err, outreachstore.ErrNoEmail) {
			t.Fatalf("empty email: %v", err)
		}
	})
}

func TestDueEnrollmentsOrderAndLimit(t *testing.T) {
	e := setup(t)
	seq := e.newSeq(wsA, "S")
	other := e.newSeq(wsA, "O")
	now := time.Now()
	e1 := e.enroll(wsA, seq.ID, 1, "1@example.com")
	e2 := e.enroll(wsA, seq.ID, 2, "2@example.com")
	e3 := e.enroll(wsA, seq.ID, 3, "3@example.com")
	e4 := e.enroll(wsA, seq.ID, 4, "4@example.com") // future
	e5 := e.enroll(wsA, seq.ID, 5, "5@example.com") // wrong status
	e6 := e.enroll(wsA, seq.ID, 6, "6@example.com") // already has a send for the current step
	e7 := e.enroll(wsA, seq.ID, 7, "7@example.com") // send exists for an earlier step only
	_ = e.enroll(wsA, other.ID, 8, "8@example.com")
	e.in(wsA, func(tx pgx.Tx) {
		for id, at := range map[int]time.Time{
			e1: now.Add(-1 * time.Hour), e2: now.Add(-3 * time.Hour), e3: now.Add(-2 * time.Hour),
			e4: now.Add(time.Hour), e5: now.Add(-4 * time.Hour), e6: now.Add(-5 * time.Hour), e7: now.Add(-6 * time.Hour),
		} {
			if err := outreachstore.RescheduleEnrollment(e.ctx, tx, id, at); err != nil {
				t.Fatal(err)
			}
		}
	})
	e.exec(wsA, `UPDATE outreach_enrollments SET status='replied' WHERE id=$1`, e5)
	e.exec(wsA, `INSERT INTO outreach_sends (workspace_id, sequence_id, enrollment_id, step_number, to_email, status, idempotency_key)
		VALUES ($1,$2,$3,0,'6@example.com','sent','due-6')`, wsA, seq.ID, e6)
	e.exec(wsA, `UPDATE outreach_enrollments SET current_step=1 WHERE id=$1`, e7)
	e.exec(wsA, `INSERT INTO outreach_sends (workspace_id, sequence_id, enrollment_id, step_number, to_email, status, idempotency_key)
		VALUES ($1,$2,$3,0,'7@example.com','sent','due-7')`, wsA, seq.ID, e7)

	e.in(wsA, func(tx pgx.Tx) {
		got, err := outreachstore.DueEnrollments(e.ctx, tx, seq.ID, 200)
		if err != nil {
			t.Fatal(err)
		}
		var ids []int
		for _, g := range got {
			ids = append(ids, g.ID)
			if g.Status != "scheduled" {
				t.Fatalf("reschedule must flip pending to scheduled: %+v", g)
			}
		}
		want := []int{e7, e2, e3, e1}
		if len(ids) != len(want) {
			t.Fatalf("ids %v want %v", ids, want)
		}
		for i := range want {
			if ids[i] != want[i] {
				t.Fatalf("order %v want %v", ids, want)
			}
		}
		lim, err := outreachstore.DueEnrollments(e.ctx, tx, seq.ID, 2)
		if err != nil || len(lim) != 2 || lim[0].ID != e7 || lim[1].ID != e2 {
			t.Fatalf("limit: %+v %v", lim, err)
		}
		d := lim[0].Dict()
		b, _ := json.Marshal(d)
		prefix := `{"id":` + strconv.Itoa(e7) + `,"sequence_id":"` + seq.ID + `","lead_id":7,"to_email_snapshot":"7@example.com","current_step":1,"status":"scheduled","next_send_at":"`
		if !strings.HasPrefix(string(b), prefix) || !strings.HasSuffix(string(b), `+00:00","sent_count":0,"soft_bounce_count":0}`) {
			t.Fatalf("dict: %s", b)
		}
	})
}

func TestAdvanceEnrollmentVariants(t *testing.T) {
	e := setup(t)
	seq := e.newSeq(wsA, "S")
	id := e.enroll(wsA, seq.ID, 1, "a@example.com")
	next := time.Date(2026, 9, 1, 8, 30, 0, 0, time.UTC)
	step := 1
	e.in(wsA, func(tx pgx.Tx) {
		// sent: increment, step and next
		if err := outreachstore.AdvanceEnrollment(e.ctx, tx, id, outreachstore.Advance{
			Status: "scheduled", NextSendAt: &next, CurrentStep: &step, IncrementSent: true}); err != nil {
			t.Fatal(err)
		}
		en, _ := outreachstore.GetEnrollment(e.ctx, tx, id)
		if en.Status != "scheduled" || en.SentCount != 1 || en.CurrentStep != 1 || !en.NextSendAt.Equal(next) ||
			en.LastSentAt == nil || time.Since(*en.LastSentAt) > time.Minute || en.Error != "" {
			t.Fatalf("sent variant: %+v", en)
		}
		// status-only: nothing else changes, error kept
		if err := outreachstore.AdvanceEnrollment(e.ctx, tx, id, outreachstore.Advance{Status: "paused"}); err != nil {
			t.Fatal(err)
		}
		en, _ = outreachstore.GetEnrollment(e.ctx, tx, id)
		if en.Status != "paused" || en.SentCount != 1 || en.CurrentStep != 1 || !en.NextSendAt.Equal(next) {
			t.Fatalf("status only: %+v", en)
		}
		// error text set; later empty error keeps it (Python only writes when truthy)
		if err := outreachstore.AdvanceEnrollment(e.ctx, tx, id, outreachstore.Advance{Status: "failed", Error: "smtp refused"}); err != nil {
			t.Fatal(err)
		}
		if err := outreachstore.AdvanceEnrollment(e.ctx, tx, id, outreachstore.Advance{Status: "failed"}); err != nil {
			t.Fatal(err)
		}
		en, _ = outreachstore.GetEnrollment(e.ctx, tx, id)
		if en.Status != "failed" || en.Error != "smtp refused" {
			t.Fatalf("error: %+v", en)
		}
		// step 0 is a real value (pointer), not "unset"
		zero := 0
		if err := outreachstore.AdvanceEnrollment(e.ctx, tx, id, outreachstore.Advance{Status: "pending", CurrentStep: &zero}); err != nil {
			t.Fatal(err)
		}
		en, _ = outreachstore.GetEnrollment(e.ctx, tx, id)
		if en.CurrentStep != 0 {
			t.Fatalf("step zero: %+v", en)
		}
		// unknown id is a silent no-op like Python
		if err := outreachstore.AdvanceEnrollment(e.ctx, tx, 999999, outreachstore.Advance{Status: "x"}); err != nil {
			t.Fatalf("missing id: %v", err)
		}
	})
}

func TestSoftBounceCounter(t *testing.T) {
	e := setup(t)
	seq := e.newSeq(wsA, "S")
	id := e.enroll(wsA, seq.ID, 1, "a@example.com")
	e.in(wsA, func(tx pgx.Tx) {
		for want := 1; want <= 3; want++ {
			n, err := outreachstore.IncrementSoftBounce(e.ctx, tx, id)
			if err != nil || n != want {
				t.Fatalf("n=%d want %d err=%v", n, want, err)
			}
		}
		n, err := outreachstore.IncrementSoftBounce(e.ctx, tx, 999999)
		if err != nil || n != 0 {
			t.Fatalf("missing: %d %v", n, err)
		}
	})
}

func TestScheduleMirrorUpsertDisable(t *testing.T) {
	e := setup(t)
	seq := e.newSeq(wsA, "S")
	t1 := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)
	read := func() (ws string, next time.Time, enabled bool, n int) {
		_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM outreach_schedules WHERE sequence_id=$1`, seq.ID).Scan(&n)
		if n == 1 {
			if err := e.pool.QueryRow(e.ctx, `SELECT workspace_id, next_tick_at, enabled FROM outreach_schedules WHERE sequence_id=$1`, seq.ID).Scan(&ws, &next, &enabled); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	e.in(wsA, func(tx pgx.Tx) {
		if err := outreachstore.UpsertSchedule(e.ctx, tx, wsA, seq.ID, t1, true); err != nil {
			t.Fatal(err)
		}
	})
	if ws, next, en, n := read(); n != 1 || ws != wsA || !next.Equal(t1) || !en {
		t.Fatalf("insert: %s %v %v %d", ws, next, en, n)
	}
	e.in(wsA, func(tx pgx.Tx) {
		if err := outreachstore.UpsertSchedule(e.ctx, tx, wsA, seq.ID, t2, true); err != nil {
			t.Fatal(err)
		}
	})
	if _, next, _, n := read(); n != 1 || !next.Equal(t2) {
		t.Fatalf("update: %v %d", next, n)
	}
	e.in(wsA, func(tx pgx.Tx) {
		if err := outreachstore.DisableSchedule(e.ctx, tx, seq.ID); err != nil {
			t.Fatal(err)
		}
		if err := outreachstore.DisableSchedule(e.ctx, tx, "unknown-seq"); err != nil {
			t.Fatalf("disable unknown: %v", err)
		}
	})
	if _, next, en, n := read(); n != 1 || en || !next.Equal(t2) {
		t.Fatalf("disable keeps next_tick_at: %v %v %d", next, en, n)
	}
	// another workspace cannot hijack or disable it
	e.in(wsB, func(tx pgx.Tx) {
		if err := outreachstore.UpsertSchedule(e.ctx, tx, wsB, seq.ID, t1, true); err != nil {
			t.Fatal(err)
		}
		if err := outreachstore.DisableSchedule(e.ctx, tx, seq.ID); err != nil {
			t.Fatal(err)
		}
	})
	if ws, next, en, n := read(); n != 1 || ws != wsA || en || !next.Equal(t2) {
		t.Fatalf("hijack: %s %v %v %d", ws, next, en, n)
	}
}

func TestTenantIsolation(t *testing.T) {
	e := setup(t)
	seq := e.newSeq(wsA, "Secret")
	enr := e.enroll(wsA, seq.ID, 1, "a@example.com")
	e.exec(wsA, `INSERT INTO outreach_sends (workspace_id, sequence_id, enrollment_id, step_number, to_email, status, idempotency_key)
		VALUES ($1,$2,$3,0,'a@example.com','sent','iso-1')`, wsA, seq.ID, enr)
	e.in(wsB, func(tx pgx.Tx) {
		if l, err := outreachstore.ListSequences(e.ctx, tx, wsB); err != nil || len(l) != 0 {
			t.Fatalf("list: %v %v", l, err)
		}
		if g, err := outreachstore.GetSequence(e.ctx, tx, wsB, seq.ID); err != nil || g != nil {
			t.Fatalf("get: %v %v", g, err)
		}
		if ok, _ := outreachstore.SequenceExists(e.ctx, tx, wsB, seq.ID); ok {
			t.Fatal("exists leaked")
		}
		if u, err := outreachstore.UpdateSequence(e.ctx, tx, wsB, seq.ID, map[string]any{"name": "pwn"}); err != nil || u != nil {
			t.Fatalf("update: %v %v", u, err)
		}
		if en, err := outreachstore.GetEnrollment(e.ctx, tx, enr); err != nil || en != nil {
			t.Fatalf("get enrollment: %v %v", en, err)
		}
		if d, err := outreachstore.DueEnrollments(e.ctx, tx, seq.ID, 10); err != nil || len(d) != 0 {
			t.Fatalf("due: %v %v", d, err)
		}
		if n, err := outreachstore.IncrementSoftBounce(e.ctx, tx, enr); err != nil || n != 0 {
			t.Fatalf("soft bounce: %d %v", n, err)
		}
		if err := outreachstore.AdvanceEnrollment(e.ctx, tx, enr, outreachstore.Advance{Status: "failed"}); err != nil {
			t.Fatal(err)
		}
		if ok, err := outreachstore.DeleteSequence(e.ctx, tx, wsB, seq.ID); err != nil || ok {
			t.Fatalf("delete: %v %v", ok, err)
		}
		// the same lead and sequence id in B's own tenant cannot reach A's sequence either
		if _, _, err := outreachstore.Enroll(e.ctx, tx, wsA, seq.ID, 1, "a@example.com", "", time.Time{}); err == nil {
			t.Fatal("cross-workspace enroll must be refused by RLS")
		}
	})
	e.in(wsA, func(tx pgx.Tx) {
		g, _ := outreachstore.GetSequence(e.ctx, tx, wsA, seq.ID)
		en, _ := outreachstore.GetEnrollment(e.ctx, tx, enr)
		if g == nil || g.Name != "Secret" || en == nil || en.Status != "pending" {
			t.Fatalf("A's data was touched: %+v %+v", g, en)
		}
	})
	if e.count(wsA, `SELECT count(*) FROM outreach_sends WHERE sequence_id=$1`, seq.ID) != 1 {
		t.Fatal("A's sends were touched")
	}
}
