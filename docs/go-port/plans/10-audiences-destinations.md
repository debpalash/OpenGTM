# Audiences and Destinations Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Port audiences and audience destinations to Go: the 8 retained-in-scope `/api/audiences/*` routes, the 15 `/api/audience-destinations/*` routes (CRUD, sync/retry/cancel, runs, health, deliveries, inbound tokens and receipts, the public CRM inbound callback), the `audience_refresh` job (page-bounded materialization with exact membership diff, durable membership events, health and rescheduling) and the `audience_destination_sync` job (idempotent per-lead deliveries, paid-media hashed batches with exit reconciliation, checksum-verified warehouse JSONL export, cooperative cancellation, queue-failure reconciliation), so that Go runs the whole activation path and Python keeps only the signed-certification catalogs.

**Architecture:** All 10 tables (`audiences`, `audience_members`, `audience_membership_events`, `audience_schedules`, `audience_destinations`, `destination_runs`, `destination_deliveries`, `destination_inbound_tokens`, `destination_inbound_receipts`) already exist with RLS (the two schedule/token tables are intentionally non-RLS lookup tables); this plan needs no DDL. Go adds `internal/{audiencevalid,audiencesched,audiencerefresh,destvalidate,deststore,adsdrv,warehouse,destinbound,destdeliver,destsync}` and route files under `internal/api`. Membership is computed by the plan 05 `leadsread.QueryLeads` over a tenant transaction, so a refresh sees the same leads Python's `query_leads_page` sees. Delivery fans out per destination type: `webhook`/`slack`, the three ad platforms and `warehouse_http` are driven here; `hubspot`, `salesforce`, `instantly`, `smartlead`, `google_sheets`, `airtable` are consumed through small ports that plans 07 (connectors) and 08 (automation actions) fill (**assumes plan 07/08**). Executor routing is per destination type: a run whose destination type has no wired Go driver stays on Python. Python's scheduler remains the only ticker (`bootstrap_audience_schedules`); `schedule_next` is ported because routes and the refresh handler call it.

**Tech Stack:** Go 1.27.3 stdlib (`net/http`, `crypto/sha256`, `os` temp files), pgx v5 + sqlc; existing `internal/{httpx,identity,tenancy,audit,contract,queue,secrets,egress,canonjson,leadsread,ratelimit,config}`; no new modules. Python/uv for recorder, seed, vectors and transcripts.

**Spec:** `docs/go-port/inventory.md` section 8 row 9 "Audiences + destinations" (sections 2 HTTP surface, 3 jobs `audience_refresh` and `audience_destination_sync`, 5 stores, 7 integrations, 9 risks); `docs/go-port/plans/05-leads-signals.md` (`leadsread`); `docs/go-port/plans/06-enrichment-jobs.md` (`queue.EnqueueOnce`, executor routing, `Deps`); `docs/go-port/plans/09-outreach.md` (egress dial guard, public-route limiter conventions). Python sources: `apps/api/routers/{audiences,destinations}.py`, `services/audiences/{models,refresh,scheduler}.py`, `services/destinations/{models,engine,ads,warehouse,inbound}.py`, `migrations/versions/{1c2d3e4f5061_audiences,4f5061728394_audience_destinations}.py`.

**Layout note (branch `go-rewrite`):** Python lives untracked at `legacy/python/`; Python paths (`apps/api/...`, `scripts/...`, `migrations/...`) mean `legacy/python/<path>`. Go paths are relative to `apps/backend-go/`. Module `github.com/debpalash/OpenGTM/apps/backend-go`.

**Prerequisite:** plans 02-06 merged (identity, tenancy, secrets, `leadsread`, `queue.EnqueueOnce`, Go worker claim loop, `queue.HandBack`, `Deps.Limiter`, `egress`). Plans 07 and 08 are written concurrently. **assumes plan 08:** (a) `automations.EmitAudienceMembership(ctx, tx pgx.Tx, workspaceID string, events []MembershipEvent) error` (Python `events.emit_audience_membership`, called after commit in keyset pages of 500); (b) `actions.Webhook(ctx, workspaceID string, cfg map[string]any, lead map[string]any) ActionResult{Status, Summary, Error string}` (Python `_act_webhook`: URL resolution, SSRF-pinned send, header secrets); (c) `actions.PushCRM(ctx, workspaceID string, cfg map[string]any, lead map[string]any, leadID int) ActionResult` (Python `_act_push_crm`). **assumes plan 07:** `integrations.{Instantly,Smartlead}.AddLeadToCampaign`, `integrations.Sheets.UpsertRow`, `integrations.Airtable.UpsertRecord` with the Python return maps (`success`, `duplicate`, `error`, `lead_id`/`range`/`record_id`, `operation`). All are declared as local interfaces in `internal/destdeliver/ports.go` (Task 3); an unset port means that destination type is not Go-eligible and its runs stay on Python (D2). With no `Emitter`, the audience write routes and the refresh handler are not registered (D5).

## Global Constraints

- Go `go 1.27` + `toolchain go1.27.3`; pgx v5 + sqlc only; `make sqlc-check` clean. Go never runs DDL (none needed). No new modules.
- Tenant tables only inside `tenancy.InWorkspace` (API and worker). Runtime role non-superuser `NOBYPASSRLS`. Cross-workspace or unknown ids answer 404, never 403. The two non-RLS tables (`audience_schedules`, `destination_inbound_tokens`) hold identifiers and hashes only; the token lookup runs on the pool without a GUC and the callback then enters `InWorkspace` with the workspace id read from the token row, exactly like Python.
- Contract parity is the release bar: paths, methods, JSON key order, status codes (201/202/204 as Python), `detail` strings, auth gates, pagination shapes. Gates verbatim: reads = `current_workspace`; audience and destination mutations = `RequireRole("audiences.write","editor","admin")` / `RequireRole("activation.write","editor","admin")`; inbound-token routes = `RequireRole("secrets.manage","admin")`; `POST /inbound/{id}` = public bearer token (401 with `WWW-Authenticate: Bearer`). Audience `DateTime` columns are timezone-naive in Python: JSON timestamps are `isoformat()` without offset (microseconds only when nonzero); `destination_inbound_tokens.expires_at`/`last_used_at` are `timestamptz` and serialize with `+00:00`; fixtures pin both.
- Behaviour parity of job output is the release bar: `audience_members`/`audience_membership_events` rows and counts per refresh, `refresh_health` transitions, idempotency keys (`dest:<dest>:lead:<lead>:<sha256(canonical payload)>`, `...:remove:...`), delivery/run status and counter transitions, summaries and truncated error strings (1000), warehouse manifest and `records_sha256`, queue `fire_key`s (`destination_sync:<run>`, `audience_refresh:<aud>:<iso>`), ad request bodies. Python-generated vectors and HTTP transcripts for every algorithm; never hand-written.
- Security and crash hardening deviates from Python only as a documented deviation (D3-D8) recorded in `testdata/contract/README.md`: SSRF-guarded egress on every destination HTTP call (`internal/egress`, no redirects, dial-time IP check); path-segment validation of ad platform ids interpolated into URLs; 409 instead of 500 on duplicate rename; single-flight sync start per destination; atomic run insert + enqueue; bounded accounts rollup; `Authorization` and provider response bodies never logged or stored.
- Python's secret model is kept: destination `config` never holds credentials (keys `token|api_key|secret|password` rejected at any depth); secrets are workspace secrets referenced by name (`header_secret_ref`, `webhook_secret_ref`) or fixed names (`META_ACCESS_TOKEN`, `LINKEDIN_ACCESS_TOKEN`, `GOOGLE_ADS_ACCESS_TOKEN`, `GOOGLE_ADS_DEVELOPER_TOKEN`). Only SHA-256 identifiers leave the system to ad platforms; delivery `payload` for ads stores hashes only.
- Executors: `audience_refresh` is enqueued by Python with `executor='go'` only when `GO_EXECUTOR_AUDIENCE_REFRESH` is true; `audience_destination_sync` only when the destination type is in `GO_DEST_TYPES` (comma list, default empty). Go never claims a Python row. Redis not used.
- Postgres in tests: the existing `testpg` helper; HTTP drivers tested against recorded transcripts and `httptest` servers behind an injectable `Endpoints` struct, never the internet.
- Verify with `cd apps/backend-go && make sqlc-check vet test` and `make test-integration`. No CI changes.
- Every commit DCO signed (`git commit -s`), no `Co-Authored-By` trailer. GRANULAR commits: tests first (RED, message ends `(RED)`), then each implementation piece. Stage explicit paths only; `make sqlc` regenerates all of `internal/db/gen/`, so each task stages only its own `<name>.sql.go` and `.sql`.
- Retained on FastAPI after this plan (documented in `testdata/contract/retained_audience_routes.txt`, Task 22): `GET /api/audiences/capabilities` and `GET /api/audience-destinations/types` (signed-certification catalogs shared with integrations and governance, D1).

## Review Focus

