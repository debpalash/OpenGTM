# Automations, Triggers and Watches Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Port the automation engine and the intent-signal poller to Go: the 11 `/api/automations` routes, the 7 `/api/watches` routes, the `trigger_eval` and `watch_poll` job handlers (hardened condition evaluator, spend/action caps, idempotency ledger, webhook/CRM/re-enrich actions, six watch source kinds, budget, backoff, failure reconciliation), the schedule bootstrap functions, and the full set of event emitters (`on_rows_added`, `on_rows_changed`, `on_signal`, audience enter/exit) so that the Go API, the Go worker (plans 05/06) and Python all enqueue and execute the same jobs, with only JobSpy/DDG work crossing the `pyworker` boundary.

**Architecture:** Three layers. (1) Pure, vector-tested logic: `safecond` (fail-closed condition grammar), `autocaps` (atomic reservations), `autoschedule`/`poller` core (next-run alignment, keys, budget, backoff). (2) Effects behind small interfaces: `autoactions` (SSRF-pinned webhook via `internal/egress`, `crmpush` from plan 07, single-row re-enrich through plan 06 `providerrun`), `poller` sources (HTTP in Go; hiring/tech, job-change and account-group collectors through `pyworker` `ProviderCall`). (3) Handlers and routes: `autoengine.HandleTriggerEval`, `pollerengine.HandleWatchPoll`, wired into `cmd/worker`, plus stub-per-route-group API files. The Python scheduler process remains the single scheduler (D3); the Go bootstrap functions are libraries that enqueue with executor routing flags, so either implementation produces byte-identical `jobs` rows and `fire_key`s. A job the Go worker cannot run faithfully (legacy outreach actions, re-enrich of Python-only column types) is handed back to Python before any side effect (`queue.HandBack`).

**Tech Stack:** Go 1.27.3 stdlib, pgx v5 + sqlc, go-redis v9 (existing), `encoding/xml` for RSS; existing `internal/{httpx,identity,tenancy,audit,contract,queue,egress,secrets,billing,signals,signalscan,leadsread,wbcrud,workbook,pyworker,providerrun,vendorcatalog,colgraph,automationhook,crmpush,rowtmpl,providers}`; no new Go modules. Python/uv for recorder, seed, vectors, transcripts.

**Spec:** `docs/go-port/inventory.md` section 8 row 7 "Automations" (sections 2 HTTP surface, 3 job types, 5 data stores, 6 background processes, 9 risks); `docs/go-port/plans/04-workbook-crud.md` Task 5 (`automationhook.RowsAdded`, enqueue half), `05-leads-signals.md` (`signals`), `06-enrichment-jobs.md` (`pyworker`, `providerrun`, `billing.CheckAndDebit`, `signalscan`, `queue.EnqueueOnce`, `queue.HandBack`, SEC/techstack providers), `07-connectors-crm.md` (`crmpush`, `rowtmpl`, `queue.RegisterFailureHook`). Python sources: `apps/api/routers/{automations,watches}.py`, `services/automations/{engine,actions,caps,events,safe_conditions,models}.py`, `services/poller/{engine,sources,rss,job_change,account_group,keys,models}.py`, `services/job_scheduling.py`, `services/queue_service.py`, `scheduler.py`, `core/url_guard.py`, `core/config.py` (AUTOMATIONS_*/INTENT_POLLER_*/POLLER_*).

**Layout note (branch `go-rewrite`):** Python lives untracked at `legacy/python/`; Python paths (`apps/api/...`, `scripts/...`) mean `legacy/python/<path>`. Go paths are relative to `apps/backend-go/`. Module `github.com/debpalash/OpenGTM/apps/backend-go`.

**Prerequisite:** plans 01-07 merged (Waves 0-2 of plan 06 at minimum for `pyworker`/`providerrun`/`billing`/`signalscan`; plan 06 Wave 4 for the worker; plan 07 Tasks 3, 7, 10, 11 for failure hooks, `rowtmpl`, `crmpush`). No new tables: `triggers`, `trigger_runs`, `trigger_action_results`, `trigger_cap_reservations`, `scheduled_triggers`, `watch_subscriptions`, `poll_budget_ledger`, `watch_schedules`, `workbook_activity` are in `internal/db/schema.sql`. No Alembic task and no `RequiredGoSchemaRevision` bump. Cross-plan note: plan 06 Task 17 calls an `on_signal` emitter; this plan's Task 8 defines it as `automationhook.Hook.SignalsInserted` (if plan 06 shipped a placeholder, Task 8 replaces it without changing the call site).

## Global Constraints

- Go `go 1.27` + `toolchain go1.27.3`; pgx v5 + sqlc only; `make sqlc-check` clean. Go never runs DDL. No new modules.
- Tenant tables only inside `tenancy.InWorkspace` (API and worker). The two NON-RLS mirrors `scheduled_triggers` and `watch_schedules` (ids and timestamps only) are read by the bootstrap functions without a workspace and are the ONLY cross-tenant reads; every follow-up read re-enters `tenancy.InWorkspace`. Cross-workspace or unknown ids answer 404 (`trigger not found`, `run not found`, `watch not found`), never 403.
- Contract parity is the release bar: paths, methods, JSON key order, status codes, `detail` strings, 422 bodies, auth gates. Gates verbatim: automations mutations = `identity.RequireRole("automations.manage","admin")`, automations reads = `identity.Workspace`; watches mutations = `identity.RequireRole("signals.write","editor","admin")`, reads = `identity.Workspace`. Every automations route first answers 404 `automations disabled` when `AUTOMATIONS_ENABLED` is off; every watches route 404 `intent poller disabled` when `INTENT_POLLER_ENABLED` is off, then 409 `intent_poller_requires_pg_lead_store` when `PG_LEAD_STORE` is off. Dependency-before-body precedence (403 before 422, flag 404 after validation) is taken from recorded fixtures, not reasoned.
- Behaviour parity of worker output is the release bar: `trigger_runs`, `trigger_action_results` (idempotency key `trig:<ws>:<trigger>:<wb>:<row>:<idx>:<fire_key>`), `trigger_cap_reservations`, charged USD (4-decimal), `watch_subscriptions` cursor/`last_error`/`consecutive_failures`/`next_poll_at`, `poll_budget_ledger`, `scheduled_triggers`/`watch_schedules` mirrors, `jobs` rows and `fire_key`s, signal ids (`signal_event_id`), workbook activity messages. Vectors for every algorithm come from the Python modules; HTTP behaviour from Python-recorded transcripts, never hand-written, never the internet.
- Hardening deviates from Python only as a documented deviation (D4-D7) in `testdata/contract/README.md`: SSRF-pinned egress for webhooks, RSS and pricing fetches via `internal/egress` (dial-time IP check, no redirects, 1 MiB bound), cap reservation serialised by an advisory lock (the Python `FOR UPDATE` over zero rows does not serialise the first reservation), debit + reservation + `in_flight` marker in ONE transaction, bounded rule/target/template sizes, crash bugs answered as 4xx (Review Focus 5). Secrets never appear in payloads, logs, fixtures, run rows or error strings.
- Non-idempotent actions (`webhook`, `send_email`) are at-most-once: after the `in_flight` marker commits, a crash or retry never re-sends (`skipped`/`replay`). Idempotent actions (`push_crm`, `re_enrich`) may re-attempt on the same marker.
- `AUTOMATIONS_ENABLED`, `INTENT_POLLER_*`, `POLLER_*_COST_USD`, `AUTOMATIONS_*` limits keep their Python names and defaults in `internal/config`; the Go API and worker MUST agree with Python (a startup check logs and refuses Go automation routes if `AUTOMATIONS_ALLOW_LEGACY_OUTREACH` is true, D2).
- Redis in tests: `TEST_REDIS_URL` (127.0.0.1:56380); integration tests skip when unset. Verify with `cd apps/backend-go && make sqlc-check vet test` and `make test-integration`. No CI changes.
- Every commit DCO signed (`git commit -s`), no `Co-Authored-By` trailer. GRANULAR commits: tests first (RED, message ends `(RED)`), then each implementation piece. Stage explicit paths only; `make sqlc` regenerates all of `internal/db/gen/`, so each task stages only its own `<name>.sql.go` and `.sql`.
- Retained on FastAPI after this plan (`testdata/contract/retained_automation_routes.txt`, Task 21): nothing at route level. Retained at job level: legacy outreach actions (`sequencer`, `send_email`; hand-back, D2), re-enrich of Python-only column types (hand-back, D8), hiring/tech, job-change and account-group collectors (through `pyworker`, D1).

## Review Focus

