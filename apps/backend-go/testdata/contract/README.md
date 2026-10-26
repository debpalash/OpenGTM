# REST contract fixtures

Golden request/response pairs **recorded from the FastAPI reference** (never hand-written),
compared by `internal/contract` (`contract.Load`, `contract.AssertEqual`).
Layout: `testdata/contract/<area>/<case>.json`, one directory per case file
(`auth/` from `legacy/python/scripts/contract_cases/auth.yaml`).

Fixture: `{"request": {method, path, headers, body}, "response": {status, headers, body}}`.
`Authorization` / `X-Workspace-Id` request values and issued `access_token`/`refresh_token`
values are stored as `"<redacted>"`; response headers are limited to
`content-type`, `www-authenticate`, `allow`. Compare generated values with
`IgnorePaths` / `IgnoreShapeOf` (e.g. `$.last_login`, `$.access_token`).

## Record procedure

Needs the Postgres test container on 127.0.0.1:55480 and a throwaway database. The seed script
TRUNCATEs users and identity tables (CASCADE) and refuses unless the host is loopback or a unix
socket, the database name matches `^opengtm_contract(_[a-z0-9]+)?$`, and `CONTRACT_SEED_CONFIRM=yes`.

```sh
docker exec opengtm-go-test-pg psql -U postgres -c "CREATE DATABASE opengtm_contract"   # once
cd legacy/python
export DATABASE_URL=postgresql+psycopg://postgres:postgres@127.0.0.1:55480/opengtm_contract \
       SQLITE_STORES_BACKEND=postgres SECRET_KEY=<test-only-value> APP_ENV=test \
       PG_RLS_REQUIRE_SAFE_ROLE=false   # test DB role is a superuser; FastAPI refuses it otherwise
uv run alembic upgrade head
CONTRACT_SEED_CONFIRM=yes uv run python scripts/contract_seed.py   # fixed users alice/bob/carol(inactive), ws-alpha/ws-beta
uv run uvicorn apps.api.main:app --port 18000 &
python3 scripts/contract_record.py --base http://127.0.0.1:18000 \
    --out ../../apps/backend-go/testdata/contract --cases scripts/contract_cases/auth.yaml
kill %1
```

Case files are YAML (PyYAML) or JSON (same structure, `.json` extension). Re-seed before every
recording so ids and `created_at` are stable; `last_login` and tokens are not.
Seed credentials are in `legacy/python/scripts/contract_seed.py` (test-only values).
Review `git diff` of the fixtures after re-recording: a changed fixture is a changed contract.

## Deliberate deviations from FastAPI

- `POST /api/workspaces` with a name whose slug already exists: FastAPI answers 500 (unhandled
  `IntegrityError`); Go answers `409 {"detail":"Workspace slug already exists"}` (user decision,
  Task 11). No fixture exists for this case; it is covered by `TestCreateSlugCollision409`.
- The recorder (`contract_record.py`) can reference earlier responses in a case path or JSON body with
  `{{body:<case>.<dotted.path>}}` (used by `workspaces.yaml` for generated workspace ids).
- `PUT`/`PATCH /scim/v2/{slug}/Groups/{id}` renaming a group onto an existing displayName: FastAPI
  answers 500 (unhandled `IntegrityError`); Go answers `409 {"detail":"SCIM group already exists"}`
  like `POST`. Covered by `TestDuplicateDisplayName409`, no fixture.
