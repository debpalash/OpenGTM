# Connectors and CRM Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Port the connector and CRM surface to Go: the 19 routes of `connectors` (2), `hubspot` (4), the legacy `/api/data` email-data CRM (7) and `ambitionbox` (6); Go clients for HubSpot, Salesforce, Airtable, Google Sheets, Instantly, Smartlead and AmbitionBox; the signed-certification and connector-catalog logic; the `ambitionbox_import` job handler with its failure reconciliation; and the shared building blocks the automations plan (08) and the destinations plan consume (`crmpush`, `rowtmpl`, `sourceingest`, queue failure hooks), so that no tenant CRM/connector request needs Python except what stays behind the worker boundary.

**Architecture:** Plain-HTTP vendor clients live in small packages (`internal/crm`, `internal/integrations/*`, `internal/ambitionbox`) behind the existing `egress` SSRF guard and the `secrets.Resolver` (per-workspace secret, then global settings, then env, exactly as `get_secret`). Row-shaping logic is ported once (`rowtmpl` = `_get_lead_values` + `_resolve_prompt`; `crmpush` = `output._push_crm`; `sourceingest` = `upsert_source_records`) and reused by later plans. `ambitionbox_import` becomes a Go job (same `jobs` row, `executor='go'` by flag) that keeps the page-cursor/`connector_runs` protocol byte-compatible so a Python worker can resume a Go-started run and vice versa. Routes follow the plan 04/06 conventions (stub file per route group, recorded FastAPI fixtures, flag-gated registration, nginx per-route split).

**Tech Stack:** Go 1.27.3 stdlib (`net/http`, `encoding/json`, `crypto/hmac`, `crypto/ed25519` via plan 06 `manifest`), pgx v5 + sqlc, go-redis v9 (existing), existing `internal/{httpx,identity,tenancy,audit,contract,queue,egress,secrets,credentials,leadsread,automationhook,manifest,billing,canonjson}`; no new Go modules. Python/uv for recorder, seed, vectors and transcript scripts.

**Spec:** `docs/go-port/inventory.md` section 8 row 6 "Connectors + CRM" (sections 2 HTTP surface, 3 job types, 5 data stores, 7 integrations, 9 risks); `docs/go-port/plans/04-workbook-crud.md` (conventions, `connector_runs` reads, `automationhook`), `05-leads-signals.md` (`leadsread`), `06-enrichment-jobs.md` (`manifest`, `pyworker`, `queue.EnqueueOnce`, `billing`, ingest routes, `source_workbook` stays Python). Python sources: `apps/api/routers/{connectors,crm,hubspot,ambitionbox}.py`, `services/connectors/contracts.py`, `services/crm/{__init__,hubspot,salesforce}.py`, `services/integrations/{airtable,sheets,instantly,smartlead,certification}.py`, `services/workbook/{ambitionbox_import,source_ingest,crm_import,output}.py`, `services/leadgen/ambitionbox.py`, `services/leadgen/enrichment/declarative/manifest.py`, `services/workbook/ai_column.py` (template resolver), `services/queue_service.py` (failure handlers).

**Layout note (branch `go-rewrite`):** Python lives untracked at `legacy/python/`; Python paths (`apps/api/...`, `scripts/...`) mean `legacy/python/<path>`. Go paths are relative to `apps/backend-go/`. Module `github.com/debpalash/OpenGTM/apps/backend-go`.

**Prerequisite:** plans 01-05 merged; plan 06 Waves 0-2 merged (`manifest` registry and signing, `billing`, `pyworker`, `queue.EnqueueOnce`, `colgraph.ReferenceIndexes`, `colcompute` Python-`str()` helpers). Plan 06 Wave 4 (worker edits) merged before Task 13 touches `cmd/worker/main.go`. No new tables: `connector_runs`, `email_data`, `links`, `workbook_rows.source_*` already exist in `internal/db/schema.sql` (plan 04). No Alembic task and no `RequiredGoSchemaRevision` bump.

## Global Constraints

- Go `go 1.27` + `toolchain go1.27.3`; pgx v5 + sqlc only; `make sqlc-check` clean. Go never runs DDL. No new modules.
- Tenant tables only inside `tenancy.InWorkspace`. `email_data` and `links` are legacy GLOBAL tables (no `workspace_id`, no RLS); they are reachable only through the global-admin gate and only by the 7 `/api/data` routes (recorded in Review Focus 5). Cross-workspace or unknown ids answer 404, never 403, except where Python answers otherwise and the fixture says so.
- Contract parity is the release bar: paths, methods, JSON key order, status codes, `detail` strings, auth gates, 422 bodies (`httpx.ValidationError` shapes recorded from FastAPI). Gates verbatim: `connectors/*` and `hubspot status|contacts` = `current_workspace`; `hubspot connect|sync` = `identity.RequireRole("activation.write","editor","admin")`; `ambitionbox` reads = `current_workspace`, `POST /imports` = `identity.RequireRole("","owner","admin","editor")` (no permission string, as Python); `/api/data/*` = router-level `identity.RequireGlobalAdmin()` (403 `Not authorized`) on every route. Dependency-before-body precedence (403/401 before 422) is taken from the recorded fixtures.
- Behaviour parity of job output is the release bar: `connector_runs` columns and cursor JSON, workbook `source_config` mutations, `workbook_rows` positions and `_source` JSON, Redis `source_page` event, `trigger_eval` enqueue on rows added (via plan 04 `automationhook`). Vendor behaviour is pinned by Python-recorded HTTP transcripts (request and result), never hand-written, never the internet.
- Hardening deviates from Python only as a documented deviation (D2-D6) in `testdata/contract/README.md`: SSRF-guarded egress on every user-influenced URL (Salesforce `SALESFORCE_INSTANCE_URL`, webhook-like fields), bounded bodies (1 MiB) and page counts, crash bugs answered as 4xx (see Review Focus 4), no secret in any error string, log line, fixture or receipt.
- `AUTOMATIONS_ENABLED`, `CRM_IMPORT_ENABLED` and every other Python flag keep their names and defaults in `internal/config`.
- Vendor calls: 10 s timeout (HubSpot/Airtable/Sheets/Instantly/Smartlead as Python), AmbitionBox 15 s with 3 attempts, `crm.RequestWithRetry` = `crm.request_with_retry` (429 only, `Retry-After` capped at 15 s, 3 extra attempts). A context cancel stops retries immediately.
- Redis in tests: `TEST_REDIS_URL` (127.0.0.1:56380); integration tests skip when unset. HTTP clients are tested against `cmd/providersim` or recorded transcripts.
- Verify with `cd apps/backend-go && make sqlc-check vet test` and `make test-integration`. No CI changes.
- Every commit DCO signed (`git commit -s`), no `Co-Authored-By` trailer. GRANULAR commits: tests first (RED, message ends `(RED)`), then each implementation piece. Stage explicit paths only; `make sqlc` regenerates all of `internal/db/gen/`, so each task stages only its own `<name>.sql.go` and `.sql`.
- Stays on FastAPI after this plan (documented in `testdata/contract/retained_connector_routes.txt`, Task 19): the `crm_import` source materialisation (runs inside `source_workbook`, plan 06 D6, flag `CRM_IMPORT_ENABLED`), the `destinations`/`audiences` consumers of the integration clients (their plan), output-column execution (`output.execute_output_column`, destinations plan), `POST /api/v2/workbooks/{id}/ingest-token` and `/rows/ingest` (ported by plan 06 Task 30, not here).

## Review Focus

