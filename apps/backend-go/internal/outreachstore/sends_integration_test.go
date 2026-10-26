//go:build integration

package outreachstore_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachstore"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

func newWS(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	ws := "p09t13-" + hex.EncodeToString(b)
	t.Cleanup(func() {
		_ = tenancy.InWorkspace(context.Background(), pool, ws, func(tx pgx.Tx) error {
			for _, q := range []string{"outreach_sends", "outreach_enrollments", "outreach_sequences"} {
				if _, err := tx.Exec(context.Background(), "DELETE FROM "+q+" WHERE workspace_id=$1", ws); err != nil {
					return err
				}
			}
			return nil
		})
	})
	return ws
}

func in(ws, seq, idem, to string) outreachstore.SendIn {
	return outreachstore.SendIn{WorkspaceID: ws, SequenceID: seq, StepNumber: 1, ToEmail: to, Subject: "hi", IdempotencyKey: idem}
}

func inWS(t *testing.T, pool *pgxpool.Pool, ws string, fn func(tx pgx.Tx) error) {
	t.Helper()
	if err := tenancy.InWorkspace(context.Background(), pool, ws, fn); err != nil {
		t.Fatalf("tx: %v", err)
	}
}

func i32(v int32) *int32 { return &v }

func TestInsertInFlightUniqueIdem(t *testing.T) {
	pool := testpg.NewAppPool(t, 20)
	ws := newWS(t, pool)
	var created atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := tenancy.InWorkspace(context.Background(), pool, ws, func(tx pgx.Tx) error {
				ok, err := outreachstore.InsertInFlight(context.Background(), tx, in(ws, "seq-1", "seq:"+ws+":seq-1:1:1", "a@example.com"))
				if ok {
					created.Add(1)
				}
				return err
			})
			if err != nil {
				t.Errorf("insert: %v", err)
			}
		}()
	}
	wg.Wait()
	if created.Load() != 1 {
		t.Fatalf("created=%d, want exactly 1", created.Load())
	}
	inWS(t, pool, ws, func(tx pgx.Tx) error {
		var n int
		_ = tx.QueryRow(context.Background(), "SELECT count(*) FROM outreach_sends WHERE workspace_id=$1", ws).Scan(&n)
		if n != 1 {
			t.Errorf("rows=%d", n)
		}
		s, err := outreachstore.GetSendByIdem(context.Background(), tx, ws, "seq:"+ws+":seq-1:1:1")
		if err != nil || s == nil {
			t.Fatalf("get: %v %v", s, err)
		}
		if s.Status != "in_flight" || s.ChargedUSD != 0 || s.Migrated || s.Error != "" || s.MessageID != "" || s.SentAt != nil {
			t.Errorf("unexpected in_flight row %+v", s)
		}
		return nil
	})
}

func TestFinalizeOnlySetsGivenFields(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	ws := newWS(t, pool)
	idem := "k1"
	inWS(t, pool, ws, func(tx pgx.Tx) error {
		ctx := context.Background()
		if ok, err := outreachstore.InsertInFlight(ctx, tx, in(ws, "s", idem, "a@example.com")); !ok || err != nil {
			t.Fatalf("insert %v %v", ok, err)
		}
		// first finalize: error + status only
		if err := outreachstore.FinalizeSend(ctx, tx, ws, idem, outreachstore.Finalize{Status: "failed", Error: "boom"}); err != nil {
			t.Fatal(err)
		}
		s, _ := outreachstore.GetSendByIdem(ctx, tx, ws, idem)
		if s.Status != "failed" || s.Error != "boom" || s.MessageID != "" || s.SentAt != nil || s.SkipReason != "" || s.BouncedAt != nil || s.ChargedUSD != 0 {
			t.Errorf("after 1: %+v", s)
		}
		// second: set message_id/sent_at/charge; error and skip_reason must stay
		now := time.Now().UTC().Truncate(time.Microsecond)
		c := 0.05
		if err := outreachstore.FinalizeSend(ctx, tx, ws, idem, outreachstore.Finalize{Status: "sent", MessageID: "<m@x>", SentAt: &now, ChargedUSD: &c}); err != nil {
			t.Fatal(err)
		}
		s, _ = outreachstore.GetSendByIdem(ctx, tx, ws, idem)
		if s.Status != "sent" || s.Error != "boom" || s.MessageID != "<m@x>" || s.SentAt == nil || !s.SentAt.Equal(now) || s.ChargedUSD != 0.05 || s.BouncedAt != nil {
			t.Errorf("after 2: %+v", s)
		}
		// skip_reason + bounced_at only set when given
		b := now.Add(time.Minute)
		if err := outreachstore.FinalizeSend(ctx, tx, ws, idem, outreachstore.Finalize{Status: "bounced", SkipReason: "replay", BouncedAt: &b}); err != nil {
			t.Fatal(err)
		}
		s, _ = outreachstore.GetSendByIdem(ctx, tx, ws, idem)
		if s.Status != "bounced" || s.SkipReason != "replay" || s.BouncedAt == nil || s.ChargedUSD != 0.05 || s.MessageID != "<m@x>" {
			t.Errorf("after 3: %+v", s)
		}
		// finalize of an unknown key is a silent no-op (Python returns)
		if err := outreachstore.FinalizeSend(ctx, tx, ws, "nope", outreachstore.Finalize{Status: "sent"}); err != nil {
			t.Errorf("unknown idem: %v", err)
		}
		return nil
	})
}

