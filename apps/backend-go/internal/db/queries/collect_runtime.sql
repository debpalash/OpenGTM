-- plan 15 WP-COLLECT: the Go collect / bulk_enrich jobs (port of JobRunner's PgJobLedger
-- calls, PgLeadStore.get_leads / upsert_lead / update_lead_fields and source_stats). The
-- collection tables and leads are FORCE RLS: run inside tenancy.InWorkspace; every statement
-- also filters workspace_id from the transaction-local GUC. source_stats and jobs are global.

-- name: CollectMarkRunning :execrows
-- Guarded on status = 'pending' (fix: legacy flipped a done, failed or cancelled job back to
-- running on redelivery, and overwrote a cancel that landed before the run started). A retry is
-- pending again (CJRequeueWithError / CJRetry). 0 rows = do not run.
UPDATE collection_jobs
SET status = 'running', started_at = sqlc.arg(started_at)::text, attempts = attempts + 1
WHERE id = sqlc.arg(id)::text AND workspace_id = current_setting('app.workspace_id', true) AND status = 'pending';

-- name: CollectCreateStage :one
INSERT INTO collection_job_stages (workspace_id, job_id, stage, status, started_at)
VALUES (current_setting('app.workspace_id', true), sqlc.arg(job_id)::text, sqlc.arg(stage)::text, 'running',
        sqlc.arg(started_at)::text)
RETURNING id;

-- name: CollectCompleteStage :execrows
UPDATE collection_job_stages
SET status = sqlc.arg(status)::text, input_count = sqlc.arg(input_count)::int,
    output_count = sqlc.arg(output_count)::int, rejected_count = sqlc.arg(rejected_count)::int,
    details = sqlc.arg(details)::text, completed_at = sqlc.arg(completed_at)::text
WHERE id = sqlc.arg(id)::bigint AND workspace_id = current_setting('app.workspace_id', true);

-- name: CollectCompleteJob :execrows
-- Guarded on status = 'running' (fix: PgJobLedger.complete_job overwrote a cancelled job).
UPDATE collection_jobs
SET status = 'done', completed_at = sqlc.arg(completed_at)::text, leads_found = sqlc.arg(leads_found)::int
WHERE id = sqlc.arg(id)::text AND workspace_id = current_setting('app.workspace_id', true) AND status = 'running';

-- name: CollectFailJob :execrows
-- PgJobLedger.fail_job: back to pending while attempts < max_attempts, else failed. Guarded on
-- status = 'running' (fix: legacy flipped a cancelled job back to pending).
UPDATE collection_jobs
SET status = CASE WHEN attempts < max_attempts THEN 'pending' ELSE 'failed' END,
    error = sqlc.arg(error)::text,
    completed_at = CASE WHEN attempts < max_attempts THEN completed_at ELSE sqlc.arg(completed_at)::text END
WHERE id = sqlc.arg(id)::text AND workspace_id = current_setting('app.workspace_id', true) AND status = 'running';

-- name: CollectWorkspaceCompanies :many
-- Cross-job dedup source (fix: legacy compared against get_leads(limit=10000), the top leads by score).
SELECT DISTINCT company FROM leads
WHERE workspace_id = current_setting('app.workspace_id', true) AND company IS NOT NULL AND company <> '';

-- name: CollectLeadsForBulk :many
SELECT id, company, website, email, email_confidence, phone, contact_person, city, description, linkedin_url,
       twitter_url
FROM leads
WHERE workspace_id = current_setting('app.workspace_id', true) AND id = ANY(sqlc.arg(ids)::int[]);

-- name: SourceStatsRecord :exec
-- record_run for one source, atomically (legacy read-or-create then increment raced between workers).
INSERT INTO source_stats (source, region, runs, runs_with_output, emitted, survived_dedup, validated, reliability, updated_at)
VALUES (sqlc.arg(source)::text, sqlc.arg(region)::text, 1, sqlc.arg(runs_with_output)::int, sqlc.arg(emitted)::int,
        sqlc.arg(survived_dedup)::int, sqlc.arg(validated)::int, sqlc.arg(batch_reliability)::float8,
        (now() AT TIME ZONE 'UTC'))
ON CONFLICT (source, region) DO UPDATE SET
  runs = COALESCE(source_stats.runs, 0) + 1,
  runs_with_output = COALESCE(source_stats.runs_with_output, 0) + EXCLUDED.runs_with_output,
  emitted = COALESCE(source_stats.emitted, 0) + EXCLUDED.emitted,
  survived_dedup = COALESCE(source_stats.survived_dedup, 0) + EXCLUDED.survived_dedup,
  validated = COALESCE(source_stats.validated, 0) + EXCLUDED.validated,
  reliability = CASE WHEN source_stats.reliability IS NULL THEN EXCLUDED.reliability
                     ELSE 0.3::float8 * EXCLUDED.reliability + (1.0::float8 - 0.3::float8) * source_stats.reliability END,
  updated_at = EXCLUDED.updated_at;

-- name: SourceStatsReliability :many
-- reliability_map: sources of the region with at least MIN_SAMPLES (5) runs and a reliability.
SELECT source, reliability::float8 AS reliability FROM source_stats
WHERE region = sqlc.arg(region)::text AND COALESCE(runs, 0) >= 5 AND reliability IS NOT NULL;

-- name: EnqueueJobFor :one
-- queue_service.add_job(priority=1, executor=...): the collect / bulk_enrich enqueue with the
-- executor the Go cohort flags chose.
INSERT INTO jobs (executor, type, payload, workspace_id, priority, fire_key, status,
                  created_at, next_run_at, max_retries, retry_count)
VALUES (sqlc.arg(executor)::text, sqlc.arg(job_type)::text, sqlc.arg(payload)::json,
        NULLIF(sqlc.arg(workspace_id)::text, ''), 1, NULLIF(sqlc.arg(fire_key)::text, ''), 'pending',
        (now() AT TIME ZONE 'UTC'), (now() AT TIME ZONE 'UTC'), 3, 0)
RETURNING id;
