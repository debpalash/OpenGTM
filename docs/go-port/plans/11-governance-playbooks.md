# Governance and Research Playbooks Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Port the `/api/governance` router (17 of its 18 routes), the `/api/research-playbooks` router (9 of its 10 routes), and the `retention_enforce`, `research_playbook_run` and `research_playbook_schedule` job handlers to Go. Playbook steps run the web-research agent through the plan 06 `pyworker` RPC, so Python keeps only the LLM, ddgs and fetch calls.

**Architecture:** Pure domain packages (`govaudit`, `retention`, `govadmin`, `govidp`, `playbooks`) sit under thin handlers in `internal/api`; every tenant-table access runs inside `tenancy.InWorkspace`. Retention enforcement and playbook runs are Go-executor jobs claimed by the existing worker. Python's scheduler keeps running the `retention` and `playbooks` bootstraps (a Go scheduler is out of scope) and enqueues those jobs with `executor='go'` when the workspace is in the Go cohort. A playbook step is one `pyworker.Client.Call` with provider `research_column`; the Go side owns the ledger, cancellation, budgets and counts.

**Tech Stack:** Go 1.27.3 stdlib, pgx v5 + sqlc, existing `internal/{httpx,identity,tenancy,audit,contract,queue,canonjson,secrets,pyworker,leadsread}`; no new modules. Python/uv for recorder, seed, vectors and the Python-side RPC provider.

**Spec:** `docs/go-port/inventory.md` section 8 row 10 "Governance + playbooks" (sections 2 HTTP surface, 3 durable job types, 5 data stores, 6 background processes, 9 risks); `docs/go-port/plans/02-go-api-foundation-auth-workspaces.md` (audit middleware, `RequireRole`, SCIM routes), `03-settings-secrets.md` (secret resolver), `06-enrichment-jobs.md` (`pyworker`, `queue.EnqueueOnce`, executor routing). Python sources: `apps/api/routers/{governance,playbooks}.py`, `services/governance/{models,audit,retention}.py`, `services/playbooks/{models,engine,scheduler}.py`, `services/workspace/{oidc,scim,manager}.py` (member and permission helpers only), `services/workbook/research_column.py` (the RPC target), `services/job_registry.py`, `scheduler.py`.

**Layout note (branch `go-rewrite`):** Python lives untracked at `legacy/python/`; Python paths (`apps/api/...`, `scripts/...`) mean `legacy/python/<path>`. Go paths are relative to `apps/backend-go/`. Module `github.com/debpalash/OpenGTM/apps/backend-go`.

**Prerequisite:** plans 02, 03 merged (identity, `RequireRole`, audit middleware, secrets resolver, SCIM queries); plan 06 Tasks 3, 5, 12 merged (`pyworker.Client`, `queue.EnqueueOnce`, executor routing, `Deps.PyWorker`). All 11 tables used here already exist in `internal/db/schema.sql`; this plan needs no Alembic change and no `RequiredGoSchemaRevision` bump.

## Global Constraints

- Go `go 1.27` + `toolchain go1.27.3`; pgx v5 + sqlc only; `make sqlc-check` clean. Go never runs DDL. No new modules.
- Tenant tables only inside `tenancy.InWorkspace`; runtime role non-superuser `NOBYPASSRLS`. Cross-workspace ids answer 404, never 403. `retention_schedules` and `playbook_schedules` have no RLS (identifier-only mirrors for scheduler cold start); they are written in the same transaction as their policy or playbook.
- Contract parity is the release bar: paths, methods, JSON key order, status codes, `detail` strings, 422 bodies, auth gates. Gates verbatim: governance = `require_admin` (`identity.RequireRole("governance.manage","admin")`), except `GET /capabilities` (retained); playbooks writes = `identity.RequireRole("agents.write","editor","admin")`; playbook GET list, runs list, results = `current_workspace` only. Datetimes render like FastAPI (`isoformat`: naive columns without `Z`, microseconds only when non-zero; `timestamptz` with `+00:00`).
- Audit: playbook `retry` and `cancel` set `audit.SetMetadata` with the exact Python `audit_metadata` keys; no other new audit code (plan 02 middleware records every write).
- Secrets (SCIM bearer, OIDC client secret) never appear in logs, errors, audit metadata or fixtures. The plaintext SCIM token is returned exactly once.
- Hardening deviates from Python only as a documented deviation (decisions D3-D6) in `testdata/contract/README.md`: no 500 on bad input, bounded batches, no unbounded response.
- Python `datetime` query params (`since`, `before`) accept the pydantic v2 ISO-8601 grammar; Go accepts the recorded subset (Task 1 vectors) and answers 422 `datetime_from_date_parsing`/`datetime_parsing` bodies for the rest, never 500.
- Runtime flags default off: `GO_GOVERNANCE_ROUTES_ENABLED`, `GO_PLAYBOOK_ROUTES_ENABLED`, `GO_EXECUTOR_RETENTION`, `GO_EXECUTOR_PLAYBOOKS` (Task 3). Playbook runs additionally need `PY_WORKER_URL`; without it a claimed run is handed back (`queue.HandBack`).
- Verify with `cd apps/backend-go && make sqlc-check vet test` and `make test-integration`. No CI changes.
- Every commit DCO signed (`git commit -s`), no `Co-Authored-By` trailer. GRANULAR commits: tests first (RED, message ends `(RED)`), then each implementation piece. Stage explicit paths only; `make sqlc` regenerates all of `internal/db/gen/`, so each task stages only its own `<name>.sql.go` and `.sql`.
- Retained on FastAPI after this plan (`testdata/contract/retained_governance_routes.txt`, Task 19): `GET /api/governance/capabilities` and `GET /api/research-playbooks/capabilities` (D1).

## Review Focus

1. **Purge safety.** `retention_enforce` deletes data irreversibly. A legal hold set after the run started (or `enabled` false on a scheduler run) must stop deletion at the next batch, not at the next job; wrong `deleted_counts` or a cutoff computed from the wrong clock must not delete live rows; the six category cutoffs use three storage types (timestamp, `YYYY-MM-DD` string for `llm_usage_daily.date`, float epoch for `signals.created_at`); another workspace's rows must never be touched. `confirmation` must equal `PURGE EXPIRED DATA` exactly. Tasks 5, 9, 13.
2. **Double or lost runs.** Retry after a final failure, a cancel racing the worker, a scheduler occurrence enqueued twice (`fire_key`), a dead Go lease reconciled by the Python reaper. `cancelled` is never overwritten by `completed`; `success` results are never re-run on retry; counts come from the ledger, not from memory. Tasks 9, 10, 11, 17, 18.
3. **Paid research bounded.** A step calls LLM and web through `pyworker`; the per-cell budget is `cell_budget_usd / len(steps)`, a failed step stops the profile, an unreachable Python worker hands the job back instead of marking 100 profiles failed, an oversized `value` or metadata from Python is capped, and cancel stops before the next paid call. Task 11.
4. **Cursor, pagination and export correctness.** Equal `created_at` ties, naive vs aware datetimes, a forged or malformed cursor (422, never 500), `limit+1` has-more logic, `offset` ignored with cursor, CSV formula injection (`=+-@\t\r` prefixed with `'`), BOM and `\r\n`, non-ASCII `metadata_json`, streaming without loading all rows. Tasks 4, 12.
5. **Identity-admin guards.** The owner can never be demoted, overridden or removed; a non-member cannot receive a permission; enforce-SSO without OIDC is rejected; an enabled OIDC config needs a secret; issuer host must be in `SSO_ALLOWED_ISSUER_HOSTS`; rotating a SCIM token invalidates the previous one atomically; 90-day clamp from `OPENGTM_SCIM_TOKEN_TTL_DAYS`. Tasks 6, 7, 14, 15.
6. **PATCH with nulls.** `{"name": null}`, `{"enabled": null}`, a duplicate name and a foreign `schedule_audience_id` crash or leak in Python (500 on NOT NULL / unique violation). Go must answer 422/409/404. Tasks 8, 16.

