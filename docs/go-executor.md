# Go workbook executor: rollout and rollback runbook

The Go worker (`apps/backend-go/cmd/worker`) executes `run_workbook` jobs for an
eligible subset of workbooks. Python remains the default and the fallback. Jobs
carry an `executor` column (`python` | `go`); each worker claims only its own.

## Prerequisites

- Every Python api/worker/scheduler replica runs schema revision >=
  `REQUIRED_GO_SCHEMA_REVISION` = `4a5162738495`
  (`legacy/python/apps/api/services/go_executor.py`; mirrored in the Go
  `config.RequiredGoSchemaRevision`). Run the `migrate` service first and make
  sure no old-image Python worker is still running, otherwise it may mis-handle
  lease-fenced `go` rows.
- `INTERNAL_SERVICE_TOKEN` generated: `python -c "import secrets; print(secrets.token_urlsafe(48))"`.

## Enabling

1. In `.env` set `INTERNAL_SERVICE_TOKEN=<strong random>`, `GO_EXECUTOR_ENABLED=1`
   and `GO_EXECUTOR_WORKSPACES=<ws1,ws2>` (the cohort; empty means nobody).
   Optional knobs: `GO_WORKER_CONCURRENCY`, `GO_WORKER_DB_MAX_CONNS`,
   `GO_WORKER_HEARTBEAT_SECONDS`, `GO_WORKER_SHUTDOWN_GRACE_SECONDS`,
   `GO_WORKER_DEBUG_ADDR`, `WORKER_MAX_ACTIVE_PER_WORKSPACE`.
2. Recreate the API so it picks up the flag/token: `docker compose up -d api`.
3. Start the worker: `docker compose --profile go up -d worker-go`.
   It refuses to start with an empty token or a schema older than the required revision.

Only runs enqueued after the flag is on are routed; existing queued jobs keep their executor.

## Eligibility (summary of `go_eligibility`)

A run goes to Go only if the flag is on, the workspace is in the cohort, and the
workbook has columns that are all `enrichment`/`waterfall` with: no `condition`,
no `{column}` references (prompt/formula/http_url/goal/input_columns), no
structured write-back targets (decision_makers, hiring_signals, secondary
emails/phones, technographics), email targets with `verify` off, a non-empty
provider chain of declarative providers only, target-only mappings, and no
extra manifest env vars. Provenance and automations must be off for the
workspace. Anything else stays on Python; the decision reason is logged as
`run_workbook executor=<x> reason=<r>` (e.g. `not_in_cohort`, `email_verify`,
`column_refs`, `non_declarative_provider`).

## Observability

- Debug server on `127.0.0.1:6060` inside the container (`GO_WORKER_DEBUG_ADDR`,
  not published): `/metrics` (Prometheus) and `/debug/pprof/`. Reach it with
  `docker compose exec` is not possible (distroless); use a sidecar sharing the
  network namespace or temporarily set `GO_WORKER_DEBUG_ADDR=0.0.0.0:6060` on a
  private network only.
- JSON logs on stdout with fields `job_id`, `workspace_id`, `lease_token`:
  `docker compose logs -f worker-go`.

## Rollback drill (run once on staging before widening the cohort)

1. Enable the cohort and start a 10k-row run on an eligible workbook.
2. Turn the flag off: `GO_EXECUTOR_ENABLED=0`, then `docker compose up -d api`
   (new runs go to Python).
3. Stop the Go worker: `docker compose stop worker-go`.
4. Reassign queued Go work: 
   `docker compose run --rm api python -m apps.api.cli jobs reassign-executor --from go --to python [--workspace W]`.
   Only `status='pending'` rows are changed; the count is printed. Processing rows are never touched.
5. The Python reaper recovers the abandoned `processing` claim once its lease/heartbeat is stale and finishes the run.
6. Verify no double settlement (expect 0 rows):
   ```sql
   SELECT attempt_key, count(*) FROM workbook_spend_attempts
   WHERE status='settled' GROUP BY attempt_key HAVING count(*)>1;
   ```
   and that every cell of the run is terminal.

## Known limits

- Eligible scope only (see above); everything else runs on Python.
- No lead write-back from the Go path.
- Email verification must be off for email targets.
- The spend contract is narrowed for declarative providers.
- Rollback re-enqueuing the same job id can hit `contract_conflict` on attempts written before the contract alignment.

## Security notes

- `/internal/v1/credentials` is not proxied by nginx; it is reachable only on
  `lead_net` (and the host-local API diagnostics port). Do not add an nginx route.
- `INTERNAL_SERVICE_TOKEN` is the trust boundary (constant-time compare). Treat
  it like a database password; rotate by updating `.env` and recreating api and worker-go.
- `X-Workspace-Id` on internal calls is caller-asserted; any holder of the
  token can request credentials for any workspace. Keep worker-go unpublished and the token secret.
