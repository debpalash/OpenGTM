# opengtm server

One Go binary for the OpenGTM platform rewrite
([RFC](../../docs/plans/hybrid-platform-rewrite.md)). It runs alongside the
Python API during the migration: Go owns the routes and job types that have
moved, and proxies everything else to FastAPI.

| Command | Role |
| --- | --- |
| `opengtm serve [--worker]` | Web UI, Go-owned `/api/v2` routes, live events, and a reverse proxy to FastAPI for every other path. `--worker` also runs the queue in-process (the `lite` profile). |
| `opengtm worker` | Durable queue worker for job types routed to Go. Safe to run as many replicas. |
| `opengtm routes list\|set <type> <python\|go>` | Choose which executor claims each job type. Drain in-flight jobs before switching; a type this binary has no Go executor for is refused. |
| `opengtm leases list\|release <name>` | Show scheduler leadership (holder, fencing token, expiry) or expire a lease whose holder is gone. See [M8](../../docs/plans/m8-multihost-state.md). |
| `opengtm doctor [--json]` | Check configuration, the database role (must be `NOSUPERUSER NOBYPASSRLS`), migrations, routes (including a type routed to go that nothing can execute) and the legacy API. |
| `opengtm plugin ...` | Build, test, record, run, sign, pack and install plugins ([guide](../../docs/plugins/README.md)). |
| `opengtm health` | Probe `/healthz` (container healthchecks). |

## Run it

With the existing Compose stack running:

```bash
docker compose --profile server up -d server   # http://127.0.0.1:3080
docker compose exec server opengtm doctor
```

The image is built from the repository root with
`docker build -f apps/server/Dockerfile .`. It embeds the dashboard and the
Rust extraction kernel, bundles first-party plugins and the v1 connectors,
and runs as a non-root distroless container.

For development:

```bash
cd apps/server
go run ./cmd/opengtm serve --worker      # reads DATABASE_URL and the variables below
go test ./...                            # unit tests
OPENGTM_TEST_DATABASE_URL=postgresql://postgres:postgres@127.0.0.1:5432/opengtm_test \
  go test -race ./...                    # plus PostgreSQL integration tests (uses uv for migrations)
```

Integration tests need a disposable database: they migrate it with Alembic and
create their own `NOSUPERUSER NOBYPASSRLS` roles so row-level security is
really enforced.

## Configuration

Environment variables match the Python settings so one `.env` drives both.
An optional `opengtm.yaml` (`--config` or `OPENGTM_CONFIG`) sets the same
values; the environment wins. Invalid values stop startup with a clear error.

