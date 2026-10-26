# Workbook CRUD Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Port the tenant CRUD surface around workbooks to Go: 35 of the 49 `workbooks.py` route decorators (workbook/column/row CRUD, CSV import and export, saved views, cost/runs/activity/trace reads, meta), plus `/api/templates`, `/api/functions`, `/api/entities`, `/api/analytics`, `/api/me/context`, `/api/flags` and `/api/system|files`, with contract-identical JSON, status codes and `detail` strings, served side by side with FastAPI behind nginx.

**Architecture:** One domain package `internal/wbcrud` (column config model, column-dependency rules, view-driven row query and cursors, a `Service` whose methods each run inside `tenancy.InWorkspace`) sits under thin handlers in `internal/api`, exactly as plans 02/03 did. Static Python data (column types, lead fields, AI presets, provider list, recipe gallery, legacy templates) is exported once from Python into embedded JSON by an exporter script, with drift tests. Leads are read through a read-only port `internal/leadsread` over the Postgres lead tables (the only lead store Go supports). The automation `on_row_added` hook is ported as its enqueue half (`internal/automationhook`). Routes that need the enrichment engine, LLM or Redis stay on Python and nginx splits them per route by regex.

**Tech Stack:** Go 1.27.3 stdlib, pgx v5 + sqlc, existing `internal/{httpx,identity,tenancy,audit,contract,queue}`; no new Go modules (CSV via `encoding/csv`, YAML only at export time in Python, JSON embedded). Python/uv for the recorder, seed and exporter scripts.

**Spec:** `docs/go-port/inventory.md` section 8 row 3 "Workbook CRUD" (sections 2 HTTP surface, RLS, risks 1 and 3); `docs/go-port/plans/02-go-api-foundation-auth-workspaces.md` (middleware, `httpx`, `contract`); `docs/go-port/plans/03-settings-secrets.md` (conventions, nginx split, runbook); GitHub issue #33. Python sources: `legacy/python/apps/api/routers/{workbooks,templates,functions,entities,analytics,meta,system}.py`, `services/workbook/{schemas,models,column_deps,csv_import,functions,templates,cell_scope}.py`, `services/entities/{models,graph}.py`, `services/templates/recipes.py`, `services/automations/events.py`, `services/leadgen/store.py` (`PgLeadStore`).

**Layout note (branch `go-rewrite`):** Python lives untracked at `legacy/python/`; Python paths below (`apps/api/...`, `scripts/...`) mean `legacy/python/<path>`. Python edits are not versioned; this plan adds only seed/case/exporter scripts there. Go paths are relative to `apps/backend-go/`. Module `github.com/debpalash/OpenGTM/apps/backend-go`.

## Global Constraints

- Go `go 1.27` + `toolchain go1.27.3`; pgx v5 + sqlc only; `make sqlc-check` clean; no new modules. Go never runs DDL (Alembic owns it); `internal/db/schema.sql` is regenerated with `make schema` (Task 2) and nothing else edits it.
- Tenant tables (`workbooks`, `workbook_rows`, `workbook_views`, `workbook_enrichments`, `connector_runs`, `workbook_spend_attempts`, `workbook_activity`, `cell_traces`, `company_entities`, `entity_review_pairs`, `entity_merge_log`, `leads`, `jobs` reads) only inside `tenancy.InWorkspace`. `workbook_functions` is global (no RLS, user decision) and uses the plain pool. Runtime role non-superuser `NOBYPASSRLS`.
- Contract parity is the release bar: same paths, methods, JSON keys and key order within a response object, status codes, `detail` strings and auth gates as FastAPI. Gates (verbatim from Python): `current_workspace` = `identity.Workspace`; `require_editor` = `identity.RequireRole("tables.write", "editor", "admin")`; functions routes are behind `get_current_active_user` (`identity.Authenticate`) with `create`/`delete` also `RequireGlobalAdmin` and `apply` also `require_editor`. Cross-workspace or missing ids answer 404, never 403.
- Naive UTC datetimes serialise as pydantic does (`2026-10-26T10:00:00.123456`, no `Z`, fraction omitted when zero); `updated_at` is set to `now() at time zone 'utc'` by every Go UPDATE that SQLAlchemy would stamp with `onupdate=func.now()` (lists order by it).
- FastAPI input validation maps to 422 `{"detail":[{type,loc,msg,input}]}` via `httpx.ValidationError`; `Query(ge/le)` and body `Field` limits are copied per route. `redirect_slashes` parity: `/api/workbooks` 307s to `/api/workbooks/` (the router already does this; Task 9 pins it).
- `columns_config` and `config` JSON round-trip losslessly: unknown keys inside stored column objects are preserved on read, but request bodies are normalised exactly like `ColumnConfig.model_dump(exclude_none=True)` (defaults `type="lead_field"`, `width=200`, unknown request keys dropped, key order = model field order).
- Go supports only the Postgres lead store. `PG_LEAD_STORE` must be true; when it is not, `RegisterWorkbooks`/analytics register nothing and log why, so nginx canary cannot be enabled (Task 2).
- Out of scope, stays on FastAPI (documented, not silently dropped): `POST /{id}/run`, `/stop`, `/rows/{row}/cells/{col}/run`, `/sources`, `/sources/{col}/run`, `GET /sources/{col}/preview`, `POST /refresh`, `PUT /refresh-policy`, `GET /run/estimate`, `POST /migrate`, `GET /meta/provider-stats`, `GET /meta/provider-accuracy`, `POST /api/v2/workbooks/{id}/generate-column`, `WS /{id}/ws` (all enrichment/LLM/Redis, plan 05) and the 2 `ingest` routes under `/api/v2/workbooks` (plan 06). The 14 retained routes are listed once in `testdata/contract/retained_workbook_routes.txt` (Task 19).
- No per-route `@limiter.limit` exists on any ported route (the only limits in `workbooks.py` are 20/min on `run` and `sources/{col}/run` and 120/min on cell run, all retained); the "30" in inventory section 2 is the count of `require_editor` routes. `internal/ratelimit` is therefore not used here.
- No CI changes. Verify with `cd apps/backend-go && make vet test` and `make test-integration`.
- Every commit DCO signed (`git commit -s`) with the repo-local git identity, no `Co-Authored-By` trailer. GRANULAR commits: tests first (RED, message ends `(RED)`), then each implementation piece. Sonnet subagents execute; stage explicit paths only.

## Review Focus

1. **`updated_at` and list order.** `list_workbooks` orders by `updated_at desc`; SQLAlchemy stamps `updated_at` on every ORM UPDATE, plain sqlc UPDATEs do not. A missed stamp reorders the sidebar and breaks the `If-changed` UX. Every mutating task asserts `updated_at` advanced. Tasks 7, 9-13.
2. **Row cursors.** Three signed-by-shape cursor formats (row, query, connector-run) must decode what Python issued mid-pagination and reject tampering with the exact 400 strings; a sort on a column with NULLs/empty strings must keep stable `(value, position, id)` ordering with no skipped or duplicated rows across pages. Task 8.
3. **Column-edit guards.** Rename/delete/replace must keep refusing when other columns or saved views reference the column (409 strings), and cycles must stay blocked (422), including references by display name, `{Name}` placeholders in prompts/formulas/HTTP templates and `input_columns`. A looser Go check silently corrupts workbooks that the Go worker then fails to run. Tasks 3, 11.
4. **Bulk-write bounds and atomicity.** `PATCH rows` (<= 1000 updates, all-or-nothing, duplicate ids 400, one foreign id 404 for the whole batch), `rows/delete-query` (`expected_count` and `confirmation` drift protection, 409 when the live count changed), CSV import (50,000 rows, 500 columns, dedupe identity) must not partially apply on error nor exhaust memory. Tasks 12, 13.
5. **Unauthenticated file routes.** Python `/api/files/*` and `/api/system/stats` have no auth, `delete` does `rmtree` on any path under the data dir, and the traversal guard is a string prefix check (`/data` accepts `/data-evil`). Parity here would port a remote file-delete. Task 18 hardens by default (decision D1).
6. **Tenant leaks through shared payloads.** Entity merge/split rewrites references in accounts/people tables; a merge between ids of two workspaces, a review pair of another workspace, a function applied to another workspace's workbook and an `X-Workspace-Id` mismatch must all 404 and write nothing. Tasks 16, 17, 14.