1. **Cross-tenant CRM credentials and data.** `get_secret` falls back from the workspace secret to the global settings and then to the process environment. A HubSpot/Salesforce/Airtable token for workspace A must never serve workspace B, and the global fallback must stay exactly as Python (single-tenant installs) and must be visible in tests. `hubspot/sync` reads leads only through the caller's tenant (`leadsread`), never through a lead id of another workspace (404 semantics: the id is silently dropped, as Python filters `None`). Tasks 5, 10, 11, 16.
2. **SSRF through user-set hosts.** `SALESFORCE_INSTANCE_URL` is a per-workspace secret the user types; Python concatenates it into a request URL with no guard. Go validates it (https only, public address, no redirects, dial-time IP check via `internal/egress`) on every call and answers a per-run error, never a 500. HubSpot, Airtable, Sheets, Instantly, Smartlead and AmbitionBox use fixed hosts and must refuse redirects to other hosts. Tasks 5, 6, 9, 10.
3. **Run state and cursor fidelity.** `ambitionbox_import` commits each page and its cursor in one transaction; a crash between page fetch and commit re-fetches only that page; two workers (Go and Python, or a retry) never both advance the same page (the `next_page` re-check), never double-insert (`uq_workbook_row_source_identity` plus the `source_ingest` advisory lock), and the `uq_connector_runs_active` index still gives the 409 `connector_run_active`. A hand-back or lease loss leaves the run `retrying`, never `running` forever. Tasks 8, 13.
4. **Crash bugs answered as client errors.** Python 500s: `GET /api/data?sort_by=<non-column attribute>` (getattr on the ORM class), `GET /api/ambitionbox/companies|collect?location=...` and `company_type` (a `NotImplementedError` from the client), `POST /imports` with a foreign or unknown `workbook_id` or a non-AmbitionBox workbook without `replace_existing` (`ValueError`), an upstream AmbitionBox 4xx/5xx or timeout (`RuntimeError`), `hubspot/sync` with `limit` far above the lead count. Each gets a defined 4xx/502 with a recorded deviation (D4). Tasks 12, 14-18.
5. **The global `/api/data` table.** `email_data` rows are not tenant scoped, `mark-used` without ids and filters updates every row, and `DELETE` of a missing id answers 200 `{"status":"not found"}`. Parity is kept (global admin only); the global-admin gate must be applied by the route group, not per handler, so a forgotten gate is a test failure (`TestEveryDataRouteRequiresGlobalAdmin` over `Router.Patterns()`). Tasks 14, 17.
6. **Catalog signature trust.** `connectors/catalog` shows `signature` status, `package_sha256` and certification maturity; a tampered manifest or an expired/forged certification must never render as `trusted`/`certified`. The Go HMAC and manifest hash must equal the Python bytes. Tasks 4, 12.

## File Structure

```
internal/certification/{certs,catalogs}.go                  # integration_catalog, certification_statuses, attest, required checks
internal/connectors/{report,catalog}.go                     # validate_manifest_directory report + /catalog /compatibility payloads
internal/crm/{retry,hubspot,salesforce}.go                  # request_with_retry, HubSpot and Salesforce clients (+ field maps)
internal/crmpush/{push,fields,rowid}.go                     # output._push_crm / _crm_update_fields / _store_row_crm_id
internal/integrations/{airtable,sheets,instantly,smartlead}/*.go
internal/rowtmpl/{values,resolve}.go                        # _get_lead_values, _resolve_prompt, _resolve, _resolve_deep
internal/sourceingest/{upsert,record}.go                    # upsert_source_records + ConnectorRecord/Page
internal/ambitionbox/{client,types}.go                      # AmbitionBoxClient (search, jobs, detail, similar, page, collect)
internal/ambitionimport/{start,handler,reconcile,rows}.go   # start_ambitionbox_import, handle_ambitionbox_import, failure reconcile
internal/emaildata/{service,csv}.go                         # /api/data list, mark-used, update, delete, export, stats
internal/queue/failure.go                                   # failure-hook registry + notice after Finalize (Task 3)
internal/queue/finalize.go                                  # (modify, Task 3)
internal/config/config.go                                   # (modify, Task 3: flags)
cmd/worker/main.go                                          # (modify, Tasks 3 and 13)
internal/api/{connectorsroutes,hubspotroutes,datacrmroutes,ambitionboxroutes}.go
internal/api/register.go                                    # (modify once, Task 3)
internal/db/queries/{emaildata,ambitionimport,sourceingest,hubspot_leads}.sql (+ gen/)
testdata/contract/{connectors,hubspot,datacrm,ambitionbox}/*.json
testdata/contract/retained_connector_routes.txt
testdata/vendors/<crm|airtable|sheets|instantly|smartlead|ambitionbox>/*.json   # Python-recorded transcripts
../../legacy/python/scripts/{contract_seed.py,contract_cases/connectors.yaml,gen_connector_vectors.py,record_vendor_transcripts.py}
../../legacy/python/apps/api/services/{go_executor.py,workbook/ambitionbox_import.py}   # (modify, Task 2)
../../infra/nginx/go-routes.conf
../../docs/go-port/runbooks/connectors-crm-cutover.md
```

## Parallel waves

Tasks in a wave touch disjoint files and may run concurrently (up to 6 agents at a time). Each wave starts only after the previous wave is merged and `make vet test test-integration` is green.

| Wave | Tasks | Shared-file notes |
|---|---|---|
| 0 (sequential) | 1 Python seed, cases, vectors, transcripts; 2 Python executor routing for `ambitionbox_import`; 3 Go scaffold, config, register lines, stubs, queue failure hooks | T1 owns `contract_seed.py` (append-only), the three new Python scripts and every fixture/vector/transcript file; T2 owns `go_executor.py` and `ambitionbox_import.py`; T3 owns `register.go`, `config.go`, `queue/{failure,finalize}.go`, `cmd/worker/main.go` (hook call only), all stub route files |
| 1 | 4 certification, 5 crm core + HubSpot, 6 integrations (4 clients), 7 rowtmpl, 8 sourceingest, 9 AmbitionBox client | 6 disjoint packages; 5 owns `internal/crm/{retry,hubspot}.go` and `hubspot_leads.sql`; 8 owns `sourceingest.sql` |
| 2 | 10 Salesforce, 11 crmpush, 12 connectors catalog, 13 ambitionimport (job + start + worker wiring), 14 emaildata service | 10 adds `internal/crm/salesforce.go` only; 11 consumes 5, 7, 10 (start 11 after 10 merges or stub the interface); 13 alone edits `cmd/worker/main.go` and owns `ambitionimport.sql` |
| 3 | 15 connectors routes, 16 hubspot routes, 17 `/api/data` routes, 18 ambitionbox routes | each owns exactly one stub `internal/api/*.go` and one fixture dir |
| 4 (sequential) | 19 coverage, nginx split, runbook | consumes everything |

---

### Task 1: Python seed, recorder cases, vectors, vendor transcripts

**Files:**
- Modify: `legacy/python/scripts/contract_seed.py` (append-only)
- Create: `legacy/python/scripts/contract_cases/connectors.yaml`, `legacy/python/scripts/gen_connector_vectors.py`, `legacy/python/scripts/record_vendor_transcripts.py`
- Create (output, committed): `testdata/contract/{connectors,hubspot,datacrm,ambitionbox}/*.json`, `internal/{certification,connectors,crm,crmpush,rowtmpl,sourceingest,ambitionbox,ambitionimport,emaildata}/testdata/*.json`, `testdata/vendors/<vendor>/*.json`

