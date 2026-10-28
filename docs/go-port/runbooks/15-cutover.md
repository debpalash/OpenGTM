# Runbook: plan 15 cutover (Python-free runtime, jobs and worker)

The Go worker is the only job executor. Every job is enqueued with `executor = 'go'`, the worker registers every job
type unconditionally, and nothing is handed back to Python any more: a job the worker cannot run (no browser
service, no outreach master key, a destination type without a Go delivery port, a column it cannot plan) fails
permanently with the reason in `jobs.error`.

This runbook moves an installation that still has Python-era rows in `jobs` onto that worker. A fresh install needs
none of it: run `migrate up` and start the Go stack (`docker-compose.md`).

## What changed for operators

- **Migrations 3 and 4** (`opengtm-migrate up`, `RequiredGoSchemaVersion` = 4, always the latest migration; the Go
  binaries refuse to start below it):
  - 3 writes `app_settings.__collection_ledger_backend = 'postgres'` when the key is absent, and drops the refresh
    tenant fallback function `workbook_workspace_by_id`. Go no longer reads the marker: it always uses the Postgres
    collection ledger, so the leadgen routes and the copilot `start_collection` tool do not depend on it (step 5).
  - 4 sets the `jobs.executor` column default to `'go'`.
- **Database precondition.** `opengtm-migrate` adopts a database only at the exact Alembic head `e7b2c4d6f8a0` (the
  last revision Python's Alembic wrote); any other revision is refused. Check it first and bring an older database to
  that head with the previous (Python) release before upgrading (step 2).
- **Retired flags.** These are permanently on. The environment value is ignored, including an explicit `false`:
  - `GO_EXECUTOR_ENABLED`, `GO_EXECUTOR_WORKSPACES` (every workspace) and every `GO_EXECUTOR_*` job flag:
    `FULL_RUN`, `LEGACY_OUTREACH_ACTIONS`, `SIGNAL_SCAN`, `SOURCE_HEALTH`, `AMBITIONBOX`, `AUDIENCE_REFRESH`,
    `OUTREACH_SEND`, `OUTREACH_INBOUND`, `PLAYBOOKS`, `RETENTION`, `TRIGGER_EVAL`, `WATCH_POLL`, `COLLECT`,
    `BULK_ENRICH`, `SOURCE_WORKBOOK`.
  - `GO_EXECUTOR_COLUMN_TYPES`, `GO_RUN_NATIVE_PROVIDERS`, `GO_LOCAL_PROVIDERS` (all),
    `GO_NATIVE_AI_COLUMNS`, `GO_NATIVE_SOURCE_PROBE` and `GO_DEST_TYPES` (every type).

  - Also retired, because "off" only ever meant "Python serves this": every `GO_*_ROUTES_ENABLED`
    (`AUDIENCE`, `AUTOMATION`, `CAPABILITY`, `COLLECTION`, `CONNECTOR`, `DEST`, `GOVERNANCE`, `LLM`, `OUTREACH`,
    `PLAYBOOK`, `RUN`, `SCRAPE`), `GO_SOURCE_PREVIEW_ENABLED`, `GO_SCHEDULER_ENABLED`, `GO_COPILOT_NATIVE` and
    `COPILOT_GO_ENABLED`. They are on with or without `GO_ALL_NATIVE`, so `make run` and a bare binary serve the
    whole UI and run the scheduler.

  You can delete them from `.env`: a leftover `false` from the canary period (for example
  `GO_LLM_ROUTES_ENABLED=false`) no longer wins; every process logs one warning naming the retired flags set to false
  and then ignores them. `config.RetiredFlags()` lists them. Flags that still matter keep their own defaults:
  `GO_TRAILING_SLASH_COMPAT` (opt-in, on under `GO_ALL_NATIVE`), `GO_AI_BATCH_ENABLED` (opt-in provider batch
  mode for AI columns, off unless set), and the setting flags such as `AD_TOKENS_WORKSPACE_ONLY`,
  `CRM_IMPORT_ENABLED`, `PEOPLE_SEARCH_SOURCE_ENABLED` and `PG_LEAD_STORE`.
- **`AUTOMATIONS_ALLOW_LEGACY_OUTREACH` no longer unregisters anything.** Legacy outreach actions (`sequencer`,
  `send_email`) are Go-native, so an upgraded `.env` that kept this flag still gets every `/api/automations/*` route.
- **The worker no longer reads `PY_WORKER_URL`, `PY_WORKER_TOKEN` or `INTERNAL_API_URL`.**
  - Credentials resolve in process. `CREDENTIALS_SOURCE` must be unset or `local`, and `http` is refused at
    startup. `DATABASE_URL` and `SECRET_KEY` are required.
  - Connector manifests come from the worker's own index: the built-ins, or `OPENGTM_CONNECTOR_MANIFESTS_DIR` /
    `OPENGTM_CONNECTOR_TRUST_STORE` / `CONNECTOR_SIGNATURE_POLICY`. Set the same values on api-go and worker-go.
- **`OUTREACH_PUBLIC_BASE_URL` is required (https)** on every Go process unless `APP_ENV` is a development value,
  because outreach sending is always on. Compose with `GO_ALL_NATIVE=true` already required it.
- **Tenancy.** The tenant of a job is `jobs.workspace_id` only. These job types fail permanently when it is empty:
  `trigger_eval`, `watch_poll`, `refresh_workbook`, `audience_refresh`, `audience_destination_sync`,
  `outreach_inbound_poll` and `send`. Python wrote some of them with a NULL `workspace_id`; step 3 backfills those.
- **`run_workbook` payloads without a `go` block** are planned by the worker when it claims them, from the workbook's
  current columns. This covers Python-era rows as well as refresh, source-workbook and recompute enqueues.
- **Destination types without a Go delivery port fail permanently.** Today that is `hubspot`, `salesforce`,
  `instantly`, `smartlead`, `google_sheets` and `airtable`; webhook, slack, the ad platforms and `warehouse_http`
  are served.
- **`source_health_check` probes in process** over the browser service. Without `BROWSER_SERVICE_URL` /
  `BROWSER_SERVICE_TOKEN`, the source health, `collect`, `bulk_enrich` and collection-query source-workbook jobs
  fail with that reason.

## Steps

Before anything else, confirm the database is at the Alembic head the Go runner adopts:

```sql
SELECT version_num FROM alembic_version;   -- must be exactly e7b2c4d6f8a0
```

Anything else: stop and bring it to that head with the previous release's `alembic upgrade head` (the Go runner
refuses any other revision).