1. **Double fire, double charge, double send.** A `trigger_eval` can be delivered twice (retry, lease loss, Go then Python), and the debit, the cap reservation and the external effect are three separate things. Per action: replay check on the idempotency key; reservation, debit and the committed `in_flight` marker in one transaction; the effect only after the marker commits; terminal update and settle/release after. `webhook` after an `in_flight` marker is `skipped/replay`; the same `run_id` debit charges once (plan 06 `CheckAndDebit`); two concurrent evals of the same row and fire key insert one result row (`uq_trigger_action_idem`); `cancelled` jobs start no further action. Tasks 5, 10, 14.
2. **Caps are real.** Concurrent evals near a per-rule action cap, per-rule spend cap or the workspace global daily cap must never both pass; reservations use the UTC day bucket; a released reservation is never reused as held; the same key twice reserves once. Cap refusal is `skipped/cap` with summary `daily cap reached`, credit refusal `skipped/credits` with `insufficient credits`. Tasks 5, 14.
3. **Condition evaluator stays fail-closed.** Operator structure is parsed before values are bound, so a cell value containing `"`, ` AND `, ` OR ` or `{x}` can never change the parse tree; any parse or operand failure is `False` with a structured reason (never the legacy default-True); `older_than` timestamp parsing, `contains`, `not_empty`/`is_empty` and number-vs-string comparison equal Python on 200 vectors; an unbounded expression is rejected. Task 4.
4. **SSRF and secret tenancy in every outbound call.** Webhook URLs are templated from row data: after templating, loopback, link-local, private, IPv4-mapped IPv6, DNS rebinding at dial time, redirects and `AUTOMATIONS_WEBHOOK_DOMAIN_ALLOWLIST` misses must be refused with `skip_reason="blocked_url"`; `header_secret_ref` resolves only inside the rule's workspace and only injects when found; RSS and pricing fetches use the same pinned client. Tasks 6, 11, 12.
5. **Crash bugs answered as client errors.** Python 500s on non-numeric row ids in `preview`/`run` (`WorkbookRow.id == "abc"` against an integer column), a non-dict element in `actions`, `max_spend_usd_per_day` negative or NaN, `trigger_config` of the wrong JSON type, `watch` cursors with a bad base64 payload (already 422), `lead_id` of another workspace pinned to a watch (routes signals to a foreign lead), and `INTENT_POLLER_*` budget arithmetic on a missing ledger row. Each gets a defined 4xx (D4). Tasks 13, 17-20.
6. **Scheduling is single-flight across languages.** Python and Go bootstrap/reschedule the same trigger or watch in the same minute: exactly one active `jobs` row per `fire_key` (`uq_jobs_fire_key_active` is the arbiter; the advisory-lock key text must equal each Python site: `fire_key` for trigger schedules, `watch_poll:<fire_key>` for watch polls, so a Go and a Python enqueuer serialise on the same lock); mirror rows are written in the same transaction as the job; a disabled or deleted rule/watch is never rescheduled; auto-disable after `INTENT_POLLER_MAX_CONSECUTIVE_FAILURES` cancels the pending retry. Tasks 7, 9, 15.
7. **Signal emission is exactly-once.** The poller emits through deterministic `signal_event_id`; the cursor advances in the same transaction as the emitted signals for one source (a crash after emit and before the cursor commit re-emits as a no-op); `on_signal` fires only for rows actually inserted; a source failure never advances its cursor; billing refusal is a source failure, not a healthy poll. Tasks 8, 15.
8. **Hand-back safety.** The Go worker decides executor eligibility before the run row exists: legacy outreach actions or non-Go re-enrich columns hand the whole job to Python; a Python-executed run and a Go-executed run never overlap for one `fire_key`; `pyworker` unreachable for a re-enrich action marks that action `failed` (never blocks the run, never double-charges because the reservation is released). Tasks 14, 16.

## File Structure

```
internal/safecond/{tokenize,eval,validate}.go               # safe_conditions
internal/autocaps/caps.go                                   # try_reserve / settle / release / actions_today
internal/autoschedule/{next,mirror,enqueue,bootstrap}.go    # compute_next_run, scheduled_triggers mirror, schedule enqueue, bootstrap_schedules
internal/autoactions/{webhook,pushcrm,reenrich,cost,dispatch}.go   # actions.py
internal/autosvc/{validate,service,models}.go               # rule validation, CRUD, run history, to_api shapes
internal/autoengine/{candidates,run,results,finalize}.go    # trigger_eval handler
internal/automationhook/{changed,signals,audience,exec}.go  # (adds to plan 04 package; hook.go touched once, Task 8)
internal/poller/{keys,schedule,budget,failure}.go           # keys, intervals, backoff, budget ledger, reconcile
internal/poller/{rss,sources_http}.go                       # feed + SEC funding/exec + web_tech
internal/poller/{sources_py,jobchange,accountgroup}.go      # pyworker-backed collectors and normalisation
internal/poller/engine.go                                   # handle_watch_poll, bootstrap_watch_schedules, enqueue-if-absent
internal/watchsvc/{validate,service,models}.go              # watch CRUD, cursor, to_api
cmd/worker/main.go                                          # (modify, Task 16)
internal/config/config.go                                   # (modify, Task 3)
internal/api/{automationscrud,automationsrun,watchescrud,watchespoll}.go
internal/api/register.go                                    # (modify once, Task 3)
internal/db/queries/{autotriggers,autoruns,autocaps,autoschedule,watches,pollbudget,workbook_activity_auto}.sql (+ gen/)
testdata/contract/{automations,watches}/*.json
testdata/contract/retained_automation_routes.txt
testdata/transcripts/{webhook,rss,sec_funding,webtech,pricing}/*.json
../../legacy/python/scripts/{contract_seed.py,contract_cases/automations.yaml,gen_automation_vectors.py,record_automation_transcripts.py}
../../legacy/python/apps/api/{services/go_executor.py,services/automations/{events,engine}.py,services/poller/engine.py,routers/{automations,watches}.py,routers/internal_provider.py}   # (modify, Task 2)
../../infra/nginx/go-routes.conf
../../docs/go-port/runbooks/automations-cutover.md
```

## Parallel waves

Tasks in a wave touch disjoint files and may run concurrently (up to 6 agents at a time). Each wave starts only after the previous wave is merged and `make vet test test-integration` is green.

| Wave | Tasks | Shared-file notes |
|---|---|---|
| 0 (sequential) | 1 Python seed, cases, vectors, transcripts; 2 Python executor routing and `ddgs_search` RPC provider; 3 Go scaffold, config, register lines, stubs | T1 owns `contract_seed.py` (append-only), the three Python scripts and all fixture/vector/transcript files; T2 owns `go_executor.py`, the four Python enqueue sites, `routers/internal_provider.py` registry; T3 owns `register.go`, `config.go`, stub route files, empty `.sql` headers |
| 1 | 4 safecond, 5 autocaps, 6 webhook action, 7 autoschedule, 8 hook emitters, 9 poller core | 6 disjoint packages; 5 owns `autocaps.sql`, 7 owns `autoschedule.sql`, 9 owns `pollbudget.sql`; 8 adds files to `internal/automationhook/` and edits `hook.go` once |
| 2 | 10 push_crm, re_enrich and dispatch, 11 poller HTTP sources, 12 poller pyworker collectors, 13 automations and watches services | 10 adds files to `internal/autoactions/` (T6 owns `webhook.go`); 11 and 12 own distinct files of `internal/poller/`; 13 owns `internal/autosvc` and `internal/watchsvc` and their `.sql` |
| 3 | 14 trigger_eval handler, 15 watch_poll handler | 14 owns `internal/autoengine` and `autoruns.sql`; 15 owns `internal/poller/engine.go` and `watches.sql` |
| 4 (sequential) | 16 worker wiring, hand-back rules, bootstrap exports | edits `cmd/worker/main.go` only |
| 5 | 17 automations CRUD routes, 18 automations preview/run/runs routes, 19 watches CRUD routes, 20 watches poll and signals routes | each owns exactly one stub `internal/api/*.go` and one fixture dir |
| 6 (sequential) | 21 coverage, nginx split, runbook | consumes everything |

---

### Task 1: Python seed, recorder cases, vectors, transcripts

**Files:**
- Modify: `legacy/python/scripts/contract_seed.py` (append-only)
- Create: `legacy/python/scripts/contract_cases/automations.yaml`, `legacy/python/scripts/gen_automation_vectors.py`, `legacy/python/scripts/record_automation_transcripts.py`
- Create (output, committed): `testdata/contract/{automations,watches}/*.json`, `internal/{safecond,autocaps,autoschedule,autoactions,autoengine,automationhook,poller,autosvc,watchsvc}/testdata/*.json`, `testdata/transcripts/{webhook,rss,sec_funding,webtech,pricing}/*.json`

**Interfaces:**
- Consumes: plan 04/06/07 seed (workspaces A/B, admin/editor/viewer, workbooks with rows and leads, credit balance 10.0 for the billing profile).
- Produces: seed additions: triggers of every type (`on_row_added`, `on_row_changed` with `watch_fields`, `on_signal`, `on_schedule` hourly/daily/weekly, `on_audience_enter/exit`) with `re_enrich`, `push_crm`, `webhook` actions, scoped and unscoped, with per-day caps; `trigger_runs` (every status) and `trigger_action_results` (every status, `skip_reason`) for history routes; `trigger_cap_reservations` mixed states; `scheduled_triggers` and `watch_schedules` mirror rows; `watch_subscriptions` of every kind (`funding`, `hiring`, `feed`, `company`, `job_change`, `account_group`) incl. disabled, failing and auto-disabled; `poll_budget_ledger`; signals for `watches/{id}/signals`; an HTTP-recording webhook sink and RSS/SEC/pricing recorded transports. Vector files: `safecond` (200 conditions: grammar, quoting, injection strings, numbers vs strings, `older_than` durations and timestamp formats, `contains`, `not_empty/is_empty`, unknown field, empty condition, literal booleans, parse errors with their reasons), `autocaps` (reserve/settle/release sequences across the three cap axes, UTC day rollover, idempotent re-reserve, released-then-retry), `autoschedule` (`compute_next_run` 60 cases incl. `now <= anchor`, naive anchor, boundary), `autoactions` (project_action_cost over waterfalls, webhook header/body resolution, status mapping), `autoengine` (run status derivation across stats, stop_on_error, dry run, replay), `automationhook` (fire keys, sha8, scope), `poller` (`normalize_target`, `stable_company_id`, `role_tier`, `is_executive_title`, `signal_event_id`, `_source_set` per kind and signal-type subset, `compute_next_run`, `_reschedule` backoff table, `_interval_delta`, budget arithmetic), `rss` (RSS 2.0/Atom/malformed, epoch parsing, ETag/Last-Modified, max entries), `job_change` (`normalize_company`, `linkedin_handle`, `contact_key`, query building, title/company extraction), `account_group` (fingerprint, partnership terms), `watch_cursor` (encode/decode round trips and bad inputs), `validate_rule`/`validate_watch` (every 409/422 string). Transcripts: webhook sender (2xx, 4xx with body, 5xx, redirect, timeout), RSS fetch (200, 304, redirect, oversize), SEC funding/exec, web_tech homepage, pricing page.

