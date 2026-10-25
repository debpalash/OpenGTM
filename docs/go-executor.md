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
   `GO_EXECUTOR_ENABLED`/`GO_EXECUTOR_WORKSPACES` are read only at enqueue time
   (`enqueue_run_workbook`, called from the API, `seed`, and the source engine
   inside the Python worker), so restart `api` and, if you want source-engine
   enqueues from `worker` to follow the change, `worker` too:
   `docker compose up -d api worker`. The scheduler does not read the flag.
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

- The debug server (`/metrics`, `/debug/pprof/`) listens on `127.0.0.1:6060`
  inside the container. `GO_WORKER_DEBUG_ADDR` must be a loopback IP; the
  worker refuses to start otherwise, so it can never be exposed on the network.
  The image is distroless (no shell), so reach it with a sidecar sharing the
  network namespace:
  `docker run --rm --network container:$(docker compose ps -q worker-go) curlimages/curl -s http://127.0.0.1:6060/metrics`
  (or a compose sidecar with `network_mode: "service:worker-go"`).
- JSON logs on stdout with fields `job_id`, `workspace_id`, `lease_token`:
  `docker compose logs -f worker-go`.

## Rollback drill (run once on staging before widening the cohort)

Python only claims `executor='python'` rows, and the reaper requeues stale
`processing` jobs as `pending` while keeping `executor='go'`. So
`reassign-executor` moves only rows that are already `pending`; **processing
rows are not moved until the reaper requeues them**, and the command must be
repeated afterwards.

1. Enable the cohort and start a 10k-row run on an eligible workbook. Record
   the baseline before the run: `SELECT budget_spent_usd FROM workbooks WHERE id = :wb;`
2. Turn the flag off: `GO_EXECUTOR_ENABLED=0` in `.env`, then `docker compose up -d api`
   (new runs go to Python).
3. Stop the Go worker: `docker compose stop worker-go`.
4. Move the already-pending Go rows:
   `docker compose run --rm api python -m apps.api.cli jobs reassign-executor --from go --to python [--workspace W]`
   (prints the count; changes only `status='pending'` rows).
5. Wait for the Python reaper: a `processing` job becomes stale after a 5 minute
   heartbeat threshold, then is requeued with backoff `2^retry_count` minutes.
6. Re-run the step 4 command until this returns 0 (as DB owner):
   `SELECT count(*) FROM jobs WHERE executor='go' AND status IN ('pending','processing');`
7. Confirm the run finishes on Python (job `completed`, cells terminal).

Verification SQL, run as the DB owner role (the runtime role is subject to
forced RLS and would see nothing), with `:wb` = the drill workbook id:

```sql
-- (a) must return 0 rows. 'uncertain' attempts are unresolved spend: list them
--     and reconcile manually; do not ignore them.
SELECT attempt_key, status, provider, reserved_microusd
FROM workbook_spend_attempts
WHERE workbook_id = :wb AND status IN ('reserved','dispatched','uncertain');

-- (b) spent_delta must equal settled_usd (baseline from step 1).
SELECT w.budget_spent_usd - :baseline AS spent_delta,
       (SELECT coalesce(sum(settled_microusd),0)/1e6
          FROM workbook_spend_attempts a
         WHERE a.workbook_id = w.id AND a.status = 'settled') AS settled_usd
FROM workbooks w WHERE w.id = :wb;

-- (c) must return 0 rows: every cell is terminal.
SELECT status, count(*) FROM workbook_enrichments
WHERE workbook_id = :wb AND status NOT IN ('complete','error','skipped')
GROUP BY status;
```

(`settled_usd` is only comparable with the budget delta if the baseline was
taken when no other run was spending on that workbook.)

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