- `POST /Groups` with a non-numeric member `value` answers `409 SCIM group already exists` in both
  (FastAPI's blanket `except Exception` swallows the 400); PUT/PATCH answer 400. Fixture
  `scim_groups/create_nonnumeric_member`.
- `GET /api/settings/llm-usage`: FastAPI reads `db.get_llm_usage()` from the lead store, and the shared
  PostgreSQL lead store does not persist LLM usage (it answers an empty list). Go reads
  `llm_usage_daily` (tenant table, FORCE RLS) instead, so a populated table yields real rows where FastAPI
  returns none: a deliberate improvement. Fixtures `llm_usage_empty` / `llm_usage_with_date` record the
  empty FastAPI answer; populated behaviour is covered by `TestLLMUsageTenantIsolation`.
- `PUT /api/settings/enrichment-perf` returns the stored values without the `defaults` key (only GET
  includes it); Go matches the recorded fixture `perf_put_clamp`, not the plan text.
- `GET /api/settings/system`: an environment variable that is set but empty counts as unset (FastAPI keeps
  the empty string; a non-integer `API_PORT` is a 500 in both).
- `settings.yaml` fixtures need `ENRICH_*` and enrichment API keys absent from `app_settings` and no
  `DEBUG`/`API_*`/`CORS_ORIGINS`/`GOOGLE_API_KEY`/`LINKEDIN_LI_AT_COOKIE` in the FastAPI environment.
- `GET /api/entities/company` and `/api/entities/review-queue` with a negative `limit`: FastAPI only bounds
  `le=1000`, so Postgres rejects `LIMIT -1` and it answers 500; Go answers 422 `greater_than_equal`
  (`ctx {"ge": 0}`, loc `["query","limit"]`), a user-mandated crash-bug hardening. No fixture; covered by
  `TestNegativeLimit422WithCtx`.
- `POST /api/functions/{id}/apply` does not run cycle validation (existing FastAPI gap, user decision: keep
  parity): applying a chain that closes a dependency cycle succeeds in both. `GET /api/functions` ties on
  `usage_count` are ordered `created_at, id` in Go (FastAPI leaves them in heap order). Covered by
  `TestApplyDoesNotValidateCycles`; fixtures under `functions/`.

## Files / system routes (Task 18): hardened-mode deviations

`FILES_API_MODE=hardened` (default) deviates from FastAPI deliberately; `FILES_API_MODE=legacy` keeps exact parity
(fixtures `system/*`, recorded with FastAPI, are asserted in legacy mode only).
- All four routes require a token; `/api/system/stats`, `list`, `download`, `delete` all require a global admin.
- Paths: no absolute paths, no `..` segment, symlinks resolved and contained (`filepath.Rel`), dangling symlinks refused,
  downloads opened with `O_NOFOLLOW` plus an `/proc/self/fd` containment re-check, delete re-verifies the parent.
- `DELETE` never recurses: non-empty directory -> 400 `Directory is not empty`; the data dir itself -> 400
  `Cannot delete the data directory`; missing target -> 404 `File not found or could not be deleted` (FastAPI answers 500
  `404: File not found or could not be deleted`, an artefact of its blanket `except`).
- Unexpected failures -> 500 `Internal error` (FastAPI returns `str(e)`).
- `GET /api/files/list` is capped at 5000 entries (newest per group kept); when cut, response header
  `X-Files-Truncated: true` is set (body stays a bare array). Legacy is unbounded.
- `GET /api/system/stats`: `project_size` walks `DATA_DIR` only (FastAPI walks its parent, `/` for `/data`), capped at
  100,000 entries / 5 s; when capped the body gains `project_size_partial: true` (key absent otherwise). Result cached 30 s.
  A client-cancelled request is not logged.
- Listing order keeps the Python quirk: files before folders, each group by `modified` descending.

## Workbook row cursors (Task 8): deviations
- Row ordering uses `coalesce(position, 0)`. FastAPI orders raw `position` (NULLs last in offset mode) and its cursor
  predicate `position > cursor` silently drops NULL-position rows after page 1; Go pages them without skips or
  duplicates. Only legacy rows with a NULL `position` are affected.
- Cursor compatibility during an nginx canary: row cursors and short query cursors (v1) issued by either stack decode on
  the other. Long-value cursors (v2, issued when a sort value makes the v1 cursor exceed 512 characters, roughly 380+
  bytes) are bound to a scope hash of the issuing stack's SQL, so an in-flight v2 cursor answers 400
  `Invalid workbook row cursor; restart pagination` on the other stack. Clients restart pagination.
- Connector-run cursors keep `created_at.isoformat()` of the timezone-aware column (`+00:00`, microseconds only when non-zero).
- Entity merge/split/decide (`entities_write/`, Task 17): the base seed's entities keep provenance as
  `{"name": ...}` (not field -> list), which Python's merge cannot fold (TypeError, HTTP 500); the cases use
  the extra seed `scripts/contract_seed_entities_write.py`. Go answers `400 invalid_entity_fields` for such
  rows and writes nothing. A merge naming an id of another workspace answers `400 entity_not_found` in Go
  (Python: `cross_workspace_merge_forbidden`, which confirms a foreign id exists); no fixture, covered by
  `TestMergeCrossWorkspaceIDs400NothingWritten`. `kept.sources` keeps first-seen order (Python: set order);
  decide commits the merge and the pair status together (Python: two commits).

## Workbook create/list/update/delete/from-jobs (Task 9)

Fixtures `workbooks/*` (cases `legacy/python/scripts/contract_cases/workbooks.yaml`). FastAPI only snapshots leads
when its lead store is Postgres, which needs a **non-superuser** connection role (the superuser fallback silently
uses an empty SQLite store): run uvicorn as a `NOSUPERUSER NOBYPASSRLS` login role that is a member of `yupcha_app`
and set `PG_LEAD_STORE=true`; seed and record as the superuser. After `contract_seed.py`, insert three extra ws-alpha
leads (`collection_job_id` `job-1` twice, `job-2` once) before recording.
- `POST /api/workbooks/from-jobs` with `workbook_id`: FastAPI mutates the loaded `filter_criteria` dict in place and
  reassigns it, which SQLAlchemy treats as unchanged, so the merged `job_ids` are only persisted when the stored
  filter was empty/NULL (fixtures `from_jobs_merge`, `from_jobs_merge_again`). Go keeps this behaviour (parity);
  `updated_at` is always stamped. `TestFromJobsMergeKeepsPythonPersistenceQuirk`.
- `POST /from-jobs` without `name`: FastAPI looks the job queries up in the per-workspace SQLite `jobs` table; Go is
  Postgres-only, so the name is always `Task Workbook (N jobs)` (the fixture state of a PG deployment).
- `POST /from-jobs` with a `name` longer than 255 characters: FastAPI answers 500 (varchar(255) overflow); Go answers
  422 `string_too_long` (crash hardening). No fixture.
- `POST /api/workbooks/` with `source: csv` and a non-list `source_config.rows`, or `source: job_results` with a
  non-list `job_ids`: FastAPI answers 500; Go creates the workbook with no rows. No fixture
  (`TestCreateCsvTruncatesToMaxRows`).
- Request bodies are limited to 64 MiB (413 `Request body too large`); FastAPI has no limit.
- Invalid-JSON bodies answer the same simplified `json_invalid` entry as the other Go routes (`loc ["body",0]`, no `ctx`).

## Column routes (Task 11): deviations

Fixtures `columns/*` (cases `legacy/python/scripts/contract_cases/columns.yaml`, FastAPI as a non-superuser role).
- `DELETE /columns/{id}` with a saved view whose `config` is malformed (null `filters`/`sort`, non-object rule, scalar
  config): FastAPI raises (500); Go treats it as "no reference" (hardening). Views are listed `created_at, id` (FastAPI:
  heap order). Covered by `TestDeleteBlockedByView`.
- Go escapes U+2028/U+2029 in JSON strings (valid, equivalent JSON); Starlette emits them raw.
- Request bodies over 4 MiB answer 413 `Request body too large` (FastAPI: unbounded).
- `wbcrud.FastAPIModelErrors` converts `NormalizeRequestColumn` errors to FastAPI's body form (`model_attributes_type`,
  `ctx` bounds); the create route should apply it too (fixtures `columns/add_column_null`, `add_bad_types`).

