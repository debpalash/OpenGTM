package playbooks

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/govaudit"
)

// Store errors; the router maps them to the Python HTTP answers.
var (
	ErrNotFound         = errors.New("playbooks: playbook not found")         // 404 "Playbook not found"
	ErrRunNotFound      = errors.New("playbooks: playbook run not found")     // 404 "Playbook run not found"
	ErrNameTaken        = errors.New("playbooks: name taken")                 // 409 "A playbook with this name already exists"
	ErrBadOutputFormat  = errors.New("playbooks: bad output_format")          // 422 "output_format must be text or json"
	ErrAudienceNotFound = errors.New("playbooks: audience not found")         // 404 "Scheduled audience not found" (patch) / "Audience not found" (start run)
	ErrActiveRun        = errors.New("playbooks: playbook has an active run") // 409 "Cannot delete a playbook with an active run"
	ErrPlaybookDisabled = errors.New("playbooks: playbook is disabled")       // 409 "Playbook is disabled"
	ErrNotRetryable     = errors.New("playbooks: run not retryable")          // 409 "Only failed, completed-with-errors, or cancelled runs can be retried"
	ErrNotCancellable   = errors.New("playbooks: run not cancellable")        // 409 "Only pending or running playbook runs can be cancelled"
)

// Hardening caps (decision D3-D6: no unbounded response). Python lists every
// playbook; the cap only differs above MaxPlaybookList rows.
const (
	MaxPlaybookList = 1000
	maxRunLimit     = 200
	maxResultLimit  = 500
	maxOffset       = 100000
)

// ScheduleExecutor picks the executor ("go" or "python") of the
// research_playbook_schedule job Patch enqueues (go_executor.executor_for).
// The default is the Python worker; the server wiring replaces it from the
// GO_EXECUTOR_PLAYBOOKS flag and workspace cohort.
var ScheduleExecutor = func(workspaceID string) string { return "python" }

func ts(t pgtype.Timestamp) time.Time { return t.Time }

func optTime(t pgtype.Timestamp) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

func optText(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	return &t.String
}

func newUUID4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// decodeSteps reads a steps JSON column ([] for null/empty).
func decodeSteps(b []byte, out *[]Step) error {
	*out = []Step{}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	var steps []Step
	if err := json.Unmarshal(b, &steps); err != nil {
		return fmt.Errorf("playbooks: decode steps: %w", err)
	}
	if steps != nil {
		*out = steps
	}
	return nil
}

