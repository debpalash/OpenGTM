-- The audience_refresh parity dataset. Loaded as the schema owner into two
-- identical fresh databases; scenarios.json then drives the Python handler on
-- one and the Go handler on the other.
--
-- "Now" is the frozen instant 2026-06-15 12:00:00.250000 UTC. Each audience
-- below isolates one behaviour; see scenarios.json for the steps using it.
-- Lead scores are distinct inside a workspace so the order of a page (score
-- DESC), and with it every serial id, is the same on both sides.

CREATE FUNCTION pg_temp.aud(id text, filters json DEFAULT '{}', en boolean DEFAULT true, mins int DEFAULT 30,
                            ws text DEFAULT 'ws-ar') RETURNS void LANGUAGE sql AS $$
  INSERT INTO audiences (id, workspace_id, name, filters, refresh_enabled, refresh_interval_minutes)
  VALUES (id, ws, 'name-' || id, filters, en, mins) $$;

-- A claimed (processing, leased) job by default; a queued one when st = 'pending'.
CREATE FUNCTION pg_temp.job(jid int, payload json, st text DEFAULT 'processing', fk text DEFAULT NULL,
                            typ text DEFAULT 'audience_refresh', nxt timestamp DEFAULT TIMESTAMP '2026-06-15 11:59:00')
RETURNS void LANGUAGE sql AS $$
  INSERT INTO jobs (id, type, payload, workspace_id, priority, status, created_at, started_at, next_run_at,
                    max_retries, retry_count, worker_id, locked_at, last_heartbeat, fire_key)
  VALUES (jid, typ, payload, CASE WHEN fk IS NULL THEN 'ws-ar' END, 1, st, TIMESTAMP '2026-06-15 11:59:00',
          CASE WHEN st = 'processing' THEN TIMESTAMP '2026-06-15 11:59:30' END, nxt,
          3, 0,
          CASE WHEN st = 'processing' THEN 'parity-worker' END,
          CASE WHEN st = 'processing' THEN TIMESTAMP '2026-06-15 11:59:30' END,
          CASE WHEN st = 'processing' THEN TIMESTAMP '2026-06-15 11:59:30' END, fk) $$;

CREATE FUNCTION pg_temp.tick(jid int, aud text, ws text DEFAULT 'ws-ar') RETURNS void LANGUAGE sql AS $$
  SELECT pg_temp.job(jid, json_build_object('workspace_id', ws, 'audience_id', aud)) $$;

CREATE FUNCTION pg_temp.occurrence(jid int, aud text, fk text, st text DEFAULT 'pending') RETURNS void LANGUAGE sql AS $$
  SELECT pg_temp.job(jid, json_build_object('workspace_id', 'ws-ar', 'audience_id', aud), st, fk) $$;

CREATE FUNCTION pg_temp.sched(aud text, en boolean, nxt timestamp, ws text DEFAULT 'ws-ar') RETURNS void LANGUAGE sql AS $$
  INSERT INTO audience_schedules (audience_id, workspace_id, enabled, next_refresh_at) VALUES (aud, ws, en, nxt) $$;

CREATE FUNCTION pg_temp.member(aud text, lead int, snap json, tok text DEFAULT NULL) RETURNS void LANGUAGE sql AS $$
  INSERT INTO audience_members (workspace_id, audience_id, lead_id, snapshot, refresh_token, joined_at, last_seen_at)
  VALUES ('ws-ar', aud, lead, snap, tok, TIMESTAMP '2026-06-01 00:00:00', TIMESTAMP '2026-06-01 00:00:00') $$;

-- ── leads ───────────────────────────────────────────────────────────────────
-- ws-ar: 34 leads with distinct scores (100 - 3*i), spread over cities, states,
-- tiers, statuses, sources, jobs, sizes, specializations and contact details.
INSERT INTO leads (id, workspace_id, company, website, email, phone, city, state, specialization, company_size,
                   employee_count_exact, description, industry_tags, source, collection_job_id, score,
                   score_tier, status, notes, created_at, updated_at, last_enriched_at, enrichment_attempts,
                   email_confidence, contact_person, address)