1. **Exact membership diff under concurrency.** Scheduled and manual refreshes of one audience serialize on `SELECT ... FOR UPDATE` of the audience row; the diff is page-bounded (500), `refresh_token` marks the seen set, exits are computed from the unseen remainder in stable keyset order, entered/changed/unchanged counts and `member_count = matched` match Python; events are persisted in the same transaction as the diff and emitted after commit in keyset pages; a failure leaves health `degraded` with `consecutive_refresh_failures` incremented and the next occurrence still scheduled. Tasks 5, 12, 14.
2. **Double or lost delivery.** Idempotency key = destination + lead + payload fingerprint; a `success` prior is skipped, a failed prior is retried and re-homed to the new run; ad batches mark `in_flight` and increment `attempts` before the request and settle all rows of the batch together; removal reconciliation reads only hashes from the latest successful `upsert` per lead and never hashes twice; no DB row lock or transaction is held across an external HTTP call; queue retry resets `in_flight` to `pending`, terminal failure fails `pending`/`in_flight` and recomputes counters. Tasks 7, 13, 14.
3. **Cancellation.** `cancelling`/`cancelled` is re-read before every member and before every external batch; a cancel that lands during the final request keeps its completed delivery results, ends the run `cancelled` and never overwrites with `completed`; `pending` runs are finished cancelled in the route; `cancelled` is never overwritten by a late completion. Tasks 7, 13, 19.
4. **Ad platform correctness and SSRF.** Requests are byte-compatible with the recorded transcripts (Meta form body and `DELETE`, LinkedIn `dmpSegments` batch with element status validation, Google Ads offline job create, `addOperations` with partial-failure summary that retries the whole batch, then `run`); platform ids are validated before they reach a URL path (Python interpolates unvalidated config); hosts are fixed; 401/403 map to the same authorization error text; provider error bodies are summarized, never stored with PII. Tasks 8, 11.
5. **Inbound callback trust.** Bearer token compared by SHA-256 hash in constant time, bound to the destination in the URL, expiry and revocation enforced, `last_used_at` stamped; unknown/expired/revoked/foreign-destination all answer the same 401 string; events are idempotent on `(workspace, destination, external_event_id)` including the concurrent-insert race; only the allow-listed lead columns can be written, never through string-built SQL; the endpoint is rate limited by client IP. Tasks 10, 21.
6. **Crash bugs behind 500.** Duplicate rename of a destination (uncaught `IntegrityError`), unparseable website in the accounts rollup (`urlparse` ValueError), a webhook/slack template that `str.format` rejects, an enqueue failure that strands a `pending` run forever (blocks every later sync), two concurrent `sync` calls creating two active runs. Tasks 4, 6, 7, 11, 18, 19.

## File Structure

```
internal/audiencevalid/{filters,accounts,pyround}.go        # _validate_filters, filter -> leadsread.Filter, _account_key + accounts rollup, Python round()
internal/audiencesched/{schedule,failure}.go                # schedule_next, remove_schedule, mirror, reconcile_audience_refresh_failure
internal/audiencerefresh/{refresh,events,observed}.go       # refresh_audience, events emission, refresh_audience_observed
internal/destvalidate/{config,bodies,template}.go           # _validate_config, create/patch bodies, cursors, slack template
internal/deststore/{destinations,runs,deliveries,health,enqueue,cursor}.go
internal/adsdrv/{hash,meta,linkedin,google,batch}.go        # hashed identifiers, three platform drivers, Endpoints
internal/warehouse/{buffer,export}.go                       # WarehouseExportBuffer, sync_warehouse_export
internal/destinbound/{token,reconcile}.go                   # tokens (non-RLS), reconcile
internal/destdeliver/{deliver,webhook,slack,ports}.go       # per-type delivery + ports for plans 07/08
internal/destsync/{sync,ads,warehouse,cancel,failure}.go    # handle_destination_sync, reconcile_destination_job_failure
cmd/worker/main.go                                          # (modify, Task 14: audience_refresh, audience_destination_sync handlers)
internal/api/{audiencescrud,audiencesread,audiencesaccounts,destscrud,destsruns,destsinboundtokens,destsinbound}.go
internal/api/register.go                                    # (modify once, Task 3)
internal/db/queries/{audiences,audience_members,audience_events,destinations,destination_runs,destination_deliveries,destination_inbound}.sql (+ gen/)
testdata/contract/{audiences,destinations}/*.json
testdata/contract/retained_audience_routes.txt
testdata/transcripts/ads/*.json, testdata/warehouse/*.jsonl    # Python-recorded
internal/{audiencevalid,destvalidate,deststore,adsdrv,warehouse,destinbound,destdeliver,destsync,audiencerefresh}/testdata/*.json
../../legacy/python/apps/api/services/go_executor.py        # (modify, Task 2)
../../legacy/python/scripts/{contract_seed.py,contract_cases/audiences.yaml,gen_audience_vectors.py,record_ad_transcripts.py}
../../infra/nginx/go-routes.conf
../../docs/go-port/runbooks/audiences-destinations-cutover.md
```

## Parallel waves

Tasks in a wave touch disjoint files and may run concurrently (up to 6 agents at a time). Each wave starts only after the previous wave is merged and `make vet test test-integration` is green.

| Wave | Tasks | Shared-file notes |
|---|---|---|
| 0 (sequential) | 1 Python seed, cases, vectors, transcripts; 2 Python executor routing; 3 Go scaffold | T1 owns `contract_seed.py`, `gen_audience_vectors.py`, `record_ad_transcripts.py`; T2 owns `go_executor.py` and the enqueue sites in `audiences/scheduler.py` and `destinations/engine.py` (+ `routers/destinations.py` enqueue sites while those routes stay on Python); T3 owns `register.go`, `Deps`/`config` additions, all stub route files, `destdeliver/ports.go` |
| 1 | 4 audiencevalid, 5 audiencesched, 6 destvalidate, 7 deststore, 8 adsdrv, 9 warehouse | 6 disjoint packages; 5 and 7 each own their `.sql` files |
| 2 | 10 destinbound, 11 destdeliver, 12 audiencerefresh | 3 packages; 12 consumes 4, 5, 7 |
| 3 | 13 destsync | consumes 7-11 |
| 4 (sequential) | 14 worker wiring | edits `cmd/worker/main.go` |
| 5 | 15 audience CRUD routes, 16 audience refresh/members/events routes, 17 accounts route, 18 destination CRUD routes, 19 sync/runs/health/deliveries/retry/cancel routes, 20 inbound token + receipts routes, 21 public inbound callback | each owns exactly one stub `internal/api/*.go` and one fixture subdir; 7 tasks, run as batches of 4 + 3 |
| 6 | 22 coverage + nginx + runbook | consumes everything |

---

### Task 1: Python seed, recorder cases, vectors, ad transcripts

**Files:**
- Modify: `legacy/python/scripts/contract_seed.py` (append-only)
- Create: `legacy/python/scripts/contract_cases/audiences.yaml`, `legacy/python/scripts/gen_audience_vectors.py`, `legacy/python/scripts/record_ad_transcripts.py`
- Create (output, committed): `testdata/contract/{audiences,destinations}/*.json`, `internal/{audiencevalid,destvalidate,deststore,adsdrv,warehouse,destinbound,destdeliver,destsync,audiencerefresh}/testdata/*.json`, `testdata/transcripts/ads/*.json`, `testdata/warehouse/*.jsonl`