## File Structure

```
internal/govaudit/{query,cursor,csv}.go                      # audit event list, cursor codec, streaming CSV
internal/retention/{days,targets,policy,schedule}.go         # normalize, per-category deletes/counts, policy store, next-run
internal/retention/{enforce,reconcile,handler}.go            # Tasks 9, 18 (job)
internal/govadmin/{permissions,members}.go                   # PERMISSIONS catalog, member and override operations
internal/govidp/{oidc,scimtoken}.go                          # OIDC config save/read, SCIM token status/rotate/revoke
internal/playbooks/{model,validate,store}.go                 # playbook/run/result store, step validation, cursors
internal/playbooks/{schedule,engine,handler}.go              # Tasks 10, 11, 18
internal/api/{governanceaudit,governanceretention,governancerbac,governancesso}.go
internal/api/{playbooksroutes,playbookruns}.go
internal/api/register.go                                     # (modify once, Task 3)
internal/db/queries/{gov_audit,gov_retention,gov_admin,gov_idp,playbooks}.sql (+ gen/)
cmd/worker/handlers_governance.go                            # job-type -> handler map, failure hook (Task 18)
testdata/contract/{governance,retention,rbac,sso,playbooks}/*.json
testdata/contract/retained_governance_routes.txt
../../legacy/python/apps/api/services/playbooks/rpc_research.py   # Python RPC provider `research_column` (Task 2)
../../legacy/python/scripts/{contract_seed.py,contract_cases/governance.yaml,gen_governance_vectors.py}
../../infra/nginx/go-routes.conf
../../docs/go-port/runbooks/governance-playbooks-cutover.md
```

## Parallel waves

Tasks in a wave touch disjoint files and may run concurrently (up to 6 agents at a time). Each wave starts only after the previous wave is merged and `make vet test test-integration` is green.

| Wave | Tasks | Shared-file notes |
|---|---|---|
| 0 (sequential) | 1 Python seed, cases, vectors; 2 Python executor routing + RPC provider; 3 Go scaffold | T1 owns `contract_seed.py`, `governance.yaml`, `gen_governance_vectors.py`; T2 owns `rpc_research.py`, `job_scheduling.py` call sites in `governance/retention.py`, `playbooks/{scheduler}.py`, `routers/{governance,playbooks}.py` enqueue lines; T3 owns `register.go`, `config.go` additions, all stub route files, `cmd/worker/handlers_governance.go` stub |
| 1 | 4 govaudit, 5 retention core, 6 govadmin, 7 govidp, 8 playbooks store | 5 disjoint packages, each with its own `.sql` |
| 2 | 9 retention enforce + reconcile, 10 playbooks schedule, 11 playbooks engine | 9 adds files to `internal/retention/`, 10 and 11 add files to `internal/playbooks/` (different files, no overlap); 11 consumes 8 and `pyworker` |
| 3 | 12 audit routes, 13 retention routes, 14 rbac + members routes, 15 sso + scim-token routes, 16 playbook CRUD routes, 17 playbook run routes | each owns exactly one stub `internal/api/*.go` (16 and 17 own `playbooksroutes.go` and `playbookruns.go`) and one fixture dir |
| 4 (sequential) | 18 worker wiring and failure hook; 19 coverage + nginx + runbook | 18 owns `handlers_governance.go`, `retention/handler.go`, `playbooks/handler.go`; 19 consumes everything |

---

### Task 1: Python seed, recorder cases, vectors

**Files:**
- Modify: `legacy/python/scripts/contract_seed.py` (append-only)
- Create: `legacy/python/scripts/contract_cases/governance.yaml`, `legacy/python/scripts/gen_governance_vectors.py`
- Create (output, committed): `testdata/contract/{governance,retention,rbac,sso,playbooks}/*.json`, `internal/{govaudit,retention,govadmin,govidp,playbooks}/testdata/*.json`

**Interfaces:**
- Consumes: plan 04/05 seed (workspaces A/B, users alice admin/owner, bob editor, carol viewer; audiences; leads).
- Produces: seed additions (60 `governance_audit_events` for workspace A with 8 sharing one `created_at`, events for B; one retention policy row in A, none in B; 3 `retention_runs`; for every retention category one row older than its minimum and one newer in A and one old row in B; an audience with 5 members; 3 playbooks (one multi-step, one scheduled, one disabled) with 4 runs in each terminal and active status and 12 results; member overrides) and recorded fixtures for the 26 routes. Vectors: `normalized_days` (defaults, each minimum, 3650, 3651, unknown category, non-integer), cursor encode/decode (valid, padding, garbage, wrong shape, naive and aware datetimes), query-param datetime acceptance table (25 strings), CSV `safe` cases, `_safe` leading-char table, `schedule_policy` next run (`now + 1 day`, disabled, legal hold), playbook `schedule_next` interval clamp (`max(15, min(x, 10080))`), step validation (duplicate keys, bad format, 8 steps, 9 steps, key regex), OIDC `save_config` normalization (domains lowercased and sorted, trailing `/` issuer, default role), SCIM token status with TTL env 0, 1, 400, `abc`.
- Cases (`{name, method, path, as, expect_status}`): audit list default/limit 500/501 (422)/`actor_user_id=x` (422)/`method=post`/`since` forms/cursor next page/forged cursor 422; export.csv with filters; retention get (no policy, with policy), put (valid, min violation 422, unknown category 422, `enabled` missing 422), preview, enforce (wrong confirmation 422, no policy 409, legal hold 409, success 202), runs (offset, cursor); rbac get, put (unknown permission 404, non-member 404, owner 409, clear with null); members add (unknown user 404, existing 409, invalid role 422), patch (owner 409), delete (204, owner 409); sso get/put (bad issuer 422, enforce without enable 422, enabled without secret 422); scim-token get/post/delete; playbooks list, create (201, duplicate 409, `steps` duplicate keys 422, `max_steps` 9 422, `output_format` xml 422), patch (version bump on prompt change, foreign audience 404, `schedule_interval_minutes` 14 422), delete (409 active run, 204), start run (disabled 409, foreign audience 404, 202), list runs (cursor), results (cursor `abc` 422), retry (409 on running, 202 on failed), cancel (409 on completed, 200); viewer 403 for every gated route; foreign-workspace ids 404. Non-superuser role, `TZ=UTC`.

- [ ] **Step 1: Extend the seed** (fixed ids and timestamps, deterministic result ids).
- [ ] **Step 2: Write `governance.yaml` and `gen_governance_vectors.py`** (import the real Python modules; never hand-write expected values).
- [ ] **Step 3: Run** `uv run python scripts/contract_record.py --cases scripts/contract_cases/governance.yaml` and `uv run python scripts/gen_governance_vectors.py`; Expected: files written, re-running is byte-identical.
- [ ] **Step 4: Commit** (Go/doc side only) `test(go): governance and playbook contract fixtures and vectors (RED)`.

