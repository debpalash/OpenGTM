//go:build integration

package workbook_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/emailverify"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

func withRequires(wb string, reqs ...string) []byte {
	return mutate(payloadFor(wsA, wb, map[string]float64{"free1": 0}, nil), func(p map[string]any) {
		g := p["go"].(map[string]any)
		r := make([]any, len(reqs))
		for i, c := range reqs {
			r[i] = c
		}
		g["requires"] = r
	})
}

// M4, both directions: a worker without a capability the enqueuer recorded refuses the job
// before any work; a worker that has every capability runs it; a payload with no requirement
// (every flag-off run) is unaffected.
func TestRequiredCapabilitiesRefuseOrRun(t *testing.T) {
	full := func(d *workbook.Deps) {
		d.FullRun, d.Provenance = true, true
		d.Emitter = &memEmitter{}
		d.Output = &workbook.OutputPorts{}
		d.Verify = workbook.VerifyFunc(func(context.Context, string, string) (emailverify.Result, error) {
			return emailverify.Result{}, nil
		})
	}
	all := []string{"provenance", "automations", "conditions", "email_verify", "output"}
	cases := []struct {
		name  string
		reqs  []string
		wire  func(*workbook.Deps)
		hand  bool
		match string
	}{
		{"none required, flags off", nil, func(*workbook.Deps) {}, false, ""},
		{"provenance, worker flags off", []string{"provenance"}, func(*workbook.Deps) {}, true, "provenance"},
		{"automations, no emitter", []string{"automations"}, func(d *workbook.Deps) { full(d); d.Emitter = nil }, true, "automations"},
		{"provenance, worker provenance unset", []string{"provenance"}, func(d *workbook.Deps) { full(d); d.Provenance = false }, true, "provenance"},
		{"conditions, full run off", []string{"conditions"}, func(*workbook.Deps) {}, true, "conditions"},
		{"email_verify, no verifier", []string{"email_verify"}, func(d *workbook.Deps) { full(d); d.Verify = nil }, true, "email_verify"},
		{"output, no ports", []string{"output"}, func(d *workbook.Deps) { full(d); d.Output = nil }, true, "output"},
		{"unknown requirement fails closed", []string{"teleport"}, full, true, "teleport"},
		{"every capability present", all, full, false, ""},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t, wsA)
			wb := "wb-req" + string(rune('a'+i))
			e.seed(wsA, wb, "["+colJSON("c1", "free1")+"]", 1, 0)
			e.inWS(wsA, func(tx pgx.Tx) error {
				_, err := tx.Exec(e.ctx, `UPDATE workbooks SET status='running' WHERE id=$1`, wb)
				return err
			})
			s := newSim(t, nil)
			d := e.deps(s, nil)
			tc.wire(&d)
			c := e.claim(wsA, withRequires(wb, tc.reqs...))
			res, err := workbook.Run(e.ctx, d, c)
			if tc.hand {
				if !workbook.IsRefused(err) || !strings.Contains(err.Error(), tc.match) || !strings.Contains(err.Error(), "capability_unavailable") {
					t.Fatalf("want a capability refusal naming %q: %v", tc.match, err)
				}
				exec, _, _, retries, _ := e.jobRow(c.ID)
				if exec != "go" || retries != 0 {
					t.Fatalf("job row: %s retries=%d", exec, retries)
				}
				if s.total() != 0 || e.one(wsA, `SELECT count(*) FROM workbook_enrichments`) != 0 {
					t.Fatal("work happened before the refusal")
				}
				return
			}
			if err != nil || res.Completed != 1 {
				t.Fatalf("must run: %+v %v", res, err)
			}
		})
	}
}