**Interfaces:**
- Consumes: plan 04/05 seed (workspaces A/B, editor/viewer/admin, leads with varied cities, scores, emails, phones, websites, signals).
- Produces: seed additions (workspace A: 3 audiences (dynamic filter, empty filter, disabled refresh), members with snapshots, entered/exited events, schedule rows; destinations of every type (`webhook`, `hubspot`, `salesforce`, `warehouse_http`, `meta_ads`, `google_ads`, `linkedin_ads`, `instantly`, `smartlead`, `google_sheets`, `airtable`, `slack`) with runs in every status, deliveries in every status, an active inbound token (raw value stored in the fixture so the callback can be replayed), an expired and a revoked token, inbound receipts; workspace B with its own audience and destination; workspace secrets for warehouse/slack refs and ad tokens). Vectors: `_validate_filters` (30), filter -> `query_leads_page` membership parity on a 200-lead corpus (every filter key and combination), `_account_key` (60 websites incl. schemes, `www.`, ports, bad IPv6 brackets, empty), accounts rollup + `round()` (banker's) on 5 audiences, `_validate_config` per type (120 cases, exact messages), `_map_payload` and `_fingerprint` (canonical JSON `sort_keys`, compact, `default=str`, unicode, floats, nested), slack `str.format_map` vectors, `hash_email`/`hash_phone`/`identifiers_supported`, `_google_partial_failure`/`_linkedin_batch_failure` bodies, history cursor encode/decode (valid, naive timestamp, garbage), `schedule_next` times, refresh/failure reconcile sequences, sync-flow transcripts for `handle_destination_sync` against stub deliverers (success, skipped, failure, cancel at every boundary, retry, removal reconciliation, warehouse), warehouse JSONL byte goldens (small and spilled-to-disk). Transcripts: ad platform HTTP exchanges for add and remove on each platform (success, 401/403, 500, Google partial failure, LinkedIn incomplete batch) recorded by running the Python drivers through `httpx.MockTransport`.

- [ ] **Step 1: Extend the seed** (fixed ids, timestamps, token values; naive timestamps where the Python columns are naive).
- [ ] **Step 2: Write the cases** (each `{name, method, path, as, profile, expect_status}`): audiences list/create (defaults, every filter, unknown filter 422, `has_email` non-bool 422, `min_score > max_score` 422, blank name 422, interval 14 and 10081 422, duplicate name 409)/patch (name, filters re-materializes, interval reschedules, duplicate name 409, 404)/delete (204, 404)/refresh (counts, 404)/members (`limit`, `before_id`, `offset`, bounds 422)/accounts (`limit`, `offset`, bounds 422, empty audience)/events (`limit`, `before_id`); destinations list (`audience_id` filter, `latest_run`)/create (every type, each validation message, missing secret ref 422, unknown audience 404, duplicate 409, bad type 422, credentials-in-config 422)/patch (config, name, rename duplicate -> recorded 500 marked as known crash in the case file, D3)/sync (202 new, 202 existing active, 409 disabled, 404)/runs (cursor, offset, `has_more`, bad cursor 422)/health/deliveries (`status`, `after_id`, `limit` bounds, bad status 422)/retry (202, 409 wrong status, 409 disabled, 404)/cancel (202 pending, 202 running -> cancelling, 409 terminal, 404)/inbound-token rotate, status (none, active, expired), revoke (204), non-CRM 409/receipts (cursor)/inbound callback (valid, replay, unmatched, no_change, bad token 401, foreign destination token 401, expired, revoked, invalid body 422, disabled destination 404)/delete (204, 404).
- [ ] **Step 3: Write the vector and transcript scripts**; they import the real Python modules.
- [ ] **Step 4: Run** `uv run python scripts/contract_record.py --cases scripts/contract_cases/audiences.yaml`, `uv run python scripts/gen_audience_vectors.py`, `uv run python scripts/record_ad_transcripts.py`; Expected: files written, re-running is byte-identical.
- [ ] **Step 5: Commit** (Go/doc side only) `test(go): audience and destination fixtures, vectors and ad transcripts (RED)`.

### Task 2: Python executor routing

**Files:**
- Modify: `legacy/python/apps/api/services/go_executor.py` (`audience_refresh_executor() -> str`, `destination_executor(destination_type: str) -> str`), `legacy/python/apps/api/services/audiences/scheduler.py` (`schedule_next`, `bootstrap_audience_schedules` enqueue sites), `legacy/python/apps/api/services/destinations/engine.py` (`enqueue_audience_syncs`), `legacy/python/apps/api/routers/destinations.py` (`start_sync`, `retry_run` enqueue sites)
- Test: `legacy/python/tests/test_audience_executor_routing.py`

**Interfaces:**
- Produces: `audience_refresh_executor()` returns `"go"` iff env `GO_EXECUTOR_AUDIENCE_REFRESH` is truthy; `destination_executor(t)` returns `"go"` iff `t` is in env `GO_DEST_TYPES` (comma list, case-insensitive); each enqueue site passes `executor=` to `enqueue_job_once`/`queue_service.add_job`; `fire_key`, payload, priority untouched; the Python failure/reconcile callbacks (`reconcile_destination_job_failure`, `reconcile_audience_refresh_failure`) are unchanged and remain responsible only for `executor='python'` rows.

- [ ] **Step 1: Write failing tests**: `test_audience_refresh_executor_follows_flag`, `test_destination_executor_follows_type_list`, `test_enqueue_sites_pass_executor`, `test_flags_default_python`, `test_fire_keys_and_payload_unchanged`. **Step 2: Run** `uv run pytest tests/test_audience_executor_routing.py` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** (unversioned; note in PR).

### Task 3: Go scaffold: Deps, config, register lines, stubs, ports

**Files:**
- Modify: `internal/api/register.go` (+`RegisterAudiences`, `RegisterDestinations` lines, `Deps` gains `AudienceEvents audiencerefresh.Emitter` and `DestPorts destdeliver.Ports`), `internal/config/config.go` (`GO_AUDIENCE_ROUTES_ENABLED`, `GO_DEST_ROUTES_ENABLED` default false; `GO_EXECUTOR_AUDIENCE_REFRESH`; `GO_DEST_TYPES`; `OPENGTM_DESTINATION_TOKEN_TTL_DAYS` 90 clamped 1-365; `AUDIENCE_REFRESH_PAGE_SIZE` 500; `DEST_MEMBER_PAGE_SIZE` 500; `DEST_AD_BATCH_SIZE` 5000; `AUDIENCE_ACCOUNTS_MAX_MEMBERS` 200000; `DEST_DELIVERY_TIMEOUT` 30 s)
- Create: stub files `internal/api/{audiencescrud,audiencesread,audiencesaccounts,destscrud,destsruns,destsinboundtokens,destsinbound}.go` (`func registerX(r *httpx.Router, d Deps)`, empty body), `internal/destdeliver/ports.go`, `internal/audiencerefresh/emitter.go`
- Test: `internal/api/audscaffold_test.go`

**Interfaces:**
- Produces: `RegisterAudiences(r, d)` / `RegisterDestinations(r, d)` (register nothing and log when `PG_LEAD_STORE` or their flag is false; `RegisterAudiences` additionally requires `d.AudienceEvents != nil`, D5); `audiencerefresh.Emitter interface { Emit(ctx, tx pgx.Tx, workspaceID string, events []MembershipEvent) error }` (**assumes plan 08**, `MembershipEvent{ID int; AudienceID string; LeadID int; EventType string; Snapshot map[string]any; RefreshID string; CreatedAt time.Time}`); `destdeliver.Ports{Webhook WebhookSender; CRM CRMPusher; Instantly, Smartlead CampaignAdder; Sheets SheetsUpserter; Airtable AirtableUpserter}` (**assumes plan 07/08**, Python return maps as typed results) with `func (p Ports) Supports(destinationType string) bool` (webhook/slack need `Webhook`, hubspot/salesforce need `CRM`, ads and warehouse always true) feeding `GO_DEST_TYPES` validation at startup (a configured type without its port is a fatal config error, never a silent Python fallback).

- [ ] **Step 1: Write failing tests**: `TestAudienceRoutesNotRegisteredWithoutFlagOrEmitter`, `TestDestinationRoutesNotRegisteredWithoutFlag`, `TestAudienceRoutePatterns` (exactly the 8 routes of Tasks 15-17), `TestDestinationRoutePatterns` (exactly the 15 routes of Tasks 18-21), `TestConfigDefaultsMatchPython`, `TestGoDestTypesRequirePorts`, `TestSchemaHasAudienceDestinationTables` (10 tables; 8 RLS forced; `audience_schedules` and `destination_inbound_tokens` without RLS, as Python).
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test` -> PASS.
- [ ] **Step 5: Commit** `test(go): audience and destination scaffold (RED)`, `feat(go): audience and destination scaffold, config and ports`.

### Task 4: Audience filters, accounts rollup, Python rounding

**Files:**
- Create: `internal/audiencevalid/{filters,accounts,pyround}.go`, `internal/audiencevalid/valid_test.go`

**Interfaces:**
- Consumes: `leadsread.Filter` (plan 05).
- Produces: `FilterKeys` (the 14 keys), `func ValidateFilters(v map[string]any) (map[string]any, error)` (error text identical to Python's `ValueError`: `unsupported audience filters: a, b` sorted, `has_email must be a boolean`, `min_score must be an integer` (bool rejected, JSON ints only), `min_score cannot exceed max_score`); `func ToLeadFilter(f map[string]any) (leadsread.Filter, error)` (every key mapped to the same semantics as `query_leads_page`, `job_ids` and `search` included); `func AccountKey(snapshot map[string]any) (key, company string)` (website lower-cased, scheme defaulted to `https://`, hostname without `www.`, else `company:<whitespace-collapsed lower>`; an unparseable URL falls back to the company key instead of raising, D3); `func Rollup(members iter.Seq2[Member, error], signals map[int]Signal, limit, offset, maxMembers int) (Result, error)` (the exact fields, decision-maker terms, `profiles[:10]`, sort `(-signal_weight, -avg_score, -contacts, company.lower())`, `summary`, `pagination`, `next_offset`; `ErrTooLarge` past `maxMembers`); `func PyRound(x float64, ndigits int) float64` (Python `round()` half-even on the float's exact decimal expansion, used for `avg_score` and `email_coverage_pct` `int` results).

- [ ] **Step 1: Write failing tests**: `TestValidateFiltersMatchesVectors`, `TestToLeadFilterMatchesQueryLeadsPageVectors` (membership of the 200-lead corpus per filter combination), `TestAccountKeyMatchesVectors`, `TestAccountKeyBadURLNoPanic`, `TestRollupMatchesPythonOutput`, `TestRollupSortStable`, `TestRollupTooLarge`, `TestPyRoundHalfEvenVectors` (0.05, 2.675, 1.5, 2.5, negatives), `TestDecisionMakerTerms`.
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test` -> PASS.
- [ ] **Step 5: Commit** `test(go): audience filter and rollup vectors (RED)`, `feat(go): audience filter validation, rollup and Python rounding`.

### Task 5: Audience schedule

**Files:**
- Create: `internal/db/queries/audience_schedules.sql`, `internal/audiencesched/{schedule,failure}.go`, `internal/audiencesched/sched_integration_test.go`

**Interfaces:**
- Consumes: `queue.EnqueueOnce` (plan 06).
- Produces: `func ScheduleNext(ctx, tx pgx.Tx, a ScheduleInput, now time.Time, executor string) (*time.Time, error)` (= `schedule_next`: cancel pending `audience_refresh` jobs whose `fire_key LIKE 'audience_refresh:<id>:%'`; disabled -> clear `next_refresh_at`, mirror disabled, return nil; else interval clamped 15..10080 minutes, `next_at = now + interval`, `fire_key = audience_refresh:<id>:<next_at.isoformat()>`, payload `{workspace_id, audience_id}`, `next_run_at = next_at`, mirror row upserted); `RemoveSchedule(ctx, tx, audienceID)`; `func ReconcileFailure(ctx, pool, jobID int64, payload map[string]any, errText string, willRetry bool, now time.Time) error` (= `reconcile_audience_refresh_failure`: already-reconciled guard when `next_refresh_at > now`, health `degraded`, `last_refresh_error = "Queue retry: ..."|"Final failure: ..."` cut to 1000, failure counter, mirrors the queue's retry time or calls `ScheduleNext` after a terminal failure); `func DueSchedules(ctx, pool, now time.Time) ([]DueRow, error)` for a later Go scheduler (non-RLS mirror). Naive timestamps in, UTC naive out, as Python.

- [ ] **Step 1: Write failing tests**: `TestScheduleNextMatchesVectors`, `TestScheduleNextCancelsPendingOnly`, `TestScheduleNextDisabledClears`, `TestIntervalClamped`, `TestScheduleSingleFlightConcurrent` (16 goroutines), `TestReconcileFailureRetryMirrorsQueueTime`, `TestReconcileFailureTerminalSchedulesNext`, `TestReconcileFailureAlreadyReconciledNoDoubleCount`, `TestRemoveScheduleDeletesMirror`, `TestScheduleRollsBackWithTx`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): audience schedule (RED)`, `feat(go): audience schedule_next and failure reconcile`.

### Task 6: Destination config validation, bodies, cursors, slack template

**Files:**
- Create: `internal/destvalidate/{config,bodies,template,cursor}.go`, `internal/destvalidate/validate_test.go`

**Interfaces:**
- Produces: `Types` (the 12 types); `func ValidateConfig(destType string, cfg map[string]any) (map[string]any, error)` (credential-key scan at any depth first: `credentials must use workspace secrets, not destination config`; then the per-type allowed keys, URL rules, method, `consent_attested` and `consent_source`, required ad ids, instantly/smartlead `campaign_id`, sheets `columns` 1-100, airtable base/table/`idempotency_field`, slack ref and template <= 2000; every message copied verbatim from Python); ad platform ids (`customer_id`, `user_list_id`, `login_customer_id`, `custom_audience_id`, `segment_id`, `api_version`) additionally validated against `^[0-9A-Za-z_.-]{1,64}$` / `^v[0-9]{1,2}(\.[0-9])?$|^[0-9]{6}$` style patterns pinned by the vectors (D4, 422 `invalid ad destination config: <key>`); `func DecodeCreate(raw []byte) (Create, error)` / `DecodePatch` / `DecodeInbound` returning FastAPI-shaped `[]httpx.FieldError` (type `value_error` messages `Value error, unsupported destination type 'x'`, `Value error, name cannot be blank`, `Value error, fields may contain at most 50 entries`, `Value error, field values must be scalar`, `Value error, field values may not exceed 10000 characters`, length/ge/le errors, missing fields) with `exclude_unset` semantics for PATCH (a present `null` is distinct from an absent key); `EncodeHistoryCursor(createdAt time.Time, id string) string` / `DecodeHistoryCursor(cursor, label string) (time.Time, string, error)` (`invalid <label> cursor` 422; naive timestamps taken as UTC); `func RenderSlackTemplate(template string, fields map[string]any) (string, error)` supporting `{name}` and `{{`/`}}` only with missing keys empty (Python `__missing__`); any attribute, index, conversion or format-spec field is rejected at create/patch with 422 `slack message_template uses unsupported fields` (D6) and, for a stored legacy template, makes the delivery fail with `invalid slack message_template` rather than guessing.

- [ ] **Step 1: Write failing tests**: `TestValidateConfigMatchesVectors` (120 cases), `TestCredentialKeysRejectedAtAnyDepth`, `TestAdIDsRejectPathInjection` (`../`, `?`, `#`, `%2f`, whitespace, over-long), `TestDecodeCreateErrorsMatchFastAPI`, `TestDecodePatchExcludeUnset`, `TestDecodeInboundBounds`, `TestCursorRoundTripAndErrors`, `TestRenderSlackMatchesVectors`, `TestSlackUnsupportedFieldSyntaxRejected`, `TestNameTrimAndBlank`.
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test` -> PASS.
- [ ] **Step 5: Commit** `test(go): destination validation vectors (RED)`, `feat(go): destination config validation, request bodies and cursors`.

### Task 7: Destination store: destinations, runs, deliveries, health, enqueue

**Files:**
- Create: `internal/db/queries/{destinations,destination_runs,destination_deliveries}.sql`, `internal/deststore/{destinations,runs,deliveries,health,enqueue}.go`, `internal/deststore/store_integration_test.go`

**Interfaces:**
- Consumes: Task 6 cursors, `queue.EnqueueOnce`, `tenancy.InWorkspace`.
- Produces: destinations: `CreateDestination` (unique `(workspace, audience, name)` -> `ErrDuplicate`), `UpdateDestination` (rename duplicate -> `ErrDuplicate`, D3), `GetDestination`, `ListDestinationsWithLatestRun(ctx, tx, audienceID string)` (rank-1 run join, ordered `updated_at DESC, id DESC`), `DeleteDestination` (revokes inbound tokens first like Python); runs: `ActiveRun(ctx, tx, destID)` (`pending|running|cancelling`), `StartRun(ctx, tx, wsID, destID, requestedBy, executor string) (Run, created bool, err error)` (single-flight under `pg_advisory_xact_lock(hashtext('destsync:'||destID))`; run insert and `EnqueueOnce('audience_destination_sync', {workspace_id, run_id}, fire_key='destination_sync:'||runID)` in one transaction, D7), `EnqueueAudienceSyncs(ctx, tx, wsID, audienceID, executorFor func(destType string) string) (int, error)` (every enabled destination without an active run, `requested_by="audience_refresh"`), `RetryRun`, `CancelPendingRun`/`RequestCancel`, `FinishCancelled(ctx, tx, run, dest)` (pending/in_flight deliveries to `cancelled` with `Run cancelled before delivery`, counts recomputed, `last_success_at` when succeeded), `DeliveryCounts`, `ListRuns` (cursor + offset, `limit+1` has_more), `ListDeliveries(status, afterID, limit)`, `Health(ctx, tx, destID)` (run_counts by status, delivery counts, `success_rate = round(succ/total, 4)` Python rounding, `consecutive_unhealthy_runs` over the latest 20, `recent_failures` 10, `latest_run`), `UpsertDelivery`/`SettleDelivery` helpers used by the sync, `ReconcileJobFailure(ctx, tx, run, willRetry bool, errText string)` (= `reconcile_destination_job_failure`).

- [ ] **Step 1: Write failing tests**: `TestDestinationCRUDMatchesFixtures`, `TestDuplicateNameIsErrDuplicateOnCreateAndRename`, `TestListLatestRunPerDestination`, `TestStartRunSingleFlightConcurrent` (16 goroutines, one run, one job), `TestStartRunAndEnqueueAtomic` (enqueue failure leaves no run), `TestEnqueueAudienceSyncsSkipsActive`, `TestFinishCancelledRecomputesCounts`, `TestRunsCursorPagination`, `TestRunsOffsetWithoutCursor`, `TestHealthMatchesFixtures`, `TestHealthSuccessRateRounding`, `TestDeliveriesFilterAndOrder`, `TestReconcileFailureRetryResetsInFlight`, `TestReconcileFailureTerminalFailsPending`, `TestReconcileCancellingFinishesCancelled`, `TestDeleteRevokesTokens`, `TestTenantIsolation`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement** (`make sqlc`, stage only the three `.sql.go`). **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): destination store (RED)`, `feat(go): destination, run and delivery store`, `feat(go): atomic run start, audience sync enqueue and failure reconcile`.

### Task 8: Ad platform drivers

**Files:**
- Create: `internal/adsdrv/{hash,meta,linkedin,google,batch}.go`, `internal/adsdrv/ads_test.go`

**Interfaces:**
- Consumes: `egress.NewClient`, workspace secrets via `destdeliver`-style `SecretGetter func(ctx, wsID, name string) (string, error)`.
- Produces: `HashEmail(v string) string`, `HashPhone(v string) string`, `HashedIdentifiers(snapshot map[string]any) map[string]string` (email from `email`, phone from `phone` then `mobile_phone`), `IdentifiersSupported(destType string, ids map[string]string) bool`; `type Endpoints struct{ Meta, LinkedIn, Google string }` (production values `https://graph.facebook.com`, `https://api.linkedin.com`, `https://googleads.googleapis.com`; test servers only through this struct); `type Result struct{ Success bool; Summary, Error, ExternalID string }`; `func SyncBatch(ctx, c Client, wsID, destType string, cfg map[string]any, snapshots []map[string]any, op string) (Result, error)` (`op` `add|remove`, anything else error; remove accepts already-hashed 64-hex values unchanged; no rows -> `no valid email or phone identifiers`; Meta: `POST`/`DELETE {base}/{api_version|v23.0}/{custom_audience_id}/users` with form fields `access_token` and `payload={"schema":[...],"data":[...]}`, `external_id = session_id or audience id`; LinkedIn: `POST /rest/dmpSegments/{segment_id}/users` with `Linkedin-Version` (default `202607`), `X-Restli-Protocol-Version: 2.0.0`, `X-RestLi-Method: BATCH_CREATE`, elements per row with email only (`LinkedIn requires a valid email identifier` when none), response validated per element (`status == 201`, count match, safe status list); Google: create offline job, `addOperations` with `enablePartialFailure` (partial failure -> whole batch failed with the sanitized `Google Ads rejected operations (status=..., code=..., count=...)` summary and job name as external id, job not run), then `:run`); 401/403 -> `platform authorization or API approval required (<code>)`; other non-2xx -> error with status only; secrets missing -> `missing workspace secret <NAME>`; all calls 30 s, no redirects, response bodies capped at 1 MiB and never copied into errors.

- [ ] **Step 1: Write failing tests**: `TestHashesMatchVectors`, `TestIdentifiersSupportedByPlatform`, `TestMetaRequestsMatchTranscripts` (add and remove, byte-compare method, URL, form body), `TestLinkedInRequestsAndBatchValidationMatchTranscripts`, `TestGoogleThreeStepFlowMatchTranscripts`, `TestGooglePartialFailureSanitizedAndNotRun`, `TestAuthErrorsMapToPythonText`, `TestMissingSecretError`, `TestRemoveKeepsPrehashedValues`, `TestInvalidOperationErrors`, `TestBlockedHostRefused` (egress), `TestNoRedirectFollowed`, `TestResponseBodyBounded`, `TestSecretsNeverInErrorsOrLogs`.
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test` -> PASS.
- [ ] **Step 5: Commit** `test(go): ad platform driver transcripts (RED)`, `feat(go): hashed identifiers and Meta driver`, `feat(go): LinkedIn and Google Ads drivers`.

### Task 9: Warehouse export

**Files:**
- Create: `internal/warehouse/{buffer,export}.go`, `internal/warehouse/warehouse_test.go`

**Interfaces:**
- Consumes: `egress.NewClient`, secrets getter.
- Produces: `type Buffer struct{...}` with `NewBuffer(runID, workspaceID string, dest DestRef, maxMemory int) *Buffer` (default 1,000,000 bytes then spill to an unlinked `0600` temp file; PII never left in a named file), `Append(leadID int, payload map[string]any) error` (record `{"_opengtm_lead_id": id, ...payload}` as sorted-key compact JSON with Python `default=str` semantics and Python float/unicode repr, newline terminated, folded into the SHA-256), `RowCount()`, `Manifest() map[string]any` (key set and values exactly as `manifest`: type `opengtm.audience.export`, version 1, `dataset` default `opengtm_audience`, `mode` default `snapshot`, `row_count`, `records_sha256`), `RolledToDisk()`, `Reader() io.Reader` (manifest line, then records, streamed), `Close()`; `func Sync(ctx, c Client, wsID string, dest DestRef, b *Buffer, get SecretGetter) (Result, error)` (secret `header_secret_ref` missing -> `warehouse authentication secret is missing`; `POST` with `Content-Type: application/x-ndjson`, `Idempotency-Key: warehouse:<dest>:<run>`, header `header_name` default `Authorization` carrying the secret, chunked streaming body, 30 s; non-2xx -> `warehouse HTTP <code>: <first 300 chars>` (body capped and control chars stripped); `external_id` from `x-job-id`, `location`, or JSON `job_id`/`id`; summary `exported N rows; sha256=<12 hex>`).

- [ ] **Step 1: Write failing tests**: `TestExportBytesMatchPythonGoldens` (small and spilled), `TestManifestMatchesVectors`, `TestSpillsAtBoundAndUnlinks`, `TestTempFileMode0600`, `TestCloseRemovesData`, `TestSyncRequestMatchesTranscript` (headers, idempotency key, chunked body equals golden), `TestSyncMissingSecret`, `TestSyncNon2xxTruncatedBody`, `TestExternalIDSources`, `TestBlockedHostRefused`, `TestHTTPSOnlyEnforcedAtSend`, `TestFloatAndUnicodeReprMatchPython`.
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test` -> PASS.
- [ ] **Step 5: Commit** `test(go): warehouse export goldens (RED)`, `feat(go): warehouse export buffer and sync`.

### Task 10: Inbound tokens and CRM reconcile

**Files:**
- Create: `internal/db/queries/destination_inbound.sql`, `internal/destinbound/{token,reconcile}.go`, `internal/destinbound/inbound_integration_test.go`

**Interfaces:**
- Produces: `GenerateToken() (raw, hash, prefix string)` (`dsi_` + 32 random bytes base64url; hash = hex SHA-256; prefix = first 12 chars), `TokenTTL(env int) time.Duration` (clamped 1-365 days), `Rotate(ctx, tx, wsID, destID string, createdBy int32, now time.Time) (RotateResult, error)` (revokes active tokens, inserts the new one), `Status(ctx, tx, wsID, destID string, now time.Time)` (`{"active": false}` or the full shape with `expired`), `Revoke`; `Resolve(ctx, pool *pgxpool.Pool, raw, destID string, now time.Time) (workspaceID string, err error)` (non-RLS lookup by hash with constant-time compare, bound to the destination, expiry and revocation, stamps `last_used_at`; every failure is `ErrInvalidToken` = `invalid or revoked destination token`); `Reconcile(ctx, tx, dest Destination, body InboundBody) (Receipt, replay bool, err error)` (= `reconcile`: event id trimmed to 255; prior receipt -> replay; lead resolution by explicit id, by prior delivery `external_id`, by unique email; reverse `field_map`; `ALLOWED_FIELDS` allow-list (the exact set from Python); `employee_count_exact` int coercion; policy `fill_missing|crm_wins`; statuses `applied|no_change|unmatched`; lead columns updated through a fixed per-column statement table, never a built SQL string; unique violation on the receipt insert -> replay of the winning row).

- [ ] **Step 1: Write failing tests**: `TestTokenFormatAndHash`, `TestTTLClamp`, `TestRotateRevokesPrevious`, `TestStatusShapes` (none, active, expired), `TestResolveRejectsUnknownExpiredRevokedForeignDestination` (identical error), `TestResolveConstantTimeAndStampsLastUsed`, `TestReconcileMatchesFixtures`, `TestReconcileReplayIsIdempotent`, `TestReconcileConcurrentSameEventOneReceipt` (16 goroutines), `TestReconcileOnlyAllowedColumnsWritten`, `TestFillMissingVsCRMWins`, `TestEmployeeCountCoercion`, `TestUnmatchedLeadReceipt`, `TestEmailMatchRequiresUnique`, `TestTenantIsolation`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): inbound tokens and reconcile (RED)`, `feat(go): destination inbound tokens`, `feat(go): CRM inbound reconcile`.

### Task 11: Per-type delivery

**Files:**
- Create: `internal/destdeliver/{deliver,webhook,slack}.go`, `internal/destdeliver/deliver_test.go`

**Interfaces:**
- Consumes: `Ports` (Task 3), `destvalidate.RenderSlackTemplate`, `deststore` map/fingerprint helpers, secrets getter.
- Produces: `MapPayload(snapshot, fieldMap map[string]any) map[string]any` (empty map -> copy of snapshot; else `{target: snapshot[source]}` for sources whose value is not `nil`/`""`); `Fingerprint(payload map[string]any) string` (SHA-256 of canonical JSON: sorted keys, compact separators, Python `ensure_ascii` and `default=str`); `IdempotencyKey(destID string, leadID int, fingerprint string, remove bool) string`; `func (d *Deliverer) Deliver(ctx, dest Destination, leadID int, snapshot map[string]any, idem string) Result` for `webhook` (body `{event: "audience.member.upsert", audience_id, destination_id, lead_id, data}`, `Idempotency-Key` header, via `Ports.Webhook`), `hubspot|salesforce` (`Ports.CRM` with `type` and `field_map` merged into the config), `instantly`/`smartlead` (summaries `Lead already in campaign` / `Lead added to campaign`, `skip_if_in_campaign` default true, smartlead `settings`), `google_sheets` (row from `columns`, key `dest:<id>:lead:<lead>`, range default `Sheet1!A:ZZ`, summary `Sheet row <operation>`, external id = range), `airtable` (`idempotency_field` default `OpenGTM ID`, `typecast` default true, summary `Airtable record <operation>`, external id = record id), `slack` (secret `webhook_secret_ref` else `Slack webhook secret not found`; template default `New audience member: {company} {contact_person} {email}`; text cut to 3000; metadata block as Python), anything else `unsupported destination '<type>'`; a recovered panic or error becomes `{success:false, error: str[:500]}`; each delivery bounded by `DEST_DELIVERY_TIMEOUT`.

- [ ] **Step 1: Write failing tests**: `TestMapPayloadMatchesVectors`, `TestFingerprintMatchesPythonVectors`, `TestIdempotencyKeys`, `TestDeliverWebhookBodyAndHeaders`, `TestDeliverCRMConfigMerge`, `TestDeliverCampaignAddersSummaries`, `TestDeliverSheetsAirtableKeysAndDefaults`, `TestDeliverSlackTemplateAndTruncation`, `TestSlackSecretMissing`, `TestUnsupportedType`, `TestPortErrorBecomesFailedResultTruncated`, `TestPanicRecovered`, `TestDeliveryTimeoutHonoured`, `TestSecretsNeverInResults`.
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test` -> PASS.
- [ ] **Step 5: Commit** `test(go): destination delivery vectors (RED)`, `feat(go): per-type destination delivery`.

