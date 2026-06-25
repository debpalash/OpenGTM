<!-- Auto-generated design spec (2026-06-25). Review verdict: needs-revisions. -->

## ✅ LOCKED SCOPE DECISIONS (owner-approved 2026-06-25)

1. **BYO-SMTP only for v1.** Each workspace sends via its own SMTP creds (WI-6 get_secret); DKIM/DMARC are the customer's domain. NO Yupcha shared sending domain/IP in v1 (deferred WI).
2. **Basic bounce/complaint circuit breaker IN v1.** Track per-sequence bounce/complaint counts from the signals we capture at send (SMTP hard-failure = bounce; complaint signal where available); AUTO-PAUSE a sequence at >5% bounce OR >0.3% complaint, emit an activity event, surface in stats. Full async bounce/complaint feedback-loop ingestion (IMAP/FBL parsing) is a noted follow-up, but the threshold + auto-pause mechanism ships now.
3. **Metered sends = DEBIT-ON-SUCCESS.** `check_and_debit` runs AFTER a successful SMTP handoff, never before. Hard-failed sends are never charged (no refund path needed). At-most-once is still guaranteed by the committed `in_flight` marker on `outreach_sends` (independent of debit timing): mark in_flight (commit) → SMTP send → on success mark sent + debit; on failure mark failed, no debit.
4. **Per-workspace suppression for v1.** Unsubscribe/complaint in workspace A suppresses that recipient for workspace A only (each tenant is its own sender/controller under BYO-SMTP). A platform-global suppression layer is deferred to whenever shared sending infra is introduced.
5. Implementation-detail defaults accepted: ticker per-tick enqueue cap + batching; OUTREACH_TICK_INTERVAL default; sends gated behind AUTOMATIONS_ALLOW_LEGACY_OUTREACH (flips ON only after this lands).


# Production Spec — RLS-Harden the Outreach Store & Enable Trigger-Engine Sequence/Send Actions (REVISED, build-ready)

## 0. Summary

The outreach subsystem today is a **global, non-tenant SQLite file** `data/outreach.db` (`sequence.py:81`) with three tables that carry **no `workspace_id`, no RLS, and a bare integer `lead_id`** (`sequence.py:101-128`). At send time it resolves leads via the legacy non-tenant `LeadDB()` (`sequence.py:339,344`) — a confirmed cross-tenant leak, since `lead_id` is only unique per workspace (dedup key `(workspace_id, company, city)`, `orm_models.py`). It also uses a **process-global SMTP config** (`sender.py:150`, no-arg `get_smtp_config()`) and a **process-global `_rate_limiter`** (`sender.py:96,155,192`). This is why the trigger engine rejects `sequencer`/`send_email` at rule-create (`automations.py:124-128`) and at execution (`actions.py:320-322`), gated by `AUTOMATIONS_ALLOW_LEGACY_OUTREACH=False` (`config.py`).

This spec re-derives outreach onto the **proven leads/signals + automations tenancy plane**: new RLS-protected PG tables (mirroring `services/leadgen/orm_models.py` + the `c42d0273d9bd`/`a1b2c3d4e5f6` migration recipe), a `PgOutreachStore(workspace_id)` mirroring `PgLeadStore` (`store.py:141`), per-workspace SMTP via WI-6 `get_secret` (`sender.py:38-61`, `secrets.py:210`), a new `send` job type on the durable queue, an **autonomous sequence ticker reusing the `scheduled_triggers` mirror + `bootstrap_schedules` + `_enqueue_schedule_if_absent` single-flight pattern** (`engine.py:408-532`), sends metered through the credit ledger + caps exactly like `_run_one_action` (`engine.py:111-219`), a **suppression/unsubscribe list with List-Unsubscribe + pre-send enforcement** (none exists today), and a data migration of the existing `outreach.db` into the tenant tables. Then `_act_sequencer`/`_act_send_email` executors are added and the legacy flag flips on safely.

Reuse over new infra throughout: `sender.py` send/render is reused (extended for headers + CRLF sanitization); the queue, billing, caps, secrets, tenancy GUC hook, the schedule-mirror/bootstrap pattern, and the migration recipe are all reused. The single most safety-critical path — `handle_send`'s at-most-once send — is **specified explicitly below (§6.2.1), reimplemented from the `_run_one_action` ordering, not "by analogy."**

---

## 1. Goal & User-Facing Behavior

### 1.1 Sequences (CRUD)
A workspace member creates an outreach **sequence**: a named, ordered list of steps `{step_number, subject, body_html, delay_hours}` (`sequence.py:31-41`), plus `daily_limit`, `send_window_start/end`, `send_window_tz`, status `draft|active|paused|completed`. All operations are scoped to the caller's workspace via `Depends(current_workspace)`. A user in workspace A can never see/edit/start workspace B's sequence (RLS + app-layer filter).

### 1.2 Enrollment
A member enrolls leads into a sequence by `lead_id` **within their workspace**. Enrollment is deduped per `(workspace_id, sequence_id, lead_id)`. A lead with the same integer id in another workspace is a different lead and is never reachable. On enroll: (a) the `sequence_id` is **validated to exist in `ctx.workspace_id`** before any insert (no orphan enrollments — RLS cannot catch a same-ws-stamped row pointing at a non-existent sequence, §7); (b) the lead's email is **snapshotted onto the enrollment** (§1.5); (c) suppressed/unsubscribed recipients are filtered (see §6.4); (d) a **consent/lawful-basis field** is required (§6.7).

### 1.3 Sending
When a sequence is `active`, due steps are sent **autonomously by a recurring ticker** (§6.8) — not only by a manual `/execute` click. Each send applies: per-workspace SMTP creds (WI-6), **send-window enforcement** (§6.3), per-workspace rate limit, suppression check immediately before send, List-Unsubscribe header + body link (HTML and plaintext) on every message, mandatory physical-address footer, send recorded to a tenant `outreach_sends` log, lead state machine advanced (`pending→scheduled→sent→...`, `sequence.py:20-28`).

### 1.4 Trigger-engine actions (end to end)
With `AUTOMATIONS_ENABLED=True` and `AUTOMATIONS_ALLOW_LEGACY_OUTREACH=True`:

- **`enroll` (sequencer) action**: rule fires on a matched WorkbookRow → engine calls `execute_action(... "sequencer" ...)` → `_act_sequencer` validates `config.sequence_id` exists in the rule's workspace, resolves the row's `lead_id`, snapshots its email, and enrolls it **in that rule's workspace**. Idempotent (re-enroll is a no-op via the unique constraint; engine `TriggerActionResult` idem key `trig:<ws>:<trigger>:<wb>:<row>:<idx>:<fire_key>`, `engine.py:119`).
- **`send_email` action**: rule fires → `_act_send_email` enqueues a `send` job (not an inline SMTP call), so SMTP I/O happens on the durable queue. The action is **non-idempotent** (added to `NON_IDEMPOTENT_ACTION_TYPES`, `actions.py:39`) → at-most-once via the committed `in_flight` marker on `outreach_sends`. Sends are metered (if `BILLING_ENABLED`) via `check_and_debit` before the SMTP call.

A user inspects results in: the sequence stats endpoint, the `outreach_sends` log, and `trigger_runs`/`trigger_action_results`.

### 1.5 Email snapshot at enroll (NEW — closes a blocking gap)
`outreach_enrollments` stores `to_email_snapshot` captured from `PgLeadStore(ws).get_lead(lead_id).email` **at enroll time**. Rationale: leads can be deleted or change email between enroll and send; an outreach system must not silently re-target a different address or fail opaquely. Resolution rule at send: **use the snapshot**; if the lead still exists and its email differs, log at INFO but **still send to the snapshot** (the address the user consented/enrolled against). If the snapshot is empty/invalid at enroll, the enroll is rejected (`no_email`). Suppression checks (enroll, pre-send) all operate on the normalized snapshot (§6.4.1).