---

## File Structure

```
internal/wbcrud/{columns,coldeps,viewcfg,cursor,rowquery,service,create,columnsvc,rowsvc,importsvc,viewsvc,telemetry,exportcsv}.go  # (+ tests)
internal/leadsread/{filter,snapshot,stats}.go              # read-only PG lead queries (+ tests)
internal/automationhook/hook.go                            # on_row_added enqueue (+ tests)
internal/wbregistry/{registry.go,registry.json}            # embedded: column types, lead fields, presets, providers, recipes, templates
internal/entities/{store,merge,split}.go                   # canonical companies, review queue
internal/wbfunctions/store.go                              # global workbook_functions
internal/analytics/overview.go                             # 5 analytics routes over PG store
internal/sysfiles/files.go                                 # hardened data-dir file ops
internal/api/{workbookroutes,workbookdetail,workbookcolumns,workbookrows,workbookimport,workbookviews,workbooktelemetry,workbookmeta,templatesroutes,functionsroutes,entitiesroutes,analyticsroutes,systemroutes}.go
internal/api/register.go                                   # (modify once, Task 2)
internal/db/queries/{wb_workbooks,wb_rows,wb_views,wb_telemetry,leads_read,entities,wb_functions}.sql  (+ gen/)
internal/db/schema.sql                                     # (modify once, Task 2: make schema)
testdata/contract/{workbooks,columns,rows,import,views,telemetry,meta,templates,functions,entities,analytics,system}/*.json
testdata/contract/retained_workbook_routes.txt
../../legacy/python/scripts/contract_seed.py               # (modify, Task 1)
../../legacy/python/scripts/contract_cases/{workbooks,columns,rows,import,views,telemetry,meta,templates,functions,entities,analytics,system}.yaml
../../legacy/python/scripts/export_workbook_registry.py    # writes registry.json
../../infra/nginx/go-routes.conf                           # (modify, Task 19)
../../docs/go-port/runbooks/workbook-crud-cutover.md
```

## Parallel waves

Tasks in a wave touch disjoint files and may run concurrently. Each wave starts only after the previous wave is merged and `make vet test test-integration` is green. `make sqlc` regenerates all of `internal/db/gen/`, so every task stages only its own `<name>.sql.go` and its own `.sql`; Task 2 is the only task that changes `models.go`, `db.go` and `schema.sql`.

| Wave | Tasks | Shared-file notes |
|---|---|---|
| 0 (sequential) | 1 Python seed/recorder, 2 scaffold | Task 1 owns `contract_seed.py`; Task 2 owns `register.go`, `schema.sql`, `Deps`, stub route files |
| 1 | 3 columns+deps, 4 leadsread, 5 automation hook, 6 registry exporter, 15 functions, 16 entities store, 18 system/files | 7 disjoint packages |
| 2 | 7 service core + workbook store, 8 row query + cursors | 7 owns `service.go`, `wb_workbooks.sql`; 8 owns `rowquery.go`, `cursor.go`; both consume 3 |
| 3 | 9 create/list/update/delete, 10 detail/export/connector-runs, 11 columns, 12 rows, 13 CSV import, 14 views, 17 entity routes, 19 telemetry, 20 meta, 21 analytics | each owns its `internal/api/*.go` stub, its `wbcrud/*svc.go` and its `.sql`; no two touch the same file |
| 4 | 22 templates | consumes 9 (`Service.Create`) and 6 |
| 5 | 23 routing, coverage, runbook | consumes everything |

(Task numbers are fixed by the list below; the table groups them by dependency, not numeric order.)

---

### Task 1: Python contract seed and recorder cases (foundation)

**Files:**
- Modify: `legacy/python/scripts/contract_seed.py`
- Create: `legacy/python/scripts/contract_cases/{workbooks,columns,rows,import,views,telemetry,meta,templates,functions,entities,analytics,system}.yaml` (stubs with the header and one smoke case each; each route task fills its own file)

**Interfaces:**
- Seed (re-runnable, same TRUNCATE guards as plan 02) adds, for `ws-alpha` and `ws-beta`: 3 workbooks each with fixed uuids (`wb-alpha-1` empty, `wb-alpha-2` with 5 `lead_field` + 1 `waterfall` + 1 `ai_formula` column referencing `{Company}`, `wb-beta-1`), 25 `workbook_rows` for `wb-alpha-2` (ids 1..25, positions 0..24, `data` with company/website/email, a few NULL and empty values, `enrichments` for the computed columns), 2 saved views, 3 `leads` rows, 2 `company_entities` + 1 `entity_review_pair`, 2 `workbook_functions`, 1 `connector_runs`, 2 `workbook_spend_attempts`, 3 `workbook_activity`, 1 `cell_traces`, 2 `jobs` of type `run_workbook`; fixed `created_at`/`updated_at` so fixtures are stable.
- Users from plan 02 seed: `alice` (owner/admin of alpha), `bob` (viewer in alpha, owner in beta), `carol` (inactive). Add `erin` as `editor` in alpha for gate cases.

- [ ] **Step 1: Write the check** `uv run pytest tests/test_contract_seed.py::test_workbook_seed_is_idempotent` (new, PG-gated): seeds twice, asserts row counts above and that a second run changes no `updated_at`.
- [ ] **Step 2: Run** -> FAIL.
- [ ] **Step 3: Implement** the seed additions and the 12 stub case files.
- [ ] **Step 4: Run** the test -> PASS; `uv run python scripts/contract_record.py ... --cases scripts/contract_cases/workbooks.yaml` records one fixture.
- [ ] **Step 5: Checkpoint** in the `legacy/python` repo; no main-repo commit.

### Task 2: Scaffold: schema, config, `Deps`, stub route files

**Files:**
- Modify: `internal/db/schema.sql` (via `make schema`), `internal/config/config.go` (+test), `internal/api/register.go`
- Create: `internal/api/{workbookroutes,workbookdetail,workbookcolumns,workbookrows,workbookimport,workbookviews,workbooktelemetry,workbookmeta,templatesroutes,functionsroutes,entitiesroutes,analyticsroutes,systemroutes}.go` (each only `func Register<Name>(r *httpx.Router, d Deps) {}`)

**Interfaces:**
- Config additions (names as Python): `PG_LEAD_STORE` (bool), `AUTOMATIONS_ENABLED` (false), `INTENT_POLLER_ENABLED` (false), `AUTOMATIONS_ALLOW_LEGACY_OUTREACH` (false), `DATA_DIR` (default `<repo>/data`), `FILES_API_MODE` (`hardened` default | `legacy`, see D1). `Deps` gains `Flags config.Flags`, `DataDir string`.
- `RegisterAll` gains one line per stub in the order of the files above; `RegisterWorkbooks*` helpers early-return with an `slog` warning when `!d.Flags.PGLeadStore`.

- [ ] **Step 1: Write failing tests**: `TestConfigWorkbookFlagsDefaults`, `TestConfigFilesAPIModeValidation` (anything but `hardened|legacy` rejected), `TestRegisterAllWithoutPGLeadStoreRegistersNoWorkbookRoutes`, `TestSchemaContainsWorkbookTables` (parses `schema.sql` for the 13 tenant tables and `workbook_functions`).
- [ ] **Step 2: Run** `go test ./internal/config/... ./internal/api/...` -> FAIL.
- [ ] **Step 3: Implement**; run `make schema sqlc` against the migrated test DB and commit the regenerated `schema.sql` and `gen/models.go` in their own commit.
- [ ] **Step 4: Run** `make sqlc-check vet test` -> PASS.
- [ ] **Step 5: Commit** (three commits): `test(go): workbook CRUD config and registration (RED)`, `feat(go): schema for workbook CRUD tables`, `feat(go): workbook CRUD scaffold, flags and route stubs`.