### Task 12: Audience refresh

**Files:**
- Create: `internal/db/queries/{audiences,audience_members,audience_events}.sql`, `internal/audiencerefresh/{refresh,events,observed}.go`, `internal/audiencerefresh/refresh_integration_test.go`

**Interfaces:**
- Consumes: Tasks 4, 5, 7 (`EnqueueAudienceSyncs`), `leadsread.QueryLeads`, `Emitter` (Task 3).
- Produces: `func Refresh(ctx, pool, wsID, audienceID string, deps Deps) (Result, error)` (= `refresh_audience`: tx 1 `SELECT ... FOR UPDATE` the audience, then page loop over `QueryLeads(filters, page, 500)`; existing members looked up by `lead_id IN (...)`; new -> member + `entered` event; existing -> compare snapshot by deep equality, update snapshot/`refresh_token`/`last_seen_at`; `matched` accumulates; loop ends when `matched >= total` or empty page; exits = members whose `refresh_token != refresh_id` in keyset pages of 500 -> `exited` event + delete; `member_count = matched`, `refreshed_at = now`; commit; then `Emitter.Emit` over the new events in keyset pages when `entered or exited`; then `EnqueueAudienceSyncs` when `entered or exited or changed`; result `{"audience","entered","exited","unchanged": matched-entered-changed,"changed"}`); `func RefreshObserved(...)` (= `refresh_audience_observed`: on error roll back, set `degraded`, `last_refresh_error = "<Type>: <msg>"` cut to 1000, increment failures, commit, re-raise; on success `healthy`, clear error, zero failures); the whole refresh is bounded by a context timeout (route 120 s, job 900 s) and releases the row lock on cancellation. Snapshots are the lead rows' JSON exactly as `query_leads_page` returns them (vectors compare member snapshots).

