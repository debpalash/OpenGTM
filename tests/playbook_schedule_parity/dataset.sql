-- The research_playbook_schedule parity dataset. Loaded as the schema owner
-- into two identical fresh databases; scenarios.json then drives the Python
-- handler on one and the Go handler on the other.
--
-- "Now" is the frozen instant 2026-06-15 12:00:00.123456 UTC (the fractional
-- part is deliberate: it shows up in the fire_key of the next occurrence).
-- Each playbook below isolates one behaviour; see scenarios.json.

CREATE FUNCTION pg_temp.aud(ws text, id text) RETURNS void LANGUAGE sql AS $$
  INSERT INTO audiences (id, workspace_id, name, filters) VALUES (id, ws, 'name-' || id, '{}') $$;

CREATE FUNCTION pg_temp.pb(ws text, id text, en boolean, aud text, mins int, ver int DEFAULT 1,
                           steps json DEFAULT '[{"tool": "search"}]') RETURNS void LANGUAGE sql AS $$
  INSERT INTO research_playbooks (id, workspace_id, name, description, prompt_template, steps, output_format,
                                  max_steps, cell_budget_usd, version, enabled, schedule_audience_id,
                                  schedule_interval_minutes, next_run_at)
  VALUES (id, ws, 'pb-' || id, '', 'prompt for ' || id, steps, 'text', 4, 0.1, ver, en, aud, mins,
          TIMESTAMP '2026-06-15 11:00:00') $$;

CREATE FUNCTION pg_temp.run(ws text, id text, pbid text, aud text, st text) RETURNS void LANGUAGE sql AS $$
  INSERT INTO playbook_runs (id, workspace_id, playbook_id, audience_id, status, prompt_version, prompt_snapshot,
                             steps_snapshot, max_members, attempted, succeeded, failed, requested_by)
  VALUES (id, ws, pbid, aud, st, 1, 'old', '[]', 100, 0, 0, 0, 'seed') $$;

-- A claimed (processing, leased) tick by default; a queued job when st = 'pending'.
CREATE FUNCTION pg_temp.job(jid int, ws text, payload json, st text DEFAULT 'processing', fk text DEFAULT NULL,
                            typ text DEFAULT 'research_playbook_schedule') RETURNS void LANGUAGE sql AS $$
  INSERT INTO jobs (id, type, payload, workspace_id, priority, status, created_at, started_at, next_run_at,
                    max_retries, retry_count, worker_id, locked_at, last_heartbeat, fire_key)
  VALUES (jid, typ, payload, CASE WHEN fk IS NULL THEN ws END, 1, st, TIMESTAMP '2026-06-15 11:59:00',
          CASE WHEN st = 'processing' THEN TIMESTAMP '2026-06-15 11:59:30' END,
          TIMESTAMP '2026-06-15 11:59:00', 3, 0,
          CASE WHEN st = 'processing' THEN 'parity-worker' END,
          CASE WHEN st = 'processing' THEN TIMESTAMP '2026-06-15 11:59:30' END,
          CASE WHEN st = 'processing' THEN TIMESTAMP '2026-06-15 11:59:30' END, fk) $$;

CREATE FUNCTION pg_temp.tick(jid int, ws text, pbid text) RETURNS void LANGUAGE sql AS $$
  SELECT pg_temp.job(jid, ws, json_build_object('workspace_id', ws, 'playbook_id', pbid)) $$;

CREATE FUNCTION pg_temp.queued(jid int, ws text, pbid text, fk text, st text DEFAULT 'pending') RETURNS void LANGUAGE sql AS $$
  SELECT pg_temp.job(jid, ws, json_build_object('workspace_id', ws, 'playbook_id', pbid), st, fk) $$;

CREATE FUNCTION pg_temp.mirror(pbid text, ws text, en boolean) RETURNS void LANGUAGE sql AS $$
  INSERT INTO playbook_schedules (playbook_id, workspace_id, enabled, next_run_at)
  VALUES (pbid, ws, en, TIMESTAMP '2026-06-15 11:00:00') $$;

SELECT pg_temp.aud('ws-pb', 'aud-1');
SELECT pg_temp.aud('ws-pb', 'aud-2');
SELECT pg_temp.aud('ws-other', 'aud-o');

-- A normal active playbook: a tick creates a pending run and its job, books
-- the next occurrence 30 minutes out, cancels the stale pending occurrence but
-- not the processing one. A second tick finds the pending run and only
-- reschedules (the cancelled occurrence does not block the same key again).
SELECT pg_temp.pb('ws-pb', 'pb-a', true, 'aud-1', 30, 3);
SELECT pg_temp.mirror('pb-a', 'ws-pb', true);
SELECT pg_temp.tick(1001, 'ws-pb', 'pb-a');
SELECT pg_temp.queued(1901, 'ws-pb', 'pb-a', 'playbook_schedule:pb-a:2026-06-15T11:30:00+00:00');
SELECT pg_temp.queued(1902, 'ws-pb', 'pb-a', 'playbook_schedule:pb-a:2026-06-15T11:45:00+00:00', 'processing');
SELECT pg_temp.queued(1903, 'ws-pb', 'pb-a', 'playbook_schedule:pb-a:2026-06-15T10:00:00+00:00', 'completed');

-- Interval clamping: 5 -> 15 minutes, 99999 -> 10080.
SELECT pg_temp.pb('ws-pb', 'pb-b', true, 'aud-1', 5, 1, '[]');
SELECT pg_temp.tick(1002, 'ws-pb', 'pb-b');
SELECT pg_temp.pb('ws-pb', 'pb-c', true, 'aud-2', 99999, 7);
SELECT pg_temp.tick(1003, 'ws-pb', 'pb-c');

