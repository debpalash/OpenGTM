package scrape

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/browserclient"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"
)

// OutputDict is the /internal/provider-call `output` object of an enrichment
// provider (provider_runner._provider_job): provider, success, fields,
// confidence, error, billing_evidence (never set by these providers) and the
// declared license (none declares one: "unknown").
func OutputDict(provider string, out providers.Output) *providers.Obj {
	o := providers.NewObj()
	o.Set("provider", provider)
	o.Set("success", out.Error == "")
	fields := out.Extra
	if fields == nil {
		fields = providers.NewObj()
	}
	o.Set("fields", fields)
	o.Set("confidence", out.Confidence)
	o.Set("error", out.Error)
	o.Set("billing_evidence", nil)
	o.Set("license", "unknown")
	return o
}

// ResultProviderOf is the EnrichmentResult.provider of p's answer.
func ResultProviderOf(p providers.Provider, out providers.Output) string {
	if rn, ok := p.(ResultName); ok {
		return rn.ResultProvider(out)
	}
	return p.Name()
}

// RPC is a named provider-call handler that takes the raw input object (not a
// lead): it returns the provider-call `output` object. A returned error is
// infrastructure (the context ended).
type RPC func(ctx context.Context, c *http.Client, input json.RawMessage) (*providers.Obj, error)

// RPCs are the named RPC providers served in process: the poller's
// ddgs_search and jobspy_signals, the MCP get_hiring_signals tool and the
// person-intel deep scrape.
func RPCs() map[string]RPC {
	return map[string]RPC{
		"ddgs_search":            ddgsSearch,
		"jobspy_signals":         jobspySignals,
		"mcp.get_hiring_signals": mcpHiringSignals,
		"person_intel_deep":      personIntelDeep,
	}
}

func rawObject(input json.RawMessage) map[string]any {
	v, ok := decodeJSON(string(input))
	if !ok {
		return map[string]any{}
	}
	m, _ := v.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}

// pyStrictInt is isinstance(v, int) and not bool for a decoded JSON value.
func pyStrictInt(v any) (int, bool) {
	n, ok := v.(json.Number)
	if !ok || strings.ContainsAny(string(n), ".eE") {
		return 0, false
	}
	i, err := n.Int64()
	if err != nil {
		return 0, false
	}
	return int(i), true
}

// ── poller/rpc_providers.py ────────────────────────────────────────────────

