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
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/leadsread"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/pyworker"
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

// Tool is a read tool. Handler returns the text payload of the tool result.
type Tool struct {
	Name, Capability string
	Handler          func(ctx context.Context, c Ctx, args Args) (string, error)
}

// WriteTool is a write tool run by the write pipeline (role check, gate,
// reservation, idempotency, audit). Roles is the live role floor; Gate returns
// a refusal reason ("" = allowed) evaluated before any reservation; Apply runs
// inside the pipeline's workspace transaction and returns the text payload.
type WriteTool struct {
	Name, Capability string
	Roles            []string
	Gate             func(cfg Config) string
	Apply            func(ctx context.Context, tx pgx.Tx, c Ctx, args Args) (result string, err error)
}

// ErrUnavailable is returned by a port that has no adapter in this build (D9).
var ErrUnavailable = errors.New("not available in this build")

// IsUnavailable reports whether err means "no adapter or no Python worker".
func IsUnavailable(err error) bool {
	return errors.Is(err, ErrUnavailable) || errors.Is(err, pyworker.ErrUnavailable)
}

// HTTPError is a REST-style rejection surfaced by AutomationPort.ValidateRule.
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

// Lead, ListParams and StatsResult are the plan 05 read models; Stats is the
// ordered get_stats result (key order is wire parity).
type (
	Lead        = leadsread.Lead
	ListParams  = leadsread.ListParams
	StatsResult = leadsread.StatsResult
	// Column is a stored workbook column object (insertion order preserved).
	Column = wbcrud.Column
)

// LeadFields is an allowlisted, non-null lead write payload keyed by column
// name (the _LEAD_WRITE_FIELDS set); values are strings.
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

// EnrichPort reaches the cost-bearing providers. Py calls the Python worker RPC
// (internal/pyworker) and returns its output; the sentinels pyworker.ErrUnavailable,
// ErrUnknownProvider and ErrUnauthorized pass through unchanged.
type EnrichPort interface {
	VerifyEmail(ctx context.Context, ws, email string) (VerifyResult, error)
	Py(ctx context.Context, ws, provider string, in any) (json.RawMessage, error)
}

// WorkbookPort creates and deletes workbooks inside the pipeline transaction.
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

// OutreachPort is the plan 08 outreach store and send queue (D9).
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
	TriggerType       string
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

// AutomationPort is the plan 07 trigger store (D9). ValidateRule returns
// *HTTPError for a REST-style rejection; CreateTrigger returns the stored rule
// as an ordered JSON object (Trigger.to_api).
type AutomationPort interface {
	CountRules(ctx context.Context, tx pgx.Tx, ws string) (int, error)
	ValidateRule(ctx context.Context, tx pgx.Tx, ws string, r RuleInput) *HTTPError
	CreateTrigger(ctx context.Context, tx pgx.Tx, ws string, r RuleInput) (json.RawMessage, error)
	BootstrapSchedule(ctx context.Context, tx pgx.Tx, ws string, id string) error
}

// Deps wires the ports. A nil port is allowed: the accessors return a port whose
// every method answers ErrUnavailable.
type Deps struct {
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
func (noAutomation) BootstrapSchedule(context.Context, pgx.Tx, string, string) error {
	return ErrUnavailable
}
