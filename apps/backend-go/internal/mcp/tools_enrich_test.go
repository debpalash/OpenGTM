package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadscore"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/provcall"
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
	r.pyOut("ddg_email", `{}`)
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

// enrich_company runs the field waterfall in Go over the in-process providers (plan 15): website_scraper fills
// email, phone and LinkedIn from the lead's site, ddg_email searches an email the site did not give. Python built
// a WaterfallEnricher with no chains, so it always answered empty (bug fixed, not mirrored).
func TestEnrichCompanyResultShape(t *testing.T) {
	r := newEnrichRig(t, Config{})
	site := "acme.example.com"
	r.leads.Rows[1].Website = &site
	r.pyOut("website_scraper", `{"email":"found@acme.example.com","phone":"+1 555 0100","linkedin_url":"https://linkedin.com/company/acme","description":"x"}`)
	r.pyOut("ddg_email", `{"email":"other@acme.example.com"}`)
	got, err := r.call(t, "enrich_company", Args{"lead_id": 1.0})
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"enriched_fields\": {\n    \"email\": \"found@acme.example.com\",\n    \"phone\": \"+1 555 0100\",\n" +
		"    \"linkedin_url\": \"https://linkedin.com/company/acme\"\n  },\n  \"provider\": \"website_scraper\"\n}"
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if strings.Join(r.enrich.Calls, ",") != "py:website_scraper" {
		t.Fatal(r.enrich.Calls)
	}
	// the site has no email: ddg_email searches one
	r.enrich.Calls = nil
	r.pyOut("website_scraper", `{"phone":"+1 555 0100"}`)
	got, _ = r.call(t, "enrich_company", Args{"lead_id": 1.0})
	want = "{\n  \"enriched_fields\": {\n    \"email\": \"other@acme.example.com\",\n    \"phone\": \"+1 555 0100\"\n  },\n" +
		"  \"provider\": \"ddg_email,website_scraper\"\n}"
	if got != want || strings.Join(r.enrich.Calls, ",") != "py:website_scraper,py:ddg_email" {
		t.Fatalf("fallback: %q calls %v", got, r.enrich.Calls)
	}
	// nothing found anywhere: empty enriched_fields and provider "" is a success
	r.pyOut("website_scraper", `{}`)
	r.pyOut("ddg_email", `{"email":"not an email"}`)
	got, _ = r.call(t, "enrich_company", Args{"lead_id": 1.0})
	if got != "{\n  \"enriched_fields\": {},\n  \"provider\": \"\"\n}" {
		t.Fatalf("empty: %q", got)
	}
	// a lead without a website and with an email spends nothing
	r.enrich.Calls = nil
	mail := "known@acme.example.com"
	r.leads.Rows[1].Website, r.leads.Rows[1].Email = nil, &mail
	got, _ = r.call(t, "enrich_company", Args{"lead_id": 1.0})
	if got != "{\n  \"enriched_fields\": {},\n  \"provider\": \"\"\n}" || len(r.enrich.Calls) != 0 {
		t.Fatalf("nothing to do: %q calls %v", got, r.enrich.Calls)
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

// score_lead is scoring.score_lead in Go (internal/leadscore) over the workspace's lead: no provider call.
func TestScoreLeadShape(t *testing.T) {
	r := newEnrichRig(t, Config{})
	site, mail, phone := "https://acme.example.com", "ceo@acme.example.com", "+1 555 0100"
	r.leads.Rows[1].Website, r.leads.Rows[1].Email, r.leads.Rows[1].Phone = &site, &mail, &phone
	want := leadscore.Score(*r.leads.Rows[1], nil)
	if want <= 0 {
		t.Fatalf("fixture lead scores %d", want)
	}
	got, err := r.call(t, "score_lead", Args{"lead_id": 1.0})
	if err != nil || got != strconv.Itoa(want) {
		t.Fatalf("%q %v, want %d", got, err, want)
	}
	got, _ = r.call(t, "score_lead", Args{"lead_id": 999.0})
	if got != `{"error": "Lead not found"}` {
		t.Fatalf("miss: %q", got)
	}
	if len(r.enrich.Calls) != 0 {
		t.Fatalf("score_lead reached a provider: %v", r.enrich.Calls)
	}
	// score_lead spends nothing: no cap, no audit row requirement beyond the cost tools
}

func TestCostToolsAreAudited(t *testing.T) {
	r := newEnrichRig(t, Config{MaxEnrichPerDay: 9})
	r.enrich.Verify = VerifyResult{Status: "valid", Deliverable: true}
	r.pyOut("ddg_email", `{}`)
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
	r.pyOut("ddg_email", `{}`)
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
	r.enrich.Err = provcall.ErrUnavailable
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
	if r.released != 3 {
		t.Fatalf("released=%d", r.released)
	}
	// no port at all behaves the same; score_lead needs none
	r.d.Enrich = nil
	got, _ = r.call(t, "enrich_company", Args{"lead_id": 1.0})
	if got != `{"error": "enrichment backend unavailable"}` {
		t.Fatal(got)
	}
	if got, _ = r.call(t, "score_lead", Args{"lead_id": 1.0}); got != strconv.Itoa(leadscore.Score(*r.leads.Rows[1], nil)) {
		t.Fatal(got)
	}
}

func TestEnrichErrorTextNeverLeaksPythonException(t *testing.T) {
	r := newEnrichRig(t, Config{})
	for _, e := range []error{
		errors.New("Traceback (most recent call last): KeyError 'lead_id'"),
		provcall.ErrUnauthorized, provcall.ErrUnknownProvider,
	} {
		r.enrich.Err = e
		for name, args := range map[string]Args{
			"verify_email": {"email": "a@b.co"}, "enrich_company": {"lead_id": 1.0},
			"get_hiring_signals": {"company": "A"},
		} {
			got, err := r.call(t, name, args)
			if err != nil || !strings.Contains(got, "enrichment failed") || strings.Contains(got, "Traceback") || strings.Contains(got, "pyworker") {
				t.Fatalf("%s: %q %v", name, got, err)
			}
		}
	}
	// a malformed provider payload is masked too
	r.enrich.Err = nil
	r.enrich.PyOut["ddg_email"] = json.RawMessage(`not json`)
	got, _ := r.call(t, "enrich_company", Args{"lead_id": 1.0})
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

type panicEnrich struct{ fakeEnrich }

func (*panicEnrich) VerifyEmail(context.Context, string, string) (VerifyResult, error) {
	panic("adapter bug")
}

func TestEnrichPanicReleasesHeldSlot(t *testing.T) {
	r := newEnrichRig(t, Config{MaxEnrichPerDay: 2})
	r.d.Enrich = &panicEnrich{}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("panic swallowed")
			}
		}()
		_, _ = r.call(t, "verify_email", Args{"email": "a@b.co"})
	}()
	if r.released != 1 || r.settled != 0 {
		t.Fatalf("released=%d settled=%d", r.released, r.settled)
	}
}

