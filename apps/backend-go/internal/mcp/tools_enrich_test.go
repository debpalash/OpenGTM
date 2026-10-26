package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/pyworker"
)

// enrichRig wires fakes and the audit/cap seams so no database is needed.
type enrichRig struct {
	d        Deps
	enrich   *fakeEnrich
	leads    *fakeLeads
	mu       sync.Mutex
	audits   []AuditOutcome
	auditArg []Args
	reserved []string
	settled  int
	released int
	capHit   bool
}

func newEnrichRig(t *testing.T, cfg Config) *enrichRig {
	t.Helper()
	r := &enrichRig{enrich: &fakeEnrich{PyOut: map[string]json.RawMessage{}}, leads: &fakeLeads{Rows: map[int64]*Lead{1: {ID: 1, Company: "Acme"}}}}
	r.d = Deps{InWorkspace: nilTxRunner, Cfg: cfg, Leads: r.leads, Enrich: r.enrich}
	oR, oS, oL, oA := reserveCap, settleCap, releaseCap, recordAudit
	t.Cleanup(func() { reserveCap, settleCap, releaseCap, recordAudit = oR, oS, oL, oA })
	reserveCap = func(_ context.Context, _ pgx.Tx, _, rule string, cap int, _ string) (Reservation, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.reserved = append(r.reserved, rule)
		if r.capHit {
			return Reservation{Reason: "cap"}, nil
		}
		return Reservation{OK: true}, nil
	}
	settleCap = func(context.Context, pgx.Tx, Reservation) error { r.mu.Lock(); r.settled++; r.mu.Unlock(); return nil }
	releaseCap = func(context.Context, pgx.Tx, Reservation) error { r.mu.Lock(); r.released++; r.mu.Unlock(); return nil }
	recordAudit = func(_ context.Context, _ Deps, _ Ctx, _ string, args Args, o AuditOutcome) {
		r.mu.Lock()
		r.audits = append(r.audits, o)
		r.auditArg = append(r.auditArg, args)
		r.mu.Unlock()
	}
	return r
}

func (r *enrichRig) tool(t *testing.T, name string) Tool {
	t.Helper()
	for _, tl := range enrichTools(r.d) {
		if tl.Name == name {
			if tl.Capability != "leads:read" {
				t.Fatalf("%s capability %q", name, tl.Capability)
			}
			return tl
		}
	}
	t.Fatalf("tool %s missing", name)
	return Tool{}
}

func (r *enrichRig) call(t *testing.T, name string, args Args) (string, error) {
	return r.tool(t, name).Handler(context.Background(), Ctx{WorkspaceID: "ws-a", Capabilities: map[string]bool{"leads:read": true}}, args)
}

func (r *enrichRig) pyOut(provider, fields string) {
	r.enrich.PyOut[provider] = json.RawMessage(`{"provider":"` + provider + `","success":true,"fields":` + fields + `}`)
}

func TestEnrichToolNames(t *testing.T) {
	var names []string
	for _, tl := range enrichTools(Deps{}) {
		names = append(names, tl.Name)
	}
	if strings.Join(names, ",") != "verify_email,enrich_company,get_hiring_signals,score_lead" {
		t.Fatal(names)
	}
}