- [ ] **Step 1: Write failing tests**: `TestRefreshMatchesPythonVectors` (enter/exit/change counts and snapshots over multi-page corpora, page size forced to 3), `TestRefreshConcurrentSerialized` (two refreshes, second waits, final state equals one sequential run), `TestExitsOnlyForUnseenMembers`, `TestUnchangedSnapshotNotCountedChanged`, `TestEventsPersistedWithRefreshID`, `TestEventsEmittedAfterCommitInPages`, `TestEmitterFailureDoesNotUndoDiff`, `TestEnqueueSyncsOnlyOnChange`, `TestHealthyAndDegradedTransitions`, `TestFailureRollsBackDiff`, `TestContextCancelReleasesLock`, `TestEmptyFiltersMatchAllLeads`, `TestTenantIsolation`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): audience refresh (RED)`, `feat(go): audience materialization with exact diff and events`, `feat(go): observed refresh and health`.

### Task 13: Destination sync engine

**Files:**
- Create: `internal/destsync/{sync,ads,warehouse,cancel,failure}.go`, `internal/destsync/sync_test.go`, `internal/destsync/sync_integration_test.go`

**Interfaces:**
- Consumes: Tasks 7-11, `queue.Claim`.
- Produces: `func (e *Engine) Handle(ctx, c *queue.Claim) error` (= `handle_destination_sync`, payload `{workspace_id, run_id}` else error `destination sync requires workspace_id and run_id`): terminal run -> return; `cancelling` -> `FinishCancelled`; destination missing or disabled -> run `cancelled` `destination missing or disabled`; run `running`, `started_at` kept; members streamed by `id` keyset pages of 500 (each page its own short transaction); per member: re-read run status (cancel check), mapped payload (ads: hashed identifiers only), fingerprint, idem; prior `success` -> skipped; reuse or create the delivery (failed prior re-homed to this run); ads without supported identifiers -> `skipped` `No valid email or phone identifier`; batched types (`meta_ads|google_ads|linkedin_ads|warehouse_http`) stay `pending` and join a batch (ads flush at 5000; warehouse appends to the buffer), others set `in_flight`, `attempts+1` and call `Deliverer.Deliver` outside any open transaction, then settle (`success|failed`, `summary`, `error[:1000]`, `external_id`, `delivered_at`); ad batches: cancel check, mark `in_flight` + attempts, one `SyncBatch`, settle every row; after the add pass, ad exit reconciliation (latest successful `upsert` delivery per lead with no membership row, `remove` idem over the stored hashed payload, skip when a `remove` already succeeded, batches of 5000); warehouse flush (cancel check, `pending` -> `in_flight` with attempts, `Sync`, settle all `in_flight` rows to one terminal status, counters by affected rows); final cancel check keeps results and ends `cancelled`; else run counters, status `completed|completed_with_errors`, `finished_at`, destination `health_status` `healthy|degraded`, `last_error` `N deliveries failed`, `last_success_at` when anything succeeded or the audience was empty with no failures. `func (e *Engine) OnFailure(ctx, pool, c *queue.Claim, errText string, willRetry bool) error` (= `reconcile_destination_job_failure`, via `deststore.ReconcileJobFailure`). `func Eligible(destType string, ports destdeliver.Ports, goTypes map[string]bool) bool` used by the worker to `queue.HandBack` unsupported types.

- [ ] **Step 1: Write failing tests**: `TestSyncMatchesPythonTranscripts` (every flow of Task 1), `TestIdempotentRedeliverySkipsSuccess`, `TestFailedPriorRetriedAndRehomed`, `TestNoTransactionHeldDuringDeliver` (blocking deliverer; `pg_stat_activity` shows no open tx), `TestAdBatchSettlesAllRowsTogether`, `TestAdBatchSizeBoundary` (5000/5001), `TestAdUnsupportedIdentifiersSkipped`, `TestAdRemovalReconciliationHashesOnly`, `TestAdRemovalIdemSkipsAfterSuccess`, `TestWarehouseFlushSettlesInFlightRows`, `TestWarehouseNoRowsNoRequest`, `TestCancelBeforeMemberAndBeforeBatch`, `TestCancelDuringFinalRequestKeepsResultsAndCancels`, `TestCancelledNeverOverwrittenByCompleted`, `TestDisabledDestinationCancelsRun`, `TestEmptyAudienceMarksSuccess`, `TestHealthAndLastSuccess`, `TestQueueRetryResetsInFlight`, `TestTerminalFailureFailsPending`, `TestEligibleFollowsPortsAndTypes`, `TestLeaseLossStopsBeforeNextDelivery`, `TestTenantIsolation`.
- [ ] **Step 2: Run** `make test test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): destination sync engine (RED)`, `feat(go): per-member delivery loop with cancellation`, `feat(go): ad batch sync and exit reconciliation`, `feat(go): warehouse flush and queue failure reconcile`.

### Task 14: Worker wiring

**Files:**
- Modify: `cmd/worker/main.go`, `cmd/worker/main_test.go`, `cmd/worker/main_integration_test.go`

**Interfaces:** Consumes Tasks 5, 12, 13. Produces handler registration for `audience_refresh` (timeout 900 s; `RefreshObserved`, then `ScheduleNext`; on error `ScheduleNext` before returning the error, exactly like Python; a missing or refresh-disabled audience removes the schedule and returns) and `audience_destination_sync` (timeout 1800 s; `queue.HandBack` with reason when `Eligible` is false) on `executor='go'` rows only; queue failure hooks: `ReconcileFailure` for `audience_refresh` and `Engine.OnFailure` for `audience_destination_sync` run in the worker after each failed attempt (the Python parent-worker callback has no Go equivalent otherwise); `AudienceEvents` and `DestPorts` constructed from config and plan 07/08 constructors when present (absent -> handlers for the dependent types are not registered and the startup log says why).

- [ ] **Step 1: Write failing tests**: `TestWorkerRunsAudienceRefreshAndReschedules`, `TestWorkerRefreshErrorStillSchedulesNext`, `TestWorkerRunsDestinationSync`, `TestWorkerIgnoresPythonRows`, `TestWorkerHandsBackIneligibleType`, `TestWorkerFailureHooksReconcile`, `TestWorkerTimeoutsPerType`, `TestWorkerNoEmitterNoRefreshHandler`.
- [ ] **Step 2: Run** `make test test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): worker audience and destination handlers (RED)`, `feat(go): worker runs audience refresh and destination sync`.

### Task 15: Audience CRUD routes

**Files:**
- Modify: stub `internal/api/audiencescrud.go`; Create: tests + `testdata/contract/audiences/crud_*.json`

**Interfaces:** Produces `GET /api/audiences` (array, `updated_at DESC` with `id` as tiebreak, D8), `POST /api/audiences` (editor; body `AudienceCreate`; 201 with `to_api()` shape and key order `id, name, description, filters, member_count, created_at, updated_at, refreshed_at, refresh_enabled, refresh_interval_minutes, next_refresh_at, refresh_health, last_refresh_error, consecutive_refresh_failures`; duplicate name 409 `An audience with this name already exists`; after insert: `Refresh` then `ScheduleNext` in the same request, response reflects the refreshed row), `PATCH /api/audiences/{id}` (only given fields; `filters` re-materializes; `refresh_enabled`/`refresh_interval_minutes` reschedule; 409 duplicate; 404 `Audience not found`), `DELETE /api/audiences/{id}` (204; remove schedule first; cascades members/events/destinations). FastAPI 422 shapes via `audiencevalid`/`destvalidate` decoders; name trimmed, blank 422, `refresh_interval_minutes` 15-10080.

- [ ] **Step 1: Write failing tests**: `TestAudienceCRUDMatchesFixtures`, `TestCreateMaterializesAndSchedules`, `TestCreateDuplicateName409`, `TestPatchFiltersRematerializes`, `TestPatchIntervalReschedules`, `TestPatchOnlyGivenFields`, `TestDeleteCascadesAndUnschedules`, `TestValidation422Shapes`, `TestEditorRequiredViewerReadsOnly`, `TestForeignWorkspace404`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): audience CRUD routes (RED)`, `feat(go): audience CRUD routes`.

