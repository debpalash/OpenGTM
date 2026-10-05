# Benchmarks: the M0 baseline

A reproducible harness that measures the existing Python stack and the new Go
server side by side, so every migration step in
[the rewrite plan](../docs/plans/hybrid-platform-rewrite.md) can be shown to be
faster or equal, not just assumed to be.

It measures four things:

1. **Queue**: claim and completion latency, and throughput, of the shared
   PostgreSQL `jobs` queue, for the Python `QueueService` and the Go queue, at
   1, 4 and 16 concurrent slots.
2. **HTTP front door**: latency and throughput of a cheap FastAPI endpoint
   directly and through the Go reverse proxy, and of Go-owned `/api/v2`
   endpoints.
3. **Web dashboard** (a separate harness, [below](#web-baseline)): bundle sizes
   per route, cold-load FCP/LCP/CLS/TBT, INP and route-transition timings.
4. **A migrated job type**: `retention_enforce` on the Go executor and on the
   Python one ([below](#retention_enforce-go-and-python)).

The committed snapshots from the reference machine are
[`results/baseline.json`](results/baseline.json) (queue and HTTP) and
[`results/web-baseline.json`](results/web-baseline.json) (web), each with a
`.md` next to it holding the tables.

## Run it

```sh
export OPENGTM_BENCH_DATABASE_URL=postgresql://postgres:postgres@127.0.0.1:55432/postgres
./benchmarks/run.sh                 # full run, about 11 minutes
./benchmarks/run.sh --quick         # 1-2 minute smoke run (numbers are not meaningful)
./benchmarks/run.sh --only queue    # or --only http
./benchmarks/run.sh --only retention  # retention_enforce, Go vs Python (about 8 minutes)
./benchmarks/run.sh --label mychange
```

Requirements: `go`, `uv` (the Python environment comes from `uv.lock`) and a
**disposable** PostgreSQL server reachable with a superuser URL (the
`OPENGTM_TEST_DATABASE_URL` used by the Go integration tests works too). The
harness only creates a fresh database `opengtm_bench_<random>` and a
`NOSUPERUSER NOBYPASSRLS` role in it, runs the Alembic migrations, and drops
both at the end, even if a run fails. It never touches the repository's
`./data`: the Python code is copied into a throwaway directory first (so the
SQLite files the app creates and the `.env` it reads are the sandbox's, not
yours), and the run aborts if `./data` changed anyway. Servers listen on random
free ports on `127.0.0.1`.

Everything is printed and written to `benchmarks/results/<label>.json` and
`.md` (git-ignored except `baseline.*`). `./benchmarks/run.sh --help` lists all
knobs (job counts, repetitions, concurrency levels, window lengths, uvicorn
workers). `--endpoints a,b` drives only the named HTTP endpoints, and
`--fastapi-authed-max-conns N` caps the connections used against the
authenticated FastAPI endpoints (0, the default, means no cap; use 32 to
benchmark a checkout from before [the wedge fix](#the-fastapi-wedge)).

## What is measured

### Queue

For each engine and slot count, a backlog of no-op jobs of a benchmark-only job
type is enqueued (using each engine's own production enqueue function), then
drained by that many concurrent slots. The no-op handler and the job type exist
only inside the benchmark (`apps/server/internal/queue/bench_test.go`,
`benchmarks/bench_queue_python.py`); no production registry is changed.

Engines (`mode`):

| mode | what it runs |
|---|---|
| `go-queue` | `Queue.Claim` and `Queue.process` (handler call, lease monitor, guarded finalization) from `apps/server/internal/queue`. |
| `python-inproc` | `QueueService.claim_next_job` and `_process_job`, with only the process boundary replaced by an in-process call of the no-op handler (the pre-flight claim check is kept). This is the like-for-like comparison with Go: it isolates queue bookkeeping. |
| `python-subprocess` | The unmodified production path: `run_job_subprocess` spawns `python -m apps.api.job_process` for every job, which imports the full production handler registry. Only the handler is a no-op. This is what a Python job really costs today, and the reason a handler's work is dwarfed by startup. |

Variants (`variant`), because on a fsync-bound database the engines look alike
for the wrong reason:

| variant | meaning |
|---|---|
| `durable` | `synchronous_commit=on`, the PostgreSQL default. Each claim and each completion waits for a disk flush, so this mostly measures the disk. |
| `nosync` | `synchronous_commit=off` for the benchmark role. Removes the flush wait so the numbers reflect queue code, SQL and driver overhead. |

Numbers in the tables:

- `jobs/s`: jobs completed per second, from the first claim to the last
  completion of the backlog. Higher is better.
- `claim`: duration of the claim call alone (p50/p95/p99, ms).
- `cycle`: claim + handler + guarded completion commit for one job
  (p50/p95/p99, ms). This is the "claim and complete" latency. It does not
  include time a job waited in the backlog.
- `enqueue/s`: sequential single-connection enqueue rate of each engine's own
  enqueue function. Informational.
- Percentiles use the nearest-rank definition. With N jobs, p99 is the 99th
  percentile of N samples per repetition; each repetition's value is computed
  first and the table shows the **median across repetitions** (the range is
  shown for throughput).

Each slot mirrors the production worker loop (claim, then process) except that
an empty claim retries after 1 ms instead of sleeping 1 s: the backlog is
finite and the 1 s idle poll would otherwise decide the tail of a short run.
Both engines do this.

The claim query orders by a correlated count over the table, so claim latency
grows with the number of pending rows. The backlog drains from N to 0 during a
run; that effect is identical for both engines and is part of what is measured.

### HTTP

A small closed-loop load generator
(`apps/server/internal/bench/loadgen`, standard library only) drives
keep-alive connections at 1, 16 and 64 concurrency. Each point is a warm-up
window followed by a measured window, repeated several times.

| endpoint label | request | meaning |
|---|---|---|
| `fastapi-health-direct` | `GET /health` on uvicorn | Cheapest FastAPI route: no auth, no database. |
| `fastapi-health-via-go-proxy` | same, through `opengtm serve` | Cost of the Go reverse proxy in front of it. |
| `fastapi-authed-direct` | `GET /api/auth/workspace-context` with a bearer token | A cheap authenticated FastAPI route (JWT, user lookup, workspace lookup). |
| `fastapi-authed-via-go-proxy` | same, through the proxy | Proxy cost with a request that does real work. |
| `go-version` | `GET /api/v2/version` | The floor for a Go-owned route: no auth, no database. |
| `go-plugins-authed` | `GET /api/v2/plugins` with a bearer token | A Go-owned authenticated route. The server authorizes through FastAPI and caches the answer for 30 s, so after the first request this measures the Go handler, not a FastAPI round trip. |

FastAPI runs under uvicorn with 2 workers (the Dockerfile default; change with
`--api-workers`). Access logging is left on for both servers, as in a default
deployment. The harness fails loudly if any endpoint does not return 200, so a
fast run of errors can never be mistaken for a result.

### retention_enforce: Go and Python

Each case seeds a fresh workspace with N expired rows (and 100 recent ones) in
every one of the 8 tables the job purges (`retention_seed.sql`), then runs the
real code on both sides as the same `NOSUPERUSER NOBYPASSRLS` role: the handler
alone (Python `handle_retention_enforce` in-process, Go `Worker.Handle`), and
the whole job (Python `claim_next_job` + `_process_job`, which spawns the
production child process; the real Go queue with a 5 ms idle poll). Every case
verifies the outcome (run `completed`, `8 x N` rows deleted, the recent rows
left) before its time counts. Both `durable` and `nosync` variants run, the
engines interleaved per repetition (`--retention-rows`, `--retention-reps`,
`--retention-variants`; `--retention` adds it to a full run). The committed
snapshot is [`results/retention.md`](results/retention.md) (3 repetitions,
load average about 7 from other work on the machine).

Medians of 3, milliseconds, `durable` variant (`nosync` is within about 10% of
it; the report has both, with ranges):

| expired rows per table (rows deleted) | Python handler | Go handler | Python whole job | Go whole job |
|---:|---:|---:|---:|---:|
| 1,000 (8,000) | 13 | 18 | 819 | 22 |
| 20,000 (160,000) | 61 | 290 | 947 | 332 |
| 100,000 (800,000) | 292 | 1,168 | 1,252 | 1,078 |

What it says, and what it does not:

- **The whole job is faster in Go, but only because of the process.** The
  Python queue spawns a child interpreter and imports the full handler
  registry for every job, about 0.8 s; the Go queue does not. That is a
  37x difference for a small purge, 2.9x at 160,000 rows and 1.2x at 800,000.
- **The Go handler itself is slower than Python's on large purges**, by 1.3x
  at 8,000 rows and 4-5x from 160,000 rows up (0.21x and 0.25x speed). The Go
  executor deletes in batches of 5,000 (`DELETE ... WHERE id = ANY(ARRAY(SELECT
  id ... LIMIT 5000))`) to bound statement memory and WAL bursts, so every
  batch scans past the rows its predecessors deleted; Python deletes each table
  with one statement. The batch is not free: it is the price of the bounded
  statements, and the cheapest improvements to evaluate are a larger batch or a
  `ctid`-based scan. The purge still completes 800,000 rows in about a second.
- Not measured: a populated production-sized database (these tables are
  freshly seeded, so no index bloat, cold caches or concurrent writers), a
  second concurrent workspace, and the 1,800 s ceiling. Absolute numbers
  depend on the machine; the ratios within one run are the useful output.

## Compare a candidate against the baseline

```sh
./benchmarks/run.sh --label candidate
uv run --frozen python benchmarks/compare.py benchmarks/results/candidate.json
#   or an explicit baseline:  compare.py BASELINE.json CANDIDATE.json
```

`compare.py` exits **1** if a gated metric is worse than the baseline by more
than the threshold, **0** if not, and **2** on bad input. The threshold is 15%
by default; change it with `--threshold 0.10` or `BENCH_REGRESSION_THRESHOLD`.

- *Gated* metrics: throughput (`jobs_per_sec`, `rps`) and p50/p95 latency
  (`cycle_ms`, `p50_ms`, `p95_ms`). p99, claim latency and enqueue rate are
  recorded but informational (`--all` gates them too), because tail values on a
  shared machine are too noisy to fail a build on.
- `--only 'queue/nosync/go-queue/*'` (repeatable) limits the check to metrics
  matching a glob, for example when a change only touches the Go queue.
- Metrics missing from the candidate are reported; `--strict` makes them fail.
- A different CPU or core count produces a warning: do not compare across
  machines. Metric keys look like `queue/<variant>/<mode>/c<slots>/<metric>`
  and `http/<endpoint>/c<conns>/<metric>`.

### Wiring it into CI later

Not enabled today. Shared CI runners are too noisy for this (see below), so run
it on a dedicated, otherwise idle runner with a PostgreSQL service container,
and keep a baseline produced on that same runner (not this repository's laptop
snapshot):

```yaml
bench:
  runs-on: [self-hosted, bench]        # dedicated machine, nothing else running
  steps:
    - uses: actions/checkout@v4
    - run: ./benchmarks/run.sh --label candidate
      env:
        OPENGTM_BENCH_DATABASE_URL: postgresql://postgres:postgres@127.0.0.1:5432/postgres
    - run: uv run --frozen python benchmarks/compare.py benchmarks/results/runner-baseline.json benchmarks/results/candidate.json
```

## Updating the committed baseline

When an intended change legitimately moves a number, or the reference machine
changes, re-run the full benchmark on an idle machine, review the diff of
`baseline.md`, and replace the snapshot:

```sh
./benchmarks/run.sh --label baseline      # overwrites results/baseline.{json,md}
```

Say in the commit message why the numbers moved.

## Web baseline

`./benchmarks/web.sh` measures the dashboard (`apps/web`). It builds the app,
reads the bundle sizes from the Vite manifest, then drives the built SPA with
headless Chromium through Playwright (already in `uv.lock`; install the browser
once with `uv run playwright install chromium`).

```sh
./benchmarks/web.sh                    # about 8 minutes, writes results/web-<run>.json and .md
./benchmarks/web.sh --label baseline   # overwrites the committed results/web-baseline.{json,md}
./benchmarks/web.sh --quick            # 1 repetition, desktop profile (smoke run)
./benchmarks/web.sh --bundle-only      # just the deterministic bundle sizes, no browser
uv run --frozen python benchmarks/compare.py benchmarks/results/web-baseline.json benchmarks/results/web-candidate.json
```

What it records ([`results/web-baseline.md`](results/web-baseline.md) has the tables):

| group | metrics | how |
|---|---|---|
| Bundle size | Initial JS and CSS (gzip); per page chunk: JS gzip on top of the initial bundle, CSS, and the total a first visit downloads; all JS | Vite manifest, same definition of "initial" as `apps/web/scripts/check-bundle-size.ts`. Deterministic. |
| Cold load of `/chat`, `/leads`, `/workbooks`, `/plugins` | TTFB, FCP, LCP, time until the page heading is painted, load event, CLS, TBT, JS transferred, request count | Fresh browser context (cold HTTP cache) per repetition; PerformanceObserver entries (`paint`, `largest-contentful-paint`, `layout-shift`, `longtask`). CLS is the largest session window; TBT is long-task time over 50 ms after FCP. |
| INP | Worst interaction of a scripted session on the Plugins page (tab switches, a filter, Load more, the command menu, opening a run) | Real pointer and keyboard events, measured with the Event Timing API (for fewer than 50 interactions INP is the worst one). |
| Route transitions | Click on a sidebar link until the next page's heading is painted, first visit (its chunk is fetched then), plus the JS fetched | In-page timing around the click and two animation frames. |

Two profiles run: `desktop` (no throttling) and `slow` (4x CPU slowdown,
1.6 Mbit/s down, 150 ms RTT; the Lighthouse mobile simulation, applied with the
DevTools protocol). This is Lighthouse-style, not Lighthouse: no Lighthouse
score is computed, and the throttling is applied by the browser, not simulated
from a trace.

**Gating.** `compare.py` reads per-metric `threshold` and `slack` from the
result. Bundle sizes are gated at 3-5% (plus 0.5-1 kB of slack), because they
are deterministic; LCP, heading-ready, INP and transition timings are gated at
30% plus a 60 ms (desktop) or 300-400 ms (slow) floor; FCP, TBT and CLS are
recorded but not gated. Timings are only worth gating on an idle machine; on a
shared one, expect them to move by that much on their own. Pass `--threshold`
to override every metric's own.

**What this does not measure.**

- The backend. `/api` and `/auth` are answered from fixed fixtures instantly
  (500 leads, 24 workbooks, 12 plugins, 400 plugin runs paged 50 at a time),
  and everything outside the local server is answered with an empty image, so
  the numbers describe the frontend, not the whole stack. Real API latency
  adds directly to "heading ready".
- Real networks, real data volumes beyond the fixtures, or real GPUs: the
  browser is headless Chromium on the reference machine's CPU.
- Bundle sizes here are gzipped with Python's zlib at level 9, which differs by
  about 0.4% from the Bun-based `check-bundle-size.ts` that CI enforces. Use
  that script for the budget; use these numbers for comparisons with each other.
- Chromium aborts in-flight requests with `ERR_NETWORK_CHANGED` when the
  host's network interfaces change (a docker bridge or Wi-Fi flapping is
  enough, even for loopback). Such samples are detected, discarded and retaken
  (the log says "discarding a sample"); they never enter the result.

`tests/test_web_plugin_runs_paging_e2e.py` is a functional check using the same
server and fixtures: it scrolls the Plugins > Runs list in a real browser and
asserts the cursor chain, that only the first page is requested up front, that
rows are virtualized, that the Load more button and the status filter behave,
and that a failed page keeps the list. It skips when `apps/web` is not built or
no browser is installed.

## The FastAPI wedge

The M0 run found that the authenticated route (`/api/auth/workspace-context`,
which the Go server calls for every `/api/v2` request it cannot answer from its
cache) stopped answering at 64 concurrent connections against two uvicorn
workers: every request timed out and the workers stayed wedged.

**Cause.** Not load; an async/sync boundary bug. The async auth dependencies
(`get_current_user`, `current_workspace`) ran blocking calls on the event loop:
the users query on the request's SQLAlchemy `Session`, and about seven SQLite
workspace-metadata lookups. A `Session` holds its pooled connection until the
`get_db` dependency closes it, and that close runs only after the event loop has
sent the response. Once more requests than the pool can lend (10 + 20 overflow
per worker, so 30; 64 connections over two workers is 32 each) had reached their
first query, the event loop itself blocked in `Pool.connect` waiting for a
connection that only the blocked loop could release. It sat out `pool_timeout`
(30 s), failed one request, and the queue behind it repeated the stall.

**Fix** (`apps/api/core/security.py`, `core/tenancy.py`, `database.py`, `main.py`):

1. The blocking lookups run in the thread pool (`run_in_threadpool`). The
   `ContextVar` write the RLS hook depends on stays in the request's own async
   context, as before.
2. Pool exhaustion fails fast: `DB_POOL_TIMEOUT` (default 10 s instead of 30 s).
3. Threads that wait for a pooled connection can starve the threads that would
   release one (AnyIO has 40 by default), so `THREADPOOL_SIZE` defaults to 100.
   Without it, 128 connections (64 per worker) still stalled for the pool timeout.

**Regression test.** `tests/test_workspace_context_concurrency.py` drives the
real dependency chain in one event loop through a 2-connection pool with 24
concurrent requests. Before the fix: 22 of 24 requests returned 500 and it took
66 s. After: all 200, in about 0.5 s.

**Go side** (`apps/server/internal/authz`): a slow or sick FastAPI can no longer
wedge the Go server either. Lookups have a 3 s timeout; concurrent cache misses
for the same token and workspace collapse into one call; at most 32 run at once
and the rest are refused immediately with `503` and `Retry-After` (the 32 also
keeps FastAPI at or below 16 per worker, under its pool); and 5 consecutive
failures open a circuit breaker for 5 s, after which one probe decides. Tests
cover each, including 200 concurrent requests against an upstream that accepts
and never answers (all answered within a second, peak upstream concurrency
capped, the breaker cutting off further calls).

**Measured** (`./benchmarks/run.sh --only http --http-concurrency 1,16,64,128
--endpoints fastapi-authed-direct,fastapi-authed-via-go-proxy,go-plugins-authed
--http-duration 6`, 3 repetitions, same loaded machine as the baseline):

Before the fix (one 6 s window, `fastapi-authed-direct`):

| conns | req/s | p50 ms | p99 ms | errors |
|---:|---:|---:|---:|---:|
| 16 | 326 | 21.5 | 122 | 0 |
| 64 | **0.5** | 203 | 30,176 | **62** (every request timed out) |

After the fix (all fixes in, median of 3 windows, no errors anywhere):

| endpoint | conns | req/s | p50 ms | p99 ms | errors |
|---|---:|---:|---:|---:|---:|
| fastapi-authed-direct | 16 | 476 | 25.7 | 57 | 0 |
| fastapi-authed-direct | 64 | 419 | 147 | 248 | 0 |
| fastapi-authed-direct | 128 | 396 | 296 | 670 | 0 |
| fastapi-authed-via-go-proxy | 64 | 342 | 185 | 251 | 0 |
| fastapi-authed-via-go-proxy | 128 | 380 | 256 | 667 | 0 |
| go-plugins-authed | 128 | 91,952 | 0.87 | 6.5 | 0 |

The cap is lifted: the default concurrency levels now include 64 for the
authenticated FastAPI endpoints (`FASTAPI_AUTHED_MAX_CONNS` is gone;
`--fastapi-authed-max-conns` re-adds a cap), and 128 also runs clean. Throughput
past 16 connections is flat, as expected for two Python workers; what changed is
that it no longer collapses. Limits that remain: beyond roughly 100 in-flight
requests per worker the thread pool can run out again and requests fail after
`DB_POOL_TIMEOUT` instead of queueing; other async endpoints in FastAPI that run
blocking calls on the event loop have the same latent problem, and are not
changed here; the pool is still 30 connections per worker. The Go server's cap of
32 concurrent authorization lookups keeps its own traffic well inside that.

Read the shared-machine caveats below: the point is the 64-connection row
going from "no answers" to "answers at the rate of 16", not the absolute
numbers.

## Read this before trusting a number

- **Single machine, shared.** The reference run was made on a 32-thread
  workstation that was also running other containers, builds and test suites.
  The load average at the start and end of every run is recorded in the result
  and shown in the report; it was well above idle. Absolute numbers will differ
  on other hardware; ratios between engines in the same run are the useful
  output.
- **Noise is real, especially tails.** Each point is repeated 3 times (queue
  and HTTP) and the median is reported with its min-max range for throughput.
  Look at the range before believing a difference smaller than about 15-20%.
  p99 over a few hundred jobs is essentially the worst two or three samples.
- **The `durable` queue variant is dominated by fsync stalls** of the
  PostgreSQL container's disk, which on the reference machine are bursty
  (hundreds of milliseconds). Those stalls hit whichever engine is running at
  the time, so engine-to-engine ratios in that variant are weak evidence. Use
  `nosync` to compare code, `durable` to see what a deployment on this disk
  would experience.
- **Warm-up.** Every queue case runs a short discarded warm-up (to open
  connections and prime caches) on an emptied, re-analyzed table before the
  measured run; every HTTP point has a 1.5 s unmeasured warm-up, so connection setup and
  first-request costs are excluded. Cold-start cost is not measured.
- **Job counts differ by variant** (durable: 300, nosync: 2000, subprocess: 24)
  because the slow variants would otherwise take hours. Compare engines within
  a variant, not across variants.
- **Closed-loop HTTP load.** Latency here is service time at a fixed number of
  in-flight requests. It understates queueing delay when a target stalls
  (coordinated omission) and is not an SLO. The load generator, the servers and
  PostgreSQL share one machine, so very high request rates partly measure CPU
  contention with the load generator.
- **The Go queue loop is a mirror of the production slot loop**, not
  `Queue.Run` itself, because `Run` blocks until cancelled and polls an empty
  queue every second. The measured pieces (`Claim`, `process`) are the
  production functions. The Python harness mirrors `_worker_loop` the same way.
- **The committed `baseline.json` predates the FastAPI wedge fix.** Its
  `fastapi-authed-*` rows stop at 16 connections because that checkout wedged at
  64 (see [The FastAPI wedge](#the-fastapi-wedge)). Re-run the full benchmark on
  an idle machine to refresh it; the 64-connection rows now exist.
- **No real work.** These numbers bound the queue and front-door overhead only.
  They say nothing about enrichment throughput, which depends on providers.
- **Python 3.13 and Go 1.27** versions, plus the PostgreSQL settings in effect
  (including `fsync`, `synchronous_commit`, `shared_buffers`), are recorded in
  the result's `machine` block.

## Enrichment

`benchmarks/run_enrich.py` compares the Python workbook executor with the Go
one (`apps/server/internal/jobs/enrich`) on the M2 slice: a workbook run with
one enrichment column and one manifest v1 connector per cell, answered by a
local HTTPS provider simulator (`apps/server/internal/bench/providersim`, a
fixed latency, standard library only). One **cell** is one completed
enrichment: the provider call, the result and its evidence written, and for
the `paid` workload the spend reservation, dispatch and settlement.

```sh
export OPENGTM_BENCH_DATABASE_URL=postgresql://postgres:postgres@127.0.0.1:55432/postgres
uv run --frozen python benchmarks/run_enrich.py            # about 25 minutes
uv run --frozen python benchmarks/run_enrich.py --quick    # smoke run, numbers not meaningful
uv run --frozen python benchmarks/compare.py benchmarks/results/enrich-baseline.json benchmarks/results/<candidate>.json
```

Same throwaway-database, sandbox-copy and `./data` guarantees as the queue
benchmark. Engines, interleaved within a repetition:

| engine | what it runs |
|---|---|
| `go` | `Worker.Handle`, claimed job row, shared egress client. |
| `python-pool` | `handle_run_workbook` unmodified, with its production killable process pool for provider calls. What Python does per cell. |
| `python-threads` | the same handler with the provider call on a thread: no process isolation or pickling, the cheapest Python can be (a lower bound on its cost). |

Both run the handler in-process, so the per-job child process of `QueueService`
(about a second per job, see the queue benchmark) is not counted, and Redis
broadcasts are off for Python (no Redis here; production publishes one message
per cell). Both favour Python slightly. Go publishes `LISTEN/NOTIFY` progress
and that cost is included. The sandbox gets a `sitecustomize.py` so the SSRF
guard accepts the loopback simulator in the Python process and in the pool's
workers; everything else still goes through the real guard.

Workloads: `free-5ms` (overhead-bound), `free-100ms` (provider-latency-bound),
`paid-5ms` (the spend ledger), `free-5ms-wide` (32 rows and 32 provider calls in
flight). Variants as in the queue benchmark: `nosync` (`synchronous_commit=off`,
reflects code) and `durable` (the default; on this disk every commit waits for
an fsync, so it mostly reflects how many commits a cell needs; fewer rows are
used to keep it short).

Reported per engine: cells/s (rows / wall time of the job), the marginal CPU
time per cell of the whole process tree (the same engine running a one-row job
is subtracted, so start-up and imports cancel), PostgreSQL commits per 1,000
cells, and the largest process's peak RSS. Every run is verified: all cells
must be `complete`, and for `paid` every attempt `settled` with
`budget_spent_usd` equal to rows x 0.05, or the run aborts.

Caveats beyond those above: the machine is shared (see the load average in the
result), the simulator and the engines share its CPUs, and a simulator with a
fixed latency says nothing about real vendors' rate limits or tail latency.
`compare.py` gates `cells_per_sec`; CPU and commit counts are informational.

## Files

| file | role |
|---|---|
| `run.sh`, `run.py` | One-command entry point and orchestrator: builds the Go binaries, creates and drops the throwaway database, runs everything, writes results. |
| `bench_queue_python.py`, `bench_child.py` | Python queue harness and its benchmark-only child process. |
| `../apps/server/internal/queue/bench_test.go` | Go queue harness (a test, skipped unless `OPENGTM_BENCH_OUT` is set). |
| `bench_retention_python.py`, `retention_seed.sql`, `../apps/server/internal/jobs/retention/bench_test.go` | retention_enforce harnesses (Python, shared seed data, Go). |
| `../apps/server/internal/bench/loadgen/` | HTTP load generator. |
| `seed_http.py` | Creates the user and workspace the authenticated endpoints need. |
| `report.py` | Renders a result JSON as Markdown (`python benchmarks/report.py FILE`). |
| `compare.py` | Regression check against a baseline (works on the web results too). |
| `web.sh`, `web.py` | The web baseline: bundle sizes, load metrics, INP and route transitions (Playwright). |
| `../tests/test_web_plugin_runs_paging_e2e.py` | Browser check of the Plugins > Runs list (reuses `web.py`'s server and fixtures). |
| `run_enrich.py`, `bench_enrich_python.py` | Enrichment benchmark (Python executor against Go) and its Python child. Go side: `../apps/server/internal/jobs/enrich/bench_test.go`; provider simulator: `../apps/server/internal/bench/providersim/`. |
