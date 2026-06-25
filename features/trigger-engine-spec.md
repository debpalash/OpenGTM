<!-- Auto-generated design spec (2026-06-25). Review verdict: major-rework. -->

## ✅ LOCKED SCOPE DECISIONS (owner-approved 2026-06-25)

1. **v1 actions = re-enrich column(s) + CRM push + webhook ONLY.** Enroll-in-sequence and send-email are DEFERRED to a follow-up WI that RLS-hardens the legacy `outreach.db` store. `AUTOMATIONS_ALLOW_LEGACY_OUTREACH` stays absent/OFF; those action types are rejected at rule-create in v1.
2. **`on_signal` is PG-only** (requires `PG_LEAD_STORE=true`). Self-host (PG_LEAD_STORE=false) gets `on_row_added` / `on_row_changed` / `on_schedule`; creating an `on_signal` rule without PG returns `409 on_signal_requires_pg_lead_store`. No legacy global-SQLite signal scanner in v1.
3. Implementation-detail defaults accepted: `on_row_changed` uses `updated_at` epoch-ms (no extra cell_version column); scheduler bootstrap via a non-RLS `scheduled_triggers` mirror (no BYPASSRLS role); webhook actions require workspace OWNER + an enforced domain allowlist on cloud.


# Production Spec — Signal→Action Trigger Engine ("Automations / Recipes")

> **STATUS: build-ready after adversarial review.** Every blocking gap, edge case, tenancy/security issue, cost/idempotency issue, and test gap from the review has been folded in. See **"Changes after review"** at the end for the diff-level summary. Items that genuinely need a human decision are flagged inline with **[OWNER DECISION]** and collected in the open-questions list.

## 0. Summary

A **tenant-scoped rule** binds a **trigger** (on new signal / on row added-or-changed / on schedule) to a **condition** (predicate over signal/row/lead fields) and an **ordered list of actions** (re-enrich column(s), push to CRM/webhook, enroll in outreach sequence, send email). Evaluation rides the existing durable job queue (`apps/api/services/queue_service.py`). Every paid action goes through the existing spend-ceiling / credit-ledger chokepoint (`apps/api/services/billing/service.py:check_and_debit`) and is idempotent (no double-charge, no double-send). The four core actions already exist as workbook output primitives (`apps/api/services/workbook/output.py:execute_output_column`) — the new work is the standalone **rule object** that fires them outside a workbook run, plus the row-change/signal-match event source, dedup/idempotency layer, dry-run, pause/resume, per-rule + global spend caps, and observability.

**Design principle (per brief): reuse existing infra, add the missing rule entity + event hooks.** We reuse: queue + handler/self-reschedule (`refresh.py`), `output.execute_output_column` (action executors), `billing.check_and_debit`/`projected_platform_cost`, `get_secret` for creds, the RLS table pattern (migration `c42d0273d9bd`), and the `WorkbookActivity` feed. We add: a `triggers`/`trigger_runs`/`trigger_action_results` data model, a `trigger_eval` job type, a row-change / per-row signal-match emitter, a per-action idempotency key, and a per-rule + global spend cap.

### CRITICAL CORRECTION (review blocking gap #1) — the engine fires off the **workbook/PG** plane only, NOT the legacy global SQLite signal/lead/outreach stores

The review correctly established that the firing path described in the original spec was built on infra that does not exist as described:

- `run_signal_scan()` (`signals/monitor.py:212`) reads leads via the **global SQLite `LeadDB()`**, writes signals via `add_signal()` into the **single global SQLite file `data/signals.db`** (`monitor.py:_get_db`), with `Signal.workspace_id` defaulting to `""` and never set. These signals are **outside RLS** and **have no real workspace**.
- The PG RLS `leads`/`signals` tables (migration `c42d0273d9bd`) are written **only** by `PgLeadStore` when `PG_LEAD_STORE=true`; the scan path never touches them.
- `enroll_leads(seq_id, lead_ids)` (`outreach/sequence.py:212`) and `send_email`/`outreach.db` are **global, unscoped SQLite**, addressed by a per-file integer `lead_id` that is **not globally unique across workspaces**.

**Decision: the engine operates exclusively on the workbook plane.** A rule's universe of leads is the set of `WorkbookRow`s in workspaces it is scoped to (always RLS Postgres, GUC-isolated). The engine never reads `signals.db`, never reads `LeadDB()`, and never addresses a bare legacy integer `lead_id`. Concretely:

1. **`on_signal` is re-grounded on workbook cells, not the legacy scan.** Instead of querying `signals.db`, we add an explicit, workspace-stamped **signal-match emitter on the write path that already has tenancy**. There are two supported configurations, gated by `PG_LEAD_STORE`:
   - **`PG_LEAD_STORE=true` (cloud target):** `PgLeadStore.add_signal` (the only writer that has a real `workspace_id`) calls `automations.events.emit_signal_matches(db, workspace_id, signal_rows)` after insert, inside the existing workspace scope. `fire_key="signal:<signal_pk>"` where `<signal_pk>` is the PG row PK (globally unique).
   - **`PG_LEAD_STORE=false` (legacy self-host):** `on_signal` is **unavailable** and rejected at rule-create with `409 on_signal_requires_pg_lead_store`. The legacy global scan is **not** wired as an event source — doing so cannot produce a real `workspace_id` and would be a cross-tenant leak. This is the honest scope cut the review demanded; documented in §10 and acceptance AC-4.
   - **[OWNER DECISION]** whether to also build a workspace-stamped signal scanner so self-host gets `on_signal` — out of scope here, tracked as a follow-up WI.
2. **`re_enrich`, `on_row_changed`, `on_row_added`, `on_schedule`** all operate on `WorkbookRow`/workbook columns (RLS PG) and are fully available on both configs.
3. **`sequencer` and `send_email` actions are guarded by a new per-workspace ownership/scoping layer (§6.4)** because their underlying stores are global. They are **opt-in** via `AUTOMATIONS_ALLOW_LEGACY_OUTREACH` (default **False**); when off, those two action types are rejected at create. This closes the live cross-tenant write the review found.

---

## 1. Goal & User-Facing Behavior

### What it does
A user creates a **rule** ("recipe") in a workspace. Example rules:

1. **"Hot hiring lead → enrich + push to CRM"** (requires `PG_LEAD_STORE=true`)
   - Trigger: `on_signal` (signal_type ∈ {hiring, funding}) — fires from a workspace-stamped PG signal insert.
   - Condition: `{score} > 70 AND {email} != ""`
   - Actions (ordered): (a) re-enrich columns `[email, phone]` (resolved to column UUIDs, §3.5); (b) push to HubSpot; (c) enroll in sequence `welcome_v2` (only if `AUTOMATIONS_ALLOW_LEGACY_OUTREACH` and ownership-validated).

2. **"Row got an email → notify Slack webhook"**
   - Trigger: `on_row_changed` (watch field `email`, fires when it transitions empty→non-empty)
   - Condition: `{email} contains "@"`
   - Actions: (a) POST to webhook URL (SSRF + DNS-pinning guarded, §6.2).

3. **"Nightly sweep of stale enterprise leads"**
   - Trigger: `on_schedule` (daily, wall-clock-aligned, §3.7)
   - Condition: `{company_size} == "enterprise" AND {last_enriched_at older_than 30d}`
   - Actions: (a) re-enrich `[revenue_estimate]`; (b) send templated email (ownership-validated).

### User-visible behavior
- Create / edit / list / get / delete rules via REST.
- **Pause / resume** a rule (paused rules are never evaluated; in-flight runs finish).
- **Dry-run / preview**: "show me which rows match right now and what each action *would* do/cost, without sending or charging." Returns matched-row count, per-action projected cost, and a sample of resolved payloads.
- **Per-rule spend cap** (`max_spend_usd_per_day`, `max_actions_per_day`) and a **workspace-global automations cap** (`AUTOMATIONS_GLOBAL_DAILY_USD`).
- **Run history**: each evaluation produces a `trigger_run` with status, matched rows, actions attempted/succeeded/skipped/charged, cost, and per-action error detail.
- Idempotent: re-firing on the same (rule, row, action, fire-key) never double-charges or double-sends.

### Concrete trigger semantics
- `on_signal`: fires when a **PG (RLS) signal** is stored for a workbook-resident lead whose `signal_type` ∈ rule's watched types. Per-lead event emitted from the write path that already carries `workspace_id` (§3.4). **Requires `PG_LEAD_STORE=true`.**
- `on_row_changed`: fires when a watched cell value changes (incl. empty→value). Edge-triggered by a transition test against a **captured prior value** (§3.6). `fire_key` is content+version derived (§3.6) so genuine re-transitions are not dedup-swallowed.
- `on_row_added`: fires when a new `WorkbookRow` is inserted. Emitted from the **four enumerated insertion sites** (§5).
- `on_schedule`: `{hourly|daily|weekly}` self-re-enqueue, **wall-clock-aligned with a single-flight guard** (§3.7); evaluates the condition across candidate rows in scope, **chunked with a stable snapshot cursor** (§3.8).

---

## 2. Architecture & Data Flow