### Task 3: Column config model, dependency rules, view validation

**Files:**
- Create: `internal/wbcrud/{columns,coldeps,viewcfg}.go`, `columns_test.go`, `coldeps_test.go`, `viewcfg_test.go`, `testdata/wbcrud/column_vectors.json` (generated by `legacy/python/scripts/gen_column_vectors.py` from the real Python `column_deps` and `ColumnConfig`)

**Interfaces:**
- Produces: `type Column map[string]any` with `func ParseColumns(raw []byte) ([]Column, error)`; `func NormalizeRequestColumn(raw json.RawMessage) (Column, []httpx.FieldError)` (applies `ColumnConfig` defaults, drops unknown keys, `exclude_none`, keeps model field order, enforces `strict` ints, `ge`/`gt` bounds from `schemas.py:15-83`); `func BrokenRenameReferences(before, after []Column) []Column`; `func CycleBlockedColumns(cols []Column) map[string]struct{}`; `func ReferencingColumns(cols []Column, c Column) []Column`; `type ViewConfig struct{ Filters []ViewFilter; Sort []ViewSort; HiddenColumns []string }` with `func ParseViewConfig(raw json.RawMessage) (ViewConfig, []httpx.FieldError)` (ops enum `equals|not_equals|contains|not_contains|empty|not_empty`, default `contains`; sort dir `asc|desc`) and `func (v ViewConfig) MissingColumns(known map[string]struct{}) []string`.
- Errors: `type GuardError struct{ Status int; Detail string }` so routes translate guards 1:1 (`409 Rename breaks column-name references in: <names>. Change those references to stable column IDs first.`, `422 Column changes introduce circular dependencies affecting: <sorted ids>. Break the circular references before saving.`, `422 View references missing columns: <sorted>. Refresh the workbook and update the view.`). Funcs `ValidateRenames`, `ValidateNewCycles`, `ValidateViewColumns(cols []Column, v ViewConfig) *GuardError`.

- [ ] **Step 1: Write failing tests**: `TestColumnVectorsMatchPythonDeps` (every vector: refs by id, by name, `{Name}` in prompt/formula/http fields, `input_columns`, `condition`; cycles of length 1, 2, 3; rename that breaks a `{Name}`), `TestNormalizeDefaultsAndDropsUnknown`, `TestNormalizeKeyOrderMatchesModel`, `TestNormalizeStrictIntRejectsString` (`max_steps: "3"` -> 422 `Input should be a valid integer`), `TestNormalizeNegativeBudget422`, `TestCycleErrorSortedIDs`, `TestViewConfigDefaults`, `TestViewMissingColumnsSorted`, `TestUnknownColumnKeysPreservedOnRead`.
- [ ] **Step 2: Run** `go test ./internal/wbcrud/...` -> FAIL.
- [ ] **Step 3: Implement** by reading `services/workbook/column_deps.py` end to end; vectors come from Python, not from the Go port.
- [ ] **Step 4: Run** `go test -race ./internal/wbcrud/...` -> PASS.
- [ ] **Step 5: Commit** `test(go): column deps vectors from python (RED)`, then `feat(go): column config normalisation and dependency rules`, then `feat(go): saved view config validation`.

### Task 4: Read-only lead access port

**Files:**
- Create: `internal/db/queries/leads_read.sql`, `internal/leadsread/{filter,snapshot,stats}.go`, tests

**Interfaces:**
- Consumes: `tenancy.InWorkspace`. Reads the shared RLS `leads` table only; never writes.
- Produces: `type Filter struct{...}` mirroring `FilterCriteria` (`schemas.py:86-104`, including `job_ids`); `func QueryLeads(ctx, tx pgx.Tx, f Filter, page, pageSize int) (rows []map[string]any, total int, err error)` equivalent to `PgLeadStore.query_leads_page` (`store.py:316-420`) and `get_leads` (247-315): same filter semantics, ordering and row dict keys (`id` first, the lead field names from `LEAD_FIELD_MAP`); `func LeadsByIDs(ctx, tx, ids []int64) ([]map[string]any, error)`; `func Stats(ctx, tx) (StatsResult, error)` equivalent to `get_stats` (421+, keys `total`, `by_tier`, `enrichment{avg_score,with_email,with_phone,with_website,with_contact,with_linkedin}`); `func FilterOptions(ctx, tx) (map[string]any, error)` equivalent to `get_filter_options`.

- [ ] **Step 1: Write failing tests** against rows from the Task 1 seed plus vectors generated by calling the Python store (`gen_leads_vectors.py`): `TestQueryLeadsMatchesPythonVectors` (each filter field alone and combined, paging, total), `TestQueryLeadsOrderingStable`, `TestJobIDsFilter`, `TestStatsMatchPython`, `TestFilterOptionsMatchPython`, `TestLeadsTenantIsolation` (FORCE RLS: workspace B sees zero of A), `TestPageSizeBounds`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): lead read port vectors (RED)`, `feat(go): read-only lead queries for workbook snapshots and analytics`.

### Task 5: Automation `on_row_added` hook

**Files:**
- Create: `internal/automationhook/hook.go`, `hook_test.go`

**Interfaces:**
- Produces: `type Hook interface{ RowsAdded(ctx context.Context, tx pgx.Tx, workspaceID, workbookID string, rowIDs []int64) (enqueued int, err error) }`; `func New(enabled bool, log *slog.Logger) Hook` (disabled -> returns 0 immediately). Port of `events.emit_row_added`/`_enabled_rules`/`_rule_covers_workbook`/`_enqueue_eval` (`events.py:34-72,125-163`): enabled `on_row_added` triggers of the workspace whose `scope_workbook_ids` is empty or contains the workbook; per trigger one `trigger_eval` job with payload `{trigger_id, workspace_id, targets:[{workbook_id,row_id}], fire_source:"row_added", fire_key:"rowadd:<wb>:<sorted ids joined by ',' cut at 120>", dry_run:false}` inserted into the Postgres queue exactly as `queue_service.add_job` does (reuse the `jobs.sql` insert if present, else add `InsertTriggerEvalJob`).
- Failure semantics: errors are logged and swallowed (Python wraps the call in `try/except`), never fail the request.

- [ ] **Step 1: Write failing tests**: `TestDisabledEnqueuesNothing`, `TestEnqueuesOneJobPerCoveringRule`, `TestScopedRuleSkipsOtherWorkbook`, `TestFireKeyTruncatedAt120`, `TestPayloadMatchesPythonJob` (compare to a job row recorded from Python), `TestErrorIsSwallowedAndLogged`, `TestRulesTenantScoped`.
- [ ] **Step 2: Run** -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): automation row-added hook (RED)`, `feat(go): automation row-added enqueue hook`.

### Task 6: Static registries exported from Python

**Files:**
- Create: `legacy/python/scripts/export_workbook_registry.py`, `internal/wbregistry/{registry.go,registry.json,registry_test.go}`

**Interfaces:**
- Exporter writes `{"column_types": COLUMN_TYPES, "lead_fields": [keys of LEAD_FIELD_MAP], "ai_presets": {"presets": [...], "categories": [...]}, "providers": [{name, capabilities, confidence, <certification fields>}], "recipes": {"categories": RECIPE_CATEGORIES, "items": [full recipe dicts, validated via load_recipes()]}, "templates": {"categories": TEMPLATE_CATEGORIES, "items": get_templates()}}`; object order preserved (ordered decode in Go).
- Produces: `//go:embed registry.json`; `func Load() (*Registry, error)`; `(*Registry) ColumnTypes() json.RawMessage`, `LeadFields() []string`, `AIPresets(category string) (presets json.RawMessage, categories json.RawMessage)`, `Providers() []Provider`, `Recipes(category string) ([]Recipe, bool)` (`bool` false = unknown category), `Recipe(slug string) (Recipe, bool)`, `Templates(category string) []Template`, `Template(id string) (Template, bool)`, `RecipeSummary(Recipe) map[string]any` (same keys as `recipe_summary`).

