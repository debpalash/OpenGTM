<!-- Auto-generated follow-up mini-spec (2026-06-26). Effort: medium. -->

## Mini-spec: Self-host `on_signal` — workspace-stamped legacy signal scanner

### Goal
Make `on_signal` automations fire on self-host (SQLite) deployments. Today they only fire in Postgres mode because the only signal writer that emits the trigger event is `PgLeadStore.add_signal`. The legacy scanner used on SQLite (`services/signals/monitor.py:run_signal_scan`) does a **global, non-workspace-scoped** scan and writes to a separate `data/signals.db` file, bypassing the emit path entirely. Route scanner-detected signals through a single workspace-aware write+emit path so `on_signal` works on both backends.

### Current state (file:line)
- `emit_signal_matches` is the only `on_signal` emitter and is documented "PG-only", called only from `PgLeadStore.add_signal` — `apps/api/services/automations/events.py:166-222`, invoked at `apps/api/services/leadgen/store.py:438-448` inside the store's RLS-scoped session, gated `if inserted`.
- `emit_signal_matches` itself is **not actually PG-specific**: it queries `Trigger`, `WorkbookRow`, `Workbook` ORM tables (all present on SQLite) and routes by `WorkbookRow.lead_id == lead_id` (`events.py:195-207`). The only "PG" aspect is that it runs inside a session whose `after_begin` hook sets the RLS GUC — and that hook is a **no-op on SQLite** (`apps/api/database.py:60-70`, only emits `set_config` when a workspace is bound on PG).
- The shared ORM `signals` table (`SignalRow`, `apps/api/services/leadgen/orm_models.py:137-163`) **is created on SQLite too** — the tenancy migration runs `op.create_table('signals', …)` unconditionally and only guards the RLS/GRANT statements behind `dialect.name == "postgresql"` (`migrations/versions/c42d0273d9bd_…py:95-119`). So on self-host this table exists but is unused.
- Legacy scanner path:
  - `run_signal_scan` uses a **global** `LeadDB()` with no workspace (`monitor.py:217-219`), scans hot/warm leads, and persists via `monitor.add_signal` into `data/signals.db` (`monitor.py:234-236`, `monitor.py:50-107`). No emit.
  - `_apply_signal_boosts` likewise uses a global `LeadDB()` (`monitor.py:260,271`).
  - `monitor.Signal.id` defaults to a **fresh `uuid4()` every run** (`monitor.py:36`) → re-scan produces new PKs.
- Feed read path on SQLite reads `data/signals.db` via `monitor.get_signals` (`apps/api/routers/signals.py:14-48`), because `_signal_backend` returns the store only when it's `PgLeadStore` (`signals.py:17-19`).
- The scan job is a single global `signal_scan` job, not workspace-scoped (`apps/api/services/workbook/refresh.py:164-196`; bootstrap `main.py:165-171`).
- Artificial gate: rule creation rejects `on_signal` with HTTP 409 when `PG_LEAD_STORE` is false (`apps/api/routers/automations.py:106-108`, `_validate_rule`).
- For comparison, the intent poller already does this correctly: deterministic `signals.id` (`apps/api/services/poller/keys.py:92-107`) → `store.add_signal` is idempotent on PK → exactly-once `on_signal` (`apps/api/services/poller/engine.py:223-252`).

### Design (recommended: scanner writes through a shared ORM signal store)
Unify signal persistence on the shared ORM `signals` table for **both** backends, since the table already exists on SQLite. Reuse `emit_signal_matches` verbatim.