-- No usable interval (NULL, 0): the run is still created but nothing is
-- scheduled, the mirror is disabled and next_run_at cleared.
SELECT pg_temp.pb('ws-pb', 'pb-d', true, 'aud-1', NULL);
SELECT pg_temp.mirror('pb-d', 'ws-pb', true);
SELECT pg_temp.tick(1004, 'ws-pb', 'pb-d');
SELECT pg_temp.pb('ws-pb', 'pb-e', true, 'aud-1', 0);
SELECT pg_temp.tick(1005, 'ws-pb', 'pb-e');

-- Disabled and audience-less playbooks, and an unknown one, remove their
-- schedule: mirror deleted, pending (not processing) occurrences cancelled.
SELECT pg_temp.pb('ws-pb', 'pb-f', false, 'aud-1', 30);
SELECT pg_temp.mirror('pb-f', 'ws-pb', true);
SELECT pg_temp.tick(1006, 'ws-pb', 'pb-f');
SELECT pg_temp.queued(1904, 'ws-pb', 'pb-f', 'playbook_schedule:pb-f:2026-06-15T12:30:00+00:00');
SELECT pg_temp.queued(1905, 'ws-pb', 'pb-f', 'playbook_schedule:pb-f:2026-06-15T13:30:00+00:00', 'processing');
SELECT pg_temp.pb('ws-pb', 'pb-g', true, NULL, 30);
SELECT pg_temp.mirror('pb-g', 'ws-pb', true);
SELECT pg_temp.tick(1007, 'ws-pb', 'pb-g');
SELECT pg_temp.queued(1906, 'ws-pb', 'pb-g', 'playbook_schedule:pb-g:2026-06-15T12:30:00+00:00');
SELECT pg_temp.tick(1008, 'ws-pb', 'pb-ghost');
SELECT pg_temp.mirror('pb-ghost', 'ws-pb', true);

-- Only a pending or running run blocks a new one.
SELECT pg_temp.pb('ws-pb', 'pb-h', true, 'aud-1', 60);
SELECT pg_temp.run('ws-pb', 'run-h', 'pb-h', 'aud-1', 'running');
SELECT pg_temp.tick(1009, 'ws-pb', 'pb-h');
SELECT pg_temp.pb('ws-pb', 'pb-i', true, 'aud-1', 60);
SELECT pg_temp.run('ws-pb', 'run-i1', 'pb-i', 'aud-1', 'completed');
SELECT pg_temp.run('ws-pb', 'run-i2', 'pb-i', 'aud-1', 'failed');
SELECT pg_temp.tick(1010, 'ws-pb', 'pb-i');

-- `steps or []`: {} and null become [], a non-empty object is kept.
SELECT pg_temp.pb('ws-pb', 'pb-j', true, 'aud-1', 60, 1, '{}');
SELECT pg_temp.tick(1011, 'ws-pb', 'pb-j');
SELECT pg_temp.pb('ws-pb', 'pb-k', true, 'aud-1', 60, 1, 'null');
SELECT pg_temp.tick(1012, 'ws-pb', 'pb-k');
SELECT pg_temp.pb('ws-pb', 'pb-l', true, 'aud-1', 60, 2, '{"a": 1, "b": [1, 2.5, "x"]}');
SELECT pg_temp.tick(1013, 'ws-pb', 'pb-l');

-- Invalid payloads fail the attempt with Python's exception text. (Non-object
-- payloads never reach a handler: the job child rejects them first.)
SELECT pg_temp.job(1014, 'ws-pb', '{"workspace_id": "ws-pb"}');
SELECT pg_temp.job(1015, 'ws-pb', '{"playbook_id": "pb-a"}');
SELECT pg_temp.job(1018, 'ws-pb', '{"workspace_id": "", "playbook_id": "pb-a"}');
SELECT pg_temp.job(1019, 'ws-pb', '{"workspace_id": "ws-pb", "playbook_id": 0}');

-- LIKE wildcards in a playbook id: '_' matches any character, so ticking
-- "pb_w" also cancels the pending occurrence of "pbXw" (a Python quirk the
-- Go port keeps, within one workspace).
SELECT pg_temp.pb('ws-pb', 'pb_w', true, 'aud-1', 60);
SELECT pg_temp.pb('ws-pb', 'pbXw', true, 'aud-1', 60);
SELECT pg_temp.tick(1020, 'ws-pb', 'pb_w');
SELECT pg_temp.queued(1907, 'ws-pb', 'pbXw', 'playbook_schedule:pbXw:2026-06-15T15:00:00+00:00');

-- The next occurrence is already held by an active job: not enqueued twice.
SELECT pg_temp.pb('ws-pb', 'pb-dup', true, 'aud-1', 30);
SELECT pg_temp.tick(1021, 'ws-pb', 'pb-dup');
SELECT pg_temp.queued(1908, 'ws-pb', 'pb-dup', 'playbook_schedule:pb-dup:2026-06-15T12:30:00.123456+00:00', 'processing');

-- Another tenant with its own schedule, to be left alone by every step.
SELECT pg_temp.pb('ws-other', 'pb-o', true, 'aud-o', 30);
SELECT pg_temp.mirror('pb-o', 'ws-other', true);
SELECT pg_temp.queued(1909, 'ws-other', 'pb-o', 'playbook_schedule:pb-o:2026-06-15T12:30:00+00:00');
SELECT pg_temp.run('ws-other', 'run-o', 'pb-o', 'aud-o', 'pending');
