-- The retention_enforce parity dataset. Loaded after dataset_lib.sql, as the
-- schema owner, into two identical fresh databases; scenarios.json then drives
-- the Python handler on one and the Go handler on the other.
--
-- "Now" is the frozen instant 2026-06-15 12:00:00 UTC. Each workspace below
-- isolates one behaviour; see scenarios.json for the steps that exercise it.

CREATE FUNCTION pg_temp.defdays() RETURNS json LANGUAGE sql AS $$
  SELECT '{"audit": 365, "llm_usage": 365, "signals": 365, "activation": 180, "audience_history": 365, "agent_results": 180, "outreach_history": 365}'::json $$;

CREATE FUNCTION pg_temp.policy(ws text, en boolean, hold boolean, days json) RETURNS void LANGUAGE sql AS $$
  INSERT INTO retention_policies (workspace_id, enabled, legal_hold, retention_days) VALUES (ws, en, hold, days) $$;

CREATE FUNCTION pg_temp.run(ws text, id text, by text, snap json, st text DEFAULT 'pending') RETURNS void LANGUAGE sql AS $$
  INSERT INTO retention_runs (id, workspace_id, status, requested_by, policy_snapshot, deleted_counts, started_at)
  VALUES (id, ws, st, by, snap, '{}'::json, CASE WHEN st = 'running' THEN TIMESTAMP '2026-06-15 11:00:00' END) $$;

-- A claimed (processing, leased) job by default; a queued one when st = 'pending'.
CREATE FUNCTION pg_temp.job(jid int, ws text, payload json, st text DEFAULT 'processing', fk text DEFAULT NULL)
RETURNS void LANGUAGE sql AS $$
  INSERT INTO jobs (id, type, payload, workspace_id, priority, status, created_at, started_at, next_run_at,
                    max_retries, retry_count, worker_id, locked_at, last_heartbeat, fire_key)
  VALUES (jid, 'retention_enforce', payload, ws, 1, st, TIMESTAMP '2026-06-15 11:59:00',
          CASE WHEN st = 'processing' THEN TIMESTAMP '2026-06-15 11:59:30' END,
          TIMESTAMP '2026-06-15 11:59:00', 3, 0,
          CASE WHEN st = 'processing' THEN 'parity-worker' END,
          CASE WHEN st = 'processing' THEN TIMESTAMP '2026-06-15 11:59:30' END,
          CASE WHEN st = 'processing' THEN TIMESTAMP '2026-06-15 11:59:30' END, fk) $$;

CREATE FUNCTION pg_temp.setup(ws text, en boolean, hold boolean, days json) RETURNS void LANGUAGE sql AS $$
  SELECT pg_temp.seed_rows(ws, TIMESTAMP '2026-06-15 12:00:00');
  SELECT pg_temp.policy(ws, en, hold, days) $$;

-- Manual run on a default policy: deletes expired rows, reschedules, and
-- cancels the stale pending jobs (a completed job with the next key does not
-- block the new one; a job of another workspace is untouched).
SELECT pg_temp.setup('ws-default', true, false, pg_temp.defdays());
SELECT pg_temp.run('ws-default', 'run-default', '1', pg_temp.defdays());
SELECT pg_temp.job(1001, 'ws-default', '{"workspace_id": "ws-default", "run_id": "run-default"}');
SELECT pg_temp.job(2001, 'ws-default', '{"workspace_id": "ws-default"}', 'pending', 'retention:ws-default:2026-06-14');
SELECT pg_temp.job(2002, 'ws-default', '{"workspace_id": "ws-default"}', 'pending', 'retention:ws-default:2026-06-16');
SELECT pg_temp.job(2003, 'ws-other',   '{"workspace_id": "ws-other"}',   'pending', 'retention:ws-other:2026-06-16');
SELECT pg_temp.job(2004, 'ws-default', '{"workspace_id": "ws-default"}', 'completed', 'retention:ws-default:2026-06-16');

-- Custom retention_days, to prove each category uses its own window.
SELECT pg_temp.setup('ws-custom', true, false,
  '{"audit": 90, "llm_usage": 30, "signals": 30, "activation": 100, "audience_history": 3650, "agent_results": 45, "outreach_history": 365}');
