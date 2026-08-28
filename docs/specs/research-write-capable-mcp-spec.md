<!-- Auto-generated research-bet spec (2026-06-26). Effort: large. -->

## Write-capable MCP server — BUILD-READY spec

### Goal
Promote the Yupcha MCP server from a read-mostly, **unauthenticated, tenant-blind** bridge into a write-capable agent surface (create/update leads, enroll in sequences, create workbooks, create automations) that routes **every write through the same RLS-scoped stores + `require_workspace_role` checks as the REST API**, behind a default-OFF flag, a per-token capability grant, an append-only audit log, idempotency, and per-workspace rate/spend caps. The adversarial constraint dominates the design: today's MCP is a confused-deputy / cross-tenant hole, and write capability cannot ship until that is closed.

---

### Grounded current state (the problem is bigger than "read-only")
- **MCP server lives at `apps/mcp/server.py`** (406 lines, hand-rolled JSON-RPC, protocol `2024-11-05`). Tools are a static list (`apps/mcp/server.py:34-125`); dispatch in `execute_tool` (`apps/mcp/server.py:130-269`); JSON-RPC loop in `handle_message` (`apps/mcp/server.py:274-327`); stdio transport `run_stdio` (`apps/mcp/server.py:330-368`); SSE/HTTP transport in `main` (`apps/mcp/server.py:375-400`).
- **No auth anywhere.** `handle_message` accepts `initialize`/`tools/list`/`tools/call` with zero credential check. There is no token, no user, no workspace.
- **Tenant-blind by construction.** Every tool instantiates `LeadDB()` with no args (`apps/mcp/server.py:134-136,156-158,171-173,178-180,256-258`). `LeadDB.__init__` with `db_path=None` resolves to a single global SQLite file `DB_PATH` (`apps/api/services/leadgen/db.py:47-49`). That store is **outside the PG multi-tenant RLS plane entirely** — it is one shared DB for all callers. So the existing MCP already reads/writes across all tenants; it just happens to mostly read today.
- **SSE mode binds `0.0.0.0` with no auth** (`apps/mcp/server.py:397` `uvicorn.run(sse_app, host="0.0.0.0", ...)`), and `POST /message` calls `handle_message` directly (`apps/mcp/server.py:390-393`) — a network-reachable unauthenticated tool executor. Even `enrich_company` (`apps/mcp/server.py:177-188`) already triggers real spend (waterfall enrichment) with no caps.
- **`create_workbook` is a stub** that returns a fake column plan and persists nothing (`apps/mcp/server.py:208-237`) — there is no real write path wired in yet.

**The REST tenancy/RLS model we must mirror** (`apps/api/core/tenancy.py`):
- `current_workspace()` resolves workspace from `X-Workspace-Id` header or the user's active workspace, enforces `ws_manager.is_member` (`tenancy.py:73-103`).
- The resolved workspace is published into a contextvar `current_workspace_var` (`tenancy.py:40-42,69,102`) which a SQLAlchemy `after_begin` hook reads to emit `SET LOCAL app.workspace_id` so **Postgres RLS scopes the transaction** (documented `tenancy.py:36-39`).
- `workspace_scope(workspace_id)` (`tenancy.py:109-131`) is the **non-request** entrypoint for workers/CLI/jobs — it sets the GUC and refuses an empty workspace (`tenancy.py:122-126`). **MCP is exactly this case: no FastAPI request, so all MCP writes MUST run inside `workspace_scope`.**
- `require_workspace_role(*roles)` (`tenancy.py:134-144`) checks `member_role` (`apps/api/services/workspace/manager.py:194`), owner implicitly satisfies any role.
- RLS-scoped stores: `get_lead_store(workspace_id, slug)` (`apps/api/services/leadgen/store.py:106`) → `PgLeadStore` with `upsert_lead` (`store.py:182`), `update_status` (`store.py:309`), `update_lead_fields` (`store.py:318`), `delete_lead` (`store.py:328`).

**Auth model today** is JWT-only: `OAuth2PasswordBearer` + `jwt.decode` against `settings.SECRET_KEY` (`apps/api/core/security.py:9-37`). **There is no API-key / PAT / capability-token table.** MCP clients (Claude Desktop/Cursor) cannot do an OAuth password flow, so a new token type is required.