### Task 16: Audience refresh, members, events routes

**Files:**
- Modify: stub `internal/api/audiencesread.go`; Create: tests + `testdata/contract/audiences/read_*.json`

**Interfaces:** Produces `POST /api/audiences/{id}/refresh` (editor; returns the `Result` shape; 404; request context timeout 120 s -> 504 `audience refresh timed out`, row lock released, D8), `GET /{id}/members?limit=200&before_id&offset=0` (`limit` 1-1000, `before_id >= 1`, `offset` 0-100000; `id DESC`; items `{id, lead_id, snapshot, joined_at, last_seen_at}`), `GET /{id}/events?limit=100&before_id` (`limit` 1-500; `id DESC`; items `{id, audience_id, lead_id, event_type, snapshot, created_at}`); both 404 `Audience not found` first.

- [ ] **Step 1: Write failing tests**: `TestRefreshRouteMatchesFixtures`, `TestRefreshTimeoutReleasesLock`, `TestMembersPaginationAndOrder`, `TestMembersBounds422`, `TestEventsPaginationAndOrder`, `TestEventsBounds422`, `TestReadsViewerAllowedRefreshEditorOnly`, `TestForeignWorkspace404`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): audience read routes (RED)`, `feat(go): audience refresh, members and events routes`.

### Task 17: Audience accounts route

**Files:**
- Modify: stub `internal/api/audiencesaccounts.go`; Create: tests + `testdata/contract/audiences/accounts_*.json`

**Interfaces:** Produces `GET /api/audiences/{id}/accounts?limit=50&offset=0` (`limit` 1-200, `offset` 0-100000, `current_workspace`; 404 `Audience not found`; members streamed by `id ASC` in keyset pages of 1000, signal count/weight joined per lead, rolled up with `audiencevalid.Rollup`; body `{"accounts","summary":{"account_count","contact_count","accounts_with_signals","accounts_with_decision_makers"},"pagination":{"limit","offset","next_offset","total"}}` with Python key order; an audience over `AUDIENCE_ACCOUNTS_MAX_MEMBERS` members answers 422 `audience too large for account rollup` instead of exhausting memory, D3).

- [ ] **Step 1: Write failing tests**: `TestAccountsMatchesFixtures`, `TestAccountsSortAndPagination`, `TestAccountsBounds422`, `TestAccountsEmptyAudience`, `TestAccountsSignalAggregation`, `TestAccountsBadWebsiteNoCrash`, `TestAccountsTooLarge422`, `TestAccountsTenantIsolation`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): audience accounts route (RED)`, `feat(go): audience accounts rollup route`.