SELECT i, 'ws-ar', 'Co ' || i,
       CASE WHEN i % 4 = 0 THEN NULL ELSE 'https://co' || i || '.example' END,
       CASE WHEN i % 2 = 1 THEN 'a' || i || '@x.example' ELSE '' END,
       CASE WHEN i % 3 = 0 THEN '555-' || i END,
       (ARRAY['Bangalore', 'Mumbai', 'Delhi'])[i % 3 + 1],
       CASE WHEN i % 2 = 1 THEN 'KA' ELSE 'MH' END,
       CASE WHEN i % 2 = 1 THEN 'IT Staffing' ELSE 'Executive Search' END,
       (ARRAY['1-50', '51-200', '201-500'])[i % 3 + 1],
       CASE WHEN i % 7 = 0 THEN NULL ELSE i * 10 END,
       'Staffing firm number ' || i,
       'staffing, rpo',
       CASE WHEN i % 4 = 0 THEN 'job:J1' WHEN i % 4 = 1 THEN 'csv' ELSE 'maps' END,
       CASE WHEN i % 5 = 0 THEN 'J2' ELSE '' END,
       100 - 3 * i,
       CASE WHEN 100 - 3 * i >= 80 THEN 'hot' WHEN 100 - 3 * i >= 50 THEN 'warm' ELSE 'cold' END,
       (ARRAY['new', 'contacted', 'qualified'])[i % 3 + 1],
       '', '2026-01-01T00:00:00+00:00', '2026-01-02T00:00:00+00:00', '', i % 4,
       CASE WHEN i % 2 = 1 THEN 'pattern' ELSE '' END, 'Person ' || i, 'Street ' || i
FROM generate_series(1, 34) AS i;
-- specials: non-ASCII text, a lead whose created_at/updated_at are falsy (the
-- Lead dataclass substitutes the construction time), NULL text columns.
UPDATE leads SET notes = 'café ☕ naïve', company = 'Café Ünïcode' WHERE id = 5 AND workspace_id = 'ws-ar';
UPDATE leads SET created_at = '', updated_at = NULL WHERE id = 3 AND workspace_id = 'ws-ar';
UPDATE leads SET created_at = NULL, updated_at = '' WHERE id = 8 AND workspace_id = 'ws-ar';
UPDATE leads SET technographics = NULL, field_provenance = NULL, decision_makers = '{"a": 1}',
                 glassdoor_rating = '4.5' WHERE id = 2 AND workspace_id = 'ws-ar';
UPDATE leads SET description = NULL, city = NULL, status = NULL WHERE id = 11 AND workspace_id = 'ws-ar';

-- another tenant: never touched by a ws-ar job
INSERT INTO leads (id, workspace_id, company, city, score, score_tier, status, source, collection_job_id, created_at, updated_at)
SELECT 100 + i, 'ws-other', 'Other ' || i, 'Bangalore', 90 - i, 'hot', 'new', 'csv', '', '2026-01-01T00:00:00+00:00', '2026-01-01T00:00:00+00:00'
FROM generate_series(1, 3) AS i;
SELECT pg_temp.aud('a-other', '{}', true, 30, 'ws-other');

