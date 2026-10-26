package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/mcp/pyjson"
)

// Test seams: production values are the real reservation and audit writers.
var (
	reserveCap  = Reserve
	settleCap   = Settle
	releaseCap  = Release
	recordAudit = Record
)

const (
	maxEmailLen   = 320
	maxCompanyLen = 500

	msgEnrichFailed      = "enrichment failed"
	msgEnrichUnavailable = "enrichment backend unavailable"
	msgEnrichTimeout     = "enrichment timed out"
	msgEnrichCap         = "mcp enrich cap exceeded"
	msgLeadNotFound      = "Lead not found"
)

// enrichTools are the Python-RPC and cost-bearing read tools (D7, D10).
// verify_email, enrich_company and get_hiring_signals spend provider credits and
// probe third-party hosts behind a plain leads:read grant, so each call is
// audited, counted against MCP_MAX_ENRICH_PER_DAY and bounded by
// MCP_TOOL_TIMEOUT_SECONDS. No Python exception text ever reaches a client.
func enrichTools(d Deps) []Tool {
	return []Tool{
		{Name: "verify_email", Capability: "leads:read", Handler: func(ctx context.Context, c Ctx, a Args) (string, error) {
			return verifyEmailTool(ctx, d, c, a)
		}},
		{Name: "enrich_company", Capability: "leads:read", Handler: func(ctx context.Context, c Ctx, a Args) (string, error) {
			return enrichCompanyTool(ctx, d, c, a)
		}},
		{Name: "get_hiring_signals", Capability: "leads:read", Handler: func(ctx context.Context, c Ctx, a Args) (string, error) {
			return hiringSignalsTool(ctx, d, c, a)
		}},
		{Name: "score_lead", Capability: "leads:read", Handler: func(ctx context.Context, c Ctx, a Args) (string, error) {
			return scoreLeadTool(ctx, d, c, a)
		}},
	}
}

func toolDeadline(ctx context.Context, d Deps) (context.Context, context.CancelFunc) {
	sec := d.Cfg.ToolTimeoutSeconds
	if sec <= 0 {
		sec = 120
	}
	return context.WithTimeout(ctx, time.Duration(sec)*time.Second)
}

// enrichMessage maps a port error to a client-safe message.
func enrichMessage(err error) string {
	switch {
	case IsUnavailable(err):
		return msgEnrichUnavailable
	case errors.Is(err, context.DeadlineExceeded):
		return msgEnrichTimeout
	}
	return msgEnrichFailed
}

func dumpObj(o *pyjson.Obj, indent int) (string, error) {
	b, err := pyjson.Dumps(o, indent)
	return string(b), err
}

func requireString(a Args, key string, max int) (string, error) {
	s, ok, err := a.String(key)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", &ArgError{Field: key, Want: "required"}
	}
	if len([]rune(s)) > max {
		return "", &ArgError{Field: key, Want: "at most " + itoa(max) + " characters"}
	}
	return s, nil
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// leadIDArg reads the required integer lead_id.
func leadIDArg(a Args) (int64, error) {
	id, ok, err := a.Int("lead_id")
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, &ArgError{Field: "lead_id", Want: "required"}
	}
	return id, nil
}

