package autosvc

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/autoschedule"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/wbcrud"
)

// Service is the rule store behind /api/automations and the MCP adapter.
type Service struct {
	Pool *pgxpool.Pool
	Cfg  Config
	// ExecFor is go_executor.trigger_eval_executor (nil: go).
	ExecFor func(ws string) string
	Now     func() time.Time
	Log     *slog.Logger
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// executor is "go": the only executor since the plan 15 cutover (ExecFor is no longer consulted).
func (s *Service) executor(string) string { return "go" }

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

var (
	errTriggerNotFound = &HTTPError{Status: 404, Detail: "trigger not found"}
	errRunNotFound     = &HTTPError{Status: 404, Detail: "run not found"}
)

func tstz(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t.UTC(), Valid: true} }

func mustJSON(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return []byte("null")
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// objJSON renders an ordered value for a JSON column.
func objJSON(v any) []byte {
	if o, ok := v.(*wbcrud.Obj); ok {
		b, err := o.MarshalJSON()
		if err != nil {
			return []byte("{}")
		}
		return b
	}
	w := wbcrud.NewObj()
	w.Set("v", v)
	b, err := w.MarshalJSON()
	if err != nil {
		return []byte("null")
	}
	return b[len(`{"v":`) : len(b)-1]
}

// CountRules counts the workspace's rules.
func (s *Service) CountRules(ctx context.Context, tx pgx.Tx, ws string) (int, error) {
	n, err := gen.New(tx).AutoSvcCountTriggers(ctx, ws)
	return int(n), err
}

// Validate runs ValidateRule with the service config.
func (s *Service) Validate(ctx context.Context, tx pgx.Tx, ws string, r RuleInput) error {
	return ValidateRule(ctx, tx, ws, r, s.Cfg)
}

// InsertTrigger stores a validated rule. An on_schedule rule gets
// schedule_anchor = now; created_by is stamped from r.CreatedBy.
func (s *Service) InsertTrigger(ctx context.Context, tx pgx.Tx, ws string, r RuleInput) (Trigger, error) {
	tt := ""
	if r.TriggerType != nil {
		tt = *r.TriggerType
	}
	cfg := r.TriggerConfig
	p := gen.AutoSvcInsertTriggerParams{
		ID: NewID(), WorkspaceID: ws, Name: r.Name, Enabled: r.Enabled, TriggerType: tt,
		TriggerConfig: objJSON(orDefault(cfg, emptyObj())), Condition: r.Condition,
		Actions:          objJSON(orDefaultActions(r.Actions)),
		ScopeWorkbookIds: mustJSON(nonNilStrings(r.ScopeWorkbookIDs)),
		StopOnError:      r.StopOnError,
	}
	if r.MaxSpendUSDPerDay != nil {
		p.MaxSpendUsdPerDay = pgtype.Float8{Float64: *r.MaxSpendUSDPerDay, Valid: true}
	}
	if r.MaxActionsPerDay != nil {
		p.MaxActionsPerDay = pgtype.Int4{Int32: *r.MaxActionsPerDay, Valid: true}
	}
	if tt == "on_schedule" {
		p.ScheduleAnchor = tstz(s.now())
	}
	if r.CreatedBy != "" {
		p.CreatedBy = pgtype.Text{String: r.CreatedBy, Valid: true}
	}
	row, err := gen.New(tx).AutoSvcInsertTrigger(ctx, p)
	if err != nil {
		return Trigger{}, fmt.Errorf("autosvc: insert trigger: %w", err)
	}
	return toTrigger(row)
}

func orDefaultActions(a []any) any {
	if len(a) == 0 {
		return []any{}
	}
	return a
}

func nonNilStrings(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// BootstrapSchedule is schedule_bootstrap_for_trigger in its own transaction:
// compute the aligned next run and enqueue it if absent. It is the post-commit
// step of create (callers log and swallow its error). A missing, disabled or
// non-schedule rule is a no-op.
func (s *Service) BootstrapSchedule(ctx context.Context, ws, id string) error {
	return tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		t, ok, err := autoschedule.LoadTrigger(ctx, tx, id, ws)
		if err != nil || !ok || !t.Enabled || t.Type != "on_schedule" {
			return err
		}
		_, err = autoschedule.BootstrapForTrigger(ctx, tx, t, s.now(), s.executor(ws))
		return err
	})
}

