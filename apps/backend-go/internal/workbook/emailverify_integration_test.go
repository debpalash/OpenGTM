//go:build integration

package workbook_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/emailverify"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/workbook"
)

// emailManifest maps provider pe to the email field; the harness serves it through Deps.Manifests.
const emailManifest = `{"name":"pe","capability":"email","default_confidence":0.8,"auth":{"type":"none"},
  "request":{"method":"POST","url":"http://sim.provider.example/pe","timeout":5,"body_template":{"company":"{{input.company}}"}},
  "response":{"mappings":{"email":"$.v"}}}`

func emailPayload(wb string) []byte { return payloadFor(wsA, wb, map[string]float64{"pe": 0}, nil) }

func emailDeps(e *env, s *sim, v workbook.VerifyFunc) workbook.Deps {
	d := e.deps(s, nil)
	d.Manifests = manifestSrv{body: map[string]string{"pe": emailManifest}}
	d.Verify = v
	return d
}

type verifyLog struct {
	mu     sync.Mutex
	emails []string
}

func (l *verifyLog) fn(status string) workbook.VerifyFunc {
	return func(_ context.Context, _, email string) (emailverify.Result, error) {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.emails = append(l.emails, email)
		return emailverify.Result{Email: email, Status: status, Source: "test", Confidence: 0.5}, nil
	}
}

const (
	emailCol    = `{"id":"c_email","type":"waterfall","name":"Email","target_field":"email","verify":false,"waterfall":["pe"]}`
	verifyCol   = `{"id":"c_ver","type":"waterfall","name":"Email Verify","target_field":"email_verify","input_columns":["c_email"]}`
	verifyColNo = `{"id":"c_ver","type":"waterfall","name":"Email Verify","target_field":"email_verify"}`
)

// Email Verify takes the row's Email cell as input and stores the verifier's real answer. The old behaviour read
// lead.email, which the run never writes, so the column ended `no_data` in 1 ms for every row.
func TestEmailVerifyColumnVerifiesTheRowsEmailCell(t *testing.T) {
	for name, ver := range map[string]string{"explicit input_columns": verifyCol, "inferred from the Email column": verifyColNo} {
		t.Run(name, func(t *testing.T) {
			e := setup(t, wsA)
			// The verify column is declared BEFORE the email column: the dependency graph must still run it after.
			e.seed(wsA, "wb-ver", "["+ver+","+emailCol+"]", 1, 10)
			s := newSim(t, func(_ string, _ int, w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, `{"v":"info@acme0.io"}`)
			})
			vl := &verifyLog{}
			deps := emailDeps(e, s, vl.fn("catch_all"))
			c := e.claim(wsA, emailPayload("wb-ver"))
			if _, err := workbook.Run(e.ctx, deps, c); err != nil {
				t.Fatal(err)
			}
			if got := e.str(wsA, `SELECT value FROM workbook_enrichments WHERE column_id='c_email'`); got != "info@acme0.io" {
				t.Fatalf("email cell %q", got)
			}
			if len(vl.emails) != 1 || vl.emails[0] != "info@acme0.io" {
				t.Fatalf("the verifier must be asked about the row's email, even a guessed info@ one: %v", vl.emails)
			}
			if got := e.str(wsA, `SELECT value FROM workbook_enrichments WHERE column_id='c_ver'`); got != "catch_all" {
				t.Fatalf("the verify cell must hold the verifier's real answer, got %q", got)
			}
			if st := e.str(wsA, `SELECT status FROM workbook_enrichments WHERE column_id='c_ver'`); st != "complete" {
				t.Fatalf("status %q", st)
			}
		})
	}
}