1. **Shared signal store** — new `apps/api/services/signals/store.py` exposing `get_signal_store(workspace_id)` returning an ORM-backed store with `add_signal/get_signals/get_signal_counts/mark_signals_read`, all over `SignalRow` via `SessionLocal`, scoped by `workspace_id` (belt) + RLS on PG (suspenders). `add_signal` is the canonical write+emit, lifted from `PgLeadStore.add_signal:412-449`: `s.get(SignalRow, id)`; insert only when absent; `if inserted:` call `emit_signal_matches(s, workspace_id, [...])` in the same txn. Refactor `PgLeadStore.add_signal` to delegate here so PG and SQLite share one code path.
2. **Workspace-aware scan** — rewrite `run_signal_scan` (`monitor.py:212`) to iterate `ws_manager.list_workspaces()` (`apps/api/services/workspace/manager.py:291`); for each, `with workspace_scope(ws.id):` get `get_lead_store(ws.id, ws.slug)`, scan that store's hot/warm leads, stamp `Signal.workspace_id = ws.id`, assign a **deterministic id**, and write via `get_signal_store(ws.id).add_signal(...)`. Per-workspace work wrapped in try/except so one tenant's failure doesn't abort the rest. Self-host = single `main` workspace → loop runs once. `_apply_signal_boosts` (`monitor.py:256`) takes the same per-workspace scoped store instead of global `LeadDB()`.
3. **Feed read unification** — `routers/signals.py` reads through `get_signal_store(ctx.workspace_id)` on both backends; drop the `data/signals.db` branch. Optional one-time backfill of existing `data/signals.db` rows into `SignalRow` (or treat as out-of-scope; feed signals are ephemeral).
4. **Remove the gate** — drop the 409 in `_validate_rule` (`automations.py:106-108`); `on_signal` is allowed whenever `AUTOMATIONS_ENABLED` (the table is guaranteed present on both backends).

### File-by-file changes
- `apps/api/services/signals/store.py` (new): `get_signal_store(workspace_id)` + ORM signal store (`add_signal` write+emit, plus read methods).
- `apps/api/services/leadgen/store.py:412-449`: `PgLeadStore.add_signal` delegates to the shared store (keep its public signature/idempotency).
- `apps/api/services/signals/monitor.py`: `run_signal_scan` (212) → per-workspace loop using `workspace_scope` + `get_lead_store` + `get_signal_store`; deterministic `Signal.id`; `_apply_signal_boosts` (256) → scoped store. Keep `monitor.add_signal`/`get_signals` as thin back-compat shims (or delete once readers move).
- `apps/api/routers/signals.py:14-48,76-91`: route reads/mark-read through `get_signal_store` for both backends.
- `apps/api/routers/automations.py:106-108`: remove `on_signal_requires_pg_lead_store` 409.
- `apps/api/services/workbook/refresh.py:176`: optionally scope the workbook refresh-trigger loop per workspace (it currently `db.query(Workbook).all()` cross-workspace; benign for refresh, but tighten for consistency).

### Tenancy / security
- Self-host = single `main` workspace; `list_workspaces()` covers single- and multi-tenant SQLite uniformly. Every signal carries `workspace_id` (`Signal.workspace_id` already exists, `monitor.py:38`; persisted `monitor.py:101`).
- All writes/emits run inside `workspace_scope(ws.id)` so PG RLS GUC is set; on SQLite the GUC is a no-op but `emit_signal_matches` filters `Workbook.workspace_id == workspace_id` and `add_signal` stamps `workspace_id` (belt), so a SQLite signal can never map a lead into another tenant's workbook rows.
- Adversarial check — cross-tenant lead_id leak: `emit_signal_matches` joins `WorkbookRow → Workbook` filtered by `workspace_id` (`events.py:195-202`); a lead_id colliding across tenants cannot fire another tenant's rule because the workbook filter excludes them.
- Adversarial check — RLS inert on SQLite: acceptable; isolation is enforced by the explicit `workspace_id` filters in both the store and emit (the existing PG path relies on the same belt filters, RLS is suspenders).