## Row routes (Task 12): deviations

Fixtures `rows/*` and `rows_recompute/*` (cases `legacy/python/scripts/contract_cases/rows.yaml` and
`rows_recompute.yaml`, FastAPI as a non-superuser role; record `rows_recompute.yaml` after `rows.yaml`, the Python worker
rewrites enrichments asynchronously). The fixture writer sorts JSON keys, so `TestRowsMatchFixtures` re-sends the original
key order (`bodyOverrides`) for the few bodies whose response or stored text depends on it; the final database state of the
whole replay is compared with the state FastAPI left (`testdata/wbcrud/rows_final_state.json`), including the stored `json`
text (Python `json.dumps` spacing and `ensure_ascii`, `PyDumps`). `TestRowRecomputeEnqueuesRunJob` compares the queued
`run_workbook` payload with FastAPI's.
- `POST /rows` with a row whose `id` is not an integer: FastAPI answers 500 (the value goes to the integer `lead_id`
  column); Go stores the row with `lead_id` NULL. A non-string `website` / `domain` / `company` is ignored for the dedupe
  identity (FastAPI: 500). No fixture; `TestAddRowsAppendsPositions`.
- `PUT /{id}/leads/{lead_id}` with values the database driver rejects (non-numeric `score`, null `company`, list/object
  values, out-of-range score): FastAPI answers 500; Go answers 400 `Invalid value for lead field <name>`. Values the driver
  coerces still work (`"7"` and `7.5` for `score` round half-even, numbers/booleans as text). `PROVENANCE_TRACKING_ENABLED`
  (default off) is not ported: a manual edit does not merge `field_provenance`. `TestUpdateLeadBadValuesAreClientErrors`.
