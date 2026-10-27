# Python-Free Runtime Implementation Plan (Plan 15)

> **For agentic workers:** each work package (WP) below is one agent brief. Read
> `.superpowers/sdd/py-free/rules.md` first (method, environment, git rules); the raw inventory with every item, file, LOC
> and class is `.superpowers/sdd/py-free/inventory.md` (item IDs A1..H4 below refer to it).

**Goal (user decision 2026-10-28):** Python survives only as the internal **browser service** (`apps/browser-service`:
Playwright/patchright, curl_cffi/stealth fetch, ddgs search, trafilatura extraction, holehe/staffspy; no DB, no secrets,
internal network only, called only by Go). Everything else (API, worker, scheduler, migrations, seed, LLM, copilot,
leadgen pipeline logic, providers) runs in Go, and the Python `api`, `worker`, `scheduler`, `migrate` and `seed`
containers are deleted.

**Architecture:** Plans 01-14 moved every route and most job types to Go behind cohort flags, but Go still leans on the
Python `api` container in four ways: (1) the `pyworker` RPC (`/internal/provider-call`, `/internal/copilot/*`,
`/internal/leadgen/*`); (2) job types with no Go handler (`collect`, `bulk_enrich`, `source_workbook`) and job runs Go
refuses and hands back (`run_workbook` residual, `trigger_eval` with legacy outreach actions); (3) ~15 HTTP routes pinned
to FastAPI plus the nginx default upstream; (4) processes: scheduler, migrate (WP-MIG), seed. This plan removes each
dependency behind a flag that defaults to today's Python path, then a cutover wave flips and deletes the flags, and an
infra wave deletes the Python services.

Page work (fetch with TLS impersonation, headless render, scraped search engines, main-text extraction, library probes) is
the only thing that stays Python, behind `internal/browserclient` (WP-BROWSER). All parsing, scoring, dedup, LLM prompts,
DB writes, spend and secrets stay on the Go side of that boundary.

**Tech stack:** Go 1.27.3 stdlib + existing modules only (pgx v5 + sqlc, go-redis, `golang.org/x/net/html` for HTML
parsing that bs4 did, `x/text`); no LLM SDKs (plain HTTP to OpenAI-compatible and Anthropic Messages APIs, as
`internal/settings/providers.go` already does). Python/uv only for recorders (local, under legacy/python) and the browser
service.

**Already started (not planned here, referenced as dependencies):**
- **WP-MIG**: Go migration runner replacing Alembic: owns `apps/backend-go/internal/migrate`, `cmd/migrate`, the compose
  `migrate` service. Also takes `scripts/provision_runtime_role.py` (G3) if not already in its brief.
- **WP-BROWSER**: tracked Python browser service under `apps/browser-service` with an internal API, plus the Go client
  `internal/browserclient`. Its shipped API is authoritative; the op list in "Browser contract" below is what the other
  packages need. Missing ops are added by WP-WEB only.

## Inventory summary

| Class | Count | Items |
|---|---|---|
| PORT-TO-GO | 54 | RPC endpoints A1-A9; providers B1-B12, B15-B21; libraries C1, C2d, C3, C4; jobs D1-D5; processes E1-E3, E5, E7, E8; routes F1-F11 |
| BROWSER-SERVICE | 5 | fetch/render (C2a), scraped search (C2b), text extraction (C2c), holehe (B13), staffspy (B14) |
| ALREADY-IN-GO | 18 | credentials/manifests local mode (A10), ported helpers (C5), job types D6-D16, mcp (E6), 260+ routes (F14), services behind them (G5), n8n, searxng-pool |
| DROP | 6 | `/ws/scraper/preview` + `/admin/operations/*` (nginx 404s them), unmounted workspace_router, evaluation harnesses, dev CLI, ops/load tooling, one-shot scripts |

Largest Python bodies still executing: `leadgen/job_runner.py` 2311, `copilot/tools.py` 1908, `workbook/enrichment.py`
1801, the 18 Python-only providers ~3.7k, `leadgen/llm.py` 828, `research_column.py` 706, `source_registry.py` 695,
`targeted_people.py` 635, `copilot/preroute.py` 608. Total ~23k LOC to port; ~2.9k of it is browser-bound and moves to
the browser service instead.

## Global Constraints

- Rules file applies verbatim (pathspec commits, `-s`, no Co-Authored-By, no push, shared containers, private DB clone,
  own Redis db number, `GOTMPDIR`, CI-safe tests via `testutil.LegacyPythonDir`).