**Reference patterns we will copy rather than invent:**
- Flag-gated router returning 404 when off: automations (`apps/api/routers/automations.py:42-44`, `_require_enabled`).
- Per-workspace **spend + action caps with idempotency reservations**: `apps/api/services/automations/caps.py:90-140` (`TriggerCapReservation`, `idempotency_key`, `FOR UPDATE` day-bucket lock, per-rule + workspace-global `AUTOMATIONS_GLOBAL_DAILY_USD`). Table `trigger_cap_reservations` with `UniqueConstraint(workspace_id, idempotency_key)` (`apps/api/services/automations/models.py:195-208`).
- Idempotent action results with `UniqueConstraint(workspace_id, idempotency_key)` (`apps/api/services/automations/models.py:147-163`).
- The enroll write path + its consent/suppression invariants (`apps/api/routers/outreach.py:181-209`): skip no-email, skip suppressed (`store.is_suppressed`), record `consent_source`/`consent_at` (`store.enroll`, `apps/api/services/outreach/store.py:192`). MCP enroll MUST preserve these.

---

### Design

**1. Stop the bleed first (mandatory pre-req, even for read tools).**
Rewire `execute_tool` so no tool ever calls `LeadDB()`/global stores. Every call carries a resolved `MCPCtx{user_id, workspace_id, slug, granted_caps}` and runs inside `with workspace_scope(workspace_id):` using `get_lead_store(workspace_id, slug)` and the same outreach/workbook/automation stores the REST routers use. Remove the unauthenticated `0.0.0.0` SSE path (replace with loopback + token, see infra).