- `delete-query` re-checks the live count against the number of rows the DELETE removed and rolls everything back with a
  409 if a concurrent writer changed it (FastAPI loads every id first). Go also locks the workbook row for add/delete, so
  concurrent Go writers cannot assign duplicate positions.
- Request bodies are limited to 32 MiB (413 `Request body too large`); FastAPI has no limit on `POST /rows`.
- Recompute after `PATCH rows` / `PATCH rows/{id}` (`recompute` defaults to true): like FastAPI, the row data is committed
  first, then a second transaction does `run_workbook`'s work: cycle / changed-rows guards (422 / 409 after the data
  commit, as in Python), `status='running'` + progress reset (`updated_at` stamped), supersede the in-flight `run_workbook`
  job of the workbook, and enqueue ONE `run_workbook` job (executor `python`, payload key order as Python) via
  `queue.Enqueue`. Not ported (plan 05, retained `/run`): the platform-credit debit when `BILLING_ENABLED` is on (then
  `recompute` is `{"status":"unavailable", ...}` and nothing is enqueued, so runs are never unmetered), Python's Go-executor
  eligibility (`GO_EXECUTOR_WORKSPACES`; Go always queues for the Python worker) and slowapi's 20/min limit on the inner
  `run_workbook` call.
- Invalid-JSON bodies answer the same simplified `json_invalid` entry as the other Go routes (`loc ["body",0]`, no `ctx`).
- Not a deviation but worth knowing: `RowQuery.CountSQL` passed the sort-term parameters of a sorted saved view to a
  statement that does not reference them (pgx: `expected N arguments`); fixed with `RowQuery.whereArgs`
  (`TestDeleteQueryWithSortedViewAndSearch`).

## Saved views (Task 14): deviations

Fixtures `views/*` (cases `legacy/python/scripts/contract_cases/views.yaml`, FastAPI as a non-superuser role).
- A view whose stored `config` is NULL or does not parse as a `ViewConfig`: FastAPI answers 500 on the list/PUT
  response model; Go shows it as the empty default config (`{"filters":[],"sort":[],"hidden_columns":[]}`) so one bad
  row cannot take down the switcher. Views list `created_at, id` (as Task 11).
- `X-Workspace-Id` of another workspace or another workspace's workbook/view ids answer 404 and write nothing.
- Request bodies over 4 MiB answer 413 `Request body too large`.

## Workbook detail, CSV export, connector runs (Task 10): deviations

