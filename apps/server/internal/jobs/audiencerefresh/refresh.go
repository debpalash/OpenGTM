package audiencerefresh

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/debpalash/OpenGTM/apps/server/internal/jobs/jobkit"
	"github.com/debpalash/OpenGTM/apps/server/internal/queue"
)

type refreshResult struct {
	matched, entered, changed, exited int
}

type leadSnapshot struct {
	id   int64
	json string
}

// refresh ports refresh_audience and what follows its commit: it re-evaluates
// the audience's filter page by page, applies the diff to audience_members
// with durable entered/exited events, updates the audience's member_count and
// refreshed_at, then emits automation triggers for the events and enqueues
// destination syncs when anything changed. It runs in the caller's tenant
// transaction, with the audience row already locked.
func (w *Worker) refresh(ctx context.Context, tx pgx.Tx, workspaceID string, a *audience, now time.Time) (refreshResult, error) {
	var res refreshResult
	filters, err := jobkit.Decode([]byte(a.filters))
	if err != nil {
		return res, fmt.Errorf("decode audience filters: %w", err)
	}
	filter, err := buildLeadFilter(filters, 2) // $1 workspace, $2 now (the snapshot's created_at fallback)
	if err != nil {
		return res, err
	}
	copy(filter.args, []any{workspaceID, jobkit.IsoFormat(now)})

	var refreshID string
	if err := tx.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&refreshID); err != nil {
		return res, err
	}

	size := w.opts.PageSize
	for page := 1; ; page++ {
		leads, total, err := queryLeadsPage(ctx, tx, filter, page, size)
		if err != nil {
			return res, err
		}
		entered, changed, err := w.applyPage(ctx, tx, workspaceID, a.id, refreshID, leads, now)
		if err != nil {
			return res, err
		}
		res.entered += entered
		res.changed += changed
		res.matched += len(leads)
		if res.matched >= total || len(leads) == 0 {
			break
		}
	}

	exited, err := w.exitMembers(ctx, tx, workspaceID, a.id, refreshID, size)
	if err != nil {
		return res, err
	}
	res.exited = exited

	if _, err := tx.Exec(ctx, `UPDATE audiences
		SET member_count = $3, refreshed_at = `+jobkit.Stamp(4)+`, updated_at = now()
		WHERE id = $1 AND workspace_id = $2`, a.id, workspaceID, res.matched, now); err != nil {
		return res, fmt.Errorf("update audience: %w", err)
	}

	if res.entered > 0 || res.exited > 0 {
		if err := w.emitRefreshEvents(ctx, tx, workspaceID, a.id, refreshID, size); err != nil {
			return res, err
		}
	}
	if res.entered > 0 || res.exited > 0 || res.changed > 0 {
		if _, err := enqueueAudienceSyncs(ctx, tx, workspaceID, a.id); err != nil {
			return res, err
		}
	}
	return res, nil
}