SELECT pg_temp.run('ws-custom', 'run-custom', '9',
  '{"audit": 90, "llm_usage": 30, "signals": 30, "activation": 100, "audience_history": 3650, "agent_results": 45, "outreach_history": 365}');
SELECT pg_temp.job(1002, 'ws-custom', '{"workspace_id": "ws-custom", "run_id": "run-custom"}');

-- Rows and a run but no policy: the handler does nothing.
SELECT pg_temp.seed_rows('ws-nopolicy', TIMESTAMP '2026-06-15 12:00:00');
SELECT pg_temp.run('ws-nopolicy', 'run-nopolicy', '1', pg_temp.defdays());
SELECT pg_temp.job(1003, 'ws-nopolicy', '{"workspace_id": "ws-nopolicy", "run_id": "run-nopolicy"}');

-- Legal hold blocks even a manual run, clears the schedule mirror, cancels the
-- pending scheduled job and schedules nothing.
SELECT pg_temp.setup('ws-hold', true, true, pg_temp.defdays());
SELECT pg_temp.run('ws-hold', 'run-hold', '1', pg_temp.defdays());
SELECT pg_temp.job(1004, 'ws-hold', '{"workspace_id": "ws-hold", "run_id": "run-hold"}');
SELECT pg_temp.job(2005, 'ws-hold', '{"workspace_id": "ws-hold"}', 'pending', 'retention:ws-hold:2026-06-16');
INSERT INTO retention_schedules (workspace_id, enabled, next_run_at) VALUES ('ws-hold', true, TIMESTAMP '2026-06-16 12:00:00');
UPDATE retention_policies SET next_run_at = TIMESTAMP '2026-06-16 12:00:00' WHERE workspace_id = 'ws-hold';

-- Disabled policy: a scheduled (run-less) job is cancelled, a manual run still executes.
SELECT pg_temp.setup('ws-disabled', false, false, pg_temp.defdays());
SELECT pg_temp.job(1005, 'ws-disabled', '{"workspace_id": "ws-disabled"}');
SELECT pg_temp.setup('ws-disabled-manual', false, false, pg_temp.defdays());
SELECT pg_temp.run('ws-disabled-manual', 'run-dm', '7', pg_temp.defdays());
SELECT pg_temp.job(1006, 'ws-disabled-manual', '{"workspace_id": "ws-disabled-manual", "run_id": "run-dm"}');

-- A scheduled job for an enabled policy creates its own run.
SELECT pg_temp.setup('ws-sched', true, false, pg_temp.defdays());
SELECT pg_temp.job(1023, 'ws-sched', '{"workspace_id": "ws-sched"}');

-- Invalid stored retention_days: the job fails before any run exists.
SELECT pg_temp.setup('ws-badcat',  true, false, '{"leads": 30, "audit": 5, "zeta": 1}');
SELECT pg_temp.job(1007, 'ws-badcat',  '{"workspace_id": "ws-badcat"}');
SELECT pg_temp.setup('ws-badmin',  true, false, '{"audit": 30}');
SELECT pg_temp.job(1008, 'ws-badmin',  '{"workspace_id": "ws-badmin"}');
SELECT pg_temp.setup('ws-badint',  true, false, '{"signals": "abc"}');
SELECT pg_temp.job(1009, 'ws-badint',  '{"workspace_id": "ws-badint"}');
SELECT pg_temp.setup('ws-badnull', true, false, '{"audit": null}');
SELECT pg_temp.job(1010, 'ws-badnull', '{"workspace_id": "ws-badnull"}');
SELECT pg_temp.setup('ws-coerce',  true, false, '{"audit": "100", "signals": 99.9, "llm_usage": " +4_0 "}');
SELECT pg_temp.job(1024, 'ws-coerce',  '{"workspace_id": "ws-coerce"}');

-- Mid-run failure: the snapshot has no "activation" key, so the purge raises
-- KeyError('activation') after audit, llm_usage and signals were already
-- deleted in the transaction; everything must roll back.
SELECT pg_temp.setup('ws-midfail', true, false, pg_temp.defdays());
SELECT pg_temp.run('ws-midfail', 'run-mid', '3',
  '{"audit": 365, "llm_usage": 365, "signals": 365, "audience_history": 365, "agent_results": 180, "outreach_history": 365}');