Fixtures `detail/*` (cases `legacy/python/scripts/contract_cases/detail.yaml`; seed `contract_seed.py` then
`contract_seed_detail.py`, FastAPI as a non-superuser role with `PG_LEAD_STORE=true`). Go tests load the same data from
`testdata/wbcrud/detail_seed.json` (`dump_contract_seed.py`). CSV bodies are recorded as a JSON string and compared byte
for byte (BOM, `\r\n`, quoting); fixtures are key-sorted, so response key order is asserted by
`TestDetailKeyOrderMatchesPydantic`.
- `GET /{id}?page=<huge>`: FastAPI passes the offset to Postgres (an offset past bigint is a 500); Go answers an empty page
  for any offset that overflows and 422 `less_than_equal` (`le` 9223372036854775807) for a `page` beyond int64.
- An enrichment overlay whose fields have the wrong type (`status` not a string, `provenance`/`research` not matching their
  models) makes FastAPI's response model fail (500); Go reads the bad field as `null` (`status` as `"pending"`).
- A legacy `leads_filter` workbook whose `filter_criteria` does not decode as a lead filter reads as an unfiltered lead
  query.
- Export: a failure after the first byte (database error, vanished client) aborts the connection instead of ending the
  chunked body, so a truncated file never looks complete. Ids in `row_ids` beyond int64 are accepted and match no row.
- Ordered bodies are written without `encoding/json`, which would re-escape U+2028/U+2029; the workbook object inside the
  detail response still goes through it (a workbook name containing U+2028 is escaped, Python's is not).
- Long-value (v2) row cursors are not portable between the stacks (Task 8); `connector-runs` cursors are.

## Lead timeline (plan 05 Task 8): deviations

- `GET /api/lead/{id}/timeline`: an outreach send with NULL `step_number` answers description "Sequence step 1" and `metadata.step_number` null. Python raises `TypeError` (`None + 1`) and answers 500 (crash hardening).
- Naive timestamp strings on a lead (`created_at`/`updated_at`/`last_enriched_at`) are read as UTC. Python's `fromisoformat(...).timestamp()` uses the process-local zone, so parity assumes `TZ=UTC` (containers set no TZ and default to UTC).
- Timeline fixtures `timeline/*` (hot, lead6, empty_lead, beta_as_bob) were re-recorded with `TZ=UTC` (Task 11): the first recording ran on an IST machine and baked a +05:30 shift into the naive lead timestamps. Record any timestamp-dependent fixture with `TZ=UTC`.
- `GET /api/lead/{id}/timeline` (Task 11): `before` accepts pydantic lax floats (whitespace, single underscores, `inf`/`nan`); `before=nan` filters every item out. Body is written with raw non-ASCII (U+2028/U+2029 included), like Starlette. No deviations from Python otherwise.

## CSV import preview and import (Task 13): deviations

Fixtures `import/*` (cases `legacy/python/scripts/contract_cases/import.json`, generated by `scripts/gen_import_cases.py`;
FastAPI as a non-superuser role; final state golden `testdata/wbcrud/import_final_state.json` from `scripts/dump_import_state.py`).
- Request bodies over 64 MiB answer 413 `Request body too large` (FastAPI: unbounded). The body is decoded as a stream (never
  buffered whole), so memory stays proportional to the decoded rows; a 50,000-row import peaks at ~80 MiB for an 8 MiB body
  (`TestImportMemoryBounded`).
- The 50,001-row 422 (`too_long`, input echoes the list) has no fixture (it would be a 300 KB file); its shape is pydantic's own
  output and is asserted by `TestImport50001Rows422`.
- `website` / `domain` / `company` values that are not strings count as absent when computing the dedupe identity (FastAPI raises,
  500); same rule as add rows.
- A stored column without an `id` that matches an imported header resolves to the empty target (FastAPI: `KeyError`, 500).
- Python rescans `name_2`, `name_3`, ... from 2 for every colliding header (quadratic for thousands of collisions); Go resumes from
  the last free suffix. Same result, linear time.
- The automation hook (`on_row_added`) runs on the import transaction (FastAPI emits after commit); it is skipped when no row was
  inserted, like `emit_row_added` with an empty id list. A hook failure is logged and swallowed.
- `workbooks.total_rows` is not touched by import (as in FastAPI); `updated_at` is stamped by every successful import
  (`source_config.last_csv_import.imported_at` always changes) and by nothing else: a refused import (400/422/404) writes nothing.
- No cycle/identity guard runs: imported columns are plain `lead_field` columns with fresh ids (FastAPI does not validate either).
- Python's `str.lower` / `str.casefold` / `str.title` mappings for header normalisation come from `internal/wbcrud/pycase_tables.txt`,
  generated from the real interpreter by `scripts/gen_csv_vectors.py` (every code point is checked in `csvvectors_test.go`).

## Lead read routes (plan 05 Task 9): deviations
Fixtures `leads/*` (list, stats, filters, get, similar), cases `legacy/python/scripts/contract_cases/leads.yaml`. The Go replay seeds
its leads from `testdata/leads/seed.json` (generated from the constants of `contract_seed.py`; ids 1-18).
- `GET /api/leads?limit=` above 10,000 (including values beyond int64) is capped to 10,000 (D2). A negative `limit` or `offset`
  answers 422 `greater_than_equal` (FastAPI passes it to Postgres, which fails with a 500). An `offset` beyond int64 is an empty page.
- `score_min` / `score_max` outside int4 are clamped to the int4 range (FastAPI answers 500 from Postgres); the filter keeps its meaning.
- `GET /api/lead/{id}` and `/similar` with an id beyond int64 answer 404 `Lead not found` (FastAPI: 500). A `limit` above 10,000 on
  `/similar` is capped to 10,000 (the answer is the same: no workspace page holds more).
- `workspace_id` on `/api/leads` is accepted and ignored: the caller's workspace always applies (FastAPI behaves the same).
- JSON bodies keep U+2028 / U+2029 raw, as Python's `ensure_ascii=False` does.

## Budget, cost, runs, activity, cell trace (Task 19): deviations

Fixtures `telemetry/*` (cases `legacy/python/scripts/contract_cases/telemetry.yaml`; seed `contract_seed.py`, then
`psql -v ON_ERROR_STOP=1 < testdata/wbcrud/telemetry_extra.sql` as a superuser, FastAPI as a non-superuser role). The Go
tests load the same extras from that file (`-- @ws <id>` sections run inside `InWorkspace`); activity ids are mapped to the
1..6 FastAPI's fresh database assigned (the shared test database's serial is not reset). Expected values of the number /
helper vectors (`telemetry_vectors.json`) come from the real Python (`scripts/dump_telemetry_vectors.py`).
- `GET /{id}/cost` with a positive cap and any `reserved` / `dispatched` / `uncertain` spend row: FastAPI answers 500
  (`SUM(bigint)` is a `Decimal`, `float - Decimal` raises `TypeError`); Go answers the figure
  (`remaining_usd = round(cap - spent - reserved - uncertain, 6)`). `reserved_usd` / `uncertain_usd` keep the Decimal
  serialisation quirk: `0.0` when no row, an integer when the sum is a whole number of USD (`1`, not `1.0`), else a float
  (`0.002`, `1e-06`). Covered by `TestCostCappedWithExposureAnswers200NotPython500`; no fixture.
