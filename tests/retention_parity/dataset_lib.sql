-- Session-local helpers that seed one workspace across the eight retention
-- target tables. Shared by the Go integration tests and the Python/Go parity
-- harness (tests/test_retention_go_parity_pg.py), so both see the same rows.
--
-- Run as the schema owner (a superuser bypasses row-level security) in the
-- same session as the calls; the function lives in pg_temp and vanishes with
-- the connection.
--
-- For every age A in the list below and each offset O in (1, 0, -1) one row is
-- written to every table, A days and O seconds older than now_ts: the default
-- retention windows (180/365 days), the custom windows the scenarios use
-- (30, 45, 90, 100, 3650) and the cutoff itself (+-1 second) are all exercised,
-- so the strict "<" boundary is visible. signals and outreach_sends also get a
-- row with a NULL created_at, which no cutoff may ever match.
CREATE OR REPLACE FUNCTION pg_temp.seed_rows(ws text, now_ts timestamp) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE
  ages int[] := ARRAY[1, 30, 45, 90, 100, 179, 180, 181, 364, 365, 366, 400, 3650];
  offs int[] := ARRAY[1, 0, -1];
  a int; o int; n int := 0; ts timestamp; tag text;
BEGIN
  INSERT INTO audiences (id, workspace_id, name, filters)
    VALUES ('aud-' || ws, ws, 'audience', '{}') ON CONFLICT DO NOTHING;
  INSERT INTO audience_destinations (id, workspace_id, audience_id, name, destination_type, config, field_map)
    VALUES ('dst-' || ws, ws, 'aud-' || ws, 'dest', 'webhook', '{}', '{}') ON CONFLICT DO NOTHING;
  INSERT INTO destination_runs (id, workspace_id, destination_id)
    VALUES ('drun-' || ws, ws, 'dst-' || ws) ON CONFLICT DO NOTHING;
  INSERT INTO research_playbooks (id, workspace_id, name, description, prompt_template, output_format,
                                  max_steps, cell_budget_usd, version)
    VALUES ('pb-' || ws, ws, 'pb', '', 't', 'text', 1, 1, 1) ON CONFLICT DO NOTHING;
  INSERT INTO playbook_runs (id, workspace_id, playbook_id, audience_id, status, prompt_version,
                             prompt_snapshot, max_members, attempted, succeeded, failed)
    VALUES ('prun-' || ws, ws, 'pb-' || ws, 'aud-' || ws, 'completed', 1, 't', 1, 0, 0, 0)
    ON CONFLICT DO NOTHING;

  FOREACH a IN ARRAY ages LOOP
    FOREACH o IN ARRAY offs LOOP
      n := n + 1;
      tag := ws || '|' || a || '|' || o;
      ts := now_ts - make_interval(days => a) - make_interval(secs => o);

      INSERT INTO governance_audit_events (id, workspace_id, actor_role, method, route, resource_path,
                                           response_status, outcome, request_id, metadata_json, created_at)
        VALUES (tag, ws, 'admin', 'POST', '/x', '/x', 200, 'success', tag, '{}', ts);
      INSERT INTO llm_usage_daily (workspace_id, provider, date, updated_at)
        VALUES (ws, 'p|' || a || '|' || o, to_char(ts::date, 'YYYY-MM-DD'), 'x');
      INSERT INTO signals (id, workspace_id, created_at)
        VALUES (tag, ws, extract(epoch FROM ts)::float8);
      INSERT INTO destination_deliveries (workspace_id, run_id, destination_id, lead_id, operation,
                                          idempotency_key, payload_fingerprint, status, attempts, created_at)
        VALUES (ws, 'drun-' || ws, 'dst-' || ws, n, 'upsert', tag, 'f', 'ok', 1, ts);
      INSERT INTO destination_inbound_receipts (id, workspace_id, destination_id, provider, external_event_id,
                                                status, conflict_policy, applied_fields, ignored_fields,
                                                payload_fingerprint, created_at)
        VALUES (tag, ws, 'dst-' || ws, 'p', tag, 'ok', 'x', '{}', '{}', 'f', ts);
      INSERT INTO audience_membership_events (workspace_id, audience_id, lead_id, event_type, snapshot, created_at)
        VALUES (ws, 'aud-' || ws, n, 'added', '{}', ts);
      INSERT INTO playbook_results (workspace_id, run_id, lead_id, status, value, result_metadata, attempts, created_at)
        VALUES (ws, 'prun-' || ws, n, 'ok', 'v', '{}', 1, ts);
      INSERT INTO outreach_sends (workspace_id, to_email, idempotency_key, created_at)
        VALUES (ws, tag || '@x.test', tag, ts);
    END LOOP;
  END LOOP;

  INSERT INTO signals (id, workspace_id, created_at) VALUES (ws || '|null', ws, NULL);
  INSERT INTO outreach_sends (workspace_id, to_email, idempotency_key, created_at)
    VALUES (ws, ws || '|null@x.test', ws || '|null', NULL);
END $$;
