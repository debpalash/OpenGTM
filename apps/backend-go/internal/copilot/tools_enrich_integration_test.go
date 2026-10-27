//go:build integration

package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// fakeEnrichProvider is a registry provider enrich_lead runs (ungated: in process).
type fakeEnrichProvider struct {
	name  string
	mu    sync.Mutex
	calls []providers.Input
	out   providers.Output
	err   error
}

func (p *fakeEnrichProvider) Name() string { return p.name }
func (p *fakeEnrichProvider) Enrich(ctx context.Context, _ *http.Client, _ func() (string, error), in providers.Input) (providers.Output, error) {
	p.mu.Lock()
	p.calls = append(p.calls, in)
	p.mu.Unlock()
	if providers.WorkspaceFrom(ctx) == "" {
		return providers.Output{}, errors.New("no workspace on the provider context")
	}
	return p.out, p.err
}

type fakeLookup map[string]providers.Provider

func (f fakeLookup) Get(name string) (providers.Provider, bool) { p, ok := f[name]; return p, ok }

func provFields(kv ...string) *providers.Obj {
	o := providers.NewObj()
	for i := 0; i+1 < len(kv); i += 2 {
		o.Set(kv[i], kv[i+1])
	}
	return o
}

// enrich_lead (plan 15 WP-CUTOVER: was the last Python copilot tool) scans the lead's site with website_scraper,
// fills only empty fields, and searches a missing email with ddg_email.
func TestEnrichLeadTool(t *testing.T) {
	e := nativeSetup(t)
	ctx := context.Background()
	if _, err := e.owner.Exec(ctx, `UPDATE leads SET website = 'acme.example.com', phone = NULL, linkedin_url = NULL, description = NULL WHERE id = $1`, e.acme); err != nil {
		t.Fatal(err)
	}
	var delta int64
	if err := e.owner.QueryRow(ctx, `INSERT INTO leads (workspace_id, company, website) VALUES ($1, 'Delta', 'https://delta.example.com') RETURNING id`, e.ws).Scan(&delta); err != nil {
		t.Fatal(err)
	}
	scraper := &fakeEnrichProvider{name: "website_scraper"}
	finder := &fakeEnrichProvider{name: "ddg_email"}
	var checked []string
	turn := func(uid int32) *Turn {
		return &Turn{Deps: Deps{Pool: e.pool, Engine: &Engine{Providers: fakeLookup{"website_scraper": scraper, "ddg_email": finder},
			URLCheck: func(u string) error {
				checked = append(checked, u)
				if strings.Contains(u, "blocked") {
					return errors.New("blocked")
				}
				return nil
			}}}, WS: e.ws, Slug: "s", UID: i32(uid)}
	}
	run := func(uid int32, args map[string]any) map[string]any {
		t.Helper()
		res, handled, err := turn(uid).engineTool(ctx, parsedCall{Name: "enrich_lead", Args: args})
		if err != nil || !handled {
			t.Fatalf("handled=%v err=%v res=%s", handled, err, res)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(res), &m); err != nil {
			t.Fatalf("not JSON: %s", res)
		}
		if strings.Contains(res, `":"`) || strings.Contains(res, `","`) {
			t.Fatalf("not Python json.dumps spacing: %s", res)
		}
		return m
	}
	row := func(id int64) map[string]any {
		var email, phone, li, desc, enriched *string
		if err := e.owner.QueryRow(ctx, `SELECT email, phone, linkedin_url, description, last_enriched_at FROM leads WHERE id = $1`, id).
			Scan(&email, &phone, &li, &desc, &enriched); err != nil {
			t.Fatal(err)
		}
		return map[string]any{"email": sval(email), "phone": sval(phone), "linkedin_url": sval(li), "description": sval(desc), "enriched": enriched != nil}
	}

	// The site fills the empty phone, LinkedIn and description; the existing email is kept and no search runs.
	scraper.out = providers.Output{Value: "info@acme.example.com", Confidence: 0.65,
		Extra: provFields("email", "info@acme.example.com", "phone", "+91 20 5555 0100", "linkedin_url", "https://linkedin.com/company/acme", "description", "Pipes")}
	m := run(2, map[string]any{"lead_id": e.acme})
	if m["ok"] != true || m["company"] != "Acme" || m["email"] != "Acme@x.io" || m["phone"] != "+91 20 5555 0100" || m["linkedin"] != "https://linkedin.com/company/acme" {
		t.Fatalf("result %v", m)
	}
	if got, _ := json.Marshal(m["enriched_fields"]); string(got) != `["phone","linkedin","description"]` {
		t.Fatalf("enriched_fields %s", got)
	}
	if r := row(e.acme); r["phone"] != "+91 20 5555 0100" || r["linkedin_url"] != "https://linkedin.com/company/acme" || r["description"] != "Pipes" ||
		r["email"] != "Acme@x.io" || r["enriched"] != true {
		t.Fatalf("row %v", r)
	}
	if len(scraper.calls) != 1 || scraper.calls[0].Lead["website"] != "acme.example.com" || len(finder.calls) != 0 {
		t.Fatalf("calls scraper=%v finder=%d", scraper.calls, len(finder.calls))
	}
	if len(checked) != 1 || checked[0] != "https://acme.example.com" {
		t.Fatalf("url check %v", checked)
	}

	// Complete leads spend nothing; a lead without a website and with an email is not touched.
	scraper.calls, finder.calls = nil, nil
	m = run(2, map[string]any{"lead_id": e.acme})
	if got, _ := json.Marshal(m["enriched_fields"]); string(got) != `[]` || len(scraper.calls)+len(finder.calls) != 0 {
		t.Fatalf("complete lead: %v scraper=%d finder=%d", m, len(scraper.calls), len(finder.calls))
	}

	// No email on the site: ddg_email searches one.
	scraper.out = providers.Output{Error: "No contact info found on website", Extra: providers.NewObj()}
	finder.out = providers.Output{Value: "hello@delta.example.com", Confidence: 0.6, Extra: provFields("email", "hello@delta.example.com")}
	m = run(2, map[string]any{"lead_id": delta})
	if got, _ := json.Marshal(m["enriched_fields"]); string(got) != `["email"]` || m["email"] != "hello@delta.example.com" || m["phone"] != nil {
		t.Fatalf("search fallback: %v", m)
	}
	if r := row(delta); r["email"] != "hello@delta.example.com" || r["enriched"] != true {
		t.Fatalf("delta row %v", r)
	}

	// A scrape failure is reported without its text and writes nothing.
	if _, err := e.owner.Exec(ctx, `UPDATE leads SET email = NULL, last_enriched_at = NULL WHERE id = $1`, delta); err != nil {
		t.Fatal(err)
	}
	scraper.err = errors.New("dial tcp 10.0.0.7:443: secret-host refused")
	finder.out = providers.Output{Error: "not found", Extra: providers.NewObj()}
	res, handled, err := turn(2).engineTool(ctx, parsedCall{Name: "enrich_lead", Args: map[string]any{"lead_id": delta}})
	if err != nil || !handled || strings.Contains(res, "secret-host") || !strings.Contains(res, `"website_error: website scrape failed"`) {
		t.Fatalf("scrape failure: %s %v %v", res, handled, err)
	}
	if r := row(delta); r["enriched"] != false {
		t.Fatalf("a failed scrape alone must not bump last_enriched_at: %v", r)
	}
	scraper.err = nil

	// A website the egress policy refuses is never handed to a provider.
	if _, err := e.owner.Exec(ctx, `UPDATE leads SET website = 'http://blocked.internal' WHERE id = $1`, delta); err != nil {
		t.Fatal(err)
	}
	scraper.calls = nil
	m = run(2, map[string]any{"lead_id": delta})
	if !strings.Contains(strings.Join(enrichedStrings(m["enriched_fields"]), ","), "website_error: website URL is not allowed") || len(scraper.calls) != 0 {
		t.Fatalf("blocked url: %v calls %d", m, len(scraper.calls))
	}

	// Misses, missing arguments, roles.
	for _, id := range []int64{e.foreign, 999999999} {
		if res, _, _ := turn(2).engineTool(ctx, parsedCall{Name: "enrich_lead", Args: map[string]any{"lead_id": id}}); res != errLeadNotFound {
			t.Fatalf("lead %d: %s", id, res)
		}
	}
	if res, _, _ := turn(2).engineTool(ctx, parsedCall{Name: "enrich_lead", Args: map[string]any{}}); res != `{"error": "Tool 'enrich_lead' failed: 'lead_id'"}` {
		t.Fatalf("missing lead_id: %s", res)
	}
	if res, _, _ := turn(3).engineTool(ctx, parsedCall{Name: "enrich_lead", Args: map[string]any{"lead_id": e.acme}}); res != errInsufficientRole {
		t.Fatalf("viewer: %s", res)
	}
	// Without providers the tool is unavailable (a tool error from the caller).
	tn := &Turn{Deps: Deps{Pool: e.pool, Engine: &Engine{}}, WS: e.ws, Slug: "s", UID: i32(2)}
	if _, handled, err := tn.engineTool(ctx, parsedCall{Name: "enrich_lead", Args: map[string]any{"lead_id": e.acme}}); handled || err != nil {
		t.Fatalf("no providers: handled=%v err=%v", handled, err)
	}
}

func enrichedStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}
