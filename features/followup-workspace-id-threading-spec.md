<!-- Auto-generated follow-up mini-spec (2026-06-26). Effort: medium. -->

# Mini-Spec: Thread `workspace_id` into legacy `_execute_tool` / `LeadDB()`

## Goal
Make the legacy chat-agent tool path (`apps/api/routers/copilotkit.py:_execute_tool` and the Autopilot orchestrator that reuses it) **tenant-safe**: every lead/signal/workbook read and write must go through a workspace-scoped store (`PgLeadStore(workspace_id)` on cloud, per-workspace `LeadDB` on self-host) instead of a raw, non-tenant `LeadDB()`. This is the prerequisite for turning on the cloud Autopilot LLM planner (`AUTOPILOT_LLM_PLANNER`) without giving the planner cross-tenant reach.

---

## Current state (grounded, with file:line)

### The chat endpoint has no tenant at all
- `copilot_chat` is mounted with **no auth / no workspace dependency**: `@router.post("")` → `async def copilot_chat(request: Request)` (`apps/api/routers/copilotkit.py:1469-1470`). It is included plainly in `apps/api/main.py:235` with no router-level `Depends`. So `current_workspace_var` is **never set** for a chat request.
- Consequently `_autopilot_ws()` reads the contextvar and returns `None` (`copilotkit.py:305-318`), and the plan store / memory key on `None` (`copilotkit.py:1127-1134`, `1148-1153`).

### `_execute_tool` always uses a raw, non-tenant store
- `async def _execute_tool(name, args)` opens `db = LeadDB()` with no args (`copilotkit.py:678-680`, closed at `1171-1172`). `LeadDB()` with no `db_path` resolves to the **single global file** `config.DB_PATH = data/leads.db` (`apps/api/services/leadgen/db.py:47-54`, `apps/api/services/leadgen/config.py:13`). This is neither the per-workspace SQLite file (`ws_manager.workspace_leads_db_path(slug)`, used by `get_lead_store`, `store.py:121-124`) nor the RLS-protected Postgres store (`PgLeadStore`, `store.py:141`). In **cloud/PG mode the chat tools never touch Postgres at all** — they read/write a local SQLite file shared across every tenant and every request on that process.

Every data tool in `_execute_tool` is therefore non-tenant. Inventory (all in `copilotkit.py`):

| Tool | Lines | Op | Tenancy defect |
|---|---|---|---|
| `search_leads` | 682-702 | read `db.get_leads` | returns rows from all tenants |
| `get_lead_detail` | 704-715 | read `db.get_lead(id)` | IDOR — any lead id, any tenant |
| `get_lead_stats` | 717-719 | read `db.get_stats()` | global counts across tenants |
| `update_lead_status` | 721-723 | **write** `db.update_status` | cross-tenant write |
| `start_collection` | 725-737 | **write** `db.create_job` + thread `JobRunner` | job + sourced leads carry no `workspace_id` |
| `enrich_lead` | 739-792 | read+**write** `get_lead`/`update_lead_fields` | cross-tenant read+write |
| `find_similar_leads` | 794-819 | read | cross-tenant |
| `get_enrichment_gaps` | 821-858 | read (limit 500) | cross-tenant |
| `suggest_outreach` | 860-907 | read | cross-tenant |
| `compare_leads` | 909-932 | read | cross-tenant |
| `ambitionbox_*` | 934-957 | external API | OK (no tenant data) |
| `create_workbook` | 959-1012 | **write** raw sqlite via `workbooks._get_db` | no `workspace_id`; **import is broken — see note** |
| `add_workbook_column` | 1014-1045 | **write** raw sqlite, filter by id only | IDOR; **broken import** |
| `create_source_workbook` | 1048-1081 | **write** `Workbook(...)` via `SessionLocal` | created with `workspace_id=NULL` and no `source_config.workspace_id` → orphaned; downstream sourcing runs with empty workspace |
| `set_workbook_refresh` | 1083-1091 | **write** by `workbook_id` only | IDOR on any workspace's workbook |
| `add_agent_column` | 1093-1108 | **write** `Workbook` filtered by **id only** | IDOR |
| `add_signal_trigger` | 1110-1122 | **write** `Workbook` by id only | IDOR |
| `draft_plan` / `execute_plan` | 1125-1168 | orchestration | `ws_id` is always `None`; `execute_plan` recurses into `_execute_tool` (`autopilot.py:361-408`, invoked at `copilotkit.py:1158`) |