-- ── filters ─────────────────────────────────────────────────────────────────
SELECT pg_temp.aud('a-all');                                                     -- no filters; refreshed twice
SELECT pg_temp.aud('a-city', '{"city": "Bangalore"}');
SELECT pg_temp.aud('a-combo', '{"state": "KA", "score_tier": "hot", "status": "new"}');
SELECT pg_temp.aud('a-jobs', '{"job_ids": ["J1", "J2"]}');                       -- collection_job_id or source = job:<id>
SELECT pg_temp.aud('a-spec', '{"specialization": "staff"}');                     -- ILIKE %staff%
SELECT pg_temp.aud('a-email-yes', '{"has_email": true, "has_website": true}');
SELECT pg_temp.aud('a-email-no', '{"has_email": false, "has_phone": true}');
SELECT pg_temp.aud('a-nophone', '{"has_phone": false}');
SELECT pg_temp.aud('a-score', '{"min_score": 91, "max_score": 97}');            -- both bounds are inclusive (leads 3 and 1)
SELECT pg_temp.aud('a-score-zero', '{"min_score": 0, "max_score": 100}');
SELECT pg_temp.aud('a-search', '{"search": "Co 1"}');                            -- tsvector OR ILIKE
SELECT pg_temp.aud('a-search-utf', '{"search": "naïve"}');
SELECT pg_temp.aud('a-ids', '{"lead_ids": [1, 2, 3, 99]}');
SELECT pg_temp.aud('a-ids-empty', '{"lead_ids": []}');
SELECT pg_temp.aud('a-ids-null', '{"lead_ids": null, "city": "Delhi"}');
SELECT pg_temp.aud('a-falsy', '{"city": "", "status": null, "job_ids": [], "has_email": null, "unknown": 5}');
SELECT pg_temp.aud('a-size', '{"company_size": "51-200", "source": "csv"}');
SELECT pg_temp.aud('a-nullfilters', 'null');                                     -- `filters or {}`

-- ── page size 4: many pages, many exits ─────────────────────────────────────
SELECT pg_temp.aud('a-shrink', '{}');

-- ── stale members: changed snapshot, exits (NULL/old token, falsy snapshots) ──
SELECT pg_temp.aud('a-members', '{"lead_ids": [1, 2, 3]}');
SELECT pg_temp.member('a-members', 2, '{"id": 2, "company": "stale"}', 'old');
SELECT pg_temp.member('a-members', 77, '{"id": 77, "company": "gone"}');
SELECT pg_temp.member('a-members', 78, '{}', 'old');
SELECT pg_temp.member('a-members', 79, 'null', 'old');

-- ── destinations: sync runs only for enabled destinations without an active run
SELECT pg_temp.aud('a-dest', '{"lead_ids": [1, 2]}');
INSERT INTO audience_destinations (id, workspace_id, audience_id, name, destination_type, enabled, config, field_map) VALUES
  ('d-ok',        'ws-ar', 'a-dest', 'ok',        'webhook', true,  '{}', '{}'),
  ('d-active',    'ws-ar', 'a-dest', 'active',    'webhook', true,  '{}', '{}'),
  ('d-off',       'ws-ar', 'a-dest', 'off',       'webhook', false, '{}', '{}'),
  ('d-done',      'ws-ar', 'a-dest', 'done',      'webhook', true,  '{}', '{}'),
  ('d-cancelling','ws-ar', 'a-dest', 'cancelling','webhook', true,  '{}', '{}');
INSERT INTO destination_runs (id, workspace_id, destination_id, status) VALUES
  ('r-active', 'ws-ar', 'd-active', 'running'), ('r-done', 'ws-ar', 'd-done', 'completed'),
  ('r-canc', 'ws-ar', 'd-cancelling', 'cancelling');
SELECT pg_temp.aud('a-dest-noop', '{"lead_ids": [1]}');                          -- unchanged refresh: no sync
INSERT INTO audience_destinations (id, workspace_id, audience_id, name, destination_type, enabled, config, field_map)
  VALUES ('d-noop', 'ws-ar', 'a-dest-noop', 'noop', 'webhook', true, '{}', '{}');

