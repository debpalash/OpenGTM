<!-- Auto-generated design spec (2026-06-25). Review verdict: needs-revisions. -->

＃ Production Spec — Scheduled Intent-Signal Poller (v2, post-review)

## Changes after review (read first)

Every adversarial-review item was folded in. Verified against source before changing:

- **Funding key unbuildable / missed-window (BLOCKING).** Confirmed: `SecEdgarProvider.enrich()` (`sec_edgar.py:113-139`) returns only parsed Form D *fields* and drops the accession; `_latest_form_d_accession` (`:192-205`) returns only the single newest "D". → **New provider method `list_form_d_since(company_or_cik, since_accession) -> list[FormDFiling]`** returning `[{cik, accession, filing_date, fields}]` for *all* "D"/"D/A" accessions newer than the cursor (not just latest). Spec no longer claims "no change to providers"; §3.4 + §5 now list the provider edit and its test.
- **`executive_hired` source.** Confirmed JobSpy/`analyze_job_text` extract NO person names. But `sec_edgar._extract_related_persons` (`:299-329`) DOES return named officers/directors with titles. → **`executive_hired` is re-scoped to the Form-D related-persons path only** (new C-level/VP related person appearing on a new filing), with its own detector, dedup key, and AC (AC-16). The "job posting/news names a hire" path is **cut from v1** (no NER source) and listed in Out-of-scope.
- **Single-flight race (BLOCKING).** Confirmed `_enqueue_schedule_if_absent` (`engine.py:408-441`) does an unlocked Python-filtered read then insert; `Job` has no `fire_key` column/uniqueness (`models.py:30-52`). → **DB-level guard**: (a) add nullable `fire_key` column + **partial unique index** `uq_jobs_fire_key_active` on `jobs(fire_key) WHERE status IN ('pending','processing')`; (b) wrap enqueue in `pg_advisory_xact_lock(hashtext('watch_poll:'||fire_key))` then insert, catching `IntegrityError` as "already enqueued". SQLite path keeps best-effort read-then-insert (single-process). §7/§8/§3.3.
- **RSS SSRF/TOCTOU (BLOCKING).** Confirmed safe webhook path connects to the **pinned IP** with `follow_redirects=False` (`actions.py:148-181`). → `poller/rss.py` **must reuse `_validate_and_pin` + the exact `_send_webhook_pinned` connect pattern** (pin-to-IP GET, Host/SNI preserved, `follow_redirects=False`, per-hop re-validation if redirects are ever enabled — they are not). No second hostname resolution. Spec extracts a shared `pinned_get(url)` helper so RSS and webhooks share one audited sender.
- **Missed intermediate funding accessions.** Funding diff now enumerates all accessions newer than `cursor.sec_last_accession` (newest-first array, stop at cursor), emits one signal per accession, advances cursor to the newest only after all emitted.
- **JobSpy `max_jobs` cap breaks surge magnitude.** Confirmed `total_jobs = len(scraped DDG snippets)`, capped at `max_jobs*2≈10` (`jobspy_signals.py:136,230`) — it is a *search-hit* count, not a real open-role count. → **Magnitude-based surge is downgraded to a coarse `growth_signal` *band transition*** (`none→hiring→growing→rapid_growth→hypergrowth`, `jobspy_signals.py:82-93`). `hiring_surge` fires only on an **upward band crossing** vs the cursor band, not on a raw count delta. A configurable `INTENT_POLLER_JOBSPY_MAX_JOBS` lets ops raise the cap (cost trade-off documented). The unreliable raw count is never used as the dedup or threshold input.
- **`hiring_surge` period-bucket suppression.** Made explicit + intentional: at most one `hiring_surge` per lead per ISO week, AND only on an upward band crossing. Cursor stores `{band, band_week}`; re-cross within the same week is suppressed by design (documented in §8/§9).
- **`new_tech_adopted` intent-locking.** Confirmed `tech_adoption_signal[]` carries an `intent` level. → dedup key now includes a coarse intent tier: `f"{lead_id}:{tech_normalized}:{intent_tier}"` so an upgrade (e.g. `mentions`→`adopting`) emits once more; same tier never re-emits. Removal/re-add still out of scope (documented).
- **Bootstrap half-write / cursor atomicity.** Cursor write + signal emission now commit in **one transaction per source** (emit signals → advance that source's cursor sub-key → commit together). A crash before commit rolls both back; the next poll re-detects the same items and the deterministic `signals.id` makes re-emit a no-op, so a half-write can never silently drop the first real batch. Added explicit `cursor.bootstrapped: bool` flag (not "cursor exists") to distinguish bootstrap from incremental — set in the same commit.
- **Backoff formula.** Defined: `next_poll_at = compute_next_run(anchor, interval)` then, if `consecutive_failures > 0`, `next_poll_at = max(that, now + min(BASE * 2**(failures-1), CAP))` with `BASE=interval/4`, `CAP=24h`, and a hard disable after `INTENT_POLLER_MAX_CONSECUTIVE_FAILURES` (default 12) → set `enabled=False`, `last_error="auto_disabled"`, mirror disabled.
- **Target normalization frozen.** `target_normalized` rules specified and versioned (§8.1). CIK is canonical: if a CIK is ever known for a watch, the funding key uses `cik` (not the name) so editing the display name never changes the key.
- **`lead_id` in dedup key is fragile.** Hiring/tech keys now key on the **stable company identity** (`cik` when known, else `target_normalized`), NOT the mutable `lead_id`. `lead_id` is still written on the row for routing but is not part of `signals.id`.
- **Within-tenant lead ambiguity.** Resolver behavior specified: deterministic pick (exact normalized-name match → lowest `lead_id`); on >1 fuzzy match, **skip emission + record `last_error="ambiguous_lead"`** rather than mis-route an email. Pinned `watch.lead_id` always wins.
- **Mirror workspace_id verbatim + global-unique watch_id.** `watch_id` is a server-side `uuid4` (globally unique, never per-tenant). `_mirror_upsert` copies `watch.workspace_id` verbatim; AC added.
- **Two webhook paths collapsed to ONE.** `watch.webhook_url`/`watch.webhook_secret_ref` are **removed**. Delivery is **rules-only** (`on_signal`→`webhook`), so all delivery goes through the audited `_act_webhook`/`_send_webhook_pinned`/`in_flight` machinery. No second sender, no unguarded SSRF, no double-delivery. (Convenience: the create endpoint can *optionally auto-create* an `on_signal`→`webhook` rule, but the poller itself never sends.)
- **All tenant writes under scope + safe role.** Every `watch_subscriptions` write (cursor/last_error/next_poll_at) runs inside `workspace_scope` on a non-superuser session (`assert_rls_role`); AC added for fail-closed on unset GUC.
- **Billing model made coherent.** "Bill once per event" was incoherent (event id unknown pre-fetch). → **Cost is per-fetch-attempt**, `run_id = poll fire_key` (idempotent across job retries of the *same* poll), and **re-poll cost is accepted as the steady-state cost** — bounded by conditional-GET short-circuits (below). Free sources stay `*_COST_USD=0` no-op.
- **Conditional GET / steady-state fetch cost.** Added ETag/If-Modified-Since for RSS and `Last-Modified`/submissions-`accessionNumber[0]`-equality short-circuit for SEC, stored in cursor, so no-op polls cost a cheap 304 / early return. This is the dominant runaway-cost control.
- **Daily budget made atomic.** Replaced TOCTOU counter with a per-(ws, UTC-day) row in a tiny `poll_budget_ledger` upserted via `INSERT ... ON CONFLICT DO UPDATE SET n=n+1 RETURNING n` under the scoped session; over-budget → early-exit + reschedule. Manual poll-now **counts against the budget** (closes the bypass) and is additionally rate-limited per watch.
- **Manual poll-now no longer bypasses single-flight.** Poll-now uses the **same scheduled `fire_key`** when a poll is already due/in-flight (returns 202 "already queued"); a forced "poll now" uses a manual key but still takes the advisory lock + unique index, so it can't run concurrently with the scheduled job for the same watch.
- **`feed_seen_guids` trim correctness.** `max_published_epoch` is the **primary** watermark; `seen_guids` is a tie-breaker bounded list sized `N = max(200, 5×max_entries_per_poll)` and only ever trims GUIDs strictly older than `max_published_epoch`, so a trimmed GUID can never re-emit.
- **emit fan-out bound.** Added `INTENT_POLLER_MAX_FIRES_PER_SIGNAL` advisory + a load-test acceptance gate (AC-18) for the txn-hold concern.
- **Tests.** Added AC-16 (executive_hired from Form D + negative), AC-17 (single-flight concurrency / unique-index), AC-19 (missed intermediate accessions), AC-20 (poll-now no double-charge vs scheduled), AC-21 (RSS SSRF + redirect-to-private + pinned connect), AC-22 (budget atomicity + poll-now counts), AC-23 (tenant-table writes fail-closed on unset GUC), AC-24 (cursor half-write → no dropped first batch), AC-25 (real provider returns accession — exercises the contract, not just a mock). AC-6 rewritten to assert band-crossing semantics against the cap.

---

## 0. TL;DR / contract

A durable job type `watch_poll` periodically pulls fresh buying signals — **funding** (SEC EDGAR Form D), **hiring band-shift + tech-adoption** (job postings), **executive appointments** (Form D related persons), and **RSS/news** — for the companies/leads each workspace tracks, and writes **structured, timestamped, deduped, workspace-stamped** rows into the existing PG `signals` table **via `PgLeadStore.add_signal`** (never the legacy `services/signals/monitor.py` global SQLite path). Because `add_signal` fires `automations.events.emit_signal_matches` on first insert with `fire_key="signal:<pk>"` (`store.py:412-446`), poller-written signals automatically drive the trigger engine (detect→enrich→sequence→send). Webhooks are delivered **only** via `on_signal`→`webhook` rules, so SSRF-pinning + secret refs + once-per-`fire_key` semantics are reused, not reinvented.

Mirrors existing patterns: the trigger-engine scheduler/single-flight/mirror (`engine.py:394/408/497`), the durable queue (`queue_service.py`), the RLS migration recipe (`b2c3d4e5f6a7_outreach_rls.py`), per-workspace secrets (`secrets.py:210`), billing spend caps (`billing/service.py:174`), and the DNS-pinned sender (`actions.py:96/148`).

Default **OFF**, **PG-only**, guarded by `INTENT_POLLER_ENABLED`.

---

## 1. Goal & user-facing behavior

A workspace tracks companies/leads via **watch subscriptions** (per company/domain/CIK/feed). The poller runs them on a per-workspace cadence and emits exactly-once signals.

**Concrete examples**
1. **Funding**: `ws_42` watches `kind=funding, target="Acme Robotics"` (or `sec_cik:0001234567`). Poller resolves CIK once and caches it on the watch, calls **`list_form_d_since(cik, cursor.sec_last_accession)`** → all new "D"/"D/A" accessions newer than the cursor. For each, matches company→lead 771, writes a `SignalRow` with `id = sha256("ws_42|funding|<cik>|company_funded|<cik>:<accession>")`, `signal_type="company_funded"`, `created_at=time.time()`. `add_signal` inserts once → `emit_signal_matches` fires → `trigger_eval` → detect→enrich→sequence→send, and an `on_signal`→`webhook` rule POSTs `{type:"company_funded", company, amount, source_url}`. **Re-poll: same accessions ≤ cursor → no fetch beyond the cheap submissions check, no row, no fire, no charge.** If two filings landed since last poll, **both** emit (no missed-window).
2. **Hiring band-shift / new tech**: `kind=hiring`. `JobSpySignalProvider` + `analyze_job_text` return `growth_signal` band + `tech_adoption_signal[{tech,category,intent}]`. The handler compares the band against the cursor's stored band; an **upward band crossing** (e.g. `growing→rapid_growth`) emits `signal_type="hiring_surge"` once per lead per ISO week. A newly-seen tech (or an intent-tier upgrade) emits `new_tech_adopted`. Raw counts are not used as thresholds (DDG-snippet artifact).
3. **Executive appointment**: from a **new Form D**, a related person with a C-level/VP title not previously seen for this company emits `signal_type="executive_hired"`, keyed `f"{cik}:{exec_name_normalized}:{role_tier}"`.
4. **RSS/news**: `kind=feed, target="https://acme.com/blog/rss"`. Each new entry (keyed by GUID/link, bounded by `max_published_epoch`) becomes a `signal_type="news"` row. Conditional GET (ETag/If-Modified-Since) makes no-op polls a cheap 304.
5. **Manual "poll now"**: an operator clicks "Poll now" → API enqueues a one-shot `watch_poll` that respects single-flight + budget.

---

## 2. Architecture & data flow

```
                          startup (main.py)
                                │ bootstrap_watch_schedules()  (reads NON-RLS mirror, no GUC)
                                ▼
  ┌─────────────────────────────────────────────────────────────────┐
  │  durable queue (jobs, FOR UPDATE SKIP LOCKED) + uq_jobs_fire_key  │
  └─────────────────────────────────────────────────────────────────┘
        ▲ self-re-enqueue (advisory-lock + unique-index single-flight)
        │ _enqueue_poll_if_absent
        │                              handle_watch_poll(job_id, payload)
        │                              with workspace_scope(ws): SessionLocal() [assert_rls_role]
        │                                load watch (RLS); early-exit if gone/disabled
        │                                budget upsert (ON CONFLICT n=n+1) → over? reschedule+exit
        │                                per source, in its OWN txn:
        │                                  [BILLING] check_and_debit(run_id=poll fire_key) BEFORE paid fetch
        │                                  conditional-GET / cursor short-circuit (304 / acc==cursor → skip)
        │                                  fetch ── SEC list_form_d_since ─┐
        │                                  fetch ── JobSpy band+tech ──────┤ per-source
        │                                  fetch ── RSS (pinned-IP GET) ───┘
        │                                  match company→lead (stable id, RLS)
        │                                  diff vs cursor sub-key
        │                                  for each NEW event:
        │                                    Signal(id=deterministic, lead_id, type, created_at=epoch)
        │                                    store.add_signal(signal) ────────────────┐
        │                                  advance THIS source's cursor sub-key       │
        └─────────────────────────────── set next_poll_at(+backoff); mirror; COMMIT  │
                                                                                      ▼
                                          PgLeadStore.add_signal (RLS-scoped txn)
                                            s.get(SignalRow,id) → insert once + stamp workspace_id
                                            inserted? → emit_signal_matches(fire_key="signal:<pk>")
                                                              │
                                                              ▼
                                          on_signal rules → trigger_eval → actions
                                            (enrich / sequence / send / WEBHOOK[SSRF-pinned])
```

A watch has one `kind`; `kind=company` fans funding+hiring+exec **per source in independent transactions** so one source's failure never blocks or corrupts another's cursor.

---

## 3. Data model

### 3.1 `watch_subscriptions` (RLS tenant table)
- `id` String(36) PK — **server-side `uuid4`** (globally unique, never client-supplied, never per-tenant).
- `workspace_id` String(64) NOT NULL
- `kind` String(20) NOT NULL — `funding` | `hiring` | `feed` | `company`
- `target` String NOT NULL — company name | domain | `sec_cik:<cik>` | feed URL
- `resolved_cik` String(10) nullable — cached canonical CIK once resolved (freezes funding/exec dedup key against name edits)
- `lead_id` Integer nullable — pinned lead match (routing only; NOT part of `signals.id`)
- `signal_types` JSON — emittable `signal_type`s (aligns with `on_signal trigger_config.signal_types`)
- `interval` String(10) — `hourly`|`daily`|`weekly` (default `daily`)
- `schedule_anchor` DateTime(tz)
- `enabled` Boolean default true
- `next_poll_at` DateTime(tz) nullable
- `cursor` JSON default `{"bootstrapped": false}` — watermark (§8): `{bootstrapped, sec_last_accession, sec_known_execs[], hiring_band, hiring_band_week, known_tech{tech:intent_tier}, feed_seen_guids[], feed_max_published, feed_etag, feed_last_modified}`
- `last_polled_at` / `last_error` String / `consecutive_failures` Integer default 0
- `created_at` / `updated_at` DateTime(tz)
- **`webhook_url` / `webhook_secret_ref` REMOVED** (delivery is rules-only — see review note).
- `__table_args__`: `UniqueConstraint("workspace_id","kind","target", name="uq_watch_ws_kind_target")`, `Index("ix_watch_ws_enabled","workspace_id","enabled")`, `Index("ix_watch_ws_kind","workspace_id","kind")`.

### 3.2 `watch_schedules` (non-RLS mirror — byte-for-byte from `outreach_schedules`, `b2c3d4e5f6a7:134-143`)
`watch_id` String(36) PK · `workspace_id` String(64) NOT NULL · `next_poll_at` DateTime(tz) · `enabled` Boolean server_default false · `updated_at` DateTime(tz) · `Index("ix_watch_sched_due","enabled","next_poll_at")`. Carries ids/ts only, no signal content.

### 3.3 `poll_budget_ledger` (RLS tenant table, tiny)
`workspace_id` String(64) · `day` Date · `n` Integer default 0 · PK `(workspace_id, day)`. Atomic increment via `INSERT ... ON CONFLICT (workspace_id, day) DO UPDATE SET n = poll_budget_ledger.n + 1 RETURNING n`. RLS policy identical to `watch_subscriptions`. (SQLite uses `INSERT ... ON CONFLICT DO UPDATE` too.)

### 3.4 `jobs` single-flight hardening (edit to existing table)
- New nullable `fire_key` String column on `jobs`.
- **Partial unique index** `uq_jobs_fire_key_active ON jobs(fire_key) WHERE status IN ('pending','processing')` (PG). On SQLite, a plain index + best-effort read (single process).
- Enqueue takes `SELECT pg_advisory_xact_lock(hashtext('watch_poll:'||:fire_key))` then inserts; an `IntegrityError` on the partial unique index is caught and treated as "already enqueued" (single-flight is now a real guarantee on PG).

### 3.5 How it writes the EXISTING `signals` table
Unchanged write path. Handler builds the `Signal` dataclass (`monitor.py:34-47`) with a **deterministic `id`** (never `uuid4` default) and `created_at` as **epoch float** (`SignalRow.created_at` is `Float`, `orm_models.py:156`), then `store.add_signal(signal)` (`store.py:412`). That force-stamps `workspace_id=self.workspace_id` (`store.py:420`), inserts once, emits within the RLS-scoped txn. `lead_id` is **required and a real matched lead id** — `emit_signal_matches` skips `lead_id is None` (`events.py:188`).

### 3.6 New provider method (the funding-key fix — NOT reuse-as-is)
`SecEdgarProvider.list_form_d_since(self, company_or_cik, since_accession: str|None) -> list[FormDFiling]`:
- Resolve CIK (reuse `_resolve_cik`, cache on watch as `resolved_cik`).
- Walk `submissions.recent` parallel arrays newest-first; collect every `form in ("D","D/A")` with `accession > since_accession` (string compare on dashless accession is monotonic by SEC issuance; stop at the cursor accession).
- For each, fetch+parse `primary_doc.xml` (reuse `_fetch_form_d`/`_parse_form_d`) **and return the accession + filing_date + related-persons**.
- Reuse `_LIMITER`, `_HEADERS`, graceful no-raise contract. The legacy `enrich()` is untouched for the enrichment waterfall; the poller calls the new method.

### 3.7 Alembic — `xxxx_intent_poller.py`, `down_revision="b2c3d4e5f6a7"`
Clone `b2c3d4e5f6a7` exactly:
- `create_table("watch_subscriptions"|"watch_schedules"|"poll_budget_ledger", ...)` + indexes + `uq_watch_ws_kind_target`.
- `op.add_column("jobs", Column("fire_key", String, nullable=True))`; `op.create_index("uq_jobs_fire_key_active", "jobs", ["fire_key"], unique=True, postgresql_where=text("status IN ('pending','processing')"))` (plain index on SQLite branch).
- `if dialect == "postgresql": _pg_upgrade()`: GRANT DML on all new tables to `yupcha_app` (`:178-179`), GRANT USAGE,SELECT ON ALL SEQUENCES (`:181`); ENABLE+FORCE RLS + isolation policy `USING/WITH CHECK (workspace_id = current_setting('app.workspace_id', true))` on **`watch_subscriptions` and `poll_budget_ledger` only**; `watch_schedules` gets DML grant, **no RLS**.
- `downgrade`: drop policies, indexes (incl. `uq_jobs_fire_key_active`), `jobs.fire_key` column, tables; do **not** drop `yupcha_app` (`:195-199`).
- No change to `signals` (already RLS, `c42d0273d9bd:181-189`).

---

## 4. API surface

New router `apps/api/routers/watches.py` under `/api/watches`, all `Depends(get_workspace_ctx)` (GUC via `current_workspace_var`, `tenancy.py:100-103`). Belt-and-suspenders `workspace_id == ctx.workspace_id` on top of RLS. Gated by `INTENT_POLLER_ENABLED` (404 off, mirroring automations router).

- `POST /api/watches` — create. Body `{kind,target,lead_id?,signal_types?,interval?,create_webhook_rule?,webhook_url?,webhook_secret_ref?}`. Validates `kind` enum; if `create_webhook_rule`, **creates an `on_signal`→`webhook` rule** via the automations service (which validates `webhook_url` via `_validate_and_pin` at create) — the watch itself stores no webhook. Compute `next_poll_at = compute_next_run(anchor, interval)`, `_enqueue_poll_if_absent` + mirror upsert. Enforces `INTENT_POLLER_MAX_WATCHES_PER_WS`.
- `GET /api/watches` (paginated) · `GET/PATCH/DELETE /api/watches/{id}` (PATCH: enable/disable, interval, signal_types; DELETE also `mirror_delete`).
- `POST /api/watches/{id}/poll` — manual poll-now: if a poll is already due/in-flight (scheduled `fire_key` present), returns 202 "already queued" (no new job). Else enqueues with a manual `fire_key` that still takes the advisory lock + unique index. **Counts against `poll_budget_ledger`** and is per-watch rate-limited.
- `GET /api/watches/{id}/signals` — proxy to `PgLeadStore.get_signals` (`store.py:451`) filtered by watch lead/signal_types.

Auth: `require_workspace_role` (`tenancy.py:134`) — writes editor/admin, reads membership.

---

## 5. File-by-file change list

**New files**
- `migrations/versions/xxxx_intent_poller.py` — §3.7.
- `apps/api/services/poller/models.py` — `WatchSubscription`, `WatchSchedule`, `PollBudgetLedger` (single `Base`).
- `apps/api/services/poller/engine.py` — `handle_watch_poll`, `_enqueue_poll_if_absent` (advisory-lock + unique-index), `_mirror_upsert/mirror_set/mirror_delete`, `bootstrap_watch_schedules`, `schedule_bootstrap_for_watch`, `_reschedule_watch` (with backoff formula), `_debit_budget`. Clone from `automations/engine.py:408/444/454/475/497`.
- `apps/api/services/poller/sources.py` — adapters returning normalized `list[DetectedEvent]` `(natural_event_id, signal_type, title, description, source, source_url, weight, occurred_at)`: `fetch_funding` → `SecEdgarProvider().list_form_d_since(...)`; `fetch_hiring` → `JobSpySignalProvider().enrich(...)` band + `analyze_job_text`; `fetch_exec` → Form-D related persons diff; `fetch_feed` → `rss.fetch_feed(...)`.
- `apps/api/services/poller/rss.py` — RSS/Atom fetcher built on a shared **`pinned_get(url)`** (extracted from `actions._send_webhook_pinned`): pin-to-IP, Host/SNI preserved, `follow_redirects=False`, per-host token-spacer, UA, timeout, conditional GET (ETag/If-Modified-Since).
- `apps/api/services/poller/keys.py` — `signal_event_id(ws, kind, target_norm_or_cik, signal_type, natural_id) -> sha256 hex`; `normalize_target(...)` (§8.1).
- `apps/api/routers/watches.py` — §4.
- `tests/test_intent_poller.py` (+ PG-gated integration) — §12.

**Edited files**
- `apps/api/services/leadgen/enrichment/providers/sec_edgar.py` — **add `list_form_d_since` + `FormDFiling`** (§3.6). enrich() unchanged.
- `apps/api/services/automations/actions.py` — extract shared `pinned_get(url, headers)` from `_send_webhook_pinned` (no behavior change to webhooks).
- `apps/api/main.py:126-143` — `queue_service.register_handler("watch_poll", handle_watch_poll)`; near `:170` `bootstrap_watch_schedules()` (try/except); mount `watches` router.
- `apps/api/worker.py:58-81` — register `"watch_poll"` handler.
- `apps/api/services/queue_service.py:32` — `JOB_TIMEOUTS["watch_poll"]=600`; enqueue path sets `Job.fire_key` and honors the advisory-lock/unique-index single-flight.
- `apps/api/models.py` — `Job.fire_key` column.
- `apps/api/core/config.py:94-109` — `INTENT_POLLER_ENABLED=False`, `INTENT_POLLER_DEFAULT_INTERVAL="daily"`, `INTENT_POLLER_MAX_WATCHES_PER_WS=200`, `INTENT_POLLER_DAILY_POLL_BUDGET=0` (0=unlimited), `INTENT_POLLER_MAX_CONSECUTIVE_FAILURES=12`, `INTENT_POLLER_JOBSPY_MAX_JOBS=5`, `INTENT_POLLER_FEED_MAX_ENTRIES=100`, `INTENT_POLLER_MAX_FIRES_PER_SIGNAL=200`, `SEC_EDGAR_USER_AGENT` (if absent), `POLLER_FUNDING_COST_USD=0.0`, `POLLER_HIRING_COST_USD=0.0`.
- (No change to `store.py`, `events.py`, `billing/service.py`, `url_guard.py`.)

---

## 6. Tenancy / security

- **RLS** on `watch_subscriptions` + `poll_budget_ledger` (ENABLE+FORCE, `current_setting('app.workspace_id', true)` → NULL fail-closed). `watch_schedules` non-RLS mirror (ids/ts only).
- **All tenant writes under scope + safe role**: every `watch_subscriptions`/`poll_budget_ledger` write (cursor, last_error, next_poll_at, budget) runs inside the **same `workspace_scope` session** that `PgLeadStore`/`get_lead_store` re-bind `current_workspace_var` on (`store.py:158-164`), on a non-superuser role (`assert_rls_role`, `store.py:39`). RLS `WITH CHECK` rejects any mismatched/unset-GUC write (fail-closed). No tenant-table session is ever opened outside scope. **Never** `LeadDB()` or `monitor.add_signal`.
- **Mirror integrity**: `_mirror_upsert` copies `watch.workspace_id` verbatim; `watch_id` is a global `uuid4`, so a mirror row can only ever route bootstrap back to the owning tenant (AC-9b). Bootstrap reads the mirror with no GUC, then `workspace_scope(workspace_id)` before any tenant touch (clone of `engine.py:497-531`).
- **Lead matching tenant-scoped + disambiguated**: resolve `LeadRow` within the active workspace (RLS + explicit `workspace_id`, `store.py:248`). Pinned `watch.lead_id` wins. Else exact normalized-name match → lowest `lead_id`; **>1 fuzzy match → skip + `last_error="ambiguous_lead"`** (never mis-route an email). No match → skip (no row, no fire).
- **SSRF — one audited sender for everything**: webhooks AND RSS go through `pinned_get`/`_send_webhook_pinned` semantics — DNS resolved, **every** A/AAAA checked, blocked if any private (`url_guard.py:39`), pinned to validated IP, Host/SNI preserved, `follow_redirects=False`. RSS feed URL validated at create AND every fetch (no TOCTOU, no rebinding, no redirect-to-private). Optional `AUTOMATIONS_WEBHOOK_DOMAIN_ALLOWLIST`.
- **Secrets**: webhook auth via the rule's `header_secret_ref` → `get_secret(ws, ref)` (`secrets.py:210`, Fernet, fail-closed). Because there is no poller-side webhook path, no second secret/header surface exists. SEC UA from `SEC_EDGAR_USER_AGENT`. Keys never logged.

---

## 7. Cost / abuse control

- **Politeness**: SEC reuses process-wide `_RateLimiter(8/s)` (`sec_edgar.py:79-99`) + UA. RSS uses a per-host token-spacer (≤1 req/s/host) + UA + timeout in `rss.py`. JobSpy keeps `delay`/`max_jobs` (`jobspy_signals.py:136`), cap configurable via `INTENT_POLLER_JOBSPY_MAX_JOBS`.
- **Steady-state fetch cost bounded by conditional GET**: RSS sends `If-None-Match`/`If-Modified-Since` from `cursor.feed_etag/feed_last_modified` → 304 short-circuit. SEC compares `submissions.recent.accessionNumber[0]` (cheap JSON) to `cursor.sec_last_accession` → early return with no `primary_doc.xml` fetches when nothing new. This is the primary defense against the "200 watches × 24/day = mostly no-op fetches" runaway.
- **Atomic per-ws daily budget**: `poll_budget_ledger` upsert `... ON CONFLICT DO UPDATE SET n=n+1 RETURNING n` (no TOCTOU). Over `INTENT_POLLER_DAILY_POLL_BUDGET` (when >0) → early-exit + reschedule. **Manual poll-now counts** against the budget; per-watch rate-limit on poll-now.
- **Spend ceiling for metered sources**: free sources → `*_COST_USD=0` → `check_and_debit` no-op (`service.py:193`). For a paid source, call `billing.check_and_debit(db, ws, cost, run_id=<poll fire_key>, reason="poller_fetch")` **before** the fetch (`service.py:174`). `run_id = poll fire_key` → idempotent across **job retries of the same poll** (`run:<run_id>`); re-poll of a later cadence is a new poll and is **charged again** — this is the accepted, bounded steady-state cost (conditional-GET keeps it cheap; the incoherent "bill once per event" claim is dropped).
- **Single-flight is now a real guarantee** (PG): advisory-xact-lock + partial unique index on `jobs(fire_key)` → two concurrent enqueues (bootstrap vs self-re-enqueue vs poll-now across N replicas) collapse to one job → no duplicate external fetch, no duplicate debit. SQLite remains best-effort (single process).
- **Event fires ONCE**: deterministic `signals.id` + PK-idempotent `add_signal` + `fire_key="signal:<pk>"` → no duplicate signal/fire/webhook (§8).
- **Fan-out bound**: `INTENT_POLLER_MAX_FIRES_PER_SIGNAL` caps trigger_eval enqueues per signal; load-test gate AC-18.

---

## 8. Idempotency & dedup

### 8.1 Frozen target normalization (the key is permanent)
`normalize_target(target)`:
- If `target` starts `sec_cik:` → canonical `_pad_cik` 10-digit CIK.
- Else lowercase, strip legal suffixes + punctuation, collapse whitespace (reuse `sec_edgar._norm_name`), then strip scheme/`www.`/trailing slash for URLs.
- **CIK supremacy**: once `watch.resolved_cik` is set, funding/exec keys use the CIK, NOT the name — so editing the display name or later pinning the CIK never changes the key. Versioned `KEY_SCHEMA_VERSION=1` (frozen; bump only with a migration).

### 8.2 The signal-event key
```
signal.id = sha256(f"{workspace_id}|{kind}|{stable_company_id}|{signal_type}|{natural_event_id}")  # hex
```
`stable_company_id = resolved_cik or normalize_target(target)`. `natural_event_id`:
- **Funding (Form D)**: `f"{cik}:{accession}"` → `company_funded`. Enumerate ALL accessions newer than `cursor.sec_last_accession` (no missed-window).
- **Executive appointment**: `f"{cik}:{exec_name_normalized}:{role_tier}"` → `executive_hired`, from new Form D related persons not in `cursor.sec_known_execs`.
- **Hiring band-shift**: `f"{cik_or_target}:{iso_week}"` → `hiring_surge`. Emits only on an **upward band crossing** vs `cursor.hiring_band`; one per lead per week (intentional suppression of intra-week re-cross).
- **New tech adopted**: `f"{cik_or_target}:{tech_normalized}:{intent_tier}"` → `new_tech_adopted` (intent upgrade re-emits once; same tier never).
- **News/RSS**: `entry.guid or entry.link` → `news`.

`lead_id` is **not** part of the key (it's mutable); it is written on the row for routing only.

Because `add_signal` does `s.get(SignalRow, id)` + inserts only when absent (`store.py:415-433`), and `emit_signal_matches` fires `fire_key="signal:<pk>"` only on the `inserted` path (`store.py:438`, `events.py:217`), **re-polling never re-emits → never re-fires → never re-charges trigger actions** (`engine.py:119`).

### 8.3 Cursor / watermarks (atomic with emission)
`watch_subscriptions.cursor` JSON. Per source, **emit signals AND advance that source's cursor sub-key in ONE transaction** → a crash before commit rolls both back; the next poll re-detects and deterministic ids make re-emit a no-op. `cursor.bootstrapped` (explicit bool, set in the same commit) — NOT "cursor exists" — distinguishes bootstrap from incremental, so a half-write can never flip a real incremental poll into a suppressing bootstrap.
- Funding cursor: `sec_last_accession` (advance to newest only after all emitted), `sec_known_execs[]`.
- Hiring cursor: `{hiring_band, hiring_band_week}`.
- Tech cursor: `known_tech{tech: intent_tier}`.
- Feed cursor: `{feed_max_published (primary watermark), feed_seen_guids[] (N = max(200, 5×max_entries_per_poll), trims only GUIDs strictly older than feed_max_published), feed_etag, feed_last_modified}`.

### 8.4 Backfill vs incremental
First poll (`bootstrapped=false`) records current state into cursor and (configurable) suppresses emission for pre-existing items (`backfill=false` default) — bootstrap suppression is itself idempotent via deterministic ids. Subsequent polls are incremental.

---

## 9. Failure modes / edge cases (exhaustive)

1. **Source down / 5xx / timeout**: provider returns failure (sec_edgar never raises, `:137`); record `last_error`, `consecutive_failures += 1`, reschedule **with backoff** (§9.16). No cursor advance for the failed source → retried.
2. **429**: in-process limiter + per-host spacer; failure returns, cursor unchanged, retried; queue backoff `2**(n-1)` (`queue_service.py:337`) covers job-level.
3. **Partial fan-in** (company: funding ok, hiring fails): each source has its own txn → emit + advance the succeeded source's cursor sub-key, leave the failed one's untouched.
4. **Company not matchable**: pinned `lead_id` wins; else resolve by name; **>1 fuzzy → skip + `ambiguous_lead`**; none → skip + log (a `lead_id=None` signal is dropped by `emit_signal_matches`, `events.py:188`; we don't write it).
5. **Duplicate across sources** (funding round in Form D and news): different `natural_event_id`/`signal_type` → two rows, each fires its intended rule. Cross-source coalescing out of scope v1.
6. **Clock/timezone**: UTC everywhere; `compute_next_run` normalizes naive anchors (`engine.py:397`). `created_at` is **epoch float** (`time.time()`) — never an ISO string (would corrupt the `Float` column). `iso_week` for hiring buckets is UTC.
7. **Webhook retry/backoff**: rules-only delivery; `webhook` ∈ `NON_IDEMPOTENT_ACTION_TYPES` (`actions.py:40`) → at-most-once via committed `in_flight`. A failed webhook does not roll back the signal row.
8. **Watch deleted/disabled mid-poll**: handler re-loads watch at top under scope; missing/disabled → early-exit, no reschedule, `mirror_delete`. In-flight poll finishes harmlessly (deterministic/idempotent writes); next cadence not enqueued.
9. **Concurrent pollers** (N replicas): `claim_next_job` is `FOR UPDATE SKIP LOCKED` (`queue_service.py:129`) → one replica per job; advisory-lock + unique index prevent duplicate scheduled jobs; deterministic ids collapse any residual duplicate writes.
10. **Cursor corruption / schema drift**: read defaults to `{"bootstrapped": false}`; unknown keys ignored; malformed cursor → suppressed re-bootstrap, never a crash.
11. **CIK false-negative** (name→CIK miss, `:166-188`): no funding/exec that poll; user pins `target="sec_cik:<cik>"`.
12. **Huge RSS feed**: cap `INTENT_POLLER_FEED_MAX_ENTRIES` (100); `feed_max_published` primary watermark; `feed_seen_guids` bounded + trim-safe (§8.3).
13. **Billing 402** (paid source): `check_and_debit` raises `InsufficientCreditsError` (`service.py:214`) **before** the paid fetch; catch, `last_error="insufficient_credits"`, skip paid sources, still reschedule, free sources run.
14. **Automations disabled**: `emit_signal_matches` no-op (`events.py:175`); signals still written to feed (raw signal source).
15. **PG store disabled / SQLite**: `INTENT_POLLER_ENABLED` hard-requires `use_pg_store()`; otherwise bootstrap is skipped (log CRITICAL) so `on_signal` semantics hold (PG-only).
16. **Permanent failure backoff**: `next_poll_at = max(compute_next_run(anchor, interval), now + min(BASE·2**(failures-1), 24h))`, `BASE=interval/4`. After `INTENT_POLLER_MAX_CONSECUTIVE_FAILURES` (12) → `enabled=False`, `last_error="auto_disabled"`, mirror disabled (no infinite polling of a dead feed).
17. **JobSpy count is a DDG-snippet artifact**: never used as a magnitude threshold or dedup input; only the coarse `growth_signal` band drives `hiring_surge` (band crossing). Raising `INTENT_POLLER_JOBSPY_MAX_JOBS` improves band fidelity at cost.
18. **Tech intent upgrade**: dedup key includes `intent_tier`; `mentions→adopting` re-emits once. Removal/re-add not handled (out of scope).
19. **Cursor half-write**: emission + cursor advance are one txn; `bootstrapped` bool is explicit → no silent first-batch drop (AC-24).

---

## 10. Observability

- **Poll runs**: structured log per poll `{watch_id, ws, kind, target, fetched, emitted, skipped_dupe, short_circuited(304/acc-eq), budget_n, consecutive_failures, duration_ms, next_poll_at}` (mirror `bootstrap_schedules`/`refresh.py:196`). Persist `last_polled_at`, `last_error`, `consecutive_failures`.
- **Signals emitted**: existing feed (`get_signals`, `store.py:451`) + `GET /api/watches/{id}/signals`; `inserted` vs `dupe` logged.
- **Trigger fires**: existing `TriggerRun` rows (`engine.py:289`) record `fire_source="signal"`, `fire_key="signal:<pk>"`.
- **Webhook delivery**: existing `_send_webhook_pinned` result + `in_flight` markers (`actions.py:176-181`), in the automations activity log.
- **Metrics** (if a sink exists): `poller_polls_total{kind,status}`, `poller_signals_emitted_total{signal_type}`, `poller_dupes_total`, `poller_fetch_errors_total{source}`, `poller_short_circuit_total{source}`, `poller_budget_rejected_total`, histogram `poller_poll_duration_ms`. Else structured logs.
- **Activity**: optional `log_activity` on emit (`refresh.py:182`).

---

## 11. Feature flags / rollout

- **`INTENT_POLLER_ENABLED` (False)**: off → router 404, `handle_watch_poll` early-exits (like `handle_trigger_eval`, `engine.py:254`), `bootstrap_watch_schedules` no-ops. Hot paths untouched.
- **PG-only**: requires `use_pg_store()` (PG + `PG_LEAD_STORE=true` + safe role); on SQLite, bootstrap skipped (log CRITICAL). Recommend hard-gate.
- **Depends on `AUTOMATIONS_ENABLED`** for fires/webhooks; without it the poller still writes the feed.
- **Self-host vs cloud**: self-host gets tables only (SQLite branch, no RLS DDL), all-free sources at `*_COST_USD=0`. Cloud sets `BILLING_ENABLED`, allowlist, budget caps.
- **Cadence**: per-watch `interval`/`schedule_anchor`; global `INTENT_POLLER_DEFAULT_INTERVAL`.
- **Rollout**: (1) migration + models + flag OFF; (2) internal ws, watch logs; (3) enable `on_signal`/webhook rules; (4) GA. Standalone worker redeployed with the handler before flipping the flag in a `RUN_INLINE_WORKER=0` deployment.

---

## 12. Test plan (mapped to acceptance criteria)

Unit tests on SQLite where possible; on_signal/RLS/concurrency behaviors are **PG-gated integration tests** (skip unless a Postgres `DATABASE_URL` is set), matching existing automations/outreach gating. All external HTTP (SEC, DDG/JobSpy, RSS, webhooks) mocked (`respx`/`httpx.MockTransport`).

- **AC-1 (deterministic dedup / exactly-once write)** — same funding event twice through `add_signal` → one `SignalRow`, second `inserted=False`. *Unit.*
- **AC-2 (on_signal fires once, PG)** — insert poller signal for a scoped lead → one `trigger_eval` `fire_key="signal:<pk>"`; re-insert → no new job. *PG.*
- **AC-3 (no fire when lead unmatched)** — signal lead in no scoped workbook → zero `trigger_eval`. *PG.*
- **AC-4 (workspace stamping / RLS)** — `add_signal` from `PgLeadStore(wsA)` writes `workspace_id=wsA` even if dataclass carries `wsB`; read under `wsB` → zero. *PG.*
- **AC-5 (funding source)** — mock SEC; new accession emits, repeat is no-op. *Unit.*
- **AC-6 (hiring band-shift / new tech)** — mock JobSpy; band `growing→rapid_growth` emits ONE `hiring_surge` per ISO week; a same-week re-cross is suppressed; a new tech emits `new_tech_adopted` once; an intent-tier upgrade re-emits once; a raw-count change WITHIN the same band does NOT emit (asserts band semantics, not raw count). *Unit.*
- **AC-7 (RSS incremental)** — 3 entries → bootstrap (no fire, `backfill=false`); +1 entry → exactly one `news`. *Unit.*
- **AC-8 (scheduling alignment)** — `compute_next_run` alignment asserted. *Unit.*
- **AC-9 (bootstrap from mirror, no GUC)** — seed `watch_schedules` due rows → enqueue per-ws under scope without pre-set GUC. *PG.*
- **AC-9b (mirror workspace integrity)** — `_mirror_upsert` copies `watch.workspace_id` verbatim; `watch_id` is a uuid; bootstrap routes back only to the owner. *PG.*
- **AC-10 (handler registered both places)** — `"watch_poll"` in `queue_service.handlers` after `main._register_handlers` AND `worker._register_handlers`; `JOB_TIMEOUTS["watch_poll"]==600`. *Unit.*
- **AC-11 (webhook SSRF)** — `on_signal`→`webhook` to private-resolving host → blocked; public → POST against pinned IP, Host/SNI preserved, redirects off. *Unit.*
- **AC-12 (billing per-poll idempotency)** — with `BILLING_ENABLED` + cost, a poll debits with `run_id=poll fire_key`; a **job retry of the same poll** is an idempotent no-op (`run:<run_id>`); a later-cadence poll debits again (accepted); insufficient credits → 402 caught, free sources still run. *Unit/PG.*
- **AC-13 (flag OFF)** — router 404; handler early-exits; bootstrap no-op. *Unit.*
- **AC-14 (watch deleted mid-poll)** — delete then run stale `watch_poll` → early-exit, no reschedule, mirror gone. *PG.*
- **AC-15 (CRUD + scoping)** — create/list/get/patch/delete only same-ws; create validates `kind`; webhook-rule creation validates URL. *PG.*
- **AC-16 (executive_hired from Form D + negative)** — new Form D with a new C-level related person → one `executive_hired` keyed `{cik}:{name}:{role}`; repeat filing/person → no-op; AND a negative test that NO `executive_hired` is emitted from a JobSpy/news path (no NER source). *Unit.*
- **AC-17 (single-flight concurrency — the cost claim)** — two concurrent `_enqueue_poll_if_absent` for the same `fire_key` (threads/sessions) → exactly one `watch_poll` job (partial-unique-index `IntegrityError` caught); a duplicate paid fetch/debit is therefore impossible. *PG.*
- **AC-18 (fan-out bound / txn-hold load gate)** — a signal matching K on_signal rules enqueues K `trigger_eval` but is bounded by `INTENT_POLLER_MAX_FIRES_PER_SIGNAL`; load test asserts the new-signal-row txn stays under a time bound at high K. *PG load gate.*
- **AC-19 (missed intermediate funding accessions)** — submissions JSON with TWO new "D" accessions since cursor → BOTH emit (not just latest); cursor advances to newest. *Unit.*
- **AC-20 (poll-now vs scheduled: no double-charge/fetch)** — a scheduled poll already queued, then poll-now → 202 "already queued", no second job/fetch/debit; a forced poll-now still single-flights via advisory lock + unique index. *PG.*
- **AC-21 (RSS SSRF/redirect — NEW fetcher)** — feed URL resolving to private IP blocked; feed 302→private blocked (redirects disabled); public feed → pinned-IP GET, Host preserved, `follow_redirects=False`; conditional GET 304 short-circuits with no emit. *Unit.*
- **AC-22 (budget atomicity + poll-now counts)** — N concurrent polls for one ws under `DAILY_POLL_BUDGET` → exactly budget-many proceed (ON CONFLICT counter, no TOCTOU overshoot); poll-now increments the ledger (not exempt). *PG.*
- **AC-23 (tenant-table writes fail-closed)** — cursor/last_error/next_poll_at and budget writes under unset/mismatched GUC raise (RLS `WITH CHECK`); under correct scope succeed; never on a superuser role. *PG.*
- **AC-24 (cursor half-write → no dropped first batch)** — simulate crash after fetch, before cursor commit (txn rollback) → next poll re-detects, `bootstrapped` still false, real first batch NOT silently suppressed; deterministic ids prevent double-emit on the retry. *PG.*
- **AC-25 (real provider returns accession — contract test)** — call the REAL `SecEdgarProvider.list_form_d_since` against canned submissions+primary_doc fixtures (transport-mocked, real parser) and assert it returns `accession` + related persons (guards the blocking-gap fix, not just a hand-mocked adapter). *Unit.*

**Out of scope (v1)**: cross-source coalescing; `executive_hired` from job postings/news (no NER source — Form D only); enrichment of news *content* beyond classification; dedicated push UI; non-EDGAR funding; paid hiring providers (hooks via `*_COST_USD`); historical backfill emission by default; tech-removal/re-add detection.

---

## Key citations
`apps/api/services/leadgen/store.py:39,106-124,158-164,248,412-449` · `orm_models.py:137-163` · `apps/api/services/automations/events.py:168-222` · `engine.py:119,253,289,394-441,454,475-494,497-531` · `apps/api/services/queue_service.py:32,65,129,337` · `apps/api/worker.py:13,58-81` · `apps/api/models.py:30-52` · `apps/api/main.py:126-178` · `apps/api/core/tenancy.py:100-103,110-131,134` · `apps/api/core/config.py:94-109` · `migrations/versions/b2c3d4e5f6a7_outreach_rls.py:134-143,170-199` · `c42d0273d9bd:181-189` · `apps/api/services/workspace/secrets.py:210` · `apps/api/services/billing/service.py:174-220` · `apps/api/services/automations/actions.py:40,96-181` · `apps/api/core/url_guard.py:39` · `sec_edgar.py:79-99,113-139,166-205,260-329,334-349` · `jobspy_signals.py:45-136,230` · `job_tech_intent.py:282-313` · `apps/api/services/signals/monitor.py:34-47` (legacy, avoid) · `apps/api/services/workbook/refresh.py:164-216`.

---

## Open questions for the owner

1. hiring_surge magnitude is fundamentally unreliable: JobSpy's total_jobs is a count of DDG search-result snippets (capped at max_jobs*2), not real open-role counts. We downgraded to coarse growth_signal band crossings. Is band-crossing fidelity acceptable for the product, or should v1 ship without hiring_surge entirely and rely on a real job-board API (paid) before claiming a 'surge' signal type?
2. Re-poll cost: with conditional GET, no-op polls are cheap 304s, but any cadence still does at least one external request per watch per interval, and paid sources are charged per poll (not per event). For 200 watches/ws on hourly cadence that is ~4,800 requests/ws/day even when nothing changes. Confirm the default cadence floor (we propose daily default, hourly opt-in) and whether a paid source should be allowed on sub-daily cadence at all.
3. executive_hired is now Form-D-related-persons-only (the only named-person source in the codebase). This catches officers/directors named on a NEW funding filing, NOT general 'new VP hire' news. Confirm this narrower definition matches the intended signal, or whether executive_hired should be cut from the v1 enum until a real people-news/NER source exists.
4. Within-tenant lead ambiguity: on >1 fuzzy name match we SKIP emission (last_error='ambiguous_lead') rather than guess, to avoid mis-routing a real email. Confirm skip-and-surface is preferred over picking the lowest lead_id (which risks emailing the wrong contact).
5. KEY_SCHEMA_VERSION=1 freezes the dedup-key formula (incl. CIK-supremacy and target normalization) permanently. Any future change to natural_event_id construction requires a backfill/migration. Confirm the frozen scheme (especially: lead_id excluded from the key; CIK overrides name once resolved) before build, since it is effectively irreversible once signals exist.
6. DAILY_POLL_BUDGET semantics: we made poll-now count against the budget to close the bypass. Confirm operators are OK that a user spamming 'poll now' can exhaust the day's poll budget for the whole workspace (vs a separate, smaller poll-now-specific quota).
