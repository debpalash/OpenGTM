package playbooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/aicol"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/canonjson"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/pyworker"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// Researcher runs one research_column step for a workspace.
type Researcher interface {
	Research(ctx context.Context, ws string, req ResearchRequest) (ResearchResult, error)
}

// ResearchRequest is the research_column input. CallID is the RPC idempotency
// key (<run_id>:<lead_id>:<step key>:<attempt>); it never reaches the input.
type ResearchRequest struct {
	CallID         string
	PromptTemplate string
	Context        map[string]any
	MaxSteps       int
	OutputFormat   string
	CellBudgetUSD  float64
}

// ResearchResult is the research_column output. Value is any JSON-shaped value
// (string, *obj, []any, json.Number, ...). Python's budget enforcement happens
// inside research_column; the RPC reports no cost back.
type ResearchResult struct {
	Success  bool
	Value    any
	Metadata map[string]any
	Error    string

	meta *obj // Metadata with the Python key order, when it came from the worker
}

const (
	researchProvider = "research_column"
	// maxValueBytes is the cap of a stored result value (the RPC's own cap).
	maxValueBytes = 64 * 1024
	maxErrorChars = 1000
	// DefaultStepTimeout is PLAYBOOK_STEP_TIMEOUT_SECONDS' default.
	DefaultStepTimeout = 120 * time.Second
)

// StepTimeout reads PLAYBOOK_STEP_TIMEOUT_SECONDS (default 120s; invalid or
// non-positive values fall back to the default).
func StepTimeout() time.Duration {
	f, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv("PLAYBOOK_STEP_TIMEOUT_SECONDS")), 64)
	if err != nil || f <= 0 || math.IsNaN(f) || math.IsInf(f, 0) || f > 3600 {
		return DefaultStepTimeout
	}
	return time.Duration(f * float64(time.Second))
}

type pyResearcher struct {
	c       *pyworker.Client
	timeout time.Duration
}

// NewPyResearcher runs steps on the Python worker's research_column provider.
// The workspace travels in the RPC envelope only, never in the input.
func NewPyResearcher(c *pyworker.Client, timeout time.Duration) Researcher {
	if timeout <= 0 {
		timeout = DefaultStepTimeout
	}
	return &pyResearcher{c: c, timeout: timeout}
}

// nativeResearcher runs research_column steps in process (GO_NATIVE_AI_COLUMNS, plan 15
// WP-AICOLS): the Go research agent over the workspace's LLM client, search and page fetch.
// It never reports pyworker.ErrUnavailable, so a run is never handed back for it.
type nativeResearcher struct {
	deps    func(ctx context.Context, workspaceID string) aicol.Deps
	timeout time.Duration
}

// NewNativeResearcher runs steps with aicol.PlaybookResearch; deps builds the column
// collaborators of a workspace. timeout bounds one step (PLAYBOOK_STEP_TIMEOUT_SECONDS).
func NewNativeResearcher(deps func(ctx context.Context, workspaceID string) aicol.Deps, timeout time.Duration) Researcher {
	if timeout <= 0 {
		timeout = DefaultStepTimeout
	}
	return &nativeResearcher{deps: deps, timeout: timeout}
}

func (n *nativeResearcher) Research(ctx context.Context, ws string, req ResearchRequest) (ResearchResult, error) {
	sctx, cancel := context.WithTimeout(ctx, n.timeout)
	defer cancel()
	res, err := aicol.PlaybookResearch(sctx, n.deps(sctx, ws), aicol.PlaybookRequest{PromptTemplate: req.PromptTemplate,
		Context: req.Context, MaxSteps: req.MaxSteps, OutputFormat: req.OutputFormat, CellBudgetUSD: req.CellBudgetUSD})
	if ctx.Err() != nil {
		return ResearchResult{}, ctx.Err()
	}
	if err != nil {
		return failed("research step timed out"), nil
	}
	r := ResearchResult{Success: res.Success, Value: res.Value, Error: res.Error}
	if o := toObj(res.Metadata); o != nil && len(o.keys) > 0 {
		r.meta, r.Metadata = o, o.vals
	}
	return r, nil
}

// toObj converts an aicol.Obj tree to the ledger's ordered object.
func toObj(o *aicol.Obj) *obj {
	if o == nil {
		return nil
	}
	out := newObj()
	for _, k := range o.Keys {
		out.set(k, toOrdered(o.Vals[k]))
	}
	return out
}

func toOrdered(v any) any {
	switch t := v.(type) {
	case *aicol.Obj:
		return toObj(t)
	case []any:
		l := make([]any, len(t))
		for i, e := range t {
			l[i] = toOrdered(e)
		}
		return l
	}
	return v
}