- [ ] **Step 1: Write failing tests**: `TestRegistryEmbeddedMatchesPythonExportShape`, `TestRecipeOrderPreserved`, `TestEveryRecipeColumnPassesNormalize` (consumes Task 3 once merged; until then skip with a build tag-free `t.Skip` that Task 22 removes), `TestProviderCertificationFieldsPresent`, `TestPresetsFilterByCategory`, `TestRecipeSummaryKeysMatchFixture`.
- [ ] **Step 2: Run** -> FAIL. **Step 3: Implement** exporter and loader. **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): embedded workbook registries (RED)`, `feat(go): embed workbook registries exported from python`.

### Task 7: Service core and workbook store

**Files:**
- Create: `internal/db/queries/wb_workbooks.sql`, `internal/wbcrud/service.go`, `service_test.go`

**Interfaces:**
- sqlc: `GetWorkbookOwned` (`:one`, `FOR UPDATE` variant `GetWorkbookOwnedForUpdate`), `ListWorkbooks` (order `updated_at desc`), `InsertWorkbook`, `UpdateWorkbookMeta`, `UpdateWorkbookColumns`, `DeleteWorkbook`, `CountWorkbookRows`, `TouchWorkbook`.
- Produces: `type Service struct{ Pool *pgxpool.Pool; Leads LeadReader; Hook automationhook.Hook; Reg *wbregistry.Registry; Now func() time.Time }`; `type Workbook struct{...}` with `MarshalJSON` producing `WorkbookResponse` (`id,name,description,status,source_type,source_config,filter_criteria,columns_config,total_rows,completed_rows,sync_to_leads,created_at,updated_at,last_run_at`; naive ISO times; `description ""` when null, `status "draft"`, `source_type "leads_filter"` when null, `sync_to_leads` default true); `func (s *Service) Owned(ctx context.Context, tx pgx.Tx, ws, id string, forUpdate bool) (Workbook, error)` returning `ErrNotFound` (route maps to 404 `Workbook not found`); `type LeadReader interface{ QueryLeads(...); LeadsByIDs(...) }` (satisfied by Task 4); `func (s *Service) WithTx(ctx, ws string, fn func(pgx.Tx) error) error` (wraps `tenancy.InWorkspace`).
- `total_rows` rule: live `count(workbook_rows)` when > 0, else for legacy `leads_filter` workbooks (`source_type == "leads_filter"` and `source_config.row_storage_version != 2`) the lead query total, else `0`; on any error the stored `total_rows`.

- [ ] **Step 1: Write failing tests**: `TestOwnedOtherWorkspaceIsNotFound` (FORCE RLS plus explicit predicate), `TestOwnedForUpdateLocks` (second tx blocks until first commits), `TestWorkbookJSONShapeMatchesFixture`, `TestTotalRowsRules` (table: rows>0, legacy filter, v2 empty, count error), `TestUpdatedAtAdvancesOnUpdate`, `TestListOrderedByUpdatedAtDesc`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): workbook service core (RED)`, `feat(go): workbook store queries and service core`.

### Task 8: Row query engine and cursors

**Files:**
- Create: `internal/wbcrud/{cursor,rowquery}.go`, `cursor_test.go`, `rowquery_test.go`, `testdata/wbcrud/cursor_vectors.json` (generated from the Python encoders by `gen_cursor_vectors.py`)

**Interfaces:**
- Cursors (`workbooks.py:172-269`): `EncodeRowCursor(position int, id int64) string`, `DecodeRowCursor(string) (int, int64, *GuardError)` (400 `Invalid workbook row cursor`); `EncodeQueryCursor(values []any, id int64, scope string) string`, `DecodeQueryCursor(cursor string, expectedTerms int, scope string) ([]any, *GuardError)` (400 `Invalid workbook row cursor; restart pagination` on scope/shape mismatch); `EncodeConnectorRunCursor(createdAt time.Time, id string) string`, `DecodeConnectorRunCursor(string) (time.Time, string, *GuardError)` (400 `Invalid connector run cursor`); `CursorScope(view *string, terms []OrderTerm) string` (sha256 hex over the Python identity list).
- Query: `type RowQuery struct{ SQL string; Args []any; Order []OrderTerm }`; `func BuildRowQuery(wb Workbook, view *View, search string) (RowQuery, *GuardError)` replicating `_workbook_rows_query` and `_workbook_value_expression` (lead_field/input columns read `data->>(lead_field or id)`, others `enrichments->id->>'value'`; ops incl. `contains` with LIKE-escaping, `empty` = `trim(lower(coalesce(x,''))) = ''`; search = lower-cased `data::text` or `enrichments::text` contains, escaped; tie-breakers `position`, `id`; guard strings `409 Saved view references a missing filter column. Repair the view before continuing.` / `... sort column ...`, `404 Workbook view not found`); `func (q RowQuery) After(cursorValues []any) RowQuery` for keyset paging with correct NULL/empty ordering (`coalesce(expr,'')` as Python).

- [ ] **Step 1: Write failing tests**: `TestCursorsDecodePythonIssued` (vectors), `TestCursorsRejectTamperedAndWrongScope`, `TestEncodedCursorsDecodeInPython` (vector checked by a Python-computed expectation file), `TestFilterOpsMatchPythonResults` (seeded rows, each op, case-insensitive, `%` and `_` in the needle are literals), `TestSearchEscapesLikeWildcards`, `TestSortByColumnWithEmptiesIsStableAcrossPages` (page size 3 over 25 rows, concatenated pages equal the full ordered set), `TestViewOfOtherWorkbook404`, `TestMissingSortColumn409`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement** with parameterised SQL only (column ids and ops are never interpolated; JSON path keys are bound parameters). **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): row cursors and view query (RED)`, `feat(go): workbook cursors`, `feat(go): view-driven row query builder`.

### Task 9: Workbook create, list, update, delete, from-jobs

**Files:**
- Create: `internal/wbcrud/create.go`, `create_test.go`; modify `internal/api/workbookroutes.go` (stub from Task 2)

**Interfaces:**
- `GET /api/workbooks/` -> `{"workbooks":[WorkbookResponse],"total":n}` (`identity.Workspace`). `POST /api/workbooks/` (editor) body `WorkbookCreate` (`name` 1..255, `description` <= 2000, `source` default `empty`, `source_config`, `columns_config`, `filter_criteria`, `max_rows` 1..50000 default 1000) -> 201: duplicate column ids 422 `Column IDs must be unique`; `ValidateNewCycles([], cols)`; legacy compat (`filter_criteria` set and source `empty` -> `leads_filter`, `source_config = filter_criteria`); stored `source_config = {...source_config, "row_storage_version": 2}`; snapshot rows per source (`leads_filter` and `job_results` through `LeadReader` with `page_size=max_rows`, `csv` from `source_config.rows[:max_rows]`, `empty` none; row `data` = lead dict, `lead_id` = lead id, `enrichments = {}`, `position` = index); afterwards `Hook.RowsAdded` with all new row ids (errors swallowed). Returns the response built with the live row count.
- `PUT /api/workbooks/{id}` (editor), `DELETE /api/workbooks/{id}` (editor), `POST /api/workbooks/from-jobs` (editor, body `CreateFromJobsRequest` at `workbooks.py:453`): read `workbooks.py:460-572` and `946-1017` for the exact column-replacement guards (422 `Column IDs must be unique`, 409 `Replacement removes a referenced column. Update dependent columns first.`, 409 `Replacement removes a column referenced by saved view: <name>. Update the view first.`) and delete cascade (children via FK `ON DELETE CASCADE`; response body copied from source).
- Produces: `func (s *Service) Create(ctx, ws string, body CreateBody) (Workbook, error)` used by Task 22.

- [ ] **Step 1: Write failing tests**: `TestListMatchesFixture`, `TestTrailingSlashRedirect307` (`/api/workbooks` -> `/api/workbooks/`), `TestCreateEmptyMatchesFixture`, `TestCreateLeadsFilterSnapshotsRows` (rows, positions, `lead_id`), `TestCreateCsvTruncatesToMaxRows`, `TestCreateDuplicateColumnIDs422`, `TestCreateCycle422`, `TestCreateLegacyFilterCompat`, `TestCreateStoresRowStorageVersion2`, `TestCreateMaxRowsOutOfRange422`, `TestCreateFiresRowAddedHook` (fake hook), `TestHookFailureDoesNotFailCreate`, `TestViewerCannotCreate403`, `TestUpdateReplacementGuards`, `TestDeleteCascadesRowsViewsAndOtherTenantUntouched`, `TestMutationsAdvanceUpdatedAt`, `TestFromJobsMatchesFixture`.
- [ ] **Step 2: Run** -> FAIL. **Step 3: Implement** (record fixtures with Task 1's recorder first). **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): workbook create/list/update/delete (RED)`, `feat(go): workbook create and snapshot`, `feat(go): workbook list, update, delete, from-jobs routes`.