### Task 18: Destination CRUD routes

**Files:**
- Modify: stub `internal/api/destscrud.go`; Create: tests + `testdata/contract/destinations/crud_*.json`

**Interfaces:** Produces `GET /api/audience-destinations?audience_id=` (array of `to_api()` plus `latest_run`, `updated_at DESC, id DESC`), `POST` (editor, 201; checks in Python order: unknown audience 404 `Audience not found`, config validation 422 with the message as `detail` string, secret ref present for `warehouse_http`/`slack` else 422 `<type> <field> was not found`, duplicate 409 `A destination with this name already exists for the audience`), `PATCH /{id}` (editor; `name`, `enabled`, `config` (re-validated for the stored type, secret refs re-checked), `field_map`; rename duplicate answers 409 with the same detail instead of Python's 500, D3; 404 `Destination not found`), `DELETE /{id}` (204, revokes tokens). Credentials never echoed: `config` returned as stored (Python parity, no secrets inside by validation).

- [ ] **Step 1: Write failing tests**: `TestDestinationCRUDMatchesFixtures`, `TestCreateCheckOrder`, `TestCreateEveryTypeValidation` (vectors), `TestSecretRefMissing422`, `TestCreateDuplicate409`, `TestPatchRenameDuplicate409`, `TestPatchConfigRevalidated`, `TestPatchOnlyGivenFields`, `TestListLatestRunShape`, `TestDeleteCascades`, `TestEditorRequiredViewerReadsOnly`, `TestForeignAudienceAndDestination404`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): destination CRUD routes (RED)`, `feat(go): destination CRUD routes`.

### Task 19: Sync, runs, health, deliveries, retry, cancel routes

**Files:**
- Modify: stub `internal/api/destsruns.go`; Create: tests + `testdata/contract/destinations/runs_*.json`

**Interfaces:** Produces `POST /{id}/sync` (editor, 202; 404; 409 `Destination is disabled`; existing active run returned as-is; else `StartRun` with `requested_by = str(user.id)` and executor from `GO_DEST_TYPES`), `GET /{id}/runs?limit=50&offset&cursor` (shape `{runs, limit, offset (null with cursor), has_more, next_cursor}`; `cursor` max 1024), `GET /{id}/health` (shape of `Health`), `GET /runs/{run_id}/deliveries?status&after_id&limit=100` (404 `Destination run not found`; 422 `Invalid delivery status`, `limit must be between 1 and 500`), `POST /runs/{run_id}/retry` (editor, 202; 409 `Only failed or completed-with-errors runs can be retried`, 409 `Destination is disabled`; run reset to `pending`, `error` and `finished_at` cleared, job enqueued with the same `fire_key`; audit metadata `{action: "audience_destination.run.retry", destination_id, run_id, previous_failed}`), `POST /runs/{run_id}/cancel` (editor, 202; 409 `Only pending or running destination runs can be cancelled`; pending -> `FinishCancelled`, else `cancelling` + `Cancellation requested`; audit metadata with `previous_status`). Static paths (`runs/...`, `inbound/...`) are registered before `/{destination_id}` patterns.

- [ ] **Step 1: Write failing tests**: `TestSyncCreatesRunAndJob`, `TestSyncReturnsExistingActiveRun`, `TestSyncConcurrentSingleRun`, `TestSyncDisabled409`, `TestRunsCursorAndOffsetMatchFixtures`, `TestRunsBadCursor422`, `TestHealthMatchesFixtures`, `TestDeliveriesFiltersAndBounds`, `TestRetryTransitionsAndAudit`, `TestRetryWrongStatus409`, `TestCancelPendingFinishesCancelled`, `TestCancelRunningRequestsCancellation`, `TestCancelTerminal409`, `TestRoutePrecedenceStaticBeforeParam`, `TestRolesAndTenantIsolation`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): destination run routes (RED)`, `feat(go): sync, runs, health and deliveries routes`, `feat(go): run retry and cancel routes`.

### Task 20: Inbound token and receipts routes

**Files:**
- Modify: stub `internal/api/destsinboundtokens.go`; Create: tests + `testdata/contract/destinations/tokens_*.json`

**Interfaces:** Produces `POST /{id}/inbound-token` (admin `secrets.manage`; CRM destinations only else 409 `Inbound sync tokens are only available for CRM destinations`; revokes active tokens, returns `{token, prefix, created_at, expires_at, warning: "Copy this token now; it will not be shown again."}`; the plaintext is never persisted or logged and the audit metadata excludes it), `GET /{id}/inbound-token` (admin; `{"active": false}` or `{active, expired, prefix, created_at, expires_at, last_used_at, created_by}`), `DELETE /{id}/inbound-token` (admin, 204), `GET /{id}/inbound-receipts?limit=50&offset&cursor` (`current_workspace`; shape `{receipts, limit, offset, has_more, next_cursor}`).

- [ ] **Step 1: Write failing tests**: `TestRotateReturnsPlaintextOnce`, `TestRotateNonCRM409`, `TestRotateRevokesPrevious`, `TestStatusShapes`, `TestRevoke204AndCallbackRejected`, `TestReceiptsCursorMatchesFixtures`, `TestAdminRequired`, `TestTokenNeverLoggedOrAudited`, `TestForeignDestination404`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): inbound token routes (RED)`, `feat(go): inbound token and receipt routes`.

### Task 21: Public inbound callback

**Files:**
- Modify: stub `internal/api/destsinbound.go`; Create: tests + `testdata/contract/destinations/inbound_*.json`

**Interfaces:** Produces `POST /api/audience-destinations/inbound/{destination_id}` (public: global auth middleware exempt exactly like Python; bearer parsed case-insensitively; `Resolve` failure -> 401 with the error text and `WWW-Authenticate: Bearer`; body `InboundEvent` (422 FastAPI shapes); then `InWorkspace(workspace)`: destination must exist, be `hubspot|salesforce` and enabled else 404 `Active CRM destination not found`; `Reconcile` ValueError -> 422; response `{...receipt, "replay": bool}` with the receipt `to_api()` key order; hardening D5: rate limit 120 requests per minute per client IP and per token prefix via `ratelimit` (429 body as plan 02), body size capped at 1 MiB (413), the real client IP from the trusted proxy header).

- [ ] **Step 1: Write failing tests**: `TestInboundMatchesFixtures` (valid, replay, unmatched, no_change), `TestInboundAuthFailuresIdentical401WithChallenge`, `TestInboundForeignDestinationToken401`, `TestInboundDisabledOrNonCRM404`, `TestInboundBody422Shapes`, `TestInboundRateLimited`, `TestInboundBodyCap413`, `TestInboundBearerCaseInsensitive`, `TestInboundConcurrentSameEventOneReceipt`, `TestInboundWorkspaceFromTokenNotHeader` (forged workspace header ignored).
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): public inbound callback (RED)`, `feat(go): public CRM inbound callback`.

