# Follow-up: Workbooks RLS Hardening + Worker Tenant-Scoping (BUILD-READY SPEC)

## ✅ LOCKED DECISIONS (owner-approved — recommended defaults)
- OD-1 NULL workspace_id: **backfill-then-block** (auto-backfill single-workspace/`main`; ABORT migration on ambiguous multi-tenant NULLs — never guess a tenant).
- OD-2: harden **all five** tables (workbooks, workbook_rows, workbook_enrichments, workbook_activity, cell_traces) with denormalized workspace_id.
- OD-3 PR split: **PR-A** scoping (RLS OFF, no-op, ships first) → **PR-B** RLS migration + child columns + NOT NULL + remove startup backfill → **PR-C** chat_history/Mem0 partition + missing /conversations auth.
- OD-4: worker learns scope from the **job payload** (stamp workspace_id at every enqueue site; handlers enter workspace_scope first).
- OD-5 websocket auth: **workspace_id ws query param**.
- OD-6 chat/mem0 key: **workspace_id + user_id**.
- OD-7: verify services/workbook/worker.py (Redis/BullMQ) — harden if live, delete if dead.


Status: design only — NO code in this PR. The author of the implementation PRs will
follow this exactly.

PR #90 (`3171f41`) made the legacy chat path tenant-safe but **explicitly deferred**
three coupled hardening items because doing them piecemeal is dangerous. This spec
sequences them so the cutover is fail-closed and never half-applied.

The three deferred items:

1. **Add Postgres RLS to `workbooks` (+ its child tables) — they have NO row-level
   security today.** Only app-layer `workspace_id` filters protect them, which is
   load-bearing: a single missed filter = cross-tenant read/IDOR or cross-tenant write.
2. **Tenant-scope the non-request DB access in the workbook worker/job-runner path**
   (`source_engine.py`, `workbook/worker.py`, `enrichment.py`, `refresh.py`). This MUST
   land **before / together with** #1: turning on `FORCE ROW LEVEL SECURITY` on
   `workbooks` makes every query that runs without the `app.workspace_id` GUC set
   fail closed (zero rows), which would silently break sourcing/enrichment/refresh.
3. **Tenant-scope `chat_history` and Mem0 memory** (still global; separate SQLite
   stores, decoupled from the RLS migration).

---

## 1. Goal

- Bring `workbooks` and `workbook_rows` (and the equally-unprotected child tables) to
  the **same proven RLS posture** as `leads` / `signals` / `outreach_*`:
  `ENABLE` + `FORCE` ROW LEVEL SECURITY, a fail-closed `USING` + `WITH CHECK` policy on
  `current_setting('app.workspace_id', true)`, and least-privilege `GRANT`s to the
  `yupcha_app` role (recipe lifted byte-for-byte from
  `migrations/versions/c42d0273d9bd_*` and `b2c3d4e5f6a7_outreach_rls.py`).
- Make every **non-request** code path that touches a workbook table obtain its
  workspace_id and run inside `tenancy.workspace_scope(...)` **before** RLS is enabled,
  so the cutover is invisible to correct paths and fail-closed to incorrect ones.
- Keep self-host (single workspace, SQLite) working unchanged — all RLS DDL is
  `dialect == "postgresql"` guarded, exactly like the existing migrations.
- Defer chat/memory scoping to its own low-risk PR.

Non-goal: changing the workbook data model, the queue substrate, or the lead store.

---

## 2. Grounded current-state inventory (file:line)

