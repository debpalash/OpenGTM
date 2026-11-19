-- Bridge data step: the queue rows a v3.0.0 install leaves behind, moved onto the Go worker. Same statements as
-- steps 1.3, 3 and 4 of docs/go-port/runbooks/15-cutover.md (which documents what each does), run in the bridge
-- transaction after the DDL. Precondition (stated in the runbooks): the v3.0.0 api, worker and scheduler are stopped,
-- so a 'processing' row belongs to a worker that no longer exists.

-- Rows a stopped v3.0.0 worker was holding go back to the queue.
UPDATE jobs SET status = 'pending', worker_id = NULL, locked_at = NULL, started_at = NULL,
       last_heartbeat = NULL, next_run_at = now() AT TIME ZONE 'UTC'
 WHERE executor = 'python' AND status = 'processing';

-- Tenant of pending rows that lack one: from the entity the payload names, else from the payload's own workspace.
UPDATE jobs j SET workspace_id = w.workspace_id FROM workbooks w
 WHERE j.workspace_id IS NULL AND j.type = 'refresh_workbook' AND j.status = 'pending'
   AND w.id = j.payload->>'workbook_id';
UPDATE jobs j SET workspace_id = a.workspace_id FROM audiences a
 WHERE j.workspace_id IS NULL AND j.type = 'audience_refresh' AND j.status = 'pending'
   AND a.id = j.payload->>'audience_id';
UPDATE jobs j SET workspace_id = r.workspace_id FROM destination_runs r
 WHERE j.workspace_id IS NULL AND j.type = 'audience_destination_sync' AND j.status = 'pending'
   AND r.id = j.payload->>'run_id';
UPDATE jobs j SET workspace_id = t.workspace_id FROM triggers t
 WHERE j.workspace_id IS NULL AND j.type = 'trigger_eval' AND j.status = 'pending'
   AND t.id = j.payload->>'trigger_id';
UPDATE jobs j SET workspace_id = s.workspace_id FROM watch_subscriptions s
 WHERE j.workspace_id IS NULL AND j.type = 'watch_poll' AND j.status = 'pending'
   AND s.id = j.payload->>'watch_id';
-- workspaces is empty here (the bridge only creates the table); the payload's workspace is accepted when the
-- workspaces row exists, which is the case only after `import-v3`. Rows that still lack a tenant are failed below
-- with a reason instead of sitting in the queue. Re-running `migrate up` after `import-v3` does not redo this
-- step (the bridge runs once); see docs/operations/release-rehearsals.md.
UPDATE jobs j SET workspace_id = j.payload->>'workspace_id'
 WHERE j.workspace_id IS NULL AND j.type <> 'source_health_check' AND j.status = 'pending'
   AND j.payload->>'workspace_id' IS NOT NULL AND j.payload->>'workspace_id' <> '';
UPDATE jobs SET status = 'failed', completed_at = now() AT TIME ZONE 'UTC',
       error = 'cutover: no workspace_id and no entity to derive it from'
 WHERE workspace_id IS NULL AND status = 'pending' AND type <> 'source_health_check';

-- Everything still waiting is the Go worker's.
UPDATE jobs SET executor = 'go' WHERE executor = 'python' AND status = 'pending';