func TestEmailVerifyColumnWithoutAnEmailSaysSo(t *testing.T) {
	e := setup(t, wsA)
	e.seed(wsA, "wb-ver0", "["+emailCol+","+verifyCol+"]", 1, 10)
	s := newSim(t, func(_ string, _ int, w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{}`) }) // no email found
	vl := &verifyLog{}
	deps := emailDeps(e, s, vl.fn("valid"))
	c := e.claim(wsA, emailPayload("wb-ver0"))
	if _, err := workbook.Run(e.ctx, deps, c); err != nil {
		t.Fatal(err)
	}
	if len(vl.emails) != 0 {
		t.Fatalf("nothing to verify, but the verifier was called with %v", vl.emails)
	}
	if got := e.str(wsA, `SELECT error FROM workbook_enrichments WHERE column_id='c_ver'`); got != "upstream_dependency_failed" && got != "no_email_to_verify" {
		t.Fatalf("a missing email must give a specific reason, got %q", got)
	}
}

func (l *verifyLog) withDetail(status, detail string) workbook.VerifyFunc {
	return func(_ context.Context, _, email string) (emailverify.Result, error) {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.emails = append(l.emails, email)
		return emailverify.Result{Email: email, Status: status, Source: "smtp", Detail: detail, Why: "rcpt_soft:451"}, nil
	}
}

func runVerify(t *testing.T, wb string, v workbook.VerifyFunc) (workbook.Result, *env) {
	t.Helper()
	e := setup(t, wsA)
	e.seed(wsA, wb, "["+emailCol+","+verifyCol+"]", 1, 10)
	s := newSim(t, func(_ string, _ int, w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"v":"info@acme0.io"}`) })
	c := e.claim(wsA, emailPayload(wb))
	res, err := workbook.Run(e.ctx, emailDeps(e, s, v), c)
	if err != nil {
		t.Fatal(err)
	}
	return res, e
}

// A verdict of "unknown" is not information: the cell holds no value, says why, and does not count as filled.
func TestEmailVerifyUnknownIsNotAFill(t *testing.T) {
	vl := &verifyLog{}
	res, e := runVerify(t, "wb-ver-unk", vl.withDetail("unknown", "soft"))
	if res.Completed != 1 || res.NoData != 1 || res.Errors != 0 { // 1 filled: the Email cell; 1 no data: the verify cell
		t.Fatalf("counts %+v", res)
	}
	if v := e.str(wsA, `SELECT coalesce(value,'') FROM workbook_enrichments WHERE column_id='c_ver'`); v != "" {
		t.Fatalf("an unknown verdict stored the value %q", v)
	}
	got := e.str(wsA, `SELECT error FROM workbook_enrichments WHERE column_id='c_ver'`)
	if !strings.HasPrefix(got, "email_inconclusive:") || !strings.Contains(got, "rcpt_soft:451") {
		t.Fatalf("error %q should say the mail server gave no clear answer and where it stopped", got)
	}
}

// When this machine cannot reach port 25 the cell says exactly that and points at a verification provider key, and
// the run counts it as needing setup, not as an error and not as a fill.
func TestEmailVerifyPort25BlockedNeedsSetup(t *testing.T) {
	vl := &verifyLog{}
	res, e := runVerify(t, "wb-ver-p25", vl.withDetail("unknown", emailverify.DetailPort25Blocked))
	if res.NeedsSetup != 1 || res.Errors != 0 || res.Completed != 1 {
		t.Fatalf("counts %+v", res)
	}
	got := e.str(wsA, `SELECT error FROM workbook_enrichments WHERE column_id='c_ver'`)
	want := "smtp_port_blocked: SMTP port 25 is blocked on this server, so addresses cannot be verified; use a verification provider key"
	if got != want {
		t.Fatalf("error %q\nwant  %q", got, want)
	}
}

func TestEmailVerifyWithNothingToAskNeedsSetup(t *testing.T) {
	vl := &verifyLog{}
	res, e := runVerify(t, "wb-ver-nov", vl.withDetail("unknown", emailverify.DetailNoVerifier))
	if res.NeedsSetup != 1 || res.Errors != 0 {
		t.Fatalf("counts %+v", res)
	}
	if got := e.str(wsA, `SELECT error FROM workbook_enrichments WHERE column_id='c_ver'`); !strings.Contains(got, "use a verification provider key") {
		t.Fatalf("error %q", got)
	}
}

// A timeout or an open breaker is an infrastructure failure: an error worth retrying, not "no data".
func TestEmailVerifyInfrastructureFailureIsAnError(t *testing.T) {
	vl := &verifyLog{}
	res, _ := runVerify(t, "wb-ver-err", vl.withDetail("unknown", "timeout"))
	if res.Errors != 1 {
		t.Fatalf("counts %+v", res)
	}
}