Alembic head (verified by walking `down_revision` — nothing points at it):
**`d4e5f6a7b8c9`** (`outreach_inbound`). Chain:
`db9a4b81b1e6 → f3b30195d51a → 3fa508f4ae76 → f7274d0aacd9 → c42d0273d9bd →
a1b2c3d4e5f6 → b2c3d4e5f6a7 → c7d8e9f0a1b2 → d4e5f6a7b8c9`.
(MEMORY.md's "head b2c3d4e5f6a7" is stale.)

### 2.1 The RLS machinery that already works (reuse verbatim)

- **GUC publish hook** — `apps/api/database.py:50-70`. On non-SQLite, a SQLAlchemy
  `after_begin` listener reads `current_workspace_var` and emits
  `SELECT set_config('app.workspace_id', <ws>, true)` (transaction-local; reset on
  COMMIT/ROLLBACK so a pooled connection never leaks). If unset → GUC absent → policy
  yields zero rows. **The hook's own docstring (database.py:62-66) calls out that
  `workbooks` is currently a non-RLS table that "must keep working" with no workspace
  bound — that is precisely the assumption this spec removes.**
- **Request scoping** — `apps/api/core/tenancy.py:73-103` (`current_workspace` dep sets
  the contextvar) + `WorkspaceCtx.lead_db()` (tenancy.py:53-70).
- **Worker scoping primitive** — `apps/api/core/tenancy.py:109-131`
  (`workspace_scope(workspace_id)` ctx manager; raises on empty ws so an empty scope
  can't silently mask a bug).
- **Proven RLS migration recipe** — `migrations/versions/c42d0273d9bd_*.py:123-190`
  (role create + grants + ENABLE/FORCE + fail-closed policy) and
  `b2c3d4e5f6a7_outreach_rls.py:170-200` (grants + ENABLE/FORCE on new tenant tables;
  app role NOT re-created). `APP_DB_ROLE = "yupcha_app"` (config.py:90).
- **Migration vs runtime role** — `db_init.py` runs `alembic upgrade head` as the
  schema **OWNER**; runtime connects as the **`yupcha_app`** group role
  (NOLOGIN/NOSUPERUSER/NOBYPASSRLS, granted to the concrete login role). `FORCE` RLS is
  what makes the policy bind to the owner too.

### 2.2 The workbook tables (schema reality)

| Table | Model file:line | `workspace_id` column today? | Keyed by |
|---|---|---|---|
| `workbooks` | `services/workbook/models.py:148-156` | **yes** (`String, index=True, nullable=True`) | — |
| `workbook_rows` | `services/workbook/models.py:256-272` | **NO** | `workbook_id` FK |
| `workbook_enrichments` | `services/workbook/models.py:217-228` | **NO** | `workbook_id` FK |
| `workbook_activity` | `services/workbook/activity_models.py:10-16` | **NO** | `workbook_id` |
| `cell_traces` | `services/workbook/trace_models.py:10-14` | **NO** | `workbook_id` |

Implication: `workbooks.workspace_id` is **nullable** (legacy rows can be NULL — see
the runtime backfill at `main.py:90-121`), and the four child tables carry **no tenant
column at all**. Both facts drive the migration design (§5) and the NULL decision (§8).

### 2.3 Workbook-table access sites (read = R, write = W)

GUC-set means: runs inside a request (`current_workspace` dep) OR inside
`workspace_scope(...)`. "no" = currently relies on `workbooks` having no RLS.

| # | file:line | table(s) | R/W | currently scoped (app filter)? | GUC set today? | breaks under FORCE RLS? |
|---|---|---|---|---|---|---|
| A1 | `routers/workbooks.py:40-49` `_owned_workbook` | workbooks | R | yes (`wb.workspace_id != ctx.workspace_id`) | **yes** (request dep) | no |
| A2 | `routers/workbooks.py:131-150` `_workbook_response` (opens its own `SessionLocal`) | workbook_rows | R | implicit (by wb.id) | **yes** (same request contextvar) | no |
| A3 | `routers/workbooks.py:178-188, 215-..., 368-..., 422-468, 651, 678, 706-742, 896-1206` | workbooks/rows/enrichments | R/W | mixed: list filters by ws; many `Workbook.id == id` only, guarded by `_owned_workbook` first | **yes** (request) | no (RLS becomes belt-and-suspenders) |
| A4 | `routers/workbooks.py:1244-1255` websocket auth (`_authorize_ws`) | workbooks | R | reads `wb.workspace_id` to call `is_member` | **NO** (pre-context auth, no scope) | **YES — returns None → ws auth always fails** |
| B1 | `services/workbook/source_engine.py:70-94` load wb + read `source_config.workspace_id` | workbooks | R/W | reads ws FROM the row | **NO** | **YES — can't read the row to learn its ws (chicken-and-egg)** |
| B2 | `source_engine.py:113-120` `lead_db = LeadDB()` then `get_leads(source=job:...)` | leads | R | **no** (bare LeadDB, default path) | **NO** | leads already RLS → returns 0 / wrong store |
| B3 | `source_engine.py:128-207` insert WorkbookRow, count, status | workbook_rows/workbooks | R/W | by wb.id only | **NO** | **YES** |
| B4 | `source_engine.py:236-256` `handle_source_workbook` + chained `add_job("run_workbook")` | (enqueue) | W | payload `{workbook_id, column_id}` — **no workspace_id** | **NO** | enqueue ok; downstream breaks |
| C1 | `services/workbook/enrichment.py:944-971, 985-1128` `run_workbook_enrichment` (many `SessionLocal`) | workbooks/rows/enrichments | R/W | by wb.id; reads `Workbook.workspace_id` at 208-212 & 235-239 for sub-calls | **NO** | **YES** (every `query(Workbook)` → None) |
| C2 | `enrichment.py:1158-1179` `handle_run_workbook` | (dispatch) | — | payload has no workspace_id | **NO** | **YES** |
| D1 | `services/workbook/refresh.py:81-116` `refresh_workbook` | workbooks/rows/enrichments | R/W | by wb.id | **NO** | **YES** |
| D2 | `refresh.py:134-146` `handle_refresh_workbook` + `_enqueue_next` (payload `{workbook_id}`) | (dispatch/enqueue) | W | no workspace_id | **NO** | **YES** |
| D3 | `refresh.py:164-196` `handle_signal_scan` → `db.query(Workbook).all()` | workbooks (ALL) | R/W | **global cross-workspace enumerate** | **NO** | **YES — `.all()` returns 0 under RLS; the global scan is structurally incompatible** |
| E1 | `services/workbook/worker.py:62-95` `process_enrich_cell` (`SessionLocal`, `query(Workbook).filter(id==)`) | workbooks/rows | R/W | by wb.id; payload has no workspace_id | **NO** | **YES** |
| E2 | `worker.py:98-130` `_load_row_data` → bare `LeadDB()` (v1 fallback) | workbook_rows, leads | R | none | **NO** | **YES** (rows) + wrong lead store |
| F1 | `routers/copilotkit.py:1028-1097` workbook tools (`SessionLocal`, filter by `workspace_id`) | workbooks | R/W | yes (explicit `Workbook.workspace_id == workspace_id`) | **yes** (inside `workspace_scope`, copilotkit.py:1337/1456) | no |
| F2 | `routers/copilotkit.py:758-766` `_jobdb = LeadDB()` job-row stamp | legacy leadgen SQLite `jobs` (NOT a workbook/PG table) | W | n/a | n/a | no (separate SQLite file) |
| G1 | `routers/automations.py:84-95` `_scoped_columns` (`query(Workbook).filter(ws_id)`) | workbooks | R | yes | **yes** (request) | no |
| G2 | `routers/templates.py:75-83` create Workbook (stamps `ctx.workspace_id`) | workbooks/rows | W | yes | **yes** (request) | no |
| H1 | `main.py:90-121` startup backfill `UPDATE workbooks SET workspace_id=:wid WHERE workspace_id IS NULL` | workbooks | W | global, no scope | **NO** | **YES — under FORCE RLS this matches 0 rows (NULL ≠ GUC) → backfill silently no-ops** |

Bare `LeadDB()` sites that matter to this work: `source_engine.py:113`,
`workbook/worker.py:121`, `enrichment.py:615 & 727` (verify these last two are reached
only via already-scoped run paths). The full `LeadDB()` census (cli, hubspot, settings,
leads router, etc.) is out of scope — those touch `leads` (already RLS) not workbooks.

### 2.4 Enqueue sites (where workspace_id IS known and must be stamped into payload)

| file:line | job type | current payload | has ws at call site? |
|---|---|---|---|
| `routers/workbooks.py:828` | run_workbook | column/row args, no ws | yes (`ctx`) |
| `routers/workbooks.py:928` | source_workbook | `{workbook_id, column_id}` | yes (`ctx`) |
| `routers/workbooks.py:1034` | refresh_workbook | `{workbook_id, reason}` | yes (`ctx`) |
| `routers/copilotkit.py:1040` | source_workbook | `{workbook_id, column_id, enrich_after}` | yes (`workspace_id`) |
| `routers/signals.py:57` | signal_scan | `{}` | request ws (but scan is global) |
| `source_engine.py:253` | run_workbook (chain) | `{workbook_id}` | yes (`workspace_id` local) |
| `refresh.py:119-131` `_enqueue_next` | refresh_workbook | `{workbook_id}` | must thread through |
| `refresh.py:188-213` signal_scan (self re-enqueue + bootstrap) | signal_scan | `{interval_minutes}` | global |
| `refresh.py:219-225` `_enqueue_next_now` | refresh_workbook | `{workbook_id, reason}` | inside signal scan loop |

The `jobs` table (`apps/api/models.py:30`, `queue_service.add_job` at
`queue_service.py:67-83`) is **non-RLS and has no `workspace_id` column**; `add_job`
does not set one. So the worker's only tenant signal must come from the **payload**.

### 2.5 Item #3 stores (chat_history + Mem0) — confirmed global

- `services/chat_history.py` — standalone SQLite `data/chat_history.db`. Functions
  (`create_conversation`/`list_conversations`/`get_messages`/`add_message`/
  `delete_conversation`, lines 58-151) take **no workspace and no user**.
- `routers/copilotkit.py:143-163` — `/conversations` GET/DELETE endpoints have **no
  auth dependency at all** → any caller lists/reads/deletes **every** tenant's chats.
- `services/memory.py` — `_BuiltinMemory` over `chat_memory.db` (or Mem0
  `openmemory.db`). Partitioned only by `user_id`, and callers hardcode
  `user_id="default"` (`copilotkit.py:1524, 1658`) → effectively one global memory.

These are separate SQLite files, not PG tables → RLS does not apply; scoping is
application-level partitioning. Fully decoupled from #1/#2 → its own PR (§ PR-C).

---

## 3. Breakage analysis — exactly what fails when `FORCE RLS` lands on `workbooks`

`FORCE ROW LEVEL SECURITY` + the fail-closed policy means: **any connection without
`app.workspace_id` set sees zero `workbooks` rows and cannot INSERT/UPDATE** (the
`WITH CHECK` rejects `workspace_id` ≠ GUC, including NULL). Today the GUC is only set on
request paths and the already-`workspace_scope`d paths. Therefore the following break
the instant the migration applies, unless §2 code lands first:

1. **Worker job handlers run outside any request and outside `workspace_scope`** —
   `handle_run_workbook` (C2), `handle_source_workbook` (B4), `handle_refresh_workbook`
   (D2), `handle_signal_scan` (D3), and the alternate `workbook/worker.py`
   `process_enrich_cell` (E1). Every `db.query(Workbook).filter(Workbook.id==…)`
   returns `None` → handlers early-return `workbook_not_found`; sourcing/enrichment/
   refresh silently stop. **How does the worker get a workspace today?** It does *not*
   from the job — it reads it back **off the workbook row** (`source_engine.py:83`
   `(wb.source_config or {}).get("workspace_id")`; `enrichment.py:208/235`
   `query(Workbook.workspace_id)`). Under RLS that read is itself blocked → chicken-and-
   egg. **#90 stamped `source_config.workspace_id` on the workbook, NOT the job
   payload** — so the worker cannot bootstrap its scope without first reading the very
   row RLS hides.
2. **The websocket authorizer** (A4) reads `wb.workspace_id` before any scope to decide
   membership → returns `None` → all workbook websockets 403.
3. **The global signal scan** (D3) does `db.query(Workbook).all()` across all tenants —
   structurally incompatible with per-tenant RLS; returns 0.
4. **The startup NULL backfill** (H1) `UPDATE … WHERE workspace_id IS NULL` matches 0
   rows under RLS → legacy NULL-workspace workbooks become permanently invisible AND
   un-backfillable through the app. The backfill must move into the migration (owner,
   before FORCE), see §8.
5. **Bare `LeadDB()` in the sourcing read-back** (B2/E2) — independent of workbook RLS
   but in the same blast radius: on PG the canonical lead store is the RLS-protected
   shared `leads` table via `get_lead_store(ws, slug)`, not a default-path `LeadDB()`.
   Without scope these read zero / from the wrong store.

Request paths (A1-A3, F1, G1-G2) do **not** break: `current_workspace`/`workspace_scope`
already publishes the contextvar, so the `after_begin` hook sets the GUC; RLS becomes a
belt-and-suspenders backstop behind the existing app-layer filters.

---

## 4. Design + safe ordering

Two principles:

- **Scope before you enforce.** All non-request paths must obtain workspace_id and enter
  `workspace_scope(...)` **before** the table starts fail-closing. So the scoping code
  ships and soaks in a release where RLS is still OFF (everything keeps working because
  the app-layer filters + workbook reads still work), and the RLS migration follows.
- **The worker's tenant signal comes from the job payload, never from the row.** Stamp
  `workspace_id` into every workbook-job payload at enqueue time (the enqueuer always
  knows it — §2.4), and have each handler do `with workspace_scope(payload["workspace_id"])`
  as its first action. This removes the chicken-and-egg.

### 4.1 Recommended sequencing (3 PRs by dependency)

- **PR-A — Scoping (code only; RLS still OFF). Ships first, soaks.**
  1. Stamp `workspace_id` into every workbook-job payload (§2.4 sites).
  2. Wrap `handle_run_workbook`, `handle_source_workbook`, `handle_refresh_workbook`,
     `process_enrich_cell` (and their inner functions `materialize_source`,
     `run_workbook_enrichment`, `refresh_workbook`) in
     `with workspace_scope(workspace_id):` taken from the payload, not the row.
  3. Replace bare `LeadDB()` read-backs (B2, E2) with the scoped lead store
     (`get_lead_store(workspace_id, slug)`), inside the scope.
  4. Rework `handle_signal_scan` (D3) into a **workspace-enumerating** scan: read the
     `workspaces` list from the non-RLS `workspaces` table (via
     `services/workspace/manager`), then for each workspace
     `with workspace_scope(ws.id): db.query(Workbook)…`. (Mirrors the per-workspace
     enumerate pattern in `services/signals/monitor.py:314-342` and the poller bootstrap
     in `poller/engine.py:560-582`.)
  5. Rework the websocket authorizer (A4): take `workspace_id` from the client (the
     frontend already knows it — pass it as a ws query param alongside `token`), enter
     `workspace_scope`, then `query(Workbook).filter(id==…)` (now RLS-scoped) and
     `is_member`. A wrong/forged ws yields no row → 403. (Owner decision OD-5.)
  6. This PR is a **no-op behaviourally** while RLS is off and is forward-compatible
     when it turns on. Safe to deploy alone.

- **PR-B — The RLS migration + child `workspace_id` columns + remove the startup
  backfill.** Depends on PR-A being deployed. Single alembic revision off
  `d4e5f6a7b8c9` (§5). After it applies, every path is already scoped.

- **PR-C — chat_history + Mem0 tenant scoping.** Independent; can land any time.
  Add `workspace_id` (+ keep `user_id`) partitioning to `chat_history` and `memory`,
  thread `ctx.workspace_id` from the copilotkit endpoints (and add the missing auth
  dependency to `/conversations`). (Owner decision OD-6.)

> If the owner prefers fewer PRs, PR-A and PR-B may be combined into one release
> **provided the code change and `alembic upgrade head` deploy atomically** (they do —
> `db_init` runs alembic at startup). The 3-PR split is recommended purely to get a soak
> window where the scoping runs in prod with RLS still off, so a scoping miss surfaces as
> a logged anomaly rather than a fail-closed outage.

---

## 5. Migration plan (PR-B)

New revision **`e5f6a7b8c9d0`** (`workbooks_rls`), `down_revision = "d4e5f6a7b8c9"`.
Modeled byte-for-byte on `b2c3d4e5f6a7_outreach_rls.py`. SQLite path: column adds only,
no RLS (dialect-guarded). `APP_DB_ROLE = os.getenv("YUPCHA_APP_DB_ROLE", "yupcha_app")`.

Tables that get RLS:
`workbooks`, `workbook_rows`, `workbook_enrichments`, `workbook_activity`, `cell_traces`.
(Owner decision OD-4: minimum is `workbooks` + `workbook_rows`; recommend doing all five
in one pass since the three extra child tables are equally unprotected and a second pass
is a second risky cutover.)

`upgrade()` order (critical — backfill must run while the table is still policy-free):

```
1. Add denormalized workspace_id to each child table (nullable for now):
   op.add_column("workbook_rows",        sa.Column("workspace_id", sa.String(), nullable=True))
   op.add_column("workbook_enrichments", sa.Column("workspace_id", sa.String(), nullable=True))
   op.add_column("workbook_activity",    sa.Column("workspace_id", sa.String(), nullable=True))
   op.add_column("cell_traces",          sa.Column("workspace_id", sa.String(), nullable=True))

2. Backfill (no RLS yet → owner writes freely):
   a. Parent backfill (replaces main.py:90-121 — see §8 for the value chosen):
      UPDATE workbooks SET workspace_id = :fallback_ws WHERE workspace_id IS NULL
      (only when the NULL policy resolves to a single target; otherwise FAIL — §8)
   b. Child backfill from parent:
      UPDATE workbook_rows        c SET workspace_id = w.workspace_id FROM workbooks w WHERE c.workbook_id = w.id;
      …same for workbook_enrichments, workbook_activity, cell_traces.

3. Guard against orphans BEFORE NOT NULL:
   SELECT count(*) FROM <each table> WHERE workspace_id IS NULL  → assert 0 (else raise — §8).

4. Tighten to NOT NULL (all 5 incl. workbooks):
   op.alter_column("workbooks", "workspace_id", nullable=False)
   …and each child table.

5. Indexes for policy/lookups:
   ix_workbook_rows_ws        (workspace_id), ix_workbook_rows_ws_wb (workspace_id, workbook_id)
   …analogous for the other child tables. (workbooks already has ix on workspace_id.)

6. Postgres-only (_pg_upgrade, dialect-guarded):
   for tbl in (workbooks, workbook_rows, workbook_enrichments, workbook_activity, cell_traces):
       GRANT SELECT, INSERT, UPDATE, DELETE ON {tbl} TO yupcha_app
   GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO yupcha_app   # serial PKs on rows/enrichments/activity/traces
   for tbl in (...same 5...):
       ALTER TABLE {tbl} ENABLE ROW LEVEL SECURITY
       ALTER TABLE {tbl} FORCE  ROW LEVEL SECURITY
       CREATE POLICY {tbl}_workspace_isolation ON {tbl}
         USING (workspace_id = current_setting('app.workspace_id', true))
         WITH CHECK (workspace_id = current_setting('app.workspace_id', true))
```

`downgrade()`: `DROP POLICY IF EXISTS … ` on each (PG), then drop the indexes + the four
added columns. App role NOT dropped (matches head migrations). `workbooks.workspace_id`
reverts to nullable.

Notes:
- Steps 1-5 are dialect-agnostic schema ops (run on SQLite too — harmless, gives the
  self-host file the `workspace_id` columns and keeps the model/DDL in sync). Step 6 is
  PG-only.
- The migration is **idempotent-friendly**: wrap `add_column`/policy creates with
  existence guards (`IF NOT EXISTS` on policies; check `inspector.get_columns` before
  `add_column`) so a re-run / partially-applied state is safe — matches the defensive
  style in the repo's other migrations.
- App-role grants are required because runtime is `yupcha_app` (non-owner). Without the
  child-table grants the worker would get permission-denied even with the GUC set.

### 5.1 Model changes (PR-B, alongside the migration)

Add `workspace_id = Column(String, nullable=False, index=True)` to `WorkbookRow`
(`models.py:256`), `WorkbookEnrichment` (`models.py:217`), `WorkbookActivity`
(`activity_models.py`), `CellTrace` (`trace_models.py`). Change
`Workbook.workspace_id` (`models.py:156`) to `nullable=False`. Every constructor that
creates these rows must now pass `workspace_id` (see §6).

---

## 6. File-by-file changes

### PR-A (scoping; RLS OFF)

- **`apps/api/services/queue_service.py`** — *optional but recommended*: have `add_job`
  accept/store nothing new (keep `jobs` non-RLS), but document that workbook payloads
  must include `workspace_id`. (No schema change; payload is the carrier.)
- **`apps/api/routers/workbooks.py`**
  - `:828` run_workbook enqueue → add `"workspace_id": ctx.workspace_id` to payload.
  - `:928` source_workbook enqueue → add `"workspace_id": ctx.workspace_id`.
  - `:1034` refresh_workbook enqueue → add `"workspace_id": ctx.workspace_id`.
  - `:1244-1255` websocket authorizer → accept `workspace_id` from the ws query string;
    `with workspace_scope(ws): wb = sess.query(Workbook).filter(id==…).first()`;
    keep the `is_member` check. (Pairs with the `@router.websocket` handler at
    `:1258` passing the param through.)
- **`apps/api/routers/copilotkit.py:1040`** — source_workbook enqueue → add
  `"workspace_id": workspace_id` to payload (the tool already runs inside
  `workspace_scope`).
- **`apps/api/services/workbook/source_engine.py`**
  - `materialize_source(workbook_id, column_id, workspace_id)` — add the param; **enter
    `with workspace_scope(workspace_id):`** wrapping the whole body so the wb/row reads
    (B1/B3) are scoped. Drop the `source_config.workspace_id` read (B1) as the bootstrap
    source — keep it only as a sanity assert (`assert wb.workspace_id == workspace_id`).
  - `:113-120` replace `lead_db = LeadDB()` with the scoped store:
    `store = get_lead_store(workspace_id, slug); store.get_leads(source=f"job:{job_id}")`
    inside the scope. (Resolve `slug` via `ws_manager.workspace_slug(workspace_id)`.)
  - `handle_source_workbook(job_id, payload)` → read `payload["workspace_id"]`, pass to
    `materialize_source(...)`. Chained `add_job("run_workbook", …)` at `:253` → include
    `"workspace_id": workspace_id`.
- **`apps/api/services/workbook/enrichment.py`**
  - `run_workbook_enrichment(...)` → add `workspace_id` param; wrap body in
    `workspace_scope(workspace_id)`. The two inner `query(Workbook.workspace_id)` reads
    (`:208`, `:235`) become redundant — keep value but source it from the scope param.
  - `handle_run_workbook` (`:1158`) → `with workspace_scope(payload["workspace_id"]):`
    then call `run_workbook_enrichment(..., workspace_id=payload["workspace_id"])`.
  - Audit `:615` / `:727` bare `LeadDB()` — if reached only via the scoped run, replace
    with `get_lead_store(workspace_id, slug)`; else thread workspace through.
- **`apps/api/services/workbook/refresh.py`**
  - `refresh_workbook(workbook_id, reason, workspace_id)` → add param + wrap in
    `workspace_scope`. Pass workspace_id down into `materialize_source` /
    `run_workbook_enrichment`.
  - `handle_refresh_workbook` (`:134`) → `with workspace_scope(payload["workspace_id"])`.
  - `_enqueue_next` (`:119`) / `_enqueue_next_now` (`:219`) / `set_refresh_policy`
    (`:149`) → thread `workspace_id` into the re-enqueued payload.
  - `handle_signal_scan` (`:164`) → **rewrite as workspace-enumerating** (§4.1.4):
    enumerate `workspaces`, and per workspace `with workspace_scope(ws.id):
    db.query(Workbook)…`. The self-re-enqueue of the next `signal_scan` stays global
    (it touches only the non-RLS `jobs` table). `bootstrap_signal_scan` unchanged.
- **`apps/api/services/workbook/worker.py`** (alternate Redis/BullMQ worker)
  - `process_enrich_cell` (`:43`) → the Redis `job_data` must now carry `workspace_id`
    (add at every `enqueue_enrichment_job` call site); wrap body in `workspace_scope`.
    `_load_row_data` (`:98`) bare `LeadDB()` (E2) → scoped store.
  - **Owner decision OD-7**: this Redis worker overlaps the durable-queue worker
    (`apps/api/worker.py` + `enrichment.handle_run_workbook`), which is the primary path.
    If it is dead code in production, prefer **deleting it** over hardening it (less
    surface). Confirm with owner before investing in scoping it.

### PR-B (RLS)

- New migration `migrations/versions/e5f6a7b8c9d0_workbooks_rls.py` (§5).
- Model edits (§5.1).
- **`apps/api/main.py:90-121`** — remove the runtime `UPDATE workbooks SET
  workspace_id …` backfill (it can't work under RLS and is now handled in the
  migration). Keep `ensure_tenancy_backfill(admin.id)` for workspace creation.
- Constructors that build child rows must pass `workspace_id`:
  `source_engine.py:175` `WorkbookRow(...)`, `routers/workbooks.py:236/250/265/397/1115/1205`,
  `refresh.py:37` `WorkbookActivity(...)`, `enrichment._set_enrichment` (writes
  `WorkbookEnrichment`), and any `CellTrace` writer. In request paths `ctx.workspace_id`
  is in scope; in worker paths the `workspace_scope` value is (read via
  `current_workspace_var.get()` or threaded explicitly).

### PR-C (chat/memory)

- **`apps/api/services/chat_history.py`** — add `workspace_id` (and `user_id`) columns to
  the `conversations` table; thread them through `create_conversation` /
  `list_conversations(workspace_id, user_id)` / `get_conversation` / `add_message` /
  `delete_conversation` (filter by workspace_id + user_id; 404 cross-tenant).
- **`apps/api/services/memory.py`** — partition memories by `workspace_id` (compose the
  Mem0/builtin `user_id` as `f"{workspace_id}:{user_id}"`, or add an explicit
  `workspace_id` column to `_BuiltinMemory`).
- **`apps/api/routers/copilotkit.py:143-163`** — add `Depends(current_workspace)` to the
  `/conversations` GET/DELETE/GET-one endpoints (they have **none** today) and pass
  `ctx.workspace_id`/`ctx.user.id`; replace hardcoded `user_id="default"` at `:1524`,
  `:1658` with the real workspace+user.

---

## 7. Tenancy / security

- **Fail-closed by construction.** Policy uses `current_setting('app.workspace_id',
  true)` → unset GUC = NULL = `workspace_id = NULL` is never true → 0 rows; `WITH CHECK`
  rejects writes whose `workspace_id` ≠ GUC (incl. NULL). Identical to leads/signals/
  outreach. No new code path may read a workbook to *discover* its workspace; the
  workspace always arrives out-of-band (request header, job payload, ws query param).
- **Superuser vs app role.** Migrations run as OWNER (`alembic upgrade head` in
  `db_init`); `FORCE` makes the policy bind to the owner too, so even the backfill in
  step 2 would be blocked **if** RLS were already on — hence backfill **precedes**
  ENABLE/FORCE in the same `upgrade()`. Runtime connects as `yupcha_app`
  (NOSUPERUSER/NOBYPASSRLS); the new `GRANT`s give it DML on the five tables + sequence
  usage. Tests connect as a non-super login role with `yupcha_app` granted (§9).
- **Defense in depth.** App-layer `workspace_id` filters (`_owned_workbook`, the
  copilotkit `Workbook.workspace_id == workspace_id` filters) **stay** — RLS is the
  backstop, not a replacement. With both, a missed app filter degrades to "scoped by
  RLS" instead of "cross-tenant leak".
- **Child-table integrity.** Denormalized `workspace_id` on child rows could in principle
  drift from the parent. Mitigate: always set it from the parent at insert; the policy +
  `WITH CHECK` prevent writing a child row under a different tenant than the active GUC,
  and the parent workbook is itself only reachable under that GUC.

---

## 8. Data migration / NULL `workspace_id` handling (OWNER DECISION OD-1)

Today `workbooks.workspace_id` is nullable and legacy rows may be NULL (the
`main.py:90-121` startup job backfills them to the `main` workspace, but only on a host
that has run that code; a fresh PG restored from older data may still have NULLs).
RLS requires a definite tenant for every row. Options:

- **OD-1a (recommended): backfill-then-block.** In the migration, *before* ENABLE/FORCE:
  - If **exactly one** workspace exists (self-host / single-tenant — the common OSS
    case), backfill all NULL workbooks to that workspace id, then cascade to children.
  - Else if a workspace with `slug = 'main'` exists, backfill NULLs to it (matches the
    current `main.py` behaviour).
  - Else (multiple workspaces, no obvious 'main', NULLs present) **raise and abort the
    migration** with an actionable message ("N workbooks have NULL workspace_id; run the
    provided backfill script assigning each to its owner before upgrading"). We must
    never guess a tenant for ambiguous data — a wrong guess is a cross-tenant leak.
  - After backfill, assert 0 NULLs, then `SET NOT NULL`.
- **OD-1b: block-only.** Refuse to migrate if any NULL exists; require operators to run
  a standalone backfill first. Safest but worst self-host UX.
- **OD-1c: drop orphans.** Delete NULL-workspace workbooks. Rejected — destructive.

Recommendation: **OD-1a**. Provide a tiny standalone backfill helper
(`apps/api/scripts/backfill_workbook_ws.py`) for the multi-tenant abort case.

Idempotency: the migration's `add_column`/policy steps are existence-guarded; backfill
`UPDATE … WHERE workspace_id IS NULL` is naturally idempotent; re-running
`alembic upgrade head` on an already-migrated DB is a no-op (alembic_version gate).

---

## 9. Test plan

All PG-gated tests gate on `TEST_DATABASE_URL` and skip otherwise (keeps the default
SQLite suite green), connecting as a **non-super, non-BYPASSRLS** login role granted
`yupcha_app` — mirror `tests/test_outreach_rls.py` exactly (fixtures `owner_engine`,
`schema` running `alembic upgrade head`, `app_engine`, `_set_ws`).

New file **`tests/test_workbooks_rls.py`** (PG-gated):

1. **Select isolation** — seed workbooks + rows + enrichments + activity + traces for
   `W1` and `W2` (as owner, GUC per batch). With GUC=W1, `SELECT DISTINCT workspace_id`
   on each of the five tables == `{W1}`; GUC=W2 == `{W2}`. (parametrized over tables.)
2. **Fail-closed, no GUC** — with no `app.workspace_id`, `count(*)` on each table == 0.
3. **Cross-tenant write rejected** — GUC=W1, `INSERT … workspace_id=W2` raises
   `ProgrammingError/DBAPIError` (WITH CHECK). Same for an UPDATE flipping workspace_id.
4. **Child write under wrong parent rejected** — GUC=W1, insert a `workbook_rows` row
   whose `workspace_id=W1` but `workbook_id` belongs to W2 → confirm it cannot be read
   by W2 and the W1↔parent relationship holds (the policy is column-based, so also
   assert the app always sets child `workspace_id` = parent's).
5. **FORCE flags** — `pg_class.relrowsecurity AND relforcerowsecurity` == (true,true)
   for all five tables (mirrors `test_i2c_tenant_tables_force_rls`).
6. **Grants** — `yupcha_app` has SELECT/INSERT/UPDATE/DELETE on each (via
   `has_table_privilege`).

New file **`tests/test_workbook_worker_rls.py`** (PG-gated) — the worker-under-RLS proof:

7. **Worker scopes from payload, not the row.** Build an `app_engine` with the
   `after_begin` GUC hook (copy the `test_i6_lead_in_two_workspaces` monkeypatch that
   binds `SessionLocal` to `app_engine` + installs the hook). Seed a workbook+source
   column in W1. Call `handle_source_workbook(job_id, {"workbook_id":…, "column_id":…,
   "workspace_id": W1})` (stub `JobRunner.submit`/lead store to return 2 leads) and
   assert rows are created **and visible only under W1**.
8. **Missing payload workspace fails loud, not silently** — calling the handler with no
   `workspace_id` raises (via `workspace_scope("")` ValueError), never processes rows
   cross-tenant.
9. **`handle_run_workbook` under W2 enriches only W2 rows** — seed identical wb ids
   structure in W1 and W2; run for W2; assert W1 enrichments untouched.
10. **signal_scan enumerates workspaces** — seed refresh-policy workbooks in W1 and W2;
    run `handle_signal_scan`; assert it enqueues a `refresh_workbook` job (with the right
    `workspace_id` in payload) for each matching workbook across both tenants —
    proving the global scan was correctly re-expressed as per-workspace scans.

**SQLite/default suite (always-on):**

11. `tests/test_workbooks_*` existing tests still pass (RLS DDL is PG-only; SQLite gets
    the new columns only). Add a unit test that every workbook-job enqueue site includes
    `workspace_id` in the payload (guards against regressions) and that
    `materialize_source`/`run_workbook_enrichment`/`refresh_workbook` raise if called
    without a workspace.
12. **Migration round-trip** — `alembic upgrade head` then `downgrade -1` on a PG
    throwaway leaves the five tables present and policy-free (run in CI's PG job).

**PR-C tests:** `tests/test_chat_history_tenancy.py` — two workspaces, conversations
created under each are not visible/deletable across tenants; `/conversations` requires
auth; memory search/add partitioned by workspace.

Run command (matches the repo convention):
```
TEST_DATABASE_URL='postgresql+psycopg://user4@localhost:5432/<throwaway>' \
PYTHONPATH=. uv run --group dev --with 'psycopg[binary]' \
python -m pytest tests/test_workbooks_rls.py tests/test_workbook_worker_rls.py -q
```

---

## 10. Numbered acceptance criteria

1. `workbooks`, `workbook_rows`, `workbook_enrichments`, `workbook_activity`,
   `cell_traces` have `ENABLE`+`FORCE` ROW LEVEL SECURITY and a fail-closed
   `USING`+`WITH CHECK` policy on `current_setting('app.workspace_id', true)` (PG).
2. `yupcha_app` has SELECT/INSERT/UPDATE/DELETE + sequence usage on all five tables.
3. With no GUC, `count(*)` on each of the five tables is 0 (fail closed); cross-tenant
   INSERT/UPDATE raises.
4. The four child tables have a non-null `workspace_id` column, backfilled from the
   parent; `workbooks.workspace_id` is `NOT NULL`.
5. Every workbook-job enqueue site (§2.4) stamps `workspace_id` into the payload; every
   worker handler enters `workspace_scope(payload["workspace_id"])` before any workbook
   query; none read the workbook row to discover its workspace.
6. Sourcing/enrichment/refresh produce identical results with RLS ON as with RLS OFF for
   a correctly-scoped tenant (worker-under-RLS tests pass).
7. `handle_signal_scan` triggers the correct workbooks across all tenants while running
   each tenant's work under its own scope.
8. Bare `LeadDB()` read-backs in `source_engine`/`workbook/worker` are replaced by the
   scoped lead store.
9. Workbook websockets authorize correctly under RLS (workspace passed out-of-band).
10. The `main.py` startup NULL backfill is removed; the migration performs the backfill
    (OD-1a) or aborts with a clear message on ambiguous multi-tenant NULLs.
11. SQLite/self-host path is unchanged behaviourally; default test suite stays green.
12. `alembic upgrade head` / `downgrade -1` round-trips cleanly on PG; migration is
    idempotent on re-run.
13. (PR-C) chat_history + memory are workspace-partitioned; `/conversations` endpoints
    require auth and never return another tenant's data.

---

## 11. Rollout / flags

- No feature flag on the RLS itself — RLS is schema state, enforced once the migration
  applies (consistent with how leads/signals/outreach shipped).
- **Deploy order is the safety mechanism:** ship PR-A (scoping, RLS off) → soak → ship
  PR-B (migration). In a combined release they deploy atomically (`db_init` runs alembic
  at startup before serving). Roll back = `alembic downgrade -1` (policies drop; columns
  drop; app-layer filters still protect, same as pre-#90 posture).
- `YUPCHA_APP_DB_ROLE` env override already respected by the migration template.
- Self-host (`IS_SQLITE`): no RLS, no role, columns-only — zero operator action.

---

## 12. Out of scope

- The full `LeadDB()` census outside the workbook worker path (cli/hubspot/settings/
  leads-router) — those touch `leads` (already RLS) and are unaffected by workbook RLS.
- Changing the `jobs` table to carry `workspace_id` as a column (payload carries it;
  jobs stay non-RLS like the other ticker/queue tables).
- The workbook data model, queue substrate, lead store internals.
- Mem0 vendor/provider selection; only its partitioning key changes (PR-C).

---

## OWNER DECISIONS (summary)

- **OD-1 (NULL workspace_id):** backfill-then-block (OD-1a) — single-ws/`main` auto-
  backfill, abort on ambiguous multi-tenant NULLs. Confirm.
- **OD-2 (scope of RLS tables):** do all five (workbooks + rows + enrichments + activity
  + traces) in one migration, vs the minimum two named in the brief. Recommend all five.
- **OD-3 (PR split):** 3 PRs (A scoping / B RLS / C chat+mem0) vs 2 (combine A+B) vs 1.
  Recommend 3 for a soak window; A+B may combine if soak isn't wanted.
- **OD-4 (worker tenant signal):** stamp `workspace_id` into job **payload** (recommended)
  vs add a `workspace_id` column to the `jobs` table. Recommend payload.
- **OD-5 (websocket auth):** pass `workspace_id` as a ws query param from the client
  (recommended; minimal) vs a narrow `SECURITY DEFINER` workbook→workspace resolver.
- **OD-6 (chat/mem0 key):** partition by `workspace_id` only, or `workspace_id`+`user_id`
  (recommended — preserves per-user memory within a tenant).
- **OD-7 (`services/workbook/worker.py`):** harden the Redis/BullMQ worker, or delete it
  if the durable-queue worker is the only production path. Recommend delete-if-dead.

## EFFORT ESTIMATE

- **PR-A (scoping, RLS off):** ~1.5–2 days. Payload stamping + handler scope wrapping is
  mechanical; the real work is the `handle_signal_scan` workspace-enumeration rewrite,
  the websocket auth change, and the bare-`LeadDB()`→scoped-store swaps + their tests.
- **PR-B (migration + models + backfill):** ~1.5 days. Migration is a close clone of
  `b2c3d4e5f6a7`; the child-table columns/backfill/NOT-NULL + the NULL-decision logic +
  the two PG-gated test files are the bulk.
- **PR-C (chat_history + Mem0):** ~1 day. Schema + signature threading + adding the
  missing `/conversations` auth + tenancy tests.
- **Total: ~4–4.5 engineer-days** (excludes soak time between A and B). Risk
  concentrated in PR-B's cutover; PR-A de-risks it by landing the scoping first.