// queryLeadsPage is PgLeadStore.query_leads_page: a filtered page of the
// tenant's leads ordered by score, as snapshots, plus the total match count.
//
// Python orders by score alone, so leads with equal scores may move between
// pages (or be skipped or repeated at a page boundary); the tie-break on id
// makes the paging stable and is the only difference.
func queryLeadsPage(ctx context.Context, tx pgx.Tx, f *leadFilter, page, size int) ([]leadSnapshot, int, error) {
	// $2 (the clock text for the snapshot's created_at fallback) is referenced
	// by every statement so PostgreSQL can type it in the count query too.
	where := " FROM leads l WHERE l.workspace_id = $1 AND $2::text IS NOT NULL" + f.where()
	var total int
	if err := tx.QueryRow(ctx, "SELECT count(*)"+where, f.args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args := append(append([]any{}, f.args...), size, (max(1, page)-1)*max(1, size))
	limit, offset := len(args)-1, len(args)
	rows, err := tx.Query(ctx, fmt.Sprintf("SELECT l.id, %s%s ORDER BY l.score DESC, l.id ASC LIMIT $%d OFFSET $%d",
		snapshotSQL("$2"), where, limit, offset), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []leadSnapshot
	seen := map[int64]int{}
	for rows.Next() {
		var s leadSnapshot
		if err := rows.Scan(&s.id, &s.json); err != nil {
			return nil, 0, err
		}
		// `{int(row["id"]): row ...}`: a repeated id keeps its first position
		// and its last value.
		if i, dup := seen[s.id]; dup {
			out[i] = s
			continue
		}
		seen[s.id] = len(out)
		out = append(out, s)
	}
	return out, total, rows.Err()
}

// applyPage upserts one page of snapshots into audience_members: unseen leads
// become members with an "entered" event; existing members get their snapshot
// (when it changed), refresh token and last_seen_at refreshed. It returns the
// number of entered and changed members.
func (w *Worker) applyPage(ctx context.Context, tx pgx.Tx, workspaceID, audienceID, refreshID string, leads []leadSnapshot, now time.Time) (entered, changed int, err error) {
	if len(leads) == 0 {
		return 0, 0, nil
	}
	ids := make([]int64, len(leads))
	snaps := make([]string, len(leads))
	for i, l := range leads {
		ids[i], snaps[i] = l.id, l.json
	}
	// Which leads are already members, and is their stored snapshot equal?
	rows, err := tx.Query(ctx, `SELECT n.lead_id, (m.snapshot::jsonb IS NOT DISTINCT FROM n.snap::jsonb)
		FROM unnest($3::bigint[], $4::text[]) AS n(lead_id, snap)
		JOIN audience_members m ON m.lead_id = n.lead_id AND m.workspace_id = $1 AND m.audience_id = $2`,
		workspaceID, audienceID, ids, snaps)
	if err != nil {
		return 0, 0, err
	}
	same := map[int64]bool{}
	for rows.Next() {
		var id int64
		var eq bool
		if err := rows.Scan(&id, &eq); err != nil {
			rows.Close()
			return 0, 0, err
		}
		same[id] = eq
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}

	var newIDs, oldIDs []int64
	var newSnaps, oldSnaps []string
	var oldSame []bool
	for i, id := range ids {
		eq, member := same[id]
		switch {
		case !member:
			newIDs, newSnaps = append(newIDs, id), append(newSnaps, snaps[i])
		default:
			oldIDs, oldSnaps, oldSame = append(oldIDs, id), append(oldSnaps, snaps[i]), append(oldSame, eq)
			if !eq {
				changed++
			}
		}
	}
	if len(newIDs) > 0 {
		// One statement per table, in page order, so serial ids follow the
		// order Python's unit of work inserts them in.
		if _, err := tx.Exec(ctx, `WITH n AS (
				SELECT lead_id, snap, ord FROM unnest($3::bigint[], $4::text[]) WITH ORDINALITY AS t(lead_id, snap, ord)),
			members AS (
				INSERT INTO audience_members (workspace_id, audience_id, lead_id, snapshot, refresh_token, joined_at, last_seen_at)
				SELECT $1, $2, lead_id, snap::json, $5, `+jobkit.Stamp(6)+`, `+jobkit.Stamp(6)+`
				FROM n ORDER BY ord RETURNING 1)
			INSERT INTO audience_membership_events (workspace_id, audience_id, lead_id, event_type, snapshot, refresh_id)
			SELECT $1, $2, lead_id, 'entered', snap::json, $5 FROM n ORDER BY ord`,
			workspaceID, audienceID, newIDs, newSnaps, refreshID, now); err != nil {
			return 0, 0, fmt.Errorf("insert audience members: %w", err)
		}
		entered = len(newIDs)
	}
	if len(oldIDs) > 0 {
		if _, err := tx.Exec(ctx, `UPDATE audience_members m
			SET snapshot = CASE WHEN n.same THEN m.snapshot ELSE n.snap::json END,
			    refresh_token = $5, last_seen_at = `+jobkit.Stamp(6)+`
			FROM unnest($3::bigint[], $4::text[], $7::bool[]) AS n(lead_id, snap, same)
			WHERE m.workspace_id = $1 AND m.audience_id = $2 AND m.lead_id = n.lead_id`,
			workspaceID, audienceID, oldIDs, oldSnaps, refreshID, now, oldSame); err != nil {
			return 0, 0, fmt.Errorf("update audience members: %w", err)
		}
	}
	return entered, changed, nil
}

// exitMembers records an "exited" event for, and deletes, every member the
// refresh did not see (a different or missing refresh token), in id order and
// in pages. It returns how many left.
func (w *Worker) exitMembers(ctx context.Context, tx pgx.Tx, workspaceID, audienceID, refreshID string, size int) (int, error) {
	exited := 0
	err := jobkit.KeysetPages(ctx, size,
		func(ctx context.Context, after int64, limit int) ([]int64, error) {
			rows, err := tx.Query(ctx, `SELECT id FROM audience_members
				WHERE workspace_id = $1 AND audience_id = $2 AND (refresh_token IS NULL OR refresh_token <> $3) AND id > $4
				ORDER BY id LIMIT $5`, workspaceID, audienceID, refreshID, after, limit)
			if err != nil {
				return nil, err
			}
			return pgx.CollectRows(rows, pgx.RowTo[int64])
		},
		func(id int64) int64 { return id },
		func(page []int64) error {
			// `member.snapshot or {}`: a falsy stored snapshot reads as {}.
			tag, err := tx.Exec(ctx, `WITH gone AS (
					DELETE FROM audience_members WHERE workspace_id = $1 AND audience_id = $2 AND id = ANY($3::bigint[])
					RETURNING id, lead_id, snapshot)
				INSERT INTO audience_membership_events (workspace_id, audience_id, lead_id, event_type, snapshot, refresh_id)
				SELECT $1, $2, lead_id, 'exited',
				       CASE WHEN snapshot::jsonb IN ('null'::jsonb, '{}'::jsonb, '[]'::jsonb, '0'::jsonb, '""'::jsonb, 'false'::jsonb)
				            THEN '{}'::json ELSE snapshot END,
				       $4
				FROM gone ORDER BY id`, workspaceID, audienceID, page, refreshID)
			if err != nil {
				return fmt.Errorf("exit audience members: %w", err)
			}
			exited += int(tag.RowsAffected())
			return nil
		})
	return exited, err
}

// enqueueAudienceSyncs ports enqueue_audience_syncs: one pending
// destination_runs row and one audience_destination_sync job for every enabled
// destination of the audience that has no active run. The job type itself is
// still executed by Python.
func enqueueAudienceSyncs(ctx context.Context, tx pgx.Tx, workspaceID, audienceID string) (int, error) {
	rows, err := tx.Query(ctx, `SELECT id FROM audience_destinations
		WHERE workspace_id = $1 AND audience_id = $2 AND enabled IS TRUE ORDER BY created_at, id`, workspaceID, audienceID)
	if err != nil {
		return 0, err
	}
	destinations, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	enqueued := 0
	for _, destination := range destinations {
		var active bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM destination_runs
			WHERE workspace_id = $1 AND destination_id = $2 AND status IN ('pending', 'running', 'cancelling'))`,
			workspaceID, destination).Scan(&active); err != nil {
			return enqueued, err
		}
		if active {
			continue
		}
		var runID string
		if err := tx.QueryRow(ctx, `INSERT INTO destination_runs (id, workspace_id, destination_id, requested_by)
			VALUES (gen_random_uuid()::text, $1, $2, 'audience_refresh') RETURNING id`,
			workspaceID, destination).Scan(&runID); err != nil {
			return enqueued, fmt.Errorf("create destination run: %w", err)
		}
		if _, err := queue.Enqueue(ctx, tx, "audience_destination_sync",
			map[string]string{"workspace_id": workspaceID, "run_id": runID},
			queue.EnqueueOptions{FireKey: "destination_sync:" + runID}); err != nil {
			return enqueued, err
		}
		enqueued++
	}
	return enqueued, nil
}