### Idempotency
- Root cause of duplicate fires on SQLite today: `uuid4` per scan → new PK each run → would emit every scan. Fix: deterministic `Signal.id = sha256("scan|{ws}|{lead_id}|{signal_type}|{natural_key}")` mirroring `poller/keys.py:92-107`. `natural_key` must be a stable per-state value (e.g. for hiring, a coarse job-count band or a day/week bucket — OWNER decision on granularity).
- `add_signal` does `s.get(SignalRow, id)` and inserts only when absent; `emit_signal_matches` is called only `if inserted` → re-scan = no row, no fire.
- Second layer: the trigger engine's per-action replay/idempotency check keyed on `fire_key="signal:<pk>"` (`events.py:217`, engine `apps/api/services/automations/engine.py`) — even if a signal row is re-created, identical `fire_key` is deduped.

### Failure modes
- JobSpy network failure: already swallowed per-lead (`monitor.py:203-208`); keep.
- Emit failure must never break the signal write: already wrapped (`store.py:447-448`); preserve in the shared store.
- One workspace failing the scan must not abort others: wrap each iteration in try/except, log, continue.
- Cost amplification: per-workspace iteration multiplies provider I/O; keep the per-run cap (`monitor.py:185`) per workspace and consider a global cap. Single-workspace self-host is unaffected.
- Stale workspace context in pooled worker thread: `workspace_scope` restores prior value on exit (`tenancy.py:116`); ensure every iteration enters/exits scope cleanly.

### Feature flag / rollout
- No new flag required. Emit is already gated by `AUTOMATIONS_ENABLED` (`events.py:26-27`) — default off, so the scanner change is inert until automations are enabled. The 409 removal is the only behavior change for SQLite rule creation. Rollout: ship scanner+store change first (no-op while flag off), then allow `on_signal` rule creation.

### Test plan
- SQLite (default test backend): workspace + workbook row mapped to a lead + enabled `on_signal` rule (matching `signal_types`); mock `JobSpySignalProvider.enrich` to return hiring; `AUTOMATIONS_ENABLED=true`; run `run_signal_scan` → assert one `trigger_eval` job enqueued with `fire_key="signal:<pk>"`. Run again with identical mock → assert **no** second job (idempotent).
- SQLite negative tenancy: signal lead present in workspace B's workbook only; rule in workspace A → assert no fire for A.
- Router: `POST /api/automations` with `trigger_type=on_signal` on SQLite no longer returns 409.
- Feed: after scan, `GET /api/signals` returns the new signals from the ORM store on SQLite.
- PG-gated (live throwaway db, never touch `yupcha`): existing `PgLeadStore.add_signal` → on_signal emit still fires exactly once after delegating to the shared store (regression). Run as the marked PG suite: `PYTHONPATH=. uv run --group dev python -m pytest -k "signal and pg"`.
- Full suite: `PYTHONPATH=. uv run --group dev python -m pytest`.

### Acceptance criteria
1. With `AUTOMATIONS_ENABLED=true` on SQLite, a `signal_scan` that detects a signal for a lead mapped to a workbook row enqueues exactly one `trigger_eval` for each matching enabled `on_signal` rule.
2. Re-running the scan over unchanged state enqueues zero additional `trigger_eval` jobs (deterministic signal id → no duplicate row → no duplicate fire).
3. Every signal written by the scanner carries the correct `workspace_id`, and no signal maps a lead into another workspace's workbook rows.
4. Creating an `on_signal` rule on SQLite succeeds (no 409); creation still no-ops emit when `AUTOMATIONS_ENABLED=false`.
5. `PgLeadStore.add_signal` on PG continues to fire `on_signal` exactly once (no regression) after refactor to the shared store.
6. The signal feed (`GET /api/signals`) on SQLite returns scanner-written signals from the unified store.
7. A failure scanning one workspace does not prevent other workspaces from being scanned.

### Out of scope
- Backfilling/migrating historical `data/signals.db` rows into the ORM `signals` table (optional follow-up; note for owner).
- New signal detectors beyond the existing hiring/JobSpy path.
- Changing the poller (already idempotent and emit-correct).
- Per-workspace scan scheduling/interval tuning (stays a single global `signal_scan` job).