**Interfaces:**
- Consumes: plan 04/06 seed (workspaces A/B, editor/viewer, `wb-alpha-*`, `wb-beta-1`, global admin user).
- Produces: seed additions: a global-admin session; 30 `links` + 60 `email_data` rows (accents, NULL names, mixed-case domains, `is_used` mix, `created_at` ISO strings across 45 days); workspace A secret `HUBSPOT_TOKEN`, workspace B none; workspace A `SALESFORCE_*`; a connector manifest directory fixture (trusted, untrusted, invalid-signature, unsigned, duplicate-name, HTTP-url, bad-name) and a certification file with a valid, an expired and a forged entry (`OPENGTM_INTEGRATION_CERTIFICATION_KEY` set only for the recorder); a workbook `wb-alpha-ab` (source_type `ambitionbox`) and `wb-alpha-1` for `POST /imports`; one `connector_runs` row `pending` for the 409 case. Vector files: `certification` (attest, validity window, build-sha mismatch, required checks per subject), `connector_report` (every error string of `validate_manifest_directory`), `crm_maps` (HubSpot `DEFAULT_FIELD_MAP`, `_map_import_properties`, Salesforce `DEFAULT_FIELD_MAP`/`IMPORT_DEFAULT_FIELDS`/`CONTACT_UPDATE_FIELD_MAP`, `_soql_escape`, `_record_value`, `_map_import_record`), `crmpush` (`_crm_update_fields` incl. `contact_person` split, update-vs-create path, 404-recreate), `rowtmpl` (60 cases: exact, case-insensitive, underscore-space, ambiguous, unknown strict, `str()` of None/0/False/float/list), `source_ingest` (add/update/skip, reset_rows positions), `ambitionbox` (`company_to_row`, `_dedupe_companies`, `_industry_slug`, `ABCompany.from_api`, page→`ConnectorPage`), `ambitionbox_cursor` (page sequences: target met, exhausted, max pages, replace_existing, seen-id carry-over, retry after crash), `emaildata_csv` (quote and newline escaping). Transcripts per vendor: HubSpot (status, search-by-email, create, update, batch, contacts, 401, 429+Retry-After, 5xx, malformed JSON), Salesforce (SOQL, create Lead, update Contact, 404, 401, injection string), Airtable, Sheets (append, upsert, A1 column naming), Instantly, Smartlead (duplicate detection), AmbitionBox (search, jobs, detail, similar, 429/5xx retry, 400).

- [ ] **Step 1: Extend the seed** (fixed ids and timestamps; recorder sets a fixed `Retry-After` and a recording `httpx`/`aiohttp` transport so no network).
- [ ] **Step 2: Write the cases** (each `{name, method, path, as, profile, expect_status}`): connectors catalog and compatibility (member, no auth 401, signed/unsigned/policy `required`); hubspot status (not configured, configured, upstream 401), connect (editor, viewer 403, empty token 422), sync (`lead_ids`, `tier`, default, no email, not connected 400, foreign lead id, `limit` 0/-1/100000), contacts (not connected 400, ok, `limit` bounds); `/api/data` list (defaults, `q`, dates, `is_used`, `sort_by` valid/unknown/`metadata`, `order`, `limit=0`, offset), mark-used (ids, bulk filter, empty everything), patch (ok, 404 `Record with id N not found`, unknown field ignored), delete (ok, not found), export (CSV bytes, filename pattern), stats timeline and domains (`limit`), non-admin 403 on every route; ambitionbox companies (filters, `limit>20` 422, `location` NotImplemented), jobs, detail, similar, collect (`pages`/`limit` bounds, 422), imports (new, existing AmbitionBox workbook, non-AmbitionBox without and with `replace_existing`, foreign workbook, running run 409 `connector_run_active`, viewer 403, `sort_by` pattern 422, `limit` bounds 422).
- [ ] **Step 3: Write the vector and transcript scripts**; they import the real Python modules.
- [ ] **Step 4: Run** `uv run python scripts/contract_record.py --cases scripts/contract_cases/connectors.yaml`, `uv run python scripts/gen_connector_vectors.py`, `uv run python scripts/record_vendor_transcripts.py`; Expected: files written, re-running is byte-identical, no token string appears in any output file (a grep in the script fails the run).
- [ ] **Step 5: Commit** (Go/doc side only) `test(go): connector and crm contract fixtures, vectors and vendor transcripts (RED)`.

### Task 2: Python executor routing for ambitionbox_import

**Files:**
- Modify: `legacy/python/apps/api/services/workbook/ambitionbox_import.py` (enqueue site only), `legacy/python/apps/api/services/go_executor.py`, `legacy/python/apps/api/core/config.py`
- Test: `legacy/python/tests/test_executor_routing_ambitionbox.py`

**Interfaces:**
- Produces: setting `GO_EXECUTOR_AMBITIONBOX: bool = False`; `go_executor.ambitionbox_executor(workspace_id) -> "go"|"python"` (= `GO_EXECUTOR_ENABLED` and workspace in `GO_EXECUTOR_WORKSPACES` and `GO_EXECUTOR_AMBITIONBOX`); `start_ambitionbox_import` passes `executor=` to `queue_service.add_job`. `handle_ambitionbox_import` and `reconcile_ambitionbox_job_failure` stay in Python (a Python worker still resumes any `executor='python'` run; the Python reaper still calls the Python failure handler for a dead Go-executed job, which is correct because the handler only touches `connector_runs` and `workbooks`).

- [ ] **Step 1: Write failing tests**: `test_ambitionbox_enqueue_executor_follows_flag`, `test_ambitionbox_default_stays_python`, `test_reconcile_handler_unchanged_for_go_job`.
- [ ] **Step 2: Run** `uv run pytest tests/test_executor_routing_ambitionbox.py` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** (unversioned; note in the PR).

### Task 3: Go scaffold: config, register lines, stubs, queue failure hooks

**Files:**
- Modify: `internal/api/register.go` (+`RegisterConnectors`, `RegisterHubspot`, `RegisterDataCRM`, `RegisterAmbitionBox` lines), `internal/config/config.go`, `internal/queue/finalize.go`, `cmd/worker/main.go` (hook invocation only)
- Create: `internal/queue/failure.go`, stub files `internal/api/{connectorsroutes,hubspotroutes,datacrmroutes,ambitionboxroutes}.go` (`func registerX(r *httpx.Router, d Deps)`, empty body), `internal/db/queries/{emaildata,ambitionimport,sourceingest,hubspot_leads}.sql` (empty headers only)
- Test: `internal/api/connectorscaffold_test.go`, `internal/queue/failure_integration_test.go`

**Interfaces:**
- Produces: config `GO_CONNECTOR_ROUTES_ENABLED` (default false), `GO_EXECUTOR_AMBITIONBOX` (default false), `CERTIFICATION_PATH`/`CERTIFICATION_KEY`/`BUILD_SHA` read from `OPENGTM_INTEGRATION_CERTIFICATIONS`, `OPENGTM_INTEGRATION_CERTIFICATION_KEY`, `OPENGTM_BUILD_SHA`, `CRM_IMPORT_ENABLED` (read-only mirror); `RegisterX` registers nothing and logs when `PG_LEAD_STORE` or `GO_CONNECTOR_ROUTES_ENABLED` is false. `queue.FailureHook func(ctx context.Context, n FailureNotice) error` with `FailureNotice{JobID int64; JobType, WorkspaceID string; Payload []byte; Reason string; WillRetry bool}`; `queue.RegisterFailureHook(jobType string, h FailureHook)`; `queue.FinalizeNotify(ctx, pool, c, o) (persisted, willRetry bool, err error)` (the existing `Finalize` keeps its signature and delegates). The worker calls the registered hook only when the failed outcome was persisted (lease not lost, not cancelled), after the queue transition commits, exactly like `queue_service._process_job` (`failure_handlers`); a hook error is logged and never changes the job outcome.

