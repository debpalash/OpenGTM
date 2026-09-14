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
  --hold-ms 10 --output artifacts/queue-scale.json \
  --confirm "RUN QUEUE LOAD TEST"
```

The JSON report is valid evidence only when `dialect` is `postgresql` and `ok`
is true. It reports throughput, p50/p95/p99 claim latency, duplicate claims,
per-tenant peak activity, cap violations, thread failures, and unfinished jobs.
Any invariant breach makes the command fail non-zero. `--allow-sqlite` exists
only to regression-test the harness and must not be used for a supported-scale
claim.

For release evidence, run at least 10,000 jobs across 100 tenants with at least
32 claimers and set `OPENGTM_BUILD_SHA`. `--output` atomically writes clean JSON
even when startup logs are present. Attest the saved report:

```bash
OPENGTM_SCALE_ATTESTATION_KEY=... \
uv run python scripts/attest_scale_report.py \
  --input artifacts/queue-scale.json \
  --output artifacts/queue-scale.attested.json
```

Deploy the attested path as `OPENGTM_QUEUE_SCALE_REPORT`. Release readiness
requires both distinct scale reports to be current, signed, PostgreSQL-backed,
and bound to the deployed build.
