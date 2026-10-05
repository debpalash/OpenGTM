# Benchmark results: baseline

- Created: 2026-10-05T02:57:36+00:00 (took 646.6 s)
- Commit: `e5d85e1c1b45` on `rewrite/m0-baseline` (working tree dirty)
- Machine: AMD Ryzen 9 7950X 16-Core Processor, 32 logical cores, 61.9 GB RAM, Omarchy / 7.2.5-3-omarchy
- Load average (1/5/15 min) at start: 10.8, 17.3, 22.1; at end: 15.6, 14.3, 17.4
- Versions: go version go1.27.1 linux/amd64; Python 3.13.14; PostgreSQL 18.6 (Debian 18.6-1.pgdg13+2)
- PostgreSQL settings: fsync=on, synchronous_commit=on, shared_buffers=128MB, max_connections=100, wal_sync_method=fdatasync
- Single machine, shared with other processes. Cells are the **median of N repetitions**; `range` is min-max across repetitions. Read it as an indication, not a guarantee.

## Queue: claim and completion of no-op jobs

`jobs/s` is jobs completed per second over the whole drain of a pre-filled backlog. `claim` is the time of the claim call alone; `cycle` is claim plus running the no-op handler plus the guarded completion commit, per job. Times are milliseconds. Same PostgreSQL, same role, same machine for both engines.

### durable

`synchronous_commit=on` (PostgreSQL default, what production uses): every claim and every completion waits for an fsync, so this is mostly a measurement of the disk and PostgreSQL.

| engine | slots | jobs | reps | jobs/s | jobs/s range | claim p50 | claim p95 | claim p99 | cycle p50 | cycle p95 | cycle p99 | enqueue/s |
|---|---:|---:|---:|---:|---|---:|---:|---:|---:|---:|---:|---:|
| Go queue | 1 | 300 | 3 | 59.9 | 52.9-61.1 | 6.92 | 7.55 | 14.3 | 13.9 | 15.0 | 28.4 | 104 |
| Go queue | 4 | 300 | 3 | 127 | 102-134 | 14.4 | 14.5 | 16.2 | 28.7 | 29.0 | 334 | 111 |
| Go queue | 16 | 300 | 3 | 352 | 249-516 | 15.3 | 17.7 | 20.4 | 30.5 | 324 | 325 | 79.7 |
| Python QueueService (handler in-process) | 1 | 300 | 3 | 58.9 | 53.5-63.1 | 7.78 | 8.17 | 218 | 13.8 | 14.6 | 223 | 102 |
| Python QueueService (handler in-process) | 4 | 300 | 3 | 52.7 | 51.8-62.0 | 20.5 | 27.8 | 607 | 40.5 | 333 | 1,000 | 125 |
| Python QueueService (handler in-process) | 16 | 300 | 3 | 90.2 | 71.2-93.4 | 49.4 | 135 | 143 | 170 | 256 | 277 | 131 |
| Python QueueService (production child process) | 1 | 24 | 3 | 1.20 | 1.20-1.20 | 7.70 | 8.95 | 9.02 | 823 | 872 | 896 | 143 |
| Python QueueService (production child process) | 4 | 24 | 3 | 4.41 | 4.40-4.46 | 9.33 | 15.3 | 17.5 | 896 | 919 | 925 | 130 |
| Python QueueService (production child process) | 16 | 24 | 3 | 10.8 | 9.74-10.8 | 26.1 | 45.7 | 59.9 | 1,165 | 1,361 | 1,369 | 142 |

Go vs Python (in-process handler): throughput ratio (>1 means Go is faster) and cycle p50 ratio (Python / Go).

| slots | jobs/s ratio | cycle p50 ratio |
|---:|---:|---:|
| 1 | 1.02x | 0.99x |
| 4 | 2.41x | 1.41x |
| 16 | 3.91x | 5.58x |

### nosync

`synchronous_commit=off` for the benchmark role: removes the fsync wait so the numbers reflect queue code, SQL and driver cost rather than the disk.