func (s *Service) postCommitBootstrap(ctx context.Context, ws string, t Trigger) {
	if t.TriggerType != "on_schedule" || !t.Enabled {
		return
	}
	if err := s.BootstrapSchedule(ctx, ws, t.ID); err != nil {
		s.log().Warn("schedule bootstrap on create failed", "workspace_id", ws, "trigger_id", t.ID, "error", err.Error())
	}
}

func (s *Service) createTx(ctx context.Context, ws string, in RuleInput) (Trigger, error) {
	var out Trigger
	err := tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		max := s.Cfg.MaxRulesPerWS
		if max <= 0 {
			max = 50
		}
		// F21: serialise cap check + insert per workspace (held to commit).
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('auto_rules:' || $1::text, 0))`, ws); err != nil {
			return err
		}
		n, err := s.CountRules(ctx, tx, ws)
		if err != nil {
			return err
		}
		if n >= max {
			return herr(422, "max rules per workspace reached (%d)", max)
		}
		if len([]rune(in.Name)) > 200 {
			return herr(422, "name too long (max 200 characters)")
		}
		if err := s.Validate(ctx, tx, ws, in); err != nil {
			return err
		}
		out, err = s.InsertTrigger(ctx, tx, ws, in)
		return err
	})
	return out, err
}

// Create is POST /triggers: enforce the rule cap, validate, insert, then (after
// the commit) bootstrap the schedule of an enabled on_schedule rule, log only.
// The result is read back after the bootstrap so next_run_at is current.
func (s *Service) Create(ctx context.Context, ws string, in RuleInput) (Trigger, error) {
	t, err := s.createTx(ctx, ws, in)
	if err != nil {
		return Trigger{}, err
	}
	s.postCommitBootstrap(ctx, ws, t)
	if t.TriggerType == "on_schedule" && t.Enabled {
		if fresh, err := s.Get(ctx, ws, t.ID); err == nil {
			return fresh.Trigger, nil
		}
	}
	return t, nil
}

// CreateAutomation is the MCP create_automation shape: the stored rule as
// inserted (next_run_at not yet set, as Python's result predates the
// bootstrap) and its id. The schedule bootstrap runs after the commit.
func (s *Service) CreateAutomation(ctx context.Context, ws string, in RuleInput) (Created, error) {
	t, err := s.createTx(ctx, ws, in)
	if err != nil {
		return Created{}, err
	}
	s.postCommitBootstrap(ctx, ws, t)
	return Created{Automation: t}, nil
}

// ListFilter is the GET /triggers query.
type ListFilter struct {
	Enabled     *bool
	TriggerType string
}

// List returns the workspace's rules, newest first.
func (s *Service) List(ctx context.Context, ws string, f ListFilter) ([]Trigger, error) {
	var out []Trigger
	err := tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		p := gen.AutoSvcListTriggersParams{WorkspaceID: ws}
		if f.Enabled != nil {
			p.Enabled = pgtype.Bool{Bool: *f.Enabled, Valid: true}
		}
		if f.TriggerType != "" {
			p.TriggerType = pgtype.Text{String: f.TriggerType, Valid: true}
		}
		rows, err := gen.New(tx).AutoSvcListTriggers(ctx, p)
		if err != nil {
			return err
		}
		out = make([]Trigger, 0, len(rows))
		for _, r := range rows {
			t, err := toTrigger(r)
			if err != nil {
				return err
			}
			out = append(out, t)
		}
		return nil
	})
	return out, err
}

// Get is GET /triggers/{id}: the rule plus its 10 most recent runs. Unknown or
// other-workspace ids answer 404 "trigger not found".
func (s *Service) Get(ctx context.Context, ws, id string) (TriggerDetail, error) {
	var out TriggerDetail
	err := tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		q := gen.New(tx)
		row, err := q.AutoSvcGetTrigger(ctx, gen.AutoSvcGetTriggerParams{ID: id, WorkspaceID: ws})
		if errors.Is(err, pgx.ErrNoRows) {
			return errTriggerNotFound
		}
		if err != nil {
			return err
		}
		if out.Trigger, err = toTrigger(row); err != nil {
			return err
		}
		runs, err := q.AutoSvcRecentRuns(ctx, gen.AutoSvcRecentRunsParams{WorkspaceID: ws, TriggerID: id, RowLimit: 10})
		if err != nil {
			return err
		}
		out.RecentRuns = make([]Run, len(runs))
		for i, r := range runs {
			out.RecentRuns[i] = toRun(r)
		}
		return nil
	})
	return out, err
}

// PatchInput is PATCH /triggers/{id}; nil fields are unchanged.
type PatchInput struct {
	Name              *string
	TriggerConfig     *wbcrud.Obj
	Condition         *string
	Actions           *[]any
	ScopeWorkbookIDs  *[]string
	StopOnError       *bool
	MaxSpendUSDPerDay *float64
	MaxActionsPerDay  *int32
	Enabled           *bool
}

func lockTrigger(ctx context.Context, tx pgx.Tx, ws, id string) (gen.Trigger, error) {
	row, err := gen.New(tx).AutoSvcLockTrigger(ctx, gen.AutoSvcLockTriggerParams{ID: id, WorkspaceID: ws})
	if errors.Is(err, pgx.ErrNoRows) {
		return row, errTriggerNotFound
	}
	return row, err
}

func storedActions(t Trigger) []any {
	a, _ := t.Actions.([]any)
	return a
}

func storedScope(t Trigger) []string {
	var out []string
	if a, ok := t.ScopeWorkbookIDs.([]any); ok {
		for _, e := range a {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// Patch re-validates the MERGED rule (trigger_type is fixed), applies the
// changes and keeps the schedule mirror in step: an enabled on_schedule rule is
// re-bootstrapped, a disabled one gets mirror enabled=false, all in the same
// transaction.
func (s *Service) Patch(ctx context.Context, ws, id string, in PatchInput) (Trigger, error) {
	var out Trigger
	err := tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		row, err := lockTrigger(ctx, tx, ws, id)
		if err != nil {
			return err
		}
		cur, err := toTrigger(row)
		if err != nil {
			return err
		}
		merged := RuleInput{TriggerType: &cur.TriggerType, TriggerConfig: cur.TriggerConfig, Condition: cur.Condition,
			Actions: storedActions(cur), ScopeWorkbookIDs: storedScope(cur)}
		if in.TriggerConfig != nil {
			merged.TriggerConfig = in.TriggerConfig
		}
		if in.Condition != nil {
			merged.Condition = *in.Condition
		}
		if in.Actions != nil {
			merged.Actions = *in.Actions
		}
		if in.ScopeWorkbookIDs != nil {
			merged.ScopeWorkbookIDs = *in.ScopeWorkbookIDs
		}
		merged.MaxSpendUSDPerDay, merged.MaxActionsPerDay = in.MaxSpendUSDPerDay, in.MaxActionsPerDay
		if err := s.Validate(ctx, tx, ws, merged); err != nil {
			return err
		}
		if in.Name != nil && utf8.RuneCountInString(*in.Name) > 200 {
			return herr(422, "name too long (max 200 characters)")
		}
		p := gen.AutoSvcUpdateTriggerParams{ID: id, WorkspaceID: ws, Name: row.Name, Enabled: row.Enabled,
			TriggerConfig: row.TriggerConfig, Condition: row.Condition.String, Actions: row.Actions,
			ScopeWorkbookIds: row.ScopeWorkbookIds, StopOnError: row.StopOnError,
			MaxSpendUsdPerDay: row.MaxSpendUsdPerDay, MaxActionsPerDay: row.MaxActionsPerDay}
		if in.Name != nil {
			p.Name = *in.Name
		}
		if in.StopOnError != nil {
			p.StopOnError = *in.StopOnError
		}
		if in.MaxSpendUSDPerDay != nil {
			p.MaxSpendUsdPerDay = pgtype.Float8{Float64: *in.MaxSpendUSDPerDay, Valid: true}
		}
		if in.MaxActionsPerDay != nil {
			p.MaxActionsPerDay = pgtype.Int4{Int32: *in.MaxActionsPerDay, Valid: true}
		}
		if in.Enabled != nil {
			p.Enabled = *in.Enabled
		}
		if in.TriggerConfig != nil {
			p.TriggerConfig = objJSON(in.TriggerConfig)
		}
		if in.Condition != nil {
			p.Condition = *in.Condition
		}
		if in.Actions != nil {
			p.Actions = objJSON(orDefaultActions(*in.Actions))
		}
		if in.ScopeWorkbookIDs != nil {
			p.ScopeWorkbookIds = mustJSON(nonNilStrings(*in.ScopeWorkbookIDs))
		}
		upd, err := gen.New(tx).AutoSvcUpdateTrigger(ctx, p)
		if err != nil {
			return fmt.Errorf("autosvc: update trigger: %w", err)
		}
		if out, err = toTrigger(upd); err != nil {
			return err
		}
		if out.TriggerType != "on_schedule" {
			return nil
		}
		if out.Enabled {
			t, ok, err := autoschedule.LoadTrigger(ctx, tx, id, ws)
			if err != nil || !ok {
				return err
			}
			if err := s.rebootstrap(ctx, tx, t, ws); err != nil {
				return err
			}
		} else if err := autoschedule.MirrorSet(ctx, tx, id, ws, out.NextRunAt, false); err != nil {
			return err
		}
		fresh, err := gen.New(tx).AutoSvcGetTrigger(ctx, gen.AutoSvcGetTriggerParams{ID: id, WorkspaceID: ws})
		if err != nil {
			return err
		}
		out, err = toTrigger(fresh)
		return err
	})
	return out, err
}

func (s *Service) rebootstrap(ctx context.Context, tx pgx.Tx, t autoschedule.Trigger, ws string) error {
	_, err := s.rebootstrapNext(ctx, tx, t, ws)
	return err
}

// rebootstrapNext is schedule_bootstrap_for_trigger for an enable/resume. When
// a job with the aligned key is still pending, the enqueue step writes nothing
// (single flight), which in Python leaves a mirror row disabled by an earlier
// pause stale; the mirror is re-enabled explicitly here (documented deviation).
func (s *Service) rebootstrapNext(ctx context.Context, tx pgx.Tx, t autoschedule.Trigger, ws string) (time.Time, error) {
	next, err := autoschedule.BootstrapForTrigger(ctx, tx, t, s.now(), s.executor(ws))
	if err != nil {
		return next, err
	}
	return next, autoschedule.MirrorSet(ctx, tx, t.ID, ws, &next, true)
}

// Delete removes the schedule mirror first, then the rule.
func (s *Service) Delete(ctx context.Context, ws, id string) error {
	return tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		if _, err := lockTrigger(ctx, tx, ws, id); err != nil {
			return err
		}
		if err := autoschedule.MirrorDelete(ctx, tx, id); err != nil {
			return err
		}
		_, err := gen.New(tx).AutoSvcDeleteTrigger(ctx, gen.AutoSvcDeleteTriggerParams{ID: id, WorkspaceID: ws})
		return err
	})
}

// Pause disables the rule and marks its mirror row disabled.
func (s *Service) Pause(ctx context.Context, ws, id string) error {
	return tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		row, err := lockTrigger(ctx, tx, ws, id)
		if err != nil {
			return err
		}
		if _, err := gen.New(tx).AutoSvcSetEnabled(ctx, gen.AutoSvcSetEnabledParams{ID: id, WorkspaceID: ws, Enabled: false}); err != nil {
			return err
		}
		var next *time.Time
		if row.NextRunAt.Valid {
			t := row.NextRunAt.Time.UTC()
			next = &t
		}
		return autoschedule.MirrorSet(ctx, tx, id, ws, next, false)
	})
}

// Resume enables the rule. For an on_schedule rule it bootstraps the schedule
// and returns the computed next run; other types return nil.
func (s *Service) Resume(ctx context.Context, ws, id string) (*time.Time, error) {
	var next *time.Time
	err := tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		row, err := lockTrigger(ctx, tx, ws, id)
		if err != nil {
			return err
		}
		if _, err := gen.New(tx).AutoSvcSetEnabled(ctx, gen.AutoSvcSetEnabledParams{ID: id, WorkspaceID: ws, Enabled: true}); err != nil {
			return err
		}
		if row.TriggerType != "on_schedule" {
			return nil
		}
		t, ok, err := autoschedule.LoadTrigger(ctx, tx, id, ws)
		if err != nil || !ok {
			return err
		}
		n, err := s.rebootstrapNext(ctx, tx, t, ws)
		if err != nil {
			return err
		}
		next = &n
		return nil
	})
	return next, err
}

// NewID is a random RFC 4122 version 4 UUID string (Python str(uuid.uuid4())).
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("autosvc: crypto/rand: " + err.Error())
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