-- ── automations: membership events fire trigger_eval jobs (when enabled) ────
SELECT pg_temp.aud('a-auto', '{"lead_ids": [1, 2, 3]}');
SELECT pg_temp.aud('a-auto-off', '{"lead_ids": [1, 2, 3]}');
INSERT INTO workbooks (id, name, workspace_id) VALUES ('wb1', 'one', 'ws-ar'), ('wb2', 'two', 'ws-ar'), ('wbo', 'other', 'ws-other');
INSERT INTO workbook_rows (id, workbook_id, workspace_id, lead_id, data, enrichments, position) VALUES
  (11, 'wb1', 'ws-ar', 1, '{}', '{}', 0), (12, 'wb2', 'ws-ar', 1, '{}', '{}', 0), (13, 'wb2', 'ws-ar', 2, '{}', '{}', 1),
  (14, 'wbo', 'ws-other', 3, '{}', '{}', 0);                                      -- lead 3: only another tenant's row
INSERT INTO triggers (id, workspace_id, name, enabled, trigger_type, trigger_config, actions, scope_workbook_ids, created_at) VALUES
  ('t-enter-any',    'ws-ar', 'enter any',    true,  'on_audience_enter', '{}',                              '[]', '[]',       TIMESTAMP '2026-01-01 00:00:01'),
  ('t-enter-this',   'ws-ar', 'enter this',   true,  'on_audience_enter', '{"audience_ids": ["a-auto"]}',    '[]', '["wb2"]',  TIMESTAMP '2026-01-01 00:00:02'),
  ('t-enter-other',  'ws-ar', 'enter other',  true,  'on_audience_enter', '{"audience_ids": ["a-else"]}',    '[]', '[]',       TIMESTAMP '2026-01-01 00:00:03'),
  ('t-enter-off',    'ws-ar', 'enter off',    false, 'on_audience_enter', '{}',                              '[]', '[]',       TIMESTAMP '2026-01-01 00:00:04'),
  ('t-exit',         'ws-ar', 'exit',         true,  'on_audience_exit',  '{"audience_ids": ["a-auto"]}',    '[]', NULL,       TIMESTAMP '2026-01-01 00:00:05'),
  ('t-row-added',    'ws-ar', 'wrong type',   true,  'on_row_added',      '{}',                              '[]', '[]',       TIMESTAMP '2026-01-01 00:00:06'),
  ('t-enter-ws-o',   'ws-other', 'other ws',  true,  'on_audience_enter', '{}',                              '[]', '[]',       TIMESTAMP '2026-01-01 00:00:07');

-- ── failure: a database error mid-refresh rolls everything back ─────────────
SELECT pg_temp.aud('a-fail', '{"city": "Delhi"}');
CREATE FUNCTION parity_boom() RETURNS trigger LANGUAGE plpgsql AS $$
  BEGIN IF NEW.audience_id = 'a-fail' THEN RAISE EXCEPTION 'boom'; END IF; RETURN NEW; END $$;
CREATE TRIGGER audience_members_boom BEFORE INSERT ON audience_members FOR EACH ROW EXECUTE FUNCTION parity_boom();
SELECT pg_temp.aud('a-bad-ids', '{"lead_ids": "abc"}');
SELECT pg_temp.aud('a-bad-status', '{"status": 5}');
SELECT pg_temp.aud('a-bad-min', '{"min_score": "x"}');
SELECT pg_temp.aud('a-bad-shape', '[1]');