- [ ] **Step 1: Extend the seed** (fixed ids and timestamps; `AUTOMATIONS_ENABLED=true`, `INTENT_POLLER_ENABLED=true`, `PG_LEAD_STORE=true` for the recording profile; a second profile with each flag off for the 404/409 cases).
- [ ] **Step 2: Write the cases** (each `{name, method, path, as, profile, body, expect_status}`): create per trigger type (valid, invalid type 422, schedule interval 422, too many actions, `re_enrich` missing/unknown column ids, `webhook` missing url / scheme / credentials in url / templated url / missing `header_secret_ref`, legacy outreach 409 `legacy_outreach_disabled`, invalid condition 422 `invalid condition: ...`, max rules 422, viewer/member 403, name length 422), list (filters `enabled`, `trigger_type`), get with `recent_runs`, patch (each field, re-validation, schedule mirror), delete, pause, resume (`next_run_at`), preview (no ids, with ids, foreign ids, non-numeric ids, `limit` clamp 1..100), run (manual, `dry_run`, ids, non-numeric ids), runs (`limit` bounds 422 over 200), run detail (404, results ordered), disabled-flag 404 for every route; watches create per kind (every 422 string: invalid kind, `account_group` refusal, job_change interval/contacts/linkedin/max_contacts rules, signal_types per kind, interval, max watches, webhook rule creation 409/422), list (limit/offset/cursor, invalid cursor 422, `has_more`, `next_cursor`), get, patch (enable/disable/interval/config/signal_types/lead_id), delete, poll-now (disabled 409, rate limit 429, daily quota 429, already_queued, queued), signals (filters, cursor, invalid cursor 422, account_group lead/company expansion).
- [ ] **Step 3: Write the vector and transcript scripts**; they import the real Python modules.
- [ ] **Step 4: Run** `uv run python scripts/contract_record.py --cases scripts/contract_cases/automations.yaml`, `uv run python scripts/gen_automation_vectors.py`, `uv run python scripts/record_automation_transcripts.py`; Expected: files written, re-running is byte-identical, no secret value in any output (the script greps and fails).
- [ ] **Step 5: Commit** (Go/doc side only) `test(go): automation and watch contract fixtures, vectors and transcripts (RED)`.

### Task 2: Python executor routing and `ddgs_search` provider

**Files:**
- Modify: `legacy/python/apps/api/services/go_executor.py`, `legacy/python/apps/api/core/config.py`, `legacy/python/apps/api/services/automations/{events,engine}.py` (enqueue sites only), `legacy/python/apps/api/services/poller/engine.py` (`_enqueue_poll_if_absent`), `legacy/python/apps/api/routers/automations.py` (`run_trigger` enqueue), `legacy/python/apps/api/routers/internal_provider.py` (registry entry)
- Test: `legacy/python/tests/test_executor_routing_automations.py`, `legacy/python/tests/test_internal_provider_ddgs.py`

**Interfaces:**
- Produces: settings `GO_EXECUTOR_TRIGGER_EVAL: bool = False`, `GO_EXECUTOR_WATCH_POLL: bool = False`; `go_executor.trigger_eval_executor(workspace_id, payload) -> "go"|"python"` (flag + cohort only; legacy-outreach and non-Go re-enrich rules are decided by the Go worker hand-back, Tasks 14 and 16) and `watch_poll_executor(workspace_id)`; every `trigger_eval` and `watch_poll` enqueue site passes `executor=`. `POST /internal/provider-call` gains provider `ddgs_search` (input `{query, max_results}` -> output `{results:[{title, snippet, url}]}`, wraps the existing `get_ddgs` client) and confirms `jobspy_signals` is callable (input `{company, ...}` = `fetch_hiring_and_tech`/`_partnership_jobs` inputs); both listed in `internal/providers/python_providers.txt` (plan 06 file; this task only appends the two names in a Go-side line owned by Task 12).

- [ ] **Step 1: Write failing tests**: `test_trigger_eval_executor_follows_flag_and_cohort`, `test_watch_poll_executor_follows_flag`, `test_default_stays_python`, `test_row_added_and_signal_and_schedule_sites_pass_executor`, `test_poll_now_enqueue_passes_executor`, `test_provider_call_ddgs_search_timeout_kills`, `test_provider_call_jobspy_signals_shape`.
- [ ] **Step 2: Run** `uv run pytest tests/test_executor_routing_automations.py tests/test_internal_provider_ddgs.py` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** (unversioned; note in the PR).

### Task 3: Go scaffold: config, register lines, stubs

**Files:**
- Modify: `internal/api/register.go` (+`RegisterAutomations`, `RegisterWatches`), `internal/config/config.go`
- Create: stub files `internal/api/{automationscrud,automationsrun,watchescrud,watchespoll}.go` (`func registerX(r *httpx.Router, d Deps)`, empty body), empty headers `internal/db/queries/{autotriggers,autoruns,autocaps,autoschedule,watches,pollbudget,workbook_activity_auto}.sql`
- Test: `internal/api/automationsscaffold_test.go`, `internal/config/automations_test.go`

**Interfaces:**
- Produces: config `GO_AUTOMATION_ROUTES_ENABLED` (default false), `GO_EXECUTOR_TRIGGER_EVAL`, `GO_EXECUTOR_WATCH_POLL` (default false), plus the Python-named settings with Python defaults: `AUTOMATIONS_GLOBAL_DAILY_USD 0`, `AUTOMATIONS_MAX_RULES_PER_WS 50`, `AUTOMATIONS_MAX_ACTIONS_PER_RULE 10`, `AUTOMATIONS_MAX_ROWS_PER_EVAL 500`, `AUTOMATIONS_WEBHOOK_DOMAIN_ALLOWLIST`, `INTENT_POLLER_ENABLED`, `INTENT_POLLER_DEFAULT_INTERVAL daily`, `..._MAX_WATCHES_PER_WS 200`, `..._DAILY_POLL_BUDGET 0`, `..._POLL_NOW_DAILY_QUOTA 50`, `..._POLL_NOW_MIN_INTERVAL_SEC 60`, `..._MAX_CONSECUTIVE_FAILURES 12`, `..._JOBSPY_MAX_JOBS 5`, `..._FEED_MAX_ENTRIES 100`, `..._JOB_CHANGE_MAX_CONTACTS_PER_POLL 50`, `..._JOB_CHANGE_MAX_CONTACTS 500`, `..._MAX_FIRES_PER_SIGNAL 200`, `..._BACKFILL false`, `POLLER_{FUNDING,HIRING,FEED}_COST_USD 0`, `TECH_STACK_WEBSITE_FETCH_ENABLED`. `RegisterAutomations`/`RegisterWatches` register nothing and log when `GO_AUTOMATION_ROUTES_ENABLED` is false, when `PG_LEAD_STORE` is false (watches), or (automations) when `AUTOMATIONS_ALLOW_LEGACY_OUTREACH` is true (D2).

- [ ] **Step 1: Write failing tests**: `TestAutomationRoutesNotRegisteredWithoutFlags`, `TestAutomationRoutesRefusedWithLegacyOutreach`, `TestAutomationRoutePatterns` (exactly the 11 and 7 routes of Tasks 17-20), `TestConfigDefaultsMatchPython` (each name and default from `core/config.py`), `TestConfigRejectsBadInterval`.
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test` -> PASS.
- [ ] **Step 5: Commit** `test(go): automation scaffold (RED)`, `feat(go): automation and watch route scaffold and config`.

### Task 4: Safe condition evaluator

**Files:**
- Create: `internal/safecond/{tokenize,eval,validate}.go`, tests, `internal/safecond/testdata/conditions.json` (Task 1)

**Interfaces:**
- Produces: `type Result struct{ Passed bool; Reason string }`; `func Evaluate(condition string, cells map[string]any, cols []Column) Result` = `safe_evaluate_condition` (tokenize operator structure first; `{field}` placeholders and quoted literals never produce keywords; values bound afterwards via `_build_values`/`_resolve_operand`; `==,!=,>=,<=,>,<`, `contains`, `older_than <n><s|m|h|d|w>`, `not_empty(x)`, `is_empty(x)`, literal booleans, `AND`/`OR` precedence as Python; fail closed with the same reason strings); `func Validate(condition string) error` = `validate_condition` (parse against an adversarial populated row; returns `*ConditionError` whose text is spliced into `invalid condition: <e>`). Bounds: condition <= 4 KiB, <= 64 tokens, value length <= 64 KiB per operand (over -> fail closed; Python is unbounded, D4). Comparison number-vs-string and timestamp parsing (`_parse_timestamp` formats, epoch seconds/ms) reproduced exactly; float parsing uses Python `float()` acceptance (`1e3`, `inf`, `nan`, underscores) pinned by vector.

- [ ] **Step 1: Write failing tests**: `TestConditionVectors` (200), `TestInjectionValuesNeverChangeParseTree`, `TestFailClosedOnParseError`, `TestEmptyConditionPasses`, `TestOlderThanDurations`, `TestOlderThanTimestampFormats`, `TestContainsCaseRules`, `TestNumericVsStringCompare`, `TestPythonFloatAcceptance`, `TestValidateReasonsMatchVector`, `TestTokenBudgetRejected`, `TestKeywordInsidePlaceholderIsNotKeyword`, `FuzzEvaluateNeverPanics`.
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make vet test` -> PASS.
- [ ] **Step 5: Commit** `test(go): safe condition evaluator (RED)`, `feat(go): condition tokenizer and evaluator`, `feat(go): condition validation`.

### Task 5: Cap reservations

**Files:**
- Create: `internal/db/queries/autocaps.sql`, `internal/autocaps/caps.go`, `internal/autocaps/caps_integration_test.go`

