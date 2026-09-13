# Durable queue controlled-load gate

Run this only against an isolated, migrated PostgreSQL environment. The gate
refuses to start if any pending or processing jobs already exist, namespaces
every synthetic job, restricts claimers to that namespace, and deletes only its
own rows after every claimer has stopped. If a claimer cannot stop, the command
preserves its tagged rows for diagnosis instead of racing a cleanup; use the
reported `run_tag` to identify them.

```bash
DATABASE_URL=postgresql+psycopg://... \
uv run python -m apps.api.cli queue-load-test \
  --jobs 10000 --tenants 100 --claimers 32 --tenant-cap 2 \
  --hold-ms 10 --confirm "RUN QUEUE LOAD TEST"
```

The JSON report is valid evidence only when `dialect` is `postgresql` and `ok`
is true. It reports throughput, p50/p95/p99 claim latency, duplicate claims,
per-tenant peak activity, cap violations, thread failures, and unfinished jobs.
Any invariant breach makes the command fail non-zero. `--allow-sqlite` exists
only to regression-test the harness and must not be used for a supported-scale
claim.