- `GET /{id}/runs?before_id=` >= 2**31 and `GET /{id}/rows/{lead_id}/cells/{col}/trace` with `lead_id` outside int4: FastAPI
  answers 500 (integer out of range); Go answers 422 `less_than_equal` (`le` 2147483647) / `greater_than_equal` (`ge`
  -2147483648). No fixtures.
- `GET /{id}/activity?limit=<negative>`: FastAPI bounds only `le=500`, a negative `LIMIT` is a Postgres error (500); Go answers
  422 `greater_than_equal` (`ge` 0), as for entities. Rows tied on `created_at` are ordered by `id` desc (FastAPI: unspecified).
- `PUT /{id}/budget` body with a JSON `NaN` / `Infinity` literal: FastAPI parses it, then fails to serialise its own 422 (500);
  Go answers the simplified `json_invalid` 422. Strings are parsed with pydantic's lax rules (`"7.5"`, `" 5 "`, `"1_0"`;
  `"inf"` / `"nan"` -> `finite_number`), booleans count as 1.0 / 0.0, as recorded. `updated_at` is stamped only when the cap
  really changes (NULL -> 0.0 included), like an ORM attribute assignment.
- A `run_workbook` job whose payload is not a JSON object (never written by the queue): FastAPI raises `AttributeError` in the
  runs list, Go reads it as an empty payload; a scalar payload never matches (`->>` on a scalar would raise in Postgres).