**Interfaces:**
- Produces: `type Reservation struct{ OK bool; Reason string; ID int64; IdempotencyKey string }`; `func DayUTC(now time.Time) string`; `func TryReserve(ctx, tx pgx.Tx, t TriggerCaps, ws string, cost float64, idem string, globalCap float64) (Reservation, error)` = `caps.try_reserve` (existing key -> reuse, a `released` one -> `OK=false, Reason="cap"`; per-rule action cap counts held+settled actions of the day; per-rule spend cap and the workspace global cap sum held+settled `reserved_usd`; `cost = round(max(0,cost), 6)`; compares `round(used+cost, 6)`; inserts `held`); serialisation by `pg_advisory_xact_lock(hashtext('autocap:'||ws||':'||day))` taken first (D5: Python's `FOR UPDATE` over an empty set does not serialise the first reserver); `Settle(ctx, tx, r)`, `Release(ctx, tx, r)` (state transitions only from `held`; idempotent), `ActionsToday(ctx, tx, triggerID, ws, day)`. Runs in the caller's transaction.

- [ ] **Step 1: Write failing tests**: `TestCapsMatchVectors`, `TestConcurrentReserversNearCapAdmitExactlyCap` (32 goroutines, cap 5 actions and cap $1.00), `TestGlobalCapAcrossRules`, `TestIdempotentReserveReturnsSame`, `TestReleasedKeyIsRefused`, `TestSettleReleaseAreOneWay`, `TestDayRolloverUTC`, `TestCostRounding`, `TestTenantIsolation`, `TestRollsBackWithCallerTx`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): automation caps (RED)`, `feat(go): cap reservation with advisory serialisation`.

### Task 6: Webhook action

**Files:**
- Create: `internal/autoactions/webhook.go`, `internal/autoactions/webhook_test.go`

**Interfaces:**
- Consumes: `internal/egress` (pinned client, `CheckURLFor(appEnv, allowHosts, raw, allowHTTP)`), `rowtmpl` (plan 07), `secrets.Resolver`.
- Produces: `type Result struct{ Status, SkipReason, Summary, Error string }` (`success|failed|skipped`; JSON-free); `func Webhook(ctx, d WebhookDeps, ws string, cfg map[string]any, leadData map[string]any, cols []Column) Result` = `_act_webhook` + `_send_webhook_pinned` + `_resolve_webhook_headers`: empty url -> `failed`, `webhook url not configured`; templated URL via `rowtmpl.Resolve`; guard failure -> `failed`, `blocked url: <reason>`, `skip_reason="blocked_url"` (reason strings equal Python: `scheme 'x' not allowed`, `credentials in url not allowed`, `no host`, `host 'x' is blocked`, `host 'x' not in webhook domain allowlist`, `ip 'x' is private/blocked`, `host resolves to private ip x`, `cannot resolve host: ...`); method default `POST`; body per `_act_webhook` (nil -> the whole `lead_data` as JSON, dict/list -> `ResolveDeep`, string -> resolved then JSON if parseable else raw with `Content-Type: text/plain`); headers resolved, `header_secret_ref` injects `header_name` (default `Authorization`) via `setdefault` semantics only when the secret exists; 15 s timeout, no redirects, response body bounded 1 MiB; success `<METHOD> <status>`, non-2xx -> `failed` with `HTTP <status>: <body[:160]>`; other errors `str(e)[:200]` with any URL query string and header value redacted. The connection goes ONLY to the validated IP (dial-time pin) with `Host` and SNI preserved.

- [ ] **Step 1: Write failing tests**: `TestWebhookTranscripts` (against Task 1 recordings), `TestBlockedURLReasonsMatchPython`, `TestTemplatedURLResolvingToLoopbackBlocked`, `TestIPv4MappedIPv6Blocked`, `TestDNSRebindingBlockedAtDial` (resolver flips after check), `TestRedirectNotFollowed`, `TestAllowlistEnforced`, `TestHeaderSecretOnlyFromOwnWorkspace`, `TestHeaderSecretNotLoggedOrInErrors`, `TestBodyShapes` (nil/dict/list/JSON string/plain string), `TestNon2xxCarriesBodyPrefix160`, `TestOversizedResponseBounded`, `TestContextCancelAborts`.
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): webhook action (RED)`, `feat(go): pinned webhook action`.

### Task 7: Schedule math, mirrors and enqueue

**Files:**
- Create: `internal/db/queries/autoschedule.sql`, `internal/autoschedule/{next,mirror,enqueue,bootstrap}.go`, `internal/autoschedule/schedule_integration_test.go`

**Interfaces:**
- Consumes: plan 06 `queue.EnqueueOnce`; Task 3 config.
- Produces: `func IntervalDelta(interval string) time.Duration` (unknown -> daily); `func NextRun(anchor time.Time, interval string, now time.Time) time.Time` = `compute_next_run` (`now <= anchor` -> anchor; else `anchor + (floor(elapsed/step)+1)*step`); `func MirrorSet/MirrorDelete(ctx, tx, triggerID, ws string, next *time.Time, enabled bool)` for `scheduled_triggers`; `func EnqueueScheduleIfAbsent(ctx, tx, t Trigger, next time.Time, executor string) (bool, error)` = `_enqueue_schedule_if_absent` (fire key `sched:<id>:<next.isoformat()>` with Python's isoformat text including `+00:00`; payload `{trigger_id, workspace_id, fire_source:"schedule", fire_key}` exactly as `enqueue_job_once`; advisory lock key text = the fire key; sets `triggers.next_run_at` and the mirror in the same transaction); `func BootstrapForTrigger(ctx, tx, t Trigger, now time.Time, executor string) (time.Time, error)`; `func BootstrapSchedules(ctx, pool, now time.Time, execFor func(ws string) string) (int, error)` = `bootstrap_schedules` (reads the non-RLS mirror without a workspace, then each due trigger inside `tenancy.InWorkspace`; skips missing, disabled or non-`on_schedule`). Flag off -> 0.

- [ ] **Step 1: Write failing tests**: `TestNextRunVectors` (60), `TestFireKeyTextEqualsPython`, `TestEnqueueScheduleSingleFlightAcrossGoAndPython` (pre-insert the Python-shaped row; 16 goroutines; one active row), `TestMirrorWrittenInSameTx`, `TestBootstrapSkipsDisabledAndDeleted`, `TestBootstrapEntersWorkspaceScope`, `TestBootstrapIsIdempotent`, `TestExecutorColumnFollowsPolicy`, `TestPayloadShapeEqualsPython`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): automation schedule (RED)`, `feat(go): next-run math and schedule mirror`, `feat(go): schedule enqueue and bootstrap`.

### Task 8: Automation event emitters

**Files:**
- Modify: `internal/automationhook/hook.go` (extend `Hook`, add `executor` option; no change to `RowsAdded` behaviour)
- Create: `internal/automationhook/{changed,signals,audience,exec}.go`, tests

**Interfaces:**
- Produces: `Hook` gains `RowsChanged(ctx, tx, ws, workbookID string, changes []Change) (int, error)` (= `on_rows_changed`: one `trigger_eval` per (rule, changed row) for enabled `on_row_changed` rules covering the workbook whose `watch_fields` is empty or contains the field; `fire_key = rowchg:<row_id>:<field>:<sha8(old)>-><sha8(new)>:<cell_version>`, `sha8` = first 8 hex of sha256 of Python `str()`/JSON as `_sha8`; payload `fire_source:"row_changed"`), `SignalsInserted(ctx, tx, ws string, sigs []SignalRef) (int, error)` (= `emit_signal_matches`: rules whose `signal_types` intersect, signal lead mapped to workbook rows inside rule scope, no targets -> no job, `fire_key = signal:<pk>`, `fire_source:"signal"`; the caller invokes it only for rows actually inserted), `AudienceEvents(ctx, tx, ws string, evs []AudienceEvent) (int, error)` (= `emit_audience_membership`, `fire_source: audience_entered|audience_exited`, `fire_key audience:<id>:<type>`); `type Change struct{ RowID int64; Field string; Old, New any; CellVersion string }`; `type SignalRef struct{ PK string; SignalType string; LeadID *int64 }`. `func NewWithExecutor(enabled bool, exec func(ws string) string, log *slog.Logger) Hook` (`New` keeps its signature and uses `python`); the `executor` column of the inserted `jobs` row follows `exec` (`GO_EXECUTOR_TRIGGER_EVAL` + cohort). All methods: disabled -> 0; errors logged and swallowed inside a savepoint so a failed emit never aborts the caller's transaction (plan 04 `runInSavepoint` pattern); a per-signal fan-out cap `INTENT_POLLER_MAX_FIRES_PER_SIGNAL` bounds targets (D4).
- Wiring (same task, one-line call sites only): the plan 04 insertion sites already call `RowsAdded`; this task adds the `RowsChanged` flush at the end of `internal/workbook` run (plan 06 `run.go`, a single call after the run commits, as `enrichment.py:860-902`) and the `SignalsInserted` call inside plan 06 `signalscan` insert. Both are edits of at most 6 lines each, listed here so their owners know.

- [ ] **Step 1: Write failing tests**: `TestRowsChangedFireKeyVectors`, `TestWatchFieldsFilter`, `TestSha8OfPythonStr` (None, floats, bools, dict), `TestSignalMatchesMapLeadToScopedRows`, `TestSignalNoTargetsNoJob`, `TestSignalTypeIntersection`, `TestAudienceEnterExit`, `TestPayloadMatchesPythonJob` (rows recorded from Python for each), `TestExecutorColumnFollowsPolicy`, `TestDisabledIsNoop`, `TestErrorSwallowedInSavepoint` (caller tx stays usable), `TestFanOutCap`, `TestTenantScoped`, `TestRunEndFlushCallsRowsChanged`, `TestSignalscanInsertCallsSignalsInsertedOnlyForNewRows`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS (plan 04 and 06 hook tests unchanged).
- [ ] **Step 5: Commit** `test(go): automation emitters (RED)`, `feat(go): rows changed emitter`, `feat(go): signal and audience emitters`, `feat(go): hook executor policy and run/scan call sites`.

### Task 9: Poller core: keys, schedule, budget, failure reconcile

**Files:**
- Create: `internal/db/queries/pollbudget.sql`, `internal/poller/{keys,schedule,budget,failure}.go`, tests

**Interfaces:**
- Consumes: Task 7 `autoschedule.NextRun`/`IntervalDelta` (same math), plan 06 `queue.EnqueueOnce`, plan 07 `queue.FailureNotice`.
- Produces: `NormalizeTarget`, `StableCompanyID(resolvedCIK *string, target string) string`, `RoleTier`, `IsExecutiveTitle`, `NormalizeExecName`, `NormalizeTech`, `SignalEventID(ws, kind, stableID, signalType, naturalID string) string` (= `keys.py`, `KEY_SCHEMA_VERSION = 1`, byte-equal hashes); `SourceSet(kind string, signalTypes []string, webTech bool) []string` (= `_source_set`); `ScheduledFireKey(watchID string, next time.Time) string` (`watch:<id>:<isoformat>`); `EnqueuePollIfAbsent(ctx, tx, fireKey, ws, watchID string, next time.Time, pollNow bool, executor string) (bool, error)` (advisory lock key text `watch_poll:<fire_key>`, payload `{watch_id, workspace_id, fire_key}` plus `poll_now:true` set in the same insert for manual polls, priority 1, `max_retries 3`; unique-index conflict -> false); `MirrorSet/MirrorDelete` for `watch_schedules`; `DebitBudget(ctx, tx, ws string, pollNow bool, today time.Time) (n, nPollNow int, err error)` (`INSERT ... ON CONFLICT (workspace_id, day) DO UPDATE ... RETURNING`), `PollNowQuotaUsed`; `RescheduleWatch(ctx, tx, w Watch, failed bool, now time.Time, cfg Config, executor string)` = `_reschedule_watch` (failure: `consecutive_failures+1`; at `>= MAX_CONSECUTIVE_FAILURES` -> `enabled=false`, `last_error="auto_disabled"`, `next_poll_at=NULL`, mirror disabled, no enqueue; else `backoff = min(interval/4 * 2^(failures-1), 24h)` and `next = max(aligned, now+backoff)`; success resets failures); `Reconcile(ctx, d Deps, n queue.FailureNotice) error` = `reconcile_watch_poll_failure` (including cancelling the pending retry job when auto-disabling, and the `queue_failure: <err>[:255]` text), registered by Task 16 with `queue.RegisterFailureHook("watch_poll", ...)`.

- [ ] **Step 1: Write failing tests**: `TestKeysVectors`, `TestSignalEventIDEqualsPython` (50), `TestSourceSetPerKind`, `TestFireKeyAndPayloadEqualPython`, `TestEnqueuePollSingleFlightAcrossGoAndPython`, `TestPollNowFlagSetInSameInsert`, `TestDebitBudgetConcurrentIncrements` (32 goroutines, final n = 32), `TestPollNowCountsBothCounters`, `TestRescheduleBackoffTable`, `TestAutoDisableCancelsPendingRetry`, `TestRescheduleDisabledNeverEnqueues`, `TestReconcileRetryVsFinal`, `TestReconcileTenantScoped`, `TestMirrorWrittenInSameTx`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): poller core (RED)`, `feat(go): poller keys and source sets`, `feat(go): poll enqueue, budget and backoff`, `feat(go): watch poll failure reconcile`.