---

## 2. Architecture & Data Flow

```
Router (/api/outreach, Depends(current_workspace))  ── sets current_workspace_var (tenancy)
   │
   ├─ Sequence CRUD / enroll / status  ──►  PgOutreachStore(ws_id)   (mirrors PgLeadStore store.py:141)
   │                                            │  _session() re-binds contextvar (store.py:158)
   │                                            ▼
   │                                       PG tables (RLS): outreach_sequences,
   │                                       outreach_enrollments, outreach_sends,
   │                                       outreach_suppressions
   │                                       + outreach_schedules (NON-RLS mirror, like scheduled_triggers)
   │                                            ▲  after_begin hook SET LOCAL app.workspace_id (database.py:53)
   │
   ├─ "send now" / POST /execute  ──► queue_service.add_job("send", {workspace_id, idem_key, ...})
   │
   ├─ Autonomous ticker: bootstrap_outreach_schedules() (cold start) +
   │   per-active-sequence re-enqueue (single-flight) reads outreach_schedules mirror
   │   WITHOUT a GUC, then per due seq re-enters workspace_scope to enqueue due "send" jobs
   │
Trigger engine (engine.py) ── _act_send_email ──► same "send" job
                                           ▼
                          worker claim (queue_service.py:127, FOR UPDATE SKIP LOCKED)
                                           ▼
                          handle_send(job_id, payload)  ── wraps ALL work in workspace_scope(ws_id)
                                           │  MUST go through PgOutreachStore so assert_rls_role fires (§7)
                                           ├─ send-window check (skip→reschedule, NOT terminal)
                                           ├─ suppression check  (outreach_suppressions)
                                           ├─ rate check (durable count; over→reschedule, NOT terminal)
                                           ├─ caps.try_reserve + billing.check_and_debit (if metered)
                                           ├─ committed in_flight marker on outreach_sends (UNCONDITIONAL, §6.2.1)
                                           ├─ get_smtp_config(ws_id) → WI-6 get_secret (sender.py:38)
                                           ├─ send_email(... config=cfg, headers=... )  (sender.py:131, extended)
                                           ├─ classify SMTP response (5xx hard → suppress, §6.6.1)
                                           └─ terminal update of send + enrollment state
```

**Self-host / SQLite split**: identical to leads — on SQLite or `PG_LEAD_STORE=False` the store falls back to a SQLite implementation (`workspace_id`-columned single file); RLS is PG-only, app-layer `workspace_id` filtering is always on. Backend selection mirrors `use_pg_store()` (`store.py:98-103`) including `assert_rls_role` (`store.py:39`). The `outreach_schedules` mirror is non-RLS on both dialects (read without a GUC at bootstrap, mirroring `scheduled_triggers`).

---

## 3. Data Model

All new tables live on the single `Base` from `apps.api.database`, in a new module `apps/api/services/outreach/orm_models.py`, mirroring `services/leadgen/orm_models.py`. Every **tenant** table has **NOT NULL `workspace_id` as the first column** and composite `(workspace_id, …)` indexes. `outreach_schedules` is a NON-RLS mirror.

### 3.1 `outreach_sequences` (RLS)
| column | type | notes |
|---|---|---|
| `id` | String(36) PK | uuid |
| `workspace_id` | String(64) NOT NULL | tenancy key |
| `name` | String(200) NOT NULL | |
| `description` | Text default '' | |
| `steps` | JSON NOT NULL default `[]` | array of `{step_number,subject,body_html,delay_hours}` |
| `status` | String(20) default 'draft' | draft/active/paused/completed |
| `daily_limit` | Integer default 50 | |
| `send_window_start` | Integer default 9 | hour 0-23 |
| `send_window_end` | Integer default 18 | hour 0-23 |
| `send_window_tz` | String(40) default 'UTC' | IANA tz name; window evaluated in this tz (§6.3) |
| `consent_basis` | String(40) NOT NULL | lawful-basis attestation, required before `active` (§6.7) |
| `created_at`/`updated_at` | DateTime(tz) server_default now | standardized on tz DateTime |

Indexes: `ix_outreach_seq_ws (workspace_id)`, `ix_outreach_seq_ws_status (workspace_id, status)`.

### 3.2 `outreach_enrollments` (RLS) — replaces `sequence_leads`
| column | type | notes |
|---|---|---|
| `id` | Integer PK autoincrement | |
| `workspace_id` | String(64) NOT NULL | |
| `sequence_id` | String(36) NOT NULL | validated to exist in ws at enroll (§7) |
| `lead_id` | Integer NOT NULL | workspace-scoped; resolved via PgLeadStore |
| `to_email_snapshot` | String NOT NULL | normalized email captured at enroll (§1.5, §6.4.1) |
| `consent_source` | String(120) default '' | how the address was obtained (§6.7) |
| `consent_at` | DateTime(tz) nullable | opt-in timestamp (§6.7) |
| `current_step` | Integer default 0 | |
| `status` | String(20) default 'pending' | StepStatus machine |
| `next_send_at` | DateTime(tz) nullable | due-send scheduling |
| `sent_count` | Integer default 0 | |
| `soft_bounce_count` | Integer default 0 | soft-bounce counter (§6.6.1) |
| `last_sent_at` | DateTime(tz) nullable | |
| `error` | Text default '' | |

Constraints/indexes: `UniqueConstraint(workspace_id, sequence_id, lead_id, name="uq_enroll_ws_seq_lead")` (was `UNIQUE(sequence_id, lead_id)` — now tenant-keyed, the core fix); `ix_enroll_ws_seq_status (workspace_id, sequence_id, status)`; `ix_enroll_ws_due (workspace_id, status, next_send_at)` for the due-send query.

### 3.3 `outreach_sends` (RLS) — replaces `send_log`, doubles as send idempotency ledger
| column | type | notes |
|---|---|---|
| `id` | Integer PK autoincrement | |
| `workspace_id` | String(64) NOT NULL | |
| `sequence_id` | String(36) nullable | nullable for standalone trigger sends |
| `enrollment_id` | Integer nullable | |
| `lead_id` | Integer nullable | |
| `step_number` | Integer | |
| `to_email` | String NOT NULL | |
| `subject` | String | |
| `status` | String(20) default 'in_flight' | in_flight/sent/failed/bounced/skipped |
| `skip_reason` | String(40) nullable | `suppressed`/`rate_limit`/`window`/`replay`/`no_email`/`credits`/`cap`/`lead_not_found` |
| `message_id` | String default '' | |
| `idempotency_key` | String(255) NOT NULL | per-send key (every producer mints one, §8.1) |
| `charged_usd` | Float default 0.0 | |
| `migrated` | Boolean default false | true for rows inserted by the migrator (never billed, §8.4) |
| `error` | Text default '' | |
| `sent_at` / `opened_at` / `replied_at` / `bounced_at` | DateTime(tz) nullable | webhook hooks update these |
| `created_at`/`updated_at` | DateTime(tz) server_default now | |

Constraints/indexes: `UniqueConstraint(workspace_id, idempotency_key, name="uq_outreach_send_idem")` (mirrors `uq_trigger_action_idem`); `ix_send_ws_seq_created (workspace_id, sequence_id, created_at)`; `ix_send_ws_status (workspace_id, status)`; `ix_send_ws_msgid (workspace_id, message_id)`; `ix_send_ws_sent_at (workspace_id, sent_at)` for the durable per-hour rate count (§6.3).

### 3.4 `outreach_suppressions` (RLS) — NEW
| column | type | notes |
|---|---|---|
| `id` | Integer PK autoincrement | |
| `workspace_id` | String(64) NOT NULL | |
| `email` | String NOT NULL | stored in canonical normalized form (§6.4.1) |
| `reason` | String(20) | `unsubscribe`/`bounce`/`complaint`/`manual` |
| `source` | String(40) | sequence_id or trigger_id or 'webhook' |
| `locked` | Boolean default false | true for `unsubscribe`/`complaint` → cannot be removed via API (§7) |
| `created_at` | DateTime(tz) server_default now | |

