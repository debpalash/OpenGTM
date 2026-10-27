//go:build integration

package autoengine

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/outreachstore"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

func (e *env) legacySeq() string {
	var id string
	e.inWS(ws1, func(tx pgx.Tx) error {
		s, err := outreachstore.CreateSequence(e.ctx, tx, ws1, outreachstore.NewSeqInput("S"))
		id = s.ID
		return err
	})
	e.t.Cleanup(func() {
		e.inWS(ws1, func(tx pgx.Tx) error {
			for _, q := range []string{`DELETE FROM outreach_sends`, `DELETE FROM outreach_enrollments`, `DELETE FROM outreach_suppressions`, `DELETE FROM outreach_sequences`} {
				_, _ = tx.Exec(e.ctx, q)
			}
			return nil
		})
	})
	return id
}

func (e *env) legacyCounts() (enr, sendJobs int) {
	e.inWS(ws1, func(tx pgx.Tx) error {
		if err := tx.QueryRow(e.ctx, `SELECT count(*) FROM outreach_enrollments`).Scan(&enr); err != nil {
			return err
		}
		return tx.QueryRow(e.ctx, `SELECT count(*) FROM jobs WHERE type='send'`).Scan(&sendJobs)
	})
	return
}

// D5: an eval with sequencer and
// send_email runs fully in Go: the job stays on the Go executor, the sequencer
// enrols the rows with an email, and send_email only enqueues `send` jobs (no
// mail leaves this process; the outreachsend handler owns the send).
func TestLegacyOutreachActionsRunInGo(t *testing.T) {
	e := newEnv(t)
	seq := e.legacySeq()
	e.defaultSeed(trig{Actions: fmt.Sprintf(`[{"type":"sequencer","config":{"sequence_id":%q}},{"type":"send_email","config":{"subject":"s","body_html":"<p>x</p>"}}]`, seq)})
	// Enrollment and sends need a lead (review wave2a M4): give the Acme row
	// one (the Gamma row has lead 7; the email-less Beta row skips first).
	e.inWS(ws1, func(tx pgx.Tx) error {
		_, err := tx.Exec(e.ctx, `UPDATE workbook_rows SET lead_id=3 WHERE id=$1`, e.rowIDs[0])
		return err
	})
	e.d.Actions.Pool = e.pool
	c := e.claim(pl("fk-legacy", nil))
	if err := Handle(e.ctx, e.d, c); err != nil {
		t.Fatal(err)
	}
	if ex, _ := e.job(c.ID); ex == "python" {
		t.Fatal("job moved to python")
	}
	var ok, skipped int
	for _, r := range e.results(ws1) {
		switch r.Status {
		case "success":
			ok++
		case "skipped":
			skipped++
			if r.Skip == nil || *r.Skip != "no_email" {
				t.Fatalf("skip %+v", r)
			}
		default:
			t.Fatalf("%+v", r)
		}
	}
	// 2 rows with an email x 2 actions succeed; the row without one skips both
	if ok != 4 || skipped != 2 {
		t.Fatalf("ok %d skipped %d", ok, skipped)
	}
	if enr, jobs := e.legacyCounts(); enr != 2 || jobs != 2 {
		t.Fatalf("enrollments %d send jobs %d", enr, jobs)
	}
}

// A dry run previews only: no enrolment, no job, no send.
func TestLegacyOutreachDryRunWritesNothing(t *testing.T) {
	e := newEnv(t)
	seq := e.legacySeq()
	e.defaultSeed(trig{Actions: fmt.Sprintf(`[{"type":"sequencer","config":{"sequence_id":%q}},{"type":"send_email","config":{}}]`, seq)})
	e.d.Actions.Pool = e.pool
	if err := Handle(e.ctx, e.d, e.claim(pl("fk-legacy-dry", map[string]any{"dry_run": true}))); err != nil {
		t.Fatal(err)
	}
	if enr, jobs := e.legacyCounts(); enr != 0 || jobs != 0 {
		t.Fatalf("dry run wrote %d enrollments, %d send jobs", enr, jobs)
	}
	if runs := e.runs(ws1); len(runs) != 1 || runs[0].Status != "dry_run" {
		t.Fatalf("%+v", runs)
	}
}

// Review wave2a M4: a trigger row without a lead id is refused by sequencer and
// send_email (Python enrolled it as lead 0, shared by every lead-less row).
func TestLegacyOutreachRefusesLeadlessRow(t *testing.T) {
	e := newEnv(t)
	seq := e.legacySeq()
	e.defaultSeed(trig{Actions: fmt.Sprintf(`[{"type":"sequencer","config":{"sequence_id":%q}}]`, seq)})
	e.d.Actions.Pool = e.pool
	if err := Handle(e.ctx, e.d, e.claim(pl("fk-legacy-nolead", nil))); err != nil {
		t.Fatal(err)
	}
	var failed int
	for _, r := range e.results(ws1) {
		if r.Status == "failed" {
			failed++
		}
	}
	// Acme has an email and no lead: failed. Beta has no email: skipped. Gamma has lead 7: enrolled.
	if failed != 1 {
		t.Fatalf("failed results %d", failed)
	}
	if enr, _ := e.legacyCounts(); enr != 1 {
		t.Fatalf("enrollments %d", enr)
	}
}