### Task 10: Workbook detail with paging, CSV export, connector runs

**Files:**
- Create: `internal/wbcrud/exportcsv.go`, tests; modify `internal/api/workbookdetail.go`

**Interfaces:**
- `GET /api/workbooks/{id}` query `page>=1`, `page_size 1..5000 (100)`, `view_id`, `search<=500`, `cursor_mode`, `cursor<=512`; response `WorkbookWithLeadsResponse` (`workbooks.py:574-727` and `schemas.py:157-243`: row objects with `enrichments` overlay, provenance and entity fields, `next_cursor`, totals). Legacy `leads_filter` workbooks with zero v2 rows fall back to the lead query exactly as Python. Offset paging when no cursor/`cursor_mode`; keyset when either is set (Task 8).
- `GET /{id}/export.csv` (`current_workspace`): `Unknown columns: <first 10 joined>` 400, `Selected export is limited to 1,000 rows` 400, streaming `text/csv` with the same header row, cell rendering and `Content-Disposition` filename as `workbooks.py:849-909`; formula-injection guard exactly as Python does (read it; copy the prefix set).
- `GET /{id}/connector-runs` (`current_workspace`): cursor `before` via Task 8 connector-run cursor, ordered `(created_at desc, id desc)`, response keys from `workbooks.py:910-945`.

- [ ] **Step 1: Write failing tests**: `TestDetailOffsetPageMatchesFixture`, `TestDetailCursorWalkCoversAllRowsOnce`, `TestDetailViewAndSearchApplied`, `TestDetailPageSizeBounds422`, `TestDetailLegacyLeadsFallback`, `TestDetailOtherWorkspace404`, `TestExportCsvMatchesFixtureBytes`, `TestExportSelectedOver1000Is400`, `TestExportUnknownColumns400`, `TestExportEscapesFormulaPrefixes`, `TestExportStreamsWithoutBufferingAllRows` (10k rows, bounded memory via a counting writer), `TestConnectorRunsPaginateAndRejectBadCursor`.
- [ ] **Step 2: Run** -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): workbook detail, export, connector runs (RED)`, `feat(go): workbook detail with offset and keyset paging`, `feat(go): workbook csv export and connector runs`.

### Task 11: Column routes

**Files:**
- Create: `internal/wbcrud/columnsvc.go`, tests; modify `internal/api/workbookcolumns.go`

**Interfaces:** (all editor-gated, `FOR UPDATE` on the workbook)
- `PATCH /{id}/columns/{column_id}/settings`, `PATCH /{id}/columns/order`, `PATCH /{id}/columns/{column_id}/width`, `POST /{id}/columns` (add), `DELETE /{id}/columns/{column_id}` with the bodies and exact guards in `workbooks.py:744-845,1201-1308`: `422 Provide supported changes and matching expected fields`, `404 Column not found`, `409 Column identity is ambiguous`, `409 Column settings changed. Refresh before saving again.`, `422 Invalid column settings`, `422 Column name must not be empty`, `409 Column order changed. Refresh before reordering.`, `422 Column order must contain each current column exactly once`, `422 Column requires a name, identity and width from 80 to 600`, `409 Column identity already exists. Refresh before adding another column.`, `409 Column changed since confirmation. Refresh and review before deleting.`, `409 Column is referenced by: <names>. Update those references before deleting.`, `409 Column is referenced by saved views: <names>. Update those views before deleting.`. Add/delete return `WorkbookResponse`. Route-literal precedence: `columns/order` must win over `columns/{column_id}/...` (ServeMux specificity, pinned by a test).

- [ ] **Step 1: Write failing tests**: one test per guard string above plus `TestOrderLiteralNotCapturedByColumnID`, `TestSettingsPatchRenameBlockedByReference`, `TestSettingsPatchCycleBlocked`, `TestWidthBounds`, `TestAddColumnCycle422`, `TestDeleteBlockedByView`, `TestDeleteRemovesColumnValuesFromRowsOrLeavesThemAsPython` (read `workbooks.py:1252-1308` and assert the same effect on `workbook_rows.enrichments`), `TestConcurrentColumnEditsSerialize` (two goroutines, second sees 409 settings-changed), `TestColumnsMatchFixtures`.
- [ ] **Step 2: Run** -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): column routes (RED)`, `feat(go): column settings, order and width`, `feat(go): add and remove column`.

### Task 12: Row routes

**Files:**
- Create: `internal/db/queries/wb_rows.sql`, `internal/wbcrud/rowsvc.go`, tests; modify `internal/api/workbookrows.go`

**Interfaces:** (editor)
- `POST /{id}/rows` body `AddRowsRequest` (`rows` min 1, `dedupe` default true; identity = `_identity` at `workbooks.py:2067` over domain/company, skipping rows whose identity already exists or repeats), `DELETE /{id}/rows` body `{row_ids min 1}`, `POST /{id}/rows/delete-query` body `{expected_count>=1, confirmation<=64, view_id?, search?}` (409 on drift per `workbooks.py:2162-2170`; `400 confirmation must equal "<required>"`), `PATCH /{id}/rows/{row_id}` (404 `Workbook row not found`, 400 `No editable row fields supplied`), `PATCH /{id}/rows` bulk (`updates` 1..1000; 400 `Duplicate row IDs are not allowed`, 404 `One or more workbook rows were not found`, 400 `Row <id> contains non-editable fields`; one transaction, `_changed_row_fields` JSON-equality diff so unchanged values do not stamp or emit), `PUT /{id}/leads/{lead_id}` (400 `No valid lead fields to update`, 404 `Lead not found`). `position` for new rows = current max + 1 ascending; new rows fire `Hook.RowsAdded`.