- [ ] **Step 1: Write failing tests**: `TestConnectorRoutesNotRegisteredWithoutFlags`, `TestConnectorRoutePatterns` (exactly the 19 routes of Tasks 15-18), `TestFailureHookCalledAfterPersistedFailure`, `TestFailureHookReportsWillRetry` (retry vs final), `TestFailureHookNotCalledOnLostLease`, `TestFailureHookNotCalledOnCancelled`, `TestFailureHookErrorIsSwallowed`, `TestFinalizeSignatureUnchanged`.
- [ ] **Step 2: Run** `make test test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): connector scaffold and queue failure hooks (RED)`, `feat(go): connector route scaffold and config flags`, `feat(go): queue failure hooks after persisted failed outcome`.

### Task 4: Certification and integration catalogs

**Files:**
- Create: `internal/certification/{certs,catalogs}.go`, tests, `internal/certification/testdata/` (Task 1 vectors)

**Interfaces:**
- Produces: `type Subject struct{...}`; `func StatusFor(subjects []string, builds map[string]string, o Options) map[string]Status` (= `certification_statuses`); `func IntegrationCatalog`, `SignalSourceCatalog`, `AgentCapabilityCatalog`, `AudienceCapabilityCatalog`, `GovernanceCapabilityCatalog` (each = the Python function of the same name; consumed by later plans); `func Attest(cert map[string]any, key string) (map[string]any, error)`; `func RequiredChecks(subjectID string) []string`; `type Options struct{ Path, Key, BuildSHA string; Now func() time.Time }` (empty fields read the three env names; `MAX_CERTIFICATION_LIFETIME_DAYS = 92`). HMAC payload bytes equal `_payload` (canonical JSON via `canonjson`, `hmac.Equal`).
- A missing, unreadable or malformed certification file yields every subject `uncertified` and never errors the caller (as Python).

- [ ] **Step 1: Write failing tests**: `TestStatusVectors`, `TestAttestMatchesPythonBytes`, `TestExpiredAndFutureCertificateRejected`, `TestBuildShaMismatchRejected`, `TestForgedSignatureRejected`, `TestLifetimeOver92DaysRejected`, `TestUnknownSubjectRejected`, `TestRequiredChecksPerSubject`, `TestMissingFileIsUncertified`, `TestIntegrationCatalogMatchesVector`, `TestKeyNeverInOutput`.
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): certification (RED)`, `feat(go): certification validation and status`, `feat(go): integration and capability catalogs`.

### Task 5: CRM core and HubSpot client

**Files:**
- Create: `internal/crm/{retry,hubspot}.go`, `internal/db/queries/hubspot_leads.sql` (lead rows by ids / tier / limit for sync), tests, `internal/crm/testdata/` hubspot vectors

**Interfaces:**
- Consumes: `egress` client constructor, `secrets.Resolver`, `leadsread` Lead type.
- Produces: `func RequestWithRetry(ctx, c *http.Client, req func() (*http.Request, error), maxRetries int, sleep func(context.Context, time.Duration) error) (*http.Response, error)` (429 only; `Retry-After` float seconds else `2^attempt`; capped `MaxRetrySleep = 15s`; the response body of a retried 429 is drained and closed); `type HubSpot struct{ HTTP *http.Client; Secrets *secrets.Resolver; BaseURL string }` with `IsConnected(ctx, ws)`, `TestConnection(ctx, ws) map[string]any` (key order `connected, portal_id, time_zone` or `connected, error`), `PushLeadAsContact(ctx, ws, lead, fieldMap) PushResult` (search by email, update or create; `Action` `created|updated`), `PushLeadsBatch(ctx, ws, leads, fieldMap) SyncResult{Created, Updated, Failed int; Errors []string}` (150 ms spacing, context aware), `UpdateContactByID(ctx, ws, id, props) UpdateResult{Success, NotFound bool; Error string}`, `FetchContacts(ctx, ws, limit int, listID string, ...)` and `GetContacts(ctx, ws, limit) ([]map[string]any, error)`; exported `DefaultFieldMap`, `ImportDefaultProperties`; `MapImportProperties`. Token read per call, never cached across workspaces, never in an error string (errors use the vendor `HTTP <status>: <body[:200]>` text exactly as Python, with the `Authorization` header never echoed).
- `BaseURL` defaults to `https://api.hubapi.com`; tests point it at `cmd/providersim`. Redirects refused.