- **Record first.** For every user-visible behaviour (route JSON, SSE frames, LLM request bodies and prompt text, LLM
  output parsing, provider output fields, scores, dedup decisions, job side effects) run the Python code and store vectors
  under the Go package's `testdata/`. Recorders live in `legacy/python/scripts/` (local, not committed) or the WP's scratch
  dir; the vectors are committed. Live LLM and live web are never called in tests: LLM via `httptest` transcripts or
  `cmd/providersim`; page work via a fake `browserclient` that replays recorded HTML/search results.
- **Flags default to Python.** Each WP adds `GO_*` flags in `internal/config` (its own file, e.g. `config/llm.go`), off by
  default, following the existing `GO_EXECUTOR_ENABLED` + `GO_EXECUTOR_WORKSPACES` cohort pattern for jobs and the
  `GO_<AREA>_ROUTES_ENABLED` pattern for routes. WP-CUTOVER flips and deletes them.
- **Routes:** a WP that ports a retained route removes it from `testdata/contract/retained_*_routes.txt`, updates the
  matching `internal/api/*_coverage_test.go`, and registers it only behind its routes flag. nginx is changed once, by
  WP-RETIRE-INFRA; until then nginx keeps sending these routes to FastAPI, which is safe.
- **Python bugs are fixed, not mirrored**; deviations go in the WP report (contract README only where the brief owns a
  section).
- **Browser boundary:** Go validates every URL with the egress guard before calling the browser service; the service
  re-checks every redirect hop (it must, Go cannot see hops). Go never sends credentials or API keys to the service; keyed
  search APIs (Brave, Serper, SearXNG with auth, ...) are called from Go directly. Proxies: see risk R3.
- **Shared files** (touch minimally, commit immediately, rebase-free): `cmd/worker/main.go`, `cmd/api/main.go`,
  `cmd/mcp/main.go`, `internal/api/register.go`, `internal/copilot/catalog.json`, `docker-compose.yml`, `infra/nginx/*`,
  `.github/workflows/*`, `testdata/contract/README.md`. New job handlers go in a new `cmd/worker/handlers_<wp>.go` plus a
  one-line registration in `main.go`.

## Browser contract (needed ops; WP-BROWSER's API is authoritative)

| Op | Input | Output | Python replaced | Needed by |
|---|---|---|---|---|
| fetch | url, mode `plain|stealth|render`, timeout, wait selector/ms, max bytes | status, final_url, headers, html, redirect chain | leadgen/http.py StealthClient, services/scraper.py | WEB, PROVIDERS, COLLECT, AICOLS, ROUTES-B |
| search | query, backend list (`SEARCH_BACKENDS` order), max_results, region, safesearch | [{title, href, body}] in engine order | proxy_client ddgs, enrichment/web_search | WEB (then everyone) |
| extract | html or url | main text (trafilatura), markdown (markdownify), word count | content_extract, scraper.py | WEB, AICOLS, PROVIDERS (B19) |
| maps_search | query, location, max | places (name, address, phone, website, rating, ...) | scrapers/google_maps (patchright), gmaps_service_client | COLLECT |
| holehe | email | per-site results | holehe_verify | PROVIDERS |
| staffspy | company, filters | staff rows | staffspy_provider | PROVIDERS |
| stats | - | per-process fetch/search counters | proxy_pool/rate_limiter stats | ROUTES-B (system-stats) |

## Work packages

Size: S < ~500 Go LOC, M ~500-1500, L > 1500. Redis db numbers are per agent (wave 2: 2-7, wave 3: 8-13, wave 4: 2-4,
wave 5: 5-6). "Owns" = only this WP edits these paths in its wave.

### Wave 2 (6 agents; after WP-MIG and WP-BROWSER land)

#### WP-LLM: Go LLM client (C1) — M
- **Owns:** `internal/llm/**` (new: providers, chat, stream, anthropic, failover, extractjson, usage), `internal/llm/testdata/**`, `internal/config/llm.go`.
- **Read:** `leadgen/llm.py` (828), `services/copilot/llm_stream.py`, `routers/copilotkit.py` `_get_active_provider`, `routers/settings.py` llm-usage + model routes, `internal/settings/providers.go` (Go key tests), `internal/secrets`.
- **Record first:** request bodies + headers per provider id (OpenAI-compatible and native Anthropic: system blocks, cache_control, max_tokens, temperature defaults), failover order and the error that triggers failover, SSE token/usage frame parsing, `extract_json` inputs to outputs (fenced, trailing prose, invalid), llm_usage rows written per call, error texts surfaced to users ("No AI provider configured. Go to Settings.", unreachable/timed out).
- **API:** `llm.Client{Complete, Stream, ExtractJSON}` resolved per workspace from settings/secrets; usage recorded to the same table the Go `GET /api/settings/llm-usage` reads.
- **Flags:** none (library; consumers gate).
- **Accept:** transcript tests byte-compare request bodies; failover and timeout tests with `httptest`; `extract_json` vectors 100%; usage rows match recorded rows; `go test -race ./internal/llm/...`.