-- ── scheduling ──────────────────────────────────────────────────────────────
SELECT pg_temp.aud('a-i5', '{"lead_ids": [1]}', true, 5);                        -- clamped to 15
SELECT pg_temp.aud('a-i0', '{"lead_ids": [1]}', true, 0);                        -- 0 -> 60
SELECT pg_temp.aud('a-ibig', '{"lead_ids": [1]}', true, 20000);                  -- clamped to 10080
SELECT pg_temp.aud('a-disabled', '{"lead_ids": [1]}', false, 30);                -- schedule removed, nothing refreshed
SELECT pg_temp.sched('a-disabled', true, TIMESTAMP '2026-06-15 11:00:00');
SELECT pg_temp.occurrence(1901, 'a-disabled', 'audience_refresh:a-disabled:2026-06-15T12:30:00+00:00');
SELECT pg_temp.occurrence(1902, 'a-disabled', 'audience_refresh:a-disabled:2026-06-15T13:30:00+00:00', 'processing');
SELECT pg_temp.sched('a-ghost', true, TIMESTAMP '2026-06-15 11:00:00');          -- no such audience
SELECT pg_temp.occurrence(1903, 'a-ghost', 'audience_refresh:a-ghost:2026-06-15T12:30:00+00:00');
-- the next occurrence is already held by an active job
SELECT pg_temp.aud('a-dup', '{"lead_ids": [1]}', true, 30);
SELECT pg_temp.occurrence(1904, 'a-dup', 'audience_refresh:a-dup:2026-06-15T12:30:00.250000+00:00', 'processing');
-- LIKE wildcard: ticking "a_w" also cancels the pending occurrence of "aXw"
SELECT pg_temp.aud('a_w', '{"lead_ids": [1]}');
SELECT pg_temp.aud('aXw', '{"lead_ids": [1]}');
SELECT pg_temp.occurrence(1905, 'aXw', 'audience_refresh:aXw:2026-06-15T15:00:00+00:00');
-- stale mirror and pending/processing/completed occurrences of a refreshed audience
SELECT pg_temp.sched('a-all', true, TIMESTAMP '2026-06-15 11:00:00');
SELECT pg_temp.occurrence(1906, 'a-all', 'audience_refresh:a-all:2026-06-15T11:30:00+00:00');
SELECT pg_temp.occurrence(1907, 'a-all', 'audience_refresh:a-all:2026-06-15T11:45:00+00:00', 'processing');
SELECT pg_temp.occurrence(1908, 'a-all', 'audience_refresh:a-all:2026-06-15T10:00:00+00:00', 'completed');
-- another tenant's occurrence for the same audience-id shape
SELECT pg_temp.job(1909, '{"workspace_id": "ws-other", "audience_id": "a-other"}', 'pending',
                   'audience_refresh:a-other:2026-06-15T12:30:00+00:00');
SELECT pg_temp.sched('a-other', true, TIMESTAMP '2026-06-15 12:30:00', 'ws-other');