| Variable | Default | |
| --- | --- | --- |
| `DATABASE_URL` | required | PostgreSQL; SQLAlchemy URLs such as `postgresql+psycopg://` are accepted. Use the runtime role, never a superuser. |
| `OPENGTM_LISTEN` | `:8080` | |
| `OPENGTM_LEGACY_API_URL` | `http://127.0.0.1:8000` | FastAPI, for proxying and workspace authorization. |
| `OPENGTM_WEB_DIR` | embedded build | Serve the dashboard from a directory instead. |
| `WORKER_CONCURRENCY`, `WORKER_MAX_ACTIVE_PER_WORKSPACE`, `WORKER_SHUTDOWN_GRACE_SECONDS` | 1, 2, 30 | Same meaning and limits as the Python worker. |
| `OPENGTM_PLUGIN_DIRS`, `OPENGTM_CONNECTOR_DIRS`, `CONNECTOR_SIGNATURE_POLICY`, `OPENGTM_PLUGIN_TRUST_STORE`, `OPENGTM_EGRESS_PROXY`, `OPENGTM_PLUGIN_INDEX` (CLI only) | see the [plugin guide](../../docs/plugins/README.md#running-plugins-on-a-server) | |
| `SECRETS_MASTER_KEY`, `SECRET_KEY`, `APP_ENV`, `SECRETS_PROVIDER` | as in Python | Encryption key for per-workspace plugin secrets, resolved exactly as `apps/api/services/workspace/secrets.py` does; only `SECRETS_PROVIDER=local` is supported ([guide](../../docs/plugins/README.md#per-workspace-plugin-secrets)). |
| `AUTOMATIONS_ENABLED` | `false` | Same switch as the Python setting (`true/false`, `1/0`, `yes/no`, `on/off`; `automations.enabled` in `opengtm.yaml`). While off, ported executors enqueue no `trigger_eval` work, as in Python. Set it identically on both stacks. |
| `LOG_LEVEL` | `info` | JSON logs on stderr. |

## How it fits with the Python stack

- **One jobs table.** `job_executor_routes` decides which executor may claim
  each job type; unrouted types stay with Python. Both use the same lease
  (`worker_id`, `locked_at`), retry, backoff and cancellation semantics, and
  each reaps only its own types.
- **Authorization** is delegated to FastAPI's
  `/api/auth/workspace-context` (membership, roles, SSO enforcement) and
  cached for 30 seconds per token and workspace. A slow or sick FastAPI cannot
  wedge the Go server: each lookup has a 3 s timeout, concurrent misses for the
  same token and workspace share one call, at most 32 lookups run at once
  (the rest get `503` with `Retry-After` immediately, not a queue), and 5
  consecutive failures open a circuit breaker for 5 s (one probe then decides).
  A `401`/`403`/`404` from FastAPI is a healthy answer and never trips it;
  failures are never cached, so a revoked membership still takes effect at
  once.
- **Tenancy.** Every tenant query runs in a transaction bound with
  `set_config('app.workspace_id', ..., true)` under forced row-level security.
- **Progress** uses PostgreSQL `LISTEN/NOTIFY`, delivered on
  `/api/v2/events`. Unlike the Redis stream there is no replay buffer; a
  client that reconnects should refetch state.

## API contract

The `/api/v2` surface is documented in
[`packages/contracts/openapi.v2.yaml`](../../packages/contracts/openapi.v2.yaml)
(OpenAPI 3.1). It is a contract, not a description written afterwards:

- `internal/contract` validates real responses against it. The plugin-run
  integration harness checks every response it makes, the server tests check
  `/api/v2/version` and the SSE frames, and a source scan fails when a
  `/api/v2` route is registered without being documented. Documented
  operations that are not mounted fail too.
- The typed web client (`apps/web/src/lib/api-v2`) is generated from it with
  `bun run --cwd apps/web gen:api`; CI fails when the committed output is
  stale (`check:api`).

Adding or changing an endpoint therefore means editing the handler, the spec,
and regenerating the client in one change.

`GET /api/v2/plugin-runs` is paged with an opaque keyset cursor on
`(created_at, id)`: `?limit=` (1-200, default 50) and `?cursor=` from the
previous page's `next_cursor`, which is `null` on the last page. Runs created
while paging never shift or repeat rows of later pages.

## Layout

```text
cmd/opengtm          subcommands (one file each)
internal/config      configuration
internal/db          pgx pool, tenant transactions, test fixtures (dbtest)
internal/queue       jobs queue executor, routing, leases
internal/lease       scheduler leadership leases with fencing tokens (same protocol as the Python scheduler)
internal/authz       forward authorization to FastAPI (timeout, singleflight, bound, circuit breaker)
internal/contract    OpenAPI contract validation for /api/v2 (test support)
internal/progress    LISTEN/NOTIFY events
internal/server      HTTP front door, legacy proxy, SPA
internal/egress      guarded outbound HTTP (SSRF, DNS pinning, robots.txt, rate limits)
internal/kernels     Rust kernels (WebAssembly) for extraction and normalization
internal/plugin/...  manifests, templates, signing, bundles, plugin index, fixtures, runtimes
internal/pluginrun   plugin catalog, plugin_run jobs, workspace plugin secrets and /api/v2 plugin routes
internal/secrets     Fernet envelope shared with the Python app (enc:v1)
internal/plugin/process  Python/process plugin supervisor and wire protocol
internal/jobs/...    job types migrated from Python: retention, playbooksched, audiencerefresh;
                     jobkit (shared helpers), jobkit/jobtest and jobkit/paritytest (test fixtures)
internal/ops/...     operator tooling behind init, migrate, backup, restore, upgrade,
                     rollback and doctor --dir (see below)
```

## Process plugins (Python workers)

Manifest v2 plugins with `runtime: process` run as separate OS processes under
`internal/plugin/process`: one process per run, a bounded pool, a scrubbed
environment, resource limits, whole-tree kill on timeout, cancellation or crash,
and a versioned length-prefixed JSON protocol over an inherited unix socket (the
Python SDK is `packages/sdk-python`). They execute as ordinary `plugin_run` jobs
under the same lease-guarded result commit; permanent plugin failures end the run
at once and crashes, timeouts and upstream errors are retried by the queue.

| Variable | Default | |
| --- | --- | --- |
| `OPENGTM_PLUGIN_MAX_PROCESSES` | smaller of 4 and the CPU count | Plugin processes alive at once. |
| `OPENGTM_PLUGIN_MAX_PER_PLUGIN` | half of the above | Processes of one plugin. |
| `OPENGTM_PLUGIN_PYTHON`, `OPENGTM_PLUGIN_PYTHONPATH` | `python3` on `PATH`, none | Interpreter, and an SDK checkout to put on `PYTHONPATH`. |
| `OPENGTM_PLUGIN_STATE_DIR` | `$TMPDIR/opengtm-plugins-<uid>` | Run directories and the crash-recovery registry. |
| `OPENGTM_PLUGIN_SANDBOX`, `OPENGTM_PLUGIN_SANDBOX_RO` | `exec` | `bwrap` runs plugins in bubblewrap (no network, read-only system). |

The distroless image has no Python: build a Python-based worker image to run
process plugins. See the [Python plugin guide](../../docs/plugins/python.md) and
the [protocol](../../docs/plugins/process-abi.md).

### Operator tooling (`internal/ops`)

`opengtm init | migrate | backup | restore | upgrade | rollback` and
`doctor --dir` manage a Docker Compose install; the user guide is
[`docs/self-hosting.md`](../../docs/self-hosting.md) and the runbook
[`docs/operations/upgrade-backup-rollback.md`](../../docs/operations/upgrade-backup-rollback.md).
The packages: `execx` (one place that starts programs, so orchestration is
testable with a fake), `compose` (docker compose driver and health waiting),
`pg` (psql, pg_dump and pg_restore against a URL or inside the postgres
container), `backup` (checksummed backup directories), `migrate` (the
migration owner around Alembic), `install` (init, `.env`, state journal) and
`upgrade` (upgrade and rollback).

Their tests need PostgreSQL client tools (`psql`, `pg_dump`, `pg_restore` of
the server's major version or newer) and `OPENGTM_TEST_DATABASE_URL`; they
create and drop uniquely named databases on that server and skip otherwise.
The full flow against real containers is `scripts/packaging/e2e.sh`.

## Job types running in Go

| Job type | Owner by default | Switch |
| --- | --- | --- |
| `plugin_run` | Go (a new type; seeded by migration `7c1e5a9d3b20`) | n/a |
| `retention_enforce` | **Python** | `opengtm routes set retention_enforce go` |
| `research_playbook_schedule` | **Python** | `opengtm routes set research_playbook_schedule go` |
| `audience_refresh` | **Python** | `opengtm routes set audience_refresh go` |

Every other job type is Python's. Which types were considered, and why the rest
were not ported (workspace secrets in a local SQLite file, Python-only AI and
lead-generation engines), is in the
[triage table](../../docs/plans/job-port-triage.md).

A ported type moves **only its executor**: the handler and its failure
reconciler. The API routes that create the work, the schedulers' `bootstrap_*`
functions and every other job type it enqueues stay in Python. No route is
seeded for a ported type, so a fresh or upgraded install keeps running it in
Python; `opengtm routes list` shows it as `python (default, switchable)`, and
`opengtm worker` logs `job types available in go are still routed to python`
until you switch. A type is only registered after its Python/Go parity proof
passes (see [Parity](#parity)).

### Cut over and roll back

Cutting over is a routing change only; no migration, no deploy of Python code.
The steps are the same for every type below; each type adds its own check.

1. **Prerequisites.** The database is at Alembic `7c1e5a9d3b20` or later, and
   every Python API/worker replica is a version that honours
   `job_executor_routes` (older ones ignore routes and would claim the job
   too). `opengtm doctor` must pass, at least one Go worker must be running
   (`opengtm worker`, or `opengtm serve --worker`), and `AUTOMATIONS_ENABLED`
   must be the same on the Go worker as on the Python stack. `routes set`
   refuses a type that binary cannot execute (a route to go that nothing
   claims would strand the type's jobs); `--force` overrides.
2. **Drain.** `opengtm routes list` shows the `processing` count for the type.
   `routes set` refuses to switch while any are processing under the current
   executor; wait for 0 and run it again. Use `--force` only for a row you
   know is orphaned (for example, its worker is gone). The ceilings are the
   Python `JOB_TIMEOUTS`: 1,800 s for retention, 300 s for a playbook tick,
   900 s for an audience refresh.
3. **Switch.** `opengtm routes set <type> go`. Python workers stop claiming new
   jobs of the type immediately; the Go worker starts claiming. Pending jobs,
   including scheduled ones, are picked up by Go unchanged.
4. **Verify.** `opengtm routes list` shows `<type>  go` and `opengtm doctor`
   passes (its `go executors` check lists what runs in go), then the type's own
   check below.
5. **Roll back.** `opengtm routes set <type> python` (same drain rule). Python
   claims the type again at once; nothing needs to be redeployed or migrated,
   because the Python handler, its failure reconciler and the scheduling code
   were not changed. Any row a Go worker left `processing` is reaped by Python
   after the usual five minutes without a heartbeat, and reconciled by the
   Python reconciler, which behaves identically.

### What every Go executor does differently

These hold for all three types; the per-type sections list the rest.

- **One transaction, fenced by the lease.** Python commits a handler's work in
  several transactions and has no lease; Go does each handler's work in one
  tenant transaction (forced row-level security) and checks the job's lease
  before the commit, so a cancelled or reclaimed attempt writes nothing and a
  failure leaves no partial state. The only exceptions are a failure's own
  bookkeeping (recorded in a second transaction).
- **Error text.** Domain columns that record `str(exception)` match for
  validation and policy errors. For a PostgreSQL error the driver wording
  differs (SQLAlchemy's wrapper versus pgx's), though the database message
  inside is the same. The queue records the handler's real error in
  `jobs.error`; the Python queue records `job <id> (<type>) child exited with
  code 1` because it runs handlers in a child process.
- **Tenant scope of cleanups.** The schedule mirrors and `jobs` have no
  row-level security. Python's schedule cleanup (`remove_schedule`,
  cancelling pending occurrences) is keyed by an identifier taken from the
  payload, so a job in one tenant that names another tenant's identifier would
  delete that tenant's mirror row and cancel its occurrences. The Go
  executors only touch rows whose payload names the job's own workspace; for
  every payload the scheduler produces the result is identical.
- **Wildcards.** `fire_key LIKE '<type prefix>:<id>:%'` is not escaped, in
  Python or Go, so `_` and `%` inside an identifier behave as wildcards the
  same way in both.
- **Serial ids.** Rolled-back statements consume serial values, and how many
  depends on how a statement was batched; row order, not id gaps, is what the
  parity tests compare.

### retention_enforce

`internal/jobs/retention`, a port of `apps/api/services/governance/retention.py`.
The Go handler and failure reconciler keep the same status transitions, error
strings, legal-hold and disabled-schedule behaviour, `deleted_counts`, and
next-run scheduling (`retention:<workspace>:<date>` fire keys). Deletion runs
in batches of 5,000 rows inside one tenant transaction, so a failure still
rolls everything back, and the commit is fenced by the job lease.

**Verify** after switching: start a run from a workspace with a retention
policy (`POST /api/governance/retention/enforce`) and confirm
`GET /api/governance/retention/runs` reports it `completed` with
`deleted_counts`, the Go worker logs `retention enforced`, and the workspace's
next `retention:<workspace>:<date>` job is pending.

Its parity suite also compares `normalized_days` and the cutoff arithmetic over
a corpus of valid and malformed values (`tests/retention_parity/pure_cases.json`),
and `tests/retention_parity/dataset.sql` seeds all eight purged tables with rows
one second either side of every cutoff. For how it compares with the Python
executor on large purges, see
[`benchmarks/README.md`](../../benchmarks/README.md#retention_enforce-go-and-python).

### research_playbook_schedule

`internal/jobs/playbooksched`, a port of `handle_playbook_schedule` and
`schedule_next` in `apps/api/services/playbooks/scheduler.py`. A tick of a
playbook that runs on an audience either removes its schedule (the playbook is
missing, disabled or has no audience) or, unless a `pending`/`running` run
exists, creates a `playbook_runs` row (`requested_by='scheduler'`, a snapshot
of the playbook's prompt and `steps or []`, at most 100 members) and its
`research_playbook_run` job (executed by Python), then books the next tick
`max(15, min(interval, 10080))` minutes out under the fire key
`playbook_schedule:<id>:<isoformat>` (microseconds kept, as Python spells it),
keeping the non-RLS `playbook_schedules` mirror and `next_run_at` in step.
`bootstrap_playbook_schedules` and the playbook API stay in Python.

**Verify** after switching: configure a playbook with an audience and an
interval (`PATCH /api/research-playbooks/<id>`); after a tick
`GET /api/research-playbooks/<id>/runs` lists a `pending` run, the Go worker
logs `playbook run scheduled`, and the playbook's `next_run_at` and one
pending `research_playbook_schedule` job (fire key `playbook_schedule:<id>:...`)
exist.

Differences from Python:

- The run, its job and the next occurrence are written in one transaction
  (Python commits the run, then the job, then the next occurrence), so a failure
  cannot leave a pending run without its job.
- Python registers no failure reconciler for this type, so neither does Go.
- A payload that is not a JSON object fails with Python's `AttributeError`
  text, though the real Python child rejects it before the handler runs.
- See [every Go executor](#what-every-go-executor-does-differently).

### audience_refresh

`internal/jobs/audiencerefresh`, a port of `handle_audience_refresh`,
`reconcile_audience_refresh_failure` and `schedule_next`
(`audiences/scheduler.py`) with `refresh_audience` (`audiences/refresh.py`), the
membership-event emitter (`automations/events.py`) and `enqueue_audience_syncs`
(`destinations/engine.py`). A refresh evaluates the audience's filter against
the tenant's `leads` table (the same filters as `PgLeadStore.query_leads_page`:
`lead_ids`, `city`, `state`, `score_tier`, `status`, `source`, `company_size`,
`job_ids`, `specialization`, `has_email|phone|website`, `min_score`,
`max_score`, `search`) in pages of 500, applies the diff to `audience_members`
with `entered`/`exited` events, and updates `member_count` and `refreshed_at`.
If anything entered, exited or changed it enqueues one
`audience_destination_sync` run and job per enabled destination without an
active run (still executed by Python) and, when `AUTOMATIONS_ENABLED`, one
`trigger_eval` job per matching `on_audience_enter`/`on_audience_exit` rule and
event. It then records `refresh_health='healthy'` and books the next
occurrence (`audience_refresh:<id>:<isoformat>`). A failed refresh marks the
audience `degraded`, counts the failure, still books the next occurrence and
returns the error to the queue; the reconciler recognises that a future
`next_refresh_at` means the handler already recorded the attempt, so a failure
is not counted twice. The manual refresh API and
`bootstrap_audience_schedules` stay in Python.

**Verify** after switching: `POST /api/audiences/<id>/refresh` stays Python;
let a scheduled refresh fire (or enqueue one) and confirm
`GET /api/audiences/<id>` shows `refresh_health: healthy`, a fresh
`refreshed_at` and `next_refresh_at`, the Go worker logs `audience refreshed`,
and one pending `audience_refresh` job keyed `audience_refresh:<id>:...` exists.

Differences from Python:

- **Atomicity.** Python commits the member diff, then the events and syncs,
  then the health and the next occurrence separately, so a crash between them
  loses the fan-out of an already committed diff. Go does all of it in one
  transaction: a cancelled attempt or a failure anywhere (including the
  fan-out) rolls the diff back, records the failure on the audience, and the
  retry redoes the whole refresh.
- **Ordering of equal scores.** Python pages by `score DESC` alone, so leads
  with equal scores can move between pages and be skipped or repeated at a page
  boundary of a large audience; Go breaks ties by `id`. The parity datasets use
  distinct scores, so state is identical either way.
- **Workspace metadata.** Python skips a workspace whose slug is missing from
  `data/workspaces.db`; Go cannot read that file (see the
  [triage notes](../../docs/plans/job-port-triage.md#workspace-metadata-and-secrets-local-sqlite))
  and refreshes the audience.
- **Filter values of the wrong type** (a number for `status`, a string for
  `lead_ids`, a non-numeric `min_score`) fail the refresh in both, but the
  recorded `last_refresh_error` wording differs; Go accepts only integer
  `lead_ids` and only string `job_ids`/equality filters. `f"{value}"`
  formatting of a list or object in `specialization`/`search` (which Python
  renders as its repr) is rejected.
- **Malformed automation rules.** A rule whose stored `trigger_config` or
  `scope_workbook_ids` has an unexpected shape makes Python abandon the page of
  events (the exception is swallowed); Go does the same and logs a warning.
  Rules and workbook rows are visited in `created_at, id` / row-id order rather
  than the database's arbitrary order.
- **The reconciler's "already reconciled" test** reads the stored naive
  `next_refresh_at` as UTC, like Python, so in a PostgreSQL session zone behind
  UTC the handler's own booking looks past and the reconciler counts the
  failure again, in both implementations. Run PostgreSQL in UTC.
- **Snapshots.** A member snapshot is the 48 fields of the Python `Lead`
  dataclass (NULL stays null); a falsy `created_at`/`updated_at` is replaced by
  the refresh time's `isoformat`, so such a lead reads as `changed` on every
  refresh, as in Python.

### Parity

The ports are verified against the Python code, not just against their own
tests. One command runs every suite:

```bash
TEST_DATABASE_URL=postgresql+psycopg://postgres:postgres@127.0.0.1:55432/postgres \
  uv run pytest tests/test_retention_go_parity_pg.py \
                tests/test_playbook_schedule_go_parity_pg.py \
                tests/test_audience_refresh_go_parity_pg.py
```

`TEST_DATABASE_URL` names a server on which the role may create databases and
roles (the named database itself is not touched; the harness drops what it
creates). Each suite clones a migrated template into two fresh databases for
every one of four time-zone combinations (the PostgreSQL session zone and the
process zone, each UTC, Los Angeles or Kolkata), seeds both with its dataset,
runs the **unchanged Python handler** on one and the **Go handler** on the
other as `NOSUPERUSER NOBYPASSRLS` roles under forced RLS with the clock
frozen, and requires identical resulting state and the same error from every
step. Datasets and scenarios live in `tests/retention_parity/`,
`tests/playbook_schedule_parity/` and `tests/audience_refresh_parity/`.

Adding a type means adding a dataset, a `scenarios.json`, a small Python runner
and a Go driver; the harness is shared (`tests/parity_harness`,
`internal/jobs/jobkit/paritytest`), as are the Go helpers
(`internal/jobs/jobkit`: Python value semantics, lease-fenced tenant
transactions, schedule mirrors, fire-key cancellation, keyset paging, and
`jobtest` fixtures for the integration tests). The Go integration tests of each
package add tenant isolation, lease loss (including cancellation after the
work was written but before the commit), atomic failure, routing through the
real queue, and the reconciler through the real queue.

#### retention_enforce: known differences from the Python implementation

Everything the parity test compares is identical. These are not, by design or
because they cannot match:

- **Atomicity.** Python commits the deletion and completion, then schedules the
  next run in a second commit; if the second fails the run stays `completed`
  with no next run until `bootstrap_retention_schedules` notices. Go does
  deletion, completion and scheduling in one transaction, so a scheduling
  failure rolls back the purge and the retry redoes it. The legal-hold and
  disabled-schedule cancellation is likewise one transaction.
- **Lease fencing.** Python's handler has no lease; a timed-out child is killed.
  Go checks the lease before the final commit: a cancelled or reclaimed
  attempt rolls the purge back and writes nothing. The `running` marker written
  before the purge remains, as it does in Python.
- **Database error wording.** A failure inside the purge records
  `str(exception)` in `retention_runs.error` (truncated to 1,000 characters).
  For validation and policy errors the text is identical (`KeyError`,
  `TypeError`, `OverflowError` messages included). For a PostgreSQL error the
  driver wording differs (SQLAlchemy's wrapper versus pgx's), though the
  database message inside it is the same.
- **`signals` cutoff and the process time zone.** Python computes that cutoff
  with `naive_datetime.timestamp()`, which reads the wall clock in the
  *process* zone, while every timestamp is written in the *database* session
  zone. Go reproduces this exactly (the parity test covers zones that
  disagree), so on a host whose zone is not UTC the `signals` window is offset
  by the zone's UTC offset in both implementations. Run workers in UTC.
- **`jobs.workspace_id`.** `enqueue_job_once` leaves it `NULL` for the next
  scheduled run, so those jobs bypass the per-workspace active-job cap. Go
  matches that rather than silently changing scheduling fairness.
- **Malformed `retention_days`.** Values the API can never store are handled
  in Python by whatever exception `int()` raises. Go matches the messages for
  numbers, booleans, strings (ASCII digits, underscores and surrounding
  whitespace as `int()` accepts), `null`, arrays and objects. Non-ASCII Unicode
  digits in a string are rejected, and a non-empty value that is not a JSON
  object gets one fixed message instead of Python's type-specific
  `AttributeError`.
- **Reconciler race.** If the policy row disappears between the purge and
  rescheduling, Python raises `ObjectDeletedError`; Go skips scheduling.