func isUnavailable(err error) bool { return errors.Is(err, pyworker.ErrUnavailable) }

func failed(msg string) ResearchResult { return ResearchResult{Error: msg} }

func (p *pyResearcher) Research(ctx context.Context, ws string, req ResearchRequest) (ResearchResult, error) {
	if math.IsNaN(req.CellBudgetUSD) || math.IsInf(req.CellBudgetUSD, 0) {
		return failed("invalid max_steps or cell_budget_usd"), nil
	}
	cx := req.Context
	if cx == nil {
		cx = map[string]any{}
	}
	in := newObj()
	in.set("prompt_template", req.PromptTemplate)
	in.set("context", cx)
	in.set("max_steps", req.MaxSteps)
	in.set("output_format", req.OutputFormat)
	in.set("cell_budget_usd", req.CellBudgetUSD)
	input := raw(in)
	if !json.Valid(input) {
		return failed("research input is not valid JSON"), nil
	}
	res, err := p.c.Call(ctx, pyworker.ProviderCall{CallID: req.CallID, Provider: researchProvider, WorkspaceID: ws, Input: input, Timeout: p.timeout})
	if err != nil {
		return ResearchResult{}, err
	}
	if res.Err != nil {
		return ResearchResult{}, res.Err
	}
	if res.Status != "ok" {
		msg := res.Error
		if msg == "" {
			msg = "python worker call " + res.Status
		}
		return failed(msg), nil
	}
	doc, derr := decodeDoc(res.Output)
	out, ok := doc.(*obj)
	if derr != nil || !ok || out == nil {
		return failed("python worker returned malformed research output"), nil
	}
	r := ResearchResult{}
	if v, ok := out.get("success"); ok {
		r.Success = truthy(v)
	}
	r.Value, _ = out.get("value")
	if m, ok := out.get("metadata"); ok && truthy(m) {
		if mo, ok := m.(*obj); ok {
			r.meta, r.Metadata = mo, mo.vals
		}
	}
	if e, ok := out.get("error"); ok && e != nil {
		r.Error = valueText(e)
	}
	return r, nil
}

// truthy is bool(v) for decoded JSON values.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case json.Number:
		f, err := strconv.ParseFloat(string(x), 64)
		return err != nil || f != 0
	case int:
		return x != 0
	case int64:
		return x != 0
	case float64:
		return x != 0
	case []any:
		return len(x) > 0
	case *obj:
		return x != nil && len(x.keys) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// valueText is str(v or "").
func valueText(v any) string {
	if !truthy(v) {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return pyRepr(v)
}

func pyRepr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case string:
		return reprString(x)
	case json.Number:
		return string(x)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		if s, err := canonjson.FloatRepr(x); err == nil {
			return s
		}
		switch {
		case math.IsNaN(x):
			return "nan"
		case x > 0:
			return "inf"
		}
		return "-inf"
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = pyRepr(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case *obj:
		parts := make([]string, 0, len(x.keys))
		for _, k := range x.keys {
			parts = append(parts, reprString(k)+": "+pyRepr(x.vals[k]))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case map[string]any:
		o := newObj()
		for _, k := range sortedKeys(x) {
			o.set(k, x[k])
		}
		return pyRepr(o)
	}
	return fmt.Sprint(v)
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ { // tiny maps; insertion sort keeps this dependency-free
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

func reprString(s string) string {
	q := '\''
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		q = '"'
	}
	var b strings.Builder
	b.WriteRune(q)
	for _, r := range s {
		switch {
		case r == q || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == ' ' || (r > 0x20 && r != 0x7f && unicode.IsPrint(r)):
			b.WriteRune(r)
		case r < 0x100:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r < 0x10000:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
	}
	b.WriteRune(q)
	return b.String()
}

// capBytes cuts s to at most n bytes on a character boundary.
func capBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func capRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// dbText drops NUL bytes, which a PostgreSQL text column cannot hold.
func dbText(s string) string { return strings.ReplaceAll(s, "\x00", "") }

func pyStrip(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) })
}

// --- run engine ---

const (
	errRunPayload   = "playbook run requires workspace_id and run_id"
	cancelledByUser = "Cancelled by user"
)

type engine struct {
	d       Deps
	ws      string
	runID   string
	pb      gen.ResearchPlaybook
	run     gen.PbEngLockRunRow
	prev    gen.PbEngLockRunRow // run row before this attempt turned it running
	steps   []any
	stepErr error
	started int // members processed by this attempt
}

