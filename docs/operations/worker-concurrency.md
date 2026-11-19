# Worker slots and enrichment concurrency

Workbook enrichment concurrency is bounded at four nested levels. Each level
caps a different resource, and the effective parallelism is the *minimum*
along the path, not the product. This page lists every knob, its default, what
it bounds, and how to tune it. Measure changes with the
[enrichment completion harness](#measuring-with-the-enrichment-harness).

```
replicas x WORKER_CONCURRENCY slots            (durable jobs at once)
  └─ capped per tenant by WORKER_MAX_ACTIVE_PER_WORKSPACE
     └─ one killable job child per slot         (run_workbook)
        └─ row_concurrency rows per batch       (asyncio, one DB session per cell)
           └─ provider_workers pebble processes (killable provider calls)
```

## Durable queue (per worker process)

| Setting | Default | Range | Bounds |
| --- | --- | --- | --- |
| `WORKER_CONCURRENCY` | `1` | 1-64 | Claim slots per worker process (the in-API worker or each `python -m apps.api.worker` replica). Each slot claims one job and runs it in its own killable child process (`apps.api.job_process`). Total job parallelism is `replicas x WORKER_CONCURRENCY`. |
| `WORKER_MAX_ACTIVE_PER_WORKSPACE` | `2` | 0-64 (0 disables) | Jobs one workspace may have in `processing` at once, across all replicas. PostgreSQL enforces it with a per-workspace advisory lock during the claim; global jobs (no workspace) are uncapped. This is the tenant-fairness bound. |
| `WORKER_SHUTDOWN_GRACE_SECONDS` | `30` | 0-300 | How long shutdown drains active slots before cancelling (and killing) their job children. |
| `RUN_INLINE_WORKER` | `1` | `0`/`1` | Whether the API process also runs a worker. Set `0` when standalone worker replicas own processing. |
| `run_workbook` timeout | 1800 s | code (`JOB_TIMEOUTS`) | Wall-clock limit on one enrichment job; the child is killed on expiry. |
| Heartbeat interval | 30 s | code | Lease renewal; dead-worker reaping uses it. |

A single `run_workbook` job occupies one slot for its whole run, so raising
`WORKER_CONCURRENCY` speeds up *several* workbooks (or tenants) running at once,
not one large workbook. With the default of one slot, a second workbook waits
until the first finishes.

## Per-run enrichment settings (Settings -> Enrichment performance)

These are stored in the settings database as `ENRICH_<KEY>` (set them in the UI
or with `PUT /api/settings/enrichment-perf`, admin only). The API snapshots them
into each `run_workbook` job payload at enqueue time, so a change affects the
next run, not one already queued.

| Setting | Default | Range | Bounds |
| --- | --- | --- | --- |
| `row_concurrency` | `12` | 1-64 | Rows enriched concurrently inside one job. Rows run in batches of this size and each batch waits for its slowest row before the next starts. Each in-flight cell holds one database session. |
| `provider_workers` | `8` | 1-64 | Size of the pebble process pool for provider calls inside one job child. Caps concurrent provider calls per job; a hung call costs one killed process, not the run. |
| `provider_timeout` | `10` s | 3-120 | Per provider call; the provider process is killed when it expires. |
| `ENRICH_TARGET_SITE_TIMEOUT` (environment of the worker and api, not a stored setting) | `30` s | 3-120 | Timeout floor for providers that fetch the lead's own website (`website_scraper`, `deep_scraper`, `jsonld_firmographics`, `tech_stack`, `mailscout`): they run with the larger of `provider_timeout` and this value. They load several pages one after another (the first-run benchmark measured 5 to 35 s per call), which a single vendor API call, the case the 10 s default is sized for, does not. A timeout of one of these providers never starts a provider-wide cooldown. |
| `max_providers` | `8` | 0-20 (0 = full chain) | Waterfall depth per cell. |
| `retry_passes` | `1` | 0-5 | Extra passes over cells still in `error` after the main pass. |

Environment fallbacks used when a payload or the settings database lacks a
value: `WORKBOOK_RUN_CONCURRENCY` (12), `WORKBOOK_PROVIDER_WORKERS` (8),
`WORKBOOK_PROVIDER_TIMEOUT` (10), `WORKBOOK_MAX_PROVIDERS_PER_CELL` (0).

## Provider cooldown

A provider that is failing is skipped for a while so the rest of the workbook is not held up by it (`provider_stats.cooldown_until`, one row per provider and field, shared by the workspace).

- A rate limit (HTTP 429, quota, throttle) pauses the provider for 300 s at once.
- A timeout pauses it for 180 s only after three timeouts in a row with no success or miss between, and calls that were started together count as one event (twelve rows that all time out together are one timeout, three successive rounds are three).
- A provider that fetches the lead's own website or mail server never starts a timeout cooldown: its timeout belongs to that one lead. Each provider declares which kind it is (`providers.Upstream`, `providers.TargetSite`, `providers.Local`) and a test fails for a provider that declares none.
- The pages of one website are fetched once per run for all columns: `deep_scraper` and `website_scraper` keep their answer for two minutes.

## Database connection pools (per process)

Every job child creates its own SQLAlchemy engine:

| Backend | Pool |
| --- | --- |
| SQLite | SQLAlchemy default `QueuePool`: 5 connections + 10 overflow, 30 s checkout timeout |
| PostgreSQL | `pool_size=10`, `max_overflow=20` (30 connections), 30 s checkout timeout |

PostgreSQL `max_connections` must cover
`replicas x (1 + WORKER_CONCURRENCY x per-child pool)` plus the API.

## Tuning

1. **Keep `row_concurrency` below the job child's pool capacity**: at most 12
   on SQLite (15 connections) and about 25 on PostgreSQL (30 connections).
   Cells keep their session open across the provider call, and a checkout
   that has to wait blocks the event loop of the job child. Every in-flight
   cell then stalls until the 30 s pool timeout fires. The M0 harness
   reproduced this: `row_concurrency=32` on SQLite finished 14 cells in about
   ten minutes. The Settings UI currently allows up to 64. Track this as a
   bug; do not tune around it.
2. **Match `provider_workers` to `row_concurrency`** for single-provider
   columns. A provider call is the only work that uses the pool, so with
   `provider_workers < row_concurrency` part of each batch queues behind the
   rest. Each provider worker is a full Python process (about 100-160 MB RSS),
   so per host memory is roughly
   `replicas x WORKER_CONCURRENCY x provider_workers` processes.
3. **Raise `WORKER_CONCURRENCY`** when several workbooks or tenants run at
   once. Keep `WORKER_MAX_ACTIVE_PER_WORKSPACE` below it so one tenant cannot
   take every slot.
4. **Use PostgreSQL** for concurrent writers. SQLite serializes writes and is a
   development backend.
5. Re-measure after every change with the same harness parameters and record
   the identity block next to the result.

## Measuring with the enrichment harness

`scripts/bench/enrichment_harness.py` runs the real path: durable queue claim,
`run_workbook` job child, declarative HTTP provider in the pebble pool, and
result commit. Calls go to a deterministic local provider simulator
(`scripts/bench/provider_simulator.py`).

```bash
# Shipped defaults, 500 rows, fixed 50 ms provider latency, temp SQLite
uv run python scripts/bench/enrichment_harness.py --rows 500 --latency-ms 50 \
  --output artifacts/bench/enrichment-500.json

# Fault mix: lognormal latency, 1% 5xx, 2% 429, fixed seed
uv run python scripts/bench/enrichment_harness.py --rows 2000 --latency-ms 50 \
  --latency-dist lognormal --sigma 0.5 --error-rate 0.01 --rate-429 0.02 --seed 42

# Override any knob (defaults are the shipped values above)
uv run python scripts/bench/enrichment_harness.py --rows 2000 --workbooks 4 \
  --workspaces 2 --worker-concurrency 4 --row-concurrency 12 --provider-workers 12

# PostgreSQL: a disposable, migrated database only
BENCH_DATABASE_URL=postgresql+psycopg://.../opengtm_bench \
  uv run python scripts/bench/enrichment_harness.py --rows 2000
```

The report (`--output` writes JSON, `--json` prints it) includes:

- **Results:** committed completions/sec (from enqueue, and excluding the
  job-start overhead); end-to-end latency per cell (enqueue to result commit)
  and cell execution latency at p50/p95/p99; startup time to the first cell;
  cell status and error counts; job statuses; simulator request and status
  counts (provider attempts, 429s, 5xx); approximate CPU and peak RSS.
- **Identity block:** git commit and dirty flag; CPU model, cores, RAM, and OS;
  Python version; database dialect and version; Redis reachability; every
  worker and enrichment setting above; workload parameters (rows, workbooks,
  workspaces, simulator latency, error rates, and seed).

How it works and what it changes:

- Simulator outcomes are a pure function of `(seed, row domain, attempt)`, so
  identical parameters produce the same provider behavior regardless of
  scheduling.
- Benchmark processes load `scripts/bench/_shim/sitecustomize.py` through
  `PYTHONPATH`. It makes three changes: it exempts exactly the simulator's
  loopback origin from the SSRF guard (any other URL is still checked), it
  registers the generated manifest through the normal compile/register path,
  and it records per-cell timings. It refuses any non-loopback origin. None of
  this ships in the application.
- With the default temporary SQLite database, nothing persists. With a
  supplied database, the harness refuses to start if other jobs are
  pending/processing. It claims only its own fire-key-tagged jobs and deletes
  only the rows it created.
- The email verify cascade is disabled on the benchmark column (it would
  measure live DNS/SMTP). Pass `--cost-per-lookup` > 0 to route calls through
  the spend-reservation path.
- `kill -USR1 <job child pid>` dumps all thread stacks of a stalled benchmark
  process to stderr.

Developer-laptop numbers are a regression reference only. They are not capacity
claims; see [benchmarks/enrichment-baseline.md](../benchmarks/enrichment-baseline.md).