Run the SQL as the database owner (the `migrate` role): the entity tables are `FORCE ROW LEVEL SECURITY`. Take a
backup first (`pg_dump -Fc`).

### 1. Stop new Python work and drain the Python worker

1. Stop the Python `api` and `scheduler` so nothing new is enqueued for Python.
2. Leave the Python `worker` running until its in-flight jobs finish:

   ```sql
   SELECT type, count(*) FROM jobs WHERE executor = 'python' AND status = 'processing' GROUP BY type;
   ```

   Repeat until it returns no rows, then stop the Python worker.
3. A row still `processing` after the worker is gone (killed mid-job) is returned to the queue:

   ```sql
   UPDATE jobs SET status = 'pending', worker_id = NULL, locked_at = NULL, started_at = NULL,
          last_heartbeat = NULL, next_run_at = now() AT TIME ZONE 'UTC'
   WHERE executor = 'python' AND status = 'processing';
   ```

### 2. Apply the migrations

```sh
docker compose run --rm migrate          # or: opengtm-migrate up
```

### 3. Backfill the tenant of Python-era rows

Every Go job type except the global `source_health_check` takes its tenant from `jobs.workspace_id` only and fails
permanently when it is empty, and a NULL-workspace row also bypasses the per-tenant cap of the claim query. So every
pending row of a type that may lack the column needs one before step 4. Entity-bound types read the workspace from
the row their payload names (so a forged payload cannot pick a tenant); the others take the payload's own
`workspace_id` only when that workspace exists. A row whose entity is gone keeps a NULL workspace; the leftovers are
reported and then failed explicitly, so they do not sit in the queue.

```sql
BEGIN;
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
-- Every other type (send, outreach_inbound_poll, run_workbook, collect, bulk_enrich, source_workbook, signal_scan,
-- ambitionbox_import, research_playbook_run, research_playbook_schedule, retention_enforce, ...) names only its
-- workspace: accept it when that workspace exists. source_health_check is global and stays NULL.
UPDATE jobs j SET workspace_id = j.payload->>'workspace_id'
 WHERE j.workspace_id IS NULL AND j.type <> 'source_health_check' AND j.status = 'pending'
   AND EXISTS (SELECT 1 FROM workspaces ws WHERE ws.id = j.payload->>'workspace_id');
-- Report: pending rows that still have no workspace (they would fail permanently on Go):
SELECT type, count(*) FROM jobs
 WHERE workspace_id IS NULL AND status = 'pending' AND type <> 'source_health_check'
 GROUP BY type ORDER BY type;
-- Fail them now with a reason instead of leaving them in the queue (the rows stay for audit). Skip this
-- statement if the report is empty:
UPDATE jobs SET status = 'failed', completed_at = now() AT TIME ZONE 'UTC',
       error = 'cutover: no workspace_id and no entity to derive it from'
 WHERE workspace_id IS NULL AND status = 'pending' AND type <> 'source_health_check';
COMMIT;
```