### Task 10: push_crm, re_enrich, cost projection and dispatcher

**Files:**
- Create: `internal/autoactions/{pushcrm,reenrich,cost,dispatch}.go`, tests

**Interfaces:**
- Consumes: Task 6 `Webhook`/`Result`; plan 07 `crmpush.Push`; plan 06 `providerrun`, `workbook` single-row run entry (`workbook.RunRows(ctx, d, ws, workbookID, columnIDs, rowIDs, providerTimeout) (RunSummary{Completed, Errors int; BudgetExceeded bool; Error string}, error)`, which this task adds as a thin exported wrapper over the existing run loop if plan 06 did not export one), `billing.ProjectedPlatformCost`, `vendorcatalog`, plan 07 `rowtmpl`.
- Produces: `func ProjectCost(action map[string]any, cols []Column, defaults map[string][]string) float64` (= `project_action_cost`: `send_email` and everything but `re_enrich` cost 0; `re_enrich` = `ProjectedPlatformCost(1, providersByColumn)` with `_providers_by_column`: waterfall, else `[provider]`, else `DEFAULT_WATERFALLS[target]`, unknown column ids contribute nothing); `IdempotentTypes = {push_crm, re_enrich}`, `NonIdempotentTypes = {webhook, send_email}`; `PushCRM(ctx, d, ws string, cfg, leadData, cols, leadID int64) Result` (= `_act_push_crm`: success summary is `value`; failure `error or "crm push failed"` with `skip_reason="not_connected"` when the error contains `not connected` case-insensitively); `ReEnrich(ctx, d, ws, workbookID, rowID string, cfg, cols) Result` (= `_act_re_enrich`: no `column_ids` -> `skipped/no_target` `no column_ids`; none present -> `skipped/no_target` `none of the configured columns exist in this workbook`; single-row run with `provider_timeout=10s`; summary `re_enrich: <n> cells completed, <m> errors` + ` [workbook budget cap hit]`; `success` iff no errors, error text `<m> cell errors`); `Execute(ctx, d, ws string, action map[string]any, workbookID, rowID string, leadID *int64, leadData map[string]any, cols []Column, idem string) Result` (dispatcher; unsupported type -> `failed`, `action type '<t>' not supported`, `skip_reason="not_connected"`; `sequencer`/`send_email` are not executable in Go and return that same failed result because the worker never reaches them: the hand-back guard is Task 14). An unreachable or erroring `pyworker` surfaces as `failed` with the error text (never a panic, never a retry loop).