SELECT pg_temp.job(1011, 'ws-midfail', '{"workspace_id": "ws-midfail", "run_id": "run-mid"}');

-- Odd stored snapshots: the error text Python records is str(exception).
SELECT pg_temp.setup('ws-snap-str', true, false, pg_temp.defdays());
SELECT pg_temp.run('ws-snap-str', 'run-snap-str', '1', '{"audit": "abc", "llm_usage": 365, "signals": 365, "activation": 180, "audience_history": 365, "agent_results": 180, "outreach_history": 365}');
SELECT pg_temp.job(1012, 'ws-snap-str', '{"workspace_id": "ws-snap-str", "run_id": "run-snap-str"}');
SELECT pg_temp.setup('ws-snap-null', true, false, pg_temp.defdays());
SELECT pg_temp.run('ws-snap-null', 'run-snap-null', '1', '{"audit": null, "llm_usage": 365, "signals": 365, "activation": 180, "audience_history": 365, "agent_results": 180, "outreach_history": 365}');
SELECT pg_temp.job(1013, 'ws-snap-null', '{"workspace_id": "ws-snap-null", "run_id": "run-snap-null"}');
SELECT pg_temp.setup('ws-snap-list', true, false, pg_temp.defdays());
SELECT pg_temp.run('ws-snap-list', 'run-snap-list', '1', '{"audit": [1], "llm_usage": 365, "signals": 365, "activation": 180, "audience_history": 365, "agent_results": 180, "outreach_history": 365}');
SELECT pg_temp.job(1014, 'ws-snap-list', '{"workspace_id": "ws-snap-list", "run_id": "run-snap-list"}');
SELECT pg_temp.setup('ws-snap-float', true, false, pg_temp.defdays());
SELECT pg_temp.run('ws-snap-float', 'run-snap-float', '1', '{"audit": 100.5, "llm_usage": 30.25, "signals": 365.75, "activation": 180, "audience_history": 365, "agent_results": 180, "outreach_history": 365}');
SELECT pg_temp.job(1015, 'ws-snap-float', '{"workspace_id": "ws-snap-float", "run_id": "run-snap-float"}');
SELECT pg_temp.setup('ws-snap-bool', true, false, pg_temp.defdays());
SELECT pg_temp.run('ws-snap-bool', 'run-snap-bool', '1', '{"audit": true, "llm_usage": false, "signals": 365, "activation": 180, "audience_history": 365, "agent_results": 180, "outreach_history": 365}');
SELECT pg_temp.job(1016, 'ws-snap-bool', '{"workspace_id": "ws-snap-bool", "run_id": "run-snap-bool"}');
SELECT pg_temp.setup('ws-snap-huge', true, false, pg_temp.defdays());
SELECT pg_temp.run('ws-snap-huge', 'run-snap-huge', '1', '{"audit": 1000000000, "llm_usage": 365, "signals": 365, "activation": 180, "audience_history": 365, "agent_results": 180, "outreach_history": 365}');
SELECT pg_temp.job(1017, 'ws-snap-huge', '{"workspace_id": "ws-snap-huge", "run_id": "run-snap-huge"}');
SELECT pg_temp.setup('ws-snap-range', true, false, pg_temp.defdays());
SELECT pg_temp.run('ws-snap-range', 'run-snap-range', '1', '{"audit": 800000000, "llm_usage": 365, "signals": 365, "activation": 180, "audience_history": 365, "agent_results": 180, "outreach_history": 365}');
SELECT pg_temp.job(1018, 'ws-snap-range', '{"workspace_id": "ws-snap-range", "run_id": "run-snap-range"}');
SELECT pg_temp.setup('ws-snap-neg', true, false, pg_temp.defdays());
SELECT pg_temp.run('ws-snap-neg', 'run-snap-neg', '1', '{"audit": -5, "llm_usage": 365, "signals": 365, "activation": 180, "audience_history": 365, "agent_results": 180, "outreach_history": 365}');
SELECT pg_temp.job(1019, 'ws-snap-neg', '{"workspace_id": "ws-snap-neg", "run_id": "run-snap-neg"}');
SELECT pg_temp.setup('ws-snap-extra', true, false, pg_temp.defdays());
SELECT pg_temp.run('ws-snap-extra', 'run-snap-extra', '1', '{"bogus": 1, "audit": 365, "llm_usage": 365, "signals": 365, "activation": 180, "audience_history": 365, "agent_results": 180, "outreach_history": 365}');
SELECT pg_temp.job(1020, 'ws-snap-extra', '{"workspace_id": "ws-snap-extra", "run_id": "run-snap-extra"}');
SELECT pg_temp.setup('ws-snap-zero', true, false, pg_temp.defdays());
SELECT pg_temp.run('ws-snap-zero', 'run-snap-zero', '1', '{"audit": 0, "llm_usage": 0, "signals": 0, "activation": 0, "audience_history": 0, "agent_results": 0, "outreach_history": 0}');
SELECT pg_temp.job(1021, 'ws-snap-zero', '{"workspace_id": "ws-snap-zero", "run_id": "run-snap-zero"}');
SELECT pg_temp.setup('ws-snap-array', true, false, pg_temp.defdays());
SELECT pg_temp.run('ws-snap-array', 'run-snap-array', '1', '[1, 2]');
SELECT pg_temp.job(1022, 'ws-snap-array', '{"workspace_id": "ws-snap-array", "run_id": "run-snap-array"}');