#### WP-WEB: Go web layer over the browser service (C2d; browser ops) — M
- **Owns:** `internal/leadweb/**` (new: search engines + fallback order, fetch facade, URL guard reuse, rate limiter, proxy accounting, search cache, HTML extract helpers: emails/phones/links/meta via x/net/html), testdata; `apps/browser-service/**` additions for ops WP-BROWSER did not ship (maps_search, extract, holehe, staffspy, stats) with their pytest; `internal/browserclient` additions for those ops; `internal/config/leadweb.go`.
- **Read:** `leadgen/search_engines.py`, `proxy_client.py`, `proxy_pool.py`, `rate_limiter.py`, `search_cache.py`, `http.py`, `enrichment/web_search.py`, `enrichment/content_extract.py`, `job_runner._extract_emails_from_html/_extract_phones_from_html/_is_junk_url/_is_skip_domain`, `core/url_guard.py`, `apps/searxng-pool`.
- **Record first:** engine fallback order for `SEARCH_FALLBACK_ORDER`/`SEARCH_ATTEMPT_ORDER`/`SEARCH_BACKENDS`, result normalisation, cache key + TTL/empty-TTL/backoff behaviour, HTML extraction vectors (emails, phones, junk/skip filters) over 30+ recorded pages, rate-limiter decisions, system-stats JSON shape.
- **Flags:** none (library).
- **Accept:** extraction vectors match Python; search fallback tests over fake engines; cache integration test on Postgres/Redis (whichever Python used); SSRF tests (private IPs, redirects to private, oversize); browser-service ops have pytest and a contract test from Go against a recorded fake.

#### WP-SCHED: Go scheduler and demo seed (E1, E5) — M
- **Owns:** `cmd/scheduler/**` (new binary), `internal/schedboot/**` (new: reconcile loop + the 6 missing bootstraps), `cmd/seed/**`, `internal/demoseed/**`, testdata.
- **Read:** `apps/api/scheduler.py`, the 9 `bootstrap_*` functions (inventory E1), Go `autoschedule.BootstrapSchedules`, `poller.BootstrapWatchSchedules`, `leadjobs.BootstrapSourceHealth`, `scripts/seed_demo.py`.
- **Record first:** for a seeded DB, the job rows (type, executor, payload, run_at, fire key) each Python bootstrap inserts on first and second run (idempotency); seed_demo rows (admin user, workspace, workbook columns, enqueued run).
- **Behaviour:** one reconcile every `SCHEDULER_RECONCILE_SECONDS` (min 10, default 60); a failing subsystem never starves others; a Postgres advisory lock guarantees one active scheduler (Go or Python) at a time. Executor of seeded jobs uses the same `config.Executor` functions as the API.
- **Flags:** none; the binary is only started by compose in WP-RETIRE-INFRA.
- **Accept:** per-bootstrap integration tests equal the recorded rows; double-run idempotency; advisory-lock test (second instance idles); seed idempotent on rerun.

#### WP-WBEXEC: run_workbook residual parity (D4, F13 in-process) — L
- **Owns:** `internal/workbook/**` except `pycolumns.go`, `internal/runplan/**`, `internal/api/workbookcell.go`, testdata.
- **Read:** `workbook/enrichment.py` (1801), `conditions.py`, `output.py`, `output_attempts.py`, `column_deps.py`, `services/go_executor.py`, `internal/emailverify`, `internal/destdeliver` (Sheets/Airtable/HubSpot/Salesforce/Instantly/Smartlead clients).
- **Close each refusal:** `provenance_on` (write provenance like Python), `automations_on` (emit through the shared `automationhook` emitter at the same points), `condition` (port `conditions.py` via `internal/safecond` if equivalent), `email_verify` (run the `emailverify` waterfall; its website step still goes through pyworker until WP-PROVIDERS), `output` columns (port `output.execute_output_column` onto destdeliver clients), `structured_target` without extra types, single-cell run (drop the `PYTHON_API_URL` proxy when the flag is on).
- **Record first:** per refusal reason, a workbook + run with Python's resulting cells/rows/events/spend/provenance rows.
- **Flags:** `GO_EXECUTOR_FULL_RUN` (eligibility accepts the closed reasons; default off).
- **Accept:** each closed reason has a vectors test equal to Python; existing workbook integration suite green; with the flag on, `runplan.Eligible` returns ok for every recorded workbook that has no Python-only provider; `ErrNeedsPython` unreachable with the flag on.