- [ ] **Step 1: Write failing tests**: `TestProjectCostVectors`, `TestProvidersByColumnFallbacks`, `TestPushCRMResultsMatchTranscripts`, `TestPushCRMNotConnectedSkipReason`, `TestReEnrichNoColumnsSkipped`, `TestReEnrichNoneExistSkipped`, `TestReEnrichSummaryAndBudgetSuffix`, `TestReEnrichPyworkerDownIsFailedNotPanic`, `TestReEnrichUsesTenOutOfTenTimeout`, `TestDispatcherUnsupportedType`, `TestExecuteTenantIsolation`, `TestPushCRMNeverUsesOtherWorkspaceToken`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): automation actions (RED)`, `feat(go): action cost projection and push_crm`, `feat(go): single-row re_enrich action and dispatcher`.

### Task 11: Poller HTTP sources

**Files:**
- Create: `internal/poller/{rss,sources_http}.go`, tests

**Interfaces:**
- Consumes: `egress` pinned client, plan 06 `providers/firmo` (`secedgar`, `techstack`) via the `providers.Provider` interface, `internal/poller` keys (Task 9).
- Produces: `type DetectedEvent struct{ SignalType, NaturalEventID, Title, Description, Source, SourceURL string; Weight float64; OccurredAt float64; LeadID *int64; Company string }`; `ParseFeed(xml []byte, maxEntries int) ([]FeedEntry, error)` (RSS 2.0 and Atom, `_localname`, `_parse_epoch`); `FetchFeed(ctx, d, url string, etag, lastModified string) (FeedResult, error)` (pinned fetch, 304 handling, 1 MiB body bound, per-host minimum interval 1.0 s, UA `Yupcha-Poller (+https://yupcha.com)`); `FetchFundingAndExec(ctx, d, w WatchView, wantFunding, wantExec, backfill bool) (events []DetectedEvent, patch map[string]any, err error)` (= `sources.fetch_funding_and_exec`: CIK resolution side-channel `_resolved_cik`, `sec_last_accession` watermark, funding and exec detectors consuming one filing batch together); `FetchWebTech(ctx, d, w, website string, backfill bool)`; `FetchFeedSource(ctx, d, w, backfill bool)` (= `sources.fetch_feed`: `etag`/`last_modified`/seen-id cursor keys). A fetch failure returns `(nil, nil, err)` and never a partial cursor patch (the engine does not advance the cursor, Task 15). Event order, weights, natural ids and cursor key names equal the vectors.

- [ ] **Step 1: Write failing tests**: `TestParseFeedVectors` (RSS, Atom, malformed, huge), `TestFetchFeedTranscripts` (200, 304, redirect refused, oversize), `TestFeedBlockedHostsRefused` (loopback, metadata IP, rebinding at dial), `TestFeedMaxEntries`, `TestFeedCursorKeys`, `TestFundingExecTranscripts`, `TestFundingExecSharedWatermark`, `TestCIKResolvedSideChannel`, `TestWebTechTranscript`, `TestBackfillFlag`, `TestFetchFailureReturnsNoPatch`, `TestPerHostRateLimit`.
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make vet test` -> PASS.
- [ ] **Step 5: Commit** `test(go): poller http sources (RED)`, `feat(go): rss feed parse and fetch`, `feat(go): funding, exec and web tech sources`.

### Task 12: Poller pyworker collectors

**Files:**
- Create: `internal/poller/{sources_py,jobchange,accountgroup}.go`, tests; append `ddgs_search` and `jobspy_signals` to `internal/providers/python_providers.txt` (one line each)

**Interfaces:**
- Consumes: plan 06 `pyworker.Client.Call` (`ProviderCall{CallID, Provider, WorkspaceID, Input, Timeout}`), Task 11 `DetectedEvent`.
- Produces: `FetchHiringAndTech(ctx, d, w, wantHiring, wantTech, backfill bool)` (calls `jobspy_signals`; band ladder `none < hiring < growing < rapid_growth < hypergrowth` and `_iso_week` dedup in Go from the returned counts; `INTENT_POLLER_JOBSPY_MAX_JOBS` input), `MaterializeContacts(ctx, tx, w WatchView) ([]Contact, error)` (= `job_change.materialize_contacts`: roster plus lead rows read through the tenant), `FetchJobChanges(ctx, d, w, contacts []Contact, backfill bool)` (= `fetch_job_changes`: per-poll cap `max_contacts_per_poll`, query building, DDG via `ddgs_search`, `ResolveCurrentCompany` title/snippet extraction with the Python regexes and stopwords, `contact_key`, state in cursor key `job_change_contacts`, `JOB_CHANGE_WEIGHT = 9`), `FetchAccountGroup(ctx, d, w, backfill bool)` (= `fetch_account_group`: pricing page fingerprint over the pinned HTTP client, partnership jobs via `jobspy_signals`, SEC events via Task 11, `collector_health` and `_collector_failures` patch keys). Call ids are deterministic (`watch:<id>:<fire_key>:<source>:<n>`) so a retried poll dedupes at the RPC. A `pyworker` timeout or error for one collector is that collector's failure (`_collector_failures`), not an exception that aborts the others. Python regexes are compiled once; RE2 equivalence of each is pinned by vector (any construct RE2 lacks is rewritten and tested, never approximated).

- [ ] **Step 1: Write failing tests**: `TestHiringBandVectors`, `TestHiringTechTranscripts` (recorded `pyworker` replies), `TestJobChangeNormalizersVectors`, `TestCompanyExtractionVectors`, `TestJobChangeCapAndStateKey`, `TestAccountGroupFingerprintVectors`, `TestAccountGroupPartialCollectorFailure`, `TestPricingFetchBlockedHost`, `TestRPCCallIDsDeterministic`, `TestPyworkerDownIsSourceFailure`, `TestPyworkerTimeoutBounded`, `TestDDGSQueryNeverContainsSecrets`.
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make vet test` -> PASS.
- [ ] **Step 5: Commit** `test(go): poller pyworker collectors (RED)`, `feat(go): hiring and tech collector`, `feat(go): job change collector`, `feat(go): account group collector`.

### Task 13: Automation and watch services

**Files:**
- Create: `internal/db/queries/{autotriggers,watches}.sql` (fill), `internal/autosvc/{validate,service,models}.go`, `internal/watchsvc/{validate,service,models}.go`, `*_integration_test.go`

**Interfaces:**
- Consumes: Tasks 4, 7, 9; plan 04 `wbcrud` workbook column reads; `secrets.Resolver`; `queue.Enqueue`.
- Produces (`autosvc`): `type Trigger`/`Run`/`ActionResult` with `MarshalJSON` equal to `to_api` (key order, `isoformat` with `+00:00`, `round(...,4)`); `ValidateRule(ctx, tx, ws string, r RuleInput, cfg Config) error` returning `*HTTPError{Status int; Detail any}` with Python's strings in Python's order (trigger_type, schedule interval, `too many actions (max N)`, legacy 409, action type, `re_enrich action i: column_ids required` and `... not in scoped workbook(s): [..]` using Python's list repr, webhook rules, `header_secret_ref '<ref>' not found`, `invalid condition: <e>`); `Create/List/Get/Patch/Delete/Pause/Resume(ctx, ...)` (create enforces `AUTOMATIONS_MAX_RULES_PER_WS`, sets `schedule_anchor` and bootstraps the schedule for enabled `on_schedule`; patch re-validates with the merged rule and maintains the mirror; delete removes the mirror first; resume returns `next_run_at`); `PreviewTargets`, `EnqueueManualRun(ctx, tx, ws, triggerID string, rowIDs []string, dryRun bool, now time.Time) (jobID int64, fireKey string, err error)` (`manual:<trigger>:<now.isoformat()>`, payload key order `trigger_id, workspace_id, fire_source, fire_key, dry_run[, targets]`); `Runs(ctx, ws, triggerID string, limit int)`, `RunWithResults`. Row ids that are not integers are resolved as no match (D4), never a 500.
- Produces (`watchsvc`): `Watch` `MarshalJSON` equal to `_to_api` including the `account_group` extras; `ValidateCreate/ValidatePatch` with every Python string (`_validate_job_change_config`, `_KIND_SIGNAL_TYPES`, interval rules, `account_group watches require an exact persisted workbook selection`, max watches); `EncodeCursor/DecodeCursor` (urlsafe base64 without padding of compact JSON `[created_at.isoformat(), id]`; bad input -> 422 `invalid watch cursor`); `Create` (optional webhook rule: `watch webhook (<kind>:<target>)`[:200], the same validation as `_maybe_create_webhook_rule`, in its own transaction as Python); `List`, `Get`, `Patch` (enable resets failures and re-bootstraps; disable clears `next_poll_at` and the mirror), `Delete`, `PollNow` (`watch disabled` 409, per-watch min interval 429 `poll-now rate limited`, daily quota 429 `poll-now daily quota exhausted`, single-flight reuse of an active scheduled `watch:<id>:%` job returning `already_queued`, else manual key `watch:<id>:pollnow:<date>` with `poll_now:true`), `Signals` (page query through plan 05 `signals` with the account-group lead/company expansion; `lead_id` pinned on a watch must belong to the workspace, 422 otherwise, D4).

- [ ] **Step 1: Write failing tests**: `TestValidateRuleVectors` (every string), `TestTriggerJSONKeyOrder`, `TestCreateEnforcesMaxRules`, `TestCreateScheduleBootstrapsMirrorAndJob`, `TestPatchRevalidatesMergedRule`, `TestPatchDisableUpdatesMirror`, `TestDeleteRemovesMirror`, `TestResumeReturnsNextRun`, `TestManualRunPayloadEqualsPython`, `TestNonNumericRowIDsAreNoMatchNotCrash`, `TestTenantIsolationTriggersAndRuns`, `TestWatchValidateVectors`, `TestWatchJSONKeyOrderAccountGroupExtras`, `TestCursorRoundTripAndBadInputs`, `TestCreateWithWebhookRule`, `TestPatchEnableResetsFailuresAndBootstraps`, `TestPollNowRateAndQuotaAndSingleFlight`, `TestForeignLeadIDPinRejected`, `TestSignalsAccountGroupExpansion`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): automation and watch services (RED)`, `feat(go): automation rule validation and crud`, `feat(go): automation manual run and history`, `feat(go): watch validation, cursor and crud`, `feat(go): watch poll-now and signals`.

### Task 14: `trigger_eval` handler

**Files:**
- Create: `internal/db/queries/autoruns.sql`, `internal/autoengine/{candidates,run,results,finalize}.go`, `internal/autoengine/*_integration_test.go`

**Interfaces:**
- Consumes: Tasks 4, 5, 7, 10, plan 06 `billing.CheckAndDebit`, plan 04 `wbcrud` row/overlay readers, `queue.{Heartbeat,IsCancelled,HandBack}`.
- Produces: `func Handle(ctx, d Deps, c *queue.Claim) error` = `handle_trigger_eval`. Order: flag off -> return; payload without `workspace_id`/`trigger_id` -> log and return; load trigger inside `tenancy.InWorkspace`; **eligibility guard before any write**: a rule with a `sequencer`/`send_email` action, or a `re_enrich` action whose columns are not Go-eligible (plan 06 `runplan.EligibleColumns`) -> `queue.HandBack` the job and return (D2, D8); missing or paused rule -> a `skipped` run row, return; else the run row (`running` or `dry_run`) COMMITTED before any debit; `ResolveCandidates` (`AUTOMATIONS_MAX_ROWS_PER_EVAL` cap; explicit targets require the row to belong to the named workbook and workspace; sweep orders by `created_at, id` per scoped workbook); `RowCells` flatten (`data` then enrichment overlay, `{"value": v}` unwrapped, overlay wins); per matching row and action: the five-step protocol of Review Focus 1 using `idem = trig:<ws>:<trigger>:<wb>:<row>:<idx>:<fire_key>` (existing `success|skipped|charged_only|failed` -> counted as skipped; `in_flight` + non-idempotent -> `skipped/replay`; paid and not dry run -> `TryReserve`, refused -> `skipped/cap` `daily cap reached`; `CheckAndDebit(run_id=idem, reason="automation_<type>")`, `InsufficientCreditsError` -> release + `skipped/credits` `insufficient credits`; the `in_flight` marker with `charged_usd` is committed in the SAME transaction as reservation and debit (D6); `dry_run` persists and charges nothing; effect; settle on `success` else release; terminal update); `stop_on_error` semantics exactly as Python (break action loop, then row loop); a panic or error in one action is `failed` and logged, never aborts the run; run status derivation (`dry_run`, `partial`, `failed` only if `attempted`, `completed`) and `total_charged_usd = round(sum, 4)`; `last_fired_at` set when not dry run; workbook activity rows (`kind="automation"`, message format of `_log_activity`) best-effort; for `fire_source == "schedule"` and not dry run, `autoschedule` reschedule (single-flight) after commit. Heartbeat between actions; cancel or lease loss stops before the next action and leaves the run row `running` for the reaper (Python's behaviour); a cancelled job never overwrites the committed `cancelled`.

- [ ] **Step 1: Write failing tests**: `TestRunStatusDerivationVectors`, `TestEvalMatchesPythonRowsForFixtures` (same seed, same fire key, results rows equal), `TestReplayExistingResultIsSkipped`, `TestInFlightNonIdempotentIsReplaySkipped`, `TestInFlightIdempotentReattempts`, `TestWebhookSentAtMostOnceAcrossRetries` (crash injected after marker commit), `TestConcurrentSameKeyInsertsOneResult` (8 goroutines), `TestCapRefusalIsSkippedCap`, `TestCreditRefusalReleasesReservation`, `TestDebitChargedOncePerIdempotencyKey`, `TestDebitReservationAndMarkerAtomic` (kill between steps leaves nothing half), `TestDryRunWritesOnlyRunRow`, `TestStopOnErrorBreaksBothLoops`, `TestLegacyOutreachActionHandsBack`, `TestPythonOnlyColumnReEnrichHandsBack` (no run row written), `TestMissingRuleWritesSkippedRun`, `TestExplicitTargetsOtherWorkbookIgnored`, `TestSweepCapsAtMaxRows`, `TestCancelStopsBeforeNextAction`, `TestLeaseLossStopsAndPreservesState`, `TestScheduleReschedulesOnce`, `TestActivityMessageMatchesPython`, `TestTenantIsolation`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): trigger_eval handler (RED)`, `feat(go): candidate resolution and row cells`, `feat(go): per-action idempotent protocol with caps and debit`, `feat(go): run finalisation, activity and reschedule`.

### Task 15: `watch_poll` handler

**Files:**
- Create: `internal/poller/engine.go`, `internal/poller/engine_integration_test.go`

**Interfaces:**
- Consumes: Tasks 9, 11, 12; plan 06 `signalscan` insert (`InsertSignals`, exported by that plan; if not exported this task extracts it without behaviour change), Task 8 `SignalsInserted`, `billing.CheckAndDebit`, `leadsread`, `queue` helpers.
- Produces: `func Handle(ctx, d Deps, c *queue.Claim) error` = `handle_watch_poll`: flag off or no PG store -> log and return; missing `workspace_id`/`watch_id` -> log and return; transaction 1: load watch (missing/disabled -> delete mirror, return), `DebitBudget`, over `INTENT_POLLER_DAILY_POLL_BUDGET` -> reschedule (success path) and return, snapshot kinds/backfill/bootstrapped; resolve the lead (`ResolveLeadID`: pinned `lead_id` wins; `job_change`/`account_group` route per event; exact normalised-name match -> lowest id; more than one fuzzy match -> `last_error="ambiguous_lead"`, reschedule, return; none -> skip silently; lead rows read through the tenant only); per source in `SourceSet` order, ONE transaction each: billing gate (`_SOURCE_COST`, `check_and_debit(run_id="<fire_key>:<source>")`, refusal -> `last_error="insufficient_credits"` and source failure), fetch, side-channel patches (`_resolved_cik` only when unset, `_collector_failures` first entry `[:255]` into `last_error`), an event batch with no resolvable lead and a non-roster kind -> `last_error="unresolved_lead"` source failure without cursor advance, emit via `EmitEvents` (`signal_event_id`, `ON CONFLICT (id) DO NOTHING`, `SignalsInserted` for inserted rows only), cursor patch merged in the SAME transaction; a fetch failure advances nothing; finalize transaction: `bootstrapped=true`, `attempt_count+1`, `last_polled_at`, clear `last_error` when no failure, `RescheduleWatch(failed=anyFailure)`. Heartbeat between sources; cancel or lease loss stops between sources (never mid-transaction). Logging line as Python (`emitted`, `dupe`, `failure`, `dur_ms`).

- [ ] **Step 1: Write failing tests**: `TestPollMatchesPythonSignalsAndCursor` (per kind against recorded transports and `pyworker` replies), `TestReEmitAfterCursorRollbackIsNoop` (crash between emit and cursor commit), `TestSourceFailureDoesNotAdvanceCursor`, `TestBillingRefusalIsFailureNotHealthy`, `TestAmbiguousLeadReschedulesWithoutPoll`, `TestUnresolvedLeadKeepsCursor`, `TestBudgetExceededReschedulesWithoutFetch`, `TestDisabledWatchDropsMirrorAndStops`, `TestOnSignalFiresOnlyForInsertedRows`, `TestJobChangeRoutesPerContactLead`, `TestCollectorFailuresPartialSuccess`, `TestAutoDisableAtMaxFailures`, `TestPollNowCountsQuota`, `TestCancelBetweenSources`, `TestLeaseLossLeavesStateConsistent`, `TestGoAndPythonPollSameWatchEmitOnce`, `TestTenantIsolation`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): watch_poll handler (RED)`, `feat(go): watch poll budget, lead resolution and source loop`, `feat(go): watch poll emission, cursor and finalise`.

### Task 16: Worker wiring and hand-back rules

**Files:**
- Modify: `cmd/worker/main.go`, `cmd/worker/main_test.go` (or the existing harness test)

**Interfaces:**
- Produces: the worker registers `trigger_eval` (Task 14, deadline 600 s) and `watch_poll` (Task 15, deadline 600 s) handlers and `queue.RegisterFailureHook("watch_poll", poller.Reconcile)`; both handlers are inert (return `HandBack` of the claimed job) when their `GO_EXECUTOR_*` flag is false, so a mis-routed row is never executed by an unprepared worker; exported `autoschedule.BootstrapSchedules` and `poller.BootstrapWatchSchedules` are callable from a future Go scheduler (no Go scheduler is started here, D3). The worker logs the effective `AUTOMATIONS_ENABLED`/`INTENT_POLLER_ENABLED`/executor flags once at startup (it cannot verify they match the API's).

- [ ] **Step 1: Write failing tests**: `TestWorkerRegistersAutomationHandlers`, `TestHandlersHandBackWhenFlagOff`, `TestFailureHookRegisteredForWatchPoll`, `TestTriggerEvalDeadline600`, `TestWatchPollDeadline600`, `TestUnknownJobTypeStillHandedBack`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS (existing worker tests unchanged).
- [ ] **Step 5: Commit** `test(go): worker automation wiring (RED)`, `feat(go): register trigger_eval and watch_poll handlers`.

### Task 17: Automations CRUD routes

**Files:** Modify stub `internal/api/automationscrud.go`; tests `internal/api/automationscrud_test.go`

**Interfaces:** Produces `POST /api/automations/triggers` (admin; `TriggerCreate`: `name` 1..200, `trigger_type`, `trigger_config={}`, `condition=""`, `actions=[]`, `scope_workbook_ids=[]`, `stop_on_error=false`, `max_spend_usd_per_day`, `max_actions_per_day`, `enabled=true`; response `to_api`), `GET /triggers` (`enabled`, `trigger_type`; ordered `created_at desc`), `GET /triggers/{id}` (`recent_runs` last 10), `PATCH /triggers/{id}`, `DELETE /triggers/{id}` (`{"deleted": true}`), `POST /triggers/{id}/pause` (`{"enabled": false}`), `POST /triggers/{id}/resume` (`{"enabled": true, "next_run_at": ...}`). Caps validated `>= 0` and finite, `actions` elements must be objects, `trigger_config` an object (422 FastAPI shape; Python 500s or stores garbage, D4); ids are opaque strings (unknown -> 404 `trigger not found`).

- [ ] **Step 1: Write failing tests**: `TestCreateMatchesFixtures` (every trigger type), `TestCreateValidationStrings`, `TestCreateDisabledFlag404`, `TestCreateRequiresAdmin` (editor/viewer 403), `TestListAndFilters`, `TestGetWithRecentRuns`, `TestPatchMatchesFixtures`, `TestDeleteMatchesFixture`, `TestPauseResumeMatchFixtures`, `TestForeignTrigger404`, `TestBadCapValues422`, `TestNonObjectActionElement422`, `TestScheduleCreateEnqueuesAlignedJob`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): automations crud routes (RED)`, `feat(go): automation trigger create list get`, `feat(go): automation trigger patch delete pause resume`.

### Task 18: Automations preview, run and history routes

**Files:** Modify stub `internal/api/automationsrun.go`; tests

**Interfaces:** Produces `POST /api/automations/triggers/{id}/preview` (reads; `{row_ids?, limit=25}` clamped 1..100; response `{matched, sample:[{row_id, workbook_id, condition_pass:true, actions:[{type, would_charge_usd, resolved_payload_preview}]}], projected_total_usd}` with `_preview_payload` for `webhook` (`{url, method, body}` via `rowtmpl`), `push_crm` (`{crm, field_map}`), `re_enrich` (`{column_ids}`), preview failure -> `{"error": "preview failed: <e>"}`; NO writes, NO debits, NO outbound call), `POST /triggers/{id}/run` (admin; `{row_ids?, dry_run=false}` -> `{job_id, fire_key, dry_run}`, job enqueued with the trigger_eval executor policy), `GET /triggers/{id}/runs` (`limit` default 50, max 200, 422 above), `GET /runs/{run_id}` (run `to_api` + `results`, ordered by id asc, 404 `run not found`). Preview and run only accept integer row ids that belong to the caller's workspace; other ids are dropped silently as Python drops missing rows, non-integers too (D4).

- [ ] **Step 1: Write failing tests**: `TestPreviewMatchesFixtures`, `TestPreviewMakesNoOutboundCallOrWrite` (egress and DB write spies), `TestPreviewLimitClamp`, `TestPreviewNonNumericIDsNoCrash`, `TestPreviewForeignRowsDropped`, `TestPreviewTemplateErrorShape`, `TestRunEnqueuesTriggerEval` (payload and executor), `TestRunDryRunFlag`, `TestRunRequiresAdmin`, `TestRunsLimitBounds422`, `TestRunDetailMatchesFixture`, `TestRunDetailForeign404`, `TestDisabledFlag404`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): automations run routes (RED)`, `feat(go): automation preview`, `feat(go): automation manual run and history`.

### Task 19: Watches CRUD routes

**Files:** Modify stub `internal/api/watchescrud.go`; tests

**Interfaces:** Produces `POST /api/watches` (editor; `WatchCreate`: `kind`, `target` min 1, `lead_id`, `signal_types`, `interval`, `config`, `create_webhook_rule=false`, `webhook_url`, `webhook_secret_ref`; response `_to_api` + `webhook_rule_id`), `GET /api/watches` (`limit 1..500=100`, `offset>=0`, `cursor` max 1024; `{watches, limit, offset, has_more, next_cursor}`, `offset` null with a cursor), `GET /api/watches/{id}`, `PATCH /api/watches/{id}` (`enabled`, `interval`, `signal_types`, `lead_id`, `config`), `DELETE /api/watches/{id}` (`{"deleted": true, "id": id}`). Flag gating (404 then 409) precedes everything as recorded; `target` trimmed; a duplicate `(workspace, kind, target)` answers 409 `watch already exists` (Python: unhandled `IntegrityError` -> 500, D4).

- [ ] **Step 1: Write failing tests**: `TestCreateMatchesFixturesPerKind`, `TestCreateValidationStrings`, `TestCreateDuplicateIs409`, `TestCreateWithWebhookRule`, `TestListPaginationMatchesFixtures`, `TestListInvalidCursor422`, `TestGetMatchesFixture`, `TestPatchMatchesFixtures`, `TestDeleteMatchesFixture`, `TestEditorRequiredForMutations`, `TestFlagGating404Then409`, `TestForeignWatch404`, `TestForeignLeadPin422`, `TestAccountGroupExtrasInResponse`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): watches crud routes (RED)`, `feat(go): watch create list get`, `feat(go): watch patch delete`.

### Task 20: Watch poll-now and signals routes

**Files:** Modify stub `internal/api/watchespoll.go`; tests

**Interfaces:** Produces `POST /api/watches/{id}/poll` (editor; 409 `watch disabled`; 429 `poll-now rate limited`; 429 `poll-now daily quota exhausted`; `{"status":"already_queued","job_id":N,"fire_key":...}` when a scheduled poll is active; `{"status":"queued","fire_key":...}`; `{"status":"already_queued","fire_key":...}` when the manual key lost the race) and `GET /api/watches/{id}/signals` (`signal_type`, `limit 1..200=50`, `offset>=0`, `cursor`; body = plan 05 page + `limit` + `offset` (null with a cursor); a signal-store `ValueError` -> 422 with its text). Poll-now never enqueues a second active poll for one watch under 16 concurrent callers.

- [ ] **Step 1: Write failing tests**: `TestPollNowMatchesFixtures`, `TestPollNowDisabled409`, `TestPollNowRateLimited429`, `TestPollNowQuota429`, `TestPollNowReusesScheduledJob`, `TestPollNowConcurrentEnqueuesOnce` (16 goroutines), `TestPollNowPayloadHasPollNow`, `TestSignalsMatchFixtures`, `TestSignalsAccountGroupExpansion`, `TestSignalsInvalidCursor422`, `TestSignalsLimitBounds422`, `TestSignalsForeignWatch404`, `TestSignalsReadOnlyMemberAllowed`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): watches poll and signals routes (RED)`, `feat(go): watch poll-now`, `feat(go): watch signals feed`.

### Task 21: Contract coverage, nginx split, runbook

**Files:**
- Create: `testdata/contract/retained_automation_routes.txt`, `../../docs/go-port/runbooks/automations-cutover.md`
- Modify: `testdata/contract/README.md` (deviations D4-D7), `../../infra/nginx/go-routes.conf` (one delimited section `# --- plan 08 automations ---`), `internal/contract/coverage_test.go` (add the 18 routes)

**Interfaces:** Produces: every one of the 18 routes has at least one recorded fixture and a test; `retained_automation_routes.txt` lists no route (explicitly: none) and the job-level retentions; nginx maps `/api/automations/` and `/api/watches` (exact and subpaths) by the shared sticky bucket (default 0%), with `POST /api/automations/triggers/*/run` and `/api/watches/*/poll` following the bucket (the enqueue is executor-routed, not path-routed). The runbook covers: flag order (`GO_AUTOMATION_ROUTES_ENABLED` per cohort, then `GO_EXECUTOR_TRIGGER_EVAL`, then `GO_EXECUTOR_WATCH_POLL`; routes first, executors last), why the Python scheduler stays the single scheduler and how to verify one active job per `fire_key` (SQL given), hand-back behaviour and the metric to watch, cap/credit reconciliation queries (reservations held older than a day, `in_flight` markers older than a run, charged-without-marker must be zero), webhook delivery being at-most-once, rollback per flag, `AUTOMATIONS_ALLOW_LEGACY_OUTREACH` guard, and the check that `scheduled_triggers`/`watch_schedules` stay non-RLS with runtime-role grants.

- [ ] **Step 1: Run** `uv run python scripts/contract_record.py --cases scripts/contract_cases/automations.yaml --check` (fixtures complete, byte-identical).
- [ ] **Step 2: Write failing tests** `TestEveryAutomationRouteHasFixture`, `TestEveryWatchRouteHasFixture`, `TestRetainedAutomationListMatchesFastAPI` (diff of the FastAPI route table against Go patterns plus the retained list is empty), `TestDeviationsDocumented` (D4-D7 ids appear in `README.md`), `TestRuntimeRoleCanUseMirrorsAndLedgers`. Run `make test` -> FAIL.
- [ ] **Step 3:** record missing fixtures, write the nginx section, runbook and deviations. **Step 4: Run** `make vet test test-integration` and `scripts/nginx-routing-check.sh` at 0% and 100% (`/api/automations/triggers/x/run` and `/api/watches/x/poll` follow the bucket; no path of these prefixes resolves to a retained route) -> PASS.
- [ ] **Step 5: Commit** `test(go): automation route coverage (RED)`, `feat(infra): nginx per-route split for automations and watches`, `docs(go-port): automations cutover runbook and deviations`.

---

## Self-Review

- **Spec coverage:** inventory row 7 "automations engine/router, trigger_eval, watches + poller": engine/handler (4, 5, 6, 10, 14), router (13, 17, 18), emitters including the half plan 04 ported (8), schedule bootstrap (7), watches router (13, 19, 20), poller engine and sources (9, 11, 12, 15), failure reconcile (9, 16), executor routing (2, 16), nginx and runbook (21). JobSpy fan-in and DDG cross the boundary through `pyworker` (12). The Python scheduler stays (D3).
- **Consistency:** `safecond.Evaluate` (4) in 14 and 18; `autocaps.TryReserve` (5) in 14; `autoactions.Webhook` (6) and `Execute` (10) in 14; `autoschedule` (7) in 9, 13, 14; `automationhook` emitters (8) in 15 and plan 06 call sites; `poller` core (9) in 11, 12, 13, 15; `autosvc`/`watchsvc` (13) in 17-20; `queue.RegisterFailureHook` (plan 07 Task 3) in 16; `crmpush`/`rowtmpl` (plan 07) in 10 and 18.
- **Review Focus:** 1 -> Tasks 5, 10, 14; 2 -> 5, 14; 3 -> 4; 4 -> 6, 11, 12; 5 -> 13, 17-20; 6 -> 7, 9, 15; 7 -> 8, 15; 8 -> 14, 16.
- **Proportion:** interfaces, error strings, test names and file ownership carry the decisions; handler and SQL bodies are left to the implementer and to the recorded vectors and transcripts.

## Open decisions (defaults apply unless overridden)

- **D1 Python boundary for the poller.** Hiring/tech detection (JobSpy), job-change search (DDG) and account-group partnership jobs cannot run in Go. Default: they cross the plan 06 `pyworker` `ProviderCall` RPC (`jobspy_signals`, new `ddgs_search`); everything else (SEC, RSS, web tech, pricing) is Go.
- **D2 Legacy outreach actions.** `sequencer` and `send_email` depend on the outreach store and SMTP (the outreach plan) and are gated by `AUTOMATIONS_ALLOW_LEGACY_OUTREACH` (default false). Default: Go routes refuse to register when that flag is true (Python keeps the surface), and the Go worker hands back any trigger_eval whose rule contains such an action.
- **D3 Scheduler.** No Go scheduler exists. Default: the Python scheduler remains the only scheduler through this plan; Go ships `BootstrapSchedules` and `BootstrapWatchSchedules` as libraries whose enqueue is byte-identical and executor-routed, so a Go scheduler (later plan) is a wiring change.
- **D4 Hardening deviations.** Non-integer row ids, non-object actions/config, negative or non-finite caps, foreign lead pins, duplicate watches and token/body bounds answer 4xx instead of 500; templates, conditions, targets and RSS/webhook bodies are size-bounded; per-signal fan-out is capped. Default: all applied and listed in `testdata/contract/README.md`.
- **D5 Cap serialisation.** Default: an advisory lock per (workspace, UTC day) replaces Python's row lock that does not cover the first reservation of a day (documented deviation, strictly safer).
- **D6 Debit atomicity.** Python commits the debit, then the `in_flight` marker, leaving a crash window that charges without a marker. Default: reservation, debit and marker commit in one transaction (documented deviation).
- **D7 Webhook delivery.** Default: at-most-once for non-idempotent actions exactly as Python (no automatic retry after the `in_flight` marker), with the `skipped/replay` record; no new retry policy is introduced.
- **D8 Re-enrich eligibility.** Default: a `trigger_eval` job whose `re_enrich` targets any column type the Go worker cannot run is handed back to Python whole (before any write); mixed runs are never split.
- **D9 Rollout.** Default: all new Go paths off (`GO_AUTOMATION_ROUTES_ENABLED`, `GO_EXECUTOR_TRIGGER_EVAL`, `GO_EXECUTOR_WATCH_POLL` false); canary by workspace cohort with the shared nginx bucket; routes first, executors last; `AUTOMATIONS_ENABLED` must match on both stacks.

## User decisions

(none yet; the defaults above apply until the user objects)