func TestTerminalIgnoresExistingIdem(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	ws := newWS(t, pool)
	inWS(t, pool, ws, func(tx pgx.Tx) error {
		ctx := context.Background()
		if err := outreachstore.InsertTerminal(ctx, tx, in(ws, "s", "t1", "a@example.com"), "skipped", "suppressed", ""); err != nil {
			t.Fatal(err)
		}
		if err := outreachstore.InsertTerminal(ctx, tx, in(ws, "s", "t1", "b@example.com"), "failed", "", "SMTP not configured"); err != nil {
			t.Fatalf("second terminal must not error: %v", err)
		}
		s, _ := outreachstore.GetSendByIdem(ctx, tx, ws, "t1")
		if s.Status != "skipped" || s.SkipReason != "suppressed" || s.ToEmail != "a@example.com" {
			t.Errorf("existing row changed: %+v", s)
		}
		// empty recipient defaults like Python _record_terminal
		if err := outreachstore.InsertTerminal(ctx, tx, in(ws, "", "t2", ""), "skipped", "no_email", ""); err != nil {
			t.Fatal(err)
		}
		s, _ = outreachstore.GetSendByIdem(ctx, tx, ws, "t2")
		if s.ToEmail != "unknown@unknown" || s.SequenceID != "" || s.Status != "skipped" || s.SkipReason != "no_email" {
			t.Errorf("t2: %+v", s)
		}
		if err := outreachstore.InsertTerminal(ctx, tx, in(ws, "s", "t3", "a@example.com"), "failed", "", "SMTP not configured"); err != nil {
			t.Fatal(err)
		}
		s, _ = outreachstore.GetSendByIdem(ctx, tx, ws, "t3")
		if s.Status != "failed" || s.SkipReason != "" || s.Error != "SMTP not configured" {
			t.Errorf("t3: %+v", s)
		}
		if s, _ := outreachstore.GetSendByIdem(ctx, tx, ws, "absent"); s != nil {
			t.Errorf("absent should be nil")
		}
		return nil
	})
}