#### WP-AUTOLEGACY: legacy outreach automation actions (D5) — S
- **Owns:** `internal/autoactions/**`, `internal/autoengine/handback.go` (+ its test), testdata.
- **Read:** `automations/actions.py` (`sequencer` at :352, `send_email` at :79, in_flight marker), `outreach/sequence.py`, `outreach/store.py`; Go `outreachstore`, `smtpmail`, `outreachsend`.
- **Record first:** action outcomes for sequencer enrol (new, already enrolled, suppressed) and send_email (sent, in_flight replay, SMTP failure), with outreach rows and run-log entries.
- **Flags:** `GO_EXECUTOR_LEGACY_OUTREACH_ACTIONS` (when on, `handBackReason` no longer returns `legacy_outreach_action`). The `re_enrich_python_column` reason is removed by WP-CUTOVER once WP-WBEXEC and WP-AICOLS are on.
- **Accept:** vectors equal; at-most-once test for send_email; trigger_eval integration with the flag on runs fully in Go.

#### WP-ROUTES-A: no-dependency retained routes (F9, F10, F1 parity) — S
- **Owns:** new `internal/api/capabilityroutes.go`, `internal/api/trailingslash.go`, `internal/api/health.go` (if `/health` parity missing), `testdata/contract/retained_{audience,governance,workbook}_routes.txt`, `internal/api/{audience,governance}_coverage_test.go`; one registration block in `register.go`.
- **Read:** `integrations/certification.py` catalogs, `routers/audiences.py`, `destinations.py`, `governance.py`, `playbooks.py` capability routes; FastAPI `redirect_slashes` behaviour; `main.py` `/api`, `/health`.
- **Record first:** the 5 JSON bodies (with a test certification set), the response FastAPI gives for each trailing-slash pinned path (307 + Location vs served), `/health` and `/api` bodies.
- **Flags:** `GO_CAPABILITY_ROUTES_ENABLED`.
- **Accept:** contract tests byte-equal; retained lists shrink; coverage tests updated.

### Wave 3 (6 agents; after wave 2)

#### WP-PROVIDERS: the Python-only providers and RPC names (B1-B19, A1 partial) — L
- **Owns:** `internal/providers/scrape/**` (new subpackage: B1-B12, B15-B19), `internal/providers/python_providers.txt` (empties), `internal/providers/registry.go` registration lines, new `internal/rpclocal/**` (composite caller with the `pyworker.ProviderCall` signature: local registry first, then pyworker), call-site switches in `internal/emailverify/live.go`, `internal/poller/sources_py.go`, `internal/signalscan/scan.go`, `internal/personintel/pipeline.go`, `internal/mcp/production.go`, `internal/providerrun/run.go`; wiring lines in `cmd/{worker,api,mcp}/main.go`; testdata.
- **Read:** every module in inventory B (files listed there), `enrichment/provider.py`, `email_finder.py`, `decision_maker_finder.py`, `social_finder.py`, `size_heuristic.py`, `poller/rpc_providers.py`, `person_intel_rpc.py`, `routers/internal_provider.py` (scrub, cost, dedupe semantics).
- **Record first:** per provider, recorded browser responses (HTML/search JSON) in, `EnrichmentResult` out (fields, confidence, error, billing evidence, license); scrub and cost rules of `/internal/provider-call`; for `holehe`/`staffspy` the browser op responses.
- **Flags:** `GO_LOCAL_PROVIDERS` (comma list or `all`; names served in-process before pyworker; default empty).
- **Accept:** `python_providers.txt` has no entries and its guard test passes; per-provider vectors 100%; `rpclocal` routes `jobspy`, `jobspy_signals`, `ddgs_search`, `website_scraper`, `person_intel_deep` locally when listed; email waterfall test runs without pyworker.

#### WP-AICOLS: AI columns and research (B20, B21) — L
- **Owns:** `internal/aicol/**` (new: ai_formula, research, agent, ai_batch, prompt guard, presets), `internal/workbook/pycolumns.go`, the researcher in `internal/playbooks/engine.go` (`NewPyResearcher` replacement + `handBack` path), testdata; one wiring line in `cmd/worker/main.go`.
- **Read:** `workbook/column_rpc_providers.py`, `ai_column.py`, `ai_column_presets.py`, `prompt_guard.py`, `agent_column.py`, `research_column.py`, `batch_attempts.py`, `playbooks/rpc_research.py`, `vendor_catalog.py`; Go `internal/spend`, `vendorcatalog`.
- **Record first:** prompts sent (exact text) per column config, parsing of LLM outputs into cells, prompt-guard decisions, research step trace (queries, pages fetched, summary prompt), agent tool-selection loop transcripts, batch custom_id mapping; spend attempts.
- **Behaviour change:** agent columns reserve spend through the Go ledger, so the `agent_paid_tools` refusal goes away (flag on).
- **Flags:** `GO_NATIVE_AI_COLUMNS` (default off).
- **Accept:** vectors equal on prompt text and parsed cells; playbook run integration with no pyworker; agent spend test (paid tool reserved and settled once).