- `selected_cell_count` / `output_attempts` / `result` follow `workbooks.py` exactly (vectors); `execution_result` is shown only
  when its `retry_count` equals the job's (`NULL` counts as 0, `1.0` and `true` equal 1).

### CSV import hardening (Task 13 fix round 1)
- JSON nesting deeper than 200 levels answers 422 `json_invalid` on every body route that uses the shared ordered decoder
  (`decodeOrdered`); Python's `json` raises `RecursionError` near 1000 levels (500). Without a bound a `[[[[...` body kills the Go process.
- Import decoding stops at row 50,001: the 422 `too_long` reports `actual_length` 50001 (FastAPI reports the real count). Per-element
  errors for `rows` / `mapping` are capped at 100 each (pydantic lists all).
- Upload budget: `/import` and `/import/preview` reserve `Content-Length` bytes (the 64 MiB cap when chunked) from a per-process
  budget `IMPORT_MEMORY_BUDGET_MB` (default 512, range 64..65536) before reading the body, after auth and the editor gate. When it
  stays full for 5 s the answer is `503 {"detail":"Too many imports in progress"}` with `Retry-After: 5`. A decoded import holds
  roughly ten times its body size. The 30 s server read timeout still bounds slow uploads.
- JSON body edge cases differ from Python's `json.loads`: a UTF-8 BOM, UTF-16/32 bodies and `NaN`/`Infinity` (accepted by Python) are 422
  in Go; invalid UTF-8 is replaced by U+FFFD (Python: 422).

## Analytics routes (Task 21): PG lead store only

Fixtures `analytics/*` (cases `legacy/python/scripts/contract_cases/analytics.yaml`, FastAPI as a non-superuser role with
`PG_LEAD_STORE=true`). Only the Postgres-store branch of `routers/analytics.py` is ported; nothing registers when
`PG_LEAD_STORE` is false.
- The jobs/llm trend sections are file-only in Python and degrade to empty on the PG store, so `leads_this_week`,
  `leads_this_month`, `email_confidence`, `jobs`, `pipeline.status_tiers`, `collection`, `llm` and
  `enrichment.score_distribution` are constant empty/zero shapes (deliberate; migrating the trends is Python follow-up).
- `enrichment.by_city[].avg_score` and every `source_quality` aggregate other than `count` are `null` on the PG store.
- `*_pct` is `round(n / total * 100, 1)` (half-even on the binary value, like CPython) or the int `0` when the workspace
  has no leads (`TestPctBankersRounding`, `TestOverviewEmptyWorkspaceUsesIntZero`).
- Row order: Python's `GROUP BY` for sources has no `ORDER BY` and equal city counts come back in heap order. Go orders
  sources by name and city ties by name; the fixture test sorts both sides before comparing those two lists.
- A NULL city is `"city": null`; a NULL or empty source is `"unknown"`.

## Lead write routes (plan 05 Task 10): deviations
- A NUL byte in any `POST /api/lead` string, or in a `PUT /api/lead/{id}` text value, answers 422 `value_error` (loc `body.<field>`, input
  not echoed) with nothing written (D2; FastAPI answers 500 from Postgres). Create reports the first offending field in model order.
