// Package mcp is the Go port of the OpenGTM MCP server (plan 13). ports.go is
// FROZEN after Task 3: the tool tasks (10-14) consume Ctx, Tool, WriteTool, Args
// and the ports below; extend by adding files, not by editing these types.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/config"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadscore"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadsread"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/provcall"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// Config is the MCP server configuration (internal/config/mcp.go).
type Config = config.MCP

// Ctx is the identity of one authenticated session or request. It is built only
// from the resolved token (hash lookup, live membership re-check); tool
// arguments can never name a workspace.
type Ctx struct {
	WorkspaceID  string
	Slug         string
	Capabilities map[string]bool
	UserID       *int32
	TokenID      string
}

// Has reports whether the token holds capability cap.
func (c Ctx) Has(cap string) bool { return c.Capabilities[cap] }

// Tool is a read tool. Handler returns the text payload of the tool result and
// opens its own transaction with Deps.RunInWorkspace. Error contract: only
// *ArgError, ErrUnavailable (see IsUnavailable) and *HTTPError carry text that is
// safe to show a client (check with errors.As/Is); the pipeline must mask every
// other error (pgx and driver text) as "internal error".
type Tool struct {
	Name, Capability string
	Handler          func(ctx context.Context, c Ctx, args Args) (string, error)
}

// WriteTool is a write tool run by the write pipeline (role check, gate,
// reservation, idempotency, audit). Roles is the live role floor; Gate returns
// a refusal reason ("" = allowed) evaluated before any reservation; Apply runs
// inside the pipeline's workspace transaction and returns the text payload.
//
// Apply contract: a returned error rolls the transaction back and frees the cap
// slot; so does a result whose JSON object has an "error" key (the Python rule).
// Results without an "error" key commit and settle the cap, including
// {"status":"not_found"} and the idempotent "duplicate" replies (as Python does).
// Error text follows the Tool.Handler safe-error contract. Best-effort steps
// that must not abort the transaction run post-commit, never inside Apply.
type WriteTool struct {
	Name, Capability string
	Roles            []string
	Gate             func(cfg Config) string
	Apply            func(ctx context.Context, tx pgx.Tx, c Ctx, args Args) (result string, err error)
}

// ErrUnavailable is returned by a port that has no adapter in this build (D9).
var ErrUnavailable = errors.New("not available in this build")

// IsUnavailable reports whether err means "no adapter or no provider runner".
func IsUnavailable(err error) bool {
	return errors.Is(err, ErrUnavailable) || errors.Is(err, provcall.ErrUnavailable)
}

// HTTPError is a REST-style rejection surfaced by AutomationPort.ValidateRule.
// Detail is the REST detail text; a non-string Python detail (a repr) is an
// accepted deviation (testdata/contract/README.md, MCP section).
type HTTPError struct {
	Status int
	Detail string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("http %d: %s", e.Status, e.Detail) }

// ArgError is a mistyped tool argument (D3); an absent key is reported by the
// "present" result, so callers render "<field> is required" themselves.
type ArgError struct{ Field, Want string }

func (e *ArgError) Error() string { return e.Field + " must be " + e.Want }

// Args is the decoded `arguments` object of a tools/call. Values come from
// encoding/json (float64 numbers) or json.Number; helpers never panic. An
// absent key and an explicit null are both "not present".
type Args map[string]any

func (a Args) get(key string) (any, bool) {
	v, ok := a[key]
	if !ok || v == nil {
		return nil, false
	}
	return v, true
}

// asInt converts a JSON number to int64 only when it is integral and exact.
func asInt(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case int32:
		return int64(n), true
	case float64:
		if n != math.Trunc(n) || n < -(1<<53) || n > 1<<53 {
			return 0, false
		}
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, false
		}
		return i, true
	}
	return 0, false
}

// Int returns the integer argument key. Booleans, strings and fractional or
// out-of-range numbers are an ArgError.
func (a Args) Int(key string) (int64, bool, error) {
	v, ok := a.get(key)
	if !ok {
		return 0, false, nil
	}
	i, ok := asInt(v)
	if !ok {
		return 0, true, &ArgError{Field: key, Want: "an integer"}
	}
	return i, true, nil
}