- [ ] **Step 1: Write failing tests**: `TestAddRowsDedupeSkipsExistingAndRepeated`, `TestAddRowsAppendsPositions`, `TestAddRowsFiresHookWithNewIDs`, `TestDeleteRowsOnlyOwnWorkbook`, `TestDeleteQueryDriftIs409`, `TestDeleteQueryConfirmationMismatch400`, `TestDeleteQueryHonoursViewAndSearch`, `TestPatchRowEditableFieldsOnly`, `TestPatchRowFalseVsZeroIsAChange`, `TestBulkPatchAtomicOnForeignID`, `TestBulkPatch1001Is422`, `TestBulkPatchDuplicateIDs400`, `TestBulkPatchUnchangedValuesNotStamped`, `TestUpdateLeadFieldAllowlist`, `TestRowsTenantIsolation`, `TestRowsMatchFixtures`.
- [ ] **Step 2: Run** -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): row routes (RED)`, `feat(go): add and delete workbook rows`, `feat(go): row and bulk row patch, lead field update`.

### Task 13: CSV import preview and import

**Files:**
- Create: `internal/wbcrud/importsvc.go`, tests, `testdata/wbcrud/csv_vectors.json` (from `gen_csv_vectors.py` over `services/workbook/csv_import.py`); modify `internal/api/workbookimport.go`

**Interfaces:**
- `POST /{id}/import/preview` (editor; Python handler is sync) and `POST /{id}/import` (editor) with `ImportRowsRequest` (`rows` 1..50000, `mapping`, `create_columns` default true, `dedupe`, `file_name<=255`, `source_system` in `auto|clay|generic`). Port `normalize_header`, `column_key`, `infer_target`, `prepare_csv_import`, `analyze_csv_import` exactly (read `csv_import.py` fully); errors `400 CSV has no importable rows`, `400 CSV has more than 500 columns`, 404 `Workbook not found`; import runs in one transaction, creates editable columns (cycle/identity guards), dedupes by the same identity rule as Task 12, fires `Hook.RowsAdded`, returns the keys at `workbooks.py:1088-1200`.

- [ ] **Step 1: Write failing tests**: `TestHeaderNormalisationVectors` (unicode, BOM, whitespace, duplicates, `Clay` exports), `TestInferTargetVectors`, `TestPreviewMatchesFixture`, `TestImportCreatesColumnsAndRows`, `TestImportMappingOverridesInference`, `TestImportDedupeWithinFileAndAgainstWorkbook`, `TestImport501Columns400`, `TestImport50001Rows422`, `TestImportAllEmptyRows400`, `TestImportIsAtomicOnFailure` (inject an error on the last batch), `TestImportFiresHookOnce`, `TestImportMemoryBounded` (50k rows).
- [ ] **Step 2: Run** -> FAIL. **Step 3: Implement** with batched `COPY`/multi-row insert (batch size 1000). **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): csv import vectors (RED)`, `feat(go): csv header and mapping inference`, `feat(go): csv import preview and import routes`.

### Task 14: Saved views routes

**Files:**
- Create: `internal/db/queries/wb_views.sql`, `internal/wbcrud/viewsvc.go`, tests; modify `internal/api/workbookviews.go`

**Interfaces:**
- Under `/api/v2/workbooks/{id}/views`: `GET` (`current_workspace`) -> `{"views":[WorkbookViewResponse],"total":n}` ordered `(created_at, id)`; `POST` (editor) -> 201; `PUT /{view_id}` (editor); `DELETE /{view_id}` (editor) -> `{"status":"deleted"}`. View response: `{id, workbook_id, name, config:{filters:[{column,op,value}],sort:[{column,dir}],hidden_columns:[]}, created_at, updated_at}` (config always fully expanded with defaults). Guards: 404 `Workbook not found`, 404 `View not found` (wrong workbook or workspace), 422 view-column guard (Task 3). PUT stamps `updated_at` explicitly.

- [ ] **Step 1: Write failing tests**: `TestViewsListOrderedOldestFirst`, `TestCreateExpandsConfigDefaults`, `TestCreateMissingColumn422`, `TestPutRenameOnlyKeepsConfig`, `TestPutConfigValidated`, `TestViewOfOtherWorkbook404`, `TestDeleteKeepsRows`, `TestViewerForbiddenOnWrites`, `TestBadOp422Shape`, `TestViewsMatchFixtures`.
- [ ] **Step 2: Run** -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): saved views routes (RED)`, `feat(go): saved views routes`.

### Task 15: Reusable functions

**Files:**
- Create: `internal/db/queries/wb_functions.sql`, `internal/wbfunctions/store.go`, tests; modify `internal/api/functionsroutes.go`

**Interfaces:**
- Global table `workbook_functions` via the plain pool. `GET /api/functions?category=` -> `{"functions":[...],"total":n}`; `GET /api/functions/{func_id}` (404 `Function not found`); `POST /api/functions` (global admin; body `{name, description="", category="custom", columns_chain: list[dict]}`) -> the created function dict as `services/workbook/functions.py:create_function` returns it (read it for keys, `version` 1, `usage_count` 0, float epoch `created_at/updated_at`); `DELETE /api/functions/{func_id}` (global admin) -> `{"status":"ok"}` (idempotent, as Python); `POST /api/functions/{func_id}/apply` (`require_editor`, body `{workbook_id}`): 404 `Workbook not found` when not in the caller's workspace, 404 `Function not found`, appends chain columns whose `id`/`key` is not already present, bumps `usage_count`, returns `{"applied": name, "columns_added": n, "total_columns": m}`.
- Note: apply does NOT run cycle validation in Python; Go matches (parity), recorded in the fixture README as an existing gap.

- [ ] **Step 1: Write failing tests**: `TestListFilterByCategory`, `TestGetUnknown404`, `TestCreateRequiresGlobalAdmin`, `TestCreateShapeMatchesFixture`, `TestDeleteUnknownStillOK`, `TestApplyAppendsOnlyNewColumns`, `TestApplyOtherWorkspaceWorkbook404`, `TestApplyBumpsUsageOnce`, `TestApplyFunctionNeedsEditorRole`, `TestFunctionsTableIsGlobalAcrossWorkspaces`.
- [ ] **Step 2: Run** -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): functions routes (RED)`, `feat(go): reusable workbook functions routes`.

### Task 16: Entity store and read routes

**Files:**
- Create: `internal/db/queries/entities.sql`, `internal/entities/store.go`, tests; modify `internal/api/entitiesroutes.go` (GET routes only; Task 17 adds the writes in `entitiesroutes_write.go`)

**Interfaces:**
- `GET /api/entities/company?min_corroboration>=1 (1)&limit<=1000 (100)` -> `{"entities":[CompanyEntity.to_api()]}` ordered `corroboration_count desc`; `GET /api/entities/company/{id}` (404 `Entity not found`); `GET /api/entities/review-queue?status=pending&limit<=1000` -> `{"pairs":[{id,entity_id,candidate,score,reason,status}]}` joined to the caller's entities ordered `score desc`. `to_api()` key set from `services/entities/models.py` (read it, 214 LOC). All `current_workspace`.
- Produces: `entities.Store{Pool}`: `ListCompanies`, `GetCompany`, `ReviewQueue`.

- [ ] **Step 1: Write failing tests**: `TestCompanyJSONMatchesToAPI` (fixture), `TestListMinCorroborationAndLimitBounds422`, `TestGetOtherWorkspace404`, `TestReviewQueueJoinsTenantEntitiesOnly`, `TestReviewQueueStatusFilter`, `TestEntitiesFORCERLS`.
- [ ] **Step 2: Run** -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): entity read routes (RED)`, `feat(go): entity store and read routes`.

### Task 17: Entity merge, split, review decision

**Files:**
- Create: `internal/entities/{merge,split}.go`, tests; Create `internal/api/entitiesroutes_write.go` registered from `RegisterEntities` (one added line in Task 16's file by this task)

**Interfaces:**
- `POST /api/entities/merge` body `{kept_id, merged_id, reason="manual"}` (editor): 400 with the Python `error` string on failure; `POST /api/entities/split` body `{merge_log_id:int}` (editor); `POST /api/entities/review-queue/decide` body `{pair_id:int, decision}`: 404 `Review pair not found`, 400 `No candidate entity to merge`, merge -> `{"status":"merged", ...merge result}`, anything else -> `{"status":"rejected"}` (parity: any non-`merge` decision rejects).
- Port `merge_entities` (463-553), `split_entity` (604-663) and the helpers `_repoint_account_references`, `_restore_account_references`, `_repoint_person_references`, `_restore_person_references`, `_restore_company_evidence`, `_recompute` (graph.py) with their exact `error` strings and merge-log snapshots (JSON must be restorable by Python's `split_entity` and vice versa).

- [ ] **Step 1: Write failing tests**: `TestMergeThenPythonSplitRestores` and `TestPythonMergeThenGoSplitRestores` (snapshot JSON vectors), `TestMergeWritesLogAndRepointsAccountsAndPeople`, `TestMergeCrossWorkspaceIDs400NothingWritten`, `TestMergeSameIDError`, `TestSplitUnknownLog400`, `TestSplitOtherWorkspaceLog400`, `TestDecideMergeUsesCandidateNewEntityID`, `TestDecideRejectAnyOtherDecision`, `TestDecidePairOfOtherWorkspace404`, `TestMergeConcurrentSamePairOnce` (row locks as `_lock_for_update`), `TestEntityWritesMatchFixtures`.
- [ ] **Step 2: Run** -> FAIL. **Step 3: Implement** in one transaction per call. **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): entity merge/split (RED)`, `feat(go): entity merge`, `feat(go): entity split and review decision routes`.