### Task 22: Contract coverage, nginx split, runbook

**Files:**
- Modify: `infra/nginx/go-routes.conf` (one delimited `# BEGIN audiences-destinations` / `# END audiences-destinations` section), `scripts/nginx-routing-check.sh` (cases only), `testdata/contract/README.md` (deviations D3-D8)
- Create: `testdata/contract/retained_audience_routes.txt`, `docs/go-port/runbooks/audiences-destinations-cutover.md`, `internal/api/audience_coverage_test.go`

**Interfaces:** Consumes everything. Produces per-route nginx blocks (retained paths first, always `api_backend`: `GET /api/audiences/capabilities`, `GET /api/audience-destinations/types`; ported paths follow the canary bucket; `POST /api/audiences` and `POST /{id}/refresh` and `PATCH /{id}` are sticky per workspace during canary so a refresh, its events and its sync enqueue never split across stacks; the public `/inbound/{id}` forwards `X-Forwarded-For`); runbook: prerequisites (plan 08 `AudienceEvents` wired or the audience routes stay off, plan 07/08 ports for each type listed in `GO_DEST_TYPES`), flag order (`GO_AUDIENCE_ROUTES_ENABLED` and `GO_DEST_ROUTES_ENABLED` first, then `GO_EXECUTOR_AUDIENCE_REFRESH`, then `GO_DEST_TYPES` one type at a time: `warehouse_http`, ads, `slack`/`webhook`, then the plan 07/08 types), why the Python scheduler stays, drain procedure (set `GO_DEST_TYPES` and the refresh flag off, wait for `executor='go'` rows to leave `pending/processing`; runs `running` at the moment of rollback are completed by `OnFailure` reconciliation), cancellation semantics, token rotation notes (existing tokens keep working on both stacks), ad credential checks.

- [ ] **Step 1: Write failing tests** `TestEveryAudienceDestinationRouteIsPortedOrRetained` (parses `routers/{audiences,destinations}.py`; every route is in the Go patterns or in `retained_audience_routes.txt`; expected 23 ported + 2 retained), `TestRetainedRoutesNotRegisteredInGo`, `TestEveryJobTypeHandledOrPython` (`audience_refresh` and `audience_destination_sync` dispatched by Go, no others), `TestEveryDestinationTypeHasDriverOrPort` (the 12 types), `TestNoSecretsInFixturesOrLogs` (greps fixtures, transcripts and a captured log for seeded ad tokens, secret ref values, inbound tokens and warehouse auth values).
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3:** record missing fixtures, write the nginx blocks, runbook and deviations. **Step 4: Run** `make vet test test-integration` and `scripts/nginx-routing-check.sh` at 0% and 100% (retained paths always `py`; mutation and refresh routes sticky; others follow the bucket) -> PASS.
- [ ] **Step 5: Commit** `test(go): audience and destination route coverage (RED)`, `feat(infra): nginx per-route split for audiences and destinations`, `docs(go-port): audiences and destinations cutover runbook and deviations`.

---

## Self-Review

- **Spec coverage:** inventory row "audiences, audience_destination_sync, ads, warehouse": audiences (4, 5, 12, 15-17), destination sync (7, 11, 13), ads (8, 13), warehouse (9, 13), destinations routes (6, 18-21), inbound (10, 20, 21), job wiring and failure reconcilers (5, 7, 13, 14), executor routing (2, 3). All 9 routes of `audiences.py` (8 ported + capabilities retained) and 16 of `destinations.py` (15 ported + types retained) are covered.
- **Consistency:** `deststore.StartRun`/`EnqueueAudienceSyncs` (7) used by 12 and 19; `destvalidate` decoders and cursors (6) used by 15, 18-21; `destdeliver.Deliverer` (11) and `Ports` (3) used by 13, 14; `adsdrv.SyncBatch` (8) and `warehouse.Sync` (9) used by 13; `audiencesched.ScheduleNext` (5) used by 12, 14, 15; `audiencevalid.Rollup` (4) used by 17; `destinbound` (10) used by 20, 21.
- **Review Focus:** each line has its tests in Tasks 5, 12, 14 (1); 7, 13, 14 (2); 7, 13, 19 (3); 8, 11 (4); 10, 21 (5); 4, 6, 7, 11, 18, 19 (6).
- **Proportion:** interfaces, error strings, ordering rules, test names and file ownership carry the decisions; SQL and handler glue are left to the implementer and the recorded vectors and transcripts.

## Open decisions (defaults apply unless overridden)

- **D1 Certification catalogs.** `GET /api/audiences/capabilities` and `GET /api/audience-destinations/types` return signed-certification maturity (`audience_capability_catalog`, `integration_catalog`), shared with the integrations and governance routes. Default: retained on Python in `retained_audience_routes.txt`; they move with whichever plan ports `certification` (plan 07 or later).
- **D2 Delivery types and executor routing.** `webhook`/`slack` reuse plan 08's webhook action, `hubspot`/`salesforce` its CRM push, the four campaign/sheet types plan 07's clients. Default: ads and `warehouse_http` are fully Go here; every other type is Go-eligible only when its port is wired and its name is in `GO_DEST_TYPES`; otherwise its runs stay on Python (and a configured-but-unwired type is a startup error, not a silent fallback). Rollout one type at a time.
- **D3 Crash and validation hardening.** Duplicate destination rename 500 -> 409; unparseable website in the accounts rollup falls back to the company key; accounts rollup bounded (`AUDIENCE_ACCOUNTS_MAX_MEMBERS`, 422 above); ad platform ids and `api_version` validated before URL interpolation (422 at create/patch, delivery error for legacy stored configs). Documented deviations.
- **D4 Ad SSRF/path injection.** Hosts are fixed constants; ids are validated; no redirects; responses bounded. Default as stated (deviation from Python's unvalidated interpolation).
- **D5 Emitter dependency and public endpoint.** Audience write routes and the refresh handler need plan 08's membership-event emitter; without it they are not registered (Go never persists events without emitting them). The unauthenticated inbound callback gains a per-IP/per-token rate limit and a 1 MiB body cap. Documented deviation.
- **D6 Slack template.** Python uses `str.format_map`, which allows attribute and index access on string values. Default (confirm): Go supports `{name}` and brace escapes only; other syntax is rejected at create/patch with 422 and fails the delivery for legacy stored templates. Documented deviation.
- **D7 Run start atomicity.** Python commits the run before enqueueing (a failed enqueue strands a `pending` run that blocks every later sync) and two concurrent `sync` calls can create two active runs. Default: single-flight under an advisory lock and run insert + enqueue in one transaction. Documented deviation.
- **D8 Request-bound refresh and ordering.** `POST /refresh` and `POST /api/audiences` refresh inline like Python, bounded by a 120 s context (504 on timeout, lock released); audience and destination lists get an `id` tiebreak after `updated_at DESC`, which Python leaves undefined. Documented deviations.
- **D9 Scheduler.** The Python scheduler remains the only ticker for `bootstrap_audience_schedules`; it enqueues `audience_refresh` with `executor` per flag. Go ports `ScheduleNext`/`ReconcileFailure`/`DueSchedules` for routes, handlers and a later Go scheduler. Default: no Go scheduler in this plan.
- **D10 Rollout.** Default: all new Go paths off (`GO_AUDIENCE_ROUTES_ENABLED`, `GO_DEST_ROUTES_ENABLED`, `GO_EXECUTOR_AUDIENCE_REFRESH`, empty `GO_DEST_TYPES`); canary by workspace cohort with sticky mutation/refresh routing.

## User decisions

(none yet; the defaults above apply until the user objects)

## User decisions (2026-10-26) — defaults applied under the standing rules

- D1: the two certification catalog routes stay on Python.
- D2: ads and warehouse are Go here; other destination types become Go-eligible only when their plan 07/08 port is wired and listed in `GO_DEST_TYPES`.
- D3: duplicate rename is 409, bad websites never crash the rollup, rollup is bounded, ad ids are validated (documented deviations).
- D4: fixed ad hosts, validated path segments, no redirects, bounded responses.
- D5: audience writes and refresh require the plan 08 event emitter; the public inbound callback is rate limited and size capped.
- D6: Slack templates support `{name}` and brace escapes only (documented deviation).
- D7: sync start is single-flight and atomic with its enqueue (documented deviation).
- D8: inline refresh is bounded by a 120 s timeout; lists get a deterministic tiebreak.
- D9: Python scheduler remains the only ticker.
- D10: all new Go paths default off; per-type rollout; sticky routing for mutations and refresh.