- [ ] **Step 1: Write failing tests**: `TestRequestWithRetryHonorsRetryAfterAndCaps`, `TestRetryExponentialFallback`, `TestRetryStopsOnContextCancel`, `TestHubSpotTranscripts` (every recording: status, push create/update, batch, update-by-id incl. 404 `NotFound`, contacts, 401, 429, 5xx, malformed JSON), `TestDefaultFieldMapMatchesVector`, `TestMapImportPropertiesMatchesVector`, `TestTokenResolvedPerWorkspace`, `TestGlobalFallbackMatchesGetSecret`, `TestNoSecretInErrors`, `TestRedirectRefused`, `TestBatchStopsOnContextCancel`.
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test` -> PASS.
- [ ] **Step 5: Commit** `test(go): crm core and hubspot (RED)`, `feat(go): crm retry helper`, `feat(go): hubspot client`.

### Task 6: Airtable, Sheets, Instantly, Smartlead clients

**Files:**
- Create: `internal/integrations/{airtable,sheets,instantly,smartlead}/*.go`, tests per package

**Interfaces:**
- Produces (one package each, ports of `services/integrations/*.py`): `airtable.PushRecord/UpsertRecord(ctx, ws, fields, baseID, table, ...)`, `sheets.AppendRow/UpsertRow(ctx, ws, spreadsheetID, values, ...)` with `cell()` and A1 `columnName`, `instantly.AddLeadToCampaign`, `smartlead.AddLeadToCampaign` (duplicate detection helpers), each with `IsConnected(ctx, ws)`; result maps keep Python key order (`success`, `record_id`/`action`/`error`). Path segments (`baseID`, `table`, spreadsheet id, campaign id) are `url.PathEscape`d (Python concatenates raw; a `../` is a documented hardening, D4). Fixed hosts, redirects refused, 10 s timeout, 1 MiB response bound.

- [ ] **Step 1: Write failing tests** per package against transcripts: `TestAirtablePushTranscripts`, `TestAirtableUpsertCreatesOrUpdates`, `TestSheetsCellFormattingVectors` (formula-injection guard exactly as `_cell`), `TestSheetsColumnName`, `TestSheetsUpsertTranscripts`, `TestInstantlyTopLevelFieldsAndCustom`, `TestInstantlyDuplicateDetection`, `TestSmartleadDuplicateCount`, `TestPathSegmentsEscaped`, `TestNoSecretInErrors`, `TestRedirectRefused`, `TestTenantTokenIsolation`.
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3: Implement** (4 packages; one commit each). **Step 4: Run** `make vet test` -> PASS.
- [ ] **Step 5: Commit** `test(go): integration clients (RED)`, `feat(go): airtable client`, `feat(go): sheets client`, `feat(go): instantly client`, `feat(go): smartlead client`.

### Task 7: Row template resolver

**Files:**
- Create: `internal/rowtmpl/{values,resolve}.go`, tests, `internal/rowtmpl/testdata/rowtmpl.json` (Task 1)

**Interfaces:**
- Consumes: plan 06 `colgraph.ReferenceIndexes(ref string, cols []Column) []int` and the `colcompute` Python-`str()` helper (`PyStr(any) string`).
- Produces: `func CellText(v any) string` (= `cell_text`: nil -> `""`, else Python `str`); `func LeadValues(leadData map[string]any, cols []Column) map[string]string` (= `_get_lead_values`, including the alias-then-values precedence and `lead_field` aliasing); `func ResolvePrompt(tmpl string, values map[string]string, strict bool) (string, error)` (= `_resolve_prompt`; errors `Ambiguous column reference: <key>. Use an exact column ID.` and `Unknown column reference: <key>. Fix the template before sending.`, non-strict `[<key>: not found]`); `func Resolve(tmpl string, leadData map[string]any, cols []Column) (string, error)` (= `output._resolve`, calling `ValidateTemplateReferences` first); `func ResolveDeep(v any, leadData map[string]any, cols []Column) (any, error)` (dict/list/str, key order preserved). Bounds: template <= 64 KiB, <= 256 placeholders, resolved output <= 1 MiB (error beyond; Python is unbounded, D4). Regex is `\{([^}]+)\}` with Go RE2 semantics tested equal on the vector (no backtracking difference).

- [ ] **Step 1: Write failing tests**: `TestRowTmplVectors` (60), `TestAmbiguousReference`, `TestStrictUnknown`, `TestUnicodeCaseFolding` (Python `.lower()` vs Go: Turkish dotted I, German sharp s must equal the vector), `TestResolveDeepPreservesKeyOrder`, `TestPyStrOfNonStrings`, `TestTemplateBoundsEnforced`.
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): row template resolver (RED)`, `feat(go): row values and template resolution`.

### Task 8: Source ingest upsert

**Files:**
- Create: `internal/db/queries/sourceingest.sql`, `internal/sourceingest/{upsert,record}.go`, `upsert_integration_test.go`

**Interfaces:**
- Produces: `type Record struct{ Provider, RecordID string; Data map[string]any; Rank int; SourceURL *string; FetchedAt time.Time }`; `type Page struct{ Provider string; Records []Record; Page, PageSize int; SourceTotal, TotalPages *int; HasMore bool }` with `NewPage(...)` (`has_more` = `page < total_pages` else `len(records) >= page_size`); `type Summary struct{ Added, Updated, Skipped int; AddedRowIDs []int64 }`; `func Upsert(ctx, tx pgx.Tx, workbookID, workspaceID, provider string, recs []Record, resetRows bool) (Summary, error)` = `upsert_source_records`: `pg_advisory_xact_lock(hashtext('source_ingest:<wb>:<provider>'))`, optional delete of all rows, existing rows by `(provider, record_id)`, positions `max+1` (or `rank` when reset), `_source` JSON key order `provider, record_id, rank, url, fetched_at`, merge `{**old, **new}`, changed-detection, skipped for blank/mismatched/duplicate ids. It runs in the caller's transaction (it does not commit).

- [ ] **Step 1: Write failing tests**: `TestUpsertMatchesVectors`, `TestUpsertConcurrentSamePageInsertsOnce` (8 goroutines), `TestResetRowsPositionsFollowRank`, `TestSourceJSONKeyOrder`, `TestMismatchedProviderSkipped`, `TestRollsBackWithCallerTx`, `TestTenantIsolation`, `TestPageHasMoreRule`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): source ingest upsert (RED)`, `feat(go): connector record and page types`, `feat(go): source records upsert`.

### Task 9: AmbitionBox client

**Files:**
- Create: `internal/ambitionbox/{client,types}.go`, tests

**Interfaces:**
- Produces: `type Client struct{ HTTP *http.Client; Base string; Sleep func(context.Context, time.Duration) error }`; `SearchCompanies(ctx, SearchParams) (map[string]any, error)`, `FetchCompanyPage(ctx, PageParams) (sourceingest.Page, error)` (body `isFilterApplied, page (string), sortBy, limit 20, industries (slugs), ratings`; `ErrFilterUnsupported{Field}` for `location` and `company_type` with Python's message), `GetCompanyJobs(ctx, id, page)`, `GetCompanyDetail`, `GetSimilarCompanies`, `CollectCompanies(ctx, requested, maxPages, industry, location) (Collection, error)` with `Collection.Summary(includeRecords bool)`; `ABCompany`/`ABJob` with `FromAPI` and `ToDict` key order; `industrySlug`, `slugify`. Requests: 3 attempts, retry only on 429/5xx/network/timeout with Python's backoff, 15 s total timeout, 4xx fails immediately with `AmbitionBox API <status>: <body[:200]>`; response bounded to 4 MiB; `Base` fixed to the AmbitionBox gateway (tests use `providersim`); redirects refused. Errors are typed (`*UpstreamError{Status int}`, `ErrFilterUnsupported`) so routes map them (D4).

- [ ] **Step 1: Write failing tests**: `TestAmbitionBoxTranscripts` (search, page, jobs, detail, similar, 429-then-200, 5xx exhausted, 400 immediate), `TestFetchCompanyPageBodyKeyOrder`, `TestIndustrySlugVectors`, `TestCompanyFromAPIVectors`, `TestFilterUnsupportedLocationAndCompanyType`, `TestCollectStopsAtTargetOrMaxPages`, `TestCollectSummaryMatchesVector`, `TestContextCancelStopsRetries`, `TestResponseBodyBounded`.
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): ambitionbox client (RED)`, `feat(go): ambitionbox types and mapping`, `feat(go): ambitionbox client with retry and collection`.

### Task 10: Salesforce client

**Files:**
- Create: `internal/crm/salesforce.go`, `internal/crm/salesforce_test.go`

**Interfaces:**
- Consumes: Task 5 `RequestWithRetry`; `egress`.
- Produces: `type Salesforce struct{ HTTP *http.Client; Secrets *secrets.Resolver }`; `IsConnected`, `FetchContacts(ctx, ws, limit int, where string, ...) (FetchResult, error)`, `UpdateRecord(ctx, ws, sobject, id string, fields map[string]string) UpdateResult`, `PushLeadAsContact(ctx, ws, lead, fieldMap) PushResult` (returns `salesforce_id`, `action` `created|updated`; the create path makes a `Lead` sObject, as Python), exported `DefaultFieldMap`, `ImportDefaultFields`, `ContactUpdateFieldMap`, `APIVersion = "v59.0"`, `SOQLEscape`, `recordValue`, `mapImportRecord`. The instance URL is validated per call with `egress.CheckURL(raw, false)` (https, public host, no userinfo, dial-time check); a failure is a per-run `{"success": false, "error": "blocked url: ..."}` (D3). Token never in errors.

- [ ] **Step 1: Write failing tests**: `TestSalesforceTranscripts` (query, create, update, 404 `not_found`, 401, 429), `TestSOQLEscapeVectors` (injection strings from Task 1), `TestSearchByEmailUsesEscape`, `TestFieldMapsMatchVectors`, `TestInstanceURLPrivateAddressBlocked` (127.0.0.1, 169.254.169.254, IPv4-mapped IPv6, userinfo, http scheme), `TestInstanceURLRebindingBlockedAtDial`, `TestNoSecretInErrors`, `TestTenantCredentialsIsolated`.
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): salesforce client (RED)`, `feat(go): salesforce client`.

### Task 11: CRM push (output destination logic)

**Files:**
- Create: `internal/crmpush/{push,fields,rowid}.go`, `internal/crmpush/push_integration_test.go`

**Interfaces:**
- Consumes: Tasks 5, 7, 10; plan 04 workbook row access (`wbcrud` row read/update inside `tenancy.InWorkspace`).
- Produces: `type Config struct{ Type string; FieldMap map[string]string }` (parsed from `destination_config`, `type` default `hubspot`, lower-cased); `type Result struct{ Success bool; Value string; Error string }` (JSON key order `success, value, error`); `func Push(ctx, d Deps, ws string, cfg map[string]any, leadData map[string]any, workbookID string, leadID int64) Result` = `output._push_crm`: unsupported type -> `unsupported crm '<x>'`; not connected -> `<Label> not connected`; write-back by `crm_external_id` + matching `crm_type` (update only mapped fields, `contact_person` split into first/last), 404 falls through to create and stamps the fresh id back onto the row (`crm_external_id`, `crm_type`, `crm_object` `contact` for HubSpot, `lead` for Salesforce) only when the row still carries the stale id; success values `HubSpot: updated <id>` / `<Label>: <action> <id>`; `func UpdateFields(crmType string, cfg, leadData map[string]any) map[string]string` = `_crm_update_fields`. The row stamp is best-effort and logged, never fails the push (as Python).

- [ ] **Step 1: Write failing tests**: `TestPushVectors`, `TestUnsupportedCRM`, `TestNotConnectedMessage`, `TestUpdateByExternalIDOnlyMappedFields`, `TestContactPersonSplit`, `TestNotFoundFallsThroughToCreate`, `TestFreshIDStampedOnlyIfStaleKeyMatches` (integration), `TestStampFailureDoesNotFailPush`, `TestCrossWorkspaceRowNotStamped`, `TestTokenOfOtherWorkspaceNeverUsed`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): crm push (RED)`, `feat(go): crm update fields and push`, `feat(go): stamp fresh crm id on rows`.

### Task 12: Connector catalog and compatibility report

**Files:**
- Create: `internal/connectors/{report,catalog}.go`, tests

**Interfaces:**
- Consumes: plan 06 `manifest` (`Verify`, `CanonicalManifest`, `TrustStore`), Task 4 `certification`, `secrets.Resolver`.
- Produces: `func ValidateDirectory(dir, policy string, trust manifest.TrustStore) Report` equal to `validate_manifest_directory` (rules and error strings in order: name regex `[a-z][a-z0-9_]{2,63}`, duplicate id, method in GET/POST/PUT/PATCH, `connector request URL must use HTTPS`, non-empty `response.mappings`, confidence 0..1 and cost >= 0, timeout in (0,120], `authenticated connectors require auth.env_var`, signature `invalid|untrusted` or policy `required` without `trusted`; policy other than `optional|required` becomes `required`); `Report{OK bool; ManifestVersion string; SignaturePolicy string; Count int; Connectors []Entry; Errors []ReportError}` with Python key order; `func Catalog(ctx, d Deps, ws string) (CatalogResponse, error)` (`manifest_version, signature_policy, total, connectors`; per entry `configured` = auth `none` or a stored secret, `auth_type`, `signature`, `package_sha256`, plus certification maturity keys; `credential_key` removed from entries); `func Compatibility(report Report) Report` (strips `credential_key`). Manifest directory is walked without following symlinks (`..` or a symlink escaping the root is skipped and reported as an error entry, D4).

- [ ] **Step 1: Write failing tests**: `TestValidateDirectoryVectors` (every error string), `TestCatalogMatchesFixture`, `TestConfiguredReflectsWorkspaceSecret`, `TestOtherWorkspaceSecretNotConfigured`, `TestSignatureStatusesRendered`, `TestPackageSHAEqualsPython`, `TestCertificationMaturityKeys`, `TestCredentialKeyNeverReturned`, `TestSymlinkOutsideRootRejected`, `TestPolicyRequiredRejectsUnsigned`.
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): connector catalog (RED)`, `feat(go): manifest directory validation report`, `feat(go): connector catalog and compatibility payloads`.

### Task 13: ambitionbox_import job, start and failure reconcile

**Files:**
- Create: `internal/db/queries/ambitionimport.sql`, `internal/ambitionimport/{start,handler,reconcile,rows}.go`, `internal/ambitionimport/*_integration_test.go`
- Modify: `cmd/worker/main.go` (register handler + failure hook)

**Interfaces:**
- Consumes: Tasks 3, 8, 9; plan 04 `automationhook`; plan 06 `progress`/Redis broadcast (`workbook:<id>` channel); `queue.EnqueueOnce`/`queue.Enqueue`.
- Produces: `type StartParams struct{ WorkspaceID, Name string; Industry, Rating *string; SortBy string; Limit int; WorkbookID *string; ReplaceExisting bool }`; `func Start(ctx, d Deps, p StartParams) (StartResult, error)` = `start_ambitionbox_import` in ONE transaction (row-lock the workbook `FOR UPDATE`, active-run check -> `*AlreadyRunningError{RunID}`, non-AmbitionBox workbook without `replace_existing` -> `ErrNeedsReplace`, foreign/unknown workbook -> `ErrWorkbookNotFound`, `max_pages = min(25, max(1, ceil(limit/20)+5))`, `limit` clamped 1..500, `AMBITIONBOX_COLUMNS` verbatim, `connector_runs` row `pending` with cursor `{"next_page":1,"reset_done":false,"seen_record_ids":[]}`, workbook `source_config` and `status`, job `ambitionbox_import` priority 2 with `executor` from `GO_EXECUTOR_AMBITIONBOX`+cohort, workbook insert for the new case; `StartResult` key order `workbook_id, name, run_id, job_id, status, requested_limit, source, url`); a unique-index conflict (`uq_connector_runs_active`) re-reads the active run and returns `AlreadyRunningError`. `func Handle(ctx, d Deps, c *queue.Claim) error` = `handle_ambitionbox_import` (page loop: mark running, fetch page OUTSIDE any transaction, then one transaction: re-check `next_page`, `Upsert`, update cursor/counters/status/`source_config`, commit; `automationhook.RowsAdded` after commit for added rows; Redis `source_page` event `{type, connector, runId, page, added, total, done}` best-effort; on error mark the run `retrying` with `error[:1000]` and return the error). `func Reconcile(ctx, d Deps, n queue.FailureNotice) error` = `reconcile_ambitionbox_job_failure` (`retrying` vs `failed`, workbook `running` vs `draft`, `source_config.run_status/last_error/target_met`, no-op when `complete|partial`), registered with `queue.RegisterFailureHook("ambitionbox_import", ...)`. Lease: `queue.Heartbeat` per page; a lost lease or cancel stops before the next fetch; per-page fetch has the 15 s client timeout and the job has a 900 s deadline (`JOB_TIMEOUTS`).
- A payload without `workspace_id`/`workbook_id`/`run_id` fails with `ambitionbox_import requires workspace_id, workbook_id, run_id`. A `connector run or workbook not found` error is final (no retry storm): returned as a permanent failure.

- [ ] **Step 1: Write failing tests**: `TestStartMatchesVectors`, `TestStartResultKeyOrder`, `TestStartRowLocksAndRejectsActive` (concurrent 8 starts -> 1 run, 7 `AlreadyRunning`), `TestStartNonAmbitionBoxNeedsReplace`, `TestStartForeignWorkbookNotFound`, `TestStartEnqueuesWithExecutorFlag`, `TestStartRollsBackJobOnFailure`, `TestHandleCursorSequenceMatchesVectors` (target met, exhausted, max pages, replace_existing, seen carry-over), `TestHandleCrashBeforeCommitRefetchesPage`, `TestHandleTwoWorkersAdvanceOncePerPage`, `TestHandleResumesPythonStartedRun`, `TestHandleRowsAddedHookAfterCommit`, `TestHandleBroadcastsSourcePage`, `TestHandleRedisDownDoesNotFail`, `TestHandleErrorLeavesRetrying`, `TestHandleStopsOnLeaseLoss`, `TestReconcileRetryVsFinal`, `TestReconcileNoopOnComplete`, `TestReconcileTenantScoped`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): ambitionbox import (RED)`, `feat(go): ambitionbox import start`, `feat(go): ambitionbox import page loop`, `feat(go): ambitionbox failure reconcile and worker wiring`.

### Task 14: Email data service (`/api/data`)

**Files:**
- Create: `internal/db/queries/emaildata.sql`, `internal/emaildata/{service,csv}.go`, `internal/emaildata/service_integration_test.go`

**Interfaces:**
- Consumes: pool (no `tenancy.InWorkspace`: legacy global tables, runtime role grants verified in Task 19).
- Produces: `type Filter struct{ SourceLinkID *int64; Q *string; StartDate, EndDate *string; IsUsed *bool; SortBy, Order string }`; `List(ctx, f Filter, limit, offset int) (Page{Total int64; Items []Item}, error)` (left join `links` for `source_url`; `ILIKE %q%` on name or email; `created_at` compared as TEXT like the String column; `sort_by` allow-list `id,name,email,source_link_id,created_at,is_used,tags,notes`, anything else -> `id` (Python falls back for unknown names and crashes for non-column attributes, D4); `limit == 0` -> `{"total": n, "items": []}`; `Item` JSON key order = ORM column order then `source_url`); `MarkUsed(ctx, req) (count int64, err)` (ids, else bulk filter incl. `is_used_filter`, else every row); `Update(ctx, id, patch)` with `exclude_unset` semantics (explicit `null` sets NULL) returning the full row; `Delete(ctx, id) (found bool)`; `Export(ctx, f, w io.Writer)` streaming in 1000-row batches with Python's escaping (`"` doubled; `notes` newlines -> space; `Yes/No`); `TimelineStats` (counts by `created_at[:10]`, ascending), `DomainStats(limit)` (lower-cased domain after the last `@`, `most_common` order: count desc then first-seen). Offset/limit validated as Python (negative offset/limit are 500 in Python; Go answers 422 in FastAPI shape, D4).

- [ ] **Step 1: Write failing tests**: `TestListMatchesFixture`, `TestListFilters`, `TestSortByAllowListAndFallback`, `TestSortByNonColumnAttributeIsNotACrash`, `TestLimitZeroShape`, `TestMarkUsedByIDs`, `TestMarkUsedBulkFilterAndEverything`, `TestUpdateExcludeUnset`, `TestUpdateMissingIs404Detail`, `TestDeleteMissingStatus`, `TestExportBytesMatchFixture`, `TestExportEscaping` (quotes, commas, newlines, formula prefixes kept as Python), `TestTimelineStatsMatchFixture`, `TestDomainStatsOrderingTies`, `TestLikeWildcardsAreParity`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): email data service (RED)`, `feat(go): email data list update delete`, `feat(go): email data export and stats`.

### Task 15: Connector routes

**Files:** Modify stub `internal/api/connectorsroutes.go`; create `testdata/contract/connectors/*` assertions in `internal/api/connectorsroutes_test.go`

**Interfaces:** Produces `GET /api/connectors/catalog` and `GET /api/connectors/compatibility` (`identity.Workspace`), bodies from Task 12; no auth 401 as recorded.

- [ ] **Step 1: Write failing tests**: `TestCatalogMatchesFixture`, `TestCompatibilityMatchesFixture`, `TestCatalogRequiresWorkspace`, `TestCompatibilityHasNoCredentialKey`, `TestCatalogConfiguredPerWorkspace`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): connector routes (RED)`, `feat(go): connector catalog routes`.

### Task 16: HubSpot routes

**Files:** Modify stub `internal/api/hubspotroutes.go`; tests

**Interfaces:** Produces `GET /api/crm/hubspot/status` (`{"connected": false, "error": "No HubSpot token configured"}` when unset), `POST /connect` (editor, body `{token: str}` required; secret `HUBSPOT_TOKEN` stored through the plan 03 resolver in the caller's workspace; `{"status":"ok","message":"HubSpot token saved"}`), `POST /sync` (editor; body `{lead_ids?, tier?, limit=50}`; 400 `HubSpot not connected. Add token first.`; leads via `leadsread` in the caller's tenant: by ids (missing ones dropped), by `tier`, else by `limit`; only leads with an email; no leads -> `{"created":0,"updated":0,"failed":0,"message":"No leads with emails to sync"}`; else `{"created","updated","failed","errors"[:5],"total_synced"}`), `GET /contacts` (`limit=20`; 400 `HubSpot not connected`; `{"contacts","count"}`). `limit` must be 1..1000 (422 FastAPI shape; Python has no bound, D4); `lead_ids` <= 1000; the batch honours client disconnect (context) and stops pushing.

- [ ] **Step 1: Write failing tests**: `TestStatusMatchesFixtures`, `TestConnectStoresPerWorkspaceSecret`, `TestConnectViewerForbidden`, `TestConnectEmptyTokenValidation`, `TestSyncMatchesFixtures` (against `providersim`), `TestSyncForeignLeadIDsDropped`, `TestSyncNoEmailLeadsMessage`, `TestSyncNotConnected400`, `TestSyncLimitBounds422`, `TestSyncErrorsCappedAtFive`, `TestSyncTokenNeverInResponse`, `TestContactsMatchesFixture`, `TestContactsNotConnected400`, `TestOtherWorkspaceTokenNotUsed`, `TestSyncStopsOnClientDisconnect`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): hubspot routes (RED)`, `feat(go): hubspot status connect contacts`, `feat(go): hubspot sync`.

### Task 17: `/api/data` routes

**Files:** Modify stub `internal/api/datacrmroutes.go`; tests

**Interfaces:** Produces the 7 routes, all behind `identity.RequireGlobalAdmin()` applied at group level: `GET /api/data` (`limit=50, offset=0, source_link_id, q, sort_by=id, order=desc, start_date, end_date, is_used`), `POST /api/data/mark-used` (`{ids:[], used:true, q, start_date, end_date, source_link_id, is_used_filter}` -> `{"count","success":true}`), `PATCH /api/data/{id}` (404 `Record with id N not found`; `{"status":"success","data":{...}}`), `DELETE /api/data/{id}` (`{"status":"success"}` or `{"status":"not found"}`), `GET /api/data/export` (`text/csv`, header `Content-Disposition: attachment; filename=export_<YYYYmmdd_HHMMSS>.csv` in server local time as Python, TZ=UTC in fixtures), `GET /api/data/stats/timeline`, `GET /api/data/stats/domains` (`limit=10`). Path `{id}` that is not an integer answers 422 (FastAPI shape). `/api/data/export` and `/stats/*` are registered before `/{id}` so they are never shadowed.

- [ ] **Step 1: Write failing tests**: `TestEveryDataRouteRequiresGlobalAdmin` (iterates `Router.Patterns()` for the prefix; a workspace admin and a member get 403 `Not authorized`), `TestListMatchesFixture`, `TestMarkUsedMatchesFixture`, `TestPatchMatchesFixture`, `TestDeleteMatchesFixture`, `TestExportMatchesFixtureBytes`, `TestStatsMatchFixtures`, `TestPathIDValidation422`, `TestExportStreamsWithoutBufferingAll` (1e5 rows, bounded memory), `TestExportClientDisconnectStops`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): data crm routes (RED)`, `feat(go): data crm list mark-used update delete`, `feat(go): data crm export and stats`.

### Task 18: AmbitionBox routes

**Files:** Modify stub `internal/api/ambitionboxroutes.go`; tests

**Interfaces:** Produces `GET /api/ambitionbox/companies` (`page>=1, limit 1..20, sort_by=popular, industry, location, company_type, rating`; comma lists split as Python; `location`/`company_type` -> 422 `{"detail": "<Python NotImplementedError message>"}`, D4), `GET /companies/{company_id}/jobs?page=1`, `GET /companies/{company_id}/detail`, `GET /companies/{company_id}/similar`, `GET /collect` (`pages 1..25 default 5`, `limit 1..500 default 100`, `industry`, `location` -> 422 as above; body = `collection.summary(include_records=False)` + `companies` + `total`), `POST /imports` (202; body `name="AmbitionBox Companies", industry, rating, sort_by pattern ^(popular|rating|reviews)$, limit 1..500 default 100, workbook_id, replace_existing=false`; gate `RequireRole("","owner","admin","editor")`; `Start` errors: `AlreadyRunningError` -> 409 `{"detail":{"code":"connector_run_active","message":"AmbitionBox import <id> is already active","run_id":"<id>"}}`, `ErrWorkbookNotFound` -> 404 `{"detail":"workbook not found in the active workspace"}`, `ErrNeedsReplace` -> 409 `{"detail":"existing non-AmbitionBox workbooks require replace_existing=true"}`; Python answers 500 for the last two, D4). Upstream `*UpstreamError` -> 502 `{"detail":"AmbitionBox upstream error"}` (Python 500, D4); every GET honours the request context and a 20 s overall deadline.

- [ ] **Step 1: Write failing tests**: `TestCompaniesMatchesFixture`, `TestCompaniesLimitBounds422`, `TestLocationFilterIs422NotCrash`, `TestJobsDetailSimilarMatchFixtures`, `TestPathIDNotIntegerIs422`, `TestCollectMatchesFixture`, `TestCollectBounds422`, `TestImportsAcceptedMatchesFixture`, `TestImportsRunningIs409WithRunID`, `TestImportsForeignWorkbookIs404`, `TestImportsNonAmbitionBoxNeedsReplace`, `TestImportsViewerForbidden`, `TestImportsSortByPattern422`, `TestUpstreamErrorIs502NoBodyLeak`, `TestImportsEnqueueExecutorFlag`, `TestReadsRequireWorkspace`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): ambitionbox routes (RED)`, `feat(go): ambitionbox read routes`, `feat(go): ambitionbox import route`.

### Task 19: Contract coverage, nginx split, runbook

**Files:**
- Create: `testdata/contract/retained_connector_routes.txt`, `../../docs/go-port/runbooks/connectors-crm-cutover.md`
- Modify: `testdata/contract/README.md` (deviations D3-D5), `../../infra/nginx/go-routes.conf` (one delimited section `# --- plan 07 connectors-crm ---`), `internal/contract/coverage_test.go` (add the 19 routes)

**Interfaces:** Produces: every one of the 19 routes has at least one recorded fixture and a test; routes that remain on Python are listed (`crm_import` is not a route; `ingest*` belong to plan 06); nginx maps `/api/connectors/`, `/api/crm/hubspot/`, `/api/data` (exact and subpaths), `/api/ambitionbox/` to the Go upstream by the existing sticky percentage bucket (per-workspace cohort variable shared with plans 03-06, default 0%); `scripts/nginx-routing-check.sh` at 0% and 100% covers each prefix (a path under `/api/data/` that is also a prefix of nothing else stays unambiguous; `/api/crm/hubspot/` must not capture a hypothetical `/api/crm/other`). The runbook covers: flag order (`GO_CONNECTOR_ROUTES_ENABLED`, then `GO_EXECUTOR_AMBITIONBOX` for one cohort workspace), the Python-reaper-calls-Python-failure-handler note, resuming a run across executors, per-route rollback, runtime-role grants check on `email_data` and `links` (`SELECT/INSERT/UPDATE/DELETE`, sequence usage), how to verify no token appears in logs, and the legacy global-table caveat.

- [ ] **Step 1: Run** `uv run python scripts/contract_record.py --cases scripts/contract_cases/connectors.yaml --check` (fixtures complete, byte-identical).
- [ ] **Step 2: Write failing tests** `TestEveryConnectorRouteHasFixture`, `TestRetainedConnectorListMatchesFastAPI` (diff of FastAPI route table against Go patterns plus the retained list is empty), `TestDeviationsDocumented` (each D3-D5 deviation id appears in `README.md`), `TestRuntimeRoleCanReadWriteEmailData`. Run `make test` -> FAIL.
- [ ] **Step 3:** record missing fixtures, write the nginx section, runbook and deviations. **Step 4: Run** `make vet test test-integration` and `scripts/nginx-routing-check.sh` at 0% and 100% -> PASS.
- [ ] **Step 5: Commit** `test(go): connector route coverage (RED)`, `feat(infra): nginx per-route split for connectors and crm`, `docs(go-port): connectors-crm cutover runbook and deviations`.

---

## Self-Review

- **Spec coverage:** inventory row 6 "connectors, crm, hubspot, integrations, ambitionbox_import, csv/crm ingest": connectors (4, 12, 15), crm = `/api/data` (14, 17) and CRM clients (5, 10, 11), hubspot routes (16), integrations (6; certification 4), ambitionbox routes and job (9, 13, 18), source ingest (8). "csv/crm ingest": CSV import is plan 04 (Task 13 there), the two ingest routes are plan 06 Task 30, `crm_import` materialisation stays in `source_workbook` (D1); this plan provides the shared `sourceingest` upsert and the CRM clients it needs. `ambitionbox_import` failure handler: 3, 13. Executor routing: 2, 13.
- **Consistency:** `sourceingest.Upsert` (8) used in 13; `crm.RequestWithRetry` (5) in 10; `crmpush.Push` (11) consumes 5, 7, 10 and is consumed by plan 08; `rowtmpl.Resolve` (7) consumed by 11-adjacent callers and plan 08; `queue.RegisterFailureHook` (3) used in 13 and by plan 08 (`watch_poll`); `certification.StatusFor` (4) used in 12.
- **Review Focus:** 1 -> Tasks 5, 10, 11, 16; 2 -> 5, 6, 9, 10; 3 -> 8, 13; 4 -> 12, 14-18; 5 -> 14, 17; 6 -> 4, 12.
- **Proportion:** interfaces, error strings, test names and file ownership carry the decisions; client and handler bodies are left to the implementer and the recorded transcripts.

## Open decisions (defaults apply unless overridden)

- **D1 `crm_import`.** The source kind runs inside the Python `source_workbook` job (plan 06 D6, scrapers and collection stay Python). Default: stays on Python behind `CRM_IMPORT_ENABLED`; Go ships the HubSpot/Salesforce `FetchContacts` clients so the leadgen plan can move it. Not a route; nothing to cut over here.
- **D2 Integration clients without routes.** Airtable, Sheets, Instantly, Smartlead and Salesforce have no router of their own; their only callers are `output.execute_output_column` and `destinations/engine.py`. Default: ported now as libraries (tested on transcripts) so plan 08 `push_crm` and the destinations plan do not block; no route is added.
- **D3 Salesforce SSRF.** `SALESFORCE_INSTANCE_URL` is user-typed and unguarded in Python. Default: validated through `internal/egress` on every call (documented deviation; a blocked URL is a per-run error, not a 500).
- **D4 Crash and hardening deviations.** Each of: unknown `sort_by` falls back to `id` for non-column attributes; AmbitionBox `location`/`company_type`, bad import preconditions and upstream failures answer 422/404/409/502 instead of 500; `hubspot/sync` `limit` bounded 1..1000; template and path-segment bounds; negative `limit`/`offset` on `/api/data` answer 422. Default: all applied and listed in `testdata/contract/README.md`.
- **D5 `/api/data` stays global.** The table has no `workspace_id`; making it tenant scoped is a schema and product change. Default: global-admin gate, exact parity, noted in the runbook.
- **D6 Failure hooks.** Go jobs need the failure-handler semantics Python has only in-process. Default: `queue.RegisterFailureHook` after a persisted failed outcome (Task 3); dead-job recovery (lease expiry) stays with the Python reaper, which already calls the Python handler for any executor.
- **D7 Rollout.** Default: all new Go paths off (`GO_CONNECTOR_ROUTES_ENABLED`, `GO_EXECUTOR_AMBITIONBOX` false); canary by workspace cohort with the shared nginx bucket; `POST /imports` sticky to the cohort so a run is started and resumed by one executor family.

## User decisions

(none yet; the defaults above apply until the user objects)