#### WP-COLLECT: collect + bulk_enrich jobs and source probes (D1, D2, C3, A7-A8) — L (long pole)
- **Owns:** `internal/collect/**` (new: JobRunner strategies, query expansion, region/city/size detection, business-name cleaning, AI stages, lead validator, source registry + probes), `internal/collect/scrapers/**` (Go side of C3: request building + result parsing over browserclient), `cmd/worker/handlers_collect.go` (+ one line in `main.go`), `internal/leadjobs` probe switch (replace `Prober` RPC), `internal/api/collectroute.go`, `jobswrite.go`, `leadsbulk.go` executor choice; testdata.
- **Read:** `leadgen/job_runner.py` (2311), `ai_stages.py`, `lead_validator.py`, `source_registry.py`, `source_health.py`, `scrapers/*`, `leadgen/models.py`, `export.py` (field names only); Go `collectledger`, `collectintent`, `leadscore`, `leaddedup`, `leadprogress`, `leadswrite`.
- **Record first:** per strategy, recorded browser responses -> leads (fields, sources, scores, dedup merges); stage records and progress events of a whole job; cancel and failure reconciliation (`reconcile_collect_job_failure`); bulk_enrich updates; source-list and probe results.
- **Flags:** `GO_EXECUTOR_COLLECT`, `GO_EXECUTOR_BULK_ENRICH` (executor 'go' at enqueue when also enabled + cohort), `GO_NATIVE_SOURCE_PROBE`.
- **Accept:** end-to-end collect job test on a fake browser produces the recorded ledger rows, stages, leads and progress frames; cancel mid-job; retry/failure hook; bulk_enrich vectors; source_health_check runs with no pyworker.
- **Split option:** if the agent judges L too large, scrapers (`internal/collect/scrapers`) go first as their own commit series; do not hand half to another agent in the same wave.

#### WP-PEOPLE: people discovery and contacts (C4 people half) — M
- **Owns:** `internal/leadpeople/**` (new: targeted_people, people_contacts, account_discovery, contact_execution), testdata.
- **Read:** `leadgen/targeted_people.py` (635), `people_contacts.py` (416), `account_discovery.py`, `contact_execution.py`, `enrichment/email_deliverability.py`, `licenses.py`, `email_waterfall.py`, `email_verify_cascade.py`; Go `providers/firmo/wikidata.go`, `entities`, `leaddedup`, `emailverify`.
- **Record first:** person lists for recorded search results, role matching and ranking, contact waterfall outcomes, license filtering, contact_execution rows.
- **Dependencies:** crosslinked comes from WP-PROVIDERS (same wave): consume it through `providers.Get("crosslinked")` behind an interface; tests use fakes.
- **Flags:** none (library; consumers gate).
- **Accept:** vectors equal; no DB writes outside `tenancy.InWorkspace`.

#### WP-COPILOT-CORE: copilot LLM round, preroute, prompt, describe, Go-only tools (A2, A3, A5, A6, A4 part) — M
- **Owns:** `internal/copilot/**` except tool files WP-COPILOT-TOOLS creates in wave 4, `internal/copilot/catalog.json`, `internal/api/copilotchat.go` (system-prompt call), testdata.
- **Read:** `services/copilot/{llm_stream,preroute,prompt,formatting,conversation}.py`, `tools.py` (`describe_action`, tool schemas), `routers/internal_copilot.py`, `routers/copilotkit.py`.
- **Tools in scope** (only need existing Go services): `start_collection`, `create_source_workbook`, `set_workbook_refresh`, `add_agent_column`, `add_signal_trigger`, `track_account_signals`, `ambitionbox_search`, `ambitionbox_jobs`, `import_ambitionbox_to_workbook`, `get_enrichment_gaps`, `compare_leads`, `find_similar_leads`, `enrich_lead`.
- **Record first:** SSE frames for recorded LLM transcripts (tool calls, text, usage, errors), preroute decisions for a corpus of user messages, system prompt text per workspace state, describe text per dangerous tool, each in-scope tool's result JSON.
- **Flags:** `GO_COPILOT_NATIVE` (Go llm/preroute/prompt/describe, and tools with a Go implementation; others still go to `/internal/copilot/tool`).
- **Accept:** chat turn integration with no pyworker for in-scope tools; frame-by-frame equality with recorded SSE; catalog lists every tool with `executor: go` once its port lands.