func ddgsSearch(ctx context.Context, _ *http.Client, input json.RawMessage) (*providers.Obj, error) {
	raw := rawObject(input)
	in := providers.Input{}
	q, isStr := raw["query"].(string)
	if !isStr || pyStrip(q) == "" {
		return OutputDict("ddgs_search", fail(in, "query required")), nil
	}
	q = trunc(pyStrip(q), 500)
	n := 5
	if m, ok := pyStrictInt(raw["max_results"]); ok && m >= 1 {
		n = min(m, 20)
	}
	s := searchSeam()
	if s == nil {
		return OutputDict("ddgs_search", fail(in, errNoWeb)), nil
	}
	results := []any{}
	for _, h := range s.Text(ctx, q, n) {
		o := providers.NewObj()
		o.Set("title", h.Title)
		o.Set("snippet", h.Body)
		o.Set("url", h.Href)
		results = append(results, o)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return OutputDict("ddgs_search", ok(in, 1.0, "results", results)), nil
}

// leadFromRaw is Lead.from_dict over the raw input: string-valued Lead
// attributes as given (the providers read them through ls).
func leadFromRaw(raw map[string]any) map[string]any { return raw }

func jobspySignals(ctx context.Context, c *http.Client, input json.RawMessage) (*providers.Obj, error) {
	raw := rawObject(input)
	j := JobSpy{MaxJobs: 5}
	if m, ok := pyStrictInt(raw["max_jobs"]); ok && m >= 1 {
		j.MaxJobs = min(m, 20)
	}
	out, err := j.Enrich(ctx, c, nil, providers.Input{Lead: leadFromRaw(raw)})
	if err != nil {
		return nil, err
	}
	return OutputDict("jobspy_signals", out), nil
}

// ── mcp/rpc_tools.py McpGetHiringSignals ───────────────────────────────────

func mcpHiringSignals(ctx context.Context, c *http.Client, input json.RawMessage) (*providers.Obj, error) {
	raw := rawObject(input)
	company, _ := raw["company"].(string)
	out, err := JobSpy{}.Enrich(ctx, c, nil, providers.Input{Lead: map[string]any{"company": company}})
	if err != nil {
		return nil, err
	}
	data := out.Extra
	if data == nil {
		data = providers.NewObj()
	}
	return OutputDict("mcp.get_hiring_signals", ok(providers.Input{}, 1.0, "hiring", out.Error == "", "data", data)), nil
}

// ── person_intel_rpc.py + services/scraper.py UniversalScraper ─────────────

const (
	piMaxURL    = 2048
	piMaxText   = 1000
	piMaxEmails = 50
)

var piEmailRe = pyRe(`[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}`)

func piFail(msg string) *providers.Obj {
	o := providers.NewObj()
	o.Set("success", false)
	o.Set("preview_text", "")
	o.Set("extracted_emails", []any{})
	o.Set("word_count", 0)
	o.Set("error", msg)
	return o
}

// piJSWall is the UniversalScraper fast-path test (Python precedence:
// "enable javascript" OR ("please wait..." AND short page)).
func piJSWall(page string) bool {
	l := strings.ToLower(page)
	return strings.Contains(l, "enable javascript") || (strings.Contains(l, "please wait...") && runeLen(page) < 2000)
}

// personIntelDeep scrapes ONE page: the fast path is a TLS-impersonating
// fetch through the browser service, a JS wall or a failed fetch falls back
// to a real browser render with auto-scroll. The text is the browser
// service's legacy _parse_html markdown (scripts, styles and embeds dropped,
// links and images stripped). Deviation: extracted emails keep first-seen
// order (Python returned list(set(...)), an arbitrary order).
func personIntelDeep(ctx context.Context, _ *http.Client, input json.RawMessage) (*providers.Obj, error) {
	raw := rawObject(input)
	u, isStr := raw["url"].(string)
	if !isStr || !(strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")) || len(u) > piMaxURL {
		return piFail("url must be an http(s) URL"), nil
	}
	b := current().Browser
	if b == nil {
		return piFail("scrape failed: " + errNoWeb), nil
	}
	text, found, err := piRender(ctx, b, u)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
		var be *browserclient.Error
		if asBrowserErr(err, &be) && be.Code == "egress_blocked" {
			return piFail("url blocked"), nil
		}
		return piFail("scrape failed: " + browserCode(err)), nil
	}
	if !found {
		return piFail("no content"), nil
	}
	emails := []any{}
	seen := map[string]bool{}
	for _, e := range piEmailRe.FindAllString(text, -1) {
		if !seen[e] && len(emails) < piMaxEmails {
			seen[e] = true
			emails = append(emails, e)
		}
	}
	o := providers.NewObj()
	o.Set("success", true)
	o.Set("preview_text", trunc(trunc(text, 2000), piMaxText))
	o.Set("extracted_emails", emails)
	o.Set("word_count", len(pySplit(text)))
	o.Set("error", nil)
	return o, nil
}

func piRender(ctx context.Context, b Browser, u string) (string, bool, error) {
	fast, err := b.Render(ctx, browserclient.RenderRequest{URL: u, Engine: "impersonate", IncludeHTML: true,
		IncludeText: true, Timeout: 10 * time.Second})
	if err == nil && fast.Status == 200 && !piJSWall(fast.HTML) {
		return fast.Text, true, nil
	}
	if err != nil {
		var be *browserclient.Error
		if asBrowserErr(err, &be) && be.Code == "egress_blocked" {
			return "", false, err // a blocked hop is a decision, not a reason to retry in a browser
		}
	}
	res, err := b.Render(ctx, browserclient.RenderRequest{URL: u, Engine: "browser", WaitUntil: "domcontentloaded",
		Scroll: true, MaxScrolls: 5, IncludeText: true, Timeout: 45 * time.Second})
	if err != nil {
		return "", false, err
	}
	return res.Text, true, nil
}