### Task 18: System stats and files (hardened)

**Files:**
- Create: `internal/sysfiles/files.go`, tests; modify `internal/api/systemroutes.go`

**Interfaces:**
- `GET /api/system/stats` -> `{disk_total, disk_used, disk_free, project_size, db_size}` (statfs on `/`, walk of the project root skipping symlinks, size of the SQLite file named by `DATABASE_URL` when it starts with `sqlite:///`, else `0`); `GET /api/files/list?path=` -> list of `{name,path,is_dir,size,modified,type}` sorted folders first then `modified` desc (type buckets as `file_manager.get_file_type`; names starting with `.` hidden; missing dir -> `[]`); `GET /api/files/download/{path...}`; `DELETE /api/files/delete/{path...}` -> `{"status":"success","message":"File deleted"}`. Status/detail: traversal 403 `Access denied` (download) or 403 with the `ValueError` text `Invalid path: Access denied` (list/delete); `404 File not found` / `404 File not found or could not be deleted`; other failures 500 with a generic `Internal error` instead of `str(e)` (deviation D1).
- Hardened mode (default, `FILES_API_MODE=hardened`): all four routes behind `Authenticate`, delete/list/download additionally `RequireGlobalAdmin`; paths resolved with `filepath.Rel` against the cleaned data dir after `EvalSymlinks` (rejects `..`, absolute paths, sibling-prefix dirs, symlink escapes); `delete` refuses the data dir root and removes directories only when empty or when `recursive=true` is not supplied -> 400 `Directory is not empty` (deviation). `legacy` mode reproduces Python (no auth, prefix check, `rmtree`) for diffing only and logs a startup warning.

- [ ] **Step 1: Write failing tests**: `TestStatsShape`, `TestListSortingAndHiddenFiles`, `TestListMissingDirIsEmpty`, `TestDownloadStreamsFileWithFilename`, `TestTraversalRejected` (table: `../x`, `/etc/passwd`, `a/../../x`, encoded `%2e%2e`, sibling `data-evil` prefix, symlink to `/etc`), `TestHardenedRequiresAuthAndAdmin` (anonymous 401, non-admin 403), `TestDeleteRefusesRootAndNonEmptyDir`, `TestLegacyModeMatchesPythonFixtures` (non-traversal cases only), `TestNoInternalErrorTextLeaks`.
- [ ] **Step 2: Run** -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): system and files routes (RED)`, `feat(go): hardened data-dir file operations`, `feat(go): system stats and files routes`.

### Task 19: Budget, cost, runs, activity, cell trace

**Files:**
- Create: `internal/db/queries/wb_telemetry.sql`, `internal/wbcrud/telemetry.go`, tests; modify `internal/api/workbooktelemetry.go`

**Interfaces:**
- `PUT /{id}/budget` (editor, `{max_usd>=0, finite}`) -> `{budget_max_usd, budget_spent_usd}`; `GET /{id}/cost` -> `{budget_max_usd, budget_spent_usd (4dp), reserved_usd, uncertain_usd, remaining_usd (6dp or null when cap<=0), accounting_basis:"catalog_estimates_and_recorded_charges", unlimited}` with exposure = sum of `reserved_microusd` by status (`reserved`+`dispatched` = reserved, `uncertain`) / 1e6; `GET /{id}/runs?before_id>=1&limit 1..100 (20)` -> `{runs,has_more,next_before_id}` from `jobs` where `type='run_workbook'` and `payload->>'workbook_id'` matches (fields and rules as `workbooks.py:1577-1627`: `selected_cell_count`, `summarize_output_attempts`, retry-count result masking, UTC ISO with offset); `GET /{id}/activity?limit<=500 (50)` -> `{"activity":[to_api()]}`; `GET /{id}/rows/{lead_id}/cells/{col_id}/trace` (404 `No trace for this cell`). Reads `current_workspace`; budget write editor.
- Produces `func SelectedCellCount(rows, columns any, rowColumns map[string][]string) *int` ported from `cell_scope.selected_cell_count` (+ vectors) and `SummarizeOutputAttempts` from `output_attempts.py` (read and port; vectors from Python).

- [ ] **Step 1: Write failing tests**: `TestSelectedCellCountVectors`, `TestSummarizeOutputAttemptsVectors`, `TestCostExposureMath` (reserved+dispatched vs uncertain, unlimited -> `remaining_usd` null), `TestBudgetNegative422AndNaN422`, `TestRunsPaginationAndMasking` (previous attempt result hidden when `retry_count` differs), `TestRunsOnlyThisWorkbookAndTenant`, `TestActivityLimitBound`, `TestTrace404`, `TestTelemetryMatchesFixtures`.
- [ ] **Step 2: Run** -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): cost, runs, activity, trace (RED)`, `feat(go): budget and cost routes`, `feat(go): runs, activity and cell trace reads`.

### Task 20: Meta routes, `/api/me/context`, `/api/flags`

**Files:**
- Create: tests; modify `internal/api/workbookmeta.go`

**Interfaces:**
- `GET /api/workbooks/meta/column-types`, `/meta/lead-fields` (`{"fields":[...]}`), `/meta/providers` (`{"providers":[{name,capabilities,confidence,<certification keys>}]}`), `/meta/ai-column-presets?category=` (`{"presets","categories"}`), `/meta/filter-options` (via `leadsread.FilterOptions`), all `current_workspace` and served from Task 6/Task 4. Route precedence: `meta/*` literals must win over `/{workbook_id}` (pinned test).
- `GET /api/me/context` -> `{user_id:str, workspace_id, role (member_role or "member"), is_owner: role=="owner"}`; `GET /api/flags` -> `{automations_enabled, intent_poller_enabled, pg_lead_store, allow_legacy_outreach}` from `Deps.Flags` (`pg_lead_store` is the effective value: true here by the Task 2 gate).

- [ ] **Step 1: Write failing tests**: `TestMetaRoutesMatchFixtures`, `TestMetaLiteralsBeatWorkbookID`, `TestAIPresetsCategoryFilter`, `TestFilterOptionsTenantScoped`, `TestMeContextRoleAndOwner`, `TestMeContextMemberFallback`, `TestFlagsReflectConfig`, `TestMetaRequiresWorkspace` (no `X-Workspace-Id` and no active -> 403 text from plan 02).
- [ ] **Step 2: Run** -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): workbook meta, me context, flags (RED)`, `feat(go): workbook meta, me context and flags routes`.

### Task 21: Analytics

**Files:**
- Create: `internal/analytics/overview.go`, tests; modify `internal/api/analyticsroutes.go`

**Interfaces:**
- `GET /api/analytics/{overview,pipeline,collection,enrichment,llm}` (`current_workspace`), Postgres-store branch only (`analytics.py:33-70` for overview; read each route's `c is None` branch): overview `{total_leads, leads_this_week:0, leads_this_month:0, avg_score, tiers, enrichment:{total,with_email,with_phone,with_website,with_contact,with_linkedin,email_pct,phone_pct,website_pct,contact_pct}, ...}` with `round(x/total*100, 1)` using Python rounding (banker's: `round(0.25*100/..)` cases pinned) and `0` (int) when `total == 0`; pipeline/collection/enrichment/llm return the same degraded-empty shapes Python returns on the PG store. The file-only trend sections stay empty (existing Python behaviour, deliberate, noted in the fixture README).

- [ ] **Step 1: Write failing tests**: `TestOverviewMatchesFixture`, `TestPctBankersRounding` (table incl. 1 of 8, 1 of 24, `total == 0` -> int `0`), `TestOtherRoutesEmptyShapesMatchFixtures`, `TestAnalyticsTenantIsolation`, `TestAnalyticsRequiresWorkspace`.
- [ ] **Step 2: Run** -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): analytics routes (RED)`, `feat(go): analytics routes on the postgres lead store`.