#### WP-LLMROUTES: LLM-backed routes (F2, F3, F4, F6) — M
- **Owns:** new `internal/api/{campaigngen,generatecolumn,provideraccuracy,leadenrich}.go`, `internal/nlcolumn/**`, `internal/provaccuracy/**` (eval scorer + golden set as embedded testdata), `testdata/contract/retained_{outreach,enrichment,leads,leadgen}_routes.txt` lines for these routes, matching coverage tests; one registration block in `register.go`.
- **Read:** `routers/campaigns.py`, `workbook/nl_column.py`, `leadgen/enrichment/eval/*` (+ its data), `routers/leads.py` `enrich_lead` (:374).
- **Record first:** request/response and SSE frames per route (LLM via transcript), generated column configs, accuracy report JSON over the golden set.
- **Dependencies:** WP-LLM; `find_emails`/`scrape_website` actions call providers by name (`website_scraper`, `ddg_email_provider`) through the registry, which WP-PROVIDERS fills in the same wave; tests use fakes.
- **Flags:** `GO_LLM_ROUTES_ENABLED`.
- **Accept:** contract tests; retained lists shrink by 4 routes.

### Wave 4 (3 agents; after wave 3)

#### WP-SOURCEWB: source_workbook job and preview (D3, F5) — L
- **Owns:** `internal/sourcewb/**` (new: source_engine, source_ingest, crm_import, people_search), `cmd/worker/handlers_sourcewb.go` (+ one line in `main.go`), `internal/api/workbooksources.go` (executor + preview), `internal/refresh/job.go` (executor), `testdata/contract/retained_enrichment_routes.txt` (preview line), testdata.
- **Read:** `workbook/source_engine.py`, `source_ingest.py`, `crm_import.py`, `people_search.py`, `entities/graph`; Go `collect`, `leadpeople`, `crm`, `entities`.
- **Record first:** row_added / source_done events, rows and stage status per source kind (collection query, people_search, crm_import), partial/failed completion, preview JSON.
- **Flags:** `GO_EXECUTOR_SOURCE_WORKBOOK`; preview under `GO_RUN_ROUTES_ENABLED` style flag `GO_SOURCE_PREVIEW_ENABLED`.
- **Accept:** each source kind end-to-end on fakes equals recorded rows/events; refresh chain enqueues 'go' when on.

#### WP-COPILOT-TOOLS: remaining copilot tools (A4 rest) — M
- **Owns:** new `internal/copilot/tools_people.go`, `tools_outreach.go`, `tools_plan.go`, `internal/autopilot/**` (plan store + executor), `internal/outreachdraft/**` (port of `outreach/drafting.py`), the catalog entries for these tools, testdata.
- **Tools:** `find_people_at_company`, `verify_people_at_company`, `enrich_people_contacts`, `create_people_workbook`, `suggest_outreach`, `draft_grounded_outreach`, `draft_plan`, `execute_plan`.
- **Read:** `copilot/tools.py` for these, `services/agent/autopilot*.py`, `outreach/drafting.py`.
- **Record first:** tool results JSON, drafting prompts and parsed drafts, plan JSON and step execution trace.
- **Flags:** covered by `GO_COPILOT_NATIVE`.
- **Accept:** catalog has 0 `executor: python` entries; `/internal/copilot/*` is unreachable with the flag on.

#### WP-ROUTES-B: browser-backed and registry routes (F7, F8, F11, A9) — M
- **Owns:** new `internal/unisearch/**` (academic source registry: arxiv, crossref, doaj, gutenberg, openlibrary, pubmed, semantic_scholar, google_search via leadweb, legacy), new `internal/api/{unifiedsearch,scrape,scrapehistory}.go`, `internal/api/systemstats.go`, `testdata/contract/retained_{leads,leadgen}_routes.txt` remaining lines, coverage tests; one registration block in `register.go`.
- **Read:** `sources/*`, `routers/scraper.py`, `routers/leads.py` `public_unified_search`/`public_scrape`, `routers/internal_leadgen.py` system-stats.
- **Record first:** unified search JSON per source with recorded HTTP; scrape response for recorded pages; scrape history rows; system-stats shape (now Go process + browser `stats` op; document the semantics change).
- **Flags:** `GO_SCRAPE_ROUTES_ENABLED`.
- **Decision (D4 below):** F11's `/api/v2/scrape`, `/api/v2/scrapes`, `/api/sources`, `/api/trending` are ported unless the user drops them.
- **Accept:** all retained lists empty except what D-decisions drop; contract tests.

### Wave 5 (2 agents; after wave 4 and a soak with all flags on)