**2. Token & capability model (new).**
Introduce a **scoped MCP token** type (PAT-style), distinct from JWT access tokens:
- New table `mcp_tokens` (NOT RLS — it's an auth-plane table, like users): `id`, `token_hash` (sha256; never store plaintext), `prefix` (first 8 chars for display), `user_id` FK, `workspace_id` (token is **bound to exactly one workspace** — no cross-workspace token), `capabilities` (JSON array of grant strings), `expires_at`, `revoked_at`, `last_used_at`, `created_at`, `created_by`.
- **Capabilities are explicit, additive, default-empty:** e.g. `leads:read`, `leads:write`, `sequences:enroll`, `workbooks:write`, `automations:write`. A token with no `*:write` cap is read-only. The MCP `tools/list` is **filtered to the token's caps** so an agent never even sees tools it can't call.
- **Role is still enforced at call time** (not just at grant): each write tool calls the equivalent of `require_workspace_role(...)` against the token's `user_id`+`workspace_id` via `ws_manager.member_role`. Capability grant is necessary but not sufficient — if the user is downgraded to viewer after the token was minted, writes fail. This defeats the privilege-freeze confused-deputy variant.
- Token is presented per transport: stdio → `YUPCHA_MCP_TOKEN` env var read at startup; SSE/HTTP → `Authorization: Bearer <token>` header, validated in `handle_message`/the HTTP layer before dispatch.

**3. Write ops in v1 (deliberately small).**
- `update_lead` (`leads:write`) → `PgLeadStore.update_lead_fields` / `update_status`. Field allowlist; no arbitrary column writes.
- `create_lead` (`leads:write`) → `PgLeadStore.upsert_lead`.
- `enroll_leads` (`sequences:enroll`) → reuse the exact enroll path incl. suppression + consent (`outreach.py:181-209`); `consent_source="mcp_enroll"`.
- `create_workbook` (`workbooks:write`) → replace the stub with the real workbook-create store call used by `apps/api/routers/workbooks.py`.
- **Deferred to v2:** `create_automation` (high blast radius — automations can spend/send and themselves enroll; expose only after MCP write is proven), bulk operations, `delete_lead`, sequence start/pause (these stay REST-only / admin-UI-only in v1).

**4. Audit log (new, append-only).**
New table `mcp_audit_log` (RLS-scoped by `workspace_id`): `id`, `workspace_id`, `token_id`, `user_id`, `tool_name`, `arguments_redacted` (JSON, secrets/PII-trimmed), `idempotency_key`, `result_status` (ok/error/denied/capped), `error`, `estimated_cost_usd`, `created_at`. **Every write tool call writes exactly one row** before returning, including denials (cap/role/capability). This is the forensic trail for the confused-deputy/abuse case.

**5. Idempotency + rate/spend caps (reuse caps.py).**
- Each write tool accepts an optional `idempotency_key`; reuse the reservation pattern from `caps.py` keyed on `(workspace_id, idempotency_key)` so a retried tool call is a no-op.
- Add MCP-specific caps: `MCP_MAX_WRITES_PER_DAY` per workspace and reuse the existing `AUTOMATIONS_GLOBAL_DAILY_USD`-style global spend cap for any cost-incurring tool (enrichment). Caps checked + reserved through the same `caps.py` machinery (or a thin sibling table) so MCP writes can't exceed automation budgets.

**6. Default posture: OFF.**
New flag `MCP_WRITE_ENABLED: bool = False` (mirrors `AUTOMATIONS_ENABLED`, `apps/api/core/config.py:108`). When False, `tools/list` exposes only read tools and every write tool returns a structured `{"error":"mcp writes disabled"}` (the MCP analog of automations' 404 gate). Even with the flag on, a token still needs the per-cap grant.

---

### File-by-file changes
- **`apps/api/core/config.py`** — add `MCP_WRITE_ENABLED=False`, `MCP_MAX_WRITES_PER_DAY`, `MCP_TOKEN_TTL_DAYS`. (near existing flags ~`:108`).
- **`apps/api/models.py` (or `apps/api/services/mcp/models.py`)** — `MCPToken` and `MCPAuditLog` ORM models; reuse the `UniqueConstraint(workspace_id, idempotency_key)` idiom from `automations/models.py:147-163`.
- **`migrations/versions/<new>.py`** — create `mcp_tokens`, `mcp_audit_log` (+ RLS policy on `mcp_audit_log` mirroring the leads/workbooks RLS migrations, e.g. `e5f6a7b8c9d0_workbooks_rls.py`). down_revision = current head `a7b8c9d0e1f2`.
- **`apps/api/services/mcp/auth.py` (new)** — `resolve_mcp_token(raw) -> MCPCtx`: hash-lookup, expiry/revocation check, load caps, update `last_used_at`; raises on invalid.
- **`apps/api/services/mcp/audit.py` (new)** — `record(ctx, tool, args, status, cost, idem, error)`.
- **`apps/api/services/mcp/tools.py` (new)** — the write tool implementations, each: cap-check capability → `require_workspace_role` equiv → `workspace_scope` + idempotency reservation → call the **same store method as REST** → audit. Read tools migrate here too (off `LeadDB()` onto `get_lead_store`).
- **`apps/mcp/server.py`** — (a) add token extraction (env/header); (b) filter `TOOLS` by granted caps in `tools/list`; (c) `execute_tool` delegates to `services/mcp/tools.py` with `MCPCtx`; (d) **remove `0.0.0.0` SSE bind**, replace with `127.0.0.1` + bearer auth on `/message`; (e) reject `tools/call` when `initialize` had no valid token.
- **`apps/api/routers/mcp_tokens.py` (new)** — REST CRUD to mint/list/revoke MCP tokens, gated by `require_workspace_role("admin")` and returning plaintext token **once**. UI to manage these later.
- **`docker-compose.yml` / docs** — document `YUPCHA_MCP_TOKEN` and that SSE is loopback-only by default.

---

### Data / persistence + tenancy/security
- `mcp_tokens` is auth-plane (not RLS) but **every row is workspace-bound**; lookups by hash, plaintext never stored.
- `mcp_audit_log` is **RLS-scoped** (`app.workspace_id`) so audit reads can only see the caller's tenant.
- All write/read tool DB work runs inside `workspace_scope(ctx.workspace_id)` so the `after_begin` GUC hook scopes RLS exactly as REST does — closing the current `LeadDB()` cross-tenant hole.
- Defense in depth against confused-deputy: (1) token bound to one workspace, (2) capability grant required, (3) live role re-check, (4) flag gate, (5) caps, (6) audit. An agent that is socially-engineered into calling a write tool still cannot exceed the token's single workspace, granted caps, the user's current role, or the daily caps.

### Infra / deploy
- No new service. stdio mode unchanged in topology (now token-gated). SSE/HTTP mode: bind `127.0.0.1` only; for remote access require a reverse proxy + the bearer token (documented, not auto-exposed). Availability is non-critical (local dev tool), but the unauthenticated `0.0.0.0` listener is a live exposure to remove regardless of this feature.

### License / compliance
- No new third-party data providers. **Consent/CAN-SPAM surface**: MCP `enroll_leads` must keep suppression + consent recording (`outreach.py:189-208`); allowing an LLM to silently enroll contacts is a compliance risk, so enroll requires `sequences:enroll` cap AND records `consent_source="mcp_enroll"` for audit.

### Feature-flag / rollout (default-OFF)
- `MCP_WRITE_ENABLED=False` by default. Phase 1: ship auth + RLS rewiring of **read** tools + token table + audit (no write tools) — closes the existing hole. Phase 2: enable write tools behind the flag for internal workspaces. Phase 3: docs + UI for token management. v2: `create_automation`.

### Test plan
- **PG-gated (live throwaway DB, never `yupcha`):** (1) a token for workspace A cannot read/write workspace B's leads (RLS proof); (2) `update_lead`/`create_lead` go through `PgLeadStore` and the GUC is set (assert via `workspace_scope`); (3) enroll respects suppression + records consent; (4) idempotent retry of a write produces one row (reservation table); (5) caps: N+1 writes/day is denied + audited as `capped`.
- **Auth unit tests:** invalid/expired/revoked token rejected; read-only token sees only read tools in `tools/list`; write tool without cap → denied+audited; user role downgraded after mint → write denied.
- **Flag tests:** `MCP_WRITE_ENABLED=False` → write tools error, read tools still work.
- **Regression:** SSE no longer binds `0.0.0.0`; `/message` without bearer → 401.
- Run: `PYTHONPATH=. uv run --group dev python -m pytest`.

### Numbered acceptance criteria
1. With `MCP_WRITE_ENABLED=False` (default), no write tool mutates data and `tools/list` hides write tools.
2. No MCP tool (read or write) instantiates `LeadDB()`/global store; all go through `get_lead_store`/REST stores inside `workspace_scope`.
3. A valid MCP token is bound to exactly one workspace and a capability set; cross-workspace access is impossible.
4. Each write tool enforces capability AND live `require_workspace_role` AND records one `mcp_audit_log` row (incl. denials).
5. Writes are idempotent per `(workspace_id, idempotency_key)`; retries do not double-apply.
6. Per-workspace daily write/spend caps deny + audit when exceeded.
7. `enroll_leads` preserves suppression skip and consent recording.
8. SSE/HTTP transport binds loopback and requires a bearer token; unauthenticated calls are rejected.
9. RLS-scoping is proven by a PG test where workspace A's token sees zero of workspace B's rows.
10. Plaintext tokens are shown once and only hashes are stored.

### Out of scope (v1)
`create_automation`/`send_email` via MCP, bulk/`delete_lead`, sequence start/pause, OAuth dynamic client registration, multi-workspace tokens, a full MCP UI (REST CRUD only in v1).

---

### Owner decisions
1. **v1 write ops** — Recommend: `update_lead`, `create_lead`, `enroll_leads`, `create_workbook`; defer `create_automation`/sends/deletes. (Automations can themselves spend/send/enroll → too much blast radius for the first write release.)
2. **Auth model** — Recommend: new hashed, workspace-bound, capability-scoped MCP token (PAT) + live role re-check. (JWT password flow doesn't fit desktop MCP clients; capability+role is the confused-deputy defense.)
3. **Audit scope** — Recommend: audit **all writes including denials**, RLS-scoped table. (Forensics for abuse; reads optional/sampled to limit volume.)
4. **Default posture** — Recommend: `MCP_WRITE_ENABLED=False` global flag AND per-token caps both required. (Two independent off-switches.)
5. **Fix the existing hole now or only with writes?** — Recommend: ship the read-tool RLS rewiring + remove `0.0.0.0` SSE in Phase 1 **independently**, since it's a live cross-tenant exposure today regardless of write capability.
6. **Caps backend** — Recommend: reuse `automations/caps.py` reservation machinery rather than a parallel system, to share the global spend cap.
