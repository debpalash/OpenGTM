# Benchmarks: the M0 baseline

A reproducible harness that measures the existing Python stack and the new Go
server side by side, so every migration step in
[the rewrite plan](../docs/plans/hybrid-platform-rewrite.md) can be shown to be
faster or equal, not just assumed to be.

It measures two things:

1. **Queue**: claim and completion latency, and throughput, of the shared
   PostgreSQL `jobs` queue, for the Python `QueueService` and the Go queue, at
   1, 4 and 16 concurrent slots.
2. **HTTP front door**: latency and throughput of a cheap FastAPI endpoint
   directly and through the Go reverse proxy, and of Go-owned `/api/v2`
   endpoints.

The committed snapshot from the reference machine is
[`results/baseline.json`](results/baseline.json) (machine-readable) and
[`results/baseline.md`](results/baseline.md) (tables).

## Run it

```sh
export OPENGTM_BENCH_DATABASE_URL=postgresql://postgres:postgres@127.0.0.1:55432/postgres
./benchmarks/run.sh                 # full run, about 11 minutes
./benchmarks/run.sh --quick         # 1-2 minute smoke run (numbers are not meaningful)
./benchmarks/run.sh --only queue    # or --only http
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
workers).

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
- **A FastAPI finding, not benchmarked through.** The authenticated route
  (`/api/auth/workspace-context`) stops answering at 64 concurrent connections
  against two uvicorn workers: every request times out and the workers stay
  wedged until restarted (found while building this harness; cause not
  investigated, a thread-pool or connection-pool exhaustion with sync
  dependencies is the likely suspect). The harness therefore drives the
  `fastapi-authed-*` endpoints only up to 16 connections by default
  (`FASTAPI_AUTHED_MAX_CONNS = 32` in `run.py` is the hard cap). The Go-owned
  routes were driven to 64 without errors.
- **No real work.** These numbers bound the queue and front-door overhead only.
  They say nothing about enrichment throughput, which depends on providers.
- **Python 3.13 and Go 1.27** versions, plus the PostgreSQL settings in effect
  (including `fsync`, `synchronous_commit`, `shared_buffers`), are recorded in
  the result's `machine` block.

## Files

| file | role |
|---|---|
| `run.sh`, `run.py` | One-command entry point and orchestrator: builds the Go binaries, creates and drops the throwaway database, runs everything, writes results. |
| `bench_queue_python.py`, `bench_child.py` | Python queue harness and its benchmark-only child process. |
| `../apps/server/internal/queue/bench_test.go` | Go queue harness (a test, skipped unless `OPENGTM_BENCH_OUT` is set). |
| `../apps/server/internal/bench/loadgen/` | HTTP load generator. |
| `seed_http.py` | Creates the user and workspace the authenticated endpoints need. |
| `report.py` | Renders a result JSON as Markdown (`python benchmarks/report.py FILE`). |
| `compare.py` | Regression check against a baseline. |
