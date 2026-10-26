package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func osp(s string) *string { return &s }

// outreachFixtureLeads mirrors the recorded workspace: 1 and 2 have an email,
// 3 has none, 4 is suppressed; 7 and 999 do not exist in this workspace.
func outreachFixtureLeads() *fakeLeads {
	return &fakeLeads{Rows: map[int64]*Lead{
		1: {ID: 1, Company: "Acme", Email: osp("one@acme.test")},
		2: {ID: 2, Company: "Beta", Email: osp("Two@Beta.TEST ")},
		3: {ID: 3, Company: "NoMail"},
		4: {ID: 4, Company: "Sup", Email: osp("sup@x.test")},
	}}
}

func outreachTool(t *testing.T, d Deps, name string) WriteTool {
	t.Helper()
	for _, w := range outreachWriteTools(d) {
		if w.Name == name {
			return w
		}
	}
	t.Fatalf("tool %s missing", name)
	return WriteTool{}
}

func runOutreach(t *testing.T, d Deps, name string, a Args) string {
	t.Helper()
	got, err := outreachTool(t, d, name).Apply(context.Background(), nil, Ctx{WorkspaceID: "ws-a"}, a)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func enrollDeps(o *fakeOutreach) Deps {
	return Deps{Leads: outreachFixtureLeads(), Outreach: o}
}

func TestOutreachToolMetadata(t *testing.T) {
	d := Deps{}
	e, s := outreachTool(t, d, "enroll_leads"), outreachTool(t, d, "send_email")
	if e.Capability != "sequences:enroll" || len(e.Roles) != 3 || e.Gate != nil {
		t.Fatalf("%+v", e)
	}
	if s.Capability != "outreach:send" || len(s.Roles) != 1 || s.Roles[0] != "admin" {
		t.Fatalf("%+v", s)
	}
	if s.Gate(Config{}) != "legacy_outreach_disabled" {
		t.Fatal("gate must refuse by default")
	}
	cfg := Config{}
	cfg.AutomationsAllowLegacyOutreach = true
	if s.Gate(cfg) != "" {
		t.Fatal("gate must open with AUTOMATIONS_ALLOW_LEGACY_OUTREACH")
	}
}

func TestEnrollLeadsMatchesFixture(t *testing.T) {
	o := &fakeOutreach{Sequences: map[string]bool{"seq-alpha-1": true}, Suppressed: map[string]bool{"sup@x.test": true}}
	got := runOutreach(t, enrollDeps(o), "enroll_leads",
		Args{"sequence_id": "seq-alpha-1", "lead_ids": []any{1.0, 2.0, 3.0, 4.0, 999.0, 7.0}})
	if want := fixtureText(t, "writes__enroll_leads.json"); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if !o.Enrolled["seq-alpha-1/1"] || !o.Enrolled["seq-alpha-1/2"] || len(o.Enrolled) != 2 {
		t.Fatalf("%v", o.Enrolled)
	}
}

func TestEnrollSkipsNoEmailSuppressedAndDuplicate(t *testing.T) {
	o := &fakeOutreach{Sequences: map[string]bool{"seq-alpha-1": true}, Suppressed: map[string]bool{"sup@x.test": true}}
	d := enrollDeps(o)
	args := Args{"sequence_id": "seq-alpha-1", "lead_ids": []any{1.0, 2.0}}
	runOutreach(t, d, "enroll_leads", args)
	got := runOutreach(t, d, "enroll_leads", args)
	if want := fixtureText(t, "writes__enroll_leads_again_already_enrolled.json"); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	// the email handed to the port is normalized and the consent source is fixed
	var gotEmail, gotSrc string
	d.Outreach = &enrollSpy{fakeOutreach: o, email: &gotEmail, src: &gotSrc}
	runOutreach(t, d, "enroll_leads", Args{"sequence_id": "seq-alpha-1", "lead_ids": []any{2.0}})
	if gotEmail != "two@beta.test" || gotSrc != "mcp_enroll" {
		t.Fatal(gotEmail, gotSrc)
	}
}

type enrollSpy struct {
	*fakeOutreach
	email, src *string
}

func (s *enrollSpy) Enroll(ctx context.Context, tx0 pgx.Tx, ws, seq string, lead int64, email, src string) (bool, error) {
	*s.email, *s.src = email, src
	return s.fakeOutreach.Enroll(ctx, tx0, ws, seq, lead, email, src)
}

func TestEnrollUnknownSequenceNotFound(t *testing.T) {
	o := &fakeOutreach{Sequences: map[string]bool{"seq-alpha-1": true}}
	d := enrollDeps(o)
	want := fixtureText(t, "writes__enroll_leads_sequence_not_found.json")
	if got := runOutreach(t, d, "enroll_leads", Args{"sequence_id": "nope", "lead_ids": []any{1.0}}); got != want {
		t.Fatal(got)
	}
	if got := runOutreach(t, d, "enroll_leads", Args{"lead_ids": []any{1.0}}); got != fixtureText(t, "writes__enroll_leads_missing_sequence.json") {
		t.Fatal(got)
	}
	if len(o.Enrolled) != 0 {
		t.Fatal("must not enroll into a missing sequence")
	}
	empty := runOutreach(t, d, "enroll_leads", Args{"sequence_id": "seq-alpha-1", "lead_ids": []any{}})
	if empty != fixtureText(t, "writes__enroll_leads_empty.json") {
		t.Fatal(empty)
	}
}

func TestEnrollCrossTenantLeadSkipped(t *testing.T) {
	o := &fakeOutreach{Sequences: map[string]bool{"s": true}}
	// lead 50 belongs to another workspace: the scoped port does not return it
	got := runOutreach(t, enrollDeps(o), "enroll_leads", Args{"sequence_id": "s", "lead_ids": []any{50.0}})
	if got != `{"enrolled": 0, "skipped": [{"lead_id": 50, "reason": "no_email"}], "total_lead_ids": 1}` {
		t.Fatal(got)
	}
	if len(o.Enrolled) != 0 {
		t.Fatal("enrolled a foreign lead")
	}
}

func TestEnrollTooManyIdsRejected(t *testing.T) {
	o := &fakeOutreach{Sequences: map[string]bool{"s": true}}
	ids := make([]any, 1001)
	for i := range ids {
		ids[i] = float64(i + 1)
	}
	d := enrollDeps(o)
	if got := runOutreach(t, d, "enroll_leads", Args{"sequence_id": "s", "lead_ids": ids}); got != `{"error": "lead_ids must contain at most 1000 ids"}` {
		t.Fatal(got)
	}
	if got := runOutreach(t, d, "enroll_leads", Args{"sequence_id": "s", "lead_ids": ids[:1000]}); !strings.Contains(got, `"total_lead_ids": 1000`) {
		t.Fatal(got)
	}
	for _, bad := range []any{"1,2", []any{"1"}, []any{1.5}, []any{true}} {
		got := runOutreach(t, d, "enroll_leads", Args{"sequence_id": "s", "lead_ids": bad})
		if got != `{"error": "lead_ids must be an array of integers"}` {
			t.Fatalf("%v -> %s", bad, got)
		}
	}
	if got := runOutreach(t, d, "enroll_leads", Args{"sequence_id": 5.0, "lead_ids": []any{}}); got != `{"error": "sequence_id must be a string"}` {
		t.Fatal(got)
	}
}

func sendDeps(o *fakeOutreach) Deps { return Deps{Leads: outreachFixtureLeads(), Outreach: o} }

func TestSendEmailResultShape(t *testing.T) {
	o := &fakeOutreach{SendRes: SendResult{Status: "success", Summary: "send enqueued"}}
	d := sendDeps(o)
	got := runOutreach(t, d, "send_email", Args{"lead_id": 1.0, "subject": "Hello", "body_html": "<p>hi</p>", "idempotency_key": "k-send-1"})
	if got != fixtureText(t, "writes__send_email.json") {
		t.Fatal(got)
	}
	r := o.Sent[0]
	if r.LeadID != 1 || r.Subject != "Hello" || r.BodyHTML != "<p>hi</p>" || r.IdempotencyKey != "k-send-1" || r.SequenceID != "" || r.StepNumber != 0 {
		t.Fatalf("%+v", r)
	}
	if r.LeadData["email"] != "one@acme.test" || r.LeadData["company"] != "Acme" || len(r.LeadData) != 2 {
		t.Fatalf("lead data %v", r.LeadData)
	}

	o.SendRes = SendResult{Status: "success", Summary: "send enqueued"}
	got = runOutreach(t, d, "send_email", Args{"lead_id": 2.0, "sequence_id": "seq-alpha-1", "step_number": 1.0, "subject": "Step", "body_html": "x"})
	if got != fixtureText(t, "writes__send_email_with_sequence_step.json") {
		t.Fatal(got)
	}
	if r := o.Sent[1]; r.SequenceID != "seq-alpha-1" || r.StepNumber != 1 {
		t.Fatalf("%+v", r)
	}

	o.SendRes = SendResult{Status: "failed", SkipReason: "not_connected", Error: "sequence not found in workspace"}
	got = runOutreach(t, d, "send_email", Args{"lead_id": 2.0, "sequence_id": "nope"})
	if got != fixtureText(t, "writes__send_email_sequence_not_found.json") {
		t.Fatal(got)
	}
	if got := runOutreach(t, d, "send_email", Args{}); got != fixtureText(t, "writes__send_email_missing_lead_id.json") {
		t.Fatal(got)
	}
	if got := runOutreach(t, d, "send_email", Args{"lead_id": "x"}); got != `{"error": "lead_id must be an integer"}` {
		t.Fatal(got)
	}
}

func TestSendEmailCrossTenantLeadSkippedNeverSends(t *testing.T) {
	o := &fakeOutreach{SendRes: SendResult{Status: "success", Summary: "send enqueued"}}
	d := sendDeps(o)
	for name, id := range map[string]float64{"writes__send_email_unknown_lead.json": 999, "writes__send_email_no_email_skipped.json": 3} {
		if got := runOutreach(t, d, "send_email", Args{"lead_id": id, "subject": "x"}); got != fixtureText(t, name) {
			t.Fatalf("%s: %s", name, got)
		}
	}
	if len(o.Sent) != 0 {
		t.Fatalf("queued a send for a lead with no visible email: %+v", o.Sent)
	}
}

func TestSendEmailBodyLimits(t *testing.T) {
	o := &fakeOutreach{SendRes: SendResult{Status: "success", Summary: "send enqueued"}}
	d := sendDeps(o)
	ok := func(a Args) bool { return strings.Contains(runOutreach(t, d, "send_email", a), `"success"`) }
	if !ok(Args{"lead_id": 1.0, "subject": strings.Repeat("é", 998), "body_html": strings.Repeat("a", 200*1024)}) {
		t.Fatal("limits must be inclusive")
	}
	if got := runOutreach(t, d, "send_email", Args{"lead_id": 1.0, "subject": strings.Repeat("a", 999)}); got != `{"error": "subject must be at most 998 characters"}` {
		t.Fatal(got)
	}
	if got := runOutreach(t, d, "send_email", Args{"lead_id": 1.0, "body_html": strings.Repeat("a", 200*1024+1)}); got != `{"error": "body_html must be at most 204800 bytes"}` {
		t.Fatal(got)
	}
	for _, a := range []Args{{"lead_id": 1.0, "subject": 5.0}, {"lead_id": 1.0, "body_html": []any{}}, {"lead_id": 1.0, "step_number": "1"}, {"lead_id": 1.0, "step_number": -1.0}} {
		if got := runOutreach(t, d, "send_email", a); !strings.HasPrefix(got, `{"error": "`) {
			t.Fatalf("%v -> %s", a, got)
		}
	}
	if len(o.Sent) != 1 {
		t.Fatalf("rejected input must never reach the port: %d", len(o.Sent))
	}
}

func TestOutreachToolsUnavailableWithoutPort(t *testing.T) {
	d := Deps{Leads: outreachFixtureLeads()}
	if got := runOutreach(t, d, "enroll_leads", Args{"sequence_id": "s", "lead_ids": []any{1.0}}); got != `{"error": "enroll_leads is not available in this build"}` {
		t.Fatal(got)
	}
	if got := runOutreach(t, d, "send_email", Args{"lead_id": 1.0}); got != `{"error": "send_email is not available in this build"}` {
		t.Fatal(got)
	}
	// a port that reports ErrUnavailable answers the same text
	d.Outreach = &fakeOutreach{Err: ErrUnavailable}
	if got := runOutreach(t, d, "enroll_leads", Args{"sequence_id": "s", "lead_ids": []any{1.0}}); got != `{"error": "enroll_leads is not available in this build"}` {
		t.Fatal(got)
	}
	if got := runOutreach(t, d, "send_email", Args{"lead_id": 1.0}); got != `{"error": "send_email is not available in this build"}` {
		t.Fatal(got)
	}
}

func TestOutreachPortErrorIsReturnedForMasking(t *testing.T) {
	boom := errors.New("pq: secret detail")
	d := Deps{Leads: outreachFixtureLeads(), Outreach: &fakeOutreach{Sequences: map[string]bool{"s": true}, Err: boom}}
	if _, err := outreachTool(t, d, "enroll_leads").Apply(context.Background(), nil, Ctx{}, Args{"sequence_id": "s", "lead_ids": []any{1.0}}); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if _, err := outreachTool(t, d, "send_email").Apply(context.Background(), nil, Ctx{}, Args{"lead_id": 1.0}); !errors.Is(err, boom) {
		t.Fatal(err)
	}
}

func TestSendEmailRejectsLineBreaksInSubject(t *testing.T) {
	o := &fakeOutreach{SendRes: SendResult{Status: "success", Summary: "send enqueued"}}
	d := sendDeps(o)
	for _, s := range []string{"a\r\nBcc: x@y.test", "a\nb", "a\rb", "a\x00b"} {
		if got := runOutreach(t, d, "send_email", Args{"lead_id": 1.0, "subject": s}); got != `{"error": "subject must not contain line breaks"}` {
			t.Fatalf("%q -> %s", s, got)
		}
	}
	if len(o.Sent) != 0 {
		t.Fatal("header-injection subject reached the port")
	}
}

func TestSendEmailNormalizesLeadEmailAndRejectsBreaks(t *testing.T) {
	o := &fakeOutreach{SendRes: SendResult{Status: "success", Summary: "send enqueued"}}
	d := sendDeps(o)
	runOutreach(t, d, "send_email", Args{"lead_id": 2.0}) // "Two@Beta.TEST "
	if o.Sent[0].LeadData["email"] != "two@beta.test" {
		t.Fatalf("%v", o.Sent[0].LeadData)
	}
	l := d.Leads.(*fakeLeads)
	l.Rows[5] = &Lead{ID: 5, Email: osp("a@b.test\r\nBcc: x@y.test")}
	l.Rows[6] = &Lead{ID: 6, Email: osp("   ")}
	for _, id := range []float64{5, 6} {
		if got := runOutreach(t, d, "send_email", Args{"lead_id": id}); got != fixtureText(t, "writes__send_email_no_email_skipped.json") {
			t.Fatal(got)
		}
	}
	if len(o.Sent) != 1 {
		t.Fatal("unusable email reached the port")
	}
}

func TestSendEmailOmitsEmptyCompany(t *testing.T) {
	o := &fakeOutreach{SendRes: SendResult{Status: "success", Summary: "send enqueued"}}
	d := sendDeps(o)
	d.Leads.(*fakeLeads).Rows[8] = &Lead{ID: 8, Email: osp("x@y.test")}
	runOutreach(t, d, "send_email", Args{"lead_id": 8.0})
	if _, has := o.Sent[0].LeadData["company"]; has {
		t.Fatalf("%v", o.Sent[0].LeadData)
	}
}

func TestOutreachIDLengthCaps(t *testing.T) {
	o := &fakeOutreach{Sequences: map[string]bool{"s": true}, SendRes: SendResult{Status: "success", Summary: "x"}}
	d := sendDeps(o)
	long := strings.Repeat("a", 256)
	cases := []struct {
		tool string
		a    Args
		want string
	}{
		{"send_email", Args{"lead_id": 1.0, "sequence_id": long}, `{"error": "sequence_id must be at most 255 characters"}`},
		{"send_email", Args{"lead_id": 1.0, "idempotency_key": long}, `{"error": "idempotency_key must be at most 255 characters"}`},
		{"enroll_leads", Args{"sequence_id": long, "lead_ids": []any{}}, `{"error": "sequence_id must be at most 255 characters"}`},
	}
	for _, c := range cases {
		if got := runOutreach(t, d, c.tool, c.a); got != c.want {
			t.Fatalf("%s: %s", c.tool, got)
		}
	}
	if len(o.Sent) != 0 {
		t.Fatal("overlong id reached the port")
	}
}

func TestNormalizeEmailMatchesPythonIDNA2003(t *testing.T) {
	for in, want := range map[string]string{
		"A@Straße.DE":    "a@strasse.de",
		" U@Example.COM": "u@example.com",
		"no-at":          "no-at",
		"":               "",
	} {
		if got := normalizeEmail(in); got != want {
			t.Fatalf("%q -> %q, want %q", in, got, want)
		}
	}
}
