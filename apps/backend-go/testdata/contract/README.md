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

## Plan 06 (enrichment) recording profiles

`scripts/contract_cases/enrichment.yaml` (areas `estimate run cellrun meta refresh sources migrate ingest ws`) needs the
enrichment seed, which is NOT part of the default seed (it adds workbooks `wb-enrich-*`, rows, an ingest token, provider stats,
a refresh policy, scan signals; plan 02-05 fixtures stay byte-stable): `CONTRACT_SEED_ENRICHMENT=yes`. Each profile re-seeds, then
records `--profile <p>`; start FastAPI as a NON-superuser role, `TZ=UTC`, and always `RUN_INLINE_WORKER=0` (otherwise the in-API
queue worker executes the enqueued runs against the seed). Server env per profile:

| profile | seed env | server env | recorder |
|---|---|---|---|
| `default` | | `INGEST_API_ENABLED=true` | `contract_record.py` |
| `billing` | `BILLING_ENABLED=true` (ws-alpha gets 10.0 USD) | `BILLING_ENABLED=true RESEARCH_VENDOR_COST=1 INGEST_API_ENABLED=true` | `contract_record.py` |
| `ingest_off` | | (no flag) | `contract_record.py` |
| `ws_echo` | | `REDIS_URL=redis://127.0.0.1:1/0` (unreachable: echo mode) | `contract_record_ws.py` |
| `ws_redis` | | `REDIS_URL=<test redis>` | `contract_record_ws.py` |

```sh
python3 scripts/contract_record.py --base http://127.0.0.1:18000 --out ../../apps/backend-go/testdata/contract \
    --cases scripts/contract_cases/enrichment.yaml --profile default
uv run python scripts/contract_record_ws.py --base http://127.0.0.1:18000 --out ../../apps/backend-go/testdata/contract \
    --cases scripts/contract_cases/enrichment.yaml --profile ws_echo
```

Server-generated randoms differ between recordings (compare with `IgnoreShapeOf`): `run_id`, `src_<8 hex>` source column ids, ingest
token `id`/`prefix`/`created_at`, ledger `created_at`/`run_id` in `billing_balance_*`. Everything else re-records byte-identically.
`ws/*.json` fixtures are `{request: {path, query, steps}, response: {handshake_status, steps[].received, closed_by_server}}`: a
pre-accept `close(4403)` reaches the client as a refused handshake (HTTP 403), so `handshake_status` is 403, not a close code.
Other vectors: `internal/<pkg>/testdata/*.json` (`legacy/python/scripts/gen_enrichment_vectors.py`) and provider transcripts
`testdata/providers/<provider>/*.json` (`record_provider_transcripts.py`, scripted vendor bodies, fake `ctk_*` credentials replaced by
`{{secret:NAME}}` / `{{b64secret:NAME}}` placeholders, no network).

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

## Plan 04 final-review hardening: deviations

Beyond Python (which has none of these limits); every cap is env-configurable (`internal/config`) and a request over it
writes nothing. No fixture; tests named in parentheses.
- Add rows (I1): at most `WORKBOOK_MAX_ADD_ROWS` (10000) rows per request and `WORKBOOK_MAX_ROW_KEYS` (2000) keys in one row
  (or its `data`), else 422 `too_long` (`TestAddRowsRowCountCap`, `TestAddRowsKeyCap`). Add rows and workbook
  create/update/from-jobs now draw on the same upload byte budget as import (`IMPORT_MEMORY_BUDGET_MB`); when it stays full
  they answer 503 `Too many uploads in progress` + `Retry-After: 5` (`TestBufferedBodiesShareImportBudget`). The streaming
  import decoder has no per-row key cap and the budget keeps the "about ten times" decode estimate (deferred).
- CSV export (I2): each write extends a connection write deadline by `EXPORT_WRITE_TIMEOUT_SECONDS` (15), so a reader that
  stops reading aborts the export (connection cut) and releases its pooled connection and transaction; at most
  `EXPORT_MAX_CONCURRENT` (8) exports run at once and `EXPORT_MAX_PER_WORKSPACE` (2) per workspace, else 429
  `Too many exports in progress` + `Retry-After: 5` (`TestExportLimiterGlobalAndPerWorkspace`,
  `TestDeadlineWriterExtendsDeadlinePerWrite`).
- CSV export (I3): header cells get the same formula guard as values (apostrophe prefix), and a leading LF is guarded too
  (Python guards only `= + - @ TAB CR`, values only) (`TestExportHeaderCellIsNeutralised`, `TestExportEscapesFormulaPrefixes`).
- Saved views and columns (I4): at most `WORKBOOK_MAX_VIEW_FILTERS` (50) filters and `WORKBOOK_MAX_VIEW_SORTS` (10) sorts per
  saved view on write, and `WORKBOOK_MAX_COLUMNS` (500) columns per workbook (create, update, add column), else 422
  `too_long`. Views already stored over the cap still load (no read-side cap; deferred)
  (`TestViewConfigFilterAndSortCaps`, `TestColumnListCap`).
