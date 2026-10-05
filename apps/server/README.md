# opengtm server

One Go binary for the OpenGTM platform rewrite
([RFC](../../docs/plans/hybrid-platform-rewrite.md)). It runs alongside the
Python API during the migration: Go owns the routes and job types that have
moved, and proxies everything else to FastAPI.

| Command | Role |
| --- | --- |
| `opengtm serve [--worker]` | Web UI, Go-owned `/api/v2` routes, live events, and a reverse proxy to FastAPI for every other path. `--worker` also runs the queue in-process (the `lite` profile). |
| `opengtm worker` | Durable queue worker for job types routed to Go. Safe to run as many replicas. |
| `opengtm routes list\|set <type> <python\|go>` | Choose which executor claims each job type. Drain in-flight jobs before switching. |
| `opengtm doctor [--json]` | Check configuration, the database role (must be `NOSUPERUSER NOBYPASSRLS`), migrations, routes and the legacy API. |
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
| `OPENGTM_PLUGIN_DIRS`, `OPENGTM_CONNECTOR_DIRS`, `CONNECTOR_SIGNATURE_POLICY`, `OPENGTM_PLUGIN_TRUST_STORE`, `OPENGTM_EGRESS_PROXY` | see the [plugin guide](../../docs/plugins/README.md#running-plugins-on-a-server) | |
| `OPENGTM_ENRICH_DOMAIN_RPS` | 1000 | Per-registrable-domain request rate for connector enrichment runs (Python applies no limit). |
| `LOG_LEVEL` | `info` | JSON logs on stderr. |

## How it fits with the Python stack

- **One jobs table.** `job_executor_routes` decides which executor may claim
  each job type; unrouted types stay with Python. Both use the same lease
  (`worker_id`, `locked_at`), retry, backoff and cancellation semantics, and
  each reaps only its own types.
- **Authorization** is delegated to FastAPI's
  `/api/auth/workspace-context` (membership, roles, SSO enforcement) and
  cached for 30 seconds per token and workspace.
- **Tenancy.** Every tenant query runs in a transaction bound with
  `set_config('app.workspace_id', ..., true)` under forced row-level security.
- **Progress** uses PostgreSQL `LISTEN/NOTIFY`, delivered on
  `/api/v2/events`. Unlike the Redis stream there is no replay buffer; a
  client that reconnects should refetch state.

## Layout

```text
cmd/opengtm          subcommands (one file each)
internal/config      configuration
internal/db          pgx pool, tenant transactions, test fixtures (dbtest)
internal/queue       jobs queue executor, routing, leases
internal/authz       forward authorization to FastAPI
internal/progress    LISTEN/NOTIFY events
internal/server      HTTP front door, legacy proxy, SPA
internal/egress      guarded outbound HTTP (SSRF, DNS pinning, robots.txt, rate limits)
internal/kernels     Rust kernels (WebAssembly) for extraction and normalization
internal/plugin/...  manifests, templates, signing, bundles, fixtures, runtimes
internal/pluginrun   plugin catalog, plugin_run jobs and /api/v2 plugin routes
internal/jobs/...    job types migrated from Python (retention_enforce, run_workbook_connector)
```

## Job types running in Go

| Job type | Owner by default | Switch |
| --- | --- | --- |
| `plugin_run` | Go (a new type; seeded by migration `7c1e5a9d3b20`) | n/a |
| `retention_enforce` | **Python** | `opengtm routes set retention_enforce go` |
| `run_workbook_connector` | **Python** | `opengtm routes set run_workbook_connector go` ([details](#connector-enrichment-runs-in-go)) |

`retention_enforce` is the first job type migrated from Python
(`internal/jobs/retention`, a port of
`apps/api/services/governance/retention.py`). Only the executor moved: the
API routes that create runs and enqueue jobs, and the scheduler's
`bootstrap_retention_schedules`, stay in Python. The Go handler and failure
reconciler keep the same status transitions, error strings, legal-hold and
disabled-schedule behaviour, `deleted_counts`, and next-run scheduling
(`retention:<workspace>:<date>` fire keys). Deletion runs in batches of 5,000
rows inside one tenant transaction under forced row-level security, so a
failure still rolls everything back, and the commit is fenced by the job
lease so a cancelled or reclaimed attempt writes nothing.

No route is seeded for it, so a fresh or upgraded install keeps running it in
Python. `opengtm routes list` shows it as `python (default, switchable)`.

### Cut over and roll back

Cutting over is a routing change only; no migration, no deploy of Python code.

1. **Prerequisites.** The database is at Alembic `7c1e5a9d3b20` or later, and
   every Python API/worker replica is a version that honours
   `job_executor_routes` (older ones ignore routes and would claim the job
   too). `opengtm doctor` must pass, and at least one Go worker must be
   running (`opengtm worker`, or `opengtm serve --worker`): it logs
   `job types available in go are still routed to python` until you switch.
2. **Drain.** `opengtm routes list` shows the `processing` count for the type.
   A retention job runs for seconds to minutes (the ceiling is 1,800 s).
   `routes set` refuses to switch while any are processing under the current
   executor; wait for 0 and run it again. Use `--force` only for a row you
   know is orphaned (for example, its worker is gone).
3. **Switch.** `opengtm routes set retention_enforce go`. Python workers stop
   claiming new jobs of the type immediately; the Go worker starts claiming.
   Pending jobs, including the scheduled `retention:<workspace>:<date>` ones,
   are picked up by Go unchanged.
4. **Verify.** `opengtm routes list` shows `retention_enforce  go`;
   `opengtm doctor` passes (it counts the new explicit route). Start a run
   from a workspace with a retention policy
   (`POST /api/governance/retention/enforce`) and confirm
   `GET /api/governance/retention/runs` reports it `completed` with
   `deleted_counts`, the Go worker logs `retention enforced`, and the
   workspace's next `retention:<workspace>:<date>` job is pending.
5. **Roll back.** `opengtm routes set retention_enforce python` (same drain
   rule). Python claims the type again at once; nothing needs to be redeployed
   or migrated, because the Python handler, its failure reconciler and the
   scheduling code were not changed. Any row a Go worker left `processing` is
   reaped by Python after the usual five minutes without a heartbeat, and
   reconciled by the Python reconciler, which behaves identically.

### Parity

The port is verified against the Python code, not just against its own tests:

```bash
TEST_DATABASE_URL=postgresql+psycopg://postgres:postgres@127.0.0.1:5432/postgres \
  uv run pytest tests/test_retention_go_parity_pg.py
```

`TEST_DATABASE_URL` names a server on which the role may create databases
(the named database itself is not touched). For four time-zone combinations
the test clones a migrated template into two fresh databases, seeds both with
the dataset in `tests/retention_parity/` (all eight target tables, rows one
second either side of every cutoff, several workspaces, legal hold, disabled
and custom policies, an unsupported category, mid-run failures and
reconciliation cases), runs the unchanged Python handler on one and the Go
handler on the other under forced RLS, and requires identical
`retention_runs`, policies, schedule mirror, jobs and remaining rows, and the
same error from every step. A second test compares `normalized_days` and the
cutoff arithmetic over a corpus of malformed values. The Go integration tests
in `internal/jobs/retention` add tenant isolation, lease loss, mid-run
failure, reconciliation, and routing through the real queue.

#### Known differences from the Python implementation

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

## Connector enrichment runs in Go

`run_workbook_connector` is the HTTP enrichment slice of milestone M2: one
workbook run whose cells are provider waterfalls made only of manifest v1
connectors. The Go executor is `internal/jobs/enrich`; the design and scoping
decision are in [`docs/plans/m2-enrichment-slice.md`](../../docs/plans/m2-enrichment-slice.md).
It is a **new job type**, so the Python path is untouched: `run_workbook` stays
Python's, and the new type is Python-owned until you route it.

### What runs in Go

The Python route (`POST /api/workbooks/{id}/run`) enqueues
`run_workbook_connector` instead of `run_workbook` only when **both** hold:
`job_executor_routes` sends the type to `go`, and the run qualifies
(`apps/api/services/workbook/connector_run.py`). Anything else is enqueued as
`run_workbook` exactly as before.

| Qualifies | Stays on `run_workbook` (Python) |
| --- | --- |
| `enrichment` and `waterfall` columns with an **explicit** provider or waterfall | default waterfalls, `ai_formula`, `research`, `agent`, `http`, `formula`, `output` columns |
| every provider is a manifest v1 connector served over https, with no `${env:X}` other than its own auth key, and its key (if any) in the process environment | Python provider classes, scrapers, WASM, http connectors |
| v2 workbook rows with no linked lead | rows linked to a lead (lead write-back and provenance), legacy lead-backed workbooks |
| no `condition`, no `{column}` references, email targets only with `verify: false` | dependency ordering, the SMTP verify cascade |
| `AUTOMATIONS_ENABLED` and `PROVENANCE_TRACKING_ENABLED` off | `on_row_changed` emits, provenance JSON |

What the executor does for one run, as Python does it: bounded concurrency
(`concurrency` rows in flight, `provider_workers` provider calls in flight),
columns in order per row with each result threaded into the row, the planner's
cooldown and budget filtering, the spend ledger (reserve, dispatch under the
lease, settle) with `budget_spent_usd`, retry passes, `fill_missing` and
per-row column scopes, the workbook status and `completed_rows`, the
`execution_result` receipt that `/runs` serves, and provider telemetry.
Provider calls go through the guarded egress client (SSRF guard with DNS
pinning, response size cap, a per-domain limiter) and never inside a database
transaction. Every state change is a tenant transaction under forced row-level
security that first proves the job lease; the last paid attempt's settlement,
the budget increment, the `workbook_enrichments` upsert, the `workbook_rows`
mirror, and the new evidence are **one transaction**. If the lease was lost
while a paid call was in flight, the answer is still settled (it was paid for)
but no result is written.

Progress is published on PostgreSQL `LISTEN/NOTIFY` and served on
`/api/v2/events`: `workbook_cell_update` (per committed cell),
`workbook_run_progress` (rows done, at most four times a second) and
`workbook_status`.

Each winning cell also gets an additive `cell_metadata.evidence` object in
`workbook_enrichments`: provider, source URL with credentials redacted, method,
status, content type, byte count, SHA-256 of the response, fetch time, the
mappings that produced the value, confidence and cost. The Python path writes
no such key.

There is no new HTTP route and no new migration: the schema is Alembic
`7c1e5a9d3b20`, and `/api/workbooks/{id}/run`, `/stop` and `/runs` keep their
contracts (`/runs` and `/stop` now include the new job type).

### Cut over and roll back

Cutting over is a routing change; no migration and no Python deploy beyond this
release.

1. **Prerequisites.** Every Python API and worker replica runs a version with
   `run_workbook_connector` (the API chooses the type at enqueue time, and a
   Python worker needs the handler to pick the type up again after a rollback;
   an older worker would fail such a job with "No handler"). At least one Go
   worker is running (`opengtm worker`, or `opengtm serve --worker`) with
   `OPENGTM_CONNECTOR_DIRS` pointing at the same connectors the Python registry
   loads (the image bundles them) and the connectors' key variables (for
   example `LEADMAGIC_API_KEY`) in **its** environment: the Go worker resolves a
   connector's key from the environment only. `opengtm doctor` passes.
2. **Drain.** Runs already started stay with the executor that owns them; a
   `run_workbook` run is never affected. `opengtm routes list` shows the
   `processing` count of `run_workbook_connector` (zero before the first
   cutover).
3. **Switch.** `opengtm routes set run_workbook_connector go`. From now on
   eligible runs are enqueued as the new type and only Go claims it. The API
   reads the route on every `/run`, so no restart is needed.
4. **Verify.** Run a small workbook with an enrichment column that names a
   connector. `GET /api/workbooks/{id}/runs` shows the run with
   `result.completed`, the worker logs `run_workbook_connector done`,
   `SELECT type FROM jobs WHERE id = <job_id>` is `run_workbook_connector`, and
   `workbook_enrichments.cell_metadata->'evidence'` is present on the cells.
   For paid connectors, `SELECT status, count(*) FROM workbook_spend_attempts
   GROUP BY 1` shows `settled`.
5. **Roll back.** `opengtm routes set run_workbook_connector python` (it refuses
   while attempts are processing; wait, or use `--force` for a row whose worker
   is gone). New runs are enqueued as `run_workbook` at once, and any pending
   `run_workbook_connector` job is claimed by Python and run by the unchanged
   handler. A job a Go worker was in the middle of is recovered by whichever
   executor owns the type when its heartbeat stops: the spend ledger is shared
   and keyed identically, so the other executor reuses settled attempts,
   refuses uncertain ones and never charges twice
   (`test_a_job_started_by_one_executor_is_finished_by_the_other` runs this in
   both directions). Nothing needs to be migrated or redeployed.

Credits (`workspace_credits`) are debited once, by the route, before the job
exists (`check_and_debit`, idempotent per run id). That is unchanged and
independent of the executor; the job only does the spend-ledger accounting.

### Parity and tests

```bash
# cross-language parity, handoff in both directions (Python and Go on identical databases)
TEST_DATABASE_URL=postgresql+psycopg://postgres:postgres@127.0.0.1:55432/postgres \
  uv run pytest tests/test_enrichment_go_parity_pg.py

# Go: lease loss, cancellation, crash recovery, atomicity, concurrency bound, tenant isolation, routing
OPENGTM_TEST_DATABASE_URL=postgresql://postgres:postgres@127.0.0.1:55432/opengtm_test \
  go test -race ./internal/jobs/enrich/

# Python: the gate, the route, the queue pins
uv run pytest tests/test_connector_run_routing.py tests/test_connector_run_endpoint.py \
  tests/test_queue_claim.py tests/test_provider_runner_provider_name.py
```

`tests/test_enrichment_go_parity_pg.py` clones a migrated template into two
databases, seeds both identically (nine manifest v1 connectors, workbooks in
two workspaces, a capped budget, a benched provider, non-ASCII identities, a
float budget edge, rows that make a provider fail, time out, rate limit, return
nothing, return a JSON blob, or break on a non-string value), starts one local
HTTPS provider simulator, and runs the same job rows through the unchanged
Python handler (`handle_run_workbook`) on one database and the Go handler on the
other, both as `NOSUPERUSER NOBYPASSRLS` roles. It requires identical
workbooks, rows and cell overlays, `workbook_enrichments` (including
`cell_metadata`), `workbook_spend_attempts` (attempt keys, contract digests,
settlements), `provider_stats`, job receipts, the same multiset of requests at
the simulator, and the same outcome from every step. A second test lets one
executor start the paid workbooks, wipes the cells but keeps the spend
attempts (a worker dying between settling and committing), and retries the same
job on the other executor: no vendor call, no second charge, the same cells.
Golden values for the attempt key, the contract digest and the micro-USD
rounding are pinned in `internal/jobs/enrich/unit_test.go`.

The Python harness differs from production in three ways that do not change
what is stored: the killable provider process pool is replaced by a thread
running the same `_provider_job` with the same deadline, `check_url` accepts
the loopback simulator, and Redis broadcasts are off.

### Benchmark

```bash
OPENGTM_BENCH_DATABASE_URL=postgresql://postgres:postgres@127.0.0.1:55432/postgres \
  uv run python benchmarks/run_enrich.py            # about 25 minutes; --quick for a smoke run
```

See [`benchmarks/README.md`](../../benchmarks/README.md#enrichment) for the
method and `benchmarks/results/enrich-baseline.md` for the committed numbers.

Headline numbers (`benchmarks/results/enrich-baseline.md`: one machine, a 32-thread
workstation with a load average of about 5, 3 repetitions, medians; a local
simulator with a fixed latency, so these bound the executor, not real vendors;
read the caveats in `benchmarks/README.md`). Cells per second, Go against the
unmodified Python handler with its production provider process pool:

| workload (`synchronous_commit=off`) | Go | Python (process pool) | Go / Python |
| --- | ---: | ---: | ---: |
| free connector, 5 ms provider, 12 rows and 8 calls in flight, 3,000 rows | 1,369 | 94 | 14.5x |
| free connector, 100 ms provider (latency-bound) | 79 | 51 | 1.6x |
| paid connector (reserve, dispatch, settle), 5 ms, 600 rows | 636 | 57 | 11.1x |
| free connector, 24 rows and 24 calls in flight | 2,875 | 76 | 37.7x |

With durable commits (the PostgreSQL default; this disk makes every commit wait
for an fsync) the gap narrows to 3.0x for a free connector (138 against 47
cells/s) and 4.9x for a paid one (210 against 43), because Go needs about 5.0
commits per cell against Python's 8.3 (7.1 against 14.5 when paid). Marginal CPU
per cell is 0.45 ms against 6.1 ms (0.9 against 9.7 paid), and the largest
process is 45 MB against 150 MB. The 100 ms row is bound by the provider (8
calls in flight cap both at 80 cells/s), and Go reaches that bound while Python
does not. Python's thread variant, the cheapest Python can be, is no faster than
its pool (82 cells/s), so the cost is the handler and its session handling, not
the process pool.

One finding about the Python path: at 32 rows in flight with 32 provider
workers the Python run stopped making progress (an idle process for 38 minutes
before it was killed) while Go completed it in under two seconds at 3,451
cells/s. The Python engine's PostgreSQL pool holds 30 connections and each cell
keeps a session open across its provider call; pool exhaustion is the likely
cause but was not confirmed. The committed wide workload therefore uses 24.

### Known differences and limitations

- **Scope.** Only the slice in the table above. Everything else, including the
  email verify cascade, lead write-back, AI/research/agent/http/formula/output
  columns, default waterfalls and legacy lead-backed workbooks, stays Python.
  A payload outside the slice that reaches Go anyway fails the attempt before
  any work (`run_workbook_connector cannot run this workbook: ...`) and is
  retried by the queue, so a misrouted job is recoverable by routing back.
- **Live updates in the dashboard.** Go publishes progress on `LISTEN/NOTIFY`
  (`/api/v2/events`), not on the Redis channel `workbook:<id>` that the current
  grid WebSocket listens on, so a Go-run workbook updates on refetch rather than
  cell by cell until the web client subscribes to v2 events.
- **Secrets.** A connector's key is read from the Go worker's environment.
  Per-workspace secrets still live in the legacy SQLite control plane (RFC
  milestone M8) and are not reachable from Go; the Python route therefore only
  sends a run here when the key resolves identically in its own environment.
  Keep the two environments in step.
- **Connector set.** Python loads connectors from its manifests directory, Go
  from `OPENGTM_CONNECTOR_DIRS`. They must hold the same connectors; a chain
  naming one Go lacks fails the attempt rather than being skipped.
- **Paid connectors and a Python defect found on the way.** Before this branch
  the provider runner left a declarative provider's result unnamed and
  `accounting_envelope` rejects an unnamed result, so every paid connector call
  in a queued Python run ended `uncertain` (the vendor was called, the cell got
  `accounting_uncertain`). The fix is one line in
  `apps/api/services/workbook/provider_runner.py` with its own test and commit.
  Go and the parity harness assume it. Reverting it restores the old Python
  behaviour for paid connectors only; Go would then settle where Python does not.
- **Transactions.** Python settles a paid attempt and commits the cell in
  separate transactions. Go settles each paid attempt right after its call
  (before the next provider is tried) except the last, which commits with the
  cell. A worker that dies between a paid answer and that commit leaves the
  attempt `dispatched`, which the next attempt treats as not retryable
  (`attempt_not_dispatchable`), the same window Python has between the answer and
  its settlement.
- **Lease.** The lease is proven with a shared row lock (`FOR SHARE`) rather
  than `queue.HoldLease`'s `FOR UPDATE`, so the cells of one attempt do not
  queue behind each other (it was the throughput ceiling). A cancel, finalize or
  reclaim still waits for every in-flight cell transaction, and later ones fail
  with `ErrLeaseLost`.
- **Scheduling.** Rows run on a worker pool instead of in barrier batches of
  `concurrency`, and `workbooks.completed_rows` is written at most every 250 ms
  and once at the end, instead of after each batch. Final values are the same.
- **HTTP.** Connectors must be https (the CI validator already requires it).
  Responses are capped at the plugin default (5 MiB), requests share the
  egress client's per-domain limiter at `OPENGTM_ENRICH_DOMAIN_RPS` (default
  1,000 per second per registrable domain; Python applies no limit and a v1
  manifest has no limit field), and the manifest `timeout` bounds one request
  as a whole where httpx applies it per phase. `provider_timeout` is a hard
  deadline in both. Redirects follow the egress client; `robots.txt` is not
  consulted for API providers.
- **Interrupted attempts.** A timeout or a shutdown leaves the workbook
  `running` until the retry, as a killed Python child does; a cancelled attempt
  writes nothing and leaves the workbook alone.
- **Values.** The provider input is `compiler._lead_input_ctx` over the row. A
  non-string `website` or `contact_person` fails that provider with Python's
  `AttributeError` text. Unicode case folding in the derived `domain` follows
  Go's `strings.ToLower`, which differs from Python's `str.lower` for a few
  special characters.
- **Not measured.** Controlled-live traffic against real vendors, a populated
  production-sized database, and multi-replica fairness under load.