// String returns the string argument key.
func (a Args) String(key string) (string, bool, error) {
	v, ok := a.get(key)
	if !ok {
		return "", false, nil
	}
	s, ok := v.(string)
	if !ok {
		return "", true, &ArgError{Field: key, Want: "a string"}
	}
	return s, true, nil
}

// StringList returns the array-of-strings argument key.
func (a Args) StringList(key string) ([]string, bool, error) {
	v, ok := a.get(key)
	if !ok {
		return nil, false, nil
	}
	arr, ok := v.([]any)
	if !ok {
		if ss, isSS := v.([]string); isSS {
			return ss, true, nil
		}
		return nil, true, &ArgError{Field: key, Want: "an array of strings"}
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		s, ok := e.(string)
		if !ok {
			return nil, true, &ArgError{Field: key, Want: "an array of strings"}
		}
		out = append(out, s)
	}
	return out, true, nil
}

// IntList returns the array-of-integers argument key.
func (a Args) IntList(key string) ([]int64, bool, error) {
	v, ok := a.get(key)
	if !ok {
		return nil, false, nil
	}
	if ii, isII := v.([]int64); isII {
		return ii, true, nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, true, &ArgError{Field: key, Want: "an array of integers"}
	}
	out := make([]int64, 0, len(arr))
	for _, e := range arr {
		i, ok := asInt(e)
		if !ok {
			return nil, true, &ArgError{Field: key, Want: "an array of integers"}
		}
		out = append(out, i)
	}
	return out, true, nil
}

// RunInWorkspace runs fn through d.InWorkspace, or returns ErrUnavailable when
// no runner is wired.
func (d Deps) RunInWorkspace(ctx context.Context, ws string, fn func(pgx.Tx) error) error {
	if d.InWorkspace == nil {
		return ErrUnavailable
	}
	return d.InWorkspace(ctx, ws, fn)
}

// Lead, ListParams and StatsResult are the plan 05 read models; Stats is the
// ordered get_stats result (key order is wire parity).
type (
	Lead        = leadsread.Lead
	ListParams  = leadsread.ListParams
	StatsResult = leadsread.StatsResult
	// Column is a stored workbook column object (insertion order preserved).
	Column = wbcrud.Column
)

// LeadFields is a lead write payload keyed by column name with untyped JSON
// values. The _LEAD_WRITE_FIELDS allowlist (19 names, null values dropped) is
// enforced by the tool before it calls the port, not by the port.
type LeadFields map[string]any

// LeadPort is the workspace-scoped lead store. Every method runs inside the
// caller's tenancy.InWorkspace transaction; a foreign or unknown id is a nil
// lead / false, never an error revealing existence.
type LeadPort interface {
	List(ctx context.Context, tx pgx.Tx, p ListParams) ([]Lead, error)
	ByID(ctx context.Context, tx pgx.Tx, id int64) (*Lead, error)
	Stats(ctx context.Context, tx pgx.Tx) (StatsResult, error)
	Upsert(ctx context.Context, tx pgx.Tx, f LeadFields) (id int64, err error)
	UpdateFields(ctx context.Context, tx pgx.Tx, id int64, f LeadFields) error
	SetStatus(ctx context.Context, tx pgx.Tx, id int64, status string) error
	Delete(ctx context.Context, tx pgx.Tx, id int64) error
}

// VerifyResult is the email verification cascade outcome (verify_email payload).
type VerifyResult struct {
	Status      string // valid | invalid | catch_all | unknown
	Deliverable bool
	Confidence  float64
	Source      string
	Detail      string
}

// scoreLead is scoring.score_lead (internal/leadscore) without a source reliability: local CPU, no port needed.
func scoreLead(l Lead) int { return leadscore.Score(l, nil) }

// EnrichPort reaches the cost-bearing providers. Py runs one provider through the provider-call port
// (rpclocal: the in-process providers) and returns its output; the sentinels provcall.ErrUnavailable,
// ErrUnknownProvider and ErrUnauthorized pass through unchanged.
type EnrichPort interface {
	VerifyEmail(ctx context.Context, ws, email string) (VerifyResult, error)
	Py(ctx context.Context, ws, provider string, in any) (json.RawMessage, error)
}

