package autosvc

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// Target is one resolved row target of a manual run or preview.
type Target struct {
	WorkbookID string `json:"workbook_id"`
	RowID      string `json:"row_id"`
}

// pyInt is Python int(str): surrounding whitespace, a sign and digits with
// single underscores between digits. ok is false where Python raises.
func pyInt(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	neg := false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		neg = s[0] == '-'
		s = s[1:]
	}
	if s == "" || s[0] == '_' || s[len(s)-1] == '_' || strings.Contains(s, "__") {
		return 0, false
	}
	s = strings.ReplaceAll(s, "_", "")
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	if neg {
		n = -n
	}
	return n, true
}

// PreviewTargets resolves row ids to {workbook_id,row_id} in the workspace, in
// request order. Ids that are not integers, or that match no workspace row, are
// no match (D4: Python 500s on a non-numeric id). tx must be inside
// tenancy.InWorkspace.
func (s *Service) PreviewTargets(ctx context.Context, tx pgx.Tx, ws string, rowIDs []string) ([]Target, error) {
	var want []int32
	parsed := make([]int64, len(rowIDs))
	valid := make([]bool, len(rowIDs))
	for i, r := range rowIDs {
		n, ok := pyInt(r)
		if ok && n >= 0 && n <= 1<<31-1 {
			parsed[i], valid[i] = n, true
			want = append(want, int32(n))
		}
	}
	targets := []Target{}
	if len(want) == 0 {
		return targets, nil
	}
	rows, err := gen.New(tx).AutoSvcRowWorkbooks(ctx, gen.AutoSvcRowWorkbooksParams{WorkspaceID: ws, Ids: want})
	if err != nil {
		return nil, fmt.Errorf("autosvc: resolve rows: %w", err)
	}
	wb := make(map[int64]string, len(rows))
	for _, r := range rows {
		wb[int64(r.RowID)] = r.WorkbookID
	}
	for i := range rowIDs {
		if !valid[i] {
			continue
		}
		if w, ok := wb[parsed[i]]; ok {
			targets = append(targets, Target{WorkbookID: w, RowID: strconv.FormatInt(parsed[i], 10)})
		}
	}
	return targets, nil
}

type manualPayload struct {
	TriggerID   string    `json:"trigger_id"`
	WorkspaceID string    `json:"workspace_id"`
	FireSource  string    `json:"fire_source"`
	FireKey     string    `json:"fire_key"`
	DryRun      bool      `json:"dry_run"`
	Targets     *[]Target `json:"targets,omitempty"`
}

// EnqueueManualRun is POST /triggers/{id}/run: 404 for an unknown trigger, the
// targets resolved from rowIDs (only when rowIDs is non-empty; no match still
// sends an empty list, as Python), then a trigger_eval job with fire key
// manual:<trigger>:<now.isoformat()>. tx must be inside tenancy.InWorkspace.
func (s *Service) EnqueueManualRun(ctx context.Context, tx pgx.Tx, ws, triggerID string, rowIDs []string, dryRun bool, now time.Time) (int64, string, error) {
	if _, err := gen.New(tx).AutoSvcGetTrigger(ctx, gen.AutoSvcGetTriggerParams{ID: triggerID, WorkspaceID: ws}); errors.Is(err, pgx.ErrNoRows) {
		return 0, "", errTriggerNotFound
	} else if err != nil {
		return 0, "", err
	}
	p := manualPayload{TriggerID: triggerID, WorkspaceID: ws, FireSource: "manual",
		FireKey: "manual:" + triggerID + ":" + Iso(now), DryRun: dryRun}
	if len(rowIDs) > 0 {
		t, err := s.PreviewTargets(ctx, tx, ws, rowIDs)
		if err != nil {
			return 0, "", err
		}
		p.Targets = &t
	}
	id, err := gen.New(tx).AutoSvcInsertEvalJob(ctx, gen.AutoSvcInsertEvalJobParams{
		Executor: s.executor(ws), Payload: mustJSON(p), WorkspaceID: ws})
	if err != nil {
		return 0, "", fmt.Errorf("autosvc: enqueue manual run: %w", err)
	}
	return int64(id), p.FireKey, nil
}

// Runs is GET /triggers/{id}/runs: newest first. A negative limit is a 422
// (Python passes it to LIMIT and 500s, D4).
func (s *Service) Runs(ctx context.Context, ws, triggerID string, limit int) ([]Run, error) {
	if limit < 0 {
		return nil, herr(422, "limit must be non-negative")
	}
	if limit > 1000 {
		limit = 1000
	}
	var out []Run
	err := tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		q := gen.New(tx)
		if _, err := q.AutoSvcGetTrigger(ctx, gen.AutoSvcGetTriggerParams{ID: triggerID, WorkspaceID: ws}); errors.Is(err, pgx.ErrNoRows) {
			return errTriggerNotFound
		} else if err != nil {
			return err
		}
		rows, err := q.AutoSvcRecentRuns(ctx, gen.AutoSvcRecentRunsParams{WorkspaceID: ws, TriggerID: triggerID, RowLimit: int32(limit)})
		if err != nil {
			return err
		}
		out = make([]Run, len(rows))
		for i, r := range rows {
			out[i] = toRun(r)
		}
		return nil
	})
	return out, err
}

// RunWithResults is GET /runs/{id}: the run and its action results by id.
func (s *Service) RunWithResults(ctx context.Context, ws, runID string) (RunDetail, error) {
	var out RunDetail
	err := tenancy.InWorkspace(ctx, s.Pool, ws, func(tx pgx.Tx) error {
		q := gen.New(tx)
		row, err := q.AutoSvcGetRun(ctx, gen.AutoSvcGetRunParams{ID: runID, WorkspaceID: ws})
		if errors.Is(err, pgx.ErrNoRows) {
			return errRunNotFound
		}
		if err != nil {
			return err
		}
		out.Run = toRun(row)
		rs, err := q.AutoSvcRunResults(ctx, gen.AutoSvcRunResultsParams{WorkspaceID: ws, RunID: runID})
		if err != nil {
			return err
		}
		out.ActionResults = make([]ActionResult, len(rs))
		for i, r := range rs {
			out.ActionResults[i] = toResult(r)
		}
		return nil
	})
	return out, err
}