### Task 22: Templates

**Files:**
- Modify: `internal/api/templatesroutes.go`; modify `internal/wbregistry/registry_test.go` (remove the skip from Task 6); create tests

**Interfaces:**
- `GET /api/templates/gallery?category=` -> `{"recipes":[summary],"categories","total"}` (unknown category 404 `Unknown category '<c>'`); `GET /api/templates/gallery/{slug}` (404 `Recipe not found`) full recipe; `POST /api/templates/gallery/{slug}/instantiate` (editor, optional body `{name 1..255}`) -> 201 via `Service.Create` with `source="empty"`, name override, `description = recipe.workbook.description or recipe.description`, columns normalised by Task 3 (same response as `POST /api/workbooks/`); `GET /api/templates` -> `{"templates","categories","total"}`; `POST /api/templates/{template_id}/create` (editor) -> `{id,name,template,columns_count,enrichment_columns_count}` building editor columns `{id: key, name, type:"lead_field", lead_field: key, width:180}` plus `enrichment_columns` appended verbatim with the three 422 guards (`Template enrichment column IDs must be explicit and unique. Repair the template before creating a workbook.`, `Template column IDs must be unique`, `Template has circular column dependencies. Repair the template before creating a workbook.`), 404 `Template not found`; the workbook gets `filter_criteria = template.filter` and does NOT set `row_storage_version` (parity).
- Route precedence: `gallery` literals beat `{template_id}`.

- [ ] **Step 1: Write failing tests**: `TestGalleryListAndCategoryFilter`, `TestUnknownCategory404`, `TestRecipeDetailMatchesFixture`, `TestInstantiateMatchesCreateResponse`, `TestInstantiateNameOverride`, `TestInstantiateViewerForbidden`, `TestLegacyTemplateListMatchesFixture`, `TestCreateFromTemplateColumnsAndCounts`, `TestCreateFromTemplateGuards` (each of the 3 strings via crafted registry), `TestEveryShippedRecipeInstantiates` (all 19 recipes pass normalisation and cycle checks), `TestGalleryLiteralsBeatTemplateID`.
- [ ] **Step 2: Run** -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): templates routes (RED)`, `feat(go): templates gallery and legacy template routes`.

### Task 23: Contract coverage, nginx split, runbook

**Files:**
- Create: `internal/api/workbookcrud_contract_test.go`, `testdata/contract/retained_workbook_routes.txt`, `docs/go-port/runbooks/workbook-crud-cutover.md`; modify `infra/nginx/go-routes.conf`, `testdata/contract/README.md` (deviations)

**Interfaces:**
- Consumes: `contract.Load`, `AssertEqual`, `IgnorePaths`; plan 02 `go-split.conf` canary map and `scripts/nginx-routing-check.sh`.
- nginx (same `$auth_upstream` canary map; header block copied from existing locations): regex locations in this order, first match wins, retained routes first and always `api_backend`: `~ ^/api/workbooks/(meta/provider-(stats|accuracy)$|[^/]+/(run(/estimate)?|stop|sources(/.*)?|refresh(-policy)?|migrate|ws)$|[^/]+/rows/[^/]+/cells/[^/]+/run$)`, `~ ^/api/v2/workbooks/[^/]+/(generate-column|ingest.*)$` (ingest paths per `routers/ingest.py`), then Go: `~ ^/api/workbooks(/|$)`, `~ ^/api/v2/workbooks/[^/]+/views`, `^~ /api/templates`, `^~ /api/functions`, `^~ /api/entities`, `^~ /api/analytics`, `= /api/me/context`, `= /api/flags`, `= /api/system/stats`, `^~ /api/files/`. Regex locations do not honour prefix priority, so the retained block must precede the Go block and the check script asserts both. WebSocket upgrade headers stay on the retained `/ws` location.
- Runbook content: prerequisites (`PG_LEAD_STORE=true`, plan 01/02/03 cut over, `AUTOMATIONS_ENABLED` consistent between stacks), canary order (read-only routes first: meta, analytics, entities reads, functions reads; then workbook reads; then writes), per-prefix percentage, comparison dashboards, cross-stack checks (create in Go, run/refresh on Python, read in Go), rollback (percentage 0; no data rollback; rows written by Go are Python-readable), the deliberate deviations list, and `FILES_API_MODE` guidance.

- [ ] **Step 1: Write failing tests**: `TestWorkbookRoutesMatchRecordedFixtures` (table over every fixture dir; replays `request` against the Go handler on the seeded DB, `AssertEqual` with `IgnorePaths("$.created_at","$.updated_at")` only where the fixture is flagged volatile), `TestEveryMigratedRouteHasAFixture` (route table minus `retained_workbook_routes.txt` equals fixture set), `TestRetainedRoutesNotRegistered` (the 14 retained routes plus 2 ingest routes 404 from Go), `TestFixturesCoverEveryGuardString` (greps Task 9-22 guard strings in the fixtures).
- [ ] **Step 2: Run** `make test-integration` -> FAIL.
- [ ] **Step 3: Record** any missing fixtures with the recorder (`--cases scripts/contract_cases/<area>.yaml`), write the nginx block, runbook, deviations.
- [ ] **Step 4: Run** `make vet test test-integration` and `scripts/nginx-routing-check.sh` at 0% and 100%: retained paths always `py`, ported paths follow the bucket, `/api/workbooks/x/run` stays `py`, `/api/v2/workbooks/x/views` goes to Go -> PASS.
- [ ] **Step 5: Commit** `test(go): workbook CRUD contract fixtures and coverage (RED)`, then `feat(infra): nginx per-route split for workbook CRUD`, `docs(go-port): workbook CRUD cutover runbook and deviations`.

---

## Self-Review

- **Spec coverage:** inventory row "workbooks router CRUD, views, templates, functions, entities, analytics, meta, system/files": workbooks CRUD/columns/rows/import/export/views/telemetry/meta (Tasks 3, 7-14, 19, 20), templates (6, 22), functions (15), entities (16, 17), analytics (4, 21), meta + me/flags (20), system/files (18). 35 workbooks-router routes ported, 14 retained and listed; websocket and Redis explicitly retained. RLS: every tenant table inside `InWorkspace` with isolation tests; `workbook_functions` global.
- **Consistency:** `Column`, `ViewConfig`, `GuardError` (Task 3), `Service`, `Workbook`, `Owned`, `WithTx` (Task 7), `RowQuery`/cursor funcs (Task 8), `LeadReader` (Task 4 satisfies Task 7), `Hook` (Task 5), `Service.Create` (Task 9, used by Task 22) are each defined once and referenced by name later.
- **Decisions (open, defaults chosen):** D1 file routes hardened (auth, traversal fix, non-recursive delete, no `str(e)` leak) instead of parity; D2 Go requires the PG lead store; D3 the 14 enrichment/LLM/Redis workbook routes and 2 ingest routes stay on Python until plans 05/06, so nginx splits per route; D4 `on_row_added` is ported as the enqueue half only and shares `AUTOMATIONS_ENABLED`; D5 analytics keeps the existing degraded PG shapes; D6 functions `apply` keeps Python's lack of cycle validation.
- **Proportion:** interfaces, guard strings and test names carry the decisions; handler and SQL bodies are left to the implementer.
