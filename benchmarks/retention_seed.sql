-- Bulk data for the retention_enforce benchmark (benchmarks/run.py --only retention).
--
--   SELECT pg_temp.bench_seed('<workspace>', <expired rows per table>, <kept rows per table>);
--
-- Run as the schema owner in the same session as the call (the function lives
-- in pg_temp). For each of the eight tables retention_enforce purges it writes
-- `expired` rows older than every default window (about three years) and
-- `kept` rows from yesterday, plus the parent rows the foreign keys need.
-- Default retention is 365 days (180 for activation and agent results), so a
-- run deletes exactly `expired` rows from each table and leaves `kept`.

CREATE OR REPLACE FUNCTION pg_temp.bench_seed(ws text, expired int, kept int) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE total int := expired + kept;
BEGIN
  INSERT INTO audiences (id, workspace_id, name, filters) VALUES ('aud-' || ws, ws, 'audience', '{}');
  INSERT INTO audience_destinations (id, workspace_id, audience_id, name, destination_type, config, field_map)
    VALUES ('dst-' || ws, ws, 'aud-' || ws, 'dest', 'webhook', '{}', '{}');
  INSERT INTO destination_runs (id, workspace_id, destination_id) VALUES ('drun-' || ws, ws, 'dst-' || ws);
  INSERT INTO research_playbooks (id, workspace_id, name, description, prompt_template, output_format,
                                  max_steps, cell_budget_usd, version)
    VALUES ('pb-' || ws, ws, 'pb', '', 't', 'text', 1, 1, 1);
  INSERT INTO playbook_runs (id, workspace_id, playbook_id, audience_id, status, prompt_version,
                             prompt_snapshot, max_members, attempted, succeeded, failed)
    VALUES ('prun-' || ws, ws, 'pb-' || ws, 'aud-' || ws, 'completed', 1, 't', 1, 0, 0, 0);

  -- ts(g): the first `expired` rows are ~3 years old, the rest one day old.
  INSERT INTO governance_audit_events (id, workspace_id, actor_role, method, route, resource_path,
                                       response_status, outcome, request_id, metadata_json, created_at)
    SELECT ws || '|' || g, ws, 'admin', 'POST', '/x', '/x', 200, 'success', ws || '|' || g, '{}',
           CASE WHEN g <= expired THEN LOCALTIMESTAMP - interval '1100 days' - g * interval '1 second'
                ELSE LOCALTIMESTAMP - interval '1 day' - g * interval '1 second' END
    FROM generate_series(1, total) AS g;
  INSERT INTO llm_usage_daily (workspace_id, provider, date, updated_at)
    SELECT ws, 'p' || g,
           to_char(CASE WHEN g <= expired THEN CURRENT_DATE - 1100 ELSE CURRENT_DATE - 1 END, 'YYYY-MM-DD'), 'x'
    FROM generate_series(1, total) AS g;
  INSERT INTO signals (id, workspace_id, created_at)
    SELECT ws || '|' || g, ws,
           extract(epoch FROM CASE WHEN g <= expired THEN now() - interval '1100 days' ELSE now() - interval '1 day' END)::float8
    FROM generate_series(1, total) AS g;
  INSERT INTO destination_deliveries (workspace_id, run_id, destination_id, lead_id, operation,
                                      idempotency_key, payload_fingerprint, status, attempts, created_at)
    SELECT ws, 'drun-' || ws, 'dst-' || ws, g, 'upsert', ws || '|' || g, 'f', 'ok', 1,
           CASE WHEN g <= expired THEN LOCALTIMESTAMP - interval '1100 days' ELSE LOCALTIMESTAMP - interval '1 day' END
    FROM generate_series(1, total) AS g;
  INSERT INTO destination_inbound_receipts (id, workspace_id, destination_id, provider, external_event_id, status,
                                            conflict_policy, applied_fields, ignored_fields, payload_fingerprint, created_at)
    SELECT ws || '|' || g, ws, 'dst-' || ws, 'p', ws || '|' || g, 'ok', 'x', '{}', '{}', 'f',
           CASE WHEN g <= expired THEN LOCALTIMESTAMP - interval '1100 days' ELSE LOCALTIMESTAMP - interval '1 day' END
    FROM generate_series(1, total) AS g;
  INSERT INTO audience_membership_events (workspace_id, audience_id, lead_id, event_type, snapshot, created_at)
    SELECT ws, 'aud-' || ws, g, 'added', '{}',
           CASE WHEN g <= expired THEN LOCALTIMESTAMP - interval '1100 days' ELSE LOCALTIMESTAMP - interval '1 day' END
    FROM generate_series(1, total) AS g;
  INSERT INTO playbook_results (workspace_id, run_id, lead_id, status, value, result_metadata, attempts, created_at)
    SELECT ws, 'prun-' || ws, g, 'ok', 'v', '{}', 1,
           CASE WHEN g <= expired THEN LOCALTIMESTAMP - interval '1100 days' ELSE LOCALTIMESTAMP - interval '1 day' END
    FROM generate_series(1, total) AS g;
  INSERT INTO outreach_sends (workspace_id, to_email, idempotency_key, created_at)
    SELECT ws, ws || '|' || g || '@x.test', ws || '|' || g,
           CASE WHEN g <= expired THEN LOCALTIMESTAMP - interval '1100 days' ELSE LOCALTIMESTAMP - interval '1 day' END
    FROM generate_series(1, total) AS g;

  -- Enabled default policy, a pending run with the default snapshot.
  INSERT INTO retention_policies (workspace_id, enabled, legal_hold, retention_days) VALUES (ws, true, false, '{}');
  INSERT INTO retention_runs (id, workspace_id, status, requested_by, policy_snapshot, deleted_counts)
    VALUES ('run-' || ws, ws, 'pending', '1',
            '{"audit": 365, "llm_usage": 365, "signals": 365, "activation": 180, "audience_history": 365, "agent_results": 180, "outreach_history": 365}',
            '{}');
END $$;