func (g *engine) in(ctx context.Context, fn func(q *gen.Queries) error) error {
	return tenancy.InWorkspace(ctx, g.d.Pool, g.ws, func(tx pgx.Tx) error { return fn(gen.New(tx)) })
}

// HandleRun is handle_playbook_run for one research_playbook_run job (named
// HandleRun because Run is the model type). Each database step is its own short
// workspace transaction; the research calls run outside any transaction.
//
// A pyworker.ErrUnavailable from the Researcher stops the attempt and is
// returned so the worker hands the job back: before this attempt processed a
// member the run is restored exactly as found; afterwards it is left pending.
func HandleRun(ctx context.Context, d Deps, workspaceID, runID string) error {
	if workspaceID == "" || runID == "" {
		return errors.New(errRunPayload)
	}
	g := &engine{d: d, ws: workspaceID, runID: runID}
	var members []gen.PbEngMembersRow
	done := false
	err := g.in(ctx, func(q *gen.Queries) error {
		row, err := q.PbEngLockRun(ctx, gen.PbEngLockRunParams{WorkspaceID: g.ws, ID: g.runID})
		if errors.Is(err, pgx.ErrNoRows) {
			done = true
			return nil
		}
		if err != nil {
			return fmt.Errorf("playbooks: run load: %w", err)
		}
		if row.Status == "completed" || row.Status == "cancelled" {
			done = true
			return nil
		}
		pb, err := q.PbGetPlaybook(ctx, gen.PbGetPlaybookParams{WorkspaceID: g.ws, ID: row.PlaybookID})
		if errors.Is(err, pgx.ErrNoRows) {
			return errors.New("playbook not found")
		}
		if err != nil {
			return fmt.Errorf("playbooks: run playbook: %w", err)
		}
		if _, err := q.PbEngStartRun(ctx, gen.PbEngStartRunParams{WorkspaceID: g.ws, ID: g.runID}); err != nil {
			return fmt.Errorf("playbooks: run start: %w", err)
		}
		members, err = q.PbEngMembers(ctx, gen.PbEngMembersParams{WorkspaceID: g.ws, AudienceID: row.AudienceID, RowLimit: max(row.MaxMembers, 0)})
		if err != nil {
			return fmt.Errorf("playbooks: run members: %w", err)
		}
		g.pb, g.run, g.prev = pb, row, row
		return nil
	})
	if err != nil || done {
		return err
	}
	if d.Researcher == nil {
		// Nothing ran yet: put the run back as found and hand the job back.
		_ = g.handBack(ctx, 0)
		return pyworker.ErrUnavailable
	}
	g.steps, g.stepErr = g.parseSteps()

	for _, m := range members {
		cancelled, err := g.cancelled(ctx)
		if err != nil || cancelled {
			return err
		}
		stop, err := g.member(ctx, m)
		if err != nil || stop {
			return err
		}
		g.started++
	}
	return g.finish(ctx)
}

// parseSteps is `run.steps_snapshot or [default step]`.
func (g *engine) parseSteps() ([]any, error) {
	def := func() []any {
		o := newObj()
		o.set("key", "result")
		o.set("name", "Research")
		o.set("prompt_template", g.run.PromptSnapshot)
		o.set("output_format", g.pb.OutputFormat)
		return []any{o}
	}
	doc, err := decodeDoc(g.run.StepsSnapshot)
	if err != nil || !truthy(doc) {
		if err != nil && len(strings.TrimSpace(string(g.run.StepsSnapshot))) > 0 {
			return nil, errors.New("invalid steps_snapshot")
		}
		return def(), nil
	}
	if list, ok := doc.([]any); ok {
		return list, nil
	}
	return nil, errors.New("invalid steps_snapshot")
}

func (g *engine) cancelled(ctx context.Context) (bool, error) {
	var st string
	err := g.in(ctx, func(q *gen.Queries) error {
		var err error
		st, err = q.PbEngRunStatus(ctx, gen.PbEngRunStatusParams{WorkspaceID: g.ws, ID: g.runID})
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, errors.New("playbook run not found")
	}
	if err != nil {
		return false, fmt.Errorf("playbooks: run status: %w", err)
	}
	return st == "cancelled", nil
}

// stepField is step[key] (a missing key is Python's KeyError text).
func stepField(step any, key string) (any, error) {
	o, ok := step.(*obj)
	if !ok {
		return nil, errors.New("step must be an object")
	}
	v, ok := o.get(key)
	if !ok {
		return nil, fmt.Errorf("'%s'", key)
	}
	return v, nil
}

func asText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return pyRepr(v)
}