-- A run id that belongs to another workspace is invisible (RLS plus the
-- workspace filter): the handler makes its own run and leaves the foreign one.
SELECT pg_temp.setup('ws-foreign', true, false, pg_temp.defdays());
SELECT pg_temp.job(1025, 'ws-foreign', '{"workspace_id": "ws-foreign", "run_id": "run-custom"}');

-- '_' is a LIKE wildcard in the pending-job cancellation: ws_wild also
-- cancels the look-alike workspace's pending job.
SELECT pg_temp.setup('ws_wild', true, false, pg_temp.defdays());
SELECT pg_temp.run('ws_wild', 'run-wild', '1', pg_temp.defdays());
SELECT pg_temp.job(1030, 'ws_wild', '{"workspace_id": "ws_wild", "run_id": "run-wild"}');
SELECT pg_temp.job(2006, 'wsXwild', '{"workspace_id": "wsXwild"}', 'pending', 'retention:wsXwild:2026-06-20');

-- An active job already holds the next fire_key: nothing may be enqueued.
SELECT pg_temp.setup('ws-dupkey', true, false, pg_temp.defdays());
SELECT pg_temp.run('ws-dupkey', 'run-dup', '1', pg_temp.defdays());
SELECT pg_temp.job(1031, 'ws-dupkey', '{"workspace_id": "ws-dupkey", "run_id": "run-dup"}');
SELECT pg_temp.job(2007, 'ws-dupkey', '{"workspace_id": "ws-dupkey"}', 'processing', 'retention:ws-dupkey:2026-06-16');

-- A run already in a terminal state is never driven again.
SELECT pg_temp.setup('ws-terminal', true, false, pg_temp.defdays());
SELECT pg_temp.run('ws-terminal', 'run-done', '1', pg_temp.defdays(), 'completed');
SELECT pg_temp.run('ws-terminal', 'run-cancelled', '1', pg_temp.defdays(), 'cancelled');
SELECT pg_temp.job(1032, 'ws-terminal', '{"workspace_id": "ws-terminal", "run_id": "run-done"}');
SELECT pg_temp.job(1033, 'ws-terminal', '{"workspace_id": "ws-terminal", "run_id": "run-cancelled"}');

-- A previously failed run is run again (only completed/cancelled are skipped).
SELECT pg_temp.setup('ws-rerun', true, false, pg_temp.defdays());
SELECT pg_temp.run('ws-rerun', 'run-rerun', '1', pg_temp.defdays(), 'failed');
SELECT pg_temp.job(1034, 'ws-rerun', '{"workspace_id": "ws-rerun", "run_id": "run-rerun"}');