#### WP-CUTOVER: delete the Python paths in Go (A1, A10, executor routing) — M
- **Owns:** `internal/pyworker/**` (deleted), `internal/config/{executor,*}.go` flag removal, every enqueue site listed in inventory section D "Cross-cutting", `cmd/worker/*.go` (`handBackInert`, `HandBack` call sites), `internal/queue/lease.go` + `db/queries/jobs.sql` (`HandBackToPython` removed; regenerate sqlc), `internal/credentials/client.go` (http mode deleted; local only), `internal/workbook/cellrun.go` (`ErrNeedsPython`), `internal/autoengine/handback.go`, `internal/rpclocal` (pyworker fallback removed), runbook `docs/go-port/runbooks/15-cutover.md`.
- **Behaviour:** every job is enqueued `executor='go'`; the cutover runbook drains the Python worker, then `UPDATE jobs SET executor='go' WHERE executor='python' AND status IN ('pending','retry')` (replacing `cli.py jobs reassign-executor`); `reset-admin` becomes a documented SQL/`cmd/api` subcommand if WP-MIG has not added one.
- **Accept:** `grep -rn 'pyworker\|"python"\|PY_WORKER\|HandBack' internal cmd` returns only comments/history; full integration suite green; no `GO_EXECUTOR_*`/`GO_*_ROUTES_ENABLED` flags remain (or each is documented as permanently on).