- `_build_system_prompt` also calls a bare `LeadDB().get_stats()` (`copilotkit.py:196-205`) → global pipeline stats leak into the system prompt of every tenant's chat.

### Why workbook tools are extra-dangerous
RLS only protects `leads` and `signals` (migration `c42d0273d9bd`, `migrations/versions/c42d0273d9bd_...py:182-186` enables RLS on `leads`,`signals` only). The `workbooks`/`workbook_rows` tables have **no RLS backstop** — the proper router enforces tenancy purely in app code via `_owned_workbook` (`apps/api/routers/workbooks.py:40-47`). The chat tools `add_agent_column`/`set_workbook_refresh`/`add_signal_trigger` query `Workbook` by id with **no workspace filter**, so in cloud mode the LLM can mutate **any** workspace's workbook by guessing/seeing an id, and nothing at the DB stops it.

### Note: `create_workbook` / `add_workbook_column` appear dead
Both import `from apps.api.routers.workbooks import _get_db as get_wb_db` (`copilotkit.py:996`, `1020`), but `workbooks.py` defines no `_get_db` (it has `_get_lead_db`). That import raises `ImportError` at call time → these two tools always error today. Recommend **deleting** them and their `DANGEROUS_TOOLS`/`_build_tools` entries rather than fixing tenancy on dead code (verify before deleting).

### Building blocks that already exist (reuse, don't reinvent)
- `get_lead_store(workspace_id, slug)` returns `PgLeadStore` or per-workspace `LeadDB`, and publishes the contextvar (`store.py:106-124`).
- `PgLeadStore` mirrors the full `LeadDB` read/write surface used here — `get_leads`, `get_lead`, `get_stats`, `update_status`, `update_lead_fields`, etc. (`store.py:182-409`).
- `workspace_scope(workspace_id)` context manager sets/clears the RLS GUC for a unit of work and **refuses empty workspace_id** (`tenancy.py:110-131`).
- `current_workspace` dependency resolves + authorizes a workspace from JWT + `X-Workspace-Id` (`tenancy.py:73-103`).
- Self-host default workspace: slug `main`, resolvable via `ws_manager._get_active_workspace_id()` (`apps/api/services/workspace/manager.py:90-116`).

---

## Design

Two coordinated changes: (1) **resolve a workspace for the chat request**, and (2) **thread a workspace-scoped store + `workspace_id` through `_execute_tool` and Autopilot**, executing inside `workspace_scope`.

### 1. Resolve the chat workspace (new helper)
Add `_resolve_chat_workspace(request) -> tuple[str, str]` (returns `(workspace_id, slug)`) in `copilotkit.py`:

- **Cloud / multi-tenant** (gate on `settings.PG_LEAD_STORE` true, or a dedicated `CHAT_REQUIRE_AUTH` flag): authenticate exactly like the `current_workspace` dependency — read `Authorization` bearer + `X-Workspace-Id`, resolve the user, enforce `ws_manager.is_member(...)`, resolve slug. **Fail closed**: missing/invalid auth → `401`; non-member workspace → `403`. Never fall through to a default in cloud mode.
- **Self-host** (SQLite, single workspace): no auth required; return `ws_manager._get_active_workspace_id()` and its slug (the `main` default). This preserves today's keyless UX.

This is the single decision point that makes the rest fail-safe.

