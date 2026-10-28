//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/audit"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/identity"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/llm"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/provaccuracy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/testpg"
)

type llmEnv struct {
	mcpEnv
	h   *llmRoutes
	llm *scriptLLM
}

func newLLMEnv(t *testing.T) *llmEnv {
	t.Helper()
	pool := testpg.NewAppPool(t, 8)
	st := identity.NewStore(pool)
	r := httpx.NewRouter()
	r.Use(httpx.RequestID(), audit.Middleware(pool, nil))
	d := Deps{Pool: pool, Signer: testSigner, Identity: st, Limiter: frozenLimiter(), Flags: config.Flags{PGLeadStore: true}}
	h := newLLMRoutes(d)
	h.httpc = http.DefaultClient
	fl := &scriptLLM{}
	h.newLLM = func(context.Context, string) llmBundle {
		return llmBundle{Extract: fl, Chat: fakeChat{llm.ChatProvider{}}}
	}
	h.register(r)
	e := &llmEnv{mcpEnv: mcpEnv{env: env{h: r.Handler(), st: st}, pool: pool}, h: h, llm: fl}
	e.cleanup(t)
	t.Cleanup(func() { e.cleanup(t); providerAccuracyNow = nil })
	e.seed(t, "alice", pythonHash, true, "admin", true)
	e.seed(t, "bob", pythonHash, true, "user", false)
	e.seed(t, "erin", pythonHash, true, "user", false)
	alice, bob, erin := e.mustUserByName(t, "alice"), e.mustUserByName(t, "bob"), e.mustUserByName(t, "erin")
	e.workspace(t, "ws-alpha", alice.ID)
	e.workspace(t, "ws-beta", bob.ID)
	e.member(t, "ws-alpha", alice.ID, "owner")
	e.member(t, "ws-alpha", bob.ID, "member") // the viewer
	e.member(t, "ws-alpha", erin.ID, "editor")
	e.member(t, "ws-beta", bob.ID, "owner")
	return e
}

func (e *llmEnv) cleanup(t *testing.T) {
	t.Helper()
	ownerExec(t, `DELETE FROM leads WHERE workspace_id IN ('ws-alpha','ws-beta') AND source = 'llmroutes'`)
	ownerExec(t, `DELETE FROM workbooks WHERE id LIKE 'wb-llm-%'`)
	ownerExec(t, `DELETE FROM provider_stats WHERE provider IN ('wikidata','jsonld_firmographics','company_intel')`)
}

func (e *llmEnv) call(method, who, ws, path, body string) *httptest.ResponseRecorder {
	hdr := map[string]string{}
	if who != "" {
		hdr = bearer(who)
	}
	if ws != "" {
		hdr["X-Workspace-Id"] = ws
	}
	rec, _ := e.do(method, path, "application/json", body, hdr)
	return rec
}

func (e *llmEnv) lead(t *testing.T, id int, ws, company, email string) {
	t.Helper()
	ownerExec(t, `INSERT INTO leads (id, workspace_id, company, city, email, status, source, created_at, updated_at)
		VALUES ($1,$2,$3,'Pune',$4,'new','llmroutes','2026-08-29T10:00:00','2026-08-29T10:00:00')`, id, ws, company, email)
}

func TestLLMRoutesAuthAndRoles(t *testing.T) {
	e := newLLMEnv(t)
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/api/campaigns/generate", `{"lead_ids":[1],"value_proposition":"vp"}`},
		{"POST", "/api/v2/workbooks/wb-x/generate-column", `{"instruction":"abc"}`},
		{"GET", "/api/workbooks/meta/provider-accuracy", ""},
		{"POST", "/api/lead/1/enrich", ""},
	} {
		if rec := e.call(c.method, "", "", c.path, c.body); rec.Code != 401 {
			t.Errorf("%s %s without a token = %d", c.method, c.path, rec.Code)
		}
		if c.method == "POST" {
			if rec := e.call(c.method, "bob", "ws-alpha", c.path, c.body); rec.Code != 403 {
				t.Errorf("%s %s as viewer = %d", c.method, c.path, rec.Code)
			}
		} else if rec := e.call(c.method, "bob", "ws-alpha", c.path, c.body); rec.Code != 200 {
			t.Errorf("%s %s as viewer = %d", c.method, c.path, rec.Code)
		}
	}
}