- `POST /api/templates/{template_id}/create`: `columns_config` is stored without HTML escaping like every other write path
  (Python text parity; decoded JSON was already identical) (`TestTemplateColumnsStoredWithoutHTMLEscaping`).

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

## Platform credit debit (plan 06 Task 6): deviations

- `CheckAndDebit` scopes the run idempotency key per workspace. Python's `credit_ledger_entries.idempotency_key` (`run:<run_id>`) is unique across all workspaces, so a run id already used by workspace A made workspace B's debit a silent replay (B ran uncharged; vectors `same_run_id_two_workspaces`, `two_workspaces_isolated` step 5). Go writes `run:<run_id>@ws:<workspace_id>` (sha256 of the run id when over the 160-char column), still treats a legacy `run:<run_id>` row in the same workspace as a replay, and keeps `run_id` raw. Go never runs DDL, so the global unique index is unchanged and cannot collide. Proven by `TestDebitTenantIsolation` and the two vector steps asserted as deviations in `TestCheckAndDebitMatchesVectors`.
- A no-op debit (disabled, cost rounds to <= 0) reads the balance but never creates the zero `workspace_credits` row Python's `get_balance` creates; `NaN` cost is a no-op.
- The debit runs in the caller's transaction (never commits), so Task 24 debits and enqueues atomically (decision D4).
- `ProjectedPlatformCost` uses a default catalog where only `research` (0.05, `RESEARCH_VENDOR_COST`) is platform-billed, exactly Python's `byok=False` set; Task 19 wires `vendorcatalog` through `billing.SetCostCatalog`.

## Manifest (plan 06 Task 8)

`internal/manifest` (`CanonicalYAML`, `Verify`, `LoadIndex`) matches `signing.py` / `go_executor.load_manifest_index` on the Task 1 vectors. Go-only deviations:

- Size caps: manifest, `.sig` envelope and trust store are each limited to 1 MiB (Python is unbounded); larger files are `invalid` / skipped.
- YAML expansion caps: an alias-expanded document over 1,000,000 nodes or nested deeper than 512 (including recursive anchors) is rejected.
- Symlinks are never followed: symlinked manifest files and directories are skipped by `LoadIndex`; a symlinked `.sig` makes `Verify` return `invalid`. A symlinked root `dir` yields an empty index.
- A `..` element in the `dir` argument returns `ErrPathTraversal`.
- Each manifest is read once; the verified bytes are the hashed and decoded bytes (no TOCTOU between verify and load).
- A broken trust store is deferred and only turns a signed manifest `invalid` at Python's point (after envelope checks); a missing trust store file is empty (`untrusted`).
- Not supported (Python accepts): `!!omap` / `!!pairs` tags, NaN mapping keys, non-string trust-store key ids (entries ignored), pydantic string-to-number coercion in manifest fields.
- `CanonicalManifest(Manifest)` is not Python-identical (Python hashes the authored YAML); the Python-equal hash is `Entry.SHA256`.

## Column graph and conditions (plan 06 Task 9): deviations
- A non-string, truthy `condition` makes Python raise (`condition.strip()` AttributeError, the cell errors). Go fails closed: the condition is false, so the column is skipped (condition not met) instead of erroring. `0`, `false`, empty and null non-strings still mean "no condition" like Python.
- A column config entry without an `id` makes Python raise KeyError while building the display-name map; Go skips that entry.
- Python iterates row cells in `lead_data` insertion order, which decides which column wins a shared display name (last wins) or a case-only collision (first wins). Go iterates row keys in column config order first, then any remaining keys sorted; identical for realistic data, arbitrary only for keys absent from the column config.
- Decimal comparison bounds (adjusted exponent <= 999999999999999999, exponent >= -1999999999999999997, 19-digit exponent cap) follow 64-bit CPython/libmpdec; 32-bit CPython differs.

## Refresh (plan 06 Task 16): deviations

- `Handle` does not source or enrich inline: it enqueues one `source_workbook` per source column and one `run_workbook` for the stale cells and their reactive dependants, both with the Python executor, fire-keyed on the claim id. Rows added by the sourcing job are therefore enriched one refresh cycle later (they have no overlay, so they are stale next time); Python enriched them in the same cycle. Task 22 restores same-cycle enrichment.
- The activity message reads `<reason>: queued N source columns, re-enrich C stale cells in R rows` (counts of work queued, not done).
- `IntervalMinutes`: NaN, infinity and values beyond int64 report "no interval" (Python raises); negative or over-10-year intervals are not chained (Python would hot-loop or overflow). `strings.ToLower` differs from Python `str.lower` for U+0130.
- The next refresh is chained unless another refresh_workbook is pending or a lower-id one is processing for the workbook (the lowest id always chains) (Python chains unconditionally and forks a chain per signal-triggered job). Retry idempotency of the next job and the activity row holds only while the first attempt's rows are active; a retry after the next job finished, or after commit but before completion, can duplicate them.
- `SetPolicy` stores `json.Marshal(policy)` (sorted keys) and returns a map; the route must emit `refresh_policy` before `next_in_minutes`.
- A payload `workspace_id` that differs from the claim's workspace is an error; neither present is an error (like `workspace_scope`).
- The next refresh uses `Deps.NextExecutor` (default `go`); Python picks it per cohort. Task 22 wires it from the cohort resolver.