### Task 2: Python executor routing and `research_column` RPC provider

**Files:**
- Create: `legacy/python/apps/api/services/playbooks/rpc_research.py`
- Modify: `legacy/python/apps/api/services/governance/retention.py` (`schedule_policy`, `bootstrap_retention_schedules` enqueue sites), `services/playbooks/scheduler.py` (`schedule_next`, `bootstrap_playbook_schedules`, `handle_playbook_schedule` enqueue sites), `routers/governance.py` (`enforce_policy`), `routers/playbooks.py` (`start_run`, `retry_run`), `services/go_executor.py` (one helper)
- Test: `legacy/python/tests/test_governance_executor_routing.py`, `tests/test_rpc_research.py`

**Interfaces:**
- Consumes: plan 06 Task 3 (`internal_provider.py` provider registry, `GO_EXECUTOR_ENABLED`, `GO_EXECUTOR_WORKSPACES`). Assumes plan 06 Task 3 exposes a way to register a named RPC provider; if it only reads `python_providers.txt`, this task adds the name there and one import line in `internal_provider.py` (append-only, coordinate with plan 06).
- Produces: `go_executor.executor_for(job_type: str, workspace_id: str) -> str` returning `'go'` only when `GO_EXECUTOR_ENABLED`, the workspace is in `GO_EXECUTOR_WORKSPACES` and the per-type flag is on (`GO_EXECUTOR_RETENTION` for `retention_enforce`; `GO_EXECUTOR_PLAYBOOKS` for `research_playbook_run` and `research_playbook_schedule`), else `'python'`; every enqueue site above passes it. RPC provider `research_column`: input `{"prompt_template": str, "context": object, "max_steps": int, "output_format": "text"|"json", "cell_budget_usd": float}` -> output `{"success": bool, "value": any, "metadata": object, "error": str|null}` by calling `execute_research_column(prompt_template, context, [], max_steps=..., output_format=..., workspace_id=<call workspace>, cell_budget_usd=...)`; the workspace comes from the RPC envelope, never from `input`; `value` and `metadata` are capped at 64 KiB serialized each.

- [ ] **Step 1: Write failing tests**: `test_retention_enqueue_executor_follows_flag`, `test_playbook_enqueue_executor_follows_flag`, `test_scheduler_bootstrap_routes_per_workspace`, `test_research_column_rpc_matches_direct_call`, `test_research_column_rpc_ignores_input_workspace`, `test_research_column_rpc_caps_output`.
- [ ] **Step 2: Run** `uv run pytest tests/test_governance_executor_routing.py tests/test_rpc_research.py` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS plus `uv run pytest tests -k "retention or playbook" -q` unchanged.
- [ ] **Step 5: Commit** (unversioned; note in the PR).

### Task 3: Go scaffold: flags, register lines, stubs

**Files:**
- Modify: `internal/api/register.go` (+`RegisterGovernance`, `RegisterPlaybooks` lines), `internal/config/config.go`
- Create: stub files `internal/api/{governanceaudit,governanceretention,governancerbac,governancesso,playbooksroutes,playbookruns}.go` (`func registerX(r *httpx.Router, d Deps)`, empty body), `cmd/worker/handlers_governance.go` (`func governanceHandlers(d workerDeps) map[string]jobHandler`, returns an empty map)
- Test: `internal/api/governancescaffold_test.go`, `internal/db/schema_governance_test.go`

**Interfaces:**
- Produces: config `GO_GOVERNANCE_ROUTES_ENABLED`, `GO_PLAYBOOK_ROUTES_ENABLED`, `GO_EXECUTOR_RETENTION`, `GO_EXECUTOR_PLAYBOOKS` (all default false), `SSO_ALLOWED_ISSUER_HOSTS` (string, default as Python), `OPENGTM_SCIM_TOKEN_TTL_DAYS` (plan 02 may already define it: reuse); `RegisterGovernance(r *httpx.Router, d Deps)` and `RegisterPlaybooks(r *httpx.Router, d Deps)` register nothing and log when `PG_LEAD_STORE` or their flag is false. Assumes plan 06 Task 5 added `Deps.PyWorker *pyworker.Client` and the worker's `jobHandler` type (`func(ctx context.Context, c *queue.Claim) error`) with a dispatch map in `cmd/worker/main.go` (plan 06 Task 22); `governanceHandlers` is merged into that map by Task 18 only.