// WorkbookPort creates and deletes workbooks inside the pipeline transaction.
// For every port method taking ws and a tx, ws must equal Ctx.WorkspaceID (the
// workspace the tx is bound to); tools pass c.WorkspaceID and nothing else.
type WorkbookPort interface {
	Create(ctx context.Context, tx pgx.Tx, ws, name, description string, columns []Column) (id string, err error)
	Delete(ctx context.Context, tx pgx.Tx, ws, id string) (found bool, err error)
}

// QueueSendRequest is the durable send job (the automations send_email action).
type QueueSendRequest struct {
	SequenceID     string
	StepNumber     int
	Subject        string
	BodyHTML       string
	LeadID         int64
	LeadData       map[string]any
	IdempotencyKey string
}

// SendResult mirrors ActionResult: Status success|failed|skipped.
type SendResult struct {
	Status, Summary, SkipReason, Error string
}

// OutreachPort is the plan 09 outreach store and send queue (D9).
type OutreachPort interface {
	SequenceExists(ctx context.Context, tx pgx.Tx, ws, seqID string) (bool, error)
	IsSuppressed(ctx context.Context, tx pgx.Tx, ws, email string) (bool, error)
	// Enroll returns enrolled=false when the lead is already enrolled.
	Enroll(ctx context.Context, tx pgx.Tx, ws, seqID string, leadID int64, email, consentSource string) (enrolled bool, err error)
	QueueSend(ctx context.Context, tx pgx.Tx, ws string, r QueueSendRequest) (SendResult, error)
}

// RuleInput is a create_automation request after argument validation.
type RuleInput struct {
	Name              string
	TriggerType       *string // nil = missing (Python passes None through to _validate_rule)
	TriggerConfig     json.RawMessage
	Condition         string
	Actions           json.RawMessage
	ScopeWorkbookIDs  []string
	Enabled           bool
	StopOnError       bool
	MaxSpendUSDPerDay *float64
	MaxActionsPerDay  *int64
	CreatedBy         *int32
}

// AutomationPort is the plan 08 trigger store (D9). ValidateRule returns
// *HTTPError for a REST-style rejection; CreateTrigger returns the stored rule
// as an ordered JSON object (Trigger.to_api). BootstrapSchedule is a best-effort
// post-commit hook (Python runs it after the commit and swallows errors): the
// pipeline calls it only after the write transaction committed, outside any tx,
// and logs (never returns) its error; the adapter opens its own transaction.
type AutomationPort interface {
	CountRules(ctx context.Context, tx pgx.Tx, ws string) (int, error)
	ValidateRule(ctx context.Context, tx pgx.Tx, ws string, r RuleInput) *HTTPError
	CreateTrigger(ctx context.Context, tx pgx.Tx, ws string, r RuleInput) (json.RawMessage, error)
	BootstrapSchedule(ctx context.Context, ws, id string) error
}

// Deps wires the ports. A nil port is allowed: the accessors return a port whose
// every method answers ErrUnavailable.
type Deps struct {
	// InWorkspace runs fn in a transaction bound to workspace ws (production:
	// tenancy.InWorkspace with the pool closed over). Reads, audit and cap
	// reservation all go through it. Nil means no database: RunInWorkspace
	// answers ErrUnavailable. Tests use nilTxRunner (fn gets a nil tx).
	InWorkspace func(ctx context.Context, ws string, fn func(pgx.Tx) error) error

	// IdentityTx runs fn in a transaction WITHOUT setting app.workspace_id, for
	// the identity plane (mcp_tokens, workspaces, workspace_members,
	// app_settings: no RLS). Optional; when nil, token resolution falls back to
	// InWorkspace with a placeholder id.
	IdentityTx func(ctx context.Context, fn func(pgx.Tx) error) error

	Cfg        Config
	Leads      LeadPort
	Enrich     EnrichPort
	Workbooks  WorkbookPort
	Outreach   OutreachPort
	Automation AutomationPort
}