-- Failure reconciliation (jobs the queue has already finalized).
SELECT pg_temp.policy('ws-rc-hold', true, true, pg_temp.defdays());
SELECT pg_temp.job(1101, 'ws-rc-hold', '{"workspace_id": "ws-rc-hold"}', 'pending');
SELECT pg_temp.policy('ws-rc-hold-failed', true, true, pg_temp.defdays());
SELECT pg_temp.job(1102, 'ws-rc-hold-failed', '{"workspace_id": "ws-rc-hold-failed"}', 'failed');
SELECT pg_temp.policy('ws-rc-retry', true, false, '{"audit": 120}');
SELECT pg_temp.job(1103, 'ws-rc-retry', '{"workspace_id": "ws-rc-retry", "fire_key": "retention:ws-rc-retry:x"}', 'pending');
SELECT pg_temp.policy('ws-rc-final', true, false, pg_temp.defdays());
SELECT pg_temp.run('ws-rc-final', 'run-rc-final', '5', pg_temp.defdays(), 'running');
SELECT pg_temp.job(1104, 'ws-rc-final', '{"workspace_id": "ws-rc-final", "run_id": "run-rc-final"}', 'failed');
SELECT pg_temp.policy('ws-rc-final-off', false, false, pg_temp.defdays());
SELECT pg_temp.run('ws-rc-final-off', 'run-rc-final-off', '5', pg_temp.defdays(), 'running');
SELECT pg_temp.job(1105, 'ws-rc-final-off', '{"workspace_id": "ws-rc-final-off", "run_id": "run-rc-final-off"}', 'failed');
SELECT pg_temp.job(1106, 'ws-rc-nopolicy', '{"workspace_id": "ws-rc-nopolicy"}', 'failed');
SELECT pg_temp.policy('ws-rc-done', true, false, pg_temp.defdays());
SELECT pg_temp.run('ws-rc-done', 'run-rc-done', '5', pg_temp.defdays(), 'completed');
SELECT pg_temp.job(1107, 'ws-rc-done', '{"workspace_id": "ws-rc-done", "run_id": "run-rc-done"}', 'failed');
SELECT pg_temp.policy('ws-rc-cancelled', true, false, pg_temp.defdays());
SELECT pg_temp.run('ws-rc-cancelled', 'run-rc-cancelled', '5', pg_temp.defdays(), 'cancelled');
SELECT pg_temp.job(1108, 'ws-rc-cancelled', '{"workspace_id": "ws-rc-cancelled", "run_id": "run-rc-cancelled"}', 'pending');
SELECT pg_temp.job(1109, NULL, '{}', 'failed');
SELECT pg_temp.policy('ws-rc-long', false, false, pg_temp.defdays());
SELECT pg_temp.job(1110, 'ws-rc-long', '{"workspace_id": "ws-rc-long"}', 'failed');
SELECT pg_temp.policy('ws-rc-foreign', true, false, pg_temp.defdays());
SELECT pg_temp.job(1111, 'ws-rc-foreign', '{"workspace_id": "ws-rc-foreign", "run_id": "run-rc-final"}', 'pending');
SELECT pg_temp.policy('ws-rc-badpolicy', true, false, '{"leads": 1}');
SELECT pg_temp.job(1112, 'ws-rc-badpolicy', '{"workspace_id": "ws-rc-badpolicy"}', 'pending');
SELECT pg_temp.policy('ws-rc-spaces', true, false, pg_temp.defdays());
SELECT pg_temp.run('ws-rc-spaces', 'run-rc-spaces', '5', pg_temp.defdays(), 'running');
SELECT pg_temp.job(1113, 'ws-rc-spaces', '{"workspace_id": " ws-rc-spaces ", "run_id": " run-rc-spaces "}', 'failed');

-- A database error part-way through the purge (the signals delete raises).
-- Python and Go word the error differently, so scenarios compare it by
-- substring; the state left behind must still be identical.
CREATE FUNCTION public.retention_parity_boom() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN IF OLD.workspace_id = 'ws-trigger' THEN RAISE EXCEPTION 'boom'; END IF; RETURN OLD; END $$;
CREATE TRIGGER retention_parity_boom BEFORE DELETE ON signals FOR EACH ROW EXECUTE FUNCTION public.retention_parity_boom();
SELECT pg_temp.setup('ws-trigger', true, false, pg_temp.defdays());
SELECT pg_temp.run('ws-trigger', 'run-trigger', '1', pg_temp.defdays());
SELECT pg_temp.job(1040, 'ws-trigger', '{"workspace_id": "ws-trigger", "run_id": "run-trigger"}');