func seedSent(t *testing.T, tx pgx.Tx, ws, seq, idem, to, msgID, status string, sentAt time.Time) {
	t.Helper()
	_, err := tx.Exec(context.Background(), `INSERT INTO outreach_sends
	  (workspace_id, sequence_id, step_number, to_email, subject, status, message_id, idempotency_key, sent_at, created_at)
	  VALUES ($1,$2,1,$3,'s',$4,$5,$6,$7,$7)`, ws, seq, to, status, msgID, idem, sentAt)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func TestMessageIDLookupWorkspaceScoped(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	a, b := newWS(t, pool), newWS(t, pool)
	now := time.Now().UTC()
	inWS(t, pool, a, func(tx pgx.Tx) error {
		seedSent(t, tx, a, "s", "a1", "x@example.com", "<dup@x>", "sent", now.Add(-2*time.Minute))
		seedSent(t, tx, a, "s", "a2", "x@example.com", "<dup@x>", "sent", now.Add(-time.Minute))
		return nil
	})
	inWS(t, pool, b, func(tx pgx.Tx) error {
		seedSent(t, tx, b, "s", "b1", "y@example.com", "<only-b@x>", "sent", now)
		return nil
	})
	inWS(t, pool, a, func(tx pgx.Tx) error {
		ctx := context.Background()
		r, err := outreachstore.GetSendByMessageID(ctx, tx, a, "  <dup@x> ")
		if err != nil || r == nil {
			t.Fatalf("dup: %v %v", r, err)
		}
		if r.ToEmail != "x@example.com" {
			t.Errorf("ref %+v", r)
		}
		var maxID int32
		_ = tx.QueryRow(ctx, "SELECT max(id) FROM outreach_sends WHERE workspace_id=$1", a).Scan(&maxID)
		if r.ID != maxID {
			t.Errorf("most recent (highest id) expected: got %d want %d", r.ID, maxID)
		}
		if r, _ := outreachstore.GetSendByMessageID(ctx, tx, a, "<only-b@x>"); r != nil {
			t.Errorf("workspace A saw B's send")
		}
		if r, _ := outreachstore.GetSendByMessageID(ctx, tx, a, "   "); r != nil {
			t.Errorf("blank message id must be nil")
		}
		return nil
	})
}

func TestGlobalLookupReturnsOnlyRoutingTuple(t *testing.T) {
	pool := testpg.NewAppPool(t, 4) // non-superuser NOBYPASSRLS, no GUC set
	a := newWS(t, pool)
	var sendID int32
	inWS(t, pool, a, func(tx pgx.Tx) error {
		seedSent(t, tx, a, "seq-g", "g1", "Victim@Example.com", "<glob@x>", "sent", time.Now())
		_, _ = tx.Exec(context.Background(), "UPDATE outreach_sends SET enrollment_id=7 WHERE workspace_id=$1", a)
		return tx.QueryRow(context.Background(), "SELECT id FROM outreach_sends WHERE workspace_id=$1", a).Scan(&sendID)
	})
	// direct table access without the GUC sees nothing (RLS), the function does.
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM outreach_sends WHERE message_id='<glob@x>'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("RLS must hide rows without GUC: n=%d err=%v", n, err)
	}
	ref, err := outreachstore.LookupSendAnyWorkspace(context.Background(), pool, "<glob@x>")
	if err != nil || ref == nil {
		t.Fatalf("lookup: %v %v", ref, err)
	}
	if ref.WorkspaceID != a || ref.SendID != sendID || ref.SequenceID != "seq-g" || ref.EnrollmentID == nil || *ref.EnrollmentID != 7 || ref.ToEmail != "Victim@Example.com" {
		t.Errorf("tuple %+v", ref)
	}
}

func TestGlobalLookupUnknownIsNil(t *testing.T) {
	pool := testpg.NewAppPool(t, 2)
	ref, err := outreachstore.LookupSendAnyWorkspace(context.Background(), pool, "<does-not-exist@x>")
	if err != nil || ref != nil {
		t.Fatalf("want nil,nil got %v %v", ref, err)
	}
	if ref, err := outreachstore.LookupSendAnyWorkspace(context.Background(), pool, ""); err != nil || ref != nil {
		t.Fatalf("empty: %v %v", ref, err)
	}
}

func TestCountsWindowBoundaries(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	ws := newWS(t, pool)
	now := time.Date(2026, 8, 29, 10, 40, 0, 0, time.UTC)
	restore := outreachstore.SetSendsClockForTest(func() time.Time { return now })
	defer restore()
	inWS(t, pool, ws, func(tx pgx.Tx) error {
		seedSent(t, tx, ws, "s1", "c1", "a@x.com", "<1>", "sent", now.Add(-time.Hour))                // exactly on cutoff: counted (>=)
		seedSent(t, tx, ws, "s1", "c2", "a@x.com", "<2>", "sent", now.Add(-time.Hour-time.Second))    // outside hour
		seedSent(t, tx, ws, "s1", "c3", "a@x.com", "<3>", "sent", now.Add(-24*time.Hour))             // exactly 24h: counted
		seedSent(t, tx, ws, "s1", "c4", "a@x.com", "<4>", "sent", now.Add(-24*time.Hour-time.Second)) // outside day
		seedSent(t, tx, ws, "s2", "c5", "a@x.com", "<5>", "sent", now.Add(-time.Minute))              // other sequence
		seedSent(t, tx, ws, "s1", "c6", "a@x.com", "<6>", "bounced", now.Add(-time.Minute))           // not 'sent'
		seedSent(t, tx, ws, "s1", "c7", "a@x.com", "<7>", "in_flight", now.Add(-time.Minute))
		ctx := context.Background()
		h, err := outreachstore.SentCountLastHour(ctx, tx, ws)
		if err != nil || h != 2 { // c1 + c5
			t.Errorf("hour=%d err=%v want 2", h, err)
		}
		d, err := outreachstore.SentCountToday(ctx, tx, ws, "s1")
		if err != nil || d != 2 { // c1 + c3 (rolling 24h as Python sent_count_today)
			t.Errorf("today=%d err=%v want 2", d, err)
		}
		return nil
	})
}

