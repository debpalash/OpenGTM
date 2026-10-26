//go:build integration

package mcp

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// jobOutreach is a fake OutreachPort whose QueueSend writes a row through the
// pipeline's transaction (a workbook row stands in for the durable send job).
type jobOutreach struct {
	fakeOutreach
	n int
}

func (j *jobOutreach) QueueSend(ctx context.Context, tx pgx.Tx, ws string, r QueueSendRequest) (SendResult, error) {
	j.n++
	if _, err := tx.Exec(ctx, `INSERT INTO workbooks (id, name, workspace_id) VALUES ($1,'job',$2)`,
		ws+"/job"+string(rune('0'+j.n)), ws); err != nil {
		return SendResult{}, err
	}
	return j.fakeOutreach.QueueSend(ctx, tx, ws, r)
}

func outreachServer(e *wEnv, o OutreachPort) *Server {
	d := e.d
	d.Leads = outreachFixtureLeads()
	d.Outreach = o
	return e.server(outreachWriteTools(d)...)
}

func TestSendEmailQueuesInSameTransaction(t *testing.T) {
	e := newWEnv(t, "admin")
	e.cfg.AutomationsAllowLegacyOutreach = true
	o := &jobOutreach{fakeOutreach: fakeOutreach{SendRes: SendResult{Status: "success", Summary: "send enqueued"}}}
	s := outreachServer(e, o)
	if got := e.call(s, "send_email", Args{"lead_id": 1.0, "subject": "s"}); got != `{"status": "success", "summary": "send enqueued"}` {
		t.Fatal(got)
	}
	if e.workbooks(t) != 1 {
		t.Fatal("job must commit with the pipeline transaction")
	}
	// a failed send result rolls the enqueue back: no job survives
	o.SendRes = SendResult{Status: "failed", SkipReason: "not_connected", Error: "sequence not found in workspace"}
	got := e.call(s, "send_email", Args{"lead_id": 1.0, "sequence_id": "nope"})
	if got != fixtureText(t, "writes__send_email_sequence_not_found.json") {
		t.Fatal(got)
	}
	if e.workbooks(t) != 1 || e.held(t) != 0 {
		t.Fatalf("rollback left a job or a held slot: wb=%d held=%d", e.workbooks(t), e.held(t))
	}
	e.wantAudit(t, 2, "error", "sequence not found in workspace")
}

func TestSendEmailDisabledGateAudited(t *testing.T) {
	e := newWEnv(t, "admin")
	o := &jobOutreach{}
	s := outreachServer(e, o)
	if got := e.call(s, "send_email", Args{"lead_id": 1.0, "subject": "x"}); got != fixtureText(t, "outreach_off__outreach_off_send_email.json") {
		t.Fatal(got)
	}
	e.wantAudit(t, 1, "denied", "legacy_outreach_disabled")
	if o.n != 0 || e.workbooks(t) != 0 || e.held(t) != 0 {
		t.Fatal("a gated send must not queue or reserve")
	}
}

func TestSendEmailIdempotentReplayNoSecondJob(t *testing.T) {
	e := newWEnv(t, "admin")
	e.cfg.AutomationsAllowLegacyOutreach = true
	o := &jobOutreach{fakeOutreach: fakeOutreach{SendRes: SendResult{Status: "success", Summary: "send enqueued"}}}
	s := outreachServer(e, o)
	args := Args{"lead_id": 1.0, "subject": "Hello", "body_html": "<p>hi</p>", "idempotency_key": "k-send-1"}
	if got := e.call(s, "send_email", args); got != fixtureText(t, "writes__send_email.json") {
		t.Fatal(got)
	}
	if got := e.call(s, "send_email", args); got != fixtureText(t, "writes__send_email_replay_duplicate.json") {
		t.Fatal(got)
	}
	if o.n != 1 || len(o.Sent) != 1 || e.workbooks(t) != 1 {
		t.Fatalf("replay queued again: n=%d wb=%d", o.n, e.workbooks(t))
	}
}
