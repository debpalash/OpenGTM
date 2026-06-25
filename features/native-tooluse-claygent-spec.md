<!-- Auto-generated design spec (2026-06-25). Review verdict: major-rework. -->

# Production Spec — Native Claude Tool-Use Claygent + LLM Autopilot Planner (v2, review folded in)

## 0. Scope at a glance

Two related migrations, both **default OFF**, both **backward-compatible** (graceful fallback to the existing path when the provider is not Anthropic or the flag is off), both **measurable** via a new offline eval target with an explicit pass/fail gate.

- **(A) Research column ("Claygent")** — replace the hand-rolled ReAct loop in `apps/api/services/workbook/research_column.py` (`execute_research_column`, `:69`; `for _ in range(bound)`, `:96`; `llm.extract_json(...)`-style dispatch; string-dispatch on the decision JSON, `:113-141`) with a **native Claude tool-use manual agentic loop**: real tool definitions (`search`, `fetch`), `tool_choice` to force the first action, structured-output synthesis **with citations**, while **retaining** the prompt-injection guard (`apps/api/services/workbook/prompt_guard.py:139/164`), SSRF guard (`apps/api/services/workbook/output.py:_is_safe_public_url`, `:38`), the ingestion/action lock (`just_ingested_untrusted`, `:94`), a hard **step cap**, and adding a **per-cell cost budget enforced pre-flight (turn + synthesis)**.
- **(B) Autopilot planner** — replace the regex/keyword planner in `apps/api/services/agent/autopilot.py` (`_FIELD_PATTERNS`, `:25`; `_detect_fields`, `:41`; `draft_plan`, `:52`) with an **LLM planner that emits a structured, validated plan** over the available chat tools, while **keeping** the human approval gate (`apps/api/routers/copilotkit.py` `DANGEROUS_TOOLS["execute_plan"]`, `:289`; `_describe_action`, `:305-316`; `_needs_confirmation`, `:333`; replay on `approved_tool_calls`, `:1361-1409`), the **bounded loop** (`execute_plan`, `:96`), and adding lightweight **memory**.

> **Path/line corrections vs the original draft (verified against this tree):**
> - The durable queue is **`apps/api/services/queue_service.py`** plus the worker **`apps/api/services/workbook/worker.py`** — there is no `apps/api/services/workbook/queue_service.py`. Research enrichment runs via the worker, not a `queue_service` contract under `workbook/`.
> - `prompt_guard.py` lives at **`apps/api/services/workbook/prompt_guard.py`** (not `leadgen/`); `guard_untrusted` `:139`, `untrusted_data_system_prompt` `:164` — both real and already imported by `research_column.py:24-25`.
> - Verify-status `cell_metadata` is written via `_set_enrichment` (**`enrichment.py:459-505`**) at the complete branch (**`enrichment.py:417-433`**). `enrich_cell` is at **`:124`**; research branch **`:225-243`**.
> - Alembic head was confirmed by walking the chain: `db9a4b81b1e6 → f3b30195d51a → 3fa508f4ae76 → f7274d0aacd9 → c42d0273d9bd`. **`c42d0273d9bd` IS the current head** and has no children. The new migration chains off it. Re-run `alembic heads` immediately before writing the migration in case the tree advances.
> - `copilotkit.py` real lines: gate config `:289`, `_describe_action` `:305-316`, `_needs_confirmation` `:333`, `draft_plan` handler `:1097-1103`, `execute_plan` handler `:1105-1110`, confirmation emit `:1300`, replay `_resolve_approved_calls` `:1361-1409`, chat endpoint `@router.post("")` `:1412`.

Decision (grounded in the brief): keep **custom tools** running self-hosted `_ddg_search`/`UniversalScraper` behind the SSRF + prompt-injection boundary. Anthropic **server-side `web_search_20260209`/`web_fetch_20260209`** run server-side and **bypass the local SSRF guard + prompt_guard fences**. **Per the review, the server-tool mode (`RESEARCH_SERVER_TOOLS`) is NOT shipped in this release** (see §6/§10/Open Questions) — it is documented as a future, per-workspace, cloud-incompatible option only.

---

## 1. Goal & user-facing behavior

### A. Research column
A research column answers e.g. "Does {company} use Kubernetes? cite a source". User-visible behavior is unchanged in the common case, but:

- The model now emits **real `tool_use` blocks**; tool selection is constrained by `tool_choice`, eliminating the malformed-action / "invalid action" noop path.
- The final answer carries **citations**: the cell value remains the plain answer; a new `cell_metadata.research` payload records `{answer, citations:[{url,title,quoted_text}], steps_used, cost_usd, stopped_reason}`. The web UI renders an "n sources" affordance off `cell_metadata` (same channel the verify-status badge uses — `enrichment.py:417-433/459-505`). `stopped_reason` distinguishes `answered` / `max_steps` / `budget` / `no_answer` / `error` so the badge can render 0 sources gracefully and so the UI never shows a "sources" affordance on a no-answer cell.
- Each cell run is bounded by **both** a step cap (`RESEARCH_MAX_STEPS_CAP`, default 6) **and** a per-cell USD budget (`RESEARCH_CELL_BUDGET_USD`, default 0.05). The budget is enforced **pre-flight with a reserve for the mandatory synthesis call** (see §7) so the true ceiling is the budget, not "budget + one overshoot turn + an uncounted synthesis turn".

Concrete example: column prompt `"Does {company} have SOC 2? Cite the source."` →
1. Model is forced (`tool_choice:{"type":"any"}`) on turn 1 to call `search({query:"{company} SOC 2 compliance"})`. The loop branches on `stop_reason in ("tool_use","end_turn","pause_turn")` (turn 1 is always `tool_use` under forced choice — see §2/edge case 1).
2. Harness runs `_ddg_search`, wraps results via `guard_untrusted(..., label="search result")`, returns a `tool_result`.
3. Model may emit **one or more** `tool_use` blocks (e.g. parallel `search` + `fetch`); the harness executes each, **applies the ingestion lock deterministically by ordering fetches after searches within the turn** (see edge case 2), and returns **all** `tool_result` blocks in a **single** user message.
4. Model returns `end_turn`; harness runs the structured-output synthesis, then **validates every emitted citation URL** against the set the harness actually fetched (see §6) before persisting.

### B. Autopilot
New behavior:

