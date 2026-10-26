package retention

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/debpalash/OpenGTM/apps/backend-go/internal/db/gen"
	"github.com/debpalash/OpenGTM/apps/backend-go/internal/tenancy"
)

// payloadString is str(payload.get(key) or "").strip(): falsy values (null, false, 0,
// "", [], {}) read as empty, other scalars are stringified.
func payloadString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(x)
	case bool:
		if x {
			return "True"
		}
	case float64:
		if x != 0 && x == math.Trunc(x) && math.Abs(x) < 1e15 {
			return strconv.FormatInt(int64(x), 10)
		}
		if x != 0 {
			return strconv.FormatFloat(x, 'g', -1, 64)
		}
	}
	return ""
}

// Reconcile is reconcile_retention_job_failure: it settles the run after the worker
// running an enforcement died. An unknown run is created (scheduler) and linked into the
// queue job's payload; completed and cancelled runs are left alone; under a legal hold
// the run and a still-pending queue job are cancelled; a retry marks the run pending
// ("Queue retry scheduled: ..."); a final failure marks it failed ("Final failure: ...")
// and re-schedules when the policy is enabled. The message is cut to 1000 characters.
func Reconcile(ctx context.Context, d Deps, jobID int64, payload []byte, errMsg string, willRetry bool) error {
	var p map[string]any
	if len(bytes.TrimSpace(payload)) > 0 {
		if err := json.Unmarshal(payload, &p); err != nil {
			return fmt.Errorf("retention failure payload: %w", err)
		}
	}
	ws := payloadString(p["workspace_id"])
	if ws == "" {
		return errors.New("retention failure payload requires workspace_id")
	}
	runID := payloadString(p["run_id"])
	if errMsg == "" {
		errMsg = noMessage
	}
	message := truncRunes(errMsg, 1000)

	return tenancy.InWorkspace(ctx, d.Pool, ws, func(tx pgx.Tx) error {
		q := gen.New(tx)
		policy, err := GetPolicy(ctx, tx, ws)
		if err != nil {
			return err
		}
		var run gen.RetRunGetRow
		found := false
		if runID != "" {
			run, err = q.RetRunGet(ctx, gen.RetRunGetParams{ID: runID, WorkspaceID: ws})
			if err == nil {
				found = true
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("retention: get run: %w", err)
			}
		}
		job, jobErr := q.RetJobGet(ctx, int32(jobID))
		if jobErr != nil && !errors.Is(jobErr, pgx.ErrNoRows) {
			return fmt.Errorf("retention: get job: %w", jobErr)
		}
		haveJob := jobErr == nil
		if !found {
			var snap []byte
			if policy != nil {
				snap, err = snapshotOf(policy)
			} else {
				snap, err = json.Marshal(Days(DefaultDays))
			}
			if err != nil {
				return err
			}
			id, err := q.RetRunCreate(ctx, gen.RetRunCreateParams{WorkspaceID: ws, PolicySnapshot: snap})
			if err != nil {
				return fmt.Errorf("retention: create run: %w", err)
			}
			if run, err = q.RetRunGet(ctx, gen.RetRunGetParams{ID: id, WorkspaceID: ws}); err != nil {
				return fmt.Errorf("retention: get run: %w", err)
			}
			if haveJob {
				merged, err := withRunID(job.Payload, id)
				if err != nil {
					return err
				}
				if err := q.RetJobSetPayload(ctx, gen.RetJobSetPayloadParams{ID: int32(jobID), Payload: merged}); err != nil {
					return fmt.Errorf("retention: link run: %w", err)
				}
			}
		}
		if run.Status == "completed" || run.Status == "cancelled" {
			return nil
		}
		now := d.now()
		if policy != nil && policy.LegalHold {
			if err := q.RetRunCancel(ctx, gen.RetRunCancelParams{ID: run.ID, WorkspaceID: ws, Error: errHoldMidRun, FinishedAt: tstz(now)}); err != nil {
				return fmt.Errorf("retention: cancel run: %w", err)
			}
			if haveJob && job.Status.String == "pending" {
				if err := q.RetJobCancelPending(ctx, gen.RetJobCancelPendingParams{ID: int32(jobID), Error: errHoldMidRun, CompletedAt: tstz(now)}); err != nil {
					return fmt.Errorf("retention: cancel job: %w", err)
				}
			}
			return nil
		}
		if willRetry {
			return q.RetRunRetry(ctx, gen.RetRunRetryParams{ID: run.ID, WorkspaceID: ws, Error: "Queue retry scheduled: " + message})
		}
		if err := finishFailed(ctx, tx, ws, run, "Final failure: "+message, now); err != nil {
			return err
		}
		if policy != nil && policy.Enabled {
			_, err = SchedulePolicy(ctx, tx, policy, now, d.ExecutorFor)
		}
		return err
	})
}

// finishFailed marks the run failed keeping the counts it already holds.
func finishFailed(ctx context.Context, tx pgx.Tx, ws string, run gen.RetRunGetRow, msg string, at time.Time) error {
	if err := gen.New(tx).RetRunFinish(ctx, gen.RetRunFinishParams{ID: run.ID, WorkspaceID: ws, Status: "failed",
		DeletedCounts: run.DeletedCounts, Error: pgtype.Text{String: msg, Valid: true}, FinishedAt: tstz(at)}); err != nil {
		return fmt.Errorf("retention: fail run: %w", err)
	}
	return nil
}

// withRunID is {**payload, "run_id": id}: key order kept, run_id replaced in place or
// appended, written with Python's json.dumps separators.
func withRunID(raw []byte, id string) ([]byte, error) {
	type kv struct {
		k string
		v json.RawMessage
	}
	var items []kv
	if len(bytes.TrimSpace(raw)) > 0 && string(bytes.TrimSpace(raw)) != "null" {
		dec := json.NewDecoder(bytes.NewReader(raw))
		if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
			return nil, errors.New("retention: queue job payload is not an object")
		}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			var v json.RawMessage
			if err := dec.Decode(&v); err != nil && !errors.Is(err, io.EOF) {
				return nil, err
			}
			items = append(items, kv{kt.(string), v})
		}
	}
	idJSON, _ := json.Marshal(id)
	replaced := false
	for i := range items {
		if items[i].k == "run_id" {
			items[i].v, replaced = idJSON, true
		}
	}
	if !replaced {
		items = append(items, kv{"run_id", idJSON})
	}
	var b bytes.Buffer
	b.WriteByte('{')
	for i, it := range items {
		if i > 0 {
			b.WriteString(", ")
		}
		k, _ := json.Marshal(it.k)
		b.Write(k)
		b.WriteString(": ")
		b.Write(it.v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}