// member runs every step of one audience member. stop reports that the run was
// cancelled (nothing further to do).
func (g *engine) member(ctx context.Context, m gen.PbEngMembersRow) (stop bool, err error) {
	var resID, attempts int32
	skipped := false
	if err := g.in(ctx, func(q *gen.Queries) error {
		row, err := q.PbEngBeginResult(ctx, gen.PbEngBeginResultParams{WorkspaceID: g.ws, RunID: g.runID, LeadID: m.LeadID})
		if errors.Is(err, pgx.ErrNoRows) {
			skipped = true // already success
			return nil
		}
		resID, attempts = row.ID, row.Attempts
		return err
	}); err != nil {
		return false, fmt.Errorf("playbooks: result begin: %w", err)
	}
	if skipped {
		return false, nil
	}

	outcome := g.steps2(ctx, m, resID, attempts)
	switch {
	case outcome.err != nil:
		return false, outcome.err
	case outcome.cancelled:
		return true, nil
	case outcome.unavailable:
		return true, g.handBack(ctx, resID)
	case outcome.exception != "":
		return false, g.write(ctx, func(q *gen.Queries) error {
			_, err := q.PbEngFailResult(ctx, gen.PbEngFailResultParams{WorkspaceID: g.ws, ID: resID, Error: dbText(capRunes(outcome.exception, maxErrorChars))})
			return err
		})
	}
	return false, g.write(ctx, func(q *gen.Queries) error {
		_, err := q.PbEngFinishResult(ctx, outcome.final)
		return err
	})
}

func (g *engine) write(ctx context.Context, fn func(q *gen.Queries) error) error {
	if err := g.in(ctx, fn); err != nil {
		return fmt.Errorf("playbooks: result write: %w", err)
	}
	return nil
}

type stepsOutcome struct {
	err         error // infrastructure failure: abort the job
	cancelled   bool
	unavailable bool
	exception   string // Python `except Exception`: member failed with this text
	final       gen.PbEngFinishResultParams
}

func (g *engine) steps2(ctx context.Context, m gen.PbEngMembersRow, resID, attempts int32) (out stepsOutcome) {
	fail := func(msg string) stepsOutcome { return stepsOutcome{exception: msg} }
	if g.stepErr != nil {
		return fail(g.stepErr.Error())
	}
	cx := map[string]any{}
	if doc, err := decodeDoc([]byte(m.Snapshot)); err != nil && strings.TrimSpace(m.Snapshot) != "" {
		return fail("invalid member snapshot")
	} else if o, ok := doc.(*obj); ok && o != nil {
		for _, k := range o.keys {
			cx[k] = o.vals[k]
		}
	} else if truthy(doc) {
		return fail("member snapshot must be an object")
	}

	total := len(g.steps)
	var outs []any
	completed := 0
	var last ResearchResult
	for _, step := range g.steps {
		cancelled, err := g.cancelled(ctx)
		if err != nil {
			return stepsOutcome{err: err}
		}
		if cancelled {
			if err := g.write(ctx, func(q *gen.Queries) error {
				_, err := q.PbEngCancelResult(ctx, gen.PbEngCancelResultParams{WorkspaceID: g.ws, ID: resID})
				return err
			}); err != nil {
				return stepsOutcome{err: err}
			}
			return stepsOutcome{cancelled: true}
		}
		tmpl, err := stepField(step, "prompt_template")
		if err != nil {
			return fail(err.Error())
		}
		tmplText, ok := tmpl.(string)
		if !ok {
			return fail("prompt_template required")
		}
		format := any("text")
		if v, ok := step.(*obj).get("output_format"); ok {
			format = v
		}
		key, err := stepField(step, "key")
		if err != nil {
			return fail(err.Error())
		}
		name, err := stepField(step, "name")
		if err != nil {
			return fail(err.Error())
		}
		keyText := asText(key)

		cc := make(map[string]any, len(cx))
		for k, v := range cx {
			cc[k] = v
		}
		res, err := g.d.Researcher.Research(ctx, g.ws, ResearchRequest{
			CallID:         fmt.Sprintf("%s:%d:%s:%d", g.runID, m.LeadID, keyText, attempts),
			PromptTemplate: tmplText, Context: cc, MaxSteps: int(g.pb.MaxSteps), OutputFormat: asText(format),
			CellBudgetUSD: g.pb.CellBudgetUsd / float64(total),
		})
		if err != nil {
			if isUnavailable(err) {
				return stepsOutcome{unavailable: true}
			}
			if ctx.Err() != nil {
				// A cancel interrupted the step: settle the open result as Python's next
				// step boundary would, on a detached context.
				dctx, dcancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
				defer dcancel()
				if c, cerr := g.cancelled(dctx); cerr == nil && c {
					_ = g.write(dctx, func(q *gen.Queries) error {
						_, err := q.PbEngCancelResult(dctx, gen.PbEngCancelResultParams{WorkspaceID: g.ws, ID: resID})
						return err
					})
					return stepsOutcome{cancelled: true}
				}
				return stepsOutcome{err: ctx.Err()}
			}
			return fail(err.Error())
		}
		last = res
		so := newObj()
		so.set("key", key)
		so.set("name", name)
		so.set("success", res.Success)
		so.set("value", res.Value)
		so.set("metadata", metaValue(res))
		if res.Error == "" {
			so.set("error", nil)
		} else {
			so.set("error", res.Error)
		}
		outs = append(outs, so)
		if !res.Success {
			break
		}
		completed++
		cx[keyText] = res.Value
	}

	meta := newObj()
	meta.set("steps", orEmpty(outs))
	meta.set("completed_steps", completed)
	meta.set("total_steps", total)
	mb := raw(meta)
	if !json.Valid(mb) { // an unencodable value (NaN): keep the ledger readable
		for _, so := range outs {
			o := so.(*obj)
			o.set("value", valueText(o.vals["value"]))
			o.set("metadata", newObj())
		}
		mb = raw(meta)
	}
	status := "failed"
	if last.Success {
		status = "success"
	}
	errText := pgtype.Text{}
	if last.Error != "" {
		errText = pgtype.Text{String: dbText(capRunes(last.Error, maxErrorChars)), Valid: true}
	}
	return stepsOutcome{final: gen.PbEngFinishResultParams{
		WorkspaceID: g.ws, ID: resID, Status: status, Value: dbText(capBytes(valueText(last.Value), maxValueBytes)),
		Metadata: mb, Error: errText,
	}}
}