### Components
- **Rule store** — `triggers` table (RLS, workspace-scoped). One row = one rule.
- **Run store** — `trigger_runs` (one per evaluation) + `trigger_action_results` (one per action-per-row, carries the idempotency key).
- **Event sources** (what enqueues a `trigger_eval` job):
  1. **PG signal-match emitter** — `automations/events.py:emit_signal_matches(db, workspace_id, signal_rows)` called from `PgLeadStore.add_signal` (the only signal writer with a real `workspace_id`). Enqueues `trigger_eval` for matching `on_signal` rules, carrying matched `workbook_id`+`row_id`s and `fire_key="signal:<signal_pk>"`. (Legacy global scan is **not** an event source — blocking gap #1.)
  2. Row-change emitter — `automations/events.py:on_rows_changed(workspace_id, workbook_id, changes)` called from the enrichment write-back paths (§3.6 covers prior-value capture across BOTH the overlay/`WorkbookEnrichment` path and the `_write_back_to_lead` path) and from the four row-insert sites.
  3. Scheduler — each `on_schedule` rule self-re-enqueues a `trigger_eval` job (the `refresh._enqueue_next` pattern, hardened against drift/duplication, §3.7), bootstrapped on rule create/resume and via a **per-workspace bootstrap loop** at startup (§3.7).
- **Evaluation engine** — `trigger_eval` job handler (`automations/engine.py:handle_trigger_eval`). Loads the rule, resolves candidate rows (always `WorkbookRow`s), runs the **hardened condition evaluator** (§3.9) per row, and for each match invokes the **action executor** in order.
- **Action executors** — `automations/actions.py`. Thin wrappers over `output.execute_output_column` (webhook/crm/sequencer) + a new `re_enrich` action that calls `enrichment.run_workbook_enrichment(workbook_id, column_ids=[...], lead_ids=[...])` (note: **`column_ids`, not field names** — see §3.5) and a `send_email` action over `outreach/sender.py:send_email`. Each paid action is gated by `billing.check_and_debit` and an idempotency key.
- **Spend gate** — per-action projection that **reuses the router's exact projection inputs** (`providers_by_column` waterfall, §3.5/§7); checked against (a) the credit ledger, (b) per-rule daily cap, (c) global daily cap — caps enforced with a **reservation row to close the read-then-act race** (§7).

### Sequence (on_signal example, PG_LEAD_STORE=true)
```
PgLeadStore.add_signal(workspace_id, lead, signal)  [inside workspace_scope]
  → emit_signal_matches(db, ws, signal_rows):
       for each enabled on_signal rule whose signal_types match,
         find the WorkbookRow(s) for that lead in the rule's scope,
         enqueue trigger_eval{rule_id, workspace_id, targets:[{workbook_id,row_id}],
                              fire_source:"signal", fire_key:"signal:<signal_pk>"}
  → queue_service claims job (FOR UPDATE SKIP LOCKED)
  → engine.handle_trigger_eval wraps work in tenancy.workspace_scope(ws_id):
        if not settings.AUTOMATIONS_ENABLED: return
        rule = load+snapshot; if missing/disabled: mark run skipped; return
        create trigger_run(status=running); db.commit()   # run row survives later billing rollbacks
        for target in targets (≤ AUTOMATIONS_MAX_ROWS_PER_EVAL):
            cells, cols_cfg = load WorkbookRow cells + columns_config (RLS)
            if not safe_evaluate_condition(rule.condition, cells, cols_cfg): record skip; continue
            for action in rule.actions (ordered):
                idem = f"trig:{ws}:{rule_id}:{workbook_id}:{row_id}:{action.idx}:{fire_key}"
                if action_result exists for idem and status in (success,skipped,charged_only):
                    record replay-skip; continue                       # no double-send
                if action.paid:
                    cost = project(action, cells, cols_cfg)             # real waterfall inputs §7
                    reserve = caps.try_reserve(db, rule, ws, cost)      # atomic, closes race §7
                    if not reserve.ok: persist result(status=skipped, reason=cap); continue
                    try: check_and_debit(db, ws, cost, run_id=f"run:{idem}")  # COMMITS internally
                    except InsufficientCredits: caps.release(reserve);
                        persist result(status=skipped, reason=credits); continue
                # PRE-SEND MARKER (closes crash-between-debit-and-send gap, §7):
                persist trigger_action_result(idem, status="in_flight"); db.commit()
                result = execute(action, ...)                           # external side effect
                update trigger_action_result(idem, status=result.status, summary, error); db.commit()
        finalize trigger_run(status=completed/partial/failed)
  → on_schedule rules: handler re-enqueues next aligned trigger_eval with single-flight guard
```

### How it rides existing infra
- **Queue**: `trigger_eval` is registered exactly like `signal_scan` in BOTH `apps/api/main.py` and `apps/api/worker.py:_register_handlers`, with a `JOB_TIMEOUTS["trigger_eval"]` entry. Handler signature `async def handle_trigger_eval(job_id: int, payload: dict)`.
- **Tenancy**: `jobs` has no `workspace_id` (confirmed `queue_service.add_job` / `models.Job`), so the payload **carries `workspace_id`** and the handler wraps all DB work in `tenancy.workspace_scope(workspace_id)` so RLS GUC `app.workspace_id` is set.
- **Billing**: action-level chokepoint reuses `check_and_debit` verbatim with a per-action `run_id`/idempotency key that is **workspace-prefixed** (§7, closes the ledger cross-tenant idem-collision finding).

---

## 3. Data Model

New ORM module: `apps/api/services/automations/models.py`, on the single `Base` from `apps/api/database.py`.

### 3.1 `triggers` (RLS, workspace-scoped)
| column | type | notes |
|---|---|---|
| `id` | String(36) PK | uuid4 |
| `workspace_id` | String(64) NOT NULL | RLS key, indexed |
| `name` | String(200) NOT NULL | |
| `enabled` | Boolean NOT NULL default True | pause/resume flag |
| `trigger_type` | String(32) NOT NULL | `on_signal`/`on_row_changed`/`on_row_added`/`on_schedule` |
| `trigger_config` | JSON NOT NULL default `{}` | e.g. `{signal_types:[...]}`, `{watch_fields:[...], workbook_ids:[...]}`, `{interval:"daily", workbook_ids:[...]}` |
| `condition` | Text default "" | expression for the **hardened** evaluator (§3.9) |
| `actions` | JSON NOT NULL default `[]` | ordered list; each `{type, config}` (see §3.5) |
| `scope_workbook_ids` | JSON default `[]` | empty = all workbooks in ws |
| `stop_on_error` | Boolean NOT NULL default False | per-rule action-loop policy |
| `max_spend_usd_per_day` | Float nullable | per-rule daily spend cap (NULL = unlimited, still bounded by global) |
| `max_actions_per_day` | Integer nullable | per-rule daily action count cap |
| `next_run_at` | DateTime(timezone=True) nullable | for `on_schedule` self-reschedule (tz-aware, §3.7) |
| `schedule_anchor` | DateTime(timezone=True) nullable | wall-clock anchor for aligned scheduling (§3.7) |
| `last_fired_at` | DateTime(timezone=True) nullable | |
| `created_by` | String(64) nullable | user id |
| `created_at` / `updated_at` | DateTime(timezone=True) server_default now / onupdate | |

Indexes: `ix_triggers_workspace_id (workspace_id)`, `ix_triggers_ws_type (workspace_id, trigger_type)`, `ix_triggers_ws_enabled (workspace_id, enabled)`, `ix_triggers_ws_next_run (workspace_id, trigger_type, next_run_at)`.

### 3.2 `trigger_runs` (RLS)
| column | type | notes |
|---|---|---|
| `id` | String(36) PK | uuid4 |
| `workspace_id` | String(64) NOT NULL | RLS key |
| `trigger_id` | String(36) NOT NULL | FK-by-convention (no hard FK, matches repo style) |
| `trigger_name_snapshot` | String(200) nullable | snapshot so run history survives rule deletion (edge: orphaned run) |
| `job_id` | Integer nullable | the `jobs.id` that ran it |
| `status` | String(20) | `running`/`completed`/`partial`/`failed`/`dry_run`/`skipped` |
| `fire_source` | String(32) | `signal`/`row_changed`/`row_added`/`schedule`/`manual` |
| `fire_key` | String(200) nullable | dedup root |
| `sweep_cursor` | String(200) nullable | for chunked schedule sweeps (§3.8) |
| `matched_rows` | Integer default 0 | |
| `actions_attempted` / `actions_succeeded` / `actions_skipped` / `actions_failed` | Integer default 0 | |
| `total_charged_usd` | Float default 0.0 | |
| `error` | Text nullable | |
| `started_at` / `finished_at` | DateTime(timezone=True) | |

Indexes: `ix_trigger_runs_ws (workspace_id)`, `ix_trigger_runs_ws_trigger_started (workspace_id, trigger_id, started_at)`.

### 3.3 `trigger_action_results` (RLS) — the idempotency + dedup ledger
| column | type | notes |
|---|---|---|
| `id` | Integer PK autoincrement | |
| `workspace_id` | String(64) NOT NULL | RLS key |
| `run_id` | String(36) NOT NULL | → trigger_runs.id |
| `trigger_id` | String(36) NOT NULL | |
| `workbook_id` | String(36) NOT NULL | row provenance (lead_id alone is not globally unique) |
| `row_id` | String(36) NOT NULL | WorkbookRow id (UUID, not legacy int) |
| `lead_id` | Integer nullable | informational only; never used as a key |
| `action_index` | Integer NOT NULL | position in `actions` |
| `action_type` | String(32) NOT NULL | |
| `idempotency_key` | String(255) NOT NULL | `trig:<ws>:<trigger_id>:<workbook_id>:<row_id>:<action_index>:<fire_key>` |
| `status` | String(20) | `in_flight`/`success`/`failed`/`skipped`/`charged_only` |
| `skip_reason` | String(40) nullable | `replay`/`cap`/`credits`/`no_target`/`condition`/`blocked_url`/`not_connected` |
| `charged_usd` | Float default 0.0 | |
| `result_summary` | Text nullable | |
| `error` | Text nullable | |
| `created_at` / `updated_at` | DateTime(timezone=True) server_default now / onupdate | |

Constraints/indexes:
- `UniqueConstraint("workspace_id", "idempotency_key", name="uq_trigger_action_idem")` — **workspace-scoped** unique key (closes the cross-tenant collision finding; even though RLS isolates rows, the constraint is explicitly ws-prefixed).
- `ix_tar_ws_trigger_created (workspace_id, trigger_id, created_at)` — drives per-rule daily cap counting.
- `ix_tar_ws_created (workspace_id, created_at)` — drives global daily cap.

### 3.3a `trigger_cap_reservations` (RLS) — closes the cap read-then-act race (review cost issue #1/#2)
A small reservation table so concurrent `trigger_eval` jobs cannot both pass a near-full cap. Reserve before debit, settle/release after.
| column | type | notes |
|---|---|---|
| `id` | Integer PK autoincrement | |
| `workspace_id` | String(64) NOT NULL | RLS key |
| `trigger_id` | String(36) NOT NULL | |
| `day_utc` | String(10) NOT NULL | `YYYY-MM-DD` UTC bucket (explicit, not derived from naive created_at) |
| `reserved_usd` | Float NOT NULL | |
| `reserved_actions` | Integer NOT NULL default 1 | |
| `idempotency_key` | String(255) NOT NULL | same as the action's idem; one reservation per action |
| `state` | String(12) NOT NULL | `held`/`settled`/`released` |
| `created_at` | DateTime(timezone=True) server_default now | |

Constraints/indexes:
- `UniqueConstraint("workspace_id", "idempotency_key", name="uq_cap_reservation_idem")` — retry-safe.
- `ix_capres_ws_trig_day (workspace_id, trigger_id, day_utc, state)` and `ix_capres_ws_day (workspace_id, day_utc, state)` — drive `SUM(reserved_usd)` cap queries.

Reservation algorithm (`caps.try_reserve`): in a single statement, `SELECT … FOR UPDATE` the day-bucket rows (or use `INSERT … ON CONFLICT DO NOTHING` + a serialized `SUM`) to compute committed+held spend, and if `held+new ≤ cap` insert a `held` reservation; else return not-ok. After a successful debit the reservation is marked `settled`; on debit failure or cap-skip it is `released`. This makes the cap check-and-increment **atomic per workspace/day**, eliminating the burst overshoot the original author admitted to.

### 3.4 The "signal matched lead X" event (per brief gap #2, re-grounded — review blocking gap #1)
There is no per-row signal-match event today. The original design (query `signals.db` grouped by workspace_id) is **invalid** because that store has empty `workspace_id` and is outside RLS. Re-grounded implementation:
- Only `PgLeadStore.add_signal` (PG RLS, real `workspace_id`, runs inside `workspace_scope`) calls `emit_signal_matches(db, workspace_id, [signal_row])`.
- `emit_signal_matches` looks up enabled `on_signal` rules in the workspace whose `signal_types` intersect the signal's type, maps the signal's lead to its `WorkbookRow`(s) within each rule's `scope_workbook_ids` (via the workbook↔lead linkage already used by `PgLeadStore`), and enqueues one `trigger_eval` per rule with `targets=[{workbook_id,row_id},…]` and `fire_key="signal:<signal_pk>"` (PG PK → globally unique → idempotent across rescans).
- If `PG_LEAD_STORE=false`, `on_signal` is unavailable (rejected at create); the global scan is not wired. Documented limitation, AC-4.

### 3.5 Action JSON shapes (stored in `triggers.actions`)
```
{type:"re_enrich",  config:{column_ids:["<uuid>", ...]}}          # column UUIDs, NOT field names
{type:"push_crm",   config:{type:"hubspot", field_map:{...}}}
{type:"webhook",    config:{url:"https://...", method:"POST", header_secret_ref?:"<key>", body:{...}}}
{type:"sequencer",  config:{sequence_id:"..."}}                    # gated; ownership-validated §6.4
{type:"send_email", config:{subject:"...", body:"...", to_field:"email"}}  # gated; §6.4
```
**re_enrich column resolution (review blocking gap #2):** the action stores **workbook column UUIDs** (`column_ids`), matching the real `run_workbook_enrichment(workbook_id, column_ids=[...], lead_ids=[...])` signature — there is no field-name→column_id mapping to invent. The create-time validator (§4) resolves the human-friendly names the UI shows into UUIDs against the rule's scoped workbook(s) and stores the UUIDs. A `re_enrich` action is **only valid when the rule's scope resolves to concrete workbook columns**; for any target row whose workbook lacks all of the configured `column_ids`, the action is recorded `status="skipped", skip_reason="no_target"` (this also covers the former "lead not in any workbook" edge — there is no lead-level re_enrich; see §3.10).

**webhook auth secrets (review blocking gap #9):** `header_secret_ref` names a per-workspace secret key; the executor resolves it via `get_secret(workspace_id, key)` and injects it as an `Authorization`/custom header at send time. Secrets are **never** stored in `trigger_config` (only the ref). This requires a small extension to `_send_webhook` to accept `workspace_id` and an optional secret-header ref (§5).

### 3.6 `on_row_changed` prior-value capture + deterministic fire_key (review edge cases)
**Prior-value capture (across both write paths):** the enrichment write-back has two distinct paths — overlay/AI cells (`WorkbookEnrichment`) and lead-field writes (`_write_back_to_lead`). For each, we read the current cell value **before overwrite** and pass `changes=[{row_id, field, old, new}]` to `on_rows_changed`. Implementation: a tiny `read_current_values(db, workbook_id, row_ids, fields)` helper invoked immediately before each write batch in both paths; the deltas (old≠new) are accumulated and emitted after the write commits. Only watch-fields configured by some enabled `on_row_changed` rule are read (bounded cost; emitter is a no-op when `AUTOMATIONS_ENABLED=false`).

**Deterministic, transition-aware fire_key:** `fire_key = f"rowchg:{row_id}:{field}:{sha1(old)[:8]}->{sha1(new)[:8]}:{cell_version}"` where `cell_version` is the monotonically increasing per-cell write counter (added as a column on the cell write, or derived from the existing `updated_at` epoch-ms of the cell). Including `cell_version` makes a genuine `empty→A→empty→A` oscillation produce **distinct** fire_keys (each write bumps the version), so the second legitimate transition is **not** dedup-swallowed, while a pure retry of the same write reuses the same `cell_version` and is correctly deduped. **[OWNER DECISION]** whether to add an explicit `cell_version` integer column vs. reuse `updated_at` epoch-ms — both work; epoch-ms avoids a migration on the cell tables but risks collision at sub-ms write bursts. Default in this spec: reuse `updated_at` epoch-ms; flagged for owner.

### 3.7 `on_schedule`: wall-clock alignment, drift, single-flight, multi-replica bootstrap (review edge cases + blocking gap #8)
- **Wall-clock alignment:** instead of `next = now + interval_minutes` (which drifts), `next_run_at` is computed from `schedule_anchor` so a `daily` rule fires at a fixed wall-clock time: `next = schedule_anchor + ceil((now - anchor)/interval) * interval`. `daily`/`weekly` use calendar arithmetic (tz-aware UTC) so they don't creep across the day.
- **Single-flight against duplicate chains (drift/duplication bug):** before self-re-enqueue and in `bootstrap_schedules`, we `SELECT … FOR UPDATE` the trigger row and only enqueue if there is **no existing pending/processing `trigger_eval` for that trigger_id** AND `next_run_at` is being advanced atomically (we set `triggers.next_run_at` in the same transaction that enqueues). This prevents the "completed-and-rescheduled + bootstrap on a second replica both enqueue" doubling. The pending-job check keys on a deterministic dedup column: the enqueued schedule job carries `fire_key="sched:<trigger_id>:<next_run_at_iso>"` and `queue_service` is asked to **skip if a job with the same `fire_key` is already pending** (a `WHERE NOT EXISTS` guard in the enqueue, mirroring `bootstrap_signal_scan:204` but keyed on fire_key, not just type).
- **Cross-workspace cold-start bootstrap (blocking gap #8):** `bootstrap_schedules()` cannot rely on RLS (GUC unset → zero rows). It runs a **dedicated maintenance path** that enumerates workspaces with enabled `on_schedule` rules using a **`SET LOCAL row_security = off`** session (the migration's APP role already has table privileges; the bootstrap uses a privileged maintenance connection that bypasses RLS *only* to read `(workspace_id, trigger_id, next_run_at)` of `triggers` — no row contents leave the loop), then for each due rule re-enters `workspace_scope(ws)` to enqueue. **[OWNER DECISION]** acceptable to grant the bootstrap a `BYPASSRLS` maintenance role, or should we instead maintain a separate non-RLS `scheduled_triggers(workspace_id, trigger_id, next_run_at)` mirror table written on rule create/update? Default in this spec: the non-RLS mirror table (`scheduled_triggers`, no row contents beyond ids+timestamp) — it avoids a BYPASSRLS role and is fail-safe; the bootstrap reads the mirror, then scopes per-workspace to act. This mirror is updated transactionally on create/resume/pause/delete.

### 3.8 Large-sweep chunking with stable snapshot cursor (review edge cases + cost issue)
A schedule sweep of N rows is chunked into jobs of ≤`AUTOMATIONS_MAX_ROWS_PER_EVAL`. To avoid double/missed actions when the candidate set shifts between chunks:
- The first sweep job records an immutable **snapshot cursor**: the ordered set of candidate `row_id`s is captured (a `sweep_id = uuid4` plus an ordered keyset on `(created_at, row_id)`), persisted on the `trigger_run`. Each chunk processes a keyset window of that frozen ordering; rows added after the snapshot are **not** included this sweep (they'll be picked up next sweep), and rows deleted are skipped harmlessly.
- `fire_key` for a scheduled action is `sched:<trigger_id>:<sweep_id>` (one stable root per sweep, **not** per-timestamp), so retries dedup and the schedule-duplication-multiplies-spend path is closed (a duplicate chain would have a different `sweep_id` only if it's a genuinely new sweep; the single-flight guard in §3.7 prevents the duplicate chain in the first place).

### 3.9 Hardened condition evaluator for paid-action gating (review edge case — the most dangerous one)
`conditions.evaluate_condition` is **unsafe to gate spend**: `_eval_single` returns `True` for any unparseable expression (`conditions.py:143`), compound parsing is a naive string split on ` AND `/` OR ` (`conditions.py:66-72`), and `_resolve_placeholders` quote-wraps values so a value containing `"` or ` AND ` corrupts the predicate and silently falls through to `True`. For **credit-saving** that's a cheap false-True; for **firing paid actions** it means unexpected spend/sends.

We add `automations/safe_conditions.py:safe_evaluate_condition(condition, cells, cols)` that:
1. Parses the condition with a **proper tokenizer** (not string-split): placeholders are substituted as typed operands, not interpolated into the string, so a value containing `"`, ` AND `, or ` OR ` cannot change the parse tree. (Equivalent: parse the operator structure first against `{field}` tokens, then bind values.)
2. **Fails CLOSED for paid actions**: if the expression is unparseable or any operand cannot be resolved, `safe_evaluate_condition` returns `False` (do NOT fire) and records `skip_reason="condition"` with the parse error. (This is the inverse of the legacy default-True; the legacy evaluator remains for the credit-saving workbook path and is untouched.)
3. Create-time validation (§4) runs the parser against a **representative populated row** including adversarial sample values (a value with embedded `"` and ` AND `) — not just an empty row — to surface injection-style failures early.

### 3.10 No lead-level firing (resolves the structurally-incompatible "bare lead row" edge)
The reused evaluator and `re_enrich` both require `row_cells` keyed by `column_id` plus `columns_config`. A bare lead row has neither. Therefore the engine **only** operates on `WorkbookRow`s. `on_signal` maps a signal to its workbook row(s) (§3.4); if a signal's lead is in **no** scoped workbook, **no** `trigger_eval` is enqueued (recorded as a no-match in metrics, no run). This deletes the original "lead-level firing" claim that the review proved structurally impossible.

### 3.11 Alembic migration plan
New revision `xxxx_automations_triggers.py`, **`down_revision = 'c42d0273d9bd'`** (current head). Mirrors the head migration's RLS pattern exactly:
- `op.create_table` for `triggers`, `trigger_runs`, `trigger_action_results`, `trigger_cap_reservations`, and `scheduled_triggers` (non-RLS mirror, §3.7) (+ indexes + unique constraints).
- Postgres-guarded `if op.get_bind().dialect.name == "postgresql": _pg_upgrade()`:
  - `GRANT SELECT, INSERT, UPDATE, DELETE ON {tbl} TO {APP_DB_ROLE}` for each new table (`APP_DB_ROLE` resolved as in `c42d0273d9bd:26`).
  - `GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO {APP_DB_ROLE}` (serial PKs on `trigger_action_results`, `trigger_cap_reservations`, `scheduled_triggers`).
  - For the **four RLS tables** (`triggers`, `trigger_runs`, `trigger_action_results`, `trigger_cap_reservations`): `ENABLE`+`FORCE ROW LEVEL SECURITY` then the byte-for-byte fail-closed policy `CREATE POLICY {tbl}_workspace_isolation … USING (workspace_id = current_setting('app.workspace_id', true)) WITH CHECK (…)` (`c42d0273d9bd:182-189`).
  - `scheduled_triggers` is **intentionally NOT RLS** (the bootstrap reads it without a workspace GUC); it contains only `(workspace_id, trigger_id, next_run_at, enabled)` — no lead/row content — and is documented as a deliberate non-RLS mirror.
- `downgrade`: drop policies (PG), then drop indexes/tables. App role not dropped (matches `_pg_downgrade`).
- SQLite/self-host path: tables created, no RLS DDL (dialect-guarded), exactly like the head migration.

---

## 4. API Surface

New router `apps/api/routers/automations.py`, prefix `/automations`, mounted in `apps/api/main.py`. **Every endpoint depends on `current_workspace` (or `require_workspace_role("admin")` for mutations)** so the RLS GUC is set and membership enforced. When `AUTOMATIONS_ENABLED=false`, the router returns `404`.

| Method | Path | Body / Query | Response | Auth |
|---|---|---|---|---|
| POST | `/automations/triggers` | `{name, trigger_type, trigger_config, condition, actions, scope_workbook_ids?, stop_on_error?, max_spend_usd_per_day?, max_actions_per_day?, enabled?}` | created trigger | admin |
| GET | `/automations/triggers` | `?enabled=&trigger_type=` | `[trigger]` | member |
| GET | `/automations/triggers/{id}` | — | trigger + recent runs summary | member |
| PATCH | `/automations/triggers/{id}` | partial fields | updated trigger | admin |
| DELETE | `/automations/triggers/{id}` | — | `{deleted:true}` | admin |
| POST | `/automations/triggers/{id}/pause` | — | `{enabled:false}` | admin |
| POST | `/automations/triggers/{id}/resume` | — | `{enabled:true, next_run_at?}` | admin |
| POST | `/automations/triggers/{id}/preview` | `{row_ids?, limit?}` | `{matched:int, sample:[{row_id, condition_pass, actions:[{type, would_charge_usd, resolved_payload_preview}]}], projected_total_usd}` — **no DB writes, no sends, no debits** | member |
| POST | `/automations/triggers/{id}/run` | `{row_ids?, dry_run?}` | `{run_id}` — enqueues a manual `trigger_eval` (`fire_source:"manual"`, dry_run honored) | admin |
| GET | `/automations/triggers/{id}/runs` | `?limit=` | `[trigger_run]` | member |
| GET | `/automations/runs/{run_id}` | — | run + `[trigger_action_result]` | member |

Validation on create/update:
- `trigger_type` ∈ enum; `actions[*].type` ∈ enum.
- **`on_signal` rejected with `409 on_signal_requires_pg_lead_store` when `PG_LEAD_STORE=false`** (blocking gap #1).
- **`sequencer`/`send_email` actions rejected with `409 legacy_outreach_disabled` when `AUTOMATIONS_ALLOW_LEGACY_OUTREACH=false`**; when enabled, `sequence_id` is **ownership-validated** against the workspace (§6.4) at create AND re-validated at execution.
- `re_enrich.config.column_ids` resolved/validated against the rule's scoped workbook columns; reject if no scoped workbook contains them (blocking gap #2).
- webhook: reject non-http(s) schemes and creds-in-url at create; full SSRF (DNS-pinned) check is at execution (§6.2). `header_secret_ref` (if present) must name an existing workspace secret.
- `condition` parse-checked by the **hardened parser against a representative + adversarial populated row** (§3.9), not an empty row.
- For `on_schedule`: require a valid `interval`; set `schedule_anchor`; write the `scheduled_triggers` mirror row; bootstrap the next aligned run.
- Enforce `AUTOMATIONS_MAX_RULES_PER_WS` and `AUTOMATIONS_MAX_ACTIONS_PER_RULE`.

### Background job type
- `trigger_eval` — payload `{trigger_id, workspace_id, targets?:[{workbook_id,row_id}], sweep_id?, sweep_cursor?, fire_source, fire_key, dry_run?}`. Registered in `main.py` and `worker.py`; `JOB_TIMEOUTS["trigger_eval"] = 600`. **Nested-timeout safety (test gap #10):** a `re_enrich` action invokes `run_workbook_enrichment` with `lead_ids` for a **single row** and a bounded `provider_timeout`; the engine caps re_enrich wall time well under 600s and the inner run's own job-cancel/pause checks halt it if the outer job is reaped. Idempotency (pre-send marker + workbook-budget guard, §7) makes a reaper requeue safe.

---

## 5. File-by-File Change List

**Add:**
- `apps/api/services/automations/__init__.py`
- `apps/api/services/automations/models.py` — `Trigger`, `TriggerRun`, `TriggerActionResult`, `TriggerCapReservation`, `ScheduledTrigger` ORM (§3).
- `apps/api/services/automations/engine.py` —
  - `async def handle_trigger_eval(job_id, payload)` (queue handler; `AUTOMATIONS_ENABLED` early-exit; `workspace_scope`; commits run row before any debit).
  - `def resolve_candidate_rows(db, trigger, targets|sweep) -> list[(workbook_id, row_id, cells, cols_cfg)]` — always `WorkbookRow`s.
  - `async def evaluate_and_act(db, trigger, run, candidates, dry_run) -> RunStats`.
  - `def _enqueue_next_schedule(db, trigger)` (aligned, single-flight, §3.7).
  - `def bootstrap_schedules()` (reads `scheduled_triggers` mirror, scopes per-ws to enqueue, §3.7).
- `apps/api/services/automations/safe_conditions.py` — `safe_evaluate_condition` (§3.9, fail-closed tokenizer).
- `apps/api/services/automations/actions.py` —
  - `async def execute_action(db, ws_id, action, workbook_id, row_id, cells, cols_cfg, idem_key, dry_run) -> ActionResult`.
  - `async def _act_re_enrich(...)` → `run_workbook_enrichment(workbook_id, column_ids=cfg["column_ids"], lead_ids=[lead_for_row])` with the **workbook-budget idempotency guard** (§7).
  - `_act_push_crm/_act_webhook/_act_sequencer` → `output.execute_output_column`; webhook path passes `workspace_id` + `header_secret_ref` (§3.5).
  - `async def _act_send_email(...)` → `outreach/sender.send_email` using `get_smtp_config(workspace_id)`, recipient resolved from the **workbook row's** `to_field` (never a bare legacy lead_id) (§6.4).
  - `def project_action_cost(action, cells, cols_cfg) -> float` → re_enrich builds `providers_by_column` from the **scoped workbook columns + DEFAULT_WATERFALLS** exactly as `routers/workbooks.py:758-766`, then calls `billing.projected_platform_cost` (blocking gap #3). Others 0 unless they consume a paid integration.
- `apps/api/services/automations/caps.py` —
  - `def try_reserve(db, trigger, ws_id, cost, idem) -> Reservation` (atomic reservation, §3.3a), `def settle(db, reservation)`, `def release(db, reservation)`, `def actions_today(db, trigger, day_utc) -> int`. Day bucket is explicit `YYYY-MM-DD` **UTC** (resolves the naive-tz cap-window edge).
- `apps/api/services/automations/events.py` —
  - `def on_rows_changed(workspace_id, workbook_id, changes)` — finds enabled `on_row_changed`/`on_row_added` rules whose `watch_fields` intersect; enqueues `trigger_eval`. No-op when `AUTOMATIONS_ENABLED=false`.
  - `def emit_signal_matches(db, workspace_id, signal_rows)` — called from `PgLeadStore.add_signal` (§3.4).
  - `def emit_row_added(workspace_id, workbook_id, row_ids)`.
- `apps/api/routers/automations.py` — the endpoints in §4.
- `migrations/versions/xxxx_automations_triggers.py` — §3.11.
- Tests under `apps/api/tests/automations/` (§11).

**Modify:**
- `apps/api/main.py` — register `trigger_eval` handler; mount `automations` router; call `bootstrap_schedules()` in startup (next to `bootstrap_signal_scan()`).
- `apps/api/worker.py:_register_handlers` — register `trigger_eval` (mirror requirement).
- `apps/api/services/queue_service.py` — add `JOB_TIMEOUTS["trigger_eval"] = 600`; add an **enqueue-if-no-pending-with-fire_key** guard helper used by the scheduler single-flight (§3.7).
- `apps/api/services/leadgen/pg_store.py` (or wherever `PgLeadStore.add_signal` lives) — after signal insert (inside scope), call `events.emit_signal_matches`. Guarded by `AUTOMATIONS_ENABLED`. **This is the only signal event source; `handle_signal_scan` is NOT extended** (blocking gap #1 — the legacy scan has no real workspace_id).
- `apps/api/services/workbook/enrichment.py` — add `read_current_values` prior-value capture immediately before both the overlay/`WorkbookEnrichment` write and the `_write_back_to_lead` write; after write-back commit, call `events.on_rows_changed(workspace_id, workbook_id, changes)`. Guarded by `AUTOMATIONS_ENABLED`.
- `apps/api/services/workbook/output.py` — extend `_send_webhook`/`execute_output_column` to accept `workspace_id` and resolve `header_secret_ref` via `get_secret` (blocking gap #9); add the **DNS-pinned connect** for SSRF (§6.2).
- **Row-insert sites (enumerated, blocking gap #7)** — emit `on_row_added` from all four: (1) source materialization, (2) CSV import, (3) manual add-row endpoint, (4) refresh source-column row creation. A single shared helper `events.emit_row_added` is called from each; sites identified during implementation by grepping `WorkbookRow(` construction/insert.
- `apps/api/core/config.py` — add flags (§10).

---

## 6. Tenancy & Security

### 6.1 Workspace isolation / RLS
All four new tenant tables (`triggers`, `trigger_runs`, `trigger_action_results`, `trigger_cap_reservations`) carry `workspace_id` + the fail-closed RLS policy. The `trigger_eval` handler **must** run inside `tenancy.workspace_scope(payload["workspace_id"])`; without it the GUC is unset and every RLS query returns zero rows (fail closed). Endpoints set the GUC via `current_workspace`. **`scheduled_triggers` is the one deliberate non-RLS mirror** (ids + timestamps only, no row content) so cold-start bootstrap works (§3.7).

**Honest scope statement (review tenancy issue #3):** the engine reads candidates **only** from `WorkbookRow`/workbook columns, which are RLS PG on both configs. It does **not** read `signals.db`/`LeadDB`/`outreach.db`. The "unset GUC → zero rows → no leak" guarantee therefore holds for everything the engine reads. The legacy global SQLite stores are never read by the engine.

### 6.2 SSRF — DNS-pinning to close the TOCTOU/rebinding hole (review tenancy issue #1)
`_is_safe_public_url` resolves DNS in the check, but httpx resolves DNS **again** at connect time — a rebinding domain can pass the guard and connect internally. Fix: webhook sends use a **custom httpx transport / connect hook that pins the connection to the IP that was validated** (or re-validates the actual connected peer address). Concretely: resolve once, validate every returned A/AAAA record, then connect by IP with the `Host` header preserved (and SNI for https), refusing if the socket's peer address is not in the validated public set. `follow_redirects=False` is already set in `_send_webhook` (confirmed `output.py:132`), so the redirect-to-internal sub-path is already closed; we keep it explicitly. The url itself is `{field}`-resolved from lead data, so the guard runs **post-resolution** (already the case) AND post-pin.

### 6.3 Prompt injection / templating
`send_email`/webhook bodies resolve `{field}` placeholders via the existing `output._resolve` (no code eval). The hardened condition evaluator (§3.9) ensures attacker-influenced field values cannot flip a predicate to fire paid actions. No LLM is invoked by the engine itself. Webhook **host** can be `{field}`-derived, so it is subject to the post-resolution DNS-pinned SSRF guard (§6.2).

### 6.4 Cross-tenant sequencer/email — new ownership/scoping layer (review blocking gap #6, tenancy issue #2)
`enroll_leads`/`send_email`/`outreach.db` are global and unscoped; a workspace-A rule could enroll a workspace-B `sequence_id` or address a workspace-B legacy `lead_id`. Mitigations (all required when `AUTOMATIONS_ALLOW_LEGACY_OUTREACH=true`):
- **No bare legacy lead_id ever crosses the boundary.** Recipients/enrollees are derived **from the firing `WorkbookRow`** (RLS-isolated): `to_field` is read from the workspace-scoped row; for `sequencer`, the lead identity passed to `enroll_leads` is the row's lead linkage **within the firing workspace's store**, never an attacker-supplied integer.
- **Sequence ownership validation.** A new lightweight per-workspace ownership check: `sequencer.config.sequence_id` must appear in a `workspace_sequences(workspace_id, sequence_id)` ownership mapping (added as part of this WI, or validated against the workspace's own sequence list if one exists). Validated at create **and** re-validated at execution. If the sequence is not owned by the firing workspace → action `failed`, `skip_reason` records `not_owned`; **no enroll**. **[OWNER DECISION]** confirm where workspace↔sequence ownership lives today; if there is no such mapping at all, we add the minimal `workspace_sequences` table as part of this WI (recommended). `enroll_leads` is confirmed idempotent (`UNIQUE(sequence_id, lead_id)` + `INSERT OR IGNORE`, `sequence.py:112/219`), so the no-double-enroll concern from cost-idempotency is satisfied — but ownership is the real risk and is now gated.
- **Default OFF.** With `AUTOMATIONS_ALLOW_LEGACY_OUTREACH=false`, both action types are unavailable, so the cross-tenant write surface is closed entirely until the legacy stores are RLS-hardened (tracked WI).

### 6.5 Secret handling
CRM tokens / SMTP creds resolved per-workspace via `get_secret(workspace_id, key)` (CRM/SMTP already wired through `output.execute_output_column`/`sender.get_smtp_config`). Webhook auth now also resolves via `get_secret` (§3.5/§6.2). No secrets stored in `triggers`.

### 6.6 Abuse limits / egress
- per-rule `max_actions_per_day`/`max_spend_usd_per_day`; global `AUTOMATIONS_GLOBAL_DAILY_USD`; `AUTOMATIONS_MAX_RULES_PER_WS`; `AUTOMATIONS_MAX_ACTIONS_PER_RULE`; `AUTOMATIONS_MAX_ROWS_PER_EVAL`.
- **Admin-as-SSRF-relay (review tenancy issue #6):** mutations require admin (not owner). The DNS-pinned SSRF guard is the primary defense. Additionally we add an **optional egress allowlist** `AUTOMATIONS_WEBHOOK_DOMAIN_ALLOWLIST` (empty = allow any public host); cloud deployments can populate it. **[OWNER DECISION]** whether to require owner (not admin) for rules containing webhook/send_email actions, and whether to ship the allowlist as enforced-by-default on cloud.

---

## 7. Cost Control & Idempotency

- **Correct re_enrich projection (blocking gap #3):** `project_action_cost` assembles `providers_by_column` from the scoped workbook's column configs **plus `DEFAULT_WATERFALLS` fallback**, exactly as the router does at `routers/workbooks.py:758-766`, then calls `billing.projected_platform_cost`. The cap and pre-debit are computed against the **real** projected cost, not a fabricated flat number.
- **No double-charge across BOTH spend ledgers (blocking gap #4, cost issue #3):** there are two spend axes — the credit ledger (`check_and_debit`, idempotent on `run_id`) and the workbook's own `budget_spent_usd` increment inside `run_workbook_enrichment` (`enrichment.py:389`, **not** idempotent on the automation idem). To prevent a retried `re_enrich` from re-incrementing `budget_spent_usd`:
  - The engine records the credit debit under `run_id=f"run:{idem}"` (workspace-prefixed idem → **closes the cross-tenant ledger collision**, tenancy issue #5).
  - `_act_re_enrich` is guarded by the **pre-send marker** (an `in_flight` `trigger_action_result` written+committed before the call): on retry the marker is found and `re_enrich` is **not re-invoked**, so `budget_spent_usd` is not double-incremented. The result row is updated to `success`/`failed` after the inner run returns.
  - The engine does **not** rely on, and does **not** duplicate, the workbook's internal budget enforcement; it additionally surfaces (in `result_summary`) when `run_workbook_enrichment` itself hits the workbook's `budget_max_usd` (`enrichment.py:297`) so a rule blowing past a workbook cap is visible, not silent (blocking gap #4 second half).
- **check_and_debit commits internally — engine ordering fixed (blocking gap #5, cost issue #5):**
  - `check_and_debit` calls `db.commit()`/`db.rollback()` (`service.py:162/213/226`). Therefore the engine **commits the `trigger_run` row and any prior `trigger_action_result` rows BEFORE every debit**, so a credit-failure rollback inside `check_and_debit` cannot destroy the run record (satisfies AC-8).
  - To close the **crash-between-debit-and-send** window (the original spec's broken "no double-send" for non-idempotent external actions): the engine writes a **`status="in_flight"` `trigger_action_result` row and commits it BEFORE the external send**. On retry, the unique idem key finds the `in_flight` row; the engine treats a found `in_flight` row as "previously attempted" and, for **non-idempotent** action types (generic webhook, send_email), records `skipped`/`failed` with `skip_reason="replay"` rather than re-sending (at-most-once for those). For **idempotent** action types (CRM upsert, sequencer `INSERT OR IGNORE` — confirmed `sequence.py:219`) it may safely re-attempt. This converts the original "documented at-least-once for email" into **at-most-once for non-idempotent actions**, which is the safer default. **[OWNER DECISION]** for `send_email` specifically, owner picks at-most-once (default, may rarely miss a send if crash lands in the window) vs at-least-once (may rarely duplicate). Default here: at-most-once.
- **Atomic caps (cost issues #1/#2):** caps are enforced via the `trigger_cap_reservations` table (§3.3a). `try_reserve` does an atomic check-and-insert (`FOR UPDATE` on the day bucket) so two concurrent jobs cannot both pass a near-full cap. Reservation is `settled` after a successful debit, `released` on debit failure/cap-skip. Caps use an explicit `YYYY-MM-DD` **UTC** bucket (closes the naive-tz/DST cap-window edge). Over-cap actions are recorded `status="skipped", skip_reason="cap"`; run becomes `partial`.
- **No double-send (steady state):** before any action the engine checks for an existing `trigger_action_result` with the idem key (unique `uq_trigger_action_idem`, ws-scoped). Present + terminal → `replay` skip. Present + `in_flight` → handled per the at-most-once rule above.
- **BILLING disabled:** when `BILLING_ENABLED=false`, `check_and_debit` is a no-op (`service.py`), never blocks; caps (action counts) still enforce.
- **Rate limits:** queue sequential per worker; `AUTOMATIONS_MAX_ROWS_PER_EVAL` bounds a job; schedule sweeps chunk with a snapshot cursor (§3.8); signal/row events deduped by `fire_key`.

---

## 8. Failure Modes & Edge Cases

| Failure / edge | Handling |
|---|---|
| Worker crashes mid-eval | Heartbeat reaper requeues; idem keys (terminal + `in_flight`) make re-run safe; non-idempotent actions are at-most-once (§7). |
| `trigger_eval` retried after transient error | Per-action unique idem → done/in-flight actions skipped; only the unfinished tail re-runs. |
| Partial failure (action 2 of 3) | Ordered; non-fatal error → `status=failed`, continue unless `stop_on_error`; run `partial`. |
| Double signal detection | `fire_key="signal:<PG signal PK>"` (globally unique); re-insert of same signal → same PK → no new fire. (PG path only; legacy scan not an event source.) |
| Row oscillates empty→A→empty→A | `fire_key` includes `cell_version`/updated_at-ms (§3.6) → each genuine transition distinct; pure retry reuses version → deduped. |
| Insufficient credits | `check_and_debit` raises; reservation released; action `skipped` (reason credits); run `partial`; surfaced in run (run row committed before debit). |
| Per-rule / global cap exceeded | Atomic reservation refuses; action `skipped` (reason cap); run `partial`. No overspend race (§7). |
| Webhook URL → internal IP / DNS-rebind | DNS-pinned connect (§6.2) → action `failed` (blocked url). |
| Webhook needs auth header | Resolved from `get_secret` via `header_secret_ref` (§3.5). |
| CRM/SMTP not connected | executor returns error → action `failed` (not connected); no charge. |
| re_enrich target columns absent in a row's workbook | `skipped`, `skip_reason="no_target"`. |
| on_signal lead in no scoped workbook | No `trigger_eval` enqueued (no lead-level firing, §3.10). |
| on_signal on legacy self-host (PG_LEAD_STORE=false) | Rule rejected at create (`409`); feature unavailable, documented (§3.4). |
| sequencer/send_email with foreign sequence_id/lead | Ownership validated at create+exec; recipients derived from the RLS row only; foreign → `failed`/`not_owned`, no write (§6.4). When `AUTOMATIONS_ALLOW_LEGACY_OUTREACH=false`, action type unavailable. |
| Paused mid-run | In-flight job finishes; events check `enabled`; scheduler stops (mirror row + single-flight). |
| Deleted rule with pending/in-flight chunked jobs | Handler no-ops if missing; `trigger_run.trigger_name_snapshot` preserves history (no orphan-to-nonexistent-name); mirror row removed on delete. |
| Condition unparseable / value contains `"`/` AND ` | Hardened evaluator (§3.9) fails CLOSED (no fire), `skip_reason="condition"`; caught at create-time against adversarial sample row. |
| Concurrent rule edits mid-eval | Handler snapshots rule at job start; edits apply to later evals. |
| Schedule drift / duplicate chains / multi-replica | Wall-clock alignment + single-flight (fire_key-keyed enqueue guard) + non-RLS mirror bootstrap (§3.7) → no doubling, no creep. |
| Large schedule sweep | Frozen snapshot cursor + keyset chunks (§3.8); `fire_key="sched:<trigger>:<sweep_id>"` stable across retries → no double/missed/multiplied spend. |
| Cold start after restart | `bootstrap_schedules` reads non-RLS `scheduled_triggers` mirror, then scopes per-ws to enqueue (§3.7) → schedules survive restart (closes blocking gap #8). |
| Tenant GUC unset (worker forgot scope) | RLS zero rows; engine reads only RLS tables → empty run, no leak. |
| Nested timeout (re_enrich vs inner run_workbook) | re_enrich single-row + bounded provider_timeout under 600s; inner run honors job-cancel; reaper requeue safe via markers (§4). |

---

## 9. Observability

- **Activity feed**: each fire writes a `WorkbookActivity` row (`kind="automation"`) scoped to the workbook (reuses `activity_models`); a `trigger_run` always records full outcome.
- **Run inspection**: `GET /automations/runs/{run_id}` returns the run + every `trigger_action_result` (per row, per action: status, skip_reason, charged_usd, result_summary, error, idempotency_key). Primary debug surface.
- **Ledger linkage**: paid actions appear in `credit_ledger_entries` with `run_id="run:"+idem` (ws-prefixed) and `reason="automation_<type>"`, joinable to `trigger_action_results`.
- **Cap visibility**: `trigger_cap_reservations` rows expose held/settled/released spend per ws/day — surfaces overspend-near-misses and skip reasons.
- **Logs**: structured logs in `engine`/`actions`/`caps` with `trigger_id`, `run_id`, `workbook_id`, `row_id`, `action_index`, `idempotency_key`, decision + reason.
- **Metrics**: evals/min, matches, actions by type & status, charged_usd, cap-skips, dedup-skips (terminal vs in_flight), condition-fail-closed count, SSRF-block count, eval latency, schedule-drift gauge.
- **WS broadcast**: optional `automation_run` event over the `cell_update` channel for live UI.

---

## 10. Feature Flags / Config / Rollout

In `apps/api/core/config.py` (`Settings`):
- `AUTOMATIONS_ENABLED: bool = False` — master switch. Off: router 404, emitters no-op, handler early-exits. **Default OFF.**
- `AUTOMATIONS_ALLOW_LEGACY_OUTREACH: bool = False` — gates `sequencer`/`send_email` (global unscoped stores). **Default OFF** until those stores are RLS-hardened (§6.4).
- `AUTOMATIONS_GLOBAL_DAILY_USD: float = 0.0` (0 = unlimited; cloud sets a cap).
- `AUTOMATIONS_MAX_RULES_PER_WS: int = 50`
- `AUTOMATIONS_MAX_ACTIONS_PER_RULE: int = 10`
- `AUTOMATIONS_MAX_ROWS_PER_EVAL: int = 500`
- `AUTOMATIONS_WEBHOOK_DOMAIN_ALLOWLIST: list[str] = []` (empty = any public host; cloud may populate).

Config dependencies:
- `on_signal` requires `PG_LEAD_STORE=true` (else rejected at create).
- `sequencer`/`send_email` require `AUTOMATIONS_ALLOW_LEGACY_OUTREACH=true`.

Rollout:
1. Ship migration + models + flags OFF; tables exist, nothing runs.
2. Enable on internal/staging (`PG_LEAD_STORE=true`); exercise dry-run + manual run + on_row_changed + on_schedule.
3. Cloud: `AUTOMATIONS_ENABLED=true`, `BILLING_ENABLED=true`, a `AUTOMATIONS_GLOBAL_DAILY_USD` cap, optional webhook allowlist. `on_signal` available (PG store). `AUTOMATIONS_ALLOW_LEGACY_OUTREACH` only after outreach RLS hardening.
4. Self-host (`PG_LEAD_STORE=false`): `on_signal`, `sequencer`, `send_email` unavailable; `on_row_changed`/`on_row_added`/`on_schedule` + `re_enrich`/`push_crm`/`webhook` fully available with billing off (caps still enforce counts). RLS DDL emitted on PG only.
5. Event emitters (enrichment hook, `PgLeadStore.add_signal` hook) guarded by `AUTOMATIONS_ENABLED` so hot paths are untouched when off.

---

## 11. Test Plan

Run: `PYTHONPATH=. uv run --group dev python -m pytest`. PG-gated tests use `TEST_DATABASE_URL`. External calls mocked: HTTP via `respx`/`httpx` mock; CRM `is_connected`/`push_lead_as_contact` monkeypatched; SMTP `sender.send_email` monkeypatched; provider runs via existing enrichment mocking.

**Unit (SQLite):**
1. CRUD + validation: bad trigger_type/action_type rejected; `on_signal` rejected when `PG_LEAD_STORE=false`; `sequencer`/`send_email` rejected when `AUTOMATIONS_ALLOW_LEGACY_OUTREACH=false`; re_enrich `column_ids` validated against scoped workbook. → AC-1
2. `safe_evaluate_condition` **adversarial**: value containing `"`, ` AND `, ` OR `; unparseable expression → returns **False** (fail closed), `skip_reason="condition"`; create-time validation runs against a populated+adversarial row. → AC-3, AC-14
3. `project_action_cost` re_enrich builds `providers_by_column` (incl. DEFAULT_WATERFALLS) and matches `projected_platform_cost`; CRM/webhook/email = 0. → AC-7
4. `caps.try_reserve` arithmetic: held+committed vs cap; settle/release transitions; UTC day bucket boundary. → AC-6
5. `evaluate_and_act` ordered execution; `stop_on_error` True/False; partial status. → AC-2, AC-8
6. Idempotency steady-state: same `fire_key` twice → second run only `replay` skips, zero new charges. → AC-5
7. `on_rows_changed` enqueues only for enabled, matching watch-field rules; paused rule → no enqueue. → AC-4, AC-9
8. **on_row_changed prior-value capture + transition fire_key**: oscillation empty→A→empty→A yields distinct fire_keys (both fire); same-value rewrite → no new fire; quote-in-value does not break condition (uses §3.9). → AC-4, AC-14
9. Dry-run/preview returns projected cost + resolved payload preview with **no** ledger entries and **no** action-result rows. → AC-11
10. `JOB_TIMEOUTS["trigger_eval"]` present; handler registered in BOTH main.py and worker.py (import/registration test). → AC-12
11. **on_row_added emitter** fires from each of the four enumerated insertion sites (unit per site with the shared helper). → AC-4

**Integration / PG-gated (`TEST_DATABASE_URL`):**
12. RLS isolation: rule/runs/results/reservations in ws A invisible to ws B under each GUC. → AC-13
13. Handler under `workspace_scope`; GUC unset → zero candidates (fail closed). → AC-13
14. Billing e2e: re_enrich debits ledger once; job retry does not double-debit (`run:<idem>`); insufficient credits → action skipped, run partial, **run row survives** the internal rollback. → AC-5, AC-7, AC-8
15. **re_enrich workbook-budget double-spend**: retry of a re_enrich asserts `Workbook.budget_spent_usd` is **not** incremented twice (pre-send marker blocks re-invocation). → AC-5, AC-7
16. **crash-between-debit-and-send replay**: simulate debit committed + `in_flight` result present + retry → assert webhook/email is **NOT re-sent** (at-most-once for non-idempotent); CRM/sequencer safe to re-attempt. → AC-5, AC-15
17. **cap concurrency race**: two concurrent `trigger_eval` jobs against a near-full global/per-rule cap → reservations serialize, total spend ≤ cap (no overshoot). → AC-6
18. **on_signal REAL path (PG_LEAD_STORE=true)**: call the **real `PgLeadStore.add_signal`** → asserts `emit_signal_matches` enqueues a `trigger_eval`; action fires once; re-insert same signal (same PK) → no re-fire. **Also** an explicit test asserting the legacy `run_signal_scan` path is **NOT** an event source (no `trigger_eval` enqueued) so broken infra cannot be green-lit. → AC-4, AC-5
19. **DNS-pinned SSRF**: a domain whose guard-time resolution is public but connect-time resolution is internal is **blocked** at connect (asserts the actual socket peer is validated, not just the static URL). Redirect-to-internal stays blocked (`follow_redirects=False`). → AC-10
20. **cross-tenant sequencer/email rejected**: workspace-A rule with a workspace-B `sequence_id` → rejected at create (ownership) AND at execution; recipient always derived from the RLS row, never a foreign lead_id. → AC-16
21. **schedule alignment + single-flight + cold-start**: daily rule fires at fixed wall-clock; a simulated duplicate chain / second replica does not double-enqueue (fire_key guard); after a restart with GUC unset, `bootstrap_schedules` reads the non-RLS mirror and re-enqueues due rules. → AC-9, AC-17
22. **large-sweep snapshot cursor**: 1200-row sweep chunked at 500; a row added mid-sweep is excluded this sweep; a row present is acted on exactly once across chunks (stable `sweep_id` fire_key); retry of a chunk does not re-act. → AC-6, AC-5
23. Pause/resume: paused rule's scheduler stops (mirror row disabled); resume re-bootstraps `next_run_at`. → AC-9
24. Concurrency: two workers claim the same `trigger_eval` → exactly one executes (FOR UPDATE SKIP LOCKED + idempotency). → AC-5, AC-13
25. **nested timeout**: re_enrich whose inner run exceeds wall budget is halted; reaper requeue does not double-enrich/double-charge (markers + workbook-budget guard). → AC-5

---

## 12. Acceptance Criteria

1. A workspace admin can create/list/get/update/delete a rule; invalid trigger/action types and unparseable conditions are rejected at create; `on_signal` requires `PG_LEAD_STORE=true`; `sequencer`/`send_email` require `AUTOMATIONS_ALLOW_LEGACY_OUTREACH=true`; `re_enrich` `column_ids` are validated against the scoped workbook.
2. Actions execute in stored order; on a non-fatal error the run continues (unless `stop_on_error`) and is marked `partial`.
3. The condition is evaluated per candidate **WorkbookRow** using the **hardened, fail-closed** evaluator; non-matching rows produce no actions.
4. Available trigger types fire correctly: `on_signal` (real PG signal-insert path; unavailable when `PG_LEAD_STORE=false`), `on_row_changed` (captured prior-value transition), `on_row_added` (all four insertion sites), `on_schedule` (wall-clock-aligned, single-flight, cold-start-recoverable).
5. Idempotency: re-running an eval with the same `fire_key` (retry, re-insert, duplicate enqueue, concurrent worker, reaper requeue) never double-charges (ws-prefixed ledger key) and never double-sends; non-idempotent external actions are **at-most-once** (pre-send `in_flight` marker), idempotent ones safely re-attempt.
6. Per-rule daily spend/action caps and the workspace-global daily cap are enforced **atomically via reservations** (no concurrent-overspend race); over-cap actions recorded `skipped` and excluded from spend; UTC day bucket.
7. Every paid action is debited via `billing.check_and_debit` with a ws-prefixed per-action idempotency key; re_enrich cost uses the **real `providers_by_column` waterfall** projection; the workbook `budget_spent_usd` axis is **not** double-incremented on retry; no-op when `BILLING_ENABLED=false`.
8. Partial failures, insufficient credits, and cap hits are each recorded per-action with a reason and surfaced in the run record; the run row **survives** `check_and_debit`'s internal rollback (committed before each debit).
9. Pause stops all future evaluations (events + schedule, incl. the mirror row); in-flight jobs finish; resume re-enables and re-bootstraps the schedule.
10. Webhook actions are SSRF-guarded with **DNS-pinned connect** (TOCTOU/rebinding closed) and `follow_redirects=False`; internal/metadata targets blocked.
11. Dry-run/preview reports matched-row count, per-action projected cost, and a resolved payload preview, writing no ledger entries and performing no sends, debits, or reservations.
12. `trigger_eval` is registered in both `apps/api/main.py` and `apps/api/worker.py` with a `JOB_TIMEOUTS` entry, following the `signal_scan` pattern.
13. RLS enforces workspace isolation on the four tenant tables; the worker handler runs inside `workspace_scope`; the engine reads only RLS tables (never legacy global SQLite), so an unset GUC fails closed (zero rows, no leak); `scheduled_triggers` is the sole, content-free non-RLS mirror.
14. The condition evaluator cannot be flipped to fire a paid action by adversarial field values (embedded `"`, ` AND `, ` OR `, unparseable) — it fails closed.
15. A crash between the (committed) debit and the external send does not re-send a non-idempotent action on retry.
16. `sequencer`/`send_email` cannot enroll/address a foreign workspace's sequence or lead: recipients/enrollees derive only from the firing RLS row, and `sequence_id` ownership is validated at create and execution.
17. Schedules do not drift, duplicate under multi-replica, or die after a restart; large sweeps act on each row exactly once via a stable snapshot cursor.

### Out of scope
- New signal **detection** sources (still hiring-only via JobSpy).
- A workspace-stamped legacy signal scanner to give self-host `on_signal` (tracked WI — see open questions).
- RLS-hardening the outreach sequencer / signals legacy SQLite (`outreach.db`/`signals.db`/`LeadDB`); until done, `sequencer`/`send_email` stay behind `AUTOMATIONS_ALLOW_LEGACY_OUTREACH` (default OFF) with ownership validation as the interim guard.
- A visual rule builder UI (backend + API only).
- ORM `after_update` DB-level change hooks (we emit change events explicitly with prior-value capture, §3.6).
- New scheduler infra (APScheduler/Celery) — reuse self-re-enqueue, hardened.
- Branching / multi-step if/else workflows beyond an ordered action list.
- Lead-level (non-workbook) firing — structurally incompatible with the reused evaluator (§3.10).

---

## Changes after review

Every review item folded in:

**Blocking gaps**
1. **Signals not RLS/global SQLite** → re-grounded `on_signal` on the PG `PgLeadStore.add_signal` write path (real `workspace_id`); legacy global scan is explicitly **not** an event source; `on_signal` gated on `PG_LEAD_STORE=true`, rejected otherwise (§0, §3.4, §5, AC-4, test 18).
2. **`run_workbook_enrichment` no `columns` kwarg** → action stores `column_ids` (UUIDs) matching the real signature; create-time name→UUID resolution; no lead-level re_enrich (§3.5, §3.10).
3. **Hand-waved re_enrich projection** → `project_action_cost` builds the real `providers_by_column` waterfall (+DEFAULT_WATERFALLS) like the router (§7, test 3).
4. **re_enrich double-charge / workbook-budget bypass** → pre-send marker prevents re-invocation; workbook `budget_spent_usd` not double-incremented; workbook-cap hits surfaced (§7, test 15).
5. **check_and_debit commits internally** → run/result rows committed before every debit; `in_flight` pre-send marker makes non-idempotent actions at-most-once (§7, AC-8/AC-15, tests 14/16).
6. **Cross-tenant sequencer/email** → recipients derive only from the RLS row; sequence ownership validated; gated behind `AUTOMATIONS_ALLOW_LEGACY_OUTREACH` (default OFF) (§6.4, AC-16, test 20).
7. **on_row_added no emitter** → four insertion sites enumerated + shared `emit_row_added` helper (§5, test 11).
8. **Scheduler bootstrap dead under RLS** → non-RLS `scheduled_triggers` mirror + per-ws scoping (§3.7, test 21).
9. **Webhook secrets** → `header_secret_ref` resolved via `get_secret`; `_send_webhook` extended to take `workspace_id` (§3.5, §6.2).

**Edge cases** — unsafe evaluator → fail-closed tokenizer (§3.9); prior-value capture across both write paths (§3.6); deterministic transition fire_key (§3.6); schedule drift/duplication → wall-clock + single-flight (§3.7); sweep chunk fire_key/cursor → frozen snapshot (§3.8); lead-not-in-workbook → no lead-level firing (§3.10); orphaned run → `trigger_name_snapshot` + mirror cleanup; UTC day bucket for caps (§3.3a/§7); redirect-following already closed (`follow_redirects=False` confirmed) + DNS-pin (§6.2).

**Tenancy/security** — DNS-pinned SSRF connect (§6.2); cross-tenant outreach gated (§6.4); honest "engine reads only RLS tables" statement (§6.1); webhook-host template + pin (§6.3); ws-prefixed ledger idem key (§7); admin-as-relay → DNS-pin + optional egress allowlist (§6.6).

**Cost/idempotency** — atomic cap reservations close the overspend race (§3.3a/§7); correct projection (§7); workbook-budget idempotency (§7); at-most-once for non-idempotent actions (§7); credit-failure rollback no longer destroys the run (§7); sequencer confirmed idempotent (`UNIQUE(sequence_id,lead_id)`, `sequence.py:112`); schedule-dup spend multiplication closed via stable `sweep_id` fire_key (§3.8).

**Tests** — added/clarified: real signal-scan path + negative legacy-scan test (18), DNS-rebind connect test (19), crash-between-debit-and-send replay (16), cross-tenant outreach rejection (20), workbook-budget double-spend (15), cap concurrency race (17), prior-value/transition adversarial (8), cold-start cross-ws bootstrap (21), on_row_added per-site (11), nested-timeout reaper (25).

---

## Open questions for the owner

1. on_row_changed cell_version source (§3.6): add an explicit integer cell_version column to the cell tables (cleaner, requires a cell-table migration) vs reuse updated_at epoch-ms (no migration, but sub-millisecond write bursts could collide). Spec defaults to updated_at epoch-ms — owner to confirm or approve the extra column.
2. Cold-start scheduler bootstrap mechanism (§3.7): use a non-RLS scheduled_triggers mirror table (spec default, no privileged role) vs grant the bootstrap a BYPASSRLS maintenance role to read triggers directly. Confirm the mirror is acceptable.
3. Sequence ownership source of truth (§6.4): does a workspace↔sequence ownership mapping exist today? If not, approve adding a minimal workspace_sequences table as part of this WI (required to make sequencer safe), or keep sequencer disabled entirely.
4. send_email crash-window semantics (§7): at-most-once (spec default — a crash in the tiny debit→send window may rarely drop an email) vs at-least-once (may rarely duplicate). Confirm at-most-once is acceptable for email.
5. Egress / privilege policy (§6.6): should rules containing webhook/send_email actions require workspace OWNER rather than ADMIN, and should AUTOMATIONS_WEBHOOK_DOMAIN_ALLOWLIST be enforced (non-empty) by default on cloud?
6. Self-host on_signal (§3.4, Out of scope): is shipping on_signal as PG-only (unavailable when PG_LEAD_STORE=false) acceptable for v1, or must we build a workspace-stamped legacy signal scanner now?
7. Legacy outreach hardening timeline: AUTOMATIONS_ALLOW_LEGACY_OUTREACH stays OFF until outreach.db is RLS-hardened. Owner to prioritize that follow-up WI or accept sequencer/send_email being unavailable at launch.