func TestEnrichCompanyPythonMissReleasesSlotAndAuditsError(t *testing.T) {
	r := newEnrichRig(t, Config{MaxEnrichPerDay: 2})
	r.pyOut("mcp.enrich_company", `{"not_found":true}`)
	got, _ := r.call(t, "enrich_company", Args{"lead_id": 1.0})
	if got != `{"error": "Lead not found"}` {
		t.Fatal(got)
	}
	if r.released != 1 || r.settled != 0 || len(r.audits) != 1 || r.audits[0].Status != "error" {
		t.Fatalf("released=%d settled=%d audits=%+v", r.released, r.settled, r.audits)
	}
}

func TestEnrichLogsNeverCarryProviderErrorText(t *testing.T) {
	var buf strings.Builder
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	r := newEnrichRig(t, Config{})
	r.enrich.Err = errors.New("GET https://api.example.com/v1?key=SECRETFRAGMENT failed")
	for name, args := range map[string]Args{
		"verify_email": {"email": "a@b.co"}, "enrich_company": {"lead_id": 1.0},
		"get_hiring_signals": {"company": "A"}, "score_lead": {"lead_id": 1.0},
	} {
		if _, err := r.call(t, name, args); err != nil {
			t.Fatal(err)
		}
	}
	logs := buf.String()
	if strings.Contains(logs, "SECRETFRAGMENT") || strings.Contains(logs, "api.example.com") {
		t.Fatalf("provider error text logged: %s", logs)
	}
	if !strings.Contains(logs, "*errors.errorString") {
		t.Fatalf("error kind not logged: %s", logs)
	}
}