- An **LLM planner** reads the goal, the catalog of available chat tools, and short memory of the user's recent autopilot runs, and emits a validated plan dict **in the exact same shape `execute_plan` already consumes** (`{goal, estimated_rows, steps:[{kind,description,params}]}`).
- The **human approval gate is unchanged in spirit but hardened for integrity**: the drafted plan is **persisted server-side** at draft time and **re-validated + bound by nonce** at execute time, closing the confused-deputy gap where the gate rendered one plan and executed a different client-resubmitted plan (see §6, Blocking Gap #5).
- If the LLM is unavailable / non-Anthropic / flag off / produces an invalid plan, `draft_plan` **falls back to the existing heuristic**.

**Prerequisite (Blocking Gaps #2/#3):** the CopilotKit chat endpoint is made **workspace-aware and authenticated** before (B) ships. (B) consumes per-workspace memory and executes credit-spending plans; an unauthenticated planner widens an already-unauthenticated spend surface. See §6.

---

## 2. Architecture & data flow

### Shared: native tool-use surface on `LLMClient`
`apps/api/services/leadgen/llm.py` `_call_anthropic` (`:385`) never assembles `tools`/`tool_choice`. We add a **new low-level method** (not a change to `complete`/`extract_json`):

- `LLMClient.anthropic_tool_call(...)` — single Messages-API turn with `tools`, optional `tool_choice`, optional structured-output `output_config={"format": {...}}`, returning the **raw response object** (content blocks + `stop_reason` + `usage`). Reuses `_make_anthropic_client` (`:373`), `_anthropic_system_blocks` (`:347`), adaptive thinking (`:413` — `thinking:{"type":"adaptive"}`, the Opus 4.8 contract), and the usage/`record_llm_usage` accounting (`:426-432`).
  - **New contract vs `_call_anthropic`:** `_call_anthropic` concatenates only text blocks. `anthropic_tool_call` returns the **raw response** so callers can append **full `response.content`** to history — preserving `thinking` and `tool_use` blocks unchanged (the API rejects *modified* thinking blocks on the same model; see edge case 4). Callers must never reconstruct or strip these blocks.
- `LLMClient.anthropic_provider()` (`:452`) gates native mode; when None, callers use legacy.
- `LLMClient.anthropic_cost_usd(usage)` — see §7. Priced from the **model registry**, not a hand-maintained table, and **fails closed** (unknown model → priced at the most expensive known tier).

The manual loop lives in the **callers** (per the brief) so the SSRF gate + ingestion lock remain **conditional tool execution**.

### A. Research column flow (native path)
```
enrichment.enrich_cell (col_type=="research", enrichment.py:225)
  └─ execute_research_column(prompt_template, lead_data, columns_config, max_steps,
                            output_format, workspace_id, cell_budget_usd)
       ├─ if not _native_enabled():  → legacy ReAct loop (unchanged code, kept as _run_legacy)
       └─ native loop:
            question   = _sanitized_question(prompt_template, values)   # see Blocking note below
            tools      = [SEARCH_TOOL_DEF, FETCH_TOOL_DEF]
            messages   = [{"role":"user","content": question}]
            tool_choice= {"type":"any"}          # force first action
            ctx        = _ResearchCtx()          # per-cell lock + fetched-URL set + citations
            spent      = 0.0
            synth_reserve = _synthesis_cost_estimate()   # reserve for the mandatory synth call
            for step in range(bound):            # bound = min(max_steps, RESEARCH_MAX_STEPS_CAP)
                if spent + synth_reserve >= cell_budget: break   # PRE-FLIGHT budget check
                resp = llm.anthropic_tool_call(messages, system=_native_system(),
                                               tools=tools, tool_choice=tool_choice,
                                               max_tokens=1024)
                spent += llm.anthropic_cost_usd(resp.usage)      # read FINAL response usage once
                tool_choice = {"type":"auto"}    # only turn 1 is forced
                messages.append({"role":"assistant","content": resp.content})  # FULL content
                if resp.stop_reason == "end_turn": break
                if resp.stop_reason == "pause_turn": ...resend...; continue   # (server-tool mode only; off here)
                # stop_reason == "tool_use": gather ALL tool_use blocks this turn
                tool_uses = [b for b in resp.content if b.type=="tool_use"]
                # Order: run every `search` first, then `fetch`, so the lock is deterministic
                results = []
                for block in _ordered(tool_uses):
                    content, is_error = await _run_tool(block.name, block.input, ctx)  # SSRF + lock
                    results.append(tool_result(block.id, guarded(content), is_error))
                messages.append({"role":"user","content": results})   # ALL results in ONE message
            return await _synthesize_with_citations(messages, ctx, spent)   # structured output
```

**`_run_tool` (conditional tool execution = the security boundary):**
- `search`: `_ddg_search(query, max_results=RESEARCH_SEARCH_RESULTS)`; clears the ingestion lock; records nothing in the fetched-URL set.
- `fetch`: refuses with an `is_error` tool_result if `ctx.just_ingested_untrusted` (mirrors `:145-153`); else `_is_safe_public_url(url)` → **re-validate/pin the resolved IP at connect time (DNS-rebinding mitigation, see §6)** → scrape → cap at `RESEARCH_FETCH_CHARS` → **add the fetched URL to `ctx.fetched_urls`** → set the lock.
- All tool outputs returned to the model are wrapped with `guard_untrusted(...)`; system prompt includes `untrusted_data_system_prompt()`.

**Synthesis** uses a second Messages call with `output_config={"format":{"type":"json_schema","schema":_RESEARCH_SCHEMA}}` producing `{answer, citations:[{url,title,quoted_text}]}`. Its `usage` cost is added to `spent` and recorded in `cell_metadata.research.cost_usd` (so the budget covers it). **Every emitted citation URL is validated** (scheme in `{http,https}` AND membership in `ctx.fetched_urls`) before persistence; non-conforming citations are dropped (logged as `research_citation_rejected`). If structured output is rejected on the model or returns empty/invalid JSON, fall back to a plain-text `complete` synthesis whose cost is **also** counted (and which still passes citations through the same validation; with no structured citations it yields 0 sources, `stopped_reason` set appropriately).

> **Blocking note — research Question injection (edge case "row-data into trusted channel"):** `_resolve_prompt` (`ai_column.py:34`, used at `research_column.py:81`) interpolates row values (company name, etc.) into the Question. Row data can be attacker-controlled (web-sourced). `prompt_guard` only wraps tool *results*, not the resolved Question. **Fix:** `_sanitized_question(...)` runs the interpolated row values through `guard_untrusted(..., label="row data")` for the interpolated *values* (not the trusted template text), so injected instructions inside a company name land in the untrusted-fenced channel. The template text itself stays trusted. Covered by a new test (§11 #21).

Per the API reference, `output_config.format` and document-level `citations` are mutually incompatible, and structured output is also incompatible with prefilling and is model-gated — hence citations are **model-emitted then harness-validated against the fetched set**, never Anthropic document-citation blocks.

### B. Autopilot flow (native path) — with approval integrity
```
POST "" copilot_chat (copilotkit.py:1412)
  └─ NOW: Depends(current_workspace) → sets current_workspace_var (RLS GUC) + enforces membership
  └─ _execute_tool("draft_plan", {goal, target_count}, ws)   (copilotkit.py:1097)
       └─ plan = await autopilot.draft_plan(goal, target_count, workspace_id=ws)
            ├─ if not _native_enabled():  → _heuristic_plan(goal, target_count)
            └─ native:
                 memory = autopilot_memory.recent(ws, limit=3)   # goal text WRAPPED in guard_untrusted
                 plan   = llm.anthropic_tool_call(... output_config.format=_PLAN_SCHEMA ...) → parse
                 plan   = _validate_plan(plan)                    # allowlist kinds + per-kind params; clamp
                 if invalid: → _heuristic_plan(...)
            plan_id, nonce = autopilot_plan_store.put(ws, plan)   # PERSIST server-side at draft time
            return {plan, plan_id, nonce}                          # client echoes plan_id+nonce back
  → _describe_action("execute_plan", {plan_id}) renders describe_plan(stored_plan)   (copilotkit.py:305-316)
  → user approves → client resubmits approved_tool_calls with {plan_id, nonce}
  → _resolve_approved_calls (copilotkit.py:1361):
       stored = autopilot_plan_store.consume(ws, plan_id, nonce)  # nonce single-use; binds to ws
       if not stored: reject (nonce reused / unknown / wrong ws)
       plan = _validate_plan(stored)                              # re-validate at execute time
       result = await execute_plan(plan, _execute_tool)           # bounded loop UNCHANGED internally
       autopilot_memory.record(ws, goal, plan, wb_id, outcome)    # best-effort, idempotent (see §7)
```
The planner **emits** a validated plan via structured output; it never *executes*. `execute_plan` remains the single linear bounded loop. The **plan executed is the plan that was drafted and displayed** because execution reads the **server-stored** plan keyed by `plan_id`, re-validates it, and consumes a **single-use nonce** — the client-resubmitted plan body is ignored for execution (it may still be displayed/echoed for UX, but is not trusted).

### How it rides existing infra
- **Job queue**: research columns run inside `enrich_cell` via the worker (`apps/api/services/workbook/worker.py`) on the durable queue (`apps/api/services/queue_service.py`); native mode changes only the in-process loop, not the queue contract. Autopilot's `execute_plan` still orchestrates `create_source_workbook`/`add_agent_column`/`set_workbook_refresh` chat tools (`autopilot.py:113-132`), which enqueue their own jobs — unchanged.
- **Providers**: native mode keys off `llm.anthropic_provider()` (`:452`); `PROVIDER_ORDER` (`:38`) failover untouched — non-Anthropic defaults use legacy.
- **Billing**: research column gets a **new spend entry** in `vendor_catalog.VENDORS` (`vendor_catalog.py:40`) so `estimate_run_cost` (`:109`) and the credit ledger cover Claygent (today no research entry). See §7 — including the worst-case sizing fix and the product decision on retroactive preview changes.

---

## 3. Data model

The existing `WorkbookCell.cell_metadata` JSON column (used by verify-status) carries research citations — **no new column for (A)**.

### New table: `autopilot_memory` (B only)
Tenant-scoped, RLS-protected on Postgres, **AND app-level workspace-filtered on every query** (SQLite has no RLS — see §6, tenancy issue #3).

| column | type | notes |
|---|---|---|
| `id` | `String` PK | uuid4 |
| `workspace_id` | `String NOT NULL` | RLS key (PG) + explicit `WHERE workspace_id=?` (all backends) |
| `goal` | `Text NOT NULL` | user's original goal (treated as untrusted on read-back) |
| `plan` | `Text` | JSON-serialized plan dict |
| `workbook_id` | `String` | nullable on failure |
| `outcome` | `String` | `ok` / `failed` |
| `created_at` | `Float` | epoch |

Indexes:
- `ix_autopilot_memory_workspace_id` on `(workspace_id)`
- `ix_autopilot_memory_ws_created` on `(workspace_id, created_at)`
- **`uq_autopilot_memory_ws_workbook`** UNIQUE on `(workspace_id, workbook_id)` where `workbook_id IS NOT NULL` — idempotency key so a retried memory write upserts rather than double-inserts (§7).

ORM: add `AutopilotMemory(Base)` in `apps/api/database.py`.

### New table: `autopilot_plan` (B only) — approval integrity store
Holds the server-side drafted plan + single-use nonce so the gate executes exactly what it displayed.

| column | type | notes |
|---|---|---|
| `id` | `String` PK | `plan_id` (uuid4) |
| `workspace_id` | `String NOT NULL` | RLS + explicit filter |
| `user_id` | `String NOT NULL` | the drafting user (re-checked at consume) |
| `plan` | `Text NOT NULL` | JSON plan dict (the displayed plan) |
| `nonce` | `String NOT NULL` | single-use token; cleared/marked on consume |
| `consumed_at` | `Float` | null until consumed; consume is atomic (`UPDATE ... WHERE consumed_at IS NULL RETURNING`) |
| `created_at` | `Float` | epoch; rows expire/are swept after `AUTOPILOT_PLAN_TTL_SEC` (default 3600) |

`consume(ws, plan_id, nonce)` does an **atomic conditional update** so a double-submit (retry, double-click, replay) consumes the nonce once and the second attempt returns nothing → second `execute_plan` is rejected (idempotency, §7, cost issue #5).

ORM: add `AutopilotPlan(Base)`.

### Alembic migration plan
New revision **chained off head `c42d0273d9bd`** (`down_revision='c42d0273d9bd'`), filename e.g. `<rev>_autopilot_memory_plan.py`. **Re-confirm head with `alembic heads` before writing.**
- `op.create_table('autopilot_memory', ...)` and `op.create_table('autopilot_plan', ...)` with columns + indexes above (dialect-agnostic create, like `leads`/`signals`).
- Postgres-only block guarded by `if op.get_bind().dialect.name == "postgresql":` mirroring `c42d0273d9bd._pg_upgrade()`:
  - `GRANT SELECT, INSERT, UPDATE, DELETE ON {table} TO {APP_DB_ROLE}` (role `yupcha_app`).
  - `ALTER TABLE {table} ENABLE ROW LEVEL SECURITY; ... FORCE ROW LEVEL SECURITY;`
  - `CREATE POLICY {table}_workspace_isolation ON {table} USING (workspace_id = current_setting('app.workspace_id', true)) WITH CHECK (...)` — identical fail-closed semantics for **both** tables.
- `downgrade()`: drop policies (PG), drop indexes, drop both tables.

No schema change for (A).

---

## 4. API surface

**No new HTTP endpoints, no new queue job types**, but the existing chat endpoint gains an **auth + workspace dependency** (prerequisite, §6).

- (A) reached via existing cell-enrichment path (`enrichment.enrich_cell`, `:225`) on the existing queue.
- (B) reached via existing CopilotKit chat tools `draft_plan` / `execute_plan`, now under `Depends(current_workspace)` so `current_workspace_var` is set (RLS) and membership is enforced.

Boundary shapes:
- `draft_plan(goal:str, target_count:int=0) -> {plan, plan_id, nonce}` — **additive**: returns `plan_id`+`nonce` alongside the same `plan` dict. Clients that ignore them still render the plan; clients that approve must echo them.
- `execute_plan` is reached only via the gate replay, which now resolves the plan from `autopilot_plan_store` by `(workspace_id, plan_id, nonce)`. The legacy `execute_plan(plan, execute_tool)` signature/return (`{ok, workbook_id, goal, steps_done, message}`) is unchanged.
- Research cell result `-> {success, value, error}` (unchanged), plus `cell_metadata.research` persisted via `_set_enrichment`.

Workspace scoping is now real on both paths: research under the cell's `workspace_id` (threaded to `_set_enrichment`, `enrichment.py:219`); autopilot memory/plan reads/writes under the chat request's resolved `workspace_id` (GUC set by `current_workspace` **plus** explicit `WHERE workspace_id=?`).

---

## 5. File-by-file change list

**Modify `apps/api/services/leadgen/llm.py`**
- Add `async def anthropic_tool_call(self, messages, *, system=None, tools=None, tool_choice=None, output_format=None, max_tokens=1024, prov=None) -> Any`: builds `create_kwargs` like `_call_anthropic` (`:408`) but adds `tools`, `tool_choice`, and `output_config={"format": output_format}` when provided; returns the **raw response**. Keeps `thinking:{"type":"adaptive"}`. Inherits `with_options(timeout=...)` (`:419`) and SDK retries (`max_retries` default 2).
- Add `def anthropic_cost_usd(self, usage) -> float`: prices `input_tokens`/`output_tokens` (+ cache read/write via `_anthropic_cache_tokens`, `:367`) using **per-MTok rates resolved from the model registry / `client.models.retrieve`** keyed on the active model id (cached at startup), with `claude-opus-4-8 = $5/$25` as the verified current value. **Unknown/overridden model → fail closed (price at the most expensive known tier).** Reads usage **only from the final response object, once** (no per-retry accumulation — SDK retries return one final `usage`; this avoids double-counting, §7 cost issue #3).
- Leave `complete`, `extract_json`, `_call_anthropic`, `batch_complete_anthropic` untouched.

**Rewrite `apps/api/services/workbook/research_column.py`**
- Keep `MAX_STEPS_CAP`/`SEARCH_RESULTS`/`FETCH_CHARS` as `RESEARCH_*` env-overridable constants; keep the legacy loop body as `_run_legacy(...)`.
- Add `SEARCH_TOOL_DEF` / `FETCH_TOOL_DEF` (`strict:True`, `additionalProperties:false`, `required` set).
- Add `_native_enabled()`, `class _ResearchCtx` (`just_ingested_untrusted`, `fetched_urls:set`, `citations`), `_sanitized_question(...)` (guards interpolated row values), `_ordered(tool_uses)` (searches before fetches), `async def _run_tool(...)`, `async def _run_native(...)`, `async def _synthesize_with_citations(...)`, `_validate_citation(url, ctx)`, `_RESEARCH_SCHEMA`, `_synthesis_cost_estimate()`.
- Change `execute_research_column(...)` to accept `workspace_id` and `cell_budget_usd`; branch native vs legacy.

**Modify `apps/api/services/workbook/enrichment.py`**
- At the research branch (`:225-243`), pass `workspace_id=workspace_id` and `cell_budget_usd=<col_config or RESEARCH_CELL_BUDGET_USD>`; persist `res.get("metadata")` via `_set_enrichment(..., metadata=cell_metadata)` (`:433`/`:459-505`).

**Modify `apps/api/services/workbook/output.py`**
- `_is_safe_public_url` (`:38`) stays; add an internal helper used by the native `fetch` to **re-validate the resolved IP at connect time / pin the IP** (DNS-rebinding mitigation). Legacy path unaffected.

**Rewrite planner half of `apps/api/services/agent/autopilot.py`**
- Rename current `draft_plan` body to `_heuristic_plan(goal, target_count)`; keep `_FIELD_PATTERNS`/`_detect_fields`.
- Add `AUTOPILOT_TOOL_CATALOG`, `_PLAN_SCHEMA`, `_native_enabled()`, `_validate_plan(plan)`, `async def _draft_plan_llm(goal, target_count, workspace_id)`.
- `_validate_plan` rules (per edge cases): **allowlist of `kind`s** (`create_source_workbook`, `add_agent_column`, `add_research_column`, `set_workbook_refresh`, `report`); **per-kind param allowlist** (reject only params not in that kind's allowlist — do not blanket-reject); clamp `estimated_rows`/`target_rows` to `AUTOPILOT_MAX_ROWS`; **clamp column count and the `rows × column` product** to `AUTOPILOT_MAX_AGENT_COLUMNS` / `AUTOPILOT_MAX_CELLS` (so N columns × M rows can't blow past the row clamp — §6); **do NOT hard-require a leading `create_source_workbook` + trailing `report`** — accept legitimate add-column-to-existing-workbook plans; only require that the plan is non-empty and every step validates.
- Change `draft_plan` to `async def draft_plan(goal, target_count=0, workspace_id=None)`: native → validate → else heuristic.
- `describe_plan`/`execute_plan` internals unchanged.

**New `apps/api/services/agent/autopilot_memory.py`**
- `recent(workspace_id, limit=3) -> list[dict]` and `record(workspace_id, goal, plan, workbook_id, outcome)` via ORM `AutopilotMemory`, on the RLS-scoped session **and** with explicit `.filter(AutopilotMemory.workspace_id == workspace_id)` on every query (SQLite safety). `record` is an **upsert on `(workspace_id, workbook_id)`** (idempotent).

**New `apps/api/services/agent/autopilot_plan_store.py`**
- `put(workspace_id, user_id, plan) -> (plan_id, nonce)` and `consume(workspace_id, user_id, plan_id, nonce) -> plan|None` (atomic single-use), via ORM `AutopilotPlan`, RLS-scoped + explicit workspace filter. Plus a sweep of rows older than `AUTOPILOT_PLAN_TTL_SEC`.

**Modify `apps/api/routers/copilotkit.py`**
- **Add auth/tenancy to `copilot_chat` (`:1412`)**: `Depends(get_current_active_user)` + `Depends(current_workspace)` (or `workspace_scope(ws)` wrapping the handler body) so `current_workspace_var` is set for RLS and membership is enforced. Resolve `ws = workspace.workspace_id`, `user_id = workspace.user.id`. This is the source for the `<request ws>` the original draft referenced but never had.
- `draft_plan` handler (`:1097`) → `await autopilot.draft_plan(args["goal"], int(args.get("target_count",0) or 0), workspace_id=ws)`; then `autopilot_plan_store.put(ws, user_id, plan)`; return `{plan, plan_id, nonce}`.
- `_describe_action` for `execute_plan` (`:313-316`) → render `describe_plan(autopilot_plan_store.peek(ws, plan_id))` (the **stored** plan, not the client's args).
- `_resolve_approved_calls` (`:1361`) → for `execute_plan`, `consume(ws, user_id, plan_id, nonce)`, re-validate, execute; reject on miss/reuse/wrong-ws/wrong-user. No change to the gate's `DANGEROUS_TOOLS` membership.
- `_execute_tool` (`:651`): the legacy `db = LeadDB()` is **not tenant-scoped**. For this release, document the limitation and ensure every tool the autopilot plan invokes is scoped via `current_workspace_var` / passes `ws` where the underlying store supports it. **Open question** flags the deeper `LeadDB()` scoping debt (predates this spec; amplified by the planner).

**Modify `apps/api/database.py`** — add `AutopilotMemory(Base)` and `AutopilotPlan(Base)`.

**New migration `migrations/versions/<rev>_autopilot_memory_plan.py`** — chained off `c42d0273d9bd` (§3).

**Modify `apps/api/services/workbook/vendor_catalog.py`** — add a `research`/`claygent` entry to `VENDORS` (`:40`); `base_cost` sized to the **worst-case** per-cell spend (budget + synthesis reserve), not just `RESEARCH_CELL_BUDGET_USD` (§7).

**New eval target `apps/api/services/leadgen/enrichment/eval/research_eval.py`** (+ CLI wiring in the existing eval cli) — see §11, with a defined regression threshold.

---

## 6. Tenancy & security

### Blocking prerequisites (must land before/with B)
- **Blocking Gap #2/#3 — chat endpoint auth + tenancy.** `copilot_chat` (`copilotkit.py:1412`) currently takes a raw `Request` with **no auth dependency and never sets `current_workspace_var`**. Consequences verified in this tree: (a) `autopilot_memory.recent()` returns zero rows always under PG RLS (GUC unset → fail-closed), so memory silently never works; (b) `autopilot_memory.record()` INSERT fails the `WITH CHECK` because the GUC is NULL. **Fix:** add `Depends(get_current_active_user)` + `Depends(current_workspace)` (from `apps/api/core/tenancy.py:73`), which both enforces workspace membership and publishes `current_workspace_var` for the `after_begin` RLS hook (`database.py:53-70`). This also closes the unauthenticated-spend surface (`_execute_tool` creates workbooks / spends credits). This is a **hard prerequisite** for (B).
- **Blocking Gap #5 — approval/replay integrity (confused deputy).** `_describe_action` renders `describe_plan(fn_args["plan"])` while `_resolve_approved_calls` executes the **client-resubmitted** `fn_args` (`copilotkit.py:1387-1409`) with no binding that approved == displayed. With an LLM emitting variable plans this is materially worse. **Fix:** persist the drafted plan server-side (`autopilot_plan` table), render and execute from the **stored** plan keyed by `plan_id`, bind execution to a **single-use nonce**, and **re-validate (`_validate_plan`) at execute time**. The client-supplied plan body is never trusted for execution.

### Standard controls
- **Workspace isolation / RLS + app-level filter**: `autopilot_memory` and `autopilot_plan` get ENABLE+FORCE RLS + fail-closed `current_setting('app.workspace_id', true)` policies (PG), granted to `yupcha_app` (NOBYPASSRLS). **Because `database.py:9-11` only registers the GUC hook `if not IS_SQLITE`, SQLite installs have NO RLS** — so `recent()/record()/put()/consume()` **also** apply an explicit `WHERE workspace_id=?` (tenancy issue #3). Research citations live in tenant-scoped `WorkbookCell.cell_metadata`.
- **SSRF**: native `fetch` is gated by `_is_safe_public_url` (`output.py:38`) **before** scraping (conditional tool execution). **DNS-rebinding TOCTOU (tenancy issue #5):** `_is_safe_public_url` resolves DNS, then `UniversalScraper().scrape(url)` resolves again — a rebinding attacker can pass the check then hit a private IP. **Fix:** pin the resolved public IP and connect to it (or re-validate at connect time) in the native fetch path.
- **Citation URL validation (tenancy issue #6 / edge case "model can hallucinate a url"):** the SSRF boundary applies at fetch time, not to what the model writes into the citations JSON. **Every emitted citation URL is validated before persistence**: scheme ∈ `{http,https}` (reject `javascript:`, `data:`, internal schemes) **AND** the URL must be in `ctx.fetched_urls` (the harness-vetted set). Non-conforming citations are dropped, not stored, so the UI never renders an unvetted/clickable internal URL. Covered by §11 #22.
- **Prompt injection**: every tool result is `guard_untrusted(...)`-wrapped (`prompt_guard.py:139`); system prompt carries `untrusted_data_system_prompt()` (`:164`); the **ingestion/action lock** is preserved and made deterministic under parallel tool_use. Native `tool_choice` removes the free-text action-parse surface. **The resolved research Question is sanitized** (interpolated row values fenced) so attacker-controlled row data cannot inject into the trusted channel (§2.A blocking note, §11 #21). The autopilot planner consumes **only** the trusted goal + static tool catalog + the workspace's own prior plans; **memory `goal` text is treated as untrusted and wrapped with `guard_untrusted` in the planner prompt** (tenancy issue #2) so a poisoned prior goal cannot bias drafting; `_validate_plan` rejects any non-allowlisted `kind`/param; and the **server-bound human gate approves the displayed plan before execution**.
- **Secrets**: Anthropic key via existing `PROVIDER_CONFIG`/`_make_anthropic_client` (`llm.py:373`); no new secret handling. Server-tool mode adds no key — and is not shipped this release.
- **Server tools NOT shipped (tenancy issue #1):** `RESEARCH_SERVER_TOOLS` (Anthropic `web_search_20260209`/`web_fetch_20260209`) bypasses both the SSRF guard and `prompt_guard` and, when flipped, silently turns an SSRF-protected install into an Anthropic-server-side fetcher that can reach URLs already in the conversation and returns un-guarded content. **Per the review it is removed from this release.** If reintroduced later it must be **per-workspace (not a global env var)** and **hard-fail when combined with any cloud/multi-tenant deployment marker**. (Open question.)
- **`_execute_tool` legacy store scoping (tenancy issue #4):** `_execute_tool` builds `db = LeadDB()` with no workspace arg; executed plan tool calls are not tenant-scoped at the legacy data layer. This predates the spec; the LLM planner amplifies it. The auth/RLS prerequisite scopes the request, but the legacy `LeadDB()` store itself remains a debt — flagged as an open question for a follow-up.
- **Abuse limits**: per-cell step cap + pre-flight USD budget; autopilot `_validate_plan` clamps `target_rows`/`estimated_rows`, **column count, and the rows×columns cell product** so an LLM can't draft a huge run within the row clamp by adding many agent/research columns.

---

## 7. Cost control

- **Spend ceiling / credit ledger**: add a `research` vendor to `vendor_catalog.VENDORS` (`:40`). **`base_cost` is sized to the worst case** — `RESEARCH_CELL_BUDGET_USD` + the synthesis reserve (not just the budget) — so `estimate_run_cost` (`:109`) and the credit projection do **not** under-estimate (cost issue #1). `estimate_run_cost` and billing then include research columns in the pre-run preview and deduction.
- **Per-cell cost budget (new, enforced correctly):** the native loop accumulates `llm.anthropic_cost_usd(resp.usage)` per turn and does a **pre-flight check** `if spent + synth_reserve >= cell_budget: break` **before** issuing the next turn, where `synth_reserve` is a reserve for the mandatory synthesis call. **The synthesis call's cost is counted** into `spent` and reported. The **plain-text fallback synthesis** (on bad JSON) cost is also counted. Net true ceiling ≈ `cell_budget` (the reserve absorbs the synth turn; a single forced/adaptive turn cannot push *final* spend past budget because the next turn is gated pre-flight). This closes "budget + one overshoot turn + uncounted synthesis" (Blocking Gap #4, cost issues #1).
- **Pricing source of truth (cost issue #2):** `anthropic_cost_usd` prices from the **model registry**, cached at startup, **failing closed** on unknown/overridden `ANTHROPIC_MODEL` (treat as most-expensive tier → premature stop, never runaway). The credit ledger and the budget therefore read the same rates. Tested via §11 #23.
- **Usage double-count under retry (cost issue #3):** the SDK auto-retries 429/5xx; budget accounting reads usage **only from the final returned response, once** — never per-attempt — so a retried turn neither inflates (premature stop) nor under-counts `spent`.
- **Idempotency / no double-charge**:
  - Research output columns are run-once-guarded by the engine (`output.py` docstring; `enrich_cell` run_once). Native mode is a re-implementation of an existing cell computation — no new external send.
  - **Autopilot memory write** is idempotent: `record` upserts on the UNIQUE `(workspace_id, workbook_id)` key (cost issue #4) and is best-effort (a failed insert never re-runs the plan). With the auth prerequisite, the write actually succeeds (no longer a 100% failure masked as "best-effort").
  - **Double-execution of `execute_plan` (cost issue #5):** the **single-use nonce** consumed atomically in `autopilot_plan_store.consume` means a resubmitted/retried/double-clicked `approved_tool_calls` runs `execute_plan` **once** — the second consume returns nothing and execution is rejected. No two workbooks, no double credit spend.
- **Rate limits**: SDK retries 429/5xx with backoff; `anthropic_tool_call` inherits `with_options(timeout=...)`; usage flows through `usage.add` + `record_llm_usage` for observability.
- **Billing preview change is a product decision (cost issue #6):** adding the `research` vendor raises pre-run spend previews for **existing** workbooks with research columns. This is a correctness fix but a visible UX change. **Open question** for the human owner: gate behind a flag / communicate / distinguish historical-uncosted runs before enabling in cloud.

---

## 8. Failure modes & edge cases

| # | Failure | Handling |
|---|---|---|
| 1 | Anthropic not default provider | `_native_enabled()` false → legacy ReAct (A) / heuristic plan (B). |
| 2 | Flag off | Same legacy fallback. |
| 3 | `anthropic` SDK missing | `anthropic_tool_call` raises `RuntimeError` (like `_call_anthropic`, `:400`); callers catch → legacy. |
| 4 | Unknown tool name in `tool_use` | `tool_result` with `is_error:true` "unknown tool"; loop continues. |
| 5 | `fetch` to private/loopback/metadata IP | `_is_safe_public_url` rejects → `is_error` tool_result; URL never scraped. |
| 6 | DNS rebinding (pass check, then private IP) | Resolved IP pinned / re-validated at connect time; rebind to private → fetch fails (§6). |
| 7 | `fetch` immediately after a page read | Ingestion lock → `is_error` "cannot fetch immediately after reading a page". |
| 8 | **Parallel tool_use in one turn** | All blocks executed; **searches ordered before fetches** so the lock is deterministic; **all `tool_result`s returned in ONE user message**. |
| 9 | `tool_choice:any` forces turn 1 | Loop switches to `auto` **before** evaluating `end_turn`; branches on `stop_reason=="tool_use"` explicitly; the turn-1 `end_turn` check is never reached prematurely. |
| 10 | Poisoned page injecting instructions | `guard_untrusted` fences; trust-boundary system clause; no free-text action surface. |
| 11 | **Injection via resolved Question (row data)** | Interpolated row values fenced via `guard_untrusted` in `_sanitized_question`. |
| 12 | Step cap reached | Break → synthesis from notes (`stopped_reason:"max_steps"`). |
| 13 | Cost budget reached (pre-flight) | Break before next turn, synth still funded by reserve (`stopped_reason:"budget"`). |
| 14 | `pause_turn` (server tools only — off this release) | Re-send bounded by `max_continuations` (5), then synthesize. |
| 15 | Synthesis empty/invalid JSON | Fall back to plain `complete` synthesis (cost counted); `stopped_reason:"no_answer"` if still empty. |
| 16 | **Empty/zero-result research** | `stopped_reason` distinguishes `no_answer` from `budget`/`max_steps`; `citations:[]`; UI "n sources" badge handles 0 (no affordance). |
| 17 | **Model emits hallucinated/internal/non-http citation url** | Citation validation (scheme + fetched-set membership) drops it before persistence. |
| 18 | `_ddg_search`/scraper throws | Caught, non-fatal tool_result ("no results"/"fetch error"); loop proceeds. |
| 19 | LLM planner returns malformed/invalid plan | `_validate_plan` fails → `_heuristic_plan`; never blocks the chat turn. |
| 20 | Planner invents dangerous/unknown `kind` or param | `_validate_plan` rejects (allowlist of kinds + per-kind params) → fallback. |
| 21 | Planner absurd row count / many columns | Clamp rows to `AUTOPILOT_MAX_ROWS`; clamp column count + rows×columns product. |
| 22 | Legitimate add-column-to-existing-workbook plan | Accepted — `_validate_plan` does **not** force leading-create/trailing-report. |
| 23 | Memory read fails (RLS/DB) | `recent()` returns `[]`; planner proceeds. |
| 24 | Memory write fails / retried | Upsert on `(ws, workbook_id)`; best-effort; no re-run, no double-insert. |
| 25 | Cross-tenant memory/plan attempt | RLS fail-closed (PG) **and** explicit `WHERE workspace_id=?` (SQLite); GUC now set by auth dep. |
| 26 | **Double-submit of approved plan** | Single-use nonce consumed atomically → second execute rejected (one workbook). |
| 27 | **Client resubmits a mutated plan** | Execution reads server-stored plan by `plan_id` + re-validates; client body ignored for execution. |
| 28 | Concurrent cells in a column | Each cell has its own `_ResearchCtx` — no shared lock/citation state. |
| 29 | Anthropic 429/5xx | SDK retries; final-response usage read once; on exhaustion turn raises → caught → fallback/notes synthesis. |
| 30 | `output_config.format` rejected on model | Caught → plain `complete` synthesis. |
| 31 | Unknown/overridden `ANTHROPIC_MODEL` pricing | `anthropic_cost_usd` fails closed (most-expensive tier) → conservative early stop, never runaway. |

---

## 9. Observability

- **Token/cost**: every `anthropic_tool_call` flows through `usage.add(...)` + `db.record_llm_usage(...)` (`llm.py:426-432`). Per-cell `cost_usd`, `steps_used`, `stopped_reason` stored in `cell_metadata.research`.
- **Run trace**: `cell_metadata.research` records `{steps_used, cost_usd, stopped_reason, citations}`.
- **Autopilot**: `autopilot_memory` rows are the audit trail (goal → plan JSON → workbook_id → outcome). `autopilot_plan` rows record drafted-vs-consumed for approval-integrity audit. `execute_plan` returns `steps_done`.
- **Logs**: keep `logging.getLogger("workbook.research")` (`:28`); add structured logs for native-vs-legacy selection, each tool decision, SSRF/lock/rebind refusals, budget stop, **citation rejections**, **question sanitization**. Add `autopilot` logger for native-vs-heuristic selection, validation rejections, **nonce reuse/plan-mismatch rejections**, memory read/write outcome.
- **Metrics**: `research_native_runs`, `research_legacy_fallbacks`, `research_budget_stops`, `research_ssrf_blocks`, `research_rebind_blocks`, `research_citation_rejected`, `research_question_sanitized`, `autopilot_llm_plans`, `autopilot_heuristic_fallbacks`, `autopilot_plan_validation_failures`, `autopilot_nonce_reuse_blocked`, `autopilot_plan_mismatch_blocked`.

---

## 10. Feature flags / config / safe rollout

All read via the existing settings/env layer (`llm._read_setting`, `:67`), native paths **default OFF**:

| flag / config | default | effect |
|---|---|---|
| `RESEARCH_NATIVE_TOOLS` | `0` (off) | enable native tool-use research; else legacy ReAct |
| `RESEARCH_MAX_STEPS_CAP` | `6` | hard step ceiling |
| `RESEARCH_CELL_BUDGET_USD` | `0.05` | per-cell LLM cost ceiling (pre-flight, incl. synthesis reserve) |
| `RESEARCH_SEARCH_RESULTS` | `5` | search results per query |
| `RESEARCH_FETCH_CHARS` | `1500` | page text cap fed to the model |
| `RESEARCH_PROMPT_GUARD` | `1` (on) | existing guard toggle (`prompt_guard.py:95`) — unchanged |
| `AUTOPILOT_LLM_PLANNER` | `0` (off) | enable LLM planner; else heuristic |
| `AUTOPILOT_MAX_ROWS` | `500` | clamp planner `target_rows`/`estimated_rows` |
| `AUTOPILOT_MAX_AGENT_COLUMNS` | `8` | clamp number of agent/research columns in a plan |
| `AUTOPILOT_MAX_CELLS` | `4000` | clamp rows × (agent+research columns) product |
| `AUTOPILOT_PLAN_TTL_SEC` | `3600` | drafted-plan store TTL |
| `ANTHROPIC_MODEL` | `claude-opus-4-8` | existing (`:48`) — model both paths use ($5/$25 per MTok) |

**`RESEARCH_SERVER_TOOLS` is intentionally NOT shipped** in this release (§6, tenancy issue #1). Removed from config; documented in Open Questions for any future reintroduction (per-workspace, cloud-incompatible).

Rollout: ship dark (flags off) → land the chat-endpoint auth/tenancy prerequisite + approval-integrity store → enable `RESEARCH_NATIVE_TOOLS` in staging, run the eval target against the legacy baseline and require the §11 threshold to pass → enable `AUTOPILOT_LLM_PLANNER` in staging → cloud-enable per-workspace once eval parity holds **and** the billing-preview product decision (§7) is made. Self-host installs default to legacy (no behavior change on upgrade).

## 11. Test plan

External calls mocked: stub `LLMClient.anthropic_tool_call` to return canned response objects (fake `content` blocks with `.type`/`.name`/`.input`/`.id`, `.stop_reason`, `.usage`); stub `_ddg_search` and `UniversalScraper.scrape`; `_is_safe_public_url` exercised against real IP literals (local DNS only, deterministic).

**Unit (no PG, no network)**
1. `test_native_search_then_answer` — `tool_use`(search) then `end_turn`; cell value + one citation. → AC1.
2. `test_tool_choice_forces_first_action_then_auto` — turn 1 `tool_choice={"type":"any"}`, subsequent `auto`; assert loop switches to `auto` before evaluating `end_turn` and branches on `stop_reason=="tool_use"`. → AC1, edge cases 1/9.
3. `test_parallel_tool_use_batched_and_ordered` — one assistant turn with `search`+`fetch` blocks; assert both executed, searches ordered before fetches, and **all** `tool_result`s returned in a single user message. → AC1/AC4, edge case 8.
4. `test_fetch_ssrf_blocked` — `fetch` to `http://169.254.169.254/...` returns `is_error`; scraper never called. → AC4.
5. `test_fetch_dns_rebind_blocked` — resolver returns public then private IP; pinned/re-validated connect rejects. → AC4, edge case 6.
6. `test_ingestion_lock` — `fetch` immediately after a prior `fetch` refused. → AC4.
7. `test_prompt_guard_wraps_results` — tool_result content contains the untrusted fence. → AC4.
8. `test_question_injection_sanitized` — a row value containing injection markers is fenced via `guard_untrusted` in the resolved Question; trusted template text is not. → AC4, edge case 11.
9. `test_step_cap` — model never answers; loop stops at cap → synthesis path, `stopped_reason:"max_steps"`. → AC2.
10. `test_cost_budget_preflight_includes_synthesis` — usage priced so `spent+synth_reserve>=budget` after 1 turn → stops with `stopped_reason:"budget"`; assert **cumulative** cost (turns + synthesis + any fallback synthesis) is ≤ a defined ceiling and the synthesis cost is counted. → AC2, Blocking Gap #4.
11. `test_anthropic_cost_unknown_model_fails_closed` — override `ANTHROPIC_MODEL` to an unknown id → priced at most-expensive tier → budget trips early (not runaway). → AC2, cost issue #2.
12. `test_usage_read_once_under_retry` — simulated retry returns one final `usage`; assert `spent` counts it once. → cost issue #3.
13. `test_native_disabled_falls_back_to_legacy` — flag off → legacy path; output unchanged. → AC3.
14. `test_synthesis_with_citations_schema` — structured output parsed into `{answer,citations}`; `cell_metadata.research` populated. → AC1.
15. `test_synthesis_fallback_on_bad_json` — invalid structured output → plain `complete` synthesis; cost counted. → AC8.
16. `test_citation_validation_rejects_unfetched_and_bad_scheme` — synthesis returns a citation `url` never fetched, an internal IP, and a `javascript:` scheme → all rejected before persistence; only fetched-set http(s) URLs survive. → AC4, tenancy issue #6.
17. `test_zero_result_no_answer_badge` — no results → `stopped_reason:"no_answer"`, `citations:[]`; assert UI metadata is 0-source-safe. → AC1, edge case 16.
18. `test_full_response_content_echoed` — assistant `thinking`/`tool_use` blocks appended unchanged to history (not reconstructed/stripped). → AC1, edge case 4.
19. `test_autopilot_llm_plan_shape` — stubbed planner returns a valid plan; `draft_plan` returns SAME dict shape `execute_plan` consumes (plus `plan_id`/`nonce` envelope). → AC5.
20. `test_autopilot_validate_rejects_unknown_kind_and_param` — `kind:"delete_everything"` and an unknown per-kind param → `_validate_plan` fails → heuristic fallback. → AC6.
21. `test_autopilot_clamps_rows_columns_and_product` — `target_rows:1_000_000` clamped to `AUTOPILOT_MAX_ROWS`; many agent columns clamped to `AUTOPILOT_MAX_AGENT_COLUMNS`; rows×columns clamped to `AUTOPILOT_MAX_CELLS`. → AC6, edge case 21.
22. `test_autopilot_accepts_add_column_to_existing_workbook` — a plan with no leading `create_source_workbook`/trailing `report` validates. → AC6, edge case 22.
23. `test_autopilot_memory_goal_treated_untrusted` — a poisoned prior goal is wrapped with `guard_untrusted` in the planner prompt. → AC6, tenancy issue #2.
24. `test_autopilot_disabled_falls_back_to_heuristic` — flag off → `_detect_fields` plan. → AC3.
25. `test_describe_plan_and_execute_plan_internals_unchanged` — gate + bounded loop contract intact for heuristic and LLM plans. → AC7.
26. `test_approval_executes_stored_plan_not_client_body` — draft benign plan; resubmit `approved_tool_calls` with a **mutated** plan body but the real `plan_id`/`nonce`; assert the **stored** plan is executed and the mutated body ignored. → AC7, Blocking Gap #5.
27. `test_nonce_single_use_blocks_double_submit` — submit identical `approved_tool_calls` twice; assert exactly one `execute_plan` runs (one workbook) and the second is rejected. → AC7, cost issue #5.
28. `test_plan_consume_wrong_workspace_or_user_rejected` — consume with a different ws/user → rejected. → AC9.

**SQLite-backend isolation (default backend, no RLS)**
29. `test_autopilot_memory_sqlite_isolation` — under SQLite, `recent(workspace_A)` never returns workspace_B rows (relies on explicit `WHERE workspace_id=?`, since SQLite has no RLS). **Designed to fail without the app-level filter.** → AC9, tenancy issue #3.
30. `test_autopilot_plan_sqlite_isolation` — same for `autopilot_plan` consume/peek. → AC9.

**Integration (PG-gated, `TEST_DATABASE_URL`)**
31. `test_autopilot_memory_rls_isolation` — write under workspace A's GUC; read under B returns nothing; under A returns it. → AC9.
32. `test_autopilot_memory_migration_head` — `alembic upgrade head` creates `autopilot_memory` **and** `autopilot_plan` with RLS enabled+forced and policies present (`pg_policies`). → AC9.
33. `test_research_cell_metadata_persisted` — full `enrich_cell` research path writes `cell_metadata.research`. → AC1.

**Auth / tenancy on the chat path (drives the real request context)**
34. `test_chat_endpoint_requires_membership` — a non-member cannot `draft_plan`/`execute_plan` against another workspace (403). → AC9, Blocking Gap #3.
35. `test_draft_then_record_through_chat_context_succeeds` — drive `draft_plan` + `record` through the **real** copilotkit request context (auth dep sets the GUC; no manually-set GUC); assert memory write succeeds and is readable. **This test would FAIL against the pre-fix endpoint, proving the prerequisite is in place.** → AC9, Blocking Gap #2.

**Eval harness (with a gate)**
36. `research_eval.py` target + CLI: fixtures of `(prompt, expected-answer-contains, expected-citation-domain)` through `execute_research_column` with mocked search/fetch (deterministic), scoring correctness + citation-presence for **both** native and legacy. **Regression gate:** native must achieve correctness ≥ legacy baseline − `EVAL_REGRESSION_TOLERANCE` (default 0; configurable) **and** citation-presence ≥ a fixed floor (e.g. ≥ 0.9 on cases expected to cite); the CLI exits non-zero if the gate fails, so §10's "compare to baseline" can actually block a rollout. → AC10.
37. Autopilot planner eval: fixture goals → assert the stubbed LLM plan validates and matches expected step kinds; compare against the heuristic plan for the same goal. → AC10.

## 12. Acceptance criteria

(Listed in the dedicated acceptance_criteria field; AC1–AC10 referenced above, with AC4 covering SSRF + injection + citation validation, AC7 covering gate integrity + nonce, AC9 covering RLS **and** SQLite **and** chat-path auth, AC10 covering a gated eval.)

### Out of scope
- Changing `complete`/`extract_json`/`batch_complete_anthropic` semantics or the OpenAI-compatible path.
- Migrating `agent_column` (Pillar 4) or the MCP server (`apps/mcp/server.py`) to native tool-use.
- New HTTP endpoints / new queue job types / UI work beyond rendering the existing `cell_metadata` channel and echoing `plan_id`/`nonce`.
- Anthropic server-side document `citations` blocks (incompatible with structured output; bypass SSRF).
- **`RESEARCH_SERVER_TOOLS` server-side web tools** — deferred entirely (Open Questions).
- Multi-turn autopilot conversations / re-planning loops; one plan per approval.
- A full rework of the legacy `LeadDB()` tenant scoping in `_execute_tool` (debt flagged in Open Questions; the auth/RLS prerequisite scopes the request, the legacy store remains).

---

## Changes after review
- **Corrected fabricated/mislocated infra:** `queue_service.py` is at `apps/api/services/queue_service.py` + worker `apps/api/services/workbook/worker.py` (no `workbook/queue_service.py`); `prompt_guard.py` is under `workbook/` not `leadgen/`; verify-status / `_set_enrichment` lines re-derived (`enrichment.py:417-433/459-505`); copilotkit gate/replay lines re-derived (`:305-316`, `:1361-1409`, `:1412`). **Confirmed `c42d0273d9bd` is the alembic head** by walking the down_revision chain; migration chains off it (re-verify before writing).
- **Closed Blocking Gap #2/#3 (auth/tenancy):** added `Depends(get_current_active_user)`+`current_workspace` to `copilot_chat`, which sets `current_workspace_var` (RLS) and enforces membership — making memory reads/writes actually work and removing the unauthenticated-spend surface. Added tests #34/#35 (the latter fails against the unfixed endpoint).
- **Closed Blocking Gap #5 (approval integrity):** drafted plans persisted in a new `autopilot_plan` table; gate renders + executes the **stored** plan by `plan_id`, re-validates at execute time, and binds to a **single-use nonce**. Tests #26/#27/#28.
- **Closed Blocking Gap #4 (budget):** pre-flight budget check including a synthesis reserve; synthesis (and fallback-synthesis) cost counted; usage read once from the final response. Tests #10/#12.
- **Fixed cost integrity:** pricing from the model registry, fail-closed on unknown model; vendor `base_cost` sized to worst case; memory upsert idempotency key; nonce dedupe for `execute_plan`; billing-preview change escalated to an Open Question. Tests #11/#27.
- **Edge cases folded in:** forced-`tool_choice`→`auto` ordering + `stop_reason=="tool_use"` branch; parallel tool_use batched into one user message with deterministic search-before-fetch ordering; full `response.content` (thinking/tool_use) echoed unchanged; research Question sanitization for row-data injection; `_validate_plan` allowlist per-kind params + clamp columns and rows×columns product + accept add-to-existing-workbook plans; zero-result `no_answer` distinction. Tests #2/#3/#8/#18/#21/#22/#17.
- **Security:** citation URLs validated (scheme + fetched-set) before persistence; DNS-rebinding pin/re-validate at connect time; memory `goal` text guarded as untrusted in the planner prompt; `autopilot_memory`/`autopilot_plan` get explicit app-level `WHERE workspace_id=?` for SQLite (no RLS there). Tests #16/#5/#23/#29/#30.
- **Removed `RESEARCH_SERVER_TOOLS`** from this release (latent foot-gun); documented future per-workspace, cloud-incompatible reintroduction.
- **Eval gate:** defined a concrete pass/fail threshold so §10's "compare to baseline" can block rollout (test #36).
- **Test gaps closed:** added the SQLite-isolation tests, the real-chat-context memory test, the approval-integrity/mutated-plan and double-submit tests, citation-validation test, parallel-tool-use test, question-injection test, unknown-model pricing test, and the eval threshold gate.

---

## Open questions for the owner

1. Billing preview regression (cost issue #6): adding the `research` vendor to `vendor_catalog.VENDORS` raises pre-run spend previews and credit projections for EXISTING workbooks that already have research columns. Do we (a) gate the vendor entry behind a flag and roll out per-workspace, (b) communicate the change to users first, and/or (c) distinguish previously-uncosted historical runs from new ones in the UI? Needs a product call before cloud enablement.
2. RESEARCH_SERVER_TOOLS (Anthropic server-side web_search_20260209 / web_fetch_20260209) is removed from this release because it bypasses both the SSRF guard and prompt_guard. If/when reintroduced: confirm it must be per-workspace (not a global env var) and must hard-fail when combined with any cloud/multi-tenant deployment marker. Is there a concrete self-host customer requiring it, or can it stay deferred indefinitely?
3. Legacy `_execute_tool` store scoping (tenancy issue #4): `_execute_tool` builds `db = LeadDB()` with no workspace argument, so executed autopilot plan tool calls are not tenant-scoped at the legacy data layer even after the chat endpoint sets the RLS GUC. This predates the spec but the LLM planner amplifies it. Should the follow-up thread `workspace_id` into `LeadDB()`/`_execute_tool` (a broader refactor), and is that a blocker for cloud rollout of the autopilot planner or acceptable debt for single-tenant self-host first?
4. Per-cell synthesis reserve sizing: `synth_reserve` (the budget held back for the mandatory synthesis call) needs a concrete default. Should it be a fixed USD constant, a fraction of RESEARCH_CELL_BUDGET_USD (e.g. 40%), or computed from a token estimate of the gathered notes? This affects how many tool turns a 0.05 USD budget actually allows.
5. AUTOPILOT_MAX_AGENT_COLUMNS (8), AUTOPILOT_MAX_CELLS (4000), and AUTOPILOT_MAX_ROWS (500) defaults are guesses to bound real spend. Confirm these against expected legitimate autopilot use (e.g. a 500-row list with 5 enrichment columns = 2500 cells fits; is that the right ceiling, or do power users routinely exceed it?).
6. Approval nonce / plan store UX: requiring the client to echo `plan_id`+`nonce` on approval is an additive contract change to the CopilotKit frontend. Confirm the frontend team can thread these through the confirmation resubmit, and decide the behavior when a client omits them (currently: execution rejected / falls back to no-op rather than executing an unbound plan).
