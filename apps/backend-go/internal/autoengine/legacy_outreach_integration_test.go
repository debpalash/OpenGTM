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

// D5: with GO_EXECUTOR_LEGACY_OUTREACH_ACTIONS on, an eval with sequencer and
// send_email runs fully in Go: the job stays on the Go executor, the sequencer
// enrols the rows with an email, and send_email only enqueues `send` jobs (no
// mail leaves this process; the outreachsend handler owns the send).
func TestLegacyOutreachActionsRunInGo(t *testing.T) {
	e := newEnv(t)
	seq := e.legacySeq()
	e.defaultSeed(trig{Actions: fmt.Sprintf(`[{"type":"sequencer","config":{"sequence_id":%q}},{"type":"send_email","config":{"subject":"s","body_html":"<p>x</p>"}}]`, seq)})
	e.d.LegacyOutreach = true
	e.d.Actions.LegacyOutreach = true
	e.d.Actions.Pool = e.pool
	c := e.claim(pl("fk-legacy", nil))
	if err := Handle(e.ctx, e.d, c); err != nil {
		if errors.Is(err, workbook.ErrHandedBack) {
			t.Fatal("handed back with the flag on")
		}
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

// The flag defaults off: the legacy actions still hand back (TestLegacyOutreachActionHandsBack).
// A dry run with the flag on previews only: no enrolment, no job, no send.
func TestLegacyOutreachDryRunWritesNothing(t *testing.T) {
	e := newEnv(t)
	seq := e.legacySeq()
	e.defaultSeed(trig{Actions: fmt.Sprintf(`[{"type":"sequencer","config":{"sequence_id":%q}},{"type":"send_email","config":{}}]`, seq)})
	e.d.LegacyOutreach = true
	e.d.Actions.LegacyOutreach = true
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