#### WP-RETIRE-INFRA: delete the Python services (E2, E3, E7, E8, F1, F10 nginx) — M
- **Owns:** `docker-compose.yml`, root `Dockerfile` (deleted or reduced to nothing Python), `infra/nginx/{default.conf,go-routes.conf,go-split.conf}`, `.github/workflows/{ci,release}.yml`, `Makefile`, `.env.example`, `README.md`, `docs/go-port/{inventory,parity}.md`, `docs/go-port/runbooks/**` (except WP-CUTOVER's), `apps/docs` pages that mention the Python services.
- **Changes:** compose loses `api`, `worker`, `scheduler`, `seed` (Python) and gains `browser-service` (internal network only, no secrets, no DB), `scheduler-go`, `seed-go` (profile demo), `worker-go` in the default profile; `migrate` is WP-MIG's. nginx: default upstream `api_go`, canary split and every `api_backend` location removed. CI: Python jobs replaced by a `browser-service` job (uv + pytest), Go jobs no longer seed `alembic_version` (WP-MIG decides). Release: no Python image except browser-service.
- **Accept:** `docker compose config` valid; `docker compose up` on a clean checkout without `legacy/python` serves login, a workbook run, a collect job, a copilot turn (scripted smoke); `grep -rn 'legacy/python\|apps.api\|api_backend' docker-compose.yml infra .github Dockerfile* Makefile` empty.

## Dependency graph

```
WP-MIG, WP-BROWSER (wave 1)
  -> WP-LLM, WP-WEB, WP-SCHED, WP-WBEXEC, WP-AUTOLEGACY, WP-ROUTES-A (wave 2)
       WP-PROVIDERS   <- WEB
       WP-AICOLS      <- LLM, WEB
       WP-COLLECT     <- LLM, WEB
       WP-PEOPLE      <- WEB (+ PROVIDERS crosslinked via interface, same wave)
       WP-COPILOT-CORE<- LLM
       WP-LLMROUTES   <- LLM (+ PROVIDERS via registry, same wave)           (wave 3)
            WP-SOURCEWB      <- COLLECT, PEOPLE, PROVIDERS
            WP-COPILOT-TOOLS <- COPILOT-CORE, PEOPLE, LLM
            WP-ROUTES-B      <- WEB, (COLLECT for source registry reuse)      (wave 4)
                 WP-CUTOVER, WP-RETIRE-INFRA <- everything                     (wave 5)
```

| WP | Wave | Size | Main Python replaced |
|---|---|---|---|
| WP-LLM | 2 | M | leadgen/llm.py, copilot/llm_stream.py |
| WP-WEB | 2 | M | search_engines, proxy_client/pool, rate_limiter, search_cache, http.py (Go half) + browser ops |
| WP-SCHED | 2 | M | scheduler.py + 6 bootstraps, seed_demo.py |
| WP-WBEXEC | 2 | L | workbook/enrichment.py residual, conditions, output |
| WP-AUTOLEGACY | 2 | S | automations/actions.py sequencer/send_email |
| WP-ROUTES-A | 2 | S | certification capability routes, trailing slashes, /health |
| WP-PROVIDERS | 3 | L | 18 Python-only providers + 3 RPC names |
| WP-AICOLS | 3 | L | ai/research/agent columns, research_column |
| WP-COLLECT | 3 | L | job_runner.py, scrapers, ai_stages, lead_validator, source_registry/health probes |
| WP-PEOPLE | 3 | M | targeted_people, people_contacts, account_discovery |
| WP-COPILOT-CORE | 3 | M | copilot llm/preroute/prompt/describe + 13 tools |
| WP-LLMROUTES | 3 | M | campaigns/generate, generate-column, provider-accuracy, lead enrich SSE |
| WP-SOURCEWB | 4 | L | source_engine, crm_import, people_search |
| WP-COPILOT-TOOLS | 4 | M | 8 remaining tools, autopilot, outreach drafting |
| WP-ROUTES-B | 4 | M | sources/ registry, scraper routes, system-stats |
| WP-CUTOVER | 5 | M | pyworker, handBack, executor routing, credentials http |
| WP-RETIRE-INFRA | 5 | M | compose/nginx/CI/Dockerfile/docs |

## Review Focus

1. **No lost or doubled jobs at the executor flip.** Jobs enqueued `executor='python'` before cutover must be drained or
   reassigned; hand-back paths must not loop once Python is gone (WP-CUTOVER deletes them in the same commit series that
   makes every enqueue 'go').
2. **Browser boundary security.** SSRF re-check per redirect hop inside the service; no secrets cross the boundary;
   response size and time caps; the service is not reachable from nginx.
3. **LLM prompt parity.** Prompts are product behaviour: exact text vectors, not paraphrases.
4. **Spend and caps.** Agent columns, research and paid providers reserve through the Go spend ledger; nothing spends
   outside it once Python is gone.
5. **Parsing parity.** bs4 vs `x/net/html` differences in text extraction and attribute order; dedup/scoring stay on the
   already-ported Go packages.
6. **One scheduler.** Advisory lock while Python and Go schedulers can coexist.

## Cross-package risks

- **R1 Browser API drift:** WP-BROWSER ships before needs are fully known. Mitigation: WP-WEB is the sole owner of
  browser-service additions in waves 2-4; others ask through their report.
- **R2 Long pole:** WP-COLLECT (job_runner 2.3k + scrapers 1.5k + helpers) gates WP-SOURCEWB and the cutover. Start it
  first in wave 3; WP-SOURCEWB can start on interfaces if wave 3 slips.
- **R3 Proxies are secrets:** `PROXY_LIST`/`PROXY_MANAGER_URL` may carry credentials, but the browser service must hold
  none. Decision D2 below.
- **R4 Shared files:** `cmd/worker/main.go` (WP-AICOLS, WP-COLLECT, WP-PROVIDERS in wave 3), `internal/api/register.go`
  (WP-LLMROUTES; WP-SOURCEWB and WP-ROUTES-B in wave 4), `catalog.json` (two copilot WPs in different waves),
  `internal/providerrun/run.go` (WP-PROVIDERS only). One-line edits, committed immediately, pathspec only.
- **R5 `automations_on` refusal** sends every run to Python today whenever automations are enabled; WP-WBEXEC must land
  it or nearly all production runs stay Python.
- **R6 Process-local semantics:** system-stats, rate limiting and search cache were per Python process; in Go they are per
  api/worker process plus the browser service. Document; do not fake aggregation.
- **R7 Schema ownership:** Python-only tables still read (scrape_history, search cache, source_stats) must be in WP-MIG's
  schema before WP-ROUTES-B / WP-WEB use them.
- **R8 Third-party libraries without Go equivalents** (holehe, staffspy, trafilatura, ddgs) stay as browser ops; if a
  library breaks upstream, only the browser image changes.
- **R9 Recording without legacy/python in CI:** vectors are committed; tests that read Python skip via
  `testutil.LegacyPythonDir`.

## Open decisions (defaults apply unless the user overrides)

- **D1 Browser-service scope:** default = the 7 ops in "Browser contract"; no DB, no secrets.
- **D2 Proxies:** default = proxy URLs are passed per request from Go (resolved from Go secrets) and never logged or stored
  by the service; alternative = browser service reads a dedicated non-credential proxy gateway URL from its env.
- **D3 provider-accuracy (F4):** default = port with the golden set embedded as testdata; alternative = drop the route
  (the web app does not call it).
- **D4 Scraper router extras (F11):** default = port `/api/v2/scrape` + history; drop `/api/trending` and `/api/sources`
  (admin-only, unused by the web app) unless the user objects.
- **D5 system-stats:** default = Go process counters + browser `stats`, per replica, documented as a deviation.
- **D6 Dropped items (F12, F13, G1-G4):** default = not ported, removed with the Python tree.

## Self-Review

- Every PORT item in the inventory maps to exactly one WP (A1/A10/E2/E3 to WP-CUTOVER/WP-RETIRE-INFRA; E4 to WP-MIG;
  E5 to WP-SCHED; E7/E8/F1 to WP-RETIRE-INFRA; F10 Go side to WP-ROUTES-A and nginx side to WP-RETIRE-INFRA).
- File ownership is disjoint inside each wave; cross-wave reuse of a file (`internal/workbook/pycolumns.go`,
  `catalog.json`, retained lists) is sequential.
- No WP plans WP-MIG or WP-BROWSER work beyond the browser ops WP-WEB adds.