// LeadPort returns d.Leads or an always-unavailable port.
func (d Deps) LeadPort() LeadPort {
	if d.Leads == nil {
		return noLeads{}
	}
	return d.Leads
}

// EnrichPort returns d.Enrich or an always-unavailable port.
func (d Deps) EnrichPort() EnrichPort {
	if d.Enrich == nil {
		return noEnrich{}
	}
	return d.Enrich
}

// WorkbookPort returns d.Workbooks or an always-unavailable port.
func (d Deps) WorkbookPort() WorkbookPort {
	if d.Workbooks == nil {
		return noWorkbooks{}
	}
	return d.Workbooks
}

// OutreachPort returns d.Outreach or an always-unavailable port.
func (d Deps) OutreachPort() OutreachPort {
	if d.Outreach == nil {
		return noOutreach{}
	}
	return d.Outreach
}

// AutomationPort returns d.Automation or an always-unavailable port.
func (d Deps) AutomationPort() AutomationPort {
	if d.Automation == nil {
		return noAutomation{}
	}
	return d.Automation
}

type noLeads struct{}

func (noLeads) List(context.Context, pgx.Tx, ListParams) ([]Lead, error) { return nil, ErrUnavailable }
func (noLeads) ByID(context.Context, pgx.Tx, int64) (*Lead, error)       { return nil, ErrUnavailable }
func (noLeads) Stats(context.Context, pgx.Tx) (StatsResult, error) {
	return StatsResult{}, ErrUnavailable
}
func (noLeads) Upsert(context.Context, pgx.Tx, LeadFields) (int64, error) {
	return 0, ErrUnavailable
}
func (noLeads) UpdateFields(context.Context, pgx.Tx, int64, LeadFields) error { return ErrUnavailable }
func (noLeads) SetStatus(context.Context, pgx.Tx, int64, string) error        { return ErrUnavailable }
func (noLeads) Delete(context.Context, pgx.Tx, int64) error                   { return ErrUnavailable }

type noEnrich struct{}

func (noEnrich) VerifyEmail(context.Context, string, string) (VerifyResult, error) {
	return VerifyResult{}, ErrUnavailable
}
func (noEnrich) Py(context.Context, string, string, any) (json.RawMessage, error) {
	return nil, ErrUnavailable
}

type noWorkbooks struct{}

func (noWorkbooks) Create(context.Context, pgx.Tx, string, string, string, []Column) (string, error) {
	return "", ErrUnavailable
}
func (noWorkbooks) Delete(context.Context, pgx.Tx, string, string) (bool, error) {
	return false, ErrUnavailable
}

type noOutreach struct{}

func (noOutreach) SequenceExists(context.Context, pgx.Tx, string, string) (bool, error) {
	return false, ErrUnavailable
}
func (noOutreach) IsSuppressed(context.Context, pgx.Tx, string, string) (bool, error) {
	return false, ErrUnavailable
}
func (noOutreach) Enroll(context.Context, pgx.Tx, string, string, int64, string, string) (bool, error) {
	return false, ErrUnavailable
}
func (noOutreach) QueueSend(context.Context, pgx.Tx, string, QueueSendRequest) (SendResult, error) {
	return SendResult{}, ErrUnavailable
}

type noAutomation struct{}

func (noAutomation) CountRules(context.Context, pgx.Tx, string) (int, error) {
	return 0, ErrUnavailable
}
func (noAutomation) ValidateRule(context.Context, pgx.Tx, string, RuleInput) *HTTPError {
	return &HTTPError{Status: 503, Detail: ErrUnavailable.Error()}
}
func (noAutomation) CreateTrigger(context.Context, pgx.Tx, string, RuleInput) (json.RawMessage, error) {
	return nil, ErrUnavailable
}
func (noAutomation) BootstrapSchedule(context.Context, string, string) error {
	return ErrUnavailable
}

// withSpendScope marks the provider calls made under ctx as paid work of the workspace spend ledger.
func withSpendScope(ctx context.Context, run, row string) context.Context {
	return provcall.WithSpendScope(ctx, provcall.SpendScope{Run: run, Row: row})
}
