# Enrichment benchmark: enrich-baseline

- Created: 2026-10-05T09:47:01+00:00 (took 1103.1 s)
- Commit: `7d17d0e09165` on `rewrite/m2-enrichment`
- Machine: AMD Ryzen 9 7950X 16-Core Processor, 32 logical cores, 61.9 GB RAM, Omarchy / 7.2.5-3-omarchy
- Load average at start: 5.4, 4.9, 5.8; at end: 5.3, 5.9, 5.9 (a shared machine: read ratios, not absolutes)
- Versions: go version go1.27.1 linux/amd64; Python 3.13.14; PostgreSQL 18.6 (Debian 18.6-1.pgdg13+2)
- 3 repetition(s), engines interleaved; cells are medians, `range` is min-max.

One cell = one completed enrichment of one row by one connector: the provider call, the result and its evidence written, and for `paid` the spend reservation, dispatch and settlement. `cells/s` = rows / wall time of the job. `cpu ms/cell` is the marginal CPU time of the whole process tree (pool workers included). `xacts` are PostgreSQL commits per 1,000 cells. `RSS` is the largest process.

## nosync / free-5ms: 3000 rows, 5 ms provider latency, 12 rows and 8 provider calls in flight

`synchronous_commit=off` for the benchmark role: the flush wait is gone, so the numbers reflect code, SQL and driver cost.

| engine | wall s | cells/s | cells/s range | cpu ms/cell | xacts/1000 cells | peak RSS MB | vs python-pool |
|---|---:|---:|---|---:|---:|---:|---:|
| go | 2.19 | 1,369 | 1,362-1,373 | 0.45 | 5,013 | 45.4 | 14.50x |
| python-pool | 31.8 | 94.4 | 90.6-94.9 | 6.07 | 8,267 | 150 | 1.00x |
| python-threads | 36.5 | 82.3 | 76.7-84.0 | 8.12 | 8,273 | 153 | 0.87x |

## nosync / free-100ms: 1200 rows, 100 ms provider latency, 12 rows and 8 provider calls in flight

`synchronous_commit=off` for the benchmark role: the flush wait is gone, so the numbers reflect code, SQL and driver cost.

| engine | wall s | cells/s | cells/s range | cpu ms/cell | xacts/1000 cells | peak RSS MB | vs python-pool |
|---|---:|---:|---|---:|---:|---:|---:|
| go | 15.2 | 79.1 | 79.0-79.1 | 0.56 | 5,073 | 39.7 | 1.56x |
| python-pool | 23.7 | 50.6 | 50.6-50.7 | 6.11 | 8,294 | 140 | 1.00x |
| python-threads | 24.4 | 49.1 | 49.0-49.6 | 8.02 | 8,275 | 144 | 0.97x |

## nosync / paid-5ms: 600 rows, 5 ms provider latency, 12 rows and 8 provider calls in flight

`synchronous_commit=off` for the benchmark role: the flush wait is gone, so the numbers reflect code, SQL and driver cost.

| engine | wall s | cells/s | cells/s range | cpu ms/cell | xacts/1000 cells | peak RSS MB | vs python-pool |
|---|---:|---:|---|---:|---:|---:|---:|
| go | 0.94 | 636 | 537-638 | 0.90 | 7,060 | 38.4 | 11.09x |
| python-pool | 10.5 | 57.4 | 55.9-60.4 | 9.70 | 14,506 | 138 | 1.00x |
| python-threads | 11.5 | 52.2 | 51.7-53.1 | 12.0 | 14,508 | 141 | 0.91x |

## nosync / free-5ms-wide: 3000 rows, 5 ms provider latency, 24 rows and 24 provider calls in flight

`synchronous_commit=off` for the benchmark role: the flush wait is gone, so the numbers reflect code, SQL and driver cost.

| engine | wall s | cells/s | cells/s range | cpu ms/cell | xacts/1000 cells | peak RSS MB | vs python-pool |
|---|---:|---:|---|---:|---:|---:|---:|
| go | 1.04 | 2,875 | 2,805-3,029 | 0.60 | 5,024 | 53.6 | 37.74x |
| python-pool | 39.4 | 76.2 | 74.9-78.0 | 6.98 | 8,219 | 149 | 1.00x |
| python-threads | 41.3 | 72.7 | 72.0-73.7 | 9.05 | 8,225 | 153 | 0.95x |

## durable / free-5ms: 600 rows, 5 ms provider latency, 12 rows and 8 provider calls in flight

`synchronous_commit=on` (the PostgreSQL default): every commit waits for an fsync, so on this disk both engines are bounded by flushes and fewer commits per cell is the only way to win.

| engine | wall s | cells/s | cells/s range | cpu ms/cell | xacts/1000 cells | peak RSS MB | vs python-pool |
|---|---:|---:|---|---:|---:|---:|---:|
| go | 4.36 | 138 | 96.3-156 | 0.61 | 5,075 | 38.5 | 2.95x |
| python-pool | 12.9 | 46.6 | 44.7-56.0 | 5.82 | 8,277 | 137 | 1.00x |
| python-threads | 11.9 | 50.5 | 48.0-53.0 | 8.05 | 8,277 | 141 | 1.08x |

## durable / paid-5ms: 200 rows, 5 ms provider latency, 12 rows and 8 provider calls in flight

`synchronous_commit=on` (the PostgreSQL default): every commit waits for an fsync, so on this disk both engines are bounded by flushes and fewer commits per cell is the only way to win.

| engine | wall s | cells/s | cells/s range | cpu ms/cell | xacts/1000 cells | peak RSS MB | vs python-pool |
|---|---:|---:|---|---:|---:|---:|---:|
| go | 0.95 | 210 | 200-270 | 1.14 | 7,181 | 36.3 | 4.89x |
| python-pool | 4.65 | 43.0 | 38.1-48.5 | 9.97 | 14,432 | 137 | 1.00x |
| python-threads | 4.52 | 44.3 | 29.7-46.2 | 11.8 | 14,503 | 140 | 1.03x |