Constraints/indexes: `UniqueConstraint(workspace_id, email, name="uq_suppress_ws_email")`; `ix_suppress_ws_email (workspace_id, email)`.

### 3.5 `outreach_schedules` (NON-RLS mirror) — NEW, models `scheduled_triggers`
Drives the autonomous ticker (§6.8). Read at cold-start WITHOUT a workspace GUC, exactly like `scheduled_triggers`.
| column | type | notes |
|---|---|---|
| `sequence_id` | String(36) PK | |
| `workspace_id` | String(64) NOT NULL | used to re-enter `workspace_scope` |
| `next_tick_at` | DateTime(tz) nullable | |
| `enabled` | Boolean default false | true while sequence is `active` |
| `updated_at` | DateTime(tz) server_default now | |

Index: `ix_outreach_sched_due (enabled, next_tick_at)`. **Not** RLS-enabled (matches `scheduled_triggers`, `a1b2:167-168`); it carries no recipient PII — only sequence/workspace ids + a timestamp.

### 3.6 Migration plan (new Alembic revision, `down_revision = a1b2c3d4e5f6`)

`migrations/versions/<rev>_outreach_rls.py`, copying the `a1b2c3d4e5f6` structure (`a1b2:157-221`):

1. `op.create_table` for all five tables + `op.create_index` for every composite index **on all dialects** (PG-only DDL guarded by `if op.get_bind().dialect.name == "postgresql"`, `a1b2:157`).
2. `_pg_upgrade()` mirroring `a1b2:_pg_upgrade`:
   - `APP_DB_ROLE = os.getenv("YUPCHA_APP_DB_ROLE", "yupcha_app")` (matches `config.py` `APP_DB_ROLE`).
   - **Do NOT create the role** — it already exists from `c42d0273d9bd`; downstream migrations assume it (`a1b2:164-168`).
   - `GRANT SELECT, INSERT, UPDATE, DELETE` on all **five** tables (the four RLS tenant tables + `outreach_schedules`).
   - `GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO yupcha_app` (serial PKs).
   - For the **four tenant tables only**: `ENABLE` + `FORCE ROW LEVEL SECURITY` + one `CREATE POLICY <tbl>_workspace_isolation USING (workspace_id = current_setting('app.workspace_id', true)) WITH CHECK (...)` (`a1b2:181-190`) — fail-closed.
   - `outreach_schedules` gets DML grants but is **intentionally NOT RLS-enabled** (read without a GUC at bootstrap, `a1b2:167-168`).
3. `_pg_downgrade()` drops the four policies only; never drops the shared role (`a1b2:216-221`).
4. Register the new ORM module in the model-import block in `main.py` (alongside the leadgen/automations ORM imports) so `Base.metadata` is complete for `create_all` on SQLite/tests.

---

## 4. Data Migration (existing `outreach.db` → tenant tables)

Current volume negligible (1 sequence, 0 enrollments, 0 sends), but the migrator is mandatory, idempotent, and resumable. Extend `apps/api/scripts/migrate_sqlite_to_pg.py` with `migrate_outreach()`, modeled on `migrate_per_workspace_leads_signals`.