-- ── failure reconciliation ──────────────────────────────────────────────────
-- (job ids 3001.. are the failed jobs; their next_run_at is the queue's retry time)
CREATE FUNCTION pg_temp.rec(aud text, nxt timestamp, jid int, fails int DEFAULT 0, en boolean DEFAULT true,
                            retry_at timestamp DEFAULT TIMESTAMP '2026-06-15 12:02:00') RETURNS void LANGUAGE sql AS $$
  INSERT INTO audiences (id, workspace_id, name, filters, refresh_enabled, refresh_interval_minutes, next_refresh_at,
                         consecutive_refresh_failures, refresh_health)
  VALUES (aud, 'ws-ar', 'name-' || aud, '{"lead_ids": [1]}', en, 45, nxt, fails, 'healthy');
  SELECT pg_temp.job(jid, json_build_object('workspace_id', 'ws-ar', 'audience_id', aud), 'pending', NULL,
                     'audience_refresh', retry_at) $$;
SELECT pg_temp.rec('a-rec1', TIMESTAMP '2026-06-15 11:00:00', 3001);             -- past: retry mirrors the job's next run
SELECT pg_temp.rec('a-rec2', TIMESTAMP '2026-06-15 11:00:00', 3002, 2);          -- past: final failure reschedules
SELECT pg_temp.rec('a-rec3', TIMESTAMP '2026-06-15 13:00:00', 3003);             -- future: already reconciled
SELECT pg_temp.rec('a-rec3b', TIMESTAMP '2026-06-15 13:00:00', 3013);
SELECT pg_temp.rec('a-rec4', NULL, 3004);                                        -- never scheduled
SELECT pg_temp.rec('a-rec5', TIMESTAMP '2026-06-15 11:00:00', 3005, 0, false);   -- disabled: left alone
SELECT pg_temp.rec('a-rec7', TIMESTAMP '2026-06-15 11:00:00', 3007);             -- 1200-char error
SELECT pg_temp.rec('a-rec8', TIMESTAMP '2026-06-15 11:00:00', 3008, 0, true, NULL); -- the job has no next_run_at
SELECT pg_temp.rec('a-rec9', TIMESTAMP '2026-06-15 11:00:00', 3009, 3);          -- empty error text
SELECT pg_temp.rec('a-rec10', TIMESTAMP '2026-06-15 11:00:00', 3010, 0);         -- stripped ids
SELECT pg_temp.rec('a-rec11', TIMESTAMP '2026-06-15 12:00:00.250000', 3011);     -- next == now: not in the future
SELECT pg_temp.job(3006, '{"workspace_id": "ws-ar"}', 'pending');                -- no audience id
SELECT pg_temp.job(3012, '{"workspace_id": "ws-ar", "audience_id": "a-nobody"}', 'pending');
SELECT pg_temp.job(3014, json_build_object('workspace_id', ' ws-ar ', 'audience_id', ' a-rec10 '), 'pending');

-- ── invalid payloads (a handler returns quietly for missing ids) ────────────
SELECT pg_temp.job(1010, '{"workspace_id": "ws-ar"}');
SELECT pg_temp.job(1011, '{"audience_id": "a-all"}');
SELECT pg_temp.job(1012, '{"workspace_id": "ws-ar", "audience_id": "a-nobody"}');
SELECT pg_temp.job(1013, '{"workspace_id": "ws-ar", "audience_id": 0}');

-- ── ticks for the refresh scenarios ─────────────────────────────────────────
SELECT pg_temp.tick(1001, 'a-all');
SELECT pg_temp.tick(1002, 'a-city');
SELECT pg_temp.tick(1003, 'a-combo');
SELECT pg_temp.tick(1004, 'a-jobs');
SELECT pg_temp.tick(1005, 'a-spec');
SELECT pg_temp.tick(1006, 'a-email-yes');
SELECT pg_temp.tick(1007, 'a-email-no');
SELECT pg_temp.tick(1008, 'a-nophone');
SELECT pg_temp.tick(1009, 'a-score');
SELECT pg_temp.tick(1014, 'a-score-zero');
SELECT pg_temp.tick(1015, 'a-search');
SELECT pg_temp.tick(1016, 'a-search-utf');
SELECT pg_temp.tick(1017, 'a-ids');
SELECT pg_temp.tick(1018, 'a-ids-empty');
SELECT pg_temp.tick(1019, 'a-ids-null');
SELECT pg_temp.tick(1020, 'a-falsy');
SELECT pg_temp.tick(1021, 'a-size');
SELECT pg_temp.tick(1022, 'a-nullfilters');
SELECT pg_temp.tick(1023, 'a-shrink');
SELECT pg_temp.tick(1024, 'a-members');
SELECT pg_temp.tick(1025, 'a-dest');
SELECT pg_temp.tick(1026, 'a-dest-noop');
SELECT pg_temp.tick(1027, 'a-auto');
SELECT pg_temp.tick(1028, 'a-auto-off');
SELECT pg_temp.tick(1029, 'a-fail');
SELECT pg_temp.tick(1030, 'a-bad-ids');
SELECT pg_temp.tick(1031, 'a-bad-status');
SELECT pg_temp.tick(1032, 'a-bad-min');
SELECT pg_temp.tick(1033, 'a-bad-shape');
SELECT pg_temp.tick(1034, 'a-i5');
SELECT pg_temp.tick(1035, 'a-i0');
SELECT pg_temp.tick(1036, 'a-ibig');
SELECT pg_temp.tick(1037, 'a-disabled');
SELECT pg_temp.tick(1038, 'a-ghost');
SELECT pg_temp.tick(1039, 'a-dup');
SELECT pg_temp.tick(1040, 'a_w');