func TestCampaignGenerateTenancy(t *testing.T) {
	e := newLLMEnv(t)
	e.lead(t, 9101, "ws-alpha", "Alpha Co", "a@alpha.test")
	e.lead(t, 9102, "ws-beta", "Beta Co", "b@beta.test")
	e.lead(t, 9103, "ws-alpha", "No Mail", "")
	e.llm.script = []json.RawMessage{json.RawMessage(`{"subject":"S","body":"B"}`)}
	rec := e.call("POST", "erin", "ws-alpha", "/api/campaigns/generate", `{"lead_ids":[9102,9103,9101,424242],"value_proposition":"vp"}`)
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"emails":[{"company":"Alpha Co","subject":"S","body":"B"}]}` {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(e.llm.calls) != 1 || !strings.Contains(e.llm.calls[0].Prompt, "Company: Alpha Co\nLocation: Pune") {
		t.Errorf("prompts %v", e.llm.calls)
	}
}

func TestGenerateColumnRoute(t *testing.T) {
	e := newLLMEnv(t)
	ownerExec(t, `INSERT INTO workbooks (id, name, description, status, workspace_id, source_type, filter_criteria, columns_config, total_rows, completed_rows, sync_to_leads, created_at, updated_at)
		VALUES ('wb-llm-1','W','','draft','ws-alpha','empty','{}'::json,'[{"id":"email","name":"Email","type":"lead_field","width":200}]'::json,0,0,false,now(),now())`)
	p := "/api/v2/workbooks/wb-llm-1/generate-column"
	if rec := e.call("POST", "erin", "ws-alpha", p, `{"instruction":"ab"}`); rec.Code != 422 {
		t.Errorf("short instruction = %d", rec.Code)
	}
	if rec := e.call("POST", "erin", "ws-beta", "/api/v2/workbooks/wb-llm-1/generate-column", `{"instruction":"abc"}`); rec.Code != 403 && rec.Code != 404 {
		t.Errorf("foreign workspace = %d", rec.Code)
	}
	if rec := e.call("POST", "bob", "ws-beta", p, `{"instruction":"abc"}`); rec.Code != 404 || strings.TrimSpace(rec.Body.String()) != `{"detail":"Workbook not found"}` {
		t.Errorf("foreign workbook = %d %s", rec.Code, rec.Body)
	}
	rec := e.call("POST", "erin", "ws-alpha", p, `{"instruction":"do the thing"}`) // the fake LLM answers {}
	if rec.Code != 502 || strings.TrimSpace(rec.Body.String()) != `{"detail":"Column generation failed: empty LLM response"}` {
		t.Errorf("empty answer = %d %s", rec.Code, rec.Body)
	}
	e.llm.script = []json.RawMessage{json.RawMessage(`{"kind":"formula","name":"Email Domain","formula":"{Email}.split(\"@\")[1]","explanation":"Splits."}`)}
	rec = e.call("POST", "erin", "ws-alpha", p, `{"instruction":"domain of the email"}`)
	var got struct {
		Column map[string]any `json:"column"`
		Kind   string         `json:"kind"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &got) != nil || got.Kind != "formula" || got.Column["id"] != "email_domain" || len(got.Column) != 32 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(e.llm.calls[len(e.llm.calls)-1].Prompt, "- {Email} (type: lead_field)") {
		t.Error("the workbook's columns were not in the prompt")
	}
}

func TestProviderAccuracyRoute(t *testing.T) {
	e := newLLMEnv(t)
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	providerAccuracyNow = func() time.Time { return now }
	rec := e.call("GET", "bob", "ws-alpha", "/api/workbooks/meta/provider-accuracy", "")
	want, _ := provaccuracy.RunEval(now)
	wb, _ := json.Marshal(map[string]any{"ranking": provaccuracy.Rows(want), "persisted": false})
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != string(wb) {
		t.Fatalf("%d %s\nwant %s", rec.Code, rec.Body, wb)
	}
	if n := ownerCount(t, `SELECT count(*) FROM provider_stats WHERE provider='wikidata'`); n != 0 {
		t.Fatalf("a plain GET wrote %d rows", n)
	}
	if rec := e.call("GET", "bob", "ws-alpha", "/api/workbooks/meta/provider-accuracy?persist=true", ""); rec.Code != 403 {
		t.Errorf("viewer persist = %d", rec.Code)
	}
	if rec := e.call("GET", "bob", "ws-alpha", "/api/workbooks/meta/provider-accuracy?persist=maybe", ""); rec.Code != 422 {
		t.Errorf("bad persist = %d", rec.Code)
	}
	rec = e.call("GET", "erin", "ws-alpha", "/api/workbooks/meta/provider-accuracy?persist=true", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"persisted":true`) {
		t.Fatalf("editor persist = %d %s", rec.Code, rec.Body)
	}
	if n := ownerCount(t, `SELECT count(*) FROM provider_stats WHERE provider='wikidata' AND field='*' AND accuracy_score IS NOT NULL`); n != 1 {
		t.Errorf("wildcard row missing (%d)", n)
	}
}

func TestLeadEnrichRoute(t *testing.T) {
	e := newLLMEnv(t)
	e.lead(t, 9201, "ws-alpha", "Enrich Co", "")
	e.lead(t, 9202, "ws-beta", "Beta Co", "")
	e.h.providerLookup = mapProviders{"ddg_email_provider": fakeProvider{out: providersOutputValue("found@enrich.test")}}
	rec := e.call("POST", "erin", "ws-alpha", "/api/lead/9201/enrich?action=find_emails", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"email": "found@enrich.test"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if n := ownerCount(t, `SELECT count(*) FROM leads WHERE id=9201 AND email='found@enrich.test' AND last_enriched_at LIKE '20%+00:00'`); n != 1 {
		t.Error("the email and an ISO last_enriched_at were not saved")
	}
	if rec := e.call("POST", "erin", "ws-alpha", "/api/lead/9202/enrich?action=find_emails", ""); rec.Code != 404 {
		t.Errorf("foreign lead = %d", rec.Code)
	}
	if rec := e.call("POST", "erin", "ws-alpha", "/api/lead/abc/enrich", ""); rec.Code != 422 {
		t.Errorf("bad id = %d", rec.Code)
	}
}

// TestLeadEnrichRateLimit: 30/minute per caller; the 422 of a bad id does not count, a 404 does (slowapi order).
func TestLeadEnrichRateLimit(t *testing.T) {
	e := newLLMEnv(t)
	for i := 0; i < 5; i++ {
		e.call("POST", "erin", "ws-alpha", "/api/lead/abc/enrich", "")
	}
	codes := map[int]int{}
	for i := 0; i < 32; i++ {
		codes[e.call("POST", "erin", "ws-alpha", "/api/lead/777777/enrich", "").Code]++
	}
	if codes[404] != 30 || codes[429] != 2 {
		t.Errorf("codes = %v, want 30x404 and 2x429 (%s)", codes, fmt.Sprint(codes))
	}
}
