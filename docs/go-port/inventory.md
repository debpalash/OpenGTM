# OpenGTM Python backend: Go porting inventory

Source: `legacy/python/` (Python LOC via `find *.py`, excl. `__pycache__`; blank/comment lines included).
Generated 2026-10-26. Recommendation document only; nothing here is committed scope.

## 1. Size summary

Total `apps/api`: **84,992 LOC / 329 files**. Plus `apps/mcp` 228, tests 42,153 (214 files), migrations 4,577 (60 versions).

### services/* (62,944 LOC, 257 files)

| Package | LOC | Files | | Package | LOC | Files |
|---|---|---|---|---|---|---|
| leadgen/ | 27,100 | 111 | | integrations/ | 1,033 | 6 |
| workbook/ | 10,697 | 40 | | crm/ | 720 | 3 |
| outreach/ | 3,599 | 11 | | agent/ | 614 | 4 |
| evaluation/ | 3,059 | 7 | | audiences/ | 522 | 4 |
| poller/ | 2,237 | 8 | | billing/ | 419 | 4 |
| automations/ | 2,027 | 7 | | governance/ | 357 | 4 |
| mcp/ | 1,394 | 6 | | playbooks/ | 340 | 4 |
| workspace/ | 1,310 | 5 | | templates/ | 259 | 2 |
| destinations/ | 1,257 | 6 | | connectors/ | 112 | 2 |
| entities/ | 1,142 | 4 | | signals/ | 1,060 | 4 |

Top-level service modules (loose .py): queue_service 664, person_intel 579, dedup 477, chat_history 324, queue_load 219, scraper 209, memory 200, backup 182, workbook_load 176, database_readiness 154, scale_certification 142, job_process_runner 122, job_registry 97, file_manager 74, job_scheduling 67.

Sub-breakdown of the two big ones:
- leadgen: enrichment/ 12,815 (35 provider files, ~11k in providers+scrapers+llm+http), scrapers/ 1,553, job_runner 2,332, llm 834, http 736, rest ~8.8k (pipeline, store, proxy, scoring, search_engines, source_*).
- workbook: enrichment.py 1,747 (job handler), planner, ai/agent/research/nl columns, provider_runner (pebble pool), spend/receipts/trace.

### routers/* (16,114 LOC, 33 files)

| Router | LOC | | Router | LOC |
|---|---|---|---|---|
| copilotkit | 3,712 | | audiences | 270 |
| workbooks | 2,696 | | playbooks | 316 |
| leads | 1,431 | | scim | 315 |
| settings | 1,266 | | person_intel | 272 |
| destinations | 620 | | crm | 281 |
| watches | 565 | | scraper | 215 |
| automations | 555 | | operations | 216 |
| outreach | 513 | | templates | 172 |
| ingest | 402 | | auth | 169 |
| analytics | 363 | | entities | 156 |
| governance | 345 | | other 10 files (<140 each) | ~900 |

### Other

| Area | LOC |
|---|---|
| core/ (config 407, tenancy 182, url_guard 183, security 107, ratelimit 47) | 926 |
| models.py (6 ORM classes) + database.py 310 | 419 |
| schemas/ | 47 |
| sources/ (arxiv, crossref, doaj, pubmed, openlibrary, gutenberg, semantic_scholar, google_search, legacy, registry) | 1,577 |
| main.py 297, worker.py 127, scheduler.py 78, job_process.py 46, auth.py 54, db_init.py 90 | 692 |
| apps/api/cli.py 502, reset_admin, eval_pipeline 318, test_*.py/validate_emails (dev scripts) | ~1,300 |
| apps/api/scripts (seed_demo, migrate_sqlite_to_pg 382, adopt_legacy_schema, provision_runtime_role) | 871 |
| apps/mcp/server.py (+ services/mcp 1,394) | 228 (+1,394) |
| legacy/python/cli.py | 6 |

## 2. HTTP surface

**~312 route decorators** (`@router.<method>` / `@app.<method>`) across 33 routers + main.py (2: `/api`, `/health`). Several files define multiple `APIRouter`s (leads.py hosts jobs/events/search routers; workbooks.py hosts v2 + views routers; scim_router in scim.py), so URL prefixes below are the primary one.