func TestVerifyEmailResultMatchesFixture(t *testing.T) {
	r := newEnrichRig(t, Config{})
	r.enrich.Verify = VerifyResult{Status: "valid", Deliverable: true, Confidence: 0.97, Source: "stub-verifier", Detail: "mailbox exists"}
	got, err := r.call(t, "verify_email", Args{"email": "info@acme.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	want := fixtureText(t, "read__verify_email.json")
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	r.enrich.Verify = VerifyResult{Status: "catch_all", Deliverable: true, Confidence: 0.5, Source: "stub-verifier", Detail: "accepts any"}
	got, _ = r.call(t, "verify_email", Args{"email": "x@acme.example.com"})
	if got != fixtureText(t, "read__verify_email_catch_all.json") {
		t.Fatalf("catch_all: %q", got)
	}
}

func TestVerifyEmailBadArgs(t *testing.T) {
	r := newEnrichRig(t, Config{})
	cases := map[string]struct {
		args Args
		want string
	}{
		"missing": {Args{}, "email is required"},
		"null":    {Args{"email": nil}, "email is required"},
		"number":  {Args{"email": 5.0}, "email must be a string"},
		"long":    {Args{"email": strings.Repeat("a", 321)}, "email must be at most 320 characters"},
	}
	for n, c := range cases {
		got, err := r.call(t, "verify_email", c.args)
		if err != nil || got != `{"error": "`+c.want+`"}` {
			t.Fatalf("%s: %q err=%v", n, got, err)
		}
	}
	if len(r.enrich.Calls) != 0 || len(r.audits) != 0 {
		t.Fatal("bad args must not spend or audit")
	}
	if got, _ := r.call(t, "verify_email", Args{"email": strings.Repeat("a", 320)}); strings.Contains(got, "at most") {
		t.Fatalf("320 chars is allowed: %q", got)
	}
}

func TestVerifyEmailFailureIsGeneric(t *testing.T) {
	r := newEnrichRig(t, Config{})
	r.enrich.Err = errors.New("dial tcp 10.1.2.3:25: secret-host refused")
	got, err := r.call(t, "verify_email", Args{"email": "x@acme.example.com"})
	if err != nil || got != `{"email": "x@acme.example.com", "error": "enrichment failed"}` {
		t.Fatalf("%q %v", got, err)
	}
}

func TestEnrichCompanyMissesCrossTenantLeadBeforeSpending(t *testing.T) {
	r := newEnrichRig(t, Config{MaxEnrichPerDay: 5})
	r.pyOut("mcp.enrich_company", `{"enriched_fields":{}}`)
	for _, id := range []float64{7, 999} {
		got, err := r.call(t, "enrich_company", Args{"lead_id": id})
		if err != nil || got != `{"error": "Lead not found"}` {
			t.Fatalf("%q %v", got, err)
		}
	}
	if len(r.enrich.Calls) != 0 || len(r.reserved) != 0 {
		t.Fatalf("spent before lead check: %v %v", r.enrich.Calls, r.reserved)
	}
}

func TestEnrichCompanyResultShape(t *testing.T) {
	r := newEnrichRig(t, Config{})
	r.pyOut("mcp.enrich_company", `{"enriched_fields":{"email":"found@acme.example.com","linkedin_url":"https://linkedin.com/company/acme"},"provider":"stub-provider"}`)
	got, err := r.call(t, "enrich_company", Args{"lead_id": 1.0})
	if err != nil {
		t.Fatal(err)
	}
	if got != fixtureText(t, "read__enrich_company.json") {
		t.Fatalf("got %q", got)
	}
	if strings.Join(r.enrich.Calls, ",") != "py:mcp.enrich_company" {
		t.Fatal(r.enrich.Calls)
	}
	// no chains registered: empty enriched_fields and provider "" is a success
	r.pyOut("mcp.enrich_company", `{"enriched_fields":{},"provider":""}`)
	got, _ = r.call(t, "enrich_company", Args{"lead_id": 1.0})
	if got != "{\n  \"enriched_fields\": {},\n  \"provider\": \"\"\n}" {
		t.Fatalf("empty: %q", got)
	}
	// Python-side miss
	r.pyOut("mcp.enrich_company", `{"not_found":true}`)
	got, _ = r.call(t, "enrich_company", Args{"lead_id": 1.0})
	if got != `{"error": "Lead not found"}` {
		t.Fatalf("not_found: %q", got)
	}
	for _, a := range []Args{{}, {"lead_id": "x"}} {
		if got, err := r.call(t, "enrich_company", a); err != nil || !strings.HasPrefix(got, `{"error": "lead_id `) {
			t.Fatalf("bad lead_id accepted: %q", got)
		}
	}
}

func TestHiringSignalsShape(t *testing.T) {
	r := newEnrichRig(t, Config{})
	r.pyOut("mcp.get_hiring_signals", `{"hiring":true,"data":{"open_roles":3,"titles":["Engineer","Designer","Ünïcode Role"],"velocity":0.1}}`)
	got, err := r.call(t, "get_hiring_signals", Args{"company": "Acme Robotics"})
	if err != nil || got != fixtureText(t, "read__get_hiring_signals.json") {
		t.Fatalf("%q %v", got, err)
	}
	r.pyOut("mcp.get_hiring_signals", `{"hiring":false,"data":{}}`)
	got, _ = r.call(t, "get_hiring_signals", Args{"company": "Nobody Inc"})
	if got != fixtureText(t, "read__get_hiring_signals_none.json") {
		t.Fatalf("none: %q", got)
	}
	got, _ = r.call(t, "get_hiring_signals", Args{"company": "Acme", "lead_id": 1.0})
	if !strings.Contains(got, `"company": "Acme"`) {
		t.Fatal(got)
	}
	r.enrich.Err = errors.New("job board timeout: traceback")
	got, _ = r.call(t, "get_hiring_signals", Args{"company": "Acme"})
	if got != `{"company": "Acme", "error": "enrichment failed"}` {
		t.Fatalf("failure: %q", got)
	}
	if got, _ := r.call(t, "get_hiring_signals", Args{}); got != `{"error": "company is required"}` {
		t.Fatal(got)
	}
}

func TestScoreLeadShape(t *testing.T) {
	r := newEnrichRig(t, Config{})
	r.pyOut("mcp.score_lead", `{"value":60}`)
	got, err := r.call(t, "score_lead", Args{"lead_id": 1.0})
	if err != nil || got != "60" {
		t.Fatalf("%q %v", got, err)
	}
	r.pyOut("mcp.score_lead", `{"total":35,"parts":{"a":1}}`)
	got, _ = r.call(t, "score_lead", Args{"lead_id": 1.0})
	if got != "{\n  \"total\": 35,\n  \"parts\": {\n    \"a\": 1\n  }\n}" {
		t.Fatalf("dict: %q", got)
	}
	r.pyOut("mcp.score_lead", `{"not_found":true}`)
	got, _ = r.call(t, "score_lead", Args{"lead_id": 999.0})
	if got != `{"error": "Lead not found"}` {
		t.Fatalf("miss: %q", got)
	}
	// score_lead spends nothing: no cap, no audit row requirement beyond the cost tools
}

func TestCostToolsAreAudited(t *testing.T) {
	r := newEnrichRig(t, Config{MaxEnrichPerDay: 9})
	r.enrich.Verify = VerifyResult{Status: "valid", Deliverable: true}
	r.pyOut("mcp.enrich_company", `{"enriched_fields":{},"provider":""}`)
	r.pyOut("mcp.get_hiring_signals", `{"hiring":false,"data":{}}`)
	_, _ = r.call(t, "verify_email", Args{"email": "a@b.co"})
	_, _ = r.call(t, "enrich_company", Args{"lead_id": 1.0})
	_, _ = r.call(t, "get_hiring_signals", Args{"company": "Acme"})
	if len(r.audits) != 3 {
		t.Fatalf("audits=%d", len(r.audits))
	}
	for _, a := range r.audits {
		if a.Status != "ok" || a.CostUSD != 0 || a.Error != "" {
			t.Fatalf("%+v", a)
		}
	}
	// failure is audited as error with a generic message
	r.enrich.Err = errors.New("boom secret")
	_, _ = r.call(t, "verify_email", Args{"email": "a@b.co"})
	if r.released != 1 || r.settled != 3 {
		t.Fatalf("settled=%d released=%d", r.settled, r.released)
	}
	last := r.audits[len(r.audits)-1]
	if last.Status != "error" || strings.Contains(last.Error, "secret") {
		t.Fatalf("%+v", last)
	}
}

func TestEnrichCapExceededIsAuditedCapped(t *testing.T) {
	r := newEnrichRig(t, Config{MaxEnrichPerDay: 1})
	r.capHit = true
	r.pyOut("mcp.enrich_company", `{"enriched_fields":{}}`)
	r.pyOut("mcp.get_hiring_signals", `{"hiring":false,"data":{}}`)
	got, _ := r.call(t, "verify_email", Args{"email": "a@b.co"})
	if got != `{"email": "a@b.co", "error": "mcp enrich cap exceeded"}` {
		t.Fatal(got)
	}
	got, _ = r.call(t, "enrich_company", Args{"lead_id": 1.0})
	if got != `{"error": "mcp enrich cap exceeded"}` {
		t.Fatal(got)
	}
	got, _ = r.call(t, "get_hiring_signals", Args{"company": "Acme"})
	if got != `{"company": "Acme", "error": "mcp enrich cap exceeded"}` {
		t.Fatal(got)
	}
	if len(r.enrich.Calls) != 0 {
		t.Fatalf("capped call reached provider: %v", r.enrich.Calls)
	}
	for _, a := range r.audits {
		if a.Status != "capped" {
			t.Fatalf("%+v", a)
		}
	}
	for _, rule := range r.reserved {
		if rule != "__mcp_enrich__" {
			t.Fatal(rule)
		}
	}
	if len(r.reserved) != 3 {
		t.Fatal(r.reserved)
	}
}

func TestEnrichCapZeroIsUnlimited(t *testing.T) {
	r := newEnrichRig(t, Config{})
	r.capHit = true
	r.enrich.Verify = VerifyResult{Status: "valid"}
	if got, _ := r.call(t, "verify_email", Args{"email": "a@b.co"}); strings.Contains(got, "cap") {
		t.Fatal(got)
	}
	if len(r.reserved) != 0 {
		t.Fatal("cap 0 must not reserve")
	}
}

func TestEnrichCapSettlesOnSuccess(t *testing.T) {
	r := newEnrichRig(t, Config{MaxEnrichPerDay: 3})
	r.enrich.Verify = VerifyResult{Status: "valid"}
	_, _ = r.call(t, "verify_email", Args{"email": "a@b.co"})
	if r.settled != 1 || r.released != 0 {
		t.Fatalf("settled=%d released=%d", r.settled, r.released)
	}
}

func TestEnrichUnavailableBackend(t *testing.T) {
	r := newEnrichRig(t, Config{MaxEnrichPerDay: 3})
	r.enrich.Err = pyworker.ErrUnavailable
	got, _ := r.call(t, "enrich_company", Args{"lead_id": 1.0})
	if got != `{"error": "enrichment backend unavailable"}` {
		t.Fatal(got)
	}
	got, _ = r.call(t, "get_hiring_signals", Args{"company": "A"})
	if got != `{"company": "A", "error": "enrichment backend unavailable"}` {
		t.Fatal(got)
	}
	got, _ = r.call(t, "verify_email", Args{"email": "a@b.co"})
	if got != `{"email": "a@b.co", "error": "enrichment backend unavailable"}` {
		t.Fatal(got)
	}
	got, _ = r.call(t, "score_lead", Args{"lead_id": 1.0})
	if got != `{"error": "enrichment backend unavailable"}` {
		t.Fatal(got)
	}
	if r.released != 3 {
		t.Fatalf("released=%d", r.released)
	}
	// no port at all behaves the same
	r.d.Enrich = nil
	got, _ = r.call(t, "score_lead", Args{"lead_id": 1.0})
	if got != `{"error": "enrichment backend unavailable"}` {
		t.Fatal(got)
	}
}

func TestEnrichErrorTextNeverLeaksPythonException(t *testing.T) {
	r := newEnrichRig(t, Config{})
	for _, e := range []error{
		errors.New("Traceback (most recent call last): KeyError 'lead_id'"),
		pyworker.ErrUnauthorized, pyworker.ErrUnknownProvider,
	} {
		r.enrich.Err = e
		for name, args := range map[string]Args{
			"verify_email": {"email": "a@b.co"}, "enrich_company": {"lead_id": 1.0},
			"get_hiring_signals": {"company": "A"}, "score_lead": {"lead_id": 1.0},
		} {
			got, err := r.call(t, name, args)
			if err != nil || !strings.Contains(got, "enrichment failed") || strings.Contains(got, "Traceback") || strings.Contains(got, "pyworker") {
				t.Fatalf("%s: %q %v", name, got, err)
			}
		}
	}
	// a Python-reported failure payload is masked too
	r.enrich.Err = nil
	r.enrich.PyOut["mcp.score_lead"] = json.RawMessage(`not json`)
	got, _ := r.call(t, "score_lead", Args{"lead_id": 1.0})
	if got != `{"error": "enrichment failed"}` {
		t.Fatal(got)
	}
}

type slowEnrich struct{ fakeEnrich }

func (s *slowEnrich) Py(ctx context.Context, _, _ string, _ any) (json.RawMessage, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (s *slowEnrich) VerifyEmail(ctx context.Context, _, _ string) (VerifyResult, error) {
	<-ctx.Done()
	return VerifyResult{}, ctx.Err()
}

func TestEnrichToolTimeout(t *testing.T) {
	r := newEnrichRig(t, Config{ToolTimeoutSeconds: 1, MaxEnrichPerDay: 2})
	r.d.Enrich = &slowEnrich{}
	start := time.Now()
	got, err := r.call(t, "verify_email", Args{"email": "a@b.co"})
	if err != nil || got != `{"email": "a@b.co", "error": "enrichment timed out"}` {
		t.Fatalf("%q %v", got, err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("did not honor MCP_TOOL_TIMEOUT_SECONDS")
	}
	if r.released != 1 || len(r.audits) != 1 || r.audits[0].Status != "error" {
		t.Fatalf("released=%d audits=%+v", r.released, r.audits)
	}
}

// TestEnrichAgainstPythonRPC runs the tools through the real pyworker client
// against a live Python API; skipped unless TEST_PY_WORKER_URL is set.
func TestEnrichAgainstPythonRPC(t *testing.T) {
	url := os.Getenv("TEST_PY_WORKER_URL")
	if url == "" {
		t.Skip("TEST_PY_WORKER_URL not set")
	}
	cl := pyworker.New(url, os.Getenv("TEST_PY_WORKER_TOKEN"), nil)
	r := newEnrichRig(t, Config{})
	r.d.Enrich = rpcEnrich{cl}
	got, err := r.call(t, "get_hiring_signals", Args{"company": "Nobody Inc"})
	if err != nil || !strings.Contains(got, `"company": "Nobody Inc"`) {
		t.Fatalf("%q %v", got, err)
	}
	got, _ = r.call(t, "score_lead", Args{"lead_id": 999999999.0})
	if got != `{"error": "Lead not found"}` {
		t.Fatalf("%q", got)
	}
}

type rpcEnrich struct{ c *pyworker.Client }

func (rpcEnrich) VerifyEmail(context.Context, string, string) (VerifyResult, error) {
	return VerifyResult{}, ErrUnavailable
}
func (p rpcEnrich) Py(ctx context.Context, ws, provider string, in any) (json.RawMessage, error) {
	b, _ := json.Marshal(in)
	res, err := p.c.Call(ctx, pyworker.ProviderCall{CallID: "it-" + time.Now().Format("150405.000000"), Provider: provider, WorkspaceID: ws, Input: b, Timeout: 30 * time.Second})
	if err != nil {
		return nil, err
	}
	if res.Err != nil {
		return nil, res.Err
	}
	if res.Status != "ok" {
		return nil, errors.New("python call " + res.Status)
	}
	return res.Output, nil
}

// fixtureText returns the recorded tool text payload of a testdata/mcp/tools fixture.
func fixtureText(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("../../testdata/mcp/tools/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Response struct {
			Result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		} `json:"response"`
	}
	if err := json.Unmarshal(b, &f); err != nil || len(f.Response.Result.Content) == 0 {
		t.Fatal(err, name)
	}
	return f.Response.Result.Content[0].Text
}