func orEmpty(l []any) []any {
	if l == nil {
		return []any{}
	}
	return l
}

// metaValue is `output.get("metadata") or {}`.
func metaValue(r ResearchResult) any {
	if r.meta != nil && len(r.meta.keys) > 0 {
		return r.meta
	}
	if len(r.Metadata) > 0 {
		o := newObj()
		for _, k := range sortedKeys(r.Metadata) {
			o.set(k, r.Metadata[k])
		}
		return o
	}
	return newObj()
}

// handBack undoes the current member's start after pyworker.ErrUnavailable
// and returns the error so the queue retries the job. resID 0 means no result
// row was started.
func (g *engine) handBack(ctx context.Context, resID int32) error {
	untouched := g.started == 0
	err := g.in(ctx, func(q *gen.Queries) error {
		if resID != 0 {
			if _, err := q.PbEngRevertResult(ctx, gen.PbEngRevertResultParams{WorkspaceID: g.ws, ID: resID}); err != nil {
				return err
			}
		}
		p := gen.PbEngRestoreRunParams{WorkspaceID: g.ws, ID: g.runID}
		if untouched {
			p.Status, p.StartedAt, p.Error = g.prev.Status, g.prev.StartedAt, g.prev.Error
		} else {
			p.Status, p.KeepStarted = "pending", true
			p.Error = pgtype.Text{String: "Python worker unavailable; run will be retried", Valid: true}
		}
		_, err := q.PbEngRestoreRun(ctx, p)
		return err
	})
	if err != nil {
		return fmt.Errorf("playbooks: hand back: %w", err)
	}
	return pyworker.ErrUnavailable
}

// finish aggregates the ledger and closes the run unless it is no longer
// running (a cancel that raced in is never overwritten).
func (g *engine) finish(ctx context.Context) error {
	return g.in(ctx, func(q *gen.Queries) error {
		if _, err := q.PbEngLockRun(ctx, gen.PbEngLockRunParams{WorkspaceID: g.ws, ID: g.runID}); err != nil {
			return fmt.Errorf("playbooks: run finish lock: %w", err)
		}
		c, err := q.PbResultCounts(ctx, gen.PbResultCountsParams{WorkspaceID: g.ws, RunID: g.runID})
		if err != nil {
			return fmt.Errorf("playbooks: run counts: %w", err)
		}
		if _, err := q.PbEngFinishRun(ctx, gen.PbEngFinishRunParams{WorkspaceID: g.ws, ID: g.runID, Attempted: c.Attempted, Succeeded: c.Succeeded, Failed: c.Failed}); err != nil {
			return fmt.Errorf("playbooks: run finish: %w", err)
		}
		return nil
	})
}