// runCost runs one cost-bearing call: reserve against the enrich cap, run,
// settle or release the slot, and audit the outcome. shape renders a failure in
// the tool's own error shape. The slot is committed before the (slow) provider
// call and released afterwards on failure, so no transaction is held open.
func runCost(ctx context.Context, d Deps, c Ctx, tool string, args Args, shape func(msg string) string, do func(context.Context) (string, error)) string {
	var res Reservation
	reserved := false
	if d.Cfg.MaxEnrichPerDay > 0 {
		err := d.RunInWorkspace(ctx, c.WorkspaceID, func(tx pgx.Tx) error {
			var e error
			res, e = reserveCap(ctx, tx, c.WorkspaceID, EnrichRule, d.Cfg.MaxEnrichPerDay, "")
			return e
		})
		if err != nil {
			slog.Error("mcp enrich reservation failed", "tool", tool, "err", err.Error())
			recordAudit(ctx, d, c, tool, args, AuditOutcome{Status: "error", Error: msgEnrichFailed})
			return shape(enrichMessage(err))
		}
		if !res.OK {
			recordAudit(ctx, d, c, tool, args, AuditOutcome{Status: "capped", Error: msgEnrichCap})
			return shape(msgEnrichCap)
		}
		reserved = true
	}
	finished := false
	defer func() { // a panic in do must not leak the held slot (the server recovers it)
		if reserved && !finished {
			finishCap(ctx, d, c, res, releaseCap)
		}
	}()
	text, err := do(ctx)
	finished = true
	if err != nil {
		slog.Error("mcp enrich failed", "tool", tool, "err", err.Error())
		if reserved {
			finishCap(ctx, d, c, res, releaseCap)
		}
		msg := enrichMessage(err)
		recordAudit(ctx, d, c, tool, args, AuditOutcome{Status: "error", Error: msg})
		return shape(msg)
	}
	if reserved {
		finishCap(ctx, d, c, res, settleCap)
	}
	// mcp.* providers and the verifier cascade report no per-call cost (cost_usd 0).
	recordAudit(ctx, d, c, tool, args, AuditOutcome{Status: "ok"})
	return text
}

// finishCap settles or releases a held slot in its own transaction; it survives a
// cancelled or expired tool context so a timeout frees the slot.
func finishCap(ctx context.Context, d Deps, c Ctx, r Reservation, f func(context.Context, pgx.Tx, Reservation) error) {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := d.RunInWorkspace(fctx, c.WorkspaceID, func(tx pgx.Tx) error { return f(fctx, tx, r) }); err != nil {
		slog.Error("mcp enrich cap update failed", "err", err.Error())
	}
}

func errShape(kv ...any) func(string) string {
	return func(msg string) string {
		pairs := append(append([]any{}, kv...), "error", msg)
		s, err := dumpObj(pyjson.NewObj(pairs...), 0)
		if err != nil {
			return errorText(msgEnrichFailed)
		}
		return s
	}
}

func verifyEmailTool(ctx context.Context, d Deps, c Ctx, a Args) (string, error) {
	email, err := requireString(a, "email", maxEmailLen)
	if err != nil {
		return argText(err), nil
	}
	ctx, cancel := toolDeadline(ctx, d)
	defer cancel()
	return runCost(ctx, d, c, "verify_email", a, errShape("email", email), func(ctx context.Context) (string, error) {
		vr, err := d.EnrichPort().VerifyEmail(ctx, c.WorkspaceID, email)
		if err != nil {
			return "", err
		}
		return dumpObj(pyjson.NewObj(
			"email", email,
			"status", vr.Status,
			"valid", vr.Status == "valid",
			"deliverable", vr.Deliverable,
			"confidence", vr.Confidence,
			"source", vr.Source,
			"detail", vr.Detail,
		), 2)
	}), nil
}

// argText renders an argument error as the tool's {"error": ...} payload: a
// missing field reads "<field> is required", anything else "<field> must be ...".
func argText(err error) string {
	var ae *ArgError
	if errors.As(err, &ae) && ae.Want == "required" {
		return errorText(ae.Field + " is required")
	}
	return errorText(safeErrorText(err))
}

func enrichCompanyTool(ctx context.Context, d Deps, c Ctx, a Args) (string, error) {
	id, err := leadIDArg(a)
	if err != nil {
		return argText(err), nil
	}
	ctx, cancel := toolDeadline(ctx, d)
	defer cancel()
	// Resolve the lead through the workspace-scoped store first: a cross-tenant id
	// is a miss, so enrichment spend can only be triggered for the caller's leads.
	var found bool
	if err := d.RunInWorkspace(ctx, c.WorkspaceID, func(tx pgx.Tx) error {
		l, e := d.LeadPort().ByID(ctx, tx, id)
		found = e == nil && l != nil
		return e
	}); err != nil {
		return "", err
	}
	if !found {
		recordAudit(ctx, d, c, "enrich_company", a, AuditOutcome{Status: "error", Error: "lead not found"})
		return errorText(msgLeadNotFound), nil
	}
	return runCost(ctx, d, c, "enrich_company", a, errShape(), func(ctx context.Context) (string, error) {
		f, err := pyFields(ctx, d, c, "mcp.enrich_company", map[string]any{"lead_id": id})
		if err != nil {
			return "", err
		}
		if isNotFound(f) {
			return errorText(msgLeadNotFound), nil
		}
		ef, prov := f.get("enriched_fields"), f.get("provider")
		if len(ef) == 0 {
			ef = json.RawMessage(`{}`)
		}
		if len(prov) == 0 {
			prov = json.RawMessage(`""`)
		}
		return dumpObj(pyjson.NewObj("enriched_fields", ef, "provider", prov), 2)
	}), nil
}