Auth dependency vocabulary: `current_workspace` (tenant resolution, sets RLS), `require_workspace_role`, `require_editor`, `require_admin` (workspace admin), `get_current_active_user`, `get_current_admin_user` (global admin), `require_enabled` (feature flag), `scim_token`, `verify_unsubscribe_token` (public).

| Router | Routes | Prefix | Auth / role deps | Streams |
|---|---|---|---|---|
| workbooks | 49 | /api/workbooks (+ /api/v2/workbooks views) | current_workspace, require_editor (30) | SSE (live cells via Redis pubsub `workbook:{id}`) |
| leads | 35 | /api (leads, jobs, events, search) | current_workspace, require_editor | SSE x3 (progress, CSV export) |
| settings | 19 | /api/settings | admin / active user / workspace_editor | no |
| governance | 18 | /api/governance | require_admin, scim_token | CSV stream |
| scim | 17 | /scim/v2/{workspace_slug} | SCIM bearer token | no |
| destinations | 16 | /api/audience-destinations | require_editor | no |
| outreach | 22 | /api/outreach | require_admin, public unsubscribe token | no |
| automations | 11 | /api/automations | require_admin, require_enabled | no |
| playbooks | 10 | /api/research-playbooks | require_editor | no |
| audiences | 9 | /api/audiences | require_editor | no |
| copilotkit | 7 | /api/copilotkit | JWT (jose), mixed | **SSE x4 (LLM agent chat, 3,712 LOC)** |
| watches | 7 | /api/watches | require_editor, require_enabled | no |
| crm | 7 | /api/crm | active user, global admin | CSV stream |
| ambitionbox | 6 | /api/ambitionbox | workspace role | no |
| auth | 6 | /auth | password login, active user | no |
| entities | 6 | /api/entities | require_editor | no |
| signals | 6 | /api/signals | workspace role, admin | no |
| workspace_manager | 6 | /api/workspaces | active user | no |
| analytics | 5 | /api/analytics | current_workspace | no |
| functions | 5 | /api/functions | admin | no |
| person_intel | 5 | (none) | admin | **WebSocket** (LinkedIn enrich) |
| scraper | 5 | (none) | admin | **WebSocket** (scrape preview) |
| templates | 5 | /api/templates | require_editor | no |
| users | 5 | /admin/users | global admin | no |
| hubspot | 4 | /api/crm/hubspot | require_editor | no |
| system | 4 | (none: /api/system/stats, /api/files/*) | none/ad hoc | file download |
| billing | 3 | /api/billing | current_workspace | no |
| mcp_tokens | 3 | /api/mcp/tokens | require_admin | no |
| connectors | 2 | /api/connectors | current_workspace | no |
| ingest | 2 | /api/v2/workbooks | require_enabled, user | no |
| meta | 2 | /api | current_workspace | no |
| operations | 2 | /admin/operations | global admin | no |
| campaigns | 1 | /api/campaigns | require_editor | no |
| main.py | 2 | /api, /health | none | no |

Streaming summary: 2 WebSockets (person_intel, scraper preview; token via query param), SSE in leads/workbooks/copilotkit (EventSource cannot send Authorization header, so token via query), CSV streams in governance/crm/leads.

**MCP server surface** (`apps/mcp/server.py` 228 LOC + `services/mcp` 1,394): JSON-RPC (`initialize`, `tools/list`, `tools/call`) over stdio and SSE (`build_sse_app`). Token-authenticated, tenant-scoped, capability-filtered per token, audited. 15 tools: find_leads, get_lead_detail, get_pipeline_stats, enrich_company, verify_email, get_hiring_signals, score_lead, create_lead, update_lead, enroll_leads, create_workbook, create_automation, send_email, delete_lead, delete_workbook. Token CRUD via `/api/mcp/tokens`.

## 3. Durable job types

17 types in `services/job_registry.py`. Default timeout 600s (`DEFAULT_JOB_TIMEOUT`). Every job runs in a child process (`job_process.py` via `job_process_runner`) with the listed timeout. Failure handler = `reconcile_*` registered via `register_failure_handler`.

| Job type | Handler module (LOC) | Failure handler | Timeout s | Main external deps |
|---|---|---|---|---|
| run_workbook | workbook/enrichment (1,747) | no | 1800 | LLM (leadgen.llm: anthropic + OpenAI-compatible HTTP), provider_runner pebble pool, ~35 enrichment providers (HTTP), email verify cascade, Redis pubsub |
| ambitionbox_import | workbook/ambitionbox_import (559) | yes | 900 | HTTP scrape ambitionbox.com |
| source_workbook | workbook/source_engine (474) | no | 1800 | runs full leadgen collection inline (scrapers, browser tier, ddgs, LLM), CRM import |
| collect | leadgen/job_runner (2,332) | yes | 600 (default) | ddgs, curl_cffi/patchright/playwright, bs4, staffspy, jobspy, anthropic, proxies, Google Maps, LinkedIn |
| bulk_enrich | leadgen/job_runner | no | 600 (default) | same as collect plus email waterfall/verify (DNS, SMTP probes, Hunter etc.) |
| refresh_workbook | workbook/refresh (399) | no | 900 | enrichment providers, signals monitor (HTTP) |
| signal_scan | workbook/refresh | no | 300 | signals/monitor (SEC, news; SQLite signals.db) |
| trigger_eval | automations/engine (521) | no | 600 | DB rules, single-row re_enrich provider calls |
| send | outreach/sending (527) | no | 300 | SMTP (smtplib/aiosmtplib) |
| outreach_inbound_poll | outreach/inbound (394) | no | 600 (default) | IMAP (imaplib) |
| watch_poll | poller/engine (705) | yes | 600 | SEC EDGAR, JobSpy, RSS (httpx) |
| source_health_check | leadgen/source_health (504) | no | 1800 | ~91 sources x DDG canary probes |
| audience_refresh | audiences/scheduler (224) | yes | 900 | DB only |
| audience_destination_sync | destinations/engine (666) | yes | 1800 | Airtable, Sheets, Instantly, Smartlead, Google Ads, warehouse (HTTP) |
| research_playbook_run | playbooks/engine (140) | yes | 3600 | LLM/agent steps via workbook engine, HTTP |
| research_playbook_schedule | playbooks/scheduler | no | 300 | DB only |
| retention_enforce | governance/retention (208) | yes | 1800 | DB only (destinations models) |

Queue itself: `queue_service.py` (664 LOC) is a Postgres table queue (claims with worker_id/lock, 30s heartbeat, leases, retry, `pg_advisory_xact_lock` in `enqueue_job_once`); not BullMQ/Redis despite the `bullmq` dependency.

## 4. Classification per service package

Python-only evidence from `pyproject.toml`: `anthropic`, `playwright`, `patchright`, `stealth-requests`, `curl-cffi`, `ddgs`/`duckduckgo-search`, `thefuzz`, `pandas`, `pebble`, `bs4`/`lxml`/`markdownify`, `logfire`. Not declared but imported lazily (optional installs): `staffspy`, `jobspy`. Import counts from grep (lazy imports included).

| Package | Class | Justification (decisive imports) |
|---|---|---|
| leadgen | **PY-SPECIALIST** | anthropic, patchright/playwright, staffspy, jobspy, ddgs, thefuzz, curl_cffi, pandas |
| workbook | GO-NATIVE (boundary) | httpx, pebble pool; provider/LLM calls cross to leadgen |
| outreach | GO-NATIVE | smtplib, imaplib, aiosmtplib; Go net/smtp + IMAP libs |
| evaluation | GO-NATIVE | httpx only; dev/benchmark tooling, low priority |
| poller | GO-NATIVE | httpx; JobSpy reached only via leadgen providers |
| automations | GO-NATIVE | httpx, DB rules engine |
| mcp | GO-NATIVE | no third-party imports; JSON-RPC over stdio/SSE |
| workspace | GO-NATIVE | sqlite3, authlib, cryptography (envelope enc); stdlib-equivalent |
| destinations | GO-NATIVE | httpx; vendor REST APIs |
| entities | GO-NATIVE | SQLAlchemy only |
| signals | GO-NATIVE | sqlite3; SEC/RSS fetch |
| integrations | GO-NATIVE | httpx (Airtable, Sheets, Instantly, Smartlead) |
| crm | GO-NATIVE | httpx (HubSpot/Salesforce) |
| agent | GO-NATIVE | no heavy imports (614 LOC); LLM call via leadgen.llm |
| audiences | GO-NATIVE | SQLAlchemy only |
| billing | GO-NATIVE | stripe (has official Go SDK) |
| governance | GO-NATIVE | SQLAlchemy only (audit, retention, SCIM) |
| playbooks | GO-NATIVE | SQLAlchemy; steps delegate to workbook |
| templates | GO-NATIVE | no third-party imports |
| connectors | GO-NATIVE | contracts only, 112 LOC |
| sources/ (top-level) | GO-NATIVE | aiohttp + bs4 against public REST APIs (goquery) |
| scraper.py | PY-SPECIALIST | playwright, bs4, markdownify, logfire |
| person_intel.py | GO-NATIVE | bs4 + httpx; WebSocket endpoint |
| queue_service, job_registry, job_process_runner, job_scheduling, queue_load, scale_certification | SHARED-INFRA | queue, child-process runner, registry |
| core/*, database.py, database_readiness, worker, scheduler, main | SHARED-INFRA | config, tenancy/RLS context, SSRF guard (url_guard), JWT (jose) |
| memory, chat_history, backup, file_manager, dedup, workbook_load | SHARED-INFRA | sqlite3 stores, file ops, dedup (no thefuzz import) |

Tally by service directory (20): **19 GO-NATIVE, 1 PY-SPECIALIST (leadgen)**; plus top-level `scraper.py` PY-SPECIALIST. leadgen is internally mixed: of ~35 enrichment providers most are plain HTTP (abstract_api, apollo, hunter, prospeo, snovio, pdl, numverify, ipinfo, gleif, sec_edgar, companies_house, wikidata, debounce, reacher) and portable; true specialists are `http.py` browser tiers, `scrapers/*` (linkedin, google_maps, crunchbase, job_boards), `staffspy_provider`, `jobspy_signals`, `crosslinked`, `holehe_verify`, `deep_scraper`, `llm.py` (anthropic SDK + 8 OpenAI-compatible endpoints), `ddgs` search. Note: `llm.py` could be Go-native over plain HTTP (Anthropic has a Go SDK); keeping it in Python is a conservative choice.

## 5. Data stores

### SQLAlchemy (Postgres, `Base`)
~68 `__tablename__` declarations across 21 modules (classes subclassing `Base`):

| Module | Models | Module | Models |
|---|---|---|---|
| apps/api/models.py | 6 | services/workbook/models.py | 5 |
| entities/models.py | 8 | workbook (spend, trace, planner, activity, ingest) | 1+1+1+1+2 |
| outreach/orm_models.py | 8 | governance/models.py | 4 |
| automations/models.py | 5 | audiences/models.py | 4 |
| destinations/models.py | 5 | playbooks/models.py | 4 |
| leadgen/orm_models.py | 3 | poller/models.py | 3 |
| leadgen (source_health, source_stats, contact_execution) | 1+1+1 | mcp/models.py | 2 |
| billing/models.py | 2 | | |

Migrations: Alembic, 60 versions in `migrations/versions`.

### RLS
- Policy-bearing migrations: ~20 (workbooks, outreach, entities, automations, audiences, destinations, playbooks, governance, mcp tokens/audit, spend attempts, company identifiers, connector_runs, llm_usage_daily, workbook views, ingest, tenancy for shared leads/signals).
- ~26 `CREATE POLICY` statements, several driven by `_RLS_TABLES` lists in 13 migrations (so table count exceeds the 6 literal `ENABLE ROW LEVEL SECURITY` hits). `b3c4d5e6f7a8_runtime_role_rls_reconciliation` reconciles runtime-role grants; `scripts/provision_runtime_role.py` creates the non-owner role. FORCE RLS used (e.g. connector_runs).
- Go port must set the tenant GUC per transaction (`core/tenancy.py`, 182 LOC) and connect as the non-owner runtime role. Treat as a hard compatibility requirement.

### SQLite stores still in use (all under `data/`)

| File | Module | Contents |
|---|---|---|
| workspaces.db | services/workspace/manager.py | workspaces, tenant membership, per-workspace meta |
| data.db | routers/settings.py, leadgen/llm.py, reset_admin.py | settings, LLM keys/config, legacy users |
| workspaces/{slug}/leads.db | workspace/manager.py:470, leadgen/config.py (DB_PATH) | per-workspace legacy leads ledger |
| outreach.db | outreach/sequence.py | sequence state |
| signals.db | signals/monitor.py | signal monitor state |
| functions.db | workbook/functions.py | user functions |
| enrichment_cache.db | leadgen/enrichment/{cache,email_verify_cache}.py | provider + email-verify caches |
| chat_history.db / openmemory.db / chat_memory.db | chat_history.py, memory.py | copilot chat + memory |
| eval_*.db | eval_pipeline.py | dev only |

`services/backup.py` snapshots every `*.db`. `scripts/migrate_sqlite_to_pg.py` (382 LOC) is the existing migration path to Postgres; finishing it removes most SQLite stores before the port.

### Redis (optional, `REDIS_URL`; degrades gracefully)
- `leadgen:progress:{workspace_id}` pubsub channel + `leadgen:progress:{workspace_id}:recent` list (LPUSH; leadgen/progress.py), with 5s/1s retry backoff on failure.
- `workbook:{workbook_id}` pubsub channel (cell updates; publisher in workbook/enrichment.py, subscriber SSE in routers/workbooks.py:2520).
- No Redis job queue (queue is Postgres). Rate limiting (slowapi) in `core/ratelimit.py`.

## 6. Background processes

| Process | File | Behavior |
|---|---|---|
| API | main.py (297) | FastAPI lifespan; optional in-API worker; `/health` |
| Worker | worker.py (127) | standalone `QueueService` consumer; concurrency 1..64 (`WORKER_CONCURRENCY`); heartbeat 30s; shutdown grace; stamps `worker_id` |
| Scheduler | scheduler.py (78) | reconciliation loop every `SCHEDULER_RECONCILE_SECONDS` (default 60, min 10). Each tick runs 9 idempotent bootstraps that enqueue jobs via `enqueue_job_once` (advisory lock): signal_scan, automations schedules, outreach schedules, outreach_inbound, intent_poller (watch_poll), source_health, audiences, retention, playbooks |
| Job child | job_process.py (46) | `python -m apps.api.job_process JOB_ID JOB_TYPE`, payload on stdin JSON, runs one handler; parent enforces per-type timeout and kills on lease loss (`should_continue`) |
| provider_runner pool | services/workbook/provider_runner.py | `pebble.ProcessPool` of killable workers for individual enrichment provider calls (hard timeout without poisoning the loop); resizable via `ensure_workers(provider_workers)` |

Go equivalent: goroutine + context cancel replaces both the child process and the pebble pool for Go-native providers; Python-specialist providers need a killable process boundary anyway.

## 7. External integrations (vendor to module)

| Vendor | Module |
|---|---|
| Anthropic API, OpenRouter, Groq, NVIDIA NIM, SambaNova, Mistral, Cerebras, Azure inference | leadgen/llm.py, routers/settings.py, routers/copilotkit.py |
| Stripe | billing/, routers/billing.py |
| SMTP / IMAP (customer-configured) | outreach/sending.py, outreach/inbound.py |
| HubSpot, Salesforce | routers/hubspot.py, crm/ |
| Airtable, Google Sheets, Instantly, Smartlead | integrations/*, destinations/engine.py |
| Google Ads, data warehouse | destinations/ads.py, warehouse.py |
| Hunter, Apollo, Prospeo, Snov, People Data Labs, Debounce, AbstractAPI, Numverify, Reacher, mailscout, Holehe | leadgen/enrichment/providers/* |
| SEC EDGAR, Companies House, GLEIF, Wikidata, ipinfo, MCA registry | providers/{sec_edgar,companies_house,gleif,wikidata_provider,ipinfo,mca_registry} |
| LinkedIn, Google Maps, Crunchbase, review directories, job boards, AmbitionBox | leadgen/scrapers/*, workbook/ambitionbox_import.py, staffspy_provider |
| JobSpy | providers/jobspy_signals.py, enrichment/licenses.py, job_runner |
| DuckDuckGo (ddgs) | leadgen/search_engines.py, web_search.py, source_health |
| Academic sources (arXiv, Crossref, DOAJ, PubMed, Semantic Scholar, Open Library, Gutenberg, archive.org) | sources/ |
| Playwright / patchright browsers | leadgen/http.py (tier 3), scraper.py |
| SCIM IdP (Okta/Entra) | routers/scim.py, governance/ |
| CopilotKit | routers/copilotkit.py |
| Logfire | main.py, scraper.py |
| OAuth (authlib), JWT (python-jose), bcrypt (passlib) | workspace/, core/security.py, auth.py |

## 8. Proposed port order (recommendation)

LOC = Python source being replaced (services + routers), approximate. "Boundary" = PY-SPECIALIST pieces that stay behind a Python worker.

| # | Domain plan | Contents | ~LOC | Python boundary |
|---|---|---|---|---|
| 0 | Shared infra | config, tenancy/RLS GUC, url_guard (SSRF), ratelimit, DB layer (sqlc), logging; Postgres queue + worker + scheduler + job_registry | ~3,000 | none; job runner gets a `python-worker` executor type |
| 1 | Auth + workspaces | auth, users, workspace_manager, SCIM, mcp_tokens, JWT/bcrypt; migrate workspaces.db + data.db users to Postgres | ~3,000 | none |
| 2 | Settings + secrets | settings router (1,266), envelope encryption (cryptography), LLM key storage, billing/Stripe | ~2,500 | none |
| 3 | Workbook CRUD | workbooks router CRUD, views, templates, functions, entities, analytics, meta, system/files | ~7,500 | none |
| 4 | Leads and signals CRUD | leads router CRUD/export/SSE, signals, dedup, Redis pubsub + SSE | ~3,500 | none |
| 5 | Enrichment jobs | run_workbook, refresh, signal_scan, provider_runner, ~20 HTTP-only providers, spend/receipts, email verify | ~12,000 | LLM calls (`llm.py`), browser/scraper providers, jobspy/staffspy, ddgs: call Python worker over HTTP/gRPC with per-call timeout |
| 6 | Connectors + CRM | connectors, crm, hubspot, integrations, ambitionbox_import, csv/crm ingest | ~3,500 | ambitionbox scrape if it uses browser tier |
| 7 | Automations | automations engine/router, trigger_eval, watches + poller | ~5,000 | JobSpy fan-in in poller |
| 8 | Outreach | send (SMTP), inbound (IMAP), sequences, unsubscribe, campaigns; outreach.db to Postgres | ~4,700 | none |
| 9 | Audiences + destinations | audiences, audience_destination_sync, ads, warehouse | ~2,700 | none |
| 10 | Governance + playbooks | audit, retention, research playbooks (playbook steps call plan 5 + LLM) | ~1,500 | LLM steps via worker |
| 11 | Copilot/agent + person_intel | copilotkit SSE (3,712 LOC), agent, chat_history, memory, WebSockets | ~5,500 | LLM streaming; linkedin enrich if browser based |
| 12 | MCP server | stdio + SSE JSON-RPC, 15 tools, audit | ~1,700 | tools that call enrich go through plan 5 |
| 13 | Leadgen collection (last) | collect, bulk_enrich, source_workbook, source_health, scoring, store | ~27,000 | **stays Python**: scrapers, browser tier, proxy pool, staffspy/jobspy, ddgs; port only the HTTP providers, store and scoring |

Cut lines: plans 0 through 4 give a Go API for all tenant CRUD with no Python dependency. Plan 5 introduces the one cross-language contract (a `ProviderCall{name, input, timeout}` RPC). After plan 13, Python remains as a stateless worker (~15k LOC: llm.py, http.py browser tiers, scrapers, staffspy/jobspy, ddgs, scraper.py, optionally the Anthropic SDK path).

Migration of SQLite stores should precede plans 1, 2, 4, 8 (finish `migrate_sqlite_to_pg.py`); otherwise the Go side must read SQLite.

## 9. Key risks

1. **RLS/tenancy fidelity**: ~20 migrations of RLS with FORCE and a non-owner runtime role; Go must set the tenant GUC on every pooled connection or it will leak or return empty rows. Keep Alembic as migration owner or port migrations carefully.
2. **Leadgen is 43% of services LOC and Python-bound**: scraper/browser/jobspy/staffspy/ddgs have no Go equivalents; the Python worker boundary needs timeouts, kill semantics (today pebble + child-process kill), and proxy/rate-limit state sharing.
3. **Scattered SQLite state and hidden coupling**: ~10 `.db` files incl. per-workspace ledgers, `data.db` opened by raw sqlite3 from routers/settings and leadgen/llm.py; Redis pubsub semantics (`leadgen:progress:*`, `workbook:{id}`) and SSE/WebSocket query-string tokens must be preserved for the existing frontend.
4. Smaller: copilotkit.py 3,712-LOC monolith streaming protocol; enrichment.py 1,747-LOC handler with lease semantics (`__queue_lease`); optional deps (staffspy/jobspy) not declared in pyproject.