func TestStatsMatchFixtures(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	ws := newWS(t, pool)
	now := time.Now().UTC()
	inWS(t, pool, ws, func(tx pgx.Tx) error {
		ctx := context.Background()
		// unknown sequence: only enrollment/sends zero-fill, no counter keys
		st, err := outreachstore.SequenceStats(ctx, tx, ws, "ghost")
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]int{"pending": 0, "scheduled": 0, "sent": 0, "opened": 0, "replied": 0, "bounced": 0,
			"failed": 0, "skipped": 0, "suppressed": 0, "completed": 0, "total": 0, "emails_sent": 0}
		assertStats(t, st, want)

		_, err = tx.Exec(ctx, `INSERT INTO outreach_sequences (id, workspace_id, name, steps, status, bounce_count, complaint_count, auto_paused)
		  VALUES ('s1',$1,'n','[]','paused',3,1,true)`, ws)
		if err != nil {
			t.Fatal(err)
		}
		for i, status := range []string{"scheduled", "scheduled", "sent", "completed", "bounced", "weird"} {
			if _, err := tx.Exec(ctx, `INSERT INTO outreach_enrollments (workspace_id, sequence_id, lead_id, to_email_snapshot, status)
			  VALUES ($1,'s1',$2,'x@y.com',$3)`, ws, i+1, status); err != nil {
				t.Fatal(err)
			}
		}
		seedSent(t, tx, ws, "s1", "e1", "a@x.com", "<a>", "sent", now.Add(-100*time.Hour))
		seedSent(t, tx, ws, "s1", "e2", "a@x.com", "<b>", "sent", now)
		seedSent(t, tx, ws, "s1", "e3", "a@x.com", "<c>", "bounced", now)
		seedSent(t, tx, ws, "s9", "e4", "a@x.com", "<d>", "sent", now)
		st, err = outreachstore.SequenceStats(ctx, tx, ws, "s1")
		if err != nil {
			t.Fatal(err)
		}
		want["scheduled"], want["sent"], want["completed"], want["bounced"] = 2, 1, 1, 1
		want["total"] = 6 // sums every status including unknown ones
		want["emails_sent"] = 2
		want["bounce_count"], want["complaint_count"], want["auto_paused"] = 3, 1, 1
		assertStats(t, st, want)
		return nil
	})
}

func assertStats(t *testing.T, got, want map[string]int) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("keys: got %v want %v", got, want)
	}
	for k, v := range want {
		if g, ok := got[k]; !ok || g != v {
			t.Errorf("stats[%s]=%d(ok=%v) want %d", k, g, ok, v)
		}
	}
}

func TestListSendsOrderAndLimit(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	ws := newWS(t, pool)
	base := time.Now().UTC().Add(-time.Hour)
	inWS(t, pool, ws, func(tx pgx.Tx) error {
		ctx := context.Background()
		seedSent(t, tx, ws, "s1", "l1", "a@x.com", "<1>", "sent", base)
		seedSent(t, tx, ws, "s1", "l2", "b@x.com", "<2>", "sent", base.Add(time.Minute))
		seedSent(t, tx, ws, "s2", "l3", "c@x.com", "<3>", "sent", base.Add(2*time.Minute))
		all, err := outreachstore.ListSends(ctx, tx, ws, "", 200)
		if err != nil || len(all) != 3 || all[0].ToEmail != "c@x.com" || all[2].ToEmail != "a@x.com" {
			t.Fatalf("all: %v %+v", err, all)
		}
		one, _ := outreachstore.ListSends(ctx, tx, ws, "s1", 200)
		if len(one) != 2 || one[0].ToEmail != "b@x.com" {
			t.Errorf("s1: %+v", one)
		}
		lim, _ := outreachstore.ListSends(ctx, tx, ws, "", 1)
		if len(lim) != 1 || lim[0].ToEmail != "c@x.com" {
			t.Errorf("limit: %+v", lim)
		}
		return nil
	})
}