## MCP (plan 13)

- `HTTPError.Detail` (internal/mcp, `AutomationPort.ValidateRule`): Python renders `str(he.detail)`; a non-string detail is a Python repr (dict/list text) that a Go adapter cannot reproduce byte for byte. Accepted deviation; string details match.
- Token capabilities (internal/mcp `Resolve`): a malformed `mcp_tokens.capabilities` JSON fails closed to an empty capability set (the token authenticates but holds no capability); Python would raise. Accepted deviation.
- Write reservation and mutation share ONE transaction (D5, D-hardening; `internal/mcp` `Reserve`/`Settle`/`Release`): the pipeline reserves, applies the write and settles inside a single `tenancy.InWorkspace` transaction, so a failed write rolls the reservation back and the same idempotency key can be retried. Python commits the reservation first, releases it on failure and then refuses a retry of the released key; a crash mid-write there leaves a held slot, Go leaves none. A `released` row is reopened (the `(workspace_id, idempotency_key)` unique constraint forbids a second row). A `pg_advisory_xact_lock` on (workspace, UTC day) serialises reservers, so the daily cap and the key dedupe are exact under concurrency. Enrichment reuses the same functions under rule `__mcp_enrich__` with `MCP_MAX_ENRICH_PER_DAY`.
- Audit argument redaction (D6, `RedactArguments`): Python redacts only top-level keys `token, password, secret, api_key, authorization, email` and truncates only top-level strings. Go additionally recurses into nested objects and lists (depth <= 4; deeper containers become `***truncated***`), redacts any key matching `token|secret|password|authorization|cookie|credential|api_?key` (case-insensitive substring) at every depth including the top level, and truncates nested strings to 500 characters plus `…`. `email` is redacted at the top level only. Audit text fields are bounded (tool 100, status 20, idempotency key 128, error 1000 characters) so an oversized value cannot fail the row. The key pattern is `token|secret|password|passwd|pwd|authorization|bearer|cookie|credential|session|api[-_]?key|private[-_]?key|access[-_]?key`. Redaction is by KEY NAME only: a secret inside a free-text VALUE (a `body_html` link token, a URL with a query token) is not detected; values are only length-limited (500 characters each, and the whole marshalled argument object is capped at 16 KiB, beyond which the row stores `{"_truncated":true}`). The Python vectors are asserted for top-level inputs; nested vectors (`nested`, `actions`, long string in a list) differ on purpose.
- JSON-RPC protocol hardening (D4, `Server.Handle`): Python raises on malformed input (SSE: HTTP 500, stdio: silent drop). Go answers JSON-RPC errors with HTTP 200 on SSE and keeps serving: invalid JSON or invalid UTF-8 is `-32700 Parse error`; a non-object body (array, scalar, batch), nesting deeper than `canonjson.MaxDepth` (200), a message over 4 MiB, a non-string or null `method`, or an id that is an object, array or boolean is `-32600 Invalid Request` (id `null` when unreadable); `params` present but not an object (including `null`), unreadable params, or a `tools/call` with a non-string `name` or non-object `arguments` is `-32602 Invalid params` (checked for every method except `notifications/*`; Python ignores `params` for most methods and would raise on a non-dict). Any `notifications/*` method gets no reply (Python answers `-32601` for all but `notifications/initialized`). `isError` stays `false`. The id is echoed with its JSON type as Python would after a round trip (`-0` is `0`, `1E2` is `100.0`, integers beyond int64 keep their digits). A tool call has a deadline of `MCP_TOOL_TIMEOUT_SECONDS` (Python: none); on expiry the text is `{"error": "tool timed out after <d>"}` and a handler that ignores its context is abandoned, not awaited. A tool error that is not an argument, unavailable or HTTP error is masked as `internal error`, never a driver string or Python exception text.
- SSE transport hardening (D6, `internal/mcp/transport_sse.go`): `GET /sse` and `POST /message` answer 403 unless the `Host` header names loopback (`localhost`, `127.0.0.0/8`, `::1`) and any `Origin` header is a loopback origin (DNS-rebinding guard; Python has none); the check is skipped when `OPENGTM_MCP_SSE_ALLOW_NONLOOPBACK` is true. The request body cap is 4 MiB (413 beyond; the plan text said 1 MiB), equal to the JSON-RPC frame cap. Failed authentications are counted per socket peer IP (forwarded headers are ignored) with a budget of 30 per minute; an IP over budget gets 429 before any token lookup. There are no SSE sessions: the endpoint stays the fixed `/message` and the `GET /sse` stream ends right after the endpoint event, as in Python.