### 4. Reassign the pending Python rows to Go

This replaces `cli.py jobs reassign-executor`.

```sql
UPDATE jobs SET executor = 'go' WHERE executor = 'python' AND status = 'pending';
-- Every pending type must be one the Go worker registers (anything else is failed as unsupported when claimed):
SELECT type, count(*) FROM jobs WHERE executor = 'go' AND status = 'pending' GROUP BY type ORDER BY type;
```

The Go worker registers 17 types:

- `run_workbook`, `ambitionbox_import`, `refresh_workbook`, `signal_scan`, `source_workbook`
- `trigger_eval`, `watch_poll`
- `send`, `outreach_inbound_poll`
- `retention_enforce`, `research_playbook_run`, `research_playbook_schedule`
- `audience_refresh`, `audience_destination_sync`
- `source_health_check`, `collect`, `bulk_enrich`

Python never wrote a `retry` status (its retry path sets `pending`), so `pending` is the only waiting status.
Before step 4, confirm that no Python container is running (`docker compose ps` lists no `api`, `worker` or
`scheduler` of the Python release, and `SELECT DISTINCT worker_id FROM jobs WHERE status = 'processing'` names no
Python worker). Run step 4 only after step 1 has drained the Python worker. A row reassigned while Python still runs could be
executed twice. The claim queries of both workers filter on `executor`, so neither ever takes a row of the other.

### 5. Collection ledger history

Go always uses the Postgres collection ledger; the `app_settings.__collection_ledger_backend` marker is not read, so
a missing, odd or unreadable value cannot switch the leadgen routes off. Migration 3 still writes `postgres` when
the key is absent, which is harmless.

If the install kept its collection history in the old SQLite ledger (`SELECT value FROM app_settings WHERE key =
'__collection_ledger_backend'` returns something other than `postgres`), that history is not visible to Go. Move it
before upgrading, with the previous (Python) release: run its `scripts/migrate_collection_ledger.py cutover`. The
script is not part of this repository any more. Skipping it only loses the old collection history; nothing else
depends on it.

### 6. Start the Go stack and verify

Start `api-go`, `worker-go`, `scheduler-go` and `browser` (`docker compose up -d`). Then check:

- `SELECT executor, status, count(*) FROM jobs WHERE created_at > now() - interval '1 hour' GROUP BY 1, 2;`
  should show only `go` rows.
- The worker log has no `cannot run:` refusals you do not expect. Each names its reason: browser service, master
  key, emitter, a destination port, or a `plan_failed` workbook.

## Admin password reset (replaces `cli.py reset-admin` / `reset_admin.py`)

Passwords are bcrypt (cost 12; any `$2a$`/`$2b$`/`$2y$` hash verifies). Generate a hash without Python:

```sh
docker run --rm httpd:2.4-alpine htpasswd -nbBC 12 admin 'NEW-PASSWORD' | cut -d: -f2
```

Then set it:

```sql
UPDATE users SET hashed_password = '<hash>' WHERE username = 'admin';
-- no admin at all: start api-go with an empty users table and complete first-run setup
-- (/auth/setup, setup token in the api-go log)
```

Or, with `pgcrypto` installed: `UPDATE users SET hashed_password = crypt('NEW-PASSWORD', gen_salt('bf', 12))
WHERE username = 'admin';`.

## Rollback

The Python services are gone from compose (WP-RETIRE-INFRA), so there is no executor to roll back to.

- Migrations 3 and 4 are forward-only. To undo 4: `ALTER TABLE jobs ALTER COLUMN executor SET DEFAULT 'python'`.
- Migration 3's function only served Python-era refresh jobs. Its marker insert has no undo: delete the
  `app_settings` row `__collection_ledger_backend` by hand if you must restore the old value.
- Restore the backup taken before step 1 if the cutover has to be abandoned.
