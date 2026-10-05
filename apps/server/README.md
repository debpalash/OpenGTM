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
| `OPENGTM_PLUGIN_DIRS`, `OPENGTM_CONNECTOR_DIRS`, `CONNECTOR_SIGNATURE_POLICY`, `OPENGTM_PLUGIN_TRUST_STORE`, `OPENGTM_EGRESS_PROXY`, `OPENGTM_PLUGIN_INDEX` (CLI only) | see the [plugin guide](../../docs/plugins/README.md#running-plugins-on-a-server) | |
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
internal/plugin/...  manifests, templates, signing, bundles, plugin index, fixtures, runtimes
internal/pluginrun   plugin catalog, plugin_run jobs and /api/v2 plugin routes
internal/jobs/...    job types migrated from Python (retention_enforce)
```

## Job types running in Go

| Job type | Owner by default | Switch |
| --- | --- | --- |
| `plugin_run` | Go (a new type; seeded by migration `7c1e5a9d3b20`) | n/a |
| `retention_enforce` | **Python** | `opengtm routes set retention_enforce go` |

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