- [ ] **Step 1: Write failing tests**: `TestGovernanceRoutesNotRegisteredWithoutFlag`, `TestGovernanceRoutePatterns` (exactly the 17 routes of Tasks 12-15), `TestPlaybookRoutePatterns` (exactly the 9 routes of Tasks 16-17), `TestSchemaHasGovernanceAndPlaybookTables` (the 11 tables and the `uq_research_playbook_name` / `uq_playbook_result_run_lead` constraints exist in `schema.sql`).
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make vet test` -> PASS.
- [ ] **Step 5: Commit** `test(go): governance scaffold (RED)`, `feat(go): governance and playbook route scaffold and flags`.

### Task 4: `govaudit`: audit event list, cursor codec, CSV stream

**Files:**
- Create: `internal/db/queries/gov_audit.sql`, `internal/govaudit/{query,cursor,csv}.go`, `internal/govaudit/*_test.go`, `internal/govaudit/*_integration_test.go`

**Interfaces:**
- Produces: `type Filter struct{ ActorUserID *int32; Method, Outcome string; Since, Before *time.Time }` (method upper-cased); `type Event struct{...}` with JSON order `id, actor_user_id, actor_role, method, route, resource_path, response_status, outcome, request_id, metadata, created_at`; `func List(ctx, tx pgx.Tx, ws string, f Filter, cursor string, limit int) (Page, *CursorError)` with `Page{Events []Event; NextCursor *string; NextBefore *time.Time; HasMore bool; Limit int}` (when a cursor is present `Before` is ignored, order `created_at DESC, id DESC`, `limit+1` fetch); `func EncodeCursor(t time.Time, id string) string` / `DecodeCursor(s, label string) (time.Time, string, *CursorError)` (urlsafe base64 without padding of compact JSON `[iso, id]`; naive times become UTC; label strings `audit event`/`retention run`/`playbook run` give 422 `invalid <label> cursor`); `func ExportCSV(ctx, pool, ws string, f Filter, w io.Writer) error` (BOM, header of 11 columns, `\r\n`, `safe()` prefixing `'`, `metadata_json` compact `ensure_ascii=False`, 500-row keyset batches, one transaction per batch so a long export holds no snapshot); `func ParseTimeParam(s string) (time.Time, error)` accepting the Task 1 table.
- Cursor and retention-run cursors share `EncodeCursor/DecodeCursor` (Tasks 5, 8 import them).

- [ ] **Step 1: Write failing tests**: `TestCursorVectorsMatchPython`, `TestCursorRejectsGarbageWrongShapeAndNonString` (422, no panic), `TestListOrderTiesByIDDesc`, `TestListHasMoreAndNextCursor`, `TestListCursorIgnoresBefore`, `TestListFiltersMethodUppercase`, `TestListTenantIsolation`, `TestCSVSafeVectors`, `TestExportCSVBytesMatchFixture`, `TestExportCSVStreamsInBatches` (3000 rows, memory bounded by batch), `TestParseTimeParamTable`.
- [ ] **Step 2: Run** `make test test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): govaudit list, cursor and csv (RED)`, `feat(go): audit event cursor codec and list`, `feat(go): audit event csv export`.

### Task 5: `retention` core: days, targets, preview, policy, schedule

**Files:**
- Create: `internal/db/queries/gov_retention.sql`, `internal/retention/{days,targets,policy,schedule}.go`, tests

**Interfaces:**
- Consumes: Task 4 cursor codec; plan 06 `queue.EnqueueOnce`.
- Produces: `var DefaultDays = ordered map` (`audit 365, llm_usage 365, signals 365, activation 180, audience_history 365, agent_results 180, outreach_history 365`), `MinDays`; `func NormalizeDays(in map[string]any) (map[string]int, error)` (error text byte-equal to Python: `unsupported retention categories: a, b` sorted; `<key> retention must be <min>..3650 days`; non-integers rejected before this by request validation); `type Target struct{ Category, Table string; Kind CutoffKind }` with the 8 targets of `_targets` (`governance_audit_events.created_at` timestamp, `llm_usage_daily.date` string `YYYY-MM-DD`, `signals.created_at` float epoch, `destination_deliveries`, `destination_inbound_receipts`, `audience_membership_events`, `playbook_results`, `outreach_sends` timestamps) and `func Cutoff(t Target, days int, now time.Time) any`; `func Preview(ctx, tx, ws string, days map[string]int, now time.Time) (map[string]int64, error)` (counts per category, categories with two tables summed, key order = `DefaultDays`); policy store `GetPolicy`, `UpsertPolicy(ctx, tx, ws, enabled, hold bool, days map[string]int, by int32) (Policy, error)`, `Policy` JSON order `workspace_id, enabled, legal_hold, retention_days, next_run_at, updated_by, created_at, updated_at`; `func SchedulePolicy(ctx, tx pgx.Tx, p *Policy, now time.Time) (*time.Time, error)` (= `schedule_policy`: cancels pending `retention_enforce` jobs with `fire_key LIKE 'retention:<ws>:%'`, next run `now + 1 day` only when enabled and not on hold, mirror upsert in `retention_schedules`, `EnqueueOnce` with `fire_key=retention:<ws>:<date>` and `executor` from the flag-driven choice `ExecutorFor`); `func ExecutorFor(cfg, ws string) string` mirrors Task 2.

- [ ] **Step 1: Write failing tests**: `TestNormalizeDaysVectors`, `TestPreviewCountsEachCategoryOldVsNew`, `TestPreviewSignalsEpochAndLLMDateString`, `TestPreviewTenantIsolation`, `TestUpsertPolicyShapeMatchesFixture`, `TestSchedulePolicyCancelsPendingAndEnqueuesNextDay`, `TestSchedulePolicyDisabledOrHoldClearsMirror`, `TestSchedulePolicyUsesGoExecutorOnlyInCohort`.
- [ ] **Step 2: Run** `make test test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): retention days, preview and schedule (RED)`, `feat(go): retention policy and preview`, `feat(go): retention scheduling`.

### Task 6: `govadmin`: permissions catalog, members, overrides

**Files:**
- Create: `internal/db/queries/gov_admin.sql`, `internal/govadmin/{permissions,members}.go`, tests

**Interfaces:**
- Produces: `var Permissions` ordered catalog of 11 entries (key, label, description, default_roles) copied from `PERMISSIONS`; `func Overview(ctx, tx, ws string) (RBAC, error)` (`permissions` list in catalog order and `members` with `username` fallback `User <id>` and `overrides`; member order = `created_at` ascending like `list_members`); `func SetOverride(ctx, tx, ws string, userID int32, permission string, effect *string) (Effective, error)` returning `{user_id, permission, effect, effective}` and the typed errors `ErrUnknownPermission` (404 `Unknown workspace permission`), `ErrNotMember` (404 `Workspace member not found`), `ErrOwner` (409 `Owner permissions cannot be overridden`); `func AddMember(ctx, tx, ws, username, role string) (MemberOut, error)` (404 `User account not found`, 409 `User is already a workspace member`; the username is trimmed; membership test matches `is_member`, i.e. member row or workspace owner); `func SetRole(ctx, tx, ws string, uid int32, role string) (MemberOut, error)` (404, 409 `Owner role cannot be changed`; permission overrides preserved); `func Remove(ctx, tx, ws string, uid int32) error` (404, 409 `Workspace owner cannot be removed`; deletes OIDC identities, overrides and the member row in one transaction and also the user's SCIM group memberships in that workspace, which Python leaves dangling: documented deviation D6).

- [ ] **Step 1: Write failing tests**: `TestCatalogMatchesPythonOrderAndRoles`, `TestOverviewUsernameFallback`, `TestSetOverrideEffectiveComputation` (allow, deny, null with role in/out of defaults), `TestSetOverrideOwnerAndNonMember`, `TestAddMemberDuplicateAndUnknown`, `TestAddMemberOwnerRoleNotAssignable` (role enum excludes `owner`), `TestSetRoleKeepsOverrides`, `TestRemoveCleansOIDCOverridesAndSCIMGroups`, `TestMembersTenantIsolation`.
- [ ] **Step 2: Run** `make test test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): governance member and rbac domain (RED)`, `feat(go): rbac overview and overrides`, `feat(go): workspace member admin`.

### Task 7: `govidp`: OIDC config and SCIM token

**Files:**
- Create: `internal/db/queries/gov_idp.sql`, `internal/govidp/{oidc,scimtoken}.go`, tests

**Interfaces:**
- Consumes: plan 03 `secrets.Resolver.Get/Set/Has(ctx, ws, key)`; plan 02 SCIM token row queries (`workspace_scim_tokens`) and config `OPENGTM_SCIM_TOKEN_TTL_DAYS`; workspace settings key `OIDC_CONFIG`.
- Produces: `type OIDCConfig struct{...}` JSON order `enabled, enforce_sso, issuer, client_id, allowed_domains, auto_provision, default_role`; `func GetConfig(ctx, tx, ws string) (OIDCConfig, error)`; `func SaveConfig(ctx, tx, r *secrets.Resolver, ws string, in OIDCInput) (OIDCConfig, error)` with Python's exact `ValueError` strings (`OIDC issuer must be a clean HTTPS origin or path`, `OIDC issuer host is not approved by SSO_ALLOWED_ISSUER_HOSTS`, `OIDC default role must be viewer, member, or editor`, `SSO enforcement requires OIDC to be enabled`, `Enabled OIDC requires issuer, client_id, and client_secret`); stored as compact JSON in the Python key order so Python's `/auth/sso/*` reads it unchanged; `client_secret` written through the resolver only when non-blank after strip, never echoed; `func SecretConfigured(ctx, r, ws) bool`; `type TokenStatus struct{ TokenPrefix string; CreatedAt, ExpiresAt float64; LastUsedAt *float64; CreatedBy *int32; Expired bool }`; `func Status(ctx, tx, ws string, now time.Time) (*TokenStatus, error)`, `func Rotate(ctx, tx, ws string, by int32, now time.Time) (plaintext string, err error)` (`og_scim_` + 36 random bytes urlsafe base64 without padding, sha256 hex digest, prefix = first 16 characters, upsert replaces hash and clears `last_used_at`), `func Revoke(ctx, tx, ws string) error`.

- [ ] **Step 1: Write failing tests**: `TestSaveConfigVectorsMatchPython`, `TestSaveConfigErrorStrings`, `TestSaveConfigDoesNotWriteSecretWhenBlank`, `TestSaveConfigStoredBytesReadableByPythonShape`, `TestIssuerHostAllowList`, `TestRotateInvalidatesPrevious` (old digest no longer authenticates through plan 02's SCIM auth), `TestRotateTTLClamp` (0 -> 1 day, 400 -> 365, `abc` -> 90), `TestStatusExpired`, `TestRevoke`, `TestTokenNeverLogged` (log capture), `TestTenantIsolation`.
- [ ] **Step 2: Run** `make test test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): oidc config and scim token domain (RED)`, `feat(go): oidc config save and read`, `feat(go): scim token rotate and revoke`.

### Task 8: `playbooks` store and validation

**Files:**
- Create: `internal/db/queries/playbooks.sql`, `internal/playbooks/{model,validate,store}.go`, tests

**Interfaces:**
- Consumes: Task 4 `govaudit.EncodeCursor/DecodeCursor`.
- Produces: `Playbook`, `Run`, `Result` structs whose JSON order equals the three `to_api` (`Result` uses key `metadata`, `Run` omits steps/prompt snapshots); `type Step struct{ Key, Name, PromptTemplate, OutputFormat string }`; `func ParseCreate(raw []byte) (CreateInput, []httpx.FieldError)` and `ParsePatch(raw []byte) (PatchInput, []httpx.FieldError)` reproducing pydantic: lengths, `max_steps` 1..8, `cell_budget_usd` `0 < x <= 5`, `steps` max 8, key regex `^[a-z][a-z0-9_]{0,39}$`, `value_error` bodies for duplicate keys and step `output_format`, plus explicit `null` handling for `PatchInput` (a present key with `null` on a non-nullable field is a 422 `none_not_allowed`-style body recorded in vectors, D3); store: `Create` (unique name -> `ErrNameTaken`), `Get`, `List` (order `updated_at DESC`), `Patch(ctx, tx, ws, id string, in PatchInput) (Playbook, error)` (version bump when any of `prompt_template, steps, output_format, max_steps, cell_budget_usd` is present; `ErrNameTaken`, `ErrAudienceNotFound`), `Delete` (`ErrActiveRun`), `StartRun(ctx, tx, ws string, pb Playbook, audienceID string, maxMembers int, by string) (Run, error)` (snapshots `prompt_template`, `steps`, version), `ListRuns(ctx, tx, ws, id string, limit, offset int, cursor string)`, `ListResults(ctx, tx, ws, runID string, limit, offset int, cursor string)` (integer cursor, `abc`/negative -> 422 `invalid playbook result cursor`), `RetryRun`, `CancelRun` (state machine errors `ErrNotRetryable`, `ErrNotCancellable`), `ResultCounts(ctx, tx, ws, runID string) (attempted, succeeded, failed int)`.

- [ ] **Step 1: Write failing tests**: `TestParseCreateMatchesPydantic422Vectors`, `TestParsePatchNullNonNullableIs422`, `TestCreateDuplicateNameConflict`, `TestPatchRenameCollisionIsErrNameTaken` (Python: 500), `TestPatchVersionBumpOnlyOnPromptFields`, `TestPatchForeignAudienceNotFound`, `TestPatchClearsScheduleWithNull`, `TestDeleteBlockedByActiveRun`, `TestStartRunSnapshotsStepsAndVersion`, `TestListRunsCursorAndOffset`, `TestListResultsCursorMalformed422`, `TestRetryOnlyTerminalFailed`, `TestCancelOnlyActive`, `TestResultCountsFromLedger`, `TestStoreTenantIsolation`.
- [ ] **Step 2: Run** `make test test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): playbook store and validation (RED)`, `feat(go): playbook models and request validation`, `feat(go): playbook store and run state machine`.

### Task 9: Retention enforcement and failure reconcile

**Files:**
- Create: `internal/retention/{enforce,reconcile}.go`, tests; extend `internal/db/queries/gov_retention.sql`

**Interfaces:**
- Consumes: Task 5 (`Targets`, `Cutoff`, `NormalizeDays`, `SchedulePolicy`).
- Produces: `func Enforce(ctx context.Context, d Deps, workspaceID, runID string) error` (= `handle_retention_enforce`: no policy -> return; unknown or missing `run_id` -> create a `scheduler` run from the policy; `completed`/`cancelled` -> return; legal hold, or disabled with `requested_by='scheduler'` -> `cancelled` with Python's error text then `SchedulePolicy`; otherwise `running`, per-category delete, `deleted_counts` keyed in `DefaultDays` order of categories reached, `completed`, `SchedulePolicy`); deletes run in batches (`DELETE ... WHERE ctid IN (SELECT ctid ... LIMIT 5000)`, each batch its own transaction, cutoffs fixed from `started_at` once per run); before every batch the run row and policy are re-read: a cancelled run or a legal hold set meanwhile stops with status `cancelled`, error `Legal hold enabled during retention enforcement`, and counts so far (D4); batch size `RETENTION_BATCH_ROWS` (default 5000); `func Reconcile(ctx, d Deps, jobID int64, payload []byte, errMsg string, willRetry bool) error` (= `reconcile_retention_job_failure` line for line: error message cut to 1000 chars, `Queue retry scheduled: ` / `Final failure: ` prefixes, legal-hold cancellation of run and pending queue job, re-schedule on final failure when the policy is enabled).

- [ ] **Step 1: Write failing tests**: `TestEnforceDeletesOnlyExpiredPerCategory` (old and new rows for all 8 targets, workspace B untouched), `TestEnforceCountsMatchFixture`, `TestEnforceBatchesStopOnLegalHoldMidRun`, `TestEnforceStopsWhenRunCancelled`, `TestEnforceSchedulerRunWhenDisabledIsCancelled`, `TestEnforceCreatesRunWhenMissing`, `TestEnforceCompletedRunIsNoop`, `TestEnforceFailureMarksFailedAndReturnsError`, `TestEnforceReschedulesNextDay`, `TestReconcileVectorsMatchPython` (retry, final, legal hold, missing run, empty workspace payload errors), `TestEnforceNeverDeletesWithoutWorkspaceScope` (pool size 1, no GUC leak).
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make sqlc-check vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): retention enforce and reconcile (RED)`, `feat(go): batched retention enforcement`, `feat(go): retention failure reconcile`.

### Task 10: Playbook scheduling

**Files:**
- Create: `internal/playbooks/schedule.go`, `internal/playbooks/schedule_test.go`, `internal/playbooks/schedule_integration_test.go`

**Interfaces:**
- Consumes: Task 8 store, plan 06 `queue.EnqueueOnce`, `retention.ExecutorFor` pattern (`playbooks.ExecutorFor`).
- Produces: `func ScheduleNext(ctx, tx pgx.Tx, pb *Playbook, now time.Time) (*time.Time, error)` (= `schedule_next`: cancels pending `research_playbook_schedule` jobs with `fire_key LIKE 'playbook_schedule:<id>:%'`; active only when `enabled` and `schedule_audience_id` and `schedule_interval_minutes`; next = `now + max(15, min(minutes, 10080))`; mirror upsert in `playbook_schedules`; `EnqueueOnce` with `fire_key=playbook_schedule:<id>:<next.isoformat()>`); `func RemoveSchedule(ctx, tx, id string) error`; `func HandleSchedule(ctx, d Deps, workspaceID, playbookID string) error` (= `handle_playbook_schedule`: playbook gone, disabled or unscheduled -> remove schedule and return; no pending/running run -> insert a `scheduler` run with `max_members=100` and enqueue `research_playbook_run` with `fire_key=playbook:<run id>` in the same transaction; then `ScheduleNext`); `func BootstrapDue(ctx, d Deps) (int, error)` is NOT ported (the Python scheduler keeps it, Task 2 routes its enqueues); a Go test pins the equivalence only.

- [ ] **Step 1: Write failing tests**: `TestScheduleNextClampsInterval`, `TestScheduleNextInactiveClearsMirror`, `TestScheduleNextCancelsPendingOccurrence`, `TestHandleScheduleCreatesRunOnce` (two concurrent calls, one run), `TestHandleScheduleSkipsWhenRunActive`, `TestHandleScheduleRemovesWhenPlaybookDisabled`, `TestHandleScheduleRunAndEnqueueAtomic` (enqueue failure rolls the run back), `TestScheduleFireKeyFormatMatchesPython`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** -> PASS.
- [ ] **Step 5: Commit** `test(go): playbook scheduling (RED)`, `feat(go): playbook schedule next and handler`.

### Task 11: Playbook run engine (steps through `pyworker`)

**Files:**
- Create: `internal/playbooks/engine.go`, `internal/playbooks/engine_test.go`, `internal/playbooks/engine_integration_test.go`

**Interfaces:**
- Consumes: Task 8 store, plan 06 `pyworker.Client.Call(ctx, ProviderCall) (ProviderResult, error)`, `pyworker.ErrUnavailable`, `queue.IsCancelled`.
- Produces: `type Researcher interface{ Research(ctx context.Context, ws string, req ResearchRequest) (ResearchResult, error) }` with `ResearchRequest{PromptTemplate string; Context map[string]any; MaxSteps int; OutputFormat string; CellBudgetUSD float64}` and `ResearchResult{Success bool; Value any; Metadata map[string]any; Error string}`; `func NewPyResearcher(c *pyworker.Client, timeout time.Duration) Researcher` (provider `research_column`, `CallID = <run_id>:<lead_id>:<step key>:<attempt>`, timeout `PLAYBOOK_STEP_TIMEOUT_SECONDS` default 120); `func Run(ctx context.Context, d Deps, workspaceID, runID string) error` (= `handle_playbook_run`: run missing or `completed`/`cancelled` -> return; playbook missing -> error `playbook not found`; status `running`, `started_at` kept when set; members of the audience ordered by `lead_id` limited to `max_members`; per member: re-read run status before the member and before every step (cancel -> result `cancelled`, `Cancelled by user`, return); a result already `success` is skipped; result `running`, `attempts+1`; steps from `steps_snapshot`, or the default single step `{"key":"result","name":"Research","prompt_template":<snapshot>,"output_format":<playbook format>}`; each step budget `cell_budget_usd / len(steps)`; context = lead snapshot plus earlier step values by key; the first failed step stops the member; result `value = str(value or "")` truncated at 64 KiB, `metadata = {steps:[{key,name,success,value,metadata,error}], completed_steps, total_steps}`, `error` truncated at 1000; a `ResearchResult` panic-free error from the client marks only that member `failed`; `pyworker.ErrUnavailable` before any member started returns the error so the worker hands the job back, after some members started marks the run `pending` for retry; finally counts from `ResultCounts`, status `completed` or `completed_with_errors`, `finished_at`); `func ReconcileFailure(ctx, d Deps, jobID int64, payload []byte, errMsg string, willRetry bool) error` (= `reconcile_playbook_job_failure`: retry -> run `pending` and `running` results back to `pending`; final -> `failed`, pending/running results `failed`, counts recomputed).

- [ ] **Step 1: Write failing tests** (with a fake `Researcher`): `TestRunProcessesMembersInLeadOrder`, `TestRunSkipsSuccessfulResultsOnRetry`, `TestRunStopsMemberAtFirstFailedStep`, `TestRunPassesPriorStepValuesInContext`, `TestRunBudgetSplitAcrossSteps`, `TestRunDefaultStepWhenSnapshotEmpty`, `TestRunCancelBeforeStepMarksCancelledResult`, `TestRunNeverOverwritesCancelledWithCompleted`, `TestRunCountsFromLedger`, `TestRunStatusCompletedWithErrors`, `TestRunUnavailableWorkerBeforeStartHandsBack`, `TestRunUnavailableMidRunMarksPending`, `TestRunCapsOversizedValue`, `TestRunOtherWorkspaceMembersNeverRead`, `TestReconcileVectorsMatchPython`, `TestPyResearcherCallShape` (provider, call id, workspace from the call, never from input).
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make vet test test-integration` -> PASS; with `TEST_PY_WORKER_URL` set also `TestRunAgainstPythonResearchColumn` (skips otherwise).
- [ ] **Step 5: Commit** `test(go): playbook run engine (RED)`, `feat(go): playbook run engine`, `feat(go): playbook failure reconcile`.

### Task 12: Audit event routes

**Files:** Modify stub `internal/api/governanceaudit.go`; tests `internal/api/governanceaudit_test.go`, `..._integration_test.go`; fixtures `testdata/contract/governance/`

**Interfaces:** Consumes Task 4. Produces `GET /api/governance/audit-events` (query `actor_user_id` int, `method`, `outcome`, `since`, `before` datetimes, `cursor` max 1024, `limit` default 100 `ge=1 le=500`; body keys `events, next_cursor, next_before, has_more, limit`; `next_before` is the last event's `created_at` when more, FastAPI datetime format) and `GET /api/governance/audit-events/export.csv` (`text/csv; charset=utf-8`, `Content-Disposition: attachment; filename=opengtm-audit-events.csv`, streamed through `govaudit.ExportCSV`; client disconnect cancels the query). Gate `RequireRole("governance.manage","admin")`; 422 bodies for bad query params carry pydantic `loc ["query", name]`.

- [ ] **Step 1: Write failing tests**: `TestAuditListMatchesFixtures`, `TestAuditListLimitBounds422`, `TestAuditListBadActorId422`, `TestAuditListBadSince422NoPanic`, `TestAuditListForgedCursor422`, `TestAuditExportMatchesFixtureBytes`, `TestAuditExportClientDisconnectStopsQuery`, `TestAuditRoutesRequireAdmin403ForEditor`, `TestAuditRoutesTenantIsolation`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): governance audit routes (RED)`, `feat(go): audit event list and csv routes`.

### Task 13: Retention routes

**Files:** Modify stub `internal/api/governanceretention.go`; tests; fixtures `testdata/contract/retention/`

**Interfaces:** Consumes Tasks 5, 9 types and `queue.EnqueueOnce`. Produces `GET /api/governance/retention` (no policy -> the default object with `workspace_id, enabled false, legal_hold false, retention_days DEFAULT, next_run_at null, updated_by null`), `PUT /retention` (body `enabled` required bool, `legal_hold` default false, `retention_days` map of ints; 422 `detail` string from `NormalizeDays`; upsert + `SchedulePolicy` in one transaction), `POST /retention/preview` (`retention_days, legal_hold, expired_counts`), `POST /retention/enforce` (202; body `confirmation`; 422 `confirmation must equal PURGE EXPIRED DATA`; 409 `Configure a retention policy first`; 409 `Legal hold blocks retention enforcement`; the run row and the `retention_enforce` job (`fire_key=retention-run:<id>`, executor from `ExecutorFor`) are inserted in ONE transaction, unlike Python which commits the run first: D5), `GET /retention/runs` (`limit` 20 `1..200`, `offset` `0..100000`, `cursor` max 1024; body `runs, limit, offset, has_more, next_cursor`).

- [ ] **Step 1: Write failing tests**: `TestRetentionRoutesMatchFixtures`, `TestRetentionPutInvalidDays422`, `TestRetentionPutMissingEnabled422Body`, `TestEnforceRequiresExactConfirmation`, `TestEnforceNoPolicy409`, `TestEnforceLegalHold409`, `TestEnforceCreatesRunAndJobAtomically`, `TestEnforceEnqueueFailureLeavesNoRun`, `TestEnforceJobExecutorFollowsFlag`, `TestRunsPaginationOffsetCursor`, `TestRetentionRoutesRequireAdmin`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): retention routes (RED)`, `feat(go): retention policy, preview and runs routes`, `feat(go): retention enforce route`.

### Task 14: RBAC and members routes

**Files:** Modify stub `internal/api/governancerbac.go`; tests; fixtures `testdata/contract/rbac/`

**Interfaces:** Consumes Task 6. Produces `GET /api/governance/rbac`, `PUT /rbac/{user_id}/{permission}` (body `{"effect": "allow"|"deny"|null}`; `user_id` int path -> 422 on non-int), `POST /members` (201; body `username` 1..150, `role` enum `viewer|member|editor|admin`, default `viewer`), `PATCH /members/{user_id}`, `DELETE /members/{user_id}` (204 empty body). Error mapping exact per Task 6 strings.

- [ ] **Step 1: Write failing tests**: `TestRbacRoutesMatchFixtures`, `TestRbacPutEffectInvalid422`, `TestMemberRoutesGuards` (owner, unknown, duplicate), `TestMemberRoleEnumRejectsOwner422`, `TestDeleteMember204EmptyBody`, `TestMembersRequireAdmin403`, `TestOverrideAffectsRequireRoleImmediately` (an override written here changes `identity.RequireRole` for that user on the next request), `TestMemberRoutesAuditMetadataHasNoSecrets`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): rbac and member routes (RED)`, `feat(go): rbac routes`, `feat(go): workspace member routes`.

### Task 15: SSO and SCIM token routes

**Files:** Modify stub `internal/api/governancesso.go`; tests; fixtures `testdata/contract/sso/`

**Interfaces:** Consumes Task 7. Produces `GET /sso` and `PUT /sso` (body `OidcUpdate` defaults as Python; response `{**config, "owner_password_fallback": true, "client_secret_configured": bool}`; `ValueError` -> 422 `detail` string), `GET /scim-token` (`{"configured": bool, "base_path": "/scim/v2/<slug>", ...status}`), `POST /scim-token` (201; `{"token", "base_path", "warning": "Copy this token now; it cannot be retrieved again.", ...status}`), `DELETE /scim-token` (204).

- [ ] **Step 1: Write failing tests**: `TestSSORoutesMatchFixtures`, `TestSSOPutErrorsAre422Strings`, `TestSSOResponseNeverContainsSecret`, `TestSCIMTokenRoutesMatchFixturesIgnoringToken` (`IgnoreShapeOf` for token, timestamps), `TestSCIMRotateThenOldTokenRejectedByScimRoutes` (end-to-end against plan 02 `/scim/v2/{slug}`), `TestSCIMTokenResponseNotLogged`, `TestSSORoutesRequireAdmin`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): sso and scim token routes (RED)`, `feat(go): sso config routes`, `feat(go): scim token routes`.

### Task 16: Playbook CRUD routes

**Files:** Modify stub `internal/api/playbooksroutes.go`; tests; fixtures `testdata/contract/playbooks/` (crud cases)

**Interfaces:** Consumes Tasks 8, 10. Produces `GET /api/research-playbooks` (`current_workspace`), `POST` (201; gate `agents.write`), `PATCH /{id}`, `DELETE /{id}` (204; also `RemoveSchedule`). Create errors: 422 `output_format must be text or json`, 409 `A playbook with this name already exists`. Patch: 404 `Playbook not found`, 404 `Scheduled audience not found`, 422/409 for the Python-500 cases (D3), `ScheduleNext` in the same transaction. Delete: 409 `Cannot delete a playbook with an active run`.

- [ ] **Step 1: Write failing tests**: `TestPlaybookCrudMatchesFixtures`, `TestCreateStepsValidationBodies`, `TestPatchNullName422NotPanic`, `TestPatchNameCollision409`, `TestPatchSchedulesNextRun`, `TestPatchDisableClearsSchedule`, `TestDeleteWithActiveRun409`, `TestDeleteRemovesScheduleMirrorAndPendingJob`, `TestViewerCanListButNotWrite`, `TestPlaybookTenantIsolation404`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): playbook crud routes (RED)`, `feat(go): playbook list, create, patch, delete routes`.

### Task 17: Playbook run routes

**Files:** Modify stub `internal/api/playbookruns.go`; tests; fixtures `testdata/contract/playbooks/` (run cases)

**Interfaces:** Consumes Tasks 8, 11 and `queue.EnqueueOnce`. Produces `POST /{id}/runs` (202; body `audience_id` required, `max_members` default 100 `1..1000`; 404 `Playbook not found`, 409 `Playbook is disabled`, 404 `Audience not found`; run row and `research_playbook_run` job (`fire_key=playbook:<run id>`, executor from `ExecutorFor`) in one transaction, D5), `GET /{id}/runs` (`limit` 50 `1..200`, `offset`, `cursor`; body `runs, limit, offset, has_more, next_cursor`), `GET /runs/{run_id}/results` (`limit` 100 `1..500`, `cursor` max 32; 404 `Playbook run not found`; body `results, limit, offset, has_more, next_cursor`), `POST /runs/{run_id}/retry` (202; 409 `Only failed, completed-with-errors, or cancelled runs can be retried`; sets `audit_metadata {action:"research_playbook.run.retry", playbook_id, run_id, previous_failed}`), `POST /runs/{run_id}/cancel` (200; 409 `Only pending or running playbook runs can be cancelled`; cancels the matching pending/processing job in the same transaction with error `Cancelled by user`; audit keys `action, playbook_id, run_id`). The static `/runs/...` paths must win over `/{id}/runs` (route order test).

- [ ] **Step 1: Write failing tests**: `TestRunRoutesMatchFixtures`, `TestStartRunDisabledAndForeignAudience`, `TestStartRunMaxMembersBounds422`, `TestStartRunCreatesRunAndJobAtomically`, `TestResultsCursorMalformed422`, `TestRetryOnlyTerminalStatesResetsRunAndEnqueues`, `TestRetryDoesNotRerunSuccessResults` (through the engine fake), `TestCancelMarksRunAndJobCancelled`, `TestCancelAfterCompleteIs409`, `TestRetryCancelAuditMetadataKeys`, `TestRoutePrecedenceRunsVsPlaybookId`, `TestRunRoutesTenantIsolation404`.
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): playbook run routes (RED)`, `feat(go): playbook run start, list and results routes`, `feat(go): playbook retry and cancel routes`.

### Task 18: Worker wiring and failure hook

**Files:**
- Create: `internal/retention/handler.go`, `internal/playbooks/handler.go`, tests
- Modify: `cmd/worker/handlers_governance.go`, `cmd/worker/main_integration_test.go` (new cases only)

**Interfaces:**
- Consumes: Tasks 9-11, plan 06 Task 22's dispatch map and `queue.HandBack`.
- Produces: `retention.Handler(d Deps) func(ctx, *queue.Claim) error`, `playbooks.RunHandler(d Deps, r Researcher)`, `playbooks.ScheduleHandler(d Deps)`; each decodes the payload (`workspace_id` required; missing -> error as Python), heartbeats through `queue.Heartbeat`, hands back with `queue.HandBack` when `pyworker` is unavailable (run only), and on a handler error or recovered panic calls the matching reconcile with `willRetry = retry_count < max_retries` read from the job row; `governanceHandlers` returns `retention_enforce`, `research_playbook_run`, `research_playbook_schedule`, each registered only when its executor flag is on. Verify while implementing that Python's `_reconcile_dead_jobs` (queue service) also reconciles Go-claimed jobs whose lease expired (it keys on job type, not executor); if it does not, add a Go reaper call and record the finding in the runbook.

- [ ] **Step 1: Write failing tests**: `TestWorkerRunsRetentionEnforceJob`, `TestWorkerRunsPlaybookRunJob`, `TestWorkerRunsPlaybookScheduleJob`, `TestWorkerHandsBackPlaybookRunWithoutPyWorker`, `TestHandlerErrorInvokesReconcileWithRetryFlag`, `TestPanicIsRecoveredAndReconciled`, `TestLeaseLostStopsPurgeBeforeNextBatch`, `TestFlagsOffLeavePythonJobsUntouched` (Go never claims `executor='python'` rows), `TestDeadGoLeaseReconciled` (documents whichever reaper owns it).
- [ ] **Step 2: Run** `make test-integration` -> FAIL. **Step 3: Implement.** **Step 4: Run** `make vet test test-integration` -> PASS.
- [ ] **Step 5: Commit** `test(go): governance worker handlers (RED)`, `feat(go): worker handlers for retention and playbooks`.

### Task 19: Contract coverage, nginx split, runbook

**Files:**
- Modify: `infra/nginx/go-routes.conf` (one delimited section `# --- governance-playbooks (plan 11) ---`), `scripts/nginx-routing-check.sh` (cases only), `testdata/contract/README.md` (deviations)
- Create: `testdata/contract/retained_governance_routes.txt`, `docs/go-port/runbooks/governance-playbooks-cutover.md`, `internal/api/governance_coverage_test.go`

**Interfaces:** Consumes everything. Produces nginx: `/api/governance/capabilities` and `/api/research-playbooks/capabilities` always `api_backend` (placed first); the other 26 routes follow the canary bucket; `POST /api/governance/retention/enforce` and `POST /api/research-playbooks/*/runs` are sticky per workspace during canary so the run row and job are never split across stacks; `/audit-events/export.csv` sets `proxy_buffering off` and `proxy_read_timeout 300s`. Runbook: prerequisites (plans 02, 03, 06 cutover done, `PY_WORKER_URL` reachable for playbooks, Python scheduler keeps running), flag order (`GO_GOVERNANCE_ROUTES_ENABLED` -> `GO_PLAYBOOK_ROUTES_ENABLED` -> `GO_EXECUTOR_RETENTION` -> `GO_EXECUTOR_PLAYBOOKS`), a dry-run procedure for retention (preview counts equal between stacks before the first enforce), rollback (bucket 0, flags off, in-flight Go jobs finish or are reconciled), and the deviations list (D3-D6).

- [ ] **Step 1: Write failing tests** `TestEveryGovernancePlaybookRouteIsPortedOrRetained` (parses `routers/{governance,playbooks}.py`; 26 ported, 2 retained), `TestRetainedRoutesNotRegisteredInGo`, `TestEveryGovernanceJobTypeHandledOrPython` (the 3 ported job types dispatch, `research_playbook_*` never without the flag), `TestRetentionTargetsCoverEveryOrmModelInPython` (parses `_targets` from the Python file and compares tables).
- [ ] **Step 2: Run** `make test` -> FAIL. **Step 3:** record missing fixtures, write the nginx section, runbook and deviations. **Step 4: Run** `make vet test test-integration` and `scripts/nginx-routing-check.sh` at 0% and 100% (capabilities paths always `py`; enforce and run-start follow the bucket and are sticky) -> PASS.
- [ ] **Step 5: Commit** `test(go): governance and playbook route coverage (RED)`, `feat(infra): nginx per-route split for governance and playbooks`, `docs(go-port): governance and playbooks cutover runbook`.

---

## Self-Review

- **Spec coverage:** inventory row 10 "audit, retention, research playbooks (playbook steps call plan 5 + LLM)": audit (4, 12), retention (5, 9, 13, 18), playbooks (8, 10, 11, 16, 17, 18), LLM steps via `pyworker` (2, 11), the remaining governance routes (rbac, members, sso, scim-token: 6, 7, 14, 15), job types `research_playbook_run`, `research_playbook_schedule`, `retention_enforce` (9, 10, 11, 18), scheduler bootstraps (kept in Python, routed in 2). Route count: governance 17 ported + 1 retained = 18; playbooks 9 + 1 = 10.
- **Consistency:** `govaudit.EncodeCursor/DecodeCursor` (4) used in 5, 8; `retention.ExecutorFor` (5) and `playbooks.ExecutorFor` (10) mirror Task 2; `SchedulePolicy` (5) used in 9, 13; `Researcher` (11) injected in 18; `ResultCounts` (8) used in 11.
- **Review Focus:** 1 -> Tasks 5, 9, 13; 2 -> 9, 10, 11, 17, 18; 3 -> 11; 4 -> 4, 12; 5 -> 6, 7, 14, 15; 6 -> 8, 16.
- **Proportion:** names, error strings, key orders, state machines and test names carry the decisions; bodies are left to the implementer and the recorded fixtures.

## Open decisions (defaults apply unless overridden)

- **D1 Capabilities routes.** Both `/capabilities` routes serve the signed certification catalog (`integrations/certification.py`, HMAC-verified evidence files and build SHA). Default (confirm): retained on Python; port with the integrations plan if wanted.
- **D2 Research step boundary.** Default (confirm): one synchronous `pyworker` call per step with provider `research_column` (Python keeps the LLM, ddgs, fetch and prompt-guard stack; the workspace and spend attribution come from the RPC envelope). Porting `research_column` is not planned.
- **D3 PATCH and validation hardening.** `PATCH` playbook with `null` on non-nullable fields or a name collision answers 500 in Python; Go answers 422 / 409. Documented deviation.
- **D4 Batched purge.** Python deletes each category with one statement and checks the legal hold only at the start. Go deletes in 5000-row batches, re-reads hold and run status before each batch, and fixes cutoffs once per run. Same final counts; documented deviation.
- **D5 Run row and job atomicity.** Python commits the run and then enqueues; a crash between the two leaves an orphan `pending` run (retention enforce, playbook start). Go inserts both in one transaction. Documented deviation.
- **D6 Member removal.** Go also removes the user's SCIM group memberships in the workspace; Python leaves them. Documented deviation.
- **D7 Scheduler.** Default (confirm): the Python scheduler keeps running the `retention` and `playbooks` bootstraps; only the enqueued job's `executor` changes. A Go scheduler loop is a later plan.
- **D8 Retention double-click.** Two `enforce` calls create two runs in Python and Go (idempotent deletes). Kept for parity; a 409 on an active run is a possible later hardening.
- **D9 Rollout.** Default: all four flags false; canary by workspace cohort; retention dry-run comparison (preview counts) before the first Go enforce.

## User decisions

(none yet; the defaults above apply until the user objects)