### 2. New `_execute_tool` signature
```
async def _execute_tool(name, args, *, store, workspace_id, slug) -> str
```
- Replace `db = LeadDB()` (`:680`) with the passed-in `store` (a `PgLeadStore` or per-workspace `LeadDB`). Drop the `db.close()` `finally` for the store object's lifecycle on PG (its `close()` is a no-op, `store.py:525`); keep a `try/except` shape.
- All lead tools now call `store.<method>` — same method names already exist on both backends.
- Workbook ORM tools: keep `SessionLocal()` but (a) run inside `workspace_scope(workspace_id)` so the PG GUC is set, and (b) **explicitly filter and stamp** `workspace_id`:
  - `create_source_workbook`: set `Workbook(..., workspace_id=workspace_id)` and `source_config={"workspace_id": workspace_id}` so the downstream source engine sources into the right tenant (`source_engine.py:83,102`).
  - `add_agent_column` / `set_workbook_refresh` / `add_signal_trigger`: add `.filter(Workbook.workspace_id == workspace_id)` to the lookup (mirror `_owned_workbook`, `workbooks.py:40-47`). Treat a miss as "not found" (don't leak existence).
- `start_collection`: stamp `workspace_id` on the job and pass it to `JobRunner` (`runner.submit(query, workspace_id=...)`, as `source_engine.py:102` already does) so sourced leads land in the tenant.
- `draft_plan` / `execute_plan`: replace `_autopilot_ws()`/`_autopilot_user()` with the resolved `workspace_id` (and the authed user id in cloud). `execute_plan` passes a **partial** that already binds store/ws to the recursive callback (see below).

### 3. Call-chain changes
- `copilot_chat` (`:1469`): call `_resolve_chat_workspace(request)` early; build `store = get_lead_store(ws_id, slug)` once; thread `ws_id/slug/store` into `_stream_chat` and `_resolve_approved_calls`.
- Wrap the actual tool dispatch in `with workspace_scope(ws_id):` at the two execution sites — `_stream_chat` (`:1342`) and `_resolve_approved_calls` (`:1450`) — so PG transactions opened by `PgLeadStore` and by the workbook `SessionLocal()` blocks both get `SET LOCAL app.workspace_id`.
- `autopilot.execute_plan(plan, execute_tool)` is unchanged in signature (`autopilot.py:361`); at the call site (`:1158`) pass a bound callback, e.g. `functools.partial(_execute_tool, store=store, workspace_id=ws_id, slug=slug)`, so Autopilot reuses the exact tenant-scoped tools.
- `_build_system_prompt` (`:172`): take `store` (or `ws_id`) and call `store.get_stats()` so the prompt shows the tenant's stats, not global.

### 4. Backward-compat for self-host (single workspace)
- Self-host returns the `main` workspace from `_resolve_chat_workspace`; `get_lead_store` returns the per-workspace `LeadDB` (`store.py:120-124`). `workspace_scope` is still entered (harmless on SQLite — the GUC hook is a no-op when `IS_SQLITE`). No auth is required, so the existing keyless desktop/self-host chat keeps working unchanged.
- **Migration nuance:** self-host chat currently writes to the **global** `data/leads.db` (`config.DB_PATH`), whereas `get_lead_store("main", "main")` resolves to the per-workspace path. For the default `main` workspace `workspace_leads_db_path("main")` returns `DB_PATH` (`manager.py:259-268` — the canonical workspace IS `DB_PATH`), so existing self-host data stays addressable. Confirm this equivalence in a test before shipping (AC8).

---

## File-by-file changes
- `apps/api/routers/copilotkit.py`
  - Add `_resolve_chat_workspace(request)` (cloud=authed/fail-closed, self-host=`main`).
  - Change `_execute_tool` signature to accept `store, workspace_id, slug`; replace `LeadDB()` with `store`; add workspace filters/stamps to workbook ORM tools; pass `workspace_id` to `start_collection`'s job + `JobRunner`.
  - `_stream_chat` / `_resolve_approved_calls`: thread `store/ws_id/slug`; wrap dispatch in `workspace_scope(ws_id)`.
  - `copilot_chat`: resolve workspace, build store once, thread through.
  - `draft_plan`/`execute_plan`: use resolved `ws_id`/user; pass `functools.partial` callback to `execute_plan`.
  - `_build_system_prompt`: use tenant store stats.
  - Delete dead `create_workbook` / `add_workbook_column` tools + their `DANGEROUS_TOOLS`/`_build_tools`/`_INFO_RESPONSE` entries (after verifying they're dead).
- `apps/api/services/agent/autopilot.py` — no signature change required (callback stays injected); update the docstring (`:361-370`) to note the callback is now tenant-bound.
- (Optional, related but separable) `apps/api/services/workbook/source_engine.py:113` uses a bare `LeadDB()` to read back sourced leads — flagged as out-of-scope follow-up (see below).
- `tests/test_copilotkit_tenancy.py` (new) + a PG-gated cross-tenant case (extend `tests/test_pg_tenancy_rls.py` or add `tests/test_copilotkit_rls.py`).

---

## Tenancy / security
- Single resolution point (`_resolve_chat_workspace`) that **fails closed** in cloud (no silent default) is the core invariant. `workspace_scope` already rejects empty `workspace_id` (`tenancy.py:122-126`), so a resolution bug surfaces as an error, not silent global access.
- Lead/signal tools get **two layers**: app-layer `workspace_id` filter in `PgLeadStore` + RLS backstop. Workbook tools get **app-layer only** (no RLS on `workbooks`) — so the explicit `.filter(workspace_id==...)` is load-bearing; call this out in review and consider a follow-up RLS migration for `workbooks`/`workbook_rows`.
- `PgLeadStore._lead_payload` force-stamps `workspace_id` on writes (`store.py:172-179`), so even a tool that forgot to set it cannot write cross-tenant on PG.

## Idempotency
- No new write semantics introduced; preserve existing idempotency: `create_job` is `INSERT OR IGNORE` (`db.py:541-549`); plan store nonce is single-use (`autopilot_plan_store.consume`, `copilotkit.py:1153`); autopilot memory is idempotent on `(ws, workbook_id)` (`copilotkit.py:1159-1167`). Adding `workspace_id` to the job is additive. The signal/automation idempotency (`fire_key="signal:<pk>"`, `store.py:434-448`) is untouched.

## Failure modes
- **Cloud, no/invalid auth** → 401/403 before any tool runs (was: silent global access). New behavior; the CopilotKit frontend must send `Authorization` + `X-Workspace-Id` in cloud.
- **Workbook lookup miss after adding workspace filter** → returns `{"error":"Workbook not found"}` (same shape as today's not-found, no existence leak).
- **PG role unsafe (super/BYPASSRLS)** → `get_lead_store`→`use_pg_store`→`assert_rls_role` already raises `RlsRoleError` (`store.py:39-103`); chat surfaces it as a tool error instead of running with RLS inert.
- **Self-host** → unchanged keyless flow; `main` workspace, SQLite, `workspace_scope` no-op.
- Long-running `start_collection` thread (`copilotkit.py:730-735`) must set `workspace_scope` **inside** the thread (contextvars don't cross `threading.Thread`), else the JobRunner runs unscoped — handle by passing `workspace_id` explicitly to `JobRunner.submit` (already its contract) rather than relying on the contextvar.

## Feature-flag / rollout
- Reuse existing flags: `PG_LEAD_STORE` (cloud vs self-host backend, `config.py:83`) drives both store selection and the auth-required branch. Optionally add `CHAT_REQUIRE_AUTH` (default = `PG_LEAD_STORE`) to decouple.
- The cloud Autopilot planner stays behind `AUTOPILOT_LLM_PLANNER` (`autopilot.py:53-59`); this refactor is its gate — do not enable the planner in cloud until this lands and the PG cross-tenant test is green.
- Rollout: ship behind self-host-first behavior (no UX change there); enable cloud auth path in staging, run PG-gated test, then production.

---

## Acceptance criteria
1. `_execute_tool` takes `(name, args, *, store, workspace_id, slug)` and contains **no bare `LeadDB()`**; all lead/signal reads/writes go through `store`.
2. `copilot_chat` resolves a workspace via `_resolve_chat_workspace`; in cloud mode an unauthenticated request gets 401 and a non-member `X-Workspace-Id` gets 403 — **no tool executes**.
3. In self-host (SQLite) mode, chat works with no auth, scoped to the `main` workspace, against the same data file as before (no data loss/relocation).
4. Workbook tools (`create_source_workbook`, `add_agent_column`, `set_workbook_refresh`, `add_signal_trigger`) stamp/filter `workspace_id`; `create_source_workbook` sets `Workbook.workspace_id` and `source_config.workspace_id`.
5. `start_collection` writes a job carrying `workspace_id` and sourced leads land in that workspace.
6. Tool dispatch in `_stream_chat` and `_resolve_approved_calls` runs inside `workspace_scope(ws_id)`; `execute_plan` receives a tenant-bound callback (Autopilot tools inherit the same scope).
7. `_build_system_prompt` shows the tenant's stats, not global.
8. Existing offline suites stay green: `PYTHONPATH=. uv run --group dev python -m pytest tests/test_autopilot_planner.py tests/test_chat_interface.py`.
9. **PG-gated cross-tenant test** proves planner/chat tools cannot touch another workspace (details below).
10. Dead `create_workbook`/`add_workbook_column` tools are removed (or, if kept, made tenant-safe with a working DB handle).

---

## Test plan
**Offline (default SQLite, always run):**
- New `tests/test_copilotkit_tenancy.py`:
  - `_execute_tool` called with a `PgLeadStore`-like fake asserts it uses the injected store, never constructs `LeadDB()` (monkeypatch `leadgen.db.LeadDB` to raise).
  - `_resolve_chat_workspace`: self-host returns `main`; cloud with no auth raises 401 (monkeypatch `PG_LEAD_STORE`/auth).
  - Workbook tools reject a `workbook_id` from another `workspace_id` (returns not-found).
  - `execute_plan` runs each step through the bound callback with the right `workspace_id` (assert via a recording fake `execute_tool`).
- Run existing `tests/test_autopilot_planner.py` (AC7/AC9 plan-store isolation already covered) and `tests/test_chat_interface.py`.

**PG-gated (live throwaway PG, `TEST_DATABASE_URL` set; skips otherwise — pattern from `tests/test_pg_tenancy_rls.py:24-31`):**
- New `tests/test_copilotkit_rls.py` (or extend `test_pg_tenancy_rls.py`):
  - Bring schema to head via Alembic; create the **non-super, non-BYPASSRLS** app login role (reuse `APP_LOGIN_ROLE` harness, `test_pg_tenancy_rls.py:32-47`); seed two tenants `W1`, `W2`, each with a lead.
  - Connect as the app role. Resolve chat to `W1` (`store = PgLeadStore("W1")`, inside `workspace_scope("W1")`).
  - Assert via `_execute_tool` with that store:
    - `search_leads` / `get_enrichment_gaps` return only `W1` rows.
    - `get_lead_detail` / `enrich_lead` / `update_lead_status` / `suggest_outreach` against **W2's lead id** return not-found / no-op and **do not** mutate W2 (verify from an owner connection that W2's row is unchanged).
    - `get_lead_stats` counts only `W1`.
  - Workbook IDOR: create a workbook in `W2`; from a `W1`-scoped `_execute_tool("add_agent_column", {workbook_id: <W2 id>})` assert not-found and the W2 workbook config is unchanged.
  - Autopilot end-to-end: bound `execute_plan` for `W1` creates a workbook with `workspace_id=W1` and `source_config.workspace_id=W1`.
- Invocation (never touch the real `yupcha` db):
  `TEST_DATABASE_URL='postgresql+psycopg://user4@localhost:5432/<throwaway>' PYTHONPATH=. uv run --group dev --with 'psycopg[binary]' python -m pytest tests/test_copilotkit_rls.py -q`

---

## Out of scope (flag as follow-ups)
- `source_engine.py:113` bare `LeadDB()` read-back and the broader workbook worker (`worker.py:121` `LeadDB()`) — separate tenancy passes; this item is the chat/agent path. They become correct once `source_config.workspace_id` is set (AC4) but should be hardened in their own change.
- Adding RLS to `workbooks`/`workbook_rows` tables (currently app-layer only) — recommended follow-up migration.
- Per-request CopilotKit frontend auth wiring (sending `Authorization`/`X-Workspace-Id`) is a frontend task this spec depends on for cloud; tracked separately.
- Mem0/`memory.*` and `chat_history.*` are global today (`copilotkit.py:1499-1515`) — not tenant-scoped; out of scope here, note for a future pass.