## MCP enrich tools (plan 13 Task 9): deviations (D3, D7)

- `verify_email`, `enrich_company`, `get_hiring_signals`, `score_lead` never echo Python exception text. Failures answer a generic `enrichment failed`, `enrichment timed out` or `enrichment backend unavailable` in the tool's own shape (`{"email", "error"}`, `{"company", "error"}`, `{"error"}`); the detail goes to the server log only. The Python fixtures whose stub `raises` (`provider unavailable`, `verifier down`, `job board timeout`) therefore differ in the error string.
- New cap `MCP_MAX_ENRICH_PER_DAY` (default 0 = unlimited, as Python which has none) on the three cost-bearing tools through `Reserve(rule "__mcp_enrich__")`; exceeded answers `{"error": "mcp enrich cap exceeded"}` and is audited `capped`. The slot is released on failure, timeout or panic.
- The three cost-bearing tools write an `mcp_audit_log` row per call (ok/error/capped, cost 0 for `mcp.*`); Python writes none for reads. `score_lead` is neither capped nor audited.
- Missing or mistyped arguments answer `{"error": "<field> is required|must be ..."}`; `email` is limited to 320 characters and `company` to 500.
- `enrich_company` goes through the Python RPC `mcp.enrich_company`; Python's own tool raised on the `(results, logs)` tuple of `WaterfallEnricher.enrich` (issue #114), the RPC provider returns `{"enriched_fields", "provider"}` instead, and with no chains registered that is an empty success (`{}` and `""`).

## MCP outreach tools (plan 13 Task 13): deviations (D3, D9)

- `enroll_leads`: `lead_ids` must be an array of integers, at most 1000 (`{"error": "lead_ids must contain at most 1000 ids"}`, `{"error": "lead_ids must be an array of integers"}`); `sequence_id` at most 255 characters. Python accepts anything and raises on a bad element.
- `send_email`: `subject` at most 998 characters and must not contain CR, LF or NUL (`{"error": "subject must not contain line breaks"}`, header-injection hardening; Python accepts them, so the send adapter must still header-encode); `body_html` at most 204800 bytes; `sequence_id` and `idempotency_key` at most 255 characters; `step_number` a non-negative integer. Mistyped arguments answer `{"error": "<field> must be ..."}`.
- `send_email` normalizes the lead email (NFKC, trim, lower, IDNA2003 domain via transitional mapping) before the send; an email containing CR/LF/NUL or empty after normalization is skipped as `no_email` without queueing. A lead with no email never reaches `QueueSend`.
- Both tools answer `{"error": "<tool> is not available in this build"}` while the plan 08 outreach adapter is not wired (nil `OutreachPort`).

## Plan 06 Task 10 deviations (formula and http columns, `internal/colcompute`)

Parity is pinned by `internal/colcompute/testdata/{formula,template,http}_vectors.json` (271 + 60 cases) and a throwaway differential run of ~16k generated expressions against the real Python module (7 mismatches, all exotic syntax-error wording). Documented deviations:

- Formula resource caps (Python has none): source <= 100,000 bytes, <= 10,000 AST nodes (`formula too complex (more than 10000 nodes)`), evaluation deadline 100 ms (`formula evaluation timed out`), str/list/bytes results <= 1 Mi items and ints <= 2^16 bits (`result too large`), cumulative produced size <= 16 MiB (`formula allocation budget exceeded`). Evaluation never compiles, evals or reflects; the whitelist walk is breadth-first like `ast.walk`, and every node kind the parser does not fully model (Dict, Set, JoinedStr, Starred, comprehensions, Lambda, NamedExpr, Await, Yield, keyword) is outside the whitelist and rejected at that node.
- Exotic SyntaxError wording may differ (CPython's invalid_* rules are matched for the common cases only); the formula is rejected either way. `\N{name}` escapes and lone surrogates (`\ud800`) are not supported; bytes `%` formatting is not supported; f-string replacement fields are not parsed (always rejected). Case-insensitive `{ref}` ties resolve in sorted key order (Python: dict order).
- http column: methods limited to GET, HEAD, POST, PUT, PATCH, DELETE (`unsupported_method`; DELETE is vector-pinned); the caller-supplied client is ignored and the dial-time-guarded egress client always used; secrets are redacted from success values too; response body capped at 1 MiB (`response_too_large`), no redirects (3xx is `http_3xx`), URL re-validated by `egress.CheckURL` after expansion plus the dial-time guard, transport errors never carry the request URL. New: `{secret:NAME}` expands in header values only (never URL/body); an unknown name is `unknown_secret: NAME`, a URL template whose authority takes row data refuses secrets (`secret_with_templated_host`), secret values are redacted from error text.
- `ExecuteFormulaColumn`/`RowValues` visit cells in sorted key order; case tables for str.upper/lower/title are generated by `internal/colcompute/gen_pycase.py` (Python 3.13, Unicode 15.1).

## Email verification (plan 06 Task 15): deviations

`internal/emailverify` ports email_verify.py, email_verify_cascade.py, email_deliverability.py and email_waterfall.py. Vectors: `internal/emailverify/testdata`. Deviations from Python:

1. `Deliverability(ctx, d, workspaceID, email)` takes the email (Python and the vectors are email-keyed), not a domain; Python has no MX/SPF/DMARC check, so none was added.
2. `Result` uses Python's field names (Email, Status, Source, Confidence, Detail, Attempts).
3. Website finder: runs on the Python worker as provider `website_scraper`; it sees only the provider's `email` plus up to two `secondary_emails` (Python scanned every page email). Without a Python worker the step is skipped (degrades, never errors).
4. MX choice: lowest preference wins, but Go's resolver randomises order among equal preferences; Python takes the first listed.
5. MX and catch-all memories are bounded (20,000 entries) and expire after 1 hour (Python: unbounded, process lifetime). A transient DNS failure therefore heals.
6. Hardening (SSRF/DoS): SMTP dials through the egress guard (private, loopback, link-local, rebinding refused; refusal is "unknown", never a reject); 10 s total budget; at most 2 concurrent sessions per MX host; reply lines capped at 512 bytes and 256 KiB per conversation; command arguments with CR/LF/space/angle brackets/non-ASCII are refused locally; only EHLO/HELO, MAIL FROM, RCPT TO, QUIT are ever sent (no AUTH, STARTTLS, DATA, VRFY).
7. Reacher uses the process-global circuit breaker of providers/contact; `REACHER_URL` comes from operator config only, never workspace settings. Reacher is prepended per call (never registered globally) and the cache is engaged only when it is in the chain, as in Python.
8. Verdict cache is Postgres, workspace-scoped under RLS (Python: one global SQLite file shared across workspaces).
9. `PROVENANCE_TRACKING_ENABLED` stamping is a `Deps.Provenance` hook (off by default); no licenses port.
10. Classification lower-casing uses Go `strings.ToLower` (differs from Python `.lower()` on a few exotic code points).
11. `enrichment_waterfall`: existing entries are preserved byte-for-byte; only the new `email_finder` entry is rendered in `json.dumps` style.

## Provider runner (plan 06 Task 18): deviations

`internal/providerrun` dispatches one provider call to a Go provider or to the Python worker (`pyworker`). Deviations and rules:

1. **Operator egress allow-list `EGRESS_ALLOW_HOSTS`** (new, global env, comma-separated exact `host` or `host:port`, empty by default). Python had no SSRF guard, so a private self-hosted Reacher always worked; Go refuses private addresses, so an operator may list that one host. Only the provider client built by `egress.NewOperatorClient` honours the list, in every `APP_ENV`; the http column, SMTP probes and every user-supplied URL still honour it only in test/bench. An entry without a port matches any port of that host; listed hosts skip the private-IP check entirely (including DNS rebinding for that host), so a misconfigured entry opens exactly that host and nothing else. Config no longer rejects `EGRESS_ALLOW_HOSTS` in production for this reason.
2. Provider settings (`REACHER_URL`/`REACHER_ENABLED`/`REACHER_TIMEOUT`, `TECH_STACK_WEBSITE_FETCH_ENABLED`, `TECH_STACK_RESPECT_ROBOTS`, `SEC_EDGAR_USER_AGENT`, `MCA_RESOURCE_ID`) are filled only from global operator config (app_settings, then env); caller-supplied `Input.Settings` is discarded and a workspace value is never read. A lookup error fails the call (no env fallback).
3. Credentials resolve per workspace (workspace secret, then global operator, then env) through the secrets store; a lookup error fails the call closed.
4. A Go provider that ignores its context is abandoned at the timeout: the caller gets `timeout`, the goroutine keeps its slot (`provider_workers`) until it returns, and `provider_calls_abandoned` on the debug `/metrics` shows the gauge. When every slot is held the call returns the cell error `provider_busy` instead of queueing.
5. Reacher `unconfigured`/`breaker_open`/`error`/`timeout`/`http_error` details are reported as failures (Python's verifier returned success on a configured-off verifier only through the cascade); the stats ledger counts them as misses.
6. A Python-RPC timeout is never retried (paid calls); every Run mints a fresh `call_id`.
7. Stats rows reuse `RecordProviderAttempt` (planner.record_attempt port): rate-limit cooldown 300 s only for a Python exception whose text looks rate-limited, timeout cooldown 180 s; a failed ledger write is logged, never fails the call. A cache hit writes no ledger row.

## Source ingest (plan 07 Task 8): deviations

`internal/sourceingest` ports `upsert_source_records` (vectors: `internal/sourceingest/testdata/source_ingest.json`). Deviations from Python:

1. Workbook pre-check: `Upsert` first requires the workbook to exist in the caller's workspace and returns `ErrWorkbookNotFound` otherwise (before the advisory lock). Python trusts its caller; a cross-workspace workbook id is an error in Go, never a write.
2. Static batch caps (Python has none): at most 50,000 records (plan 04 `wbcrud` MaxRows) and 64 MiB of encoded row data (plan 04 import body cap) per call, else `ErrBatchTooLarge`. The shared `IMPORT_MEMORY_BUDGET_MB` byte semaphore is unexported in `internal/api`, so it is not drawn on; TODO revisit if it is exported (connector pages are small).
3. `Record.Data` is an insertion-ordered `*wbcrud.Obj`: new rows keep the connector's key order with `_source` last (or in place when Data already carries it); updates keep the stored key order and append new keys, as `{**old, **new}`. Connectors must build Data in Python field order.
4. JSON text is compact (`{"a":1,"b":2}`), Python writes `", "`/`": "` separators; the column is `json`, so the text is stored as given. Readers see the same keys in the same order.
5. `UpsertNotify` enqueues the `on_row_added` `trigger_eval` jobs inside the caller's transaction (savepoint in `automationhook`); Python emits after the page commit. The jobs therefore commit atomically with the rows and cursor (no lost event on a crash, no worker seeing uncommitted rows). The hook swallows and logs its own errors like `emit_row_added`.

## Salesforce client (plan 07 Task 10): deviations (D3)
- `SALESFORCE_INSTANCE_URL` is user-typed and unguarded in Python (concatenated into every request URL). Go (`internal/crm/salesforce.go`) validates it on EVERY call, before any request: scheme `https`, no userinfo, default port (443), bare origin (no path, query or fragment), host on an allowed Salesforce suffix (`*.salesforce.com`, `*.my.salesforce.com`, `*.force.com`, `*.cloudforce.com`; the bare apex is refused), and not a private/blocked address (`egress.CheckURL(raw, false)`, which also catches IPv4-mapped IPv6 and integer/hex IP encodings). The default HTTP client is the `egress` client, which re-vets the resolved address at dial time (DNS rebinding) and never follows redirects. Python has no suffix allowlist; a custom-domain or on-prem instance URL outside these suffixes is refused with `{"success": false, "error": "blocked url: <reason>"}` (a per-run error, never a 500). Widen `sfAllowedSuffixes` if a vendor domain is missing.
- `nextRecordsUrl` from a query page must be a path under `/services/data/` (no `//`, `@`, backslash or whitespace) or the fetch fails with `invalid nextRecordsUrl from Salesforce` (Python concatenates it onto the instance URL unchecked, so `@host` could redirect the next page). Paging is capped at 1000 pages. Path segments (`sobject`, record id) are percent-escaped. Response bodies are capped at 1 MiB; the access token is scrubbed from every error and echoed upstream body.
- Exported names: Go cannot hold two `DefaultFieldMap`/`UpdateResult`/`PushResult` in package `crm`, so Salesforce uses `SalesforceDefaultFieldMap`, `SalesforceUpdateResult`, `SalesforcePushResult` (HubSpot keeps the short names). `UpdateRecord` takes an ordered `FieldMap` (body field order = Python dict order), not a Go map.

## Connector catalog (plan 07 Task 12): deviations
Fixtures `connectors/*` (profiles `default`, `catalog_ok` = default minus `contract_badname.yaml`, `required`).
- D4: a loadable manifest whose name is not a certifiable subject (`Bad-Name`) made FastAPI answer **500** on
  `GET /api/connectors/catalog` (`certification subject is unknown`, fixture `catalog_optional_badname_crash_alice`).
  Go answers **422** `{"detail":"Connector manifest name is invalid"}` (`connectors.ErrUnknownConnectorSubject`, `UnknownSubjectStatus`) without echoing the name; the 200 shape is
  pinned by the `catalog_ok` fixtures (`catalog_optional_*`). `/compatibility` still lists the name as an error entry (200).
- D4: the manifest directory is walked without following symlinks. A symlinked manifest or directory, or a `..` element in the
  directory path, becomes an error entry in `/compatibility` (Python would follow or load it); the catalog never loads through one.
  A manifest over 1 MiB is an error entry.
- Configuration: the Go routes read the manifest directory from `OPENGTM_CONNECTOR_MANIFESTS_DIR` (default
  `legacy/python/apps/api/services/leadgen/enrichment/declarative/manifests`, relative to the working directory) and the trust store
  from `OPENGTM_CONNECTOR_TRUST_STORE` (default `docs/connectors/trusted-publishers.json`); set both in deployments. The resolved
  paths and policy are logged at startup. `CONNECTOR_SIGNATURE_POLICY` set but empty is `required`, as Python. The trust store is
  re-read per request; the certification key is revealed per request and never logged.
- `/compatibility` error `path` is `str(path)` of the served directory. Fixtures carry `../../apps/backend-go/internal/connectors/`
  in front of `testdata/manifests/...`; tests strip that prefix before comparing.
- A duplicated manifest name is listed twice in `/catalog` and reported once as `duplicate provider id` by `/compatibility` (Python
  behaviour, kept). `package_sha256` is `manifest.Entry.SHA256` of the reviewed manifest and feeds the certification build map;
  an unreviewed (rejected by the report) manifest renders `signature: {"status":"unknown"}` and `package_sha256: null`.
- Error texts that carry library messages (pydantic, YAML, base64, JSON) are not byte-portable; `connector_report.json` flags them
  with `error_is_portable` and tests compare paths and counts for those.

## AmbitionBox (plan 07 Task 9): deviations
- Redirects are refused (Python/aiohttp follows them); a 3xx is an immediate typed `UpstreamError`.
- Responses are bounded to 4 MiB (Python reads them whole). An oversized, undecodable or wrong-shape 200 is a typed
  `UpstreamError` and is not retried (Python: a ContentTypeError is retried, a JSONDecodeError is a 500).
- `casefold` for name-keyed record ids (companies without an id) is an approximation (lower plus ss/sigma/ligature table); exotic
  scripts can key differently from Python's `str.casefold`.
- Timeout text is `timeout` (Python's is empty). Production clients use `ambitionbox.NewEgress` (SSRF-safe dialer, Base pinned).

Routes (plan 07 Task 18, `internal/api/ambitionboxroutes.go`, D4; replayed against `testdata/contract/ambitionbox`):
- `GET /companies` and `/collect` with `location` or `company_type` (collect: `location`) answer 422 `{"detail": "<NotImplementedError text>"}`; Python 500. An empty value is no filter, as Python.
- An upstream failure (`*UpstreamError`, also the 20 s route deadline or a client disconnect) answers 502 `{"detail":"AmbitionBox upstream error"}`; Python 500. The vendor body never reaches the client.
- `POST /imports`: an unknown or foreign `workbook_id` answers 404 `{"detail":"workbook not found in the active workspace"}`; a non-AmbitionBox workbook without `replace_existing` answers 409 `{"detail":"existing non-AmbitionBox workbooks require replace_existing=true"}`; Python 500 for both. The active-run 409 keeps Python's `connector_run_active` body.
- Every read has a 20 s overall deadline and honours the request context (Python has neither). `company_id` beyond int64 is a 422 `int_parsing`.

## Email data (plan 07 Task 14): deviations

`internal/emaildata` serves the 7 `/api/data` routes over the legacy GLOBAL `email_data`/`links` tables (no workspace_id; global admin only, plan 07 D5). Replayed against the recorded `testdata/contract/datacrm` fixtures. Deviations from Python:

1. `sort_by` outside the 8 columns (`id,name,email,source_link_id,created_at,is_used,tags,notes`), e.g. `metadata` (Python 500: getattr on a non-column attribute), falls back to `id`; `order` other than `asc` is descending (as Python). Applies to list and export (`TestSortByNonColumnAttributeIsNotACrash`).
2. Negative `limit` or `offset` answers 422 `greater_than_equal` in FastAPI shape (`emaildata.PagingErrors`, `ErrBadPaging`); Python 500. `stats/domains?limit<=0` answers `[]` as Python.
3. Ids, `source_link_id` and path ids outside the int4 range match nothing (404 for PATCH, `{"status":"not found"}` for DELETE, dropped from `mark-used`); Python raises a driver error (500).
4. A NUL byte in a filter or patch string is `ErrInvalidInput`, answered 422 by the route; Python 500.
5. Ordering appends `id ASC` as a deterministic tie-break (Python has none), so OFFSET paging and export batches are stable.
6. Domain stats lower-case with Go `strings.ToLower` (Python `.lower()` differs on a few exotic code points); counting happens in Go over the streamed email column.
7. CSV export neutralises formula prefixes `= + - @ TAB CR` and a leading LF on name, email, tags and notes with an apostrophe (the plan 04 workbook export guard, `wbcrud.ExportSafeValue`); Python writes them raw (`TestExportNeutralisesFormulaPrefixes`; the Python vector `formula_injection_not_escaped` is compared with the guarded bytes). The guard runs before the notes newline-to-space step.
8. No row cap on export or list `limit` (as Python); the route may cap `limit`.

## HubSpot routes (plan 07 Task 16): deviations

- D4: `POST /api/crm/hubspot/sync` `limit` above 1000 answers 422 (`less_than_equal`) and below 0 answers 422 (`greater_than_equal`; Python 500s on `LIMIT -1`). `limit` 0 is Python's 200 "No leads with emails to sync". `lead_ids` with more than 1000 items answers 422 (`too_long`, ctx key order field_type, max_length, actual_length as pydantic). Recorded fixtures `sync_limit_100000` (200) and `sync_limit_negative` (500) therefore answer 422.
- D4: `GET /api/crm/hubspot/contacts` `limit` above 1000 answers 422 (`less_than_equal`; fixture `contacts_limit_100000` was 200). `limit` 0 and negatives reach HubSpot, which rejects them, and the route answers 200 with an empty list, as Python.
- Contacts are passed through exactly as HubSpot sent them (key order and non-object elements), as Python.
- Security: the HubSpot token lookup fails closed. A secret-store or decryption error means "not connected" and no request is sent; Python falls back to the `HUBSPOT_TOKEN` environment variable on any error.

## Certification (plan 07 Task 4): deviations

- Naive timestamps (no zone) in certification evidence and validity windows are read as UTC. Python reads them in the process-local zone; the vectors were recorded with `TZ=UTC`, so the two agree only on a UTC host. `parseTime` accepts ISO extended forms only (not the Python 3.11 basic `YYYYMMDD` or week-date forms).

## Email data routes (plan 07 Task 17): deviations

Route level, on top of the service deviations in "Email data (plan 07 Task 14)":
1. Negative `limit` or `offset` answers 422 `greater_than_equal` (`emaildata.PagingErrors`); Python 500.
2. A NUL byte in a filter or patch value answers 422 `{"type":"value_error","loc":["query"|"body"],"msg":"Value error, string contains a NUL byte"}`; Python 500. The shape is Go's own (no Python fixture).
3. Ids beyond the int4 range (path or body) match nothing: PATCH 404 (the detail prints the number), DELETE `{"status":"not found"}`, dropped from `mark-used`; Python answers a driver 500. `limit`/`offset` beyond int64 are clamped and there is no maximum `limit`, as Python.
4. `mark-used` and PATCH bodies are capped at 1 MiB (422 `value_error`); Python has no cap.

## Plan 07 coverage and rollout (Task 19)

Coverage tests (`internal/api/connectors_coverage_test.go`): `TestEveryPythonConnectorRouteIsPortedOrRetained` (19 Python route decorators of `connectors.py`, `hubspot.py`, `ambitionbox.py` and `crm.py` = `/api/data`; 19 ported, 0 retained, see `retained_connector_routes.txt`), `TestEveryConnectorRouteHasFixture`, `TestDeviationsDocumented`, and the integration test `TestRuntimeRoleCanReadWriteEmailData`.

Open decisions and where their deviations are listed above: D3 (Salesforce SSRF guard: "Salesforce client (plan 07 Task 10)"), D4 (crash and hardening answers: "Connector catalog", "AmbitionBox", "Email data", "HubSpot routes"), D5 (`/api/data` stays global, global-admin gate on the route group, exact parity; tenant scoping is a schema and product change).

### Plan 07 fixture recording procedure

Fixtures under `connectors/`, `hubspot/`, `datacrm/`, `ambitionbox/`, the vectors under `internal/<pkg>/testdata` and the vendor transcripts under `testdata/vendors/` are recorded from the real Python code, never hand-written. Use `TZ=UTC` (this machine runs IST; naive timestamps shift by +05:30 otherwise).

```sh
docker exec opengtm-go-test-pg psql -U postgres -c "CREATE DATABASE opengtm_contract_p07" \
  -c "CREATE ROLE contract_app_p07 LOGIN PASSWORD '<x>' NOSUPERUSER NOBYPASSRLS" -c "GRANT yupcha_app TO contract_app_p07"
cd legacy/python
export SQLITE_STORES_BACKEND=postgres SECRET_KEY=<test-only> APP_ENV=test TZ=UTC RUN_INLINE_WORKER=0 PG_LEAD_STORE=true PYTHONPATH=.
DATABASE_URL=<superuser url> uv run alembic upgrade head
# per profile: reseed, start the fake-vendor server as the NON-superuser role, record, stop it
DATABASE_URL=<superuser url> CONTRACT_SEED_CONFIRM=yes CONTRACT_SEED_CONNECTORS=yes uv run python scripts/contract_seed.py
DATABASE_URL=<app role url> uv run python scripts/record_vendor_transcripts.py serve --port 8417 [--policy required|--exclude contract_badname.yaml] &
python3 scripts/contract_record.py --base http://127.0.0.1:8417 --out ../../apps/backend-go/testdata/contract \
    --cases scripts/contract_cases/connectors.yaml --profile default|catalog_ok|required
```

Profiles: `default` (signature policy optional, full manifest dir), `catalog_ok` (`--exclude contract_badname.yaml`), `required` (`--policy required`). Vendor transcripts: `uv run python scripts/record_vendor_transcripts.py`. Vectors: `DATABASE_URL=<any opengtm_* db at head> uv run python scripts/gen_connector_vectors.py`. Check: `uv run python scripts/contract_record.py --cases scripts/contract_cases/connectors.yaml --check` (complete and byte-identical). Drop the temporary database and role and stop the server afterwards. The fake-vendor server pins the certification clock (2026-09-01T12:00:00Z), the export filename clock and the run/workbook ids created by `POST /imports`; fixtures depend on the seed and case order.
