# Benchmark results: retention

- Created: 2026-10-05T08:44:35+00:00 (took 451.1 s)
- Commit: `f5c30508fa82` on `rewrite/m6-job-ports`
- Machine: AMD Ryzen 9 7950X 16-Core Processor, 32 logical cores, 61.9 GB RAM, Omarchy / 7.2.5-3-omarchy
- Load average (1/5/15 min) at start: 7.2, 7.2, 6.3; at end: 7.0, 7.2, 6.7
- Versions: go version go1.27.1 linux/amd64; Python 3.13.14; PostgreSQL 18.6 (Debian 18.6-1.pgdg13+2)
- PostgreSQL settings: fsync=on, synchronous_commit=on, shared_buffers=128MB, max_connections=100, wal_sync_method=fdatasync
- Single machine, shared with other processes. Cells are the **median of N repetitions**; `range` is min-max across repetitions. Read it as an indication, not a guarantee.

## retention_enforce: Go and Python

One workspace per case, seeded with `rows` expired and 100 recent rows in **each** of the 8 tables the job purges (`benchmarks/retention_seed.sql`), default 365/180-day windows, so a run deletes `8 x rows` rows. Wall time is milliseconds from just before the handler is called (handler rows) or from the job insert (whole-job rows) until the run is `completed` and verified; seeding is excluded. Same PostgreSQL, same NOSUPERUSER NOBYPASSRLS role, engines interleaved per repetition, every case starts from empty tables. The Python whole-job rows include the child process the production queue spawns for every job (interpreter start and the full handler-registry import); the Go whole-job rows use the real queue with a 5 ms idle poll. Cells are medians; the range is min-max across repetitions.

### durable

`synchronous_commit=on` (PostgreSQL default, what production uses): every claim and every completion waits for an fsync, so this is mostly a measurement of the disk and PostgreSQL.

| engine | expired rows per table | rows deleted | reps | wall ms (median) | wall ms range | rows deleted/s |
|---|---:|---:|---:|---:|---|---:|
| Python handler (`handle_retention_enforce`, in-process) | 1,000 | 8,000 | 3 | 13.3 | 13.0-14.0 | 602,965 |
| Go handler (`Worker.Handle`) | 1,000 | 8,000 | 3 | 17.7 | 16.6-23.0 | 451,520 |
| Python, whole job (claim, child process, handler, finalize) | 1,000 | 8,000 | 3 | 819 | 767-931 | 9,764 |
| Go, whole job (claim, handler, finalize) | 1,000 | 8,000 | 3 | 22.0 | 21.8-29.1 | 363,997 |
| Python handler (`handle_retention_enforce`, in-process) | 20,000 | 160,000 | 3 | 60.6 | 55.4-61.4 | 2,640,471 |
| Go handler (`Worker.Handle`) | 20,000 | 160,000 | 3 | 290 | 286-1,907 | 551,110 |
| Python, whole job (claim, child process, handler, finalize) | 20,000 | 160,000 | 3 | 947 | 880-1,005 | 169,034 |
| Go, whole job (claim, handler, finalize) | 20,000 | 160,000 | 3 | 332 | 313-764 | 482,109 |
| Python handler (`handle_retention_enforce`, in-process) | 100,000 | 800,000 | 3 | 292 | 284-391 | 2,739,111 |
| Go handler (`Worker.Handle`) | 100,000 | 800,000 | 3 | 1,168 | 1,078-1,193 | 684,857 |
| Python, whole job (claim, child process, handler, finalize) | 100,000 | 800,000 | 3 | 1,252 | 1,200-1,728 | 638,971 |
| Go, whole job (claim, handler, finalize) | 100,000 | 800,000 | 3 | 1,078 | 1,061-1,332 | 742,146 |

Python / Go wall-time ratio of the medians (>1 means Go is faster):

| expired rows per table | handler | whole job |
|---:|---:|---:|
| 1,000 | 0.75x | 37.28x |
| 20,000 | 0.21x | 2.85x |
| 100,000 | 0.25x | 1.16x |

### nosync

`synchronous_commit=off` for the benchmark role: removes the fsync wait so the numbers reflect queue code, SQL and driver cost rather than the disk.

| engine | expired rows per table | rows deleted | reps | wall ms (median) | wall ms range | rows deleted/s |
|---|---:|---:|---:|---:|---|---:|
| Python handler (`handle_retention_enforce`, in-process) | 1,000 | 8,000 | 3 | 10.9 | 10.1-11.4 | 733,279 |
| Go handler (`Worker.Handle`) | 1,000 | 8,000 | 3 | 15.5 | 15.3-15.8 | 516,314 |
| Python, whole job (claim, child process, handler, finalize) | 1,000 | 8,000 | 3 | 860 | 843-881 | 9,297 |
| Go, whole job (claim, handler, finalize) | 1,000 | 8,000 | 3 | 22.2 | 18.1-23.0 | 360,481 |
| Python handler (`handle_retention_enforce`, in-process) | 20,000 | 160,000 | 3 | 60.4 | 56.7-531 | 2,648,244 |
| Go handler (`Worker.Handle`) | 20,000 | 160,000 | 3 | 290 | 280-294 | 550,960 |
| Python, whole job (claim, child process, handler, finalize) | 20,000 | 160,000 | 3 | 948 | 875-996 | 168,708 |
| Go, whole job (claim, handler, finalize) | 20,000 | 160,000 | 3 | 311 | 306-318 | 514,209 |
| Python handler (`handle_retention_enforce`, in-process) | 100,000 | 800,000 | 3 | 325 | 277-486 | 2,460,653 |
| Go handler (`Worker.Handle`) | 100,000 | 800,000 | 3 | 1,189 | 1,112-1,502 | 673,077 |
| Python, whole job (claim, child process, handler, finalize) | 100,000 | 800,000 | 3 | 1,339 | 1,160-1,694 | 597,345 |
| Go, whole job (claim, handler, finalize) | 100,000 | 800,000 | 3 | 1,018 | 1,013-1,115 | 785,614 |

Python / Go wall-time ratio of the medians (>1 means Go is faster):

| expired rows per table | handler | whole job |
|---:|---:|---:|
| 1,000 | 0.70x | 38.77x |
| 20,000 | 0.21x | 3.05x |
| 100,000 | 0.27x | 1.32x |