- `PUT`/`POST`/`DELETE /api/lead/{id}` with an id above int4 (but within int64) or beyond int64 answers 404 `Lead not found` (FastAPI: likely 500).
- Bodies are decoded by `encoding/json`: `NaN`/`Infinity` literals answer 422 `json_invalid` (Python's parser accepts them), and a lone
  surrogate escape such as `\ud800` becomes U+FFFD instead of a Python 500 at encode time.
- Trailing data after the JSON value (for example a stray `}`) answers 422 `json_invalid`, as in Python.

## Dedup routes (plan 05 Task 13): deviations
- `POST /api/leads/dedup/merge`: non-integer `master_id` or `duplicate_ids` (strings, floats, booleans, nulls, a non-list) answer 400 `master_id and duplicate_ids required` (D2; Python looked them up in SQLite-style coercion or crashed with 500).
- A merge whose leads hold a NULL/non-numeric `score` or a non-string `source` answers 422 (`value_error`, loc `body.duplicate_ids`) before any write (D2; Python raises TypeError, 500).
- The master update and all deletes run in one transaction (D4); Python wrote and deleted in separate statements, so a failure left a half-merged state.
- Final-review hardening: the duplicates are deleted before the master is updated (same transaction). A master with an empty city merged with a same-company duplicate that has a city collides with that duplicate on `uq_leads_ws_company_city` when the master is updated first, so Python answers 500 (IntegrityError) for the most common duplicate shape; Go answers 200. If the merged company+city belongs to a lead outside the merge, Go answers 409 `A lead already exists for the merged company and city` and rolls everything back (Python: 500).
- Dedup run fixtures are compared order-insensitively (cluster id sets, unordered pair digests): Python's tie ordering depends on `PYTHONHASHSEED`.

## SSE events (plan 05 final fix): hardening beyond Python
- `GET /api/events`: Python has no stream limits. Go answers 429 `{"detail":"Too many event streams for this user"}` /
  `{"detail":"Too many event streams for this workspace"}` past `SSE_MAX_STREAMS_PER_USER` (10) / `SSE_MAX_STREAMS_PER_WORKSPACE`
  (200), after the 403/SSO checks and before the process-wide `SSE_MAX_STREAMS` 503. nginx adds `limit_conn sse_per_ip 20` (429).
- A stream is closed by the server after `SSE_MAX_LIFETIME_SECONDS` (30 min) and when the server shuts down; EventSource reconnects
  and re-authenticates. Python streams live until the client leaves.
- The `/api/events` nginx location logs `error_log` at `crit` only, because upstream-error lines print the `?token=` query.

## Bulk enrich (plan 05 Task 14 and final fix): deviations
- `POST /api/leads/bulk-enrich`: a JSON `NaN`/`Infinity` literal answers 422 `json_invalid` (Python parses it and answers 422 `finite_number`); a number such as `1E400` answers 422 `finite_number` with `input: null` (Python echoes the value). All are 422.
- Final-review hardening: a lead id outside int4 (for example `2147483648`, `1e10`, a 30-digit integer or a numeric string) answers 422 `value_error` ("Value error, lead id out of range", loc `body.lead_ids.<i>`, input not echoed) and nothing is enqueued. Python accepts any size, enqueues it verbatim, and the worker's Postgres lookup 500s; huge digit strings also bloated `jobs.payload`. Over-long digit strings are rejected before they are parsed.

## Signals mark-read (plan 05 Task 15): deviations
- `POST /api/signals/mark-read`: a `lead_id` outside int32 answers 422 (Python lets Postgres 500).
- Request bodies over 4 MiB answer 413 `Request body too large` (Python: unbounded).

## Contract coverage (plan 04 Task 23)

- `internal/api/workbookcrud_contract_test.go`: `TestEveryMigratedWorkbookRouteHasAFixture` (every plan 04
  route has a fixture), `TestRetainedWorkbookRoutesNotRegistered` (the 14
  retained workbook routes + 2 ingest routes in `retained_workbook_routes.txt` are not registered in Go)
  and `TestFixturesCoverEveryGuardString`. Fixture replay itself stays in the per-area tests.
- Guard strings with no fixture, by design: hardened files/system answers, Go-only memory/body limits, and
  real FastAPI guards that need corrupted stored state or a race (`Column identity is ambiguous`, stale
  saved-view column, `Selected workbook rows changed`, template registry errors). Each is asserted by a Go
  unit test; the list is `uncoveredGuardAllowlist` in the test.
- nginx: retained workbook routes always reach Python; asserted by `scripts/nginx-routing-check.sh`.