### 4.1 Workspace assignment
- **Default**: attribute all legacy rows to the `main` workspace (the script's convention for the legacy single-file `signals.db`), overridable via env `OUTREACH_MIGRATION_WS`.
- Source path: `os.path.join(settings.DATA_DIR, "outreach.db")`.

### 4.2 The non-unique integer `lead_id` problem
- Run `migrate_outreach()` **after** `migrate_per_workspace_leads_signals()`.
- For each legacy enrollment/send, **validate `lead_id` resolves** via `SELECT 1 FROM leads WHERE workspace_id=:ws AND id=:lid` (under the per-batch GUC). Unresolved rows: enrollment skipped + logged; send written as `status='skipped', skip_reason='lead_not_found', migrated=true`. Never silently dropped or mis-attributed.
- For resolved enrollments, backfill `to_email_snapshot` from the resolved lead's current email; if empty, skip + log.

### 4.3 Mechanics
- `with pg_engine.begin() as dst:` then `dst.execute(text("SELECT set_config('app.workspace_id', :ws, true)"), {"ws": ws_id})` per batch so RLS WITH CHECK passes for the owner.
- Stamp `workspace_id=ws_id` and `migrated=true` on every payload; convert `steps TEXT` JSON → JSON column; convert REAL epoch → tz `DateTime`. Default `consent_basis='legacy_migrated'`, `consent_source='legacy_import'`.
- **Migrated sends are inserted DIRECTLY as terminal rows with their legacy status — they NEVER pass through `check_and_debit`/caps/`handle_send`** (§8.4). `charged_usd=0.0`.
- Synthesize `idempotency_key='migrate:<legacy_send_id>'`; use `INSERT … ON CONFLICT (workspace_id, idempotency_key) DO NOTHING`. Sequences keyed by uuid PK with ON CONFLICT DO NOTHING.
- Add the five table names to `_TENANT_TABLES` so the generic loader skips them.
- Call `migrate_outreach()` from `main()` after `migrate_per_workspace_leads_signals()`.

---

## 5. API Surface

Router `apps/api/routers/outreach.py` (prefix `/api/outreach`) is **fully re-derived onto tenancy** — currently **none** of its endpoints take `WorkspaceCtx`. Every endpoint gets `ctx: WorkspaceCtx = Depends(current_workspace)` (reads) or `Depends(require_workspace_role("admin"))` (mutations), following `routers/automations.py:39`. Service functions take `workspace_id`/`PgOutreachStore`, not module-globals.

| Method | Path | Scope | Notes |
|---|---|---|---|
| GET | `/api/outreach/sequences` | ctx | list ws sequences |
| POST | `/api/outreach/sequences` | admin | create |
| GET | `/api/outreach/sequences/{id}` | ctx | detail + stats |
| PUT | `/api/outreach/sequences/{id}` | admin | update |
| DELETE | `/api/outreach/sequences/{id}` | admin | delete (cascades enrollments/sends + removes schedule mirror) |
| POST | `/api/outreach/sequences/{id}/start` | admin | requires `is_smtp_configured(ctx.workspace_id)` + non-empty `consent_basis` + non-empty `OUTREACH_FOOTER` (§6.7); enables `outreach_schedules` row |
| POST | `/api/outreach/sequences/{id}/pause` | admin | disables `outreach_schedules` row |
| POST | `/api/outreach/sequences/{id}/enroll` | ctx | enroll ws lead_ids; validates sequence in ws; snapshots email; filters suppressed; requires consent fields |
| POST | `/api/outreach/sequences/{id}/execute` | admin | enqueues `send` jobs for due steps (no inline send); FOR UPDATE SKIP LOCKED on the due query (§9.12) |
| GET | `/api/outreach/sequences/{id}/stats` | ctx | |
| GET | `/api/outreach/sequences/{id}/sends` | ctx | send log inspection |
| GET | `/api/outreach/smtp/status` | ctx | per-ws `get_smtp_config(ctx.workspace_id)`; never returns password |
| PUT | `/api/outreach/smtp/config` | admin | writes WI-6 `set_secret(ctx.workspace_id, ...)` not global `_db_set` |
| POST | `/api/outreach/smtp/test` | admin | `send_test_email` with per-ws config |
| GET | `/api/outreach/suppressions` | ctx | list/manage |
| POST | `/api/outreach/suppressions` | admin | manual add (reason='manual') |
| DELETE | `/api/outreach/suppressions/{email}` | admin | remove — **only when `locked=false`** (reason ∈ bounce/manual); removing an `unsubscribe`/`complaint` returns 403 (§7) |
| GET | `/api/outreach/unsubscribe` | **public, token-auth** | unsubscribe landing/confirm page |
| POST | `/api/outreach/unsubscribe` | **public, token-auth, NO CSRF, side-effecting, idempotent** | RFC 8058 one-click target (§5.1) |
| POST | `/api/outreach/webhooks/bounce` | **provider-auth** | bounce/complaint hook (§6.6) |

### 5.1 Unsubscribe endpoint auth & RFC 8058 one-click
- The unsubscribe link carries an **HMAC-signed token** over `{workspace_id, email, sequence_id, exp}`. The HMAC key is derived through the **same fail-closed `_load_master_key()` path** as secret encryption (`secrets.py:60-90`) — NOT a bare `SECRET_KEY` read — so a default/insecure `SECRET_KEY` in a real deployment cannot forge tokens (tenancy issue, §7). Verification is **constant-time** (`hmac.compare_digest`).
- Token scope is **per-recipient-per-workspace** (`email`+`workspace_id`), `sequence_id` recorded as `source` only. TTL via `exp` (default 90 days, configurable `OUTREACH_UNSUB_TTL_DAYS`); expired tokens still render a landing page that lets the recipient re-request a fresh link, but the POST itself rejects an expired token to bound replay.
- **POST is one-click compliant**: no login, **no CSRF token** (a CSRF guard would break RFC 8058), side-effecting immediately, and **idempotent** (insert into `outreach_suppressions` with `reason='unsubscribe', locked=true` via ON CONFLICT DO NOTHING). The write is wrapped in `workspace_scope(ws_id)` where `ws_id` comes only from the verified token.
- **Rate-limit the public endpoint** (per source IP + per token) to blunt a harvested-link hammering attack; the suppression insert is idempotent so repeats are cheap, but the limiter caps DB churn.

---

## 6. SMTP / Sending

### 6.1 Per-workspace credentials
`get_smtp_config(workspace_id)` is already correct (`sender.py:38-61`). The fix is **callers must pass `workspace_id`**: `handle_send` calls `get_smtp_config(payload["workspace_id"])`, replacing the no-arg call at `sender.py:150`. `update_smtp_config` writes `set_secret(ctx.workspace_id, "SMTP_*", value)` replacing global `_db_set`.

### 6.2 Send via the job queue
A new `send` job type:
- Register `"send"` in **both** `worker.py:_register_handlers` (after line 79) **and** `main.py` (after line 138) — both lists must match (`worker.py:74-79`, `main.py:124-138`).
- Add `JOB_TIMEOUTS["send"] = 300` (`queue_service.py:32-45`).
- `handle_send(job_id, payload)` early-exits if `AUTOMATIONS_ENABLED` is False (matching `handle_trigger_eval` `engine.py:254`), then wraps **all** DB work in `with workspace_scope(payload["workspace_id"])` (`engine.py:269`; the queue sets no GUC — handler owns tenancy).

#### 6.2.1 `handle_send` exact ordering (the safety-critical path — fully specified)
This is reimplemented from scratch following the `_run_one_action` ordering (`engine.py:144-211`); the engine's replay logic operates on `TriggerActionResult` and is **not importable** for `outreach_sends`. The ordering invariant exists specifically to survive the queue retry loop (`queue_service.py:329-346`) and the heartbeat watchdog (`queue_service.py:370-409`), both of which re-run a job that crashed after a successful SMTP send.

Per send, inside `workspace_scope` and a `PgOutreachStore` session (so `assert_rls_role` fires in the worker, §7):

1. **Resolve job** → `idempotency_key` is taken from the payload (every producer mints it, §8.1) — never re-derived inside the handler from producer-specific strings.
2. **Replay / dedup check** on `outreach_sends` by `(workspace_id, idempotency_key)`:
   - row exists with terminal status (`sent`/`failed`/`skipped`/`bounced`) → return (skipped, no send).
   - row exists with `in_flight` → this is a crash-replay; `send_email` is non-idempotent → set `status='skipped', skip_reason='replay'`, commit, return. **Mock SMTP must be called zero extra times.**
3. **Load enrollment** (if sequence-driven); if missing (sequence deleted mid-run) → record `skipped`, return (§9.11).
4. **Send-window check** (§6.3). Outside window → **do NOT write a terminal `outreach_sends` row**; reset `enrollment.next_send_at` to the next in-window slot and return success (job completes; the ticker re-enqueues later). This is the reschedule path (§6.3, fixes the silent-drop contradiction).
5. **Suppression check** on the normalized recipient (§6.4.1). Suppressed → `status='skipped', skip_reason='suppressed'`, advance enrollment to terminal `suppressed`, return.
6. **Durable rate check** (§6.3). Over the per-hour cap → **do NOT write a terminal row**; reset `enrollment.next_send_at` forward (e.g. +remaining window of the hour) and return success → ticker re-enqueues. (Reschedule, not silent drop.)
7. **Caps + billing** (only when `cost > 0`, §8.3):
   - `reservation = caps.try_reserve(...)`; not ok → `skipped/cap`, return.
   - `billing.check_and_debit(db, ws, cost, run_id=idempotency_key, reason="outreach_send")`; `InsufficientCreditsError` → release reservation, `skipped/credits`, return. (Idempotent on `run:<key>`, `service.py:196-208`.)
8. **PRE-SEND in_flight marker — committed BEFORE the SMTP call, UNCONDITIONALLY** (even when unmetered/`cost==0`/billing off, §8.3): insert/reuse the `outreach_sends` row with `status='in_flight', charged_usd=cost`, `db.commit()`. On a concurrent insert hitting `uq_outreach_send_idem`, catch `IntegrityError`, rollback, re-read, and treat as the replay case in step 2. This commit is what makes the queue's automatic retry safe.
9. **SMTP send**: build headers (§6.5) with CRLF sanitization (§6.5.1), `send_email(... config=cfg ...)`.
10. **Classify result** (§6.6.1): success → `sent`+`sent_at`+`message_id`; synchronous 5xx hard bounce → `bounced`, suppress recipient, terminal enrollment `bounced`; transient/connection error → `failed` (queue will retry the whole job; step 2 will then short-circuit to `skipped/replay` because the in_flight marker is committed — at-most-once).
11. **Settle/release** reservation by outcome (`engine.py:197-201`); **refund-on-failure out of scope** but see §8.5 reconciliation. Terminal update of send + enrollment, `db.commit()`.

**Crash matrix (reconciled with the real queue):**
- Crash after debit, before in_flight commit → replay finds no row; debit idempotent-replays (no double charge); fresh in_flight; SMTP proceeds (first real attempt). Correct.
- Crash after in_flight commit, before/after SMTP → retry/watchdog re-runs; step 2 finds `in_flight`, flips to `skipped/replay`; **no second SMTP, no second debit**. At-most-once.
- This is the accepted at-most-once over at-least-once tradeoff (§9.10), now pinned to the actual `queue_service.py:329-346` retry path.

### 6.3 Throttling / rate per workspace + send-window
The module-global `_rate_limiter` (`sender.py:96,155,192`) is process-wide and **must not gate per-workspace sends**. Two changes:
- **Durable per-workspace hourly rate**: in `handle_send`, count `outreach_sends WHERE workspace_id=:ws AND status='sent' AND sent_at >= now()-1h` (uses `ix_send_ws_sent_at`) against `get_smtp_config(ws).max_per_hour`. Durable, survives restart, isolated per workspace. The in-process `_rate_limiter` in `send_email` is **bypassed** when a `config` is passed by the handler (the handler is now the rate authority); to avoid the global limiter blocking cross-tenant, `send_email` only consults `_rate_limiter` when called without the handler's explicit signal — simplest: remove the `_rate_limiter` gate from `send_email` and make rate purely the handler's durable count.
- **Send-window**: evaluate `now` in `sequence.send_window_tz` (IANA); a send whose local hour ∉ `[send_window_start, send_window_end)` is rescheduled (§6.2.1 step 4), never sent out-of-window via `/execute` or a trigger. Trigger-driven `send_email` actions also honor the target sequence's window when `sequence_id` is set; standalone trigger sends with no sequence use a workspace default window secret `OUTREACH_DEFAULT_WINDOW` (default 24h/no-window).
- The sequence `daily_limit` caps per-day sends per sequence (durable count of `sent` rows for the sequence in the local day); `SMTP_MAX_PER_HOUR` caps per-hour per workspace.

### 6.4 Suppression-list enforcement
Enforce at: (1) **enroll** (skip suppressed snapshots); (2) **immediately before SMTP** in `handle_send` (TOCTOU-safe — an unsubscribe between enroll and send still blocks); (3) **on bounce/complaint webhook** and **on unsubscribe** (insert suppression).

#### 6.4.1 Canonical email normalization (NEW — closes a compliance hole)
Applied **identically** at enroll, send, suppress, webhook, and unsubscribe:
- Trim whitespace; NFKC unicode normalize; lowercase the whole address.
- Split `local@domain`; IDNA-encode the domain (punycode); lowercase domain.
- **Do NOT** strip Gmail dots/plus-aliases by default (over-normalizing risks suppressing the wrong person); instead store the lowercased canonical form and additionally store/compare a **provider-aware variant for gmail.com/googlemail.com only** (strip dots in local-part, drop `+tag`) as a secondary suppression match key. Suppression lookup checks both the exact canonical form and, for Gmail domains, the dot/plus-collapsed form, so `User.Name+x@gmail.com` and `username@gmail.com` resolve to the same suppression. Non-Gmail addresses are matched only on the exact canonical form (RFC-safe).

### 6.5 Unsubscribe / compliance (CAN-SPAM / GDPR)
Every outbound message MUST include:
- `List-Unsubscribe` header with the **HTTPS one-click URL** (signed token, §5.1) and `List-Unsubscribe-Post: List-Unsubscribe=One-Click` (RFC 8058). A `mailto:` is included **only if** inbound mailto processing exists; otherwise it is **omitted** (advertising a non-functional mailto is a compliance/anti-spam negative, §6 deliverability). Out of scope: IMAP inbound mailto parsing → so v1 ships HTTPS-only `List-Unsubscribe`.
- A **visible unsubscribe link injected into BOTH MIME parts**: the HTML body and the `text/plain` part. `send_email` currently auto-derives plaintext by stripping HTML (`sender.py:168-171`), which would drop the link — so the handler builds the unsubscribe + footer block and injects it into **both** the HTML and the explicitly-provided `body_text` (CAN-SPAM requires it be conspicuous in the message the recipient actually reads).
- A **physical-address / sender-identity footer** from `get_secret(ws, "OUTREACH_FOOTER")`. **Sending is BLOCKED** (sequence cannot go `active`; `handle_send` records `skipped/no_footer`) when the footer is missing/empty (§6.7) — not lint-warned.

`send_email` is extended to accept `headers: dict` and an explicit `body_text`, and to set `List-Unsubscribe`/`List-Unsubscribe-Post`.

#### 6.5.1 Header-injection hardening (NEW — closes an SMTP injection vector)
`from_name`, `subject`, and `to_email` flow into MIME headers (`sender.py:163-165`). All three are **CRLF-stripped and length-bounded** before being set on the message; `to_email` is validated against an email regex and rejected (send `failed`) if it contains control chars or multiple addresses. Subjects come from user templates (`render_template`) → strip `\r`/`\n` and any embedded header-like lines. Prefer `email.headerregistry`/`Header` encoding over raw f-strings for `From`.

### 6.6 Bounce/complaint hooks
`/api/outreach/webhooks/bounce` accepts provider callbacks (SES SNS / Mailgun / generic). **Auth chicken-and-egg resolved (§7):** the webhook is authenticated by a **provider-level (platform-global) shared secret / signature** verified BEFORE any workspace is trusted — NOT a per-workspace secret. Configuration: `OUTREACH_BOUNCE_WEBHOOK_SECRET` (platform env, cloud) and/or provider native signature verification (SES SNS cert chain, Mailgun signing key). Only after the signature verifies do we look up the send by `message_id` to derive `workspace_id`, then enter `workspace_scope(ws)` for the write. The message_id→workspace lookup runs under a **system/non-tenant read** (the lookup query filters by `message_id` across the platform; it is the one read that legitimately spans tenants and is done with a dedicated owner connection, not the app role under a GUC). It sets `outreach_sends.status='bounced'`/`bounced_at`, advances the enrollment, and inserts a suppression (`reason='bounce'|'complaint'`, `locked=true` for complaints).

#### 6.6.1 Synchronous SMTP response classification (NEW)
`send_email`'s failure path classifies the SMTP response code (the exception/`SendResult.error` carries it):
- **5xx hard bounce at send time** (e.g. 550 mailbox unknown) → `outreach_sends.status='bounced'`, suppress recipient (`reason='bounce'`), terminal enrollment `bounced`. Stops the sequence from re-mailing a known-bad address every step.
- **4xx / connection / transient** → `failed`; queue retries the job (the in_flight marker makes the retry at-most-once).
- **Soft bounce via async webhook** → increment `enrollment.soft_bounce_count`; at threshold (`OUTREACH_SOFT_BOUNCE_MAX`, default 3) suppress + terminal enrollment. (The async webhook count is the mechanism — NOT the queue `max_retries`, which is unrelated to bounce events; this corrects §9.8 of the prior draft.)

### 6.7 Consent / lawful-basis gating (NEW — GDPR)
- A sequence cannot transition to `active` unless `consent_basis` is non-empty (an admin attestation of lawful basis) AND `OUTREACH_FOOTER` (physical address) is set AND SMTP is configured.
- Enrollment records `consent_source` + `consent_at` (caller-supplied or defaulted from the enroll context). For trigger-driven `_act_sequencer`, the consent fields default to the rule id + fire time; the sequence-level `consent_basis` attestation is the binding gate.

### 6.8 Autonomous sequence ticker (NEW — closes the blocking functional gap)
Multi-step sequences need delayed steps to fire without manual `/execute`. Reuse the automations schedule machinery verbatim in shape:
- `outreach_schedules` mirror (§3.5) is the non-RLS cold-start source, exactly like `scheduled_triggers` (`engine.py:497-532`).
- `bootstrap_outreach_schedules()` (registered/called in `main.py` next to `bootstrap_schedules()` at `main.py:165`, gated on `AUTOMATIONS_ENABLED`): reads enabled `outreach_schedules` rows due now WITHOUT a GUC, then per sequence re-enters `workspace_scope(ws)` and enqueues a `send` job per due enrollment step (single-flight via `_enqueue_if_absent` keyed on the per-send idem key, mirroring `_enqueue_schedule_if_absent` `engine.py:408-441`).
- A recurring tick: after a sequence is started or after each tick, set `outreach_schedules.next_tick_at = now + OUTREACH_TICK_INTERVAL` (default 15 min) and single-flight re-enqueue, mirroring `_reschedule` (`engine.py:454-470`). The tick query selects due enrollments (`status in pending/scheduled AND next_send_at <= now`) with FOR UPDATE SKIP LOCKED and enqueues `send` jobs; pausing/completing the sequence sets `enabled=false`.
- `start` enables the mirror row; `pause`/`delete`/`complete` disable/remove it.

---

## 7. Tenancy & Security

- **RLS on the four tenant tables** via the migration (§3.6), fail-closed (NULL GUC → zero rows). `outreach_schedules` is non-RLS by design (carries no recipient PII; read without a GUC at bootstrap).
- **PgOutreachStore** mirrors `PgLeadStore` belt-and-suspenders (`store.py:141-208`): raises on empty `workspace_id` (`store.py:150`); `_session()` re-binds `current_workspace_var` (`store.py:158-164`); every query filters `…workspace_id == self.workspace_id`; every write force-stamps `workspace_id` (`store.py:178`).
- **`assert_rls_role` MUST fire in the worker send path.** `handle_send` opens its DB work through `PgOutreachStore` / `use_pg_store()` (`store.py:98-103`), NOT a bare `SessionLocal()`, so the memoized superuser/BYPASSRLS check (`store.py:39-85`) runs in the worker. A misconfigured superuser role must make sends fail-fast, not silently bypass RLS.
- **No cross-tenant enrollment/send**: lead resolution uses `PgLeadStore(ws).get_lead(lead_id)` (scoped), replacing the raw `LeadDB().get_lead(...)` at `sequence.py:344`. Sends use the enrollment's `to_email_snapshot`. RLS WITH CHECK is the backstop.
- **Sequence-existence validation at enroll**: the enroll endpoint and `_act_sequencer` `SELECT` the sequence in `ctx.workspace_id` before inserting an enrollment (RLS does not catch a same-ws-stamped enrollment pointing at a non-existent/foreign sequence id) → 404 if absent.
- **Secrets**: SMTP creds only via `get_secret(ws_id, ...)` (`secrets.py:210`), Fernet-encrypted (`secrets.py:97`). Never logged. `smtp/status` never returns the password.
- **Unsubscribe token key** goes through the **same fail-closed `_load_master_key()` path** as encryption (`secrets.py:60-90`) and is **constant-time** compared (`hmac.compare_digest`). A forged token cannot suppress another tenant's recipient because `workspace_id` is derived only from the verified token, then bound via `workspace_scope`.
- **Bounce webhook auth** uses a **platform-global** secret/signature verified BEFORE workspace lookup (§6.6), resolving the chicken-and-egg; the cross-tenant message_id→ws lookup runs on a dedicated owner connection, not the app role.
- **Un-suppressing is gated**: `DELETE /suppressions/{email}` refuses (403) when `locked=true` (reason ∈ `unsubscribe`/`complaint`) — re-mailing an unsubscribed/complained recipient is a CAN-SPAM/GDPR violation. Only `bounce`/`manual` may be removed, and the removal is logged to the activity feed.
- **Per-workspace suppression is intentional** (uq_suppress_ws_email): a recipient who unsubscribes from workspace A is still mailable by workspace B (each tenant is an independent sender/controller; the per-workspace token reflects this). This is documented; an optional platform-global suppression list is **out of scope** (open question §below).
- **SSRF**: the bounce webhook is inbound (no SSRF). Unsubscribe links point only at our host. Any future customer-webhook send must reuse `_validate_and_pin`/`_send_webhook_pinned` (`actions.py:89-174`).

---

## 8. Cost / Abuse Control

- **Metering**: a send's platform cost is `settings.OUTREACH_SEND_COST_USD` (new, default 0.0 → free on self-host). `project_action_cost` (`actions.py:74`) returns this for `send_email` (and 0 for `sequencer`). Note: at default 0.0, `paid = cost > 0` is False (`engine.py:145`), so caps/billing are bypassed — but the in_flight marker is still written (§8.3).

### 8.1 Idempotency key scheme — ALL producers (closes the NOT NULL/collision gap)
`idempotency_key` is NOT NULL + unique; **every producer mints one**:
- **Trigger send**: `trig:<ws>:<trigger>:<wb>:<row>:<idx>:<fire_key>` (matches `engine.py:119`).
- **Sequence-tick / `/execute` send**: `seq:<ws>:<sequence>:<enrollment>:<step>` — **keyed on (enrollment, step), NOT on the producer** (so the autonomous ticker and a manual `/execute` for the same step collapse to one row).
- **Send-now (single ad-hoc)**: `now:<ws>:<lead_or_email_hash>:<step>:<caller_request_id>`.
- **Migration**: `migrate:<legacy_send_id>`.

### 8.2 Cross-producer dedup (closes the double-send/double-charge gap)
If both a `send_email` rule and the sequence tick can target the **same enrollment/step**, they would otherwise mint different keys (`trig:…` vs `seq:…`) and double-send/double-charge. Rule: when a `send_email` action targets a specific `sequence_id`+enrollment step, `_act_send_email` mints the **`seq:<ws>:<sequence>:<enrollment>:<step>`** key (the canonical per-step key), NOT the trigger key, so it unifies with the ticker. Only standalone trigger sends with no sequence step use the `trig:` key. This makes `(enrollment, step)` the dedup unit for sequence sends.

### 8.3 in_flight marker is UNCONDITIONAL (closes the free-install double-send gap)
The pre-send `in_flight` `outreach_sends` row is committed **for every non-replay send regardless of `cost`/`BILLING_ENABLED`** (mirrors `engine.py:175`, written for all non-dry-run actions). On self-host (cost=0, billing off) there is no reservation/debit, but the marker still gives at-most-once. `handle_send` MUST NOT skip the marker when unmetered.

### 8.4 Migrated sends never bill
Migrated rows are inserted directly as terminal (`migrated=true`, `charged_usd=0.0`) and **never pass through `check_and_debit`/caps/`handle_send`** (§4.3). The ledger is untouched by migration.

### 8.5 Spend ceiling, caps, dedup
- `check_and_debit(db, ws, cost, run_id=<idem>, reason="outreach_send")` idempotent on `run:<idem>` (`service.py:196-208`), row-locks balance (`service.py:210`), `InsufficientCreditsError`→`skipped/credits`.
- `caps.try_reserve` enforces per-rule + workspace-global daily USD (`engine.py:151`); plus sequence `daily_limit` + `SMTP_MAX_PER_HOUR`.
- Enrollment dedup: `uq_enroll_ws_seq_lead` → ON CONFLICT DO NOTHING → "already enrolled".
- **Refund-on-failure out of scope** (matches engine stance, `engine.py:202-204`), BUT because SMTP failure rates are high for a *metered* email product, this spec mandates an **operator-facing reconciliation report**: a query/endpoint listing metered sends with `status in (failed)` and `charged_usd>0` per workspace per day, so support can issue manual credits. Documented operator runbook item, not auto-refund.

---

## 9. Failure Modes & Edge Cases (exhaustive)

1. **Partial send**: each send is its own row + idem key + debit; one failure never rolls back others.
2. **Crash between debit and send**: committed in_flight marker before SMTP (§6.2.1 step 8); retry → `skipped/replay`; debit idempotent. At-most-once, no double charge.
3. **Duplicate enrollment**: `uq_enroll_ws_seq_lead` → ON CONFLICT DO NOTHING.
4. **Lead in multiple workspaces**: enrollment carries `workspace_id`; send uses the snapshot + `PgLeadStore(ws)`; RLS backstop. Fixes `sequence.py:344`.
5. **Suppressed/unsubscribed after enroll**: pre-send check → `skipped/suppressed`, no SMTP.
6. **Invalid/missing SMTP creds**: `is_smtp_configured(ws)` gate on start → 400; in handler `send_email` returns `SMTP not configured` (`sender.py:152-153`) → `failed`, no debit settled.
7. **Rate limit reached**: **rescheduled** (no terminal row; `next_send_at` reset; ticker re-enqueues) — NOT a terminal `skipped` that the queue would mark `completed` and drop (§6.2.1 step 6). Closes the silent-drop contradiction.
8. **Send outside window**: rescheduled to next in-window slot (§6.2.1 step 4), never sent out-of-window.
9. **Hard bounce (sync 5xx)**: suppress immediately, terminal `bounced` (§6.6.1). **Soft bounce (async webhook)**: `soft_bounce_count` increments; threshold → suppress (NOT queue max_retries).
10. **SMTP succeeds but DB update crashes**: in_flight marker remains; replay → `skipped/replay`. At-most-once (documented).
11. **Sequence deleted mid-run**: cascade deletes enrollments/sends + removes `outreach_schedules` row; in-flight `send` jobs find no enrollment → `skipped`.
12. **Concurrent `execute`/ticker/trigger for same step**: same `seq:` idem key + `uq_outreach_send_idem` + replay-check → one send. The due-query uses FOR UPDATE SKIP LOCKED (`queue_service.py:127` pattern) to avoid enqueuing duplicate jobs.
13. **Migration re-run/resume**: `migrate:<id>` + ON CONFLICT → idempotent; migrated rows never billed.
14. **Empty `workspace_id`**: `PgOutreachStore.__init__` and `workspace_scope` raise (`store.py:150`).
15. **Header injection**: CRLF-stripped subject/from_name/to_email (§6.5.1).
16. **Missing footer/consent**: sequence cannot go `active`; ad-hoc send records `skipped/no_footer`.
17. **Expired/forged unsubscribe token**: rejected (constant-time verify; expired → re-request flow), no suppression write.

---

## 10. Observability
- **Send log**: `outreach_sends` (status, skip_reason, charged_usd, message_id, timestamps, migrated) via `GET /sequences/{id}/sends`.
- **Sequence stats**: `get_sequence_stats` re-implemented on the store (`sequence.py:233-254`), scoped.
- **Trigger inspection**: `trigger_runs`/`trigger_action_results` now carry `send_email`/`sequencer` outcomes.
- **Activity feed**: engine `_log_activity` (`engine.py:367-379`); suppression removals also logged.
- **Reconciliation report** (§8.5): metered `failed` sends with `charged_usd>0`.
- **Metrics/logs**: `outreach.sequence`/`outreach.sender` loggers; failures at WARN with `to_email` truncated, never the password.
- **Credit ledger**: each metered send → `CreditLedgerEntry` `reason="outreach_send"`, `run_id=<idem>` (`service.py:216-222`).

---

## 11. Feature Flags / Rollout
- **`AUTOMATIONS_ENABLED`** (default False): master switch; router 404s, `handle_send` early-exits, ticker no-ops (`engine.py:254,500`).
- **`AUTOMATIONS_ALLOW_LEGACY_OUTREACH`** (default False). **Corrected flip (fixes the internal contradiction):** the current block raises 409 in BOTH branches when `atype in ACTION_TYPES_LEGACY` (`automations.py:124-128`; the flag check at :126 is dead). The flip:
  1. Land migration + store + suppression + ticker + `handle_send` + executors.
  2. **Keep `sequencer`/`send_email` registered as valid action types, but make the flag check live in their validation branch — do NOT simply move them into `ACTION_TYPES_V1` (which would skip the `ACTION_TYPES_LEGACY` block entirely and LOSE the gate).** Concretely: treat them as a new accepted set whose create-time validation is `if not AUTOMATIONS_ALLOW_LEGACY_OUTREACH: raise 409 legacy_outreach_disabled`; when on, validate `config.sequence_id` exists in the workspace (sequencer) / SMTP+footer+consent configured (send_email). The flag is consulted on every create — AC10 holds.
  3. Add `send_email` to `NON_IDEMPOTENT_ACTION_TYPES` (`actions.py:39`).
  4. Remove the defensive guard at `actions.py:320-322`; add `_act_sequencer`/`_act_send_email` to the dispatcher (`actions.py:302-322`).
- **Cloud vs self-host**: cloud sets `BILLING_ENABLED=True` + `OUTREACH_SEND_COST_USD>0` + a webhook allowlist + platform bounce secret; self-host leaves billing off (free) with its own SMTP secret. in_flight marker unconditional on both (§8.3).
- **PG vs SQLite**: `use_pg_store()` governs `PgOutreachStore` vs SQLite fallback incl. `assert_rls_role`; RLS + data migration PG-only.
- Rollout: ship dark (flags off) → run data migration on PG → enable one internal workspace → flip `AUTOMATIONS_ALLOW_LEGACY_OUTREACH` globally.

---

## 12. Test Plan + Acceptance Criteria + Out-of-Scope

### 12.1 Unit (SQLite, no PG)
- U1: store CRUD scopes by `workspace_id`; cross-ws read empty.
- U2: enrollment dedup — second enroll no-op.
- U3: `get_smtp_config(ws)` resolves workspace secret over global (mock `get_secret`).
- U4: suppression pre-send skip; List-Unsubscribe header present; unsubscribe link injected into **both** HTML and text/plain; footer present.
- U5: unsubscribe token sign/verify (tamper → reject; constant-time; expired → reject) **and the token HMAC key resolves via `_load_master_key` (fails closed on insecure default in non-dev)**.
- U6: `project_action_cost("send_email")` = configured cost; `"sequencer"` = 0.
- U7: send idempotency — same idem key inserts once.
- U8: async bounce webhook → suppression(locked for complaint) + enrollment `bounced`.
- U9: per-workspace durable rate — ws A at cap does not block ws B.
- U10: empty `workspace_id` raises in store/`workspace_scope`.
- U11 (NEW): SMTP header injection — CRLF/extra-address in subject/from_name/to_email rejected/stripped.
- U12 (NEW): email normalization — `User.Name+x@gmail.com` matches suppression for `username@gmail.com`; non-Gmail matched only on exact canonical form.
- U13 (NEW): rate-limited / window-blocked send does NOT write a terminal row and DOES reschedule `next_send_at` (anti-silent-drop).
- U14 (NEW): in_flight marker written when unmetered (`OUTREACH_SEND_COST_USD=0`, billing off) — prevents free-install double-send.
- U15 (NEW): un-suppress of `unsubscribe`/`complaint` (locked) returns 403; `bounce`/`manual` removable.
- U16 (NEW): send-window enforcement honors `send_window_tz`; out-of-window blocked.
- U17 (NEW): consent/footer gate — sequence cannot go `active` without `consent_basis` + `OUTREACH_FOOTER`.

### 12.2 PG-gated integration (mock SMTP, real Postgres under `yupcha_app`)
- I1: RLS — GUC=A sees only A's rows across all four tenant tables; unset GUC → zero rows.
- I2: cross-tenant write rejected by WITH CHECK.
- I3: `handle_send` full path — reserve → debit → in_flight → mock send → terminal; ledger has one `outreach_send`.
- I4 (STRENGTHENED): **real queue retry/watchdog** — job succeeds at mock SMTP then raises before terminal commit; let `_process_job` mark it failed→retry (`queue_service.py:329-346`) and re-claim; assert mock SMTP called **zero** extra times and **no second debit** (exercises the actual requeue path, not a hand-placed in_flight row).
- I5: insufficient credits → `skipped/credits`, reservation released, no send.
- I6: lead-in-two-workspaces — same int id in A and B; each resolves correct snapshot.
- I7: data migration — seed `outreach.db`, run twice → idempotent; unresolved `lead_id` → `skipped/lead_not_found`; **migrated sends do NOT touch the ledger (no-charge invariant)**.
- I8: trigger engine e2e — `send_email` rule (flag on) enqueues `send`; run/action_result recorded.
- I9: `assert_rls_role` refuses the store under superuser — **asserted in the WORKER `handle_send` path, not only the request path**.
- I10 (NEW): AC10 regression — with flag OFF, creating a rule with `send_email`/`sequencer` returns 409 `legacy_outreach_disabled` (proves the gate survives the type-acceptance change).
- I11 (NEW): cross-producer idempotency — same `(enrollment, step)` targeted by both a trigger send and the ticker → exactly one `outreach_sends` row, one debit, one SMTP call.
- I12 (NEW): synchronous hard bounce (mock SMTP 550) → suppression(`bounce`) + enrollment `bounced`, no re-mail on next step.
- I13 (NEW): unsubscribe POST is one-click — no auth/CSRF, side-effecting, idempotent (2x POST = one suppression); forged/expired token → 4xx, no write; public endpoint rate-limited.
- I14 (NEW): autonomous ticker — active multi-step sequence advances step 2..N without manual `/execute`; `bootstrap_outreach_schedules` re-enqueues after restart.

### 12.3 Acceptance Criteria
1. Four outreach tenant tables on PG with NOT NULL `workspace_id`, composite indexes, fail-closed RLS; `yupcha_app` DML+sequence grants on all five tables; role not re-created; `outreach_schedules` non-RLS.
2. New Alembic revision chains off `a1b2c3d4e5f6`, copies the RLS recipe; downgrade drops policies only.
3. No lead resolved without a `workspace_id`; `sequence.py:344` raw `LeadDB()` gone; send uses the enrolled snapshot.
4. Every `/api/outreach` endpoint workspace-scoped; mutations role-gated.
5. SMTP creds per-workspace via `get_secret`; `update_smtp_config` writes WI-6.
6. Rate limiting per-workspace and durable (no shared process-global limiter affecting tenants); send-window enforced in `send_window_tz`.
7. Suppression/unsubscribe table exists; List-Unsubscribe + one-click POST (no CSRF, idempotent) + visible link in HTML **and** plaintext + mandatory footer on every send; pre-enroll + pre-send suppression enforced; un-suppress of unsubscribe/complaint forbidden.
8. Sends go through the `send` job (registered in both `worker.py` and `main.py`, with `JOB_TIMEOUTS["send"]`), wrapped in `workspace_scope`, routed through `PgOutreachStore` so `assert_rls_role` fires in the worker.
9. Sends at-most-once (committed in_flight, written unconditionally + unique idem, surviving the real queue retry); metered sends debit idempotently and engage caps; cross-producer same-step collapses to one send/charge.
10. `sequencer`/`send_email` accepted at rule-create and executed only when flag on; off → `legacy_outreach_disabled` (gate lives in their validation branch, proven by I10).
11. Data migration idempotent + resumable; unresolved `lead_id`s logged not mis-attributed; migrated sends never billed.
12. Cross-tenant enrollment/send impossible (RLS + app filter + snapshot); verified by I1/I2/I6/I11.
13. Autonomous ticker fires delayed steps without manual `/execute` (I14); hard bounces suppress synchronously, soft bounces via counter; header injection blocked.

### 12.4 Out of Scope
- Open/click tracking pixels + link-wrapping analytics (only `opened_at`/`replied_at` + bounce hook provided).
- Reply detection / IMAP polling (and therefore inbound `mailto:` unsubscribe — v1 ships HTTPS-only `List-Unsubscribe`).
- A/B testing, send-time optimization, warmup ramping.
- Platform-global (cross-workspace) suppression list (open question below).
- Bounce-rate circuit breaker / auto-pause on reputation spike (acknowledged deliverability risk; open question below).
- Auto-refund of debits on SMTP failure (operator reconciliation report provided instead, §8.5).
- New frontend UI (existing UI rewired separately).
- Migrating WI-6 `workspace_settings` store to PG.

---

## Changes after review
- **§6.2.1 NEW** — fully specified `handle_send` ordering (replay-check → window → suppression → rate → caps/debit → **unconditional committed in_flight** → SMTP → classify → terminal), with a crash matrix pinned to the real `queue_service.py:329-346` retry + watchdog loop. Removes all "mirrors X" hand-waving on the safety-critical path.
- **Rate-limit / window silent-drop fixed** (§6.2.1 steps 4&6, §6.3, §9.7-9.8, U13): a skip for rate/window does **not** write a terminal row (which the queue would mark `completed`); it reschedules `next_send_at` and the ticker re-enqueues.
- **Legacy-flag flip corrected** (§11): the flag check lives in the action-type validation branch — types are NOT moved into `ACTION_TYPES_V1` (which would drop the gate). I10 regression added.
- **Autonomous ticker added** (§3.5 `outreach_schedules`, §6.8, I14) reusing the `scheduled_triggers` mirror + `bootstrap_schedules`/`_enqueue_schedule_if_absent`/`_reschedule` pattern. Multi-step sequences now function.
- **Email snapshot at enroll** (§1.5, `to_email_snapshot`) — resolve-at-enroll, send-to-snapshot semantics defined.
- **RFC 8058 one-click** (§5.1, I13): POST is no-auth/no-CSRF, side-effecting, idempotent, rate-limited.
- **Unsubscribe token key fail-closed** (§5.1, §7, U5): goes through `_load_master_key`, constant-time compare.
- **Canonical email normalization** (§6.4.1, U12) with Gmail dot/plus handling, applied identically everywhere.
- **Header-injection hardening** (§6.5.1, U11): CRLF-strip subject/from_name/to_email.
- **Plaintext + footer compliance** (§6.5, §6.7): unsubscribe link in BOTH MIME parts; mandatory physical-address footer blocks sending when absent; consent/lawful-basis gate before `active`.
- **Bounce webhook auth chicken-and-egg resolved** (§6.6): platform-global secret verified before workspace lookup; sync 5xx classification + soft-bounce counter (§6.6.1).
- **Un-suppress gating** (§3.4 `locked`, §7, U15): unsubscribe/complaint cannot be removed via API.
- **Idempotency key scheme for ALL producers** + cross-producer dedup on `(enrollment, step)` (§8.1-8.2, I11); unconditional in_flight on free installs (§8.3, U14); migrated sends never billed (§8.4, I7); reconciliation report for metered failures (§8.5).
- **assert_rls_role in the worker path** (§7, I9 strengthened) — `handle_send` routes through `PgOutreachStore`.
- **Sequence-existence validation at enroll** (§7) to prevent orphan enrollments.
- Tests U11-U17, I10-I14 added; I4 strengthened to exercise the real queue requeue.

---

## Open questions for the owner

1. Platform-global suppression: should an unsubscribe/complaint in workspace A also suppress the recipient platform-wide (across all tenants)? The spec keeps suppression per-workspace (each tenant is an independent controller/sender) and marks a global list out of scope, but on shared sending infrastructure (shared IP/domain) a bad recipient in one workspace harms everyone's reputation. Owner must decide whether a global suppression layer is required for the cloud sending setup.
2. Bounce-rate / spam-complaint circuit breaker: nothing auto-pauses a sequence when bounce or complaint rate spikes. On shared Gmail/Yahoo-facing infrastructure this is a real deliverability landmine. Is an auto-pause threshold (e.g. pause sequence at >5% bounce or >0.3% complaint) in scope for v1, or deferred? If deferred, the owner accepts the reputation risk explicitly.
3. Metered-send failure economics: refund-on-SMTP-failure is out of scope (matching the engine), with only an operator reconciliation report. For a paid email product, charging for emails that hard-fail at SMTP is a trust/billing issue. Does the owner want automatic credit-back for status=failed metered sends in v1, or is the manual reconciliation runbook acceptable?
4. Shared sending domain / SPF-DKIM-DMARC: the spec assumes per-workspace SMTP creds (BYO mailbox). For cloud where Yupcha sends on the customer's behalf via a shared domain, who owns DKIM signing and DMARC alignment, and is per-workspace custom domain/return-path verification needed before send? This affects deliverability and List-Unsubscribe From-alignment but is outside the current SMTP-secret model.
5. OUTREACH_TICK_INTERVAL and the single-flight enqueue volume: at default 15 min and large active enrollment sets, the ticker could enqueue many send jobs per tick against a sequential worker. Does the owner want a per-tick enqueue cap / batching, or rely on daily_limit + per-hour cap to bound throughput?