func TestRecentSendToAndMarkBounced(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	ws := newWS(t, pool)
	now := time.Now().UTC()
	inWS(t, pool, ws, func(tx pgx.Tx) error {
		ctx := context.Background()
		seedSent(t, tx, ws, "s", "r1", "bob@example.com", "<1>", "sent", now.Add(-time.Hour))
		seedSent(t, tx, ws, "s", "r2", "bob@example.com", "<2>", "failed", now)
		r, err := outreachstore.GetRecentSendTo(ctx, tx, ws, "  BOB@Example.com ")
		if err != nil || r == nil || r.Status != "sent" { // live status preferred over newer failed
			t.Fatalf("recent: %v %+v", err, r)
		}
		if r, _ := outreachstore.GetRecentSendTo(ctx, tx, ws, "nobody@example.com"); r != nil {
			t.Errorf("unknown recipient")
		}
		if r, _ := outreachstore.GetRecentSendTo(ctx, tx, ws, ""); r != nil {
			t.Errorf("blank recipient")
		}
		ok, err := outreachstore.MarkSendBounced(ctx, tx, ws, r.ID, "550 user unknown")
		if err != nil || !ok {
			t.Fatalf("mark: %v %v", ok, err)
		}
		var first time.Time
		_ = tx.QueryRow(ctx, "SELECT bounced_at FROM outreach_sends WHERE id=$1", r.ID).Scan(&first)
		time.Sleep(5 * time.Millisecond)
		// second call keeps bounced_at (set-idempotent), keeps error when none given
		if ok, _ := outreachstore.MarkSendBounced(ctx, tx, ws, r.ID, ""); !ok {
			t.Errorf("second mark")
		}
		var st, e string
		var second time.Time
		_ = tx.QueryRow(ctx, "SELECT status, error, bounced_at FROM outreach_sends WHERE id=$1", r.ID).Scan(&st, &e, &second)
		if st != "bounced" || e != "550 user unknown" || !second.Equal(first) {
			t.Errorf("after: %s %q %v vs %v", st, e, second, first)
		}
		if ok, _ := outreachstore.MarkSendBounced(ctx, tx, ws, 999999999, "x"); ok {
			t.Errorf("missing id must be false")
		}
		return nil
	})
}

func TestTenantIsolation(t *testing.T) {
	pool := testpg.NewAppPool(t, 4)
	a, b := newWS(t, pool), newWS(t, pool)
	inWS(t, pool, a, func(tx pgx.Tx) error {
		ctx := context.Background()
		if ok, err := outreachstore.InsertInFlight(ctx, tx, in(a, "s", "shared-key", "x@example.com")); !ok || err != nil {
			t.Fatalf("a insert %v %v", ok, err)
		}
		seedSent(t, tx, a, "s", "a-sent", "x@example.com", "<iso@x>", "sent", time.Now())
		return nil
	})
	inWS(t, pool, b, func(tx pgx.Tx) error {
		ctx := context.Background()
		// same idem key in another workspace is a distinct row
		if ok, err := outreachstore.InsertInFlight(ctx, tx, in(b, "s", "shared-key", "y@example.com")); !ok || err != nil {
			t.Fatalf("b insert %v %v", ok, err)
		}
		// B asking for A's workspace id under B's GUC sees nothing (RLS)
		if s, _ := outreachstore.GetSendByIdem(ctx, tx, a, "shared-key"); s != nil {
			t.Errorf("RLS leak: %+v", s)
		}
		if r, _ := outreachstore.GetSendByMessageID(ctx, tx, a, "<iso@x>"); r != nil {
			t.Errorf("RLS leak by message id")
		}
		if l, _ := outreachstore.ListSends(ctx, tx, a, "", 50); len(l) != 0 {
			t.Errorf("RLS leak list: %d", len(l))
		}
		if n, _ := outreachstore.SentCountLastHour(ctx, tx, a); n != 0 {
			t.Errorf("RLS leak count %d", n)
		}
		// finalize through B cannot touch A's row
		if err := outreachstore.FinalizeSend(ctx, tx, a, "shared-key", outreachstore.Finalize{Status: "failed"}); err != nil {
			t.Fatal(err)
		}
		s, _ := outreachstore.GetSendByIdem(ctx, tx, b, "shared-key")
		if s.Status != "in_flight" || s.ToEmail != "y@example.com" {
			t.Errorf("b row: %+v", s)
		}
		return nil
	})
	inWS(t, pool, a, func(tx pgx.Tx) error {
		s, _ := outreachstore.GetSendByIdem(context.Background(), tx, a, "shared-key")
		if s == nil || s.Status != "in_flight" {
			t.Errorf("a row altered: %+v", s)
		}
		return nil
	})
}
