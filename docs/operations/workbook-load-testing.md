# Workbook controlled-load gate

Run this only against an isolated, migrated PostgreSQL database. It creates one
namespaced workbook, bulk-loads deterministic rows, analyzes the table, and
measures stable first/last/custom-sort pages, literal JSON search, and an exact
selection spanning distant pages. The synthetic workbook is deleted afterward.

```bash
DATABASE_URL=postgresql+psycopg://... \
uv run python -m apps.api.cli workbook-load-test \
  --rows 1000000 --page-size 100 \
  --confirm "RUN WORKBOOK LOAD TEST"
```

Valid supported-scale evidence requires `dialect: postgresql`, `rows: 1000000`
or greater, `ok: true`, `selection_exact: true`, one search match, and every
reported latency within its threshold. `--allow-sqlite` only tests the harness.

Set `OPENGTM_BUILD_SHA` before the run, save the JSON report, and attest it with
the release secret:

```bash
OPENGTM_SCALE_ATTESTATION_KEY=... \
uv run python scripts/attest_scale_report.py \
  --input artifacts/workbook-scale.json \
  --output artifacts/workbook-scale.attested.json
```

Deploy the attested path as `OPENGTM_WORKBOOK_SCALE_REPORT`. Evidence expires
after 92 days and must match the exact deployed build.
