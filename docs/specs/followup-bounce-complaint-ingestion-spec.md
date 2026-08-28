<!-- Auto-generated follow-up mini-spec (2026-06-26). Effort: large. -->

# Mini-Spec: Async bounce/complaint feedback-loop ingestion (BYO-SMTP DSN/FBL)

## Goal
Close the async-feedback gap in Outreach v1. Today the circuit breaker only sees **synchronous** SMTP hard-failures classified inline at send time (`apps/api/services/outreach/sending.py:296` `_is_hard_bounce`, `:298-302`). Async DSNs (RFC3464 delivery-status notifications) and FBL/ARF complaints arrive **minutes-to-days later as emails to the BYO-SMTP from-mailbox** and are never ingested. Build a tenant-scoped IMAP-poll ingestion job that fetches DSN/complaint mail from each workspace's mailbox, parses it, maps it back to the originating `OutreachSend`, and feeds the **existing** suppression + circuit-breaker primitives — idempotently, on the existing durable-queue/ticker pattern.

## Current state (file:line)
- **Sync-only bounce path**: `sending.py:294-310` — on SMTP failure, `_is_hard_bounce` (crude 5xx/text match, `:338-347`) → `add_suppression` + `record_bounce_and_maybe_pause`. Transient → re-raise for queue retry. No async path.
- **Existing webhook path (ESP-style)**: `apps/api/routers/outreach.py:379-434` `bounce_webhook` — platform-secret auth, **cross-tenant** `OutreachSend.message_id` lookup on an owner connection (`:400-408`), then per-workspace suppress + breaker. **Two defects to inherit-and-fix**: (a) it never marks the `OutreachSend` row `status="bounced"`/`bounced_at` (only the enrollment); (b) it is **not idempotent** — replaying the same `message_id` double-increments `bounce_count`/`complaint_count` (`store.py:481-483`) and can falsely trip the breaker.
- **CRITICAL PREREQUISITE — Message-ID is never set at send time.** `build_message` (`sending.py:41-81`) and `send_email` (`sender.py:162-274`) never add a `Message-ID` header; `message_id = msg.get("Message-ID", "")` (`sender.py:245`) therefore returns `""` for **every** send. `aiosmtplib`/`smtplib` do not inject one. So the stored `OutreachSend.message_id` is empty for all rows, and **both** the existing webhook and this new IMAP path have nothing to match on. This must be fixed first (see file-by-file #1).
- **Circuit breaker**: `store.py:467-504` `record_bounce_and_maybe_pause` — per-sequence counts, rate vs `OUTREACH_BOUNCE_PAUSE_RATE`/`OUTREACH_COMPLAINT_PAUSE_RATE` past `OUTREACH_CIRCUIT_MIN_SENDS` (config `:125-129`).
- **Suppression**: `store.py:386-423` `is_suppressed`/`add_suppression`; soft-bounce counter `store.py:293-299` + `OUTREACH_SOFT_BOUNCE_MAX` (`config.py:122`).
- **WI-6 per-workspace creds**: `apps/api/services/workspace/secrets.py` `get_secret(ws,key,default)` (encrypted, falls back to global). SMTP creds resolved per-ws in `sender.py:38-61` `get_smtp_config`.
- **Recurring/self-scheduling pattern to mirror**: intent poller `apps/api/services/poller/engine.py` — non-RLS mirror, advisory-lock single-flight enqueue (`:68-100`), `fire_key` (`:113-114`), self-reschedule (`:257-286`), `handle_watch_poll` (`:317`), cold-start bootstrap registered in `main.py:184-190`. Outreach's own ticker mirror/bootstrap: `sending.py:478-521`, `OutreachSchedule` (non-RLS) `orm_models.py:144-161`.
- **Handler registration**: `worker.py:58-83`, `main.py:128-148`; cold-start bootstraps `main.py:168-190`.
- **Migration head**: `c7d8e9f0a1b2` (`migrations/versions/c7d8e9f0a1b2_intent_poller.py`). RLS migration template to copy: `migrations/versions/b2c3d4e5f6a7_outreach_rls.py:170-192` (GRANT + ENABLE/FORCE RLS + `app.workspace_id` policy).
- **Test harness to mirror**: `tests/test_outreach_send.py:32-84` (SQLite in-memory, `SessionLocal` monkeypatched across modules, SMTP always mocked).

## Design
**Decision: IMAP-poll, default OFF, parse-only common formats in v1.** BYO-SMTP means no shared ESP, so async bounces return as DSN emails to the workspace's own mailbox; the ESP webhook (`bounce_webhook`) stays for users whose provider supports it. IMAP-poll is the BYO path.

Flow per workspace, per tick:
1. Self-scheduling `outreach_inbound_poll` job (one per workspace) on a non-RLS mirror, bootstrapped at cold-start exactly like the poller/outreach ticker.
2. `handle_inbound_poll` enters `workspace_scope(ws)`, resolves IMAP config via WI-6, connects (IMAP4_SSL), selects INBOX, fetches up to `OUTREACH_INBOUND_MAX_FETCH` **unprocessed** messages (search `UNSEEN` constrained by a server-side date floor, e.g. `SINCE` last-poll-1d; track by UID).
3. For each message: skip if already in the inbound ledger (idempotency). Parse via `dsn_parse.parse_message(raw)` → `list[BounceRecord]` with `kind ∈ {hard, soft, complaint, unknown}`, `original_message_id`, `recipient`, `diagnostic`.
4. Map back to the send: **within the polled workspace only**, `store.get_send_by_message_id(original_message_id)`; fallback to most-recent non-terminal send to `recipient`. No cross-tenant lookup needed (the mailbox belongs to this workspace) — strictly better tenancy than the webhook.
5. Apply via a single shared `store.apply_bounce(...)` (new; webhook refactored to call it too): mark `OutreachSend` bounced (`status="bounced"`, `bounced_at`), suppress, advance enrollment, feed breaker — **but only once per inbound message** (ledger insert is the idempotency gate, see below).
6. Record the inbound message in the RLS ledger, mark it processed on the server (`\Seen` flag and/or `MOVE`/`COPY+\Deleted` to a `Yupcha/Processed` folder — see Decision 4b), self-reschedule, persist mirror.

Parsing modules (`dsn_parse.py`), all pure functions, no I/O:
- **RFC3464** `multipart/report; report-type=delivery-status`: read the `message/delivery-status` part for `Action: failed/delayed`, `Status: 5.x.x`(hard)/`4.x.x`(soft), `Final-Recipient`/`Original-Recipient`; recover `original_message_id` from the `message/rfc822` or `text/rfc822-headers` third part.
- **RFC5965 ARF** `multipart/report; report-type=feedback-report` → `kind=complaint`; original Message-ID from the embedded message.
- **Heuristic fallback** for providers that send plain-text bounces (no MIME report): a small, well-commented regex set over subject/body (`mailbox unavailable`, `user unknown`, `550 5.1.1`, etc., reusing the `_is_hard_bounce` token list). Conservative — `unknown` when unsure (no breaker action, just ledger + `\Seen`).

## File-by-file changes
1. **`apps/api/services/outreach/sender.py`** — *(prerequisite)* In `build_message`/`send_email`, generate a stable `Message-ID` via `email.utils.make_msgid(domain=<from-domain>)`, set it on the MIME message, and return it from `SendResult.message_id` (replace the `msg.get("Message-ID","")` no-op at `:245`). Add `get_imap_config(ws)` mirroring `get_smtp_config` (`:38-61`): keys `IMAP_HOST`, `IMAP_PORT` (default 993), `IMAP_USER` (default = `SMTP_EMAIL`), `IMAP_PASSWORD` (default = `SMTP_PASSWORD`), `IMAP_FOLDER` (default `INBOX`), `IMAP_USE_SSL` (default 1), all via `get_secret`. Add `is_imap_configured(ws)`.
2. **`apps/api/services/outreach/sending.py`** — set Message-ID before SMTP if generation lives here instead of sender; ensure `_finalize_send(status="sent", message_id=...)` persists the real id (already wired `:288`).
3. **`apps/api/services/outreach/dsn_parse.py`** *(new)* — `parse_message(raw_bytes) -> list[BounceRecord]`; RFC3464 + ARF + heuristic parsers; `@dataclass BounceRecord`. Pure/std-lib `email` only.
4. **`apps/api/services/outreach/inbound.py`** *(new)* — `handle_inbound_poll(job_id, payload)`, `bootstrap_inbound_schedules()`, `tick_inbound(ws)`, `_enqueue_inbound_if_absent` (advisory-lock single-flight per `poller/engine.py:68-100`), `inbound_fire_key(ws, next_at)`. IMAP client wrapped behind a tiny `ImapClient` protocol so tests inject a fake. Master-switch guard `AUTOMATIONS_ENABLED` + `OUTREACH_INBOUND_POLL_ENABLED` + `is_imap_configured(ws)`.
5. **`apps/api/services/outreach/orm_models.py`** *(new tables)*:
   - `OutreachInboundMessage` (**RLS**): `id`, `workspace_id` NOT NULL, `imap_uid` (str), `uidvalidity` (str), `source_message_id` (the DSN's own Message-ID), `matched_send_id` (nullable), `kind`, `processed_at`, `created_at`. Unique `(workspace_id, uidvalidity, imap_uid)` **and** unique `(workspace_id, source_message_id)` — the idempotency gates. Composite index `(workspace_id, ...)`.
   - `OutreachInboundSchedule` (**non-RLS** mirror, like `OutreachSchedule:144-161`): `workspace_id` PK, `next_poll_at`, `enabled`, `consecutive_failures`, `uidvalidity`, `last_uid`, `updated_at`.
6. **`apps/api/services/outreach/store.py`** — add: `get_send_by_message_id(mid)` (ws-scoped), `get_recent_send_to(email)` (fallback match, ws-scoped, non-terminal preferred), `mark_send_bounced(send_id, error)` (sets `status="bounced"`+`bounced_at`), `record_inbound_processed(...)` (idempotent insert; returns False on conflict = already processed), inbound mirror `upsert_inbound_schedule`/`disable_inbound_schedule`, and a consolidated **`apply_bounce(send, kind, diagnostic, source)`** that performs mark-bounced + suppress + advance-enrollment + `record_bounce_and_maybe_pause`, reusing `add_suppression`/`increment_soft_bounce`/`record_bounce_and_maybe_pause`.
7. **`apps/api/routers/outreach.py`** — refactor `bounce_webhook` (`:379-434`) to (a) also mark the send row bounced and (b) route through `apply_bounce` + an idempotency guard (ledger keyed on `event.message_id`), fixing the two existing defects.
8. **`apps/api/worker.py` (`:58-83`) + `apps/api/main.py` (`:128-148`, `:178-190`)** — `register_handler("outreach_inbound_poll", handle_inbound_poll)` and add `bootstrap_inbound_schedules()` to cold-start (guarded try/except like the others).
9. **`apps/api/core/config.py`** — add `OUTREACH_INBOUND_POLL_ENABLED: bool = False`, `OUTREACH_INBOUND_POLL_INTERVAL: int = 900`, `OUTREACH_INBOUND_MAX_FETCH: int = 100`, `OUTREACH_INBOUND_MAX_CONSECUTIVE_FAILURES: int = 10`, `OUTREACH_INBOUND_LOOKBACK_DAYS: int = 3`.
10. **`migrations/versions/<new>_outreach_inbound.py`** *(new, `down_revision="c7d8e9f0a1b2"`)* — create both tables; for PG, GRANT DML to app role + ENABLE/FORCE RLS + `app.workspace_id` policy **only on `outreach_inbound_messages`** (the schedule mirror is non-RLS, read GUC-less at bootstrap), copying `b2c3d4e5f6a7_outreach_rls.py:170-192`. `create_all` covers SQLite/tests.
11. **Tests** — see Test plan.

## Tenancy / security
- IMAP creds are **per-workspace via WI-6** (`get_secret`); no shared mailbox. Fallback to global only for single-tenant installs (consistent with `get_smtp_config`).
- Mapping is done **inside `workspace_scope(ws)`** scoped to that workspace's sends — no cross-tenant `message_id` lookup (unlike the webhook). A DSN that references a send not found in this workspace is ledgered as `unknown`/unmatched and `\Seen`; it can never touch another tenant's data (RLS belt-and-suspenders + app-layer `workspace_id` filter).
- `outreach_inbound_messages` is RLS (FORCE) with the standard policy; `outreach_inbound_schedules` is non-RLS by design (cold-start reads it without a GUC, mirroring `OutreachSchedule`).
- Decrypted IMAP password lives only in process memory; never logged. Bounce diagnostics may contain PII (recipient address, server text) — store the recipient + a bounded diagnostic, never the full raw message body.
- Spoofed DSNs: a malicious actor could email a fake DSN to the mailbox to suppress/pause a competitor's sequence. Mitigation v1: only act when the `original_message_id` **matches one of our own sent `OutreachSend.message_id` values** (which are server-generated, high-entropy via `make_msgid`); unmatched/`recipient-only` fallback matches are recorded but gated behind a config flag (`apply on recipient-fallback` default OFF) so v1 is hard-match-only for breaker actions.

## Idempotency
- **Ledger is the gate**: `record_inbound_processed` does an idempotent insert on `(workspace_id, uidvalidity, imap_uid)` (server identity) with a secondary unique on `(workspace_id, source_message_id)` (survives mailbox UID churn). `apply_bounce` runs **only when the insert is newly created**, so a re-fetched message never double-increments the breaker counts.
- IMAP `UIDVALIDITY` is persisted on the mirror; if it changes, treat all prior UIDs as stale (re-key on `source_message_id`, which still dedups).
- Marking `\Seen` (and/or MOVE) is best-effort **after** the ledger insert commits; if the flag write fails, the ledger still prevents reprocessing next tick.
- `apply_bounce` itself is safe to call twice on the same send (suppression insert is `ON CONFLICT DO NOTHING` `store.py:401-423`; `mark_send_bounced` is set-idempotent), but the breaker counter is not — hence the ledger gate is mandatory before the breaker call.

## Failure modes
- **IMAP auth/connection failure** → increment `consecutive_failures` on the mirror, exponential backoff reschedule (mirror poller `:266-276`); hard-disable past `OUTREACH_INBOUND_MAX_CONSECUTIVE_FAILURES` and surface in sequence stats/logs. Never raise out of the handler in a way that blocks other workspaces.
- **Malformed / non-DSN mail** (replies, autoresponders, marketing) → parser returns `[]` or `unknown`; ledger + `\Seen`, no breaker action. (Reply detection is out of scope.)
- **Unmatched DSN** (no `message_id`, or send not in workspace) → ledgered `unmatched`, `\Seen`, no action.
- **Large mailbox / slow IMAP** → bounded by `OUTREACH_INBOUND_MAX_FETCH` per tick + server-side `SINCE` floor; remainder picked up next tick.
- **Soft bounce (4.x.x)** → `increment_soft_bounce` + suppress at `OUTREACH_SOFT_BOUNCE_MAX` (matches webhook `:428-433`); does not immediately suppress.
- **Worker crash mid-tick** → durable queue retries the job; ledger makes re-fetch a no-op for already-applied messages.
- **Clock/timezone** — use UTC (`_utcnow`) throughout; IMAP `SINCE` is date-granular, so the lookback floor must be ≥1 day to avoid edge misses.

## Feature-flag / rollout
- Master: `AUTOMATIONS_ENABLED` (existing). Feature: `OUTREACH_INBOUND_POLL_ENABLED` (**default False**). Per-workspace activation requires WI-6 IMAP creds present (`is_imap_configured`). Bootstrap is a no-op when either is off, so merge is dark by default.
- Ship the **Message-ID fix (#1) independently first** (it also repairs the existing webhook) — low-risk, no flag.
- Rollout: enable flag for a pilot workspace with IMAP creds, confirm ledger fills + breaker behaves, then document the per-workspace IMAP setup.

## Test plan (SQLite default; PG-gated where noted)
Mirror `tests/test_outreach_send.py` fixtures (in-memory SQLite, monkeypatched `SessionLocal`, fake IMAP client). Add `tests/test_outreach_inbound.py`:
1. **dsn_parse unit** — golden RFC3464 hard (5.1.1), soft (4.x.x), and ARF complaint fixtures → correct `kind` + extracted `original_message_id` + recipient; plain-text heuristic bounce; non-DSN → `[]`.
2. **End-to-end (fake IMAP)** — seed a `sent` `OutreachSend` with a known `message_id`; fake IMAP returns a matching DSN → assert send `status="bounced"`+`bounced_at`, suppression added, enrollment advanced, `bounce_count` incremented, breaker trips past threshold.
3. **Idempotency** — run the poll twice on the same message → breaker count incremented exactly once; ledger has one row; `\Seen` called.
4. **Unmatched / spoof** — DSN with unknown `message_id` → ledgered, no suppression, no breaker change.
5. **Soft-bounce accumulation** → suppress only at `OUTREACH_SOFT_BOUNCE_MAX`.
6. **IMAP failure** → `consecutive_failures` increments + backoff reschedule; hard-disable at cap.
7. **Message-ID prerequisite** — `send_email`/`build_message` now emit a non-empty `Message-ID` and persist it (regression-guards the empty-string bug).
8. **Webhook refactor regression** — existing `bounce_webhook` still works and now marks the send row + is idempotent.
9. **PG-gated RLS** (throwaway DB, never `yupcha`) — extend `tests/test_outreach_rls.py` style: confirm `outreach_inbound_messages` enforces `app.workspace_id` isolation (cross-tenant read returns 0 rows; insert with wrong GUC rejected) and that `outreach_inbound_schedules` is readable GUC-less at bootstrap.

Run: `PYTHONPATH=. uv run --group dev python -m pytest tests/test_outreach_inbound.py tests/test_outreach_send.py`.

## Acceptance criteria
1. Every successful send persists a non-empty server-generated `OutreachSend.message_id`.
2. With `OUTREACH_INBOUND_POLL_ENABLED=False` (default), no inbound job is bootstrapped/enqueued and behavior is unchanged.
3. When enabled + per-workspace IMAP creds present, a self-rescheduling `outreach_inbound_poll` job runs per workspace on the existing durable-queue/ticker pattern and survives restart via the non-RLS mirror.
4. A matching RFC3464 hard bounce marks the send `bounced` (+`bounced_at`), adds a `bounce`-reason suppression, advances the enrollment, and increments the sequence `bounce_count`.
5. ARF/FBL complaints add a **locked** `complaint` suppression and increment `complaint_count`; breaker trips per existing thresholds.
6. Soft bounces increment the per-enrollment counter and suppress only at `OUTREACH_SOFT_BOUNCE_MAX`.
7. Reprocessing the same inbound message (re-fetch, retry, restart) applies breaker/suppression effects **exactly once** (ledger-gated).
8. Mapping and all writes occur within `workspace_scope`; a DSN whose `message_id` is not a send in that workspace causes no cross-tenant effect and is ledgered as unmatched.
9. IMAP auth/connection failures back off and hard-disable past the cap without blocking other workspaces or the worker.
10. `outreach_inbound_messages` enforces RLS on PG; the schedule mirror is non-RLS and readable at GUC-less bootstrap.
11. New migration applies cleanly on head `c7d8e9f0a1b2` and downgrades.
12. The existing `bounce_webhook` is refactored to share `apply_bounce`, now marks the send row, and is idempotent.

## Out of scope
- Reply/auto-responder detection and reply-based enrollment pausing.
- OAuth (XOAUTH2) IMAP auth — v1 is password/app-password via WI-6 (note Gmail requires an app password).
- A settings UI for IMAP creds (creds set via existing secrets mechanism).
- Provider-native webhook expansion beyond the existing endpoint.
- Full RFC 5322 parsing; v1 is the report-type DSN/ARF + a conservative heuristic set.