func hiringSignalsTool(ctx context.Context, d Deps, c Ctx, a Args) (string, error) {
	company, err := requireString(a, "company", maxCompanyLen)
	if err != nil {
		return argText(err), nil
	}
	leadID, _, err := a.Int("lead_id")
	if err != nil {
		return argText(err), nil
	}
	ctx, cancel := toolDeadline(ctx, d)
	defer cancel()
	return runCost(ctx, d, c, "get_hiring_signals", a, errShape("company", company), func(ctx context.Context) (string, error) {
		f, err := pyFields(ctx, d, c, "mcp.get_hiring_signals", map[string]any{"company": company, "lead_id": leadID})
		if err != nil {
			return "", err
		}
		var hiring bool
		if raw := f.get("hiring"); len(raw) > 0 {
			if err := json.Unmarshal(raw, &hiring); err != nil {
				return "", errors.New("bad hiring field")
			}
		}
		data := f.get("data")
		if len(data) == 0 {
			data = json.RawMessage(`{}`)
		}
		return dumpObj(pyjson.NewObj("company", company, "hiring", hiring, "data", data), 2)
	}), nil
}

func scoreLeadTool(ctx context.Context, d Deps, c Ctx, a Args) (string, error) {
	id, err := leadIDArg(a)
	if err != nil {
		return argText(err), nil
	}
	ctx, cancel := toolDeadline(ctx, d)
	defer cancel()
	// Scoring is local CPU in Python and bills nothing: no cap, no audit row.
	f, err := pyFields(ctx, d, c, "mcp.score_lead", map[string]any{"lead_id": id})
	if err != nil {
		slog.Error("mcp score_lead failed", "err", err.Error())
		return errorText(enrichMessage(err)), nil
	}
	if isNotFound(f) {
		return errorText(msgLeadNotFound), nil
	}
	var body any
	if v, ok := f.m["value"]; ok { // Python score_lead returns a bare int, carried as {"value": n}
		body = v
	} else {
		body = f.raw
	}
	b, err := pyjson.Dumps(body, 2)
	if err != nil {
		return errorText(msgEnrichFailed), nil
	}
	return string(b), nil
}

// pyFields calls a Python RPC tool provider and returns the tool result, which
// the worker carries in output.fields. An object without a "fields" key is taken
// as the result itself.
func pyFields(ctx context.Context, d Deps, c Ctx, provider string, in any) (rpcFields, error) {
	raw, err := d.EnrichPort().Py(ctx, c.WorkspaceID, provider, in)
	if err != nil {
		return rpcFields{}, err
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil || out == nil {
		return rpcFields{}, errors.New("python rpc: output is not an object")
	}
	if fr, ok := out["fields"]; ok {
		raw = fr
		out = nil
		if err := json.Unmarshal(fr, &out); err != nil || out == nil {
			return rpcFields{}, errors.New("python rpc: fields is not an object")
		}
	}
	return rpcFields{m: out, raw: raw}, nil
}

// rpcFields is the decoded tool result; raw keeps the Python key order.
type rpcFields struct {
	m   map[string]json.RawMessage
	raw json.RawMessage
}

func (f rpcFields) get(k string) json.RawMessage { return f.m[k] }

func isNotFound(f rpcFields) bool {
	var nf bool
	return json.Unmarshal(f.m["not_found"], &nf) == nil && nf
}