func encodeSteps(steps []Step) ([]byte, error) {
	if steps == nil {
		steps = []Step{}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(steps); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func playbookFrom(r gen.ResearchPlaybook) (Playbook, error) {
	p := Playbook{
		ID: r.ID, Name: r.Name, Description: r.Description, PromptTemplate: r.PromptTemplate,
		OutputFormat: r.OutputFormat, MaxSteps: int(r.MaxSteps), CellBudgetUSD: r.CellBudgetUsd, Version: int(r.Version),
		Enabled: r.Enabled, ScheduleAudienceID: optText(r.ScheduleAudienceID), NextRunAt: optTime(r.NextRunAt),
		CreatedAt: ts(r.CreatedAt), UpdatedAt: ts(r.UpdatedAt),
	}
	if r.ScheduleIntervalMinutes.Valid {
		v := int(r.ScheduleIntervalMinutes.Int32)
		p.ScheduleIntervalMinutes = &v
	}
	if err := decodeSteps(r.Steps, &p.Steps); err != nil {
		return Playbook{}, err
	}
	return p, nil
}

func runFrom(r gen.PbGetRunRow) Run {
	return Run{
		ID: r.ID, PlaybookID: r.PlaybookID, AudienceID: r.AudienceID, Status: r.Status,
		PromptVersion: int(r.PromptVersion), MaxMembers: int(r.MaxMembers), Attempted: int(r.Attempted),
		Succeeded: int(r.Succeeded), Failed: int(r.Failed), Error: optText(r.Error),
		StartedAt: optTime(r.StartedAt), FinishedAt: optTime(r.FinishedAt), CreatedAt: ts(r.CreatedAt),
	}
}

func isUniqueViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

// Create inserts a playbook (version 1, enabled). A top-level output_format
// other than text/json is ErrBadOutputFormat (checked before the insert, as in
// the router); a duplicate name in the workspace is ErrNameTaken and leaves tx
// usable.
func Create(ctx context.Context, tx pgx.Tx, ws string, in CreateInput) (Playbook, error) {
	if !ValidOutputFormat(in.OutputFormat) {
		return Playbook{}, ErrBadOutputFormat
	}
	steps, err := encodeSteps(in.Steps)
	if err != nil {
		return Playbook{}, err
	}
	row, err := gen.New(tx).PbInsertPlaybook(ctx, gen.PbInsertPlaybookParams{
		ID: newUUID4(), WorkspaceID: ws, Name: in.Name, Description: in.Description, PromptTemplate: in.PromptTemplate,
		Steps: steps, OutputFormat: in.OutputFormat, MaxSteps: int32(in.MaxSteps), CellBudgetUsd: in.CellBudgetUSD,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Playbook{}, ErrNameTaken
	}
	if err != nil {
		return Playbook{}, fmt.Errorf("playbooks: create: %w", err)
	}
	return playbookFrom(row)
}

// Get returns one playbook of the workspace or ErrNotFound.
func Get(ctx context.Context, tx pgx.Tx, ws, id string) (Playbook, error) {
	row, err := gen.New(tx).PbGetPlaybook(ctx, gen.PbGetPlaybookParams{WorkspaceID: ws, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return Playbook{}, ErrNotFound
	}
	if err != nil {
		return Playbook{}, fmt.Errorf("playbooks: get: %w", err)
	}
	return playbookFrom(row)
}

// List returns the workspace's playbooks, most recently updated first (capped at
// MaxPlaybookList).
func List(ctx context.Context, tx pgx.Tx, ws string) ([]Playbook, error) {
	rows, err := gen.New(tx).PbListPlaybooks(ctx, gen.PbListPlaybooksParams{WorkspaceID: ws, RowLimit: MaxPlaybookList})
	if err != nil {
		return nil, fmt.Errorf("playbooks: list: %w", err)
	}
	out := make([]Playbook, 0, len(rows))
	for _, r := range rows {
		p, err := playbookFrom(r)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// Patch applies the present fields. Like Python it always recomputes the
// schedule (mirror row, next_run_at, pending schedule job), bumps version when
// any of prompt_template/steps/output_format/max_steps/cell_budget_usd is
// present, and 404s a schedule audience of another workspace.
// ErrNotFound, ErrBadOutputFormat, ErrAudienceNotFound, ErrNameTaken.
func Patch(ctx context.Context, tx pgx.Tx, ws, id string, in PatchInput) (Playbook, error) {
	q := gen.New(tx)
	cur, err := q.PbLockPlaybook(ctx, gen.PbLockPlaybookParams{WorkspaceID: ws, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return Playbook{}, ErrNotFound
	}
	if err != nil {
		return Playbook{}, fmt.Errorf("playbooks: patch lock: %w", err)
	}
	if in.OutputFormat != nil && !ValidOutputFormat(*in.OutputFormat) {
		return Playbook{}, ErrBadOutputFormat
	}
	if in.ScheduleAudienceSet && in.ScheduleAudienceID != nil {
		// Python skips the check for "" and then 500s on the foreign key; an
		// empty id simply is not an audience here.
		found, err := q.PbAudienceExists(ctx, gen.PbAudienceExistsParams{WorkspaceID: ws, ID: *in.ScheduleAudienceID})
		if err != nil {
			return Playbook{}, fmt.Errorf("playbooks: patch audience: %w", err)
		}
		if !found {
			return Playbook{}, ErrAudienceNotFound
		}
	}
	if in.Name != nil && *in.Name != cur.Name {
		taken, err := q.PbNameTaken(ctx, gen.PbNameTakenParams{WorkspaceID: ws, Name: *in.Name, ID: id})
		if err != nil {
			return Playbook{}, fmt.Errorf("playbooks: patch name: %w", err)
		}
		if taken {
			return Playbook{}, ErrNameTaken
		}
	}

	u := gen.PbUpdatePlaybookParams{
		WorkspaceID: ws, ID: id, Name: cur.Name, Description: cur.Description, PromptTemplate: cur.PromptTemplate,
		Steps: cur.Steps, OutputFormat: cur.OutputFormat, MaxSteps: cur.MaxSteps, CellBudgetUsd: cur.CellBudgetUsd,
		Version: cur.Version, Enabled: cur.Enabled, ScheduleAudienceID: cur.ScheduleAudienceID,
		ScheduleIntervalMinutes: cur.ScheduleIntervalMinutes,
	}
	if in.Name != nil {
		u.Name = *in.Name
	}
	if in.Description != nil {
		u.Description = *in.Description
	}
	if in.PromptTemplate != nil {
		u.PromptTemplate = *in.PromptTemplate
	}
	if in.OutputFormat != nil {
		u.OutputFormat = *in.OutputFormat
	}
	if in.MaxSteps != nil {
		u.MaxSteps = int32(*in.MaxSteps)
	}
	if in.CellBudgetUSD != nil {
		u.CellBudgetUsd = *in.CellBudgetUSD
	}
	if in.Enabled != nil {
		u.Enabled = *in.Enabled
	}
	if in.Steps != nil {
		if u.Steps, err = encodeSteps(*in.Steps); err != nil {
			return Playbook{}, err
		}
	}
	if in.ScheduleAudienceSet {
		u.ScheduleAudienceID = pgtype.Text{}
		if in.ScheduleAudienceID != nil {
			u.ScheduleAudienceID = pgtype.Text{String: *in.ScheduleAudienceID, Valid: true}
		}
	}
	if in.ScheduleIntervalSet {
		u.ScheduleIntervalMinutes = pgtype.Int4{}
		if in.ScheduleIntervalMinutes != nil {
			u.ScheduleIntervalMinutes = pgtype.Int4{Int32: int32(*in.ScheduleIntervalMinutes), Valid: true}
		}
	}
	if in.PromptTemplate != nil || in.Steps != nil || in.OutputFormat != nil || in.MaxSteps != nil || in.CellBudgetUSD != nil {
		u.Version++
	}

	// schedule_next: cancel pending occurrences, recompute activity and next run.
	if err := q.PbCancelScheduleJobs(ctx, gen.PbCancelScheduleJobsParams{WorkspaceID: ws, FirePrefix: schedulePrefix(id)}); err != nil {
		return Playbook{}, fmt.Errorf("playbooks: cancel schedule jobs: %w", err)
	}
	active := u.Enabled && u.ScheduleAudienceID.Valid && u.ScheduleAudienceID.String != "" &&
		u.ScheduleIntervalMinutes.Valid && u.ScheduleIntervalMinutes.Int32 != 0
	var nextAt time.Time
	if active {
		minutes := int(u.ScheduleIntervalMinutes.Int32)
		nextAt = time.Now().UTC().Truncate(time.Microsecond).Add(time.Duration(max(15, min(minutes, 10080))) * time.Minute)
		u.NextRunAt = pgtype.Timestamp{Time: nextAt, Valid: true}
	}
	if err := q.PbUpsertScheduleMirror(ctx, gen.PbUpsertScheduleMirrorParams{
		PlaybookID: id, WorkspaceID: ws, Enabled: active, NextRunAt: u.NextRunAt,
	}); err != nil {
		return Playbook{}, fmt.Errorf("playbooks: schedule mirror: %w", err)
	}
	row, err := q.PbUpdatePlaybook(ctx, u)
	if isUniqueViolation(err) {
		return Playbook{}, ErrNameTaken
	}
	if err != nil {
		return Playbook{}, fmt.Errorf("playbooks: patch update: %w", err)
	}
	if active {
		if err := enqueueSchedule(ctx, tx, ws, id, nextAt); err != nil {
			return Playbook{}, err
		}
	}
	return playbookFrom(row)
}

func schedulePrefix(playbookID string) string { return "playbook_schedule:" + playbookID + ":" }

// pyISOAware is datetime.isoformat() of an aware UTC datetime.
func pyISOAware(t time.Time) string { return isoNaive(t) + "+00:00" }

// enqueueSchedule is enqueue_job_once for the next scheduled occurrence.
func enqueueSchedule(ctx context.Context, tx pgx.Tx, ws, playbookID string, at time.Time) error {
	executor := ScheduleExecutor(ws)
	if executor != "go" && executor != "python" {
		return fmt.Errorf("playbooks: schedule executor %q must be go or python", executor)
	}
	key := schedulePrefix(playbookID) + pyISOAware(at)
	payload, err := jsonNoEscape(struct {
		WorkspaceID string `json:"workspace_id"`
		PlaybookID  string `json:"playbook_id"`
		FireKey     string `json:"fire_key"`
	}{ws, playbookID, key})
	if err != nil {
		return err
	}
	q := gen.New(tx)
	if err := q.LockJobFireKey(ctx, key); err != nil {
		return fmt.Errorf("playbooks: lock schedule job: %w", err)
	}
	if _, err := q.ActiveJobByFireKey(ctx, key); err == nil {
		return nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("playbooks: look up schedule job: %w", err)
	}
	if _, err := q.PbInsertScheduleJob(ctx, gen.PbInsertScheduleJobParams{
		Executor: executor, Payload: payload, WorkspaceID: ws, FireKey: key, NextRunAt: pgtype.Timestamp{Time: at, Valid: true},
	}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("playbooks: enqueue schedule job: %w", err)
	}
	return nil
}

func jsonNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// Delete removes a playbook (runs and results cascade) and its schedule mirror
// and pending schedule jobs. ErrNotFound, ErrActiveRun.
func Delete(ctx context.Context, tx pgx.Tx, ws, id string) error {
	q := gen.New(tx)
	if _, err := q.PbLockPlaybook(ctx, gen.PbLockPlaybookParams{WorkspaceID: ws, ID: id}); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return fmt.Errorf("playbooks: delete lock: %w", err)
	}
	active, err := q.PbActiveRunExists(ctx, gen.PbActiveRunExistsParams{WorkspaceID: ws, PlaybookID: id})
	if err != nil {
		return fmt.Errorf("playbooks: delete active check: %w", err)
	}
	if active {
		return ErrActiveRun
	}
	if err := q.PbDeleteScheduleMirror(ctx, gen.PbDeleteScheduleMirrorParams{PlaybookID: id, WorkspaceID: ws}); err != nil {
		return fmt.Errorf("playbooks: delete mirror: %w", err)
	}
	if err := q.PbCancelScheduleJobs(ctx, gen.PbCancelScheduleJobsParams{WorkspaceID: ws, FirePrefix: schedulePrefix(id)}); err != nil {
		return fmt.Errorf("playbooks: delete schedule jobs: %w", err)
	}
	if _, err := q.PbDeletePlaybook(ctx, gen.PbDeletePlaybookParams{WorkspaceID: ws, ID: id}); err != nil {
		return fmt.Errorf("playbooks: delete: %w", err)
	}
	return nil
}

// StartRun snapshots pb's prompt, steps and version into a new pending run.
// pb must have been read in this workspace; the row is re-read so a foreign or
// deleted playbook cannot start a run. ErrNotFound, ErrPlaybookDisabled,
// ErrAudienceNotFound. The caller enqueues the job with EnqueueRun.
func StartRun(ctx context.Context, tx pgx.Tx, ws string, pb Playbook, audienceID string, maxMembers int, by string) (Run, error) {
	q := gen.New(tx)
	cur, err := q.PbGetPlaybook(ctx, gen.PbGetPlaybookParams{WorkspaceID: ws, ID: pb.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	if err != nil {
		return Run{}, fmt.Errorf("playbooks: start run get: %w", err)
	}
	if !pb.Enabled || !cur.Enabled {
		return Run{}, ErrPlaybookDisabled
	}
	found, err := q.PbAudienceExists(ctx, gen.PbAudienceExistsParams{WorkspaceID: ws, ID: audienceID})
	if err != nil {
		return Run{}, fmt.Errorf("playbooks: start run audience: %w", err)
	}
	if !found {
		return Run{}, ErrAudienceNotFound
	}
	steps := cur.Steps
	if len(bytes.TrimSpace(steps)) == 0 || bytes.Equal(bytes.TrimSpace(steps), []byte("null")) {
		steps = []byte("[]")
	}
	row, err := q.PbInsertRun(ctx, gen.PbInsertRunParams{
		ID: newUUID4(), WorkspaceID: ws, PlaybookID: cur.ID, AudienceID: audienceID, PromptVersion: cur.Version,
		PromptSnapshot: cur.PromptTemplate, StepsSnapshot: steps, MaxMembers: int32(maxMembers), RequestedBy: by,
	})
	if err != nil {
		return Run{}, fmt.Errorf("playbooks: start run insert: %w", err)
	}
	return runFrom(gen.PbGetRunRow(row)), nil
}

// EnqueueRun adds the research_playbook_run job for a run (queue_service.add_job,
// fire_key "playbook:<run id>"). executor is "go" or "python". Call it in the
// same transaction as StartRun / RetryRun.
func EnqueueRun(ctx context.Context, tx pgx.Tx, ws, runID, executor string) error {
	if executor != "go" && executor != "python" {
		return fmt.Errorf("playbooks: run executor %q must be go or python", executor)
	}
	payload, err := jsonNoEscape(struct {
		WorkspaceID string `json:"workspace_id"`
		RunID       string `json:"run_id"`
	}{ws, runID})
	if err != nil {
		return err
	}
	if _, err := gen.New(tx).PbInsertRunJob(ctx, gen.PbInsertRunJobParams{
		Executor: executor, Payload: payload, WorkspaceID: ws, FireKey: "playbook:" + runID,
	}); err != nil {
		return fmt.Errorf("playbooks: enqueue run: %w", err)
	}
	return nil
}

func clamp(v, lo, hi int) int { return max(lo, min(v, hi)) }

// ListRuns pages a playbook's runs, newest first, by keyset cursor (offset is
// ignored when a cursor is given). A malformed cursor is *govaudit.CursorError
// (422); an unknown playbook is ErrNotFound. limit and offset are clamped to
// 1..200 and 0..100000.
func ListRuns(ctx context.Context, tx pgx.Tx, ws, playbookID string, limit, offset int, cursor string) (RunPage, error) {
	q := gen.New(tx)
	if _, err := q.PbGetPlaybook(ctx, gen.PbGetPlaybookParams{WorkspaceID: ws, ID: playbookID}); errors.Is(err, pgx.ErrNoRows) {
		return RunPage{}, ErrNotFound
	} else if err != nil {
		return RunPage{}, fmt.Errorf("playbooks: list runs get: %w", err)
	}
	limit, offset = clamp(limit, 1, maxRunLimit), clamp(offset, 0, maxOffset)
	p := gen.PbListRunsParams{WorkspaceID: ws, PlaybookID: playbookID, RowLimit: int32(limit + 1), RowOffset: int32(offset)}
	if cursor != "" {
		at, id, cerr := govaudit.DecodeCursor(cursor, "playbook run")
		if cerr != nil {
			return RunPage{}, cerr
		}
		p.UseCursor, p.CursorID, p.RowOffset = true, id, 0
		p.CursorCreated = pgtype.Timestamp{Time: at.UTC(), Valid: true}
	}
	rows, err := q.PbListRuns(ctx, p)
	if err != nil {
		return RunPage{}, fmt.Errorf("playbooks: list runs: %w", err)
	}
	pg := RunPage{Runs: make([]Run, 0, min(len(rows), limit)), Limit: limit, HasMore: len(rows) > limit}
	if !p.UseCursor {
		pg.Offset = &offset
	}
	for i, r := range rows {
		if i == limit {
			break
		}
		pg.Runs = append(pg.Runs, runFrom(gen.PbGetRunRow(r)))
	}
	if pg.HasMore {
		last := pg.Runs[len(pg.Runs)-1]
		c := govaudit.EncodeCursor(last.CreatedAt, last.ID)
		pg.NextCursor = &c
	}
	return pg, nil
}

// parseResultCursor is `int(cursor)` with a non-negative check: Python int()
// accepts surrounding whitespace, a sign and single underscores between digits.
func parseResultCursor(s string) (int64, bool) {
	t := strings.TrimSpace(s)
	if !intStrRE.MatchString(t) {
		return 0, false
	}
	n, ok := new(big.Int).SetString(strings.ReplaceAll(t, "_", ""), 10)
	if !ok || n.Sign() < 0 {
		return 0, false
	}
	if !n.IsInt64() {
		return math.MaxInt64, true
	}
	return n.Int64(), true
}

// ListResults pages a run's results by ascending id with an integer cursor
// (offset is ignored when a cursor is given). A malformed cursor is a 422
// *govaudit.CursorError "invalid playbook result cursor"; an unknown run is
// ErrRunNotFound. limit and offset are clamped to 1..500 and 0..100000.
func ListResults(ctx context.Context, tx pgx.Tx, ws, runID string, limit, offset int, cursor string) (ResultPage, error) {
	q := gen.New(tx)
	if _, err := q.PbGetRun(ctx, gen.PbGetRunParams{WorkspaceID: ws, ID: runID}); errors.Is(err, pgx.ErrNoRows) {
		return ResultPage{}, ErrRunNotFound
	} else if err != nil {
		return ResultPage{}, fmt.Errorf("playbooks: list results run: %w", err)
	}
	limit, offset = clamp(limit, 1, maxResultLimit), clamp(offset, 0, maxOffset)
	p := gen.PbListResultsParams{WorkspaceID: ws, RunID: runID, AfterID: -1, RowLimit: int32(limit + 1), RowOffset: int32(offset)}
	if cursor != "" {
		after, ok := parseResultCursor(cursor)
		if !ok {
			return ResultPage{}, &govaudit.CursorError{Detail: "invalid playbook result cursor"}
		}
		p.AfterID, p.RowOffset = after, 0
	}
	rows, err := q.PbListResults(ctx, p)
	if err != nil {
		return ResultPage{}, fmt.Errorf("playbooks: list results: %w", err)
	}
	pg := ResultPage{Results: make([]Result, 0, min(len(rows), limit)), Limit: limit, HasMore: len(rows) > limit}
	if cursor == "" {
		pg.Offset = &offset
	}
	for i, r := range rows {
		if i == limit {
			break
		}
		pg.Results = append(pg.Results, Result{
			ID: int64(r.ID), RunID: r.RunID, LeadID: int(r.LeadID), Status: r.Status, Value: r.Value,
			Metadata: json.RawMessage(r.Metadata), Error: optText(r.Error), Attempts: int(r.Attempts),
			CreatedAt: ts(r.CreatedAt), UpdatedAt: ts(r.UpdatedAt),
		})
	}
	if pg.HasMore {
		c := strconv.FormatInt(pg.Results[len(pg.Results)-1].ID, 10)
		pg.NextCursor = &c
	}
	return pg, nil
}

// RetryRun moves a failed, completed-with-errors or cancelled run back to
// pending (error and finished_at cleared). ErrRunNotFound, ErrNotRetryable. The
// caller enqueues the job with EnqueueRun.
func RetryRun(ctx context.Context, tx pgx.Tx, ws, runID string) (Run, error) {
	q := gen.New(tx)
	row, err := q.PbRetryRun(ctx, gen.PbRetryRunParams{WorkspaceID: ws, ID: runID})
	if err == nil {
		return runFrom(gen.PbGetRunRow(row)), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Run{}, fmt.Errorf("playbooks: retry: %w", err)
	}
	if _, gerr := q.PbGetRun(ctx, gen.PbGetRunParams{WorkspaceID: ws, ID: runID}); errors.Is(gerr, pgx.ErrNoRows) {
		return Run{}, ErrRunNotFound
	} else if gerr != nil {
		return Run{}, fmt.Errorf("playbooks: retry get: %w", gerr)
	}
	return Run{}, ErrNotRetryable
}

// CancelRun cancels a pending or running run ("Cancelled by user") and its
// pending/processing job. ErrRunNotFound, ErrNotCancellable.
func CancelRun(ctx context.Context, tx pgx.Tx, ws, runID string) (Run, error) {
	q := gen.New(tx)
	row, err := q.PbCancelRun(ctx, gen.PbCancelRunParams{WorkspaceID: ws, ID: runID})
	if errors.Is(err, pgx.ErrNoRows) {
		if _, gerr := q.PbGetRun(ctx, gen.PbGetRunParams{WorkspaceID: ws, ID: runID}); errors.Is(gerr, pgx.ErrNoRows) {
			return Run{}, ErrRunNotFound
		} else if gerr != nil {
			return Run{}, fmt.Errorf("playbooks: cancel get: %w", gerr)
		}
		return Run{}, ErrNotCancellable
	}
	if err != nil {
		return Run{}, fmt.Errorf("playbooks: cancel: %w", err)
	}
	if err := q.PbCancelRunJobs(ctx, gen.PbCancelRunJobsParams{WorkspaceID: ws, FireKey: "playbook:" + runID}); err != nil {
		return Run{}, fmt.Errorf("playbooks: cancel jobs: %w", err)
	}
	return runFrom(gen.PbGetRunRow(row)), nil
}

// CountResults aggregates a run's ledger: rows, rows with status success and
// rows with status failed.
func CountResults(ctx context.Context, tx pgx.Tx, ws, runID string) (attempted, succeeded, failed int, err error) {
	r, err := gen.New(tx).PbResultCounts(ctx, gen.PbResultCountsParams{WorkspaceID: ws, RunID: runID})
	if err != nil {
		return 0, 0, 0, fmt.Errorf("playbooks: result counts: %w", err)
	}
	return int(r.Attempted), int(r.Succeeded), int(r.Failed), nil
}

// ResultCounts is CountResults without the error (zeros on failure); prefer
// CountResults where a database error must not read as an empty ledger.
func ResultCounts(ctx context.Context, tx pgx.Tx, ws, runID string) (attempted, succeeded, failed int) {
	a, s, f, err := CountResults(ctx, tx, ws, runID)
	if err != nil {
		return 0, 0, 0
	}
	return a, s, f
}