| engine | slots | jobs | reps | jobs/s | jobs/s range | claim p50 | claim p95 | claim p99 | cycle p50 | cycle p95 | cycle p99 | enqueue/s |
|---|---:|---:|---:|---:|---|---:|---:|---:|---:|---:|---:|---:|
| Go queue | 1 | 2000 | 3 | 351 | 344-374 | 2.42 | 4.63 | 5.27 | 2.67 | 4.93 | 5.59 | 16,255 |
| Go queue | 4 | 2000 | 3 | 946 | 912-951 | 3.73 | 7.67 | 8.60 | 4.04 | 7.97 | 8.96 | 16,278 |
| Go queue | 16 | 2000 | 3 | 1,257 | 1,200-1,453 | 12.5 | 23.1 | 26.8 | 13.1 | 23.8 | 27.6 | 14,152 |
| Python QueueService (handler in-process) | 1 | 2000 | 3 | 209 | 197-212 | 2.83 | 4.11 | 4.81 | 4.80 | 6.28 | 7.19 | 1,361 |
| Python QueueService (handler in-process) | 4 | 2000 | 3 | 393 | 368-395 | 6.42 | 8.96 | 10.1 | 9.91 | 13.0 | 14.8 | 1,270 |
| Python QueueService (handler in-process) | 16 | 2000 | 3 | 371 | 353-400 | 18.2 | 29.2 | 35.1 | 42.6 | 56.4 | 65.7 | 1,174 |

Go vs Python (in-process handler): throughput ratio (>1 means Go is faster) and cycle p50 ratio (Python / Go).

| slots | jobs/s ratio | cycle p50 ratio |
|---:|---:|---:|
| 1 | 1.68x | 1.80x |
| 4 | 2.40x | 2.45x |
| 16 | 3.39x | 3.25x |

## HTTP front door

Closed-loop load generator (`apps/server/internal/bench/loadgen`), keep-alive connections, 4.0 s measured window after 1.5 s warm-up, 3 repetitions. FastAPI runs under uvicorn with 2 workers (the Dockerfile default). Access logging is enabled on both servers (written to a file), as in the default deployment. Plugins in the Go catalog: 3. `go-plugins-authed` is served from the authorization cache after the first request, so it measures the Go handler, not a FastAPI round trip.

| endpoint | conns | req/s | req/s range | p50 ms | p95 ms | p99 ms | errors |
|---|---:|---:|---|---:|---:|---:|---:|
| fastapi-health-direct | 1 | 1,121 | 1,087-1,132 | 0.84 | 1.23 | 1.58 | 0 |
| fastapi-health-direct | 16 | 2,315 | 2,293-2,335 | 6.84 | 10.1 | 12.9 | 0 |
| fastapi-health-direct | 64 | 2,288 | 2,202-2,350 | 26.4 | 30.3 | 135 | 0 |
| fastapi-health-via-go-proxy | 1 | 991 | 984-1,019 | 0.92 | 1.42 | 1.77 | 0 |
| fastapi-health-via-go-proxy | 16 | 2,303 | 2,185-2,345 | 6.69 | 9.85 | 12.3 | 0 |
| fastapi-health-via-go-proxy | 64 | 2,256 | 2,255-2,261 | 26.5 | 31.0 | 131 | 0 |
| fastapi-authed-direct | 1 | 293 | 279-300 | 3.30 | 4.16 | 4.73 | 0 |
| fastapi-authed-direct | 16 | 532 | 529-553 | 29.6 | 35.6 | 40.6 | 0 |
| fastapi-authed-via-go-proxy | 1 | 277 | 267-281 | 3.49 | 4.44 | 4.96 | 0 |
| fastapi-authed-via-go-proxy | 16 | 529 | 529-563 | 28.3 | 38.0 | 42.8 | 0 |
| go-version | 1 | 19,824 | 18,962-20,652 | 0.04 | 0.08 | 0.13 | 0 |
| go-version | 16 | 50,967 | 47,600-52,573 | 0.23 | 0.80 | 1.21 | 0 |
| go-version | 64 | 65,100 | 63,859-69,495 | 0.76 | 2.64 | 3.71 | 0 |
| go-plugins-authed | 1 | 14,320 | 13,582-14,692 | 0.06 | 0.10 | 0.19 | 0 |
| go-plugins-authed | 16 | 36,874 | 36,069-40,997 | 0.33 | 1.03 | 1.50 | 0 |
| go-plugins-authed | 64 | 53,873 | 48,846-55,484 | 1.01 | 2.95 | 4.10 | 0 |

Proxy overhead (through Go minus direct to FastAPI, medians):

| endpoint | conns | added p50 ms | req/s change |
|---|---:|---:|---:|
| /health | 1 | +0.08 | -11.6% |
| /health | 16 | -0.15 | -0.5% |
| /health | 64 | +0.08 | -1.4% |
| /api/auth/workspace-context | 1 | +0.18 | -5.4% |
| /api/auth/workspace-context | 16 | -1.36 | -0.5% |

