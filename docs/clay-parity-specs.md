# Yupcha → Clay-Alternative: Implementation Specs

Status: planning. Authored 2026-06-06. Grounded in the current codebase (exact
signatures/line numbers inline). The enrichment engine is already near Clay
parity (27 providers, waterfalls, conditional + AI columns, templates, signals).
These specs close the gaps that make it a *product*: safe multi-tenancy, an
output/push loop, a web-research agent, more destinations, and (if commercial)
billing + Postgres.

Sequencing: **P0 → P1** first. P0 makes the tenancy we shipped actually safe;
P1 is the smallest change with the biggest "now it's Clay" payoff (services
already exist, only the workbook wiring is missing).

| Phase | Item | Effort | Blocks |
|---|---|---|---|
| P0 | WI-1 JobRunner workspace scoping | M | onboarding a 2nd team |
| P0 | WI-2 Signals workspace scoping | S | " |
| P0 | WI-3 Enrichment write-back scoping | S | " |
| P0 | WI-4 SSRF allowlist | S | security |
| P1 | WI-5 Output column (webhook/CRM/sequencer) | M | the Clay loop |
| P1 | WI-6 Per-workspace integration creds | S | WI-5 correctness |
| P2 | WI-7 Web-research ("Claygent") column | L | differentiation |
| P2 | WI-8 Salesforce + Sheets/Airtable destinations | M | parity |
| P3 | WI-9 Credits/billing enforcement + Stripe | L | monetization |
| P3 | WI-10 Postgres migration | XL | scale |

S≈½–1d, M≈2–4d, L≈1–2wk, XL≈3wk+ (single dev).

---

## P0 — Make multi-tenancy actually safe

Today's holes (all verified): collection writes to the global `leads.db`,
signals are global, inline enrichment write-back hits the global DB, and the
scraper has no SSRF guard. Until these close, a second tenant can read/write the
first tenant's data.

### WI-1 — JobRunner workspace scoping

**Problem.** `routers/leads.py:532` does `runner = JobRunner()`; `JobRunner.__init__`
(`services/leadgen/job_runner.py:57`) defaults to `self.db = db or LeadDB()` →
the global `data/leads.db`. `_process_job` (`job_runner.py:106`) reads
`workspace_id` off the job but never uses it to pick a DB. So every collected
lead (`upsert_lead`, `job_runner.py:588`) lands in main. The collection `jobs`
table also lives in the per-file `LeadDB`, so job *reads* must move too.

**Spec.**
1. `routers/leads.py` `start_collection` — bind the runner to the caller's
   workspace DB:
   ```python
   from apps.api.services.workspace.manager import workspace_leads_db_path
   runner = JobRunner(db=LeadDB(workspace_leads_db_path(ctx.slug)))
   ```
   Keep setting `body.workspace_id = body.workspace_id or ctx.workspace_id`.
2. Route every collection-job **read** endpoint through `ctx.lead_db()` instead
   of `_get_db()`: `list_jobs`, `get_job_detail`, `get_job_stages`,
   `get_job_leads`, `cancel_job`, `delete_job`, `retry_job`, `system_stats`.
   (These currently keep `_get_db()` per the tenancy pass — that's the mismatch.)
   `retry_job` must re-create the runner with the same workspace path.
3. `JobRunner` already threads `self.db` through create_job/create_stage/
   upsert — no internal change needed once it's constructed with the right path.
4. Each per-request `JobRunner` opens its own `sqlite3` connection (WAL is on,
   `db.py:56`), so the background thread is isolated. No singleton sharing.

**Acceptance.** Team B runs `/api/collect`; rows + the job appear only in
`data/workspaces/<B>/leads.db`, and Team A's `/api/jobs` never shows them.
Verify the file: `sqlite3 data/workspaces/<B>/leads.db 'select count(*) from leads'`.

**Risk.** `system_stats` may aggregate across the main DB today; scope it to the
workspace (or add an explicit cross-workspace admin view later).

### WI-2 — Signals workspace scoping

**Problem.** `services/signals/monitor.py:58` `CREATE TABLE signals(...)` has no
tenant column; `add_signal` (`:83`) / `get_signals` (`:99`) don't filter; and
`run_signal_scan` (`:185`) does `db = LeadDB()` (global) and scans all hot/warm
leads regardless of workspace.

**Spec.**
1. Migration: `ALTER TABLE signals ADD COLUMN workspace_id TEXT DEFAULT ''` +
   `CREATE INDEX idx_signals_ws ON signals(workspace_id)`. Guard with a
   `PRAGMA table_info` check like the existing migrations.
2. Thread `workspace_id`:
   - `add_signal(signal, workspace_id: str)` → store it.
   - `get_signals(..., workspace_id: str)` → add `AND workspace_id = ?`.
   - `run_signal_scan(workspace_id: str, slug: str)` → `db = LeadDB(workspace_leads_db_path(slug))`; tag every detected signal with `workspace_id`.
3. `routers/signals.py` — pass `ctx.workspace_id` / `ctx.slug` into all four
   endpoints (they already take `ctx`).

**Acceptance.** A scan in workspace A produces signals only visible to A;
`get_signals` for B returns `[]`.

**Note.** Keeping one `signals.db` with a `workspace_id` column is fine (signals
are low-volume). Switch to per-file only if it grows.

### WI-3 — Enrichment write-back scoping

**Problem.** `services/workbook/enrichment.py:308` `_write_back_to_lead(lead_id,
field, value, provider)` opens `LeadDB()` (global). Called at `:213` and `:226`
inside `enrich_cell`. v2 `WorkbookRow` data (in `data.db`) is already scoped;
only the v1 lead write-back leaks.

**Spec.**
1. Add a `lead_db_path: Optional[str] = None` param to `_write_back_to_lead`;
   open `LeadDB(lead_db_path)` (falls back to default only when None).
2. `enrich_cell` (`:108`) already loads the `Workbook` (or the worker does).
   Derive the path once at the top:
   ```python
   from apps.api.services.workspace.manager import workspace_slug, workspace_leads_db_path
   wb = db.query(Workbook).filter(Workbook.id == workbook_id).first()
   _ws_slug = workspace_slug(wb.workspace_id) if wb and wb.workspace_id else None
   _lead_db_path = workspace_leads_db_path(_ws_slug) if _ws_slug else None
   ```
   Pass `_lead_db_path` to both `_write_back_to_lead` calls.
3. `services/workbook/worker.py:process_enrich_cell` already reconstructs the
   workbook — pass the resolved path along so the BullMQ path matches inline.

**Acceptance.** Enriching a workbook in workspace B updates B's `leads.db`, not
main. Add a unit check that asserts the opened path.

### WI-4 — SSRF allowlist

**Problem.** `routers/leads.py:770` `public_scrape` passes a user URL straight to
`UniversalScraper.scrape` (`services/scraper.py`), which fetches via
`httpx.AsyncClient(verify=False, follow_redirects=True)` (`:28`) and Playwright
`page.goto` (`:53`). No scheme/IP validation → a tenant can hit
`http://169.254.169.254/...`, `localhost`, or private ranges. `person_intel`'s
deep scrape (`person_intel.py:440`) shares the same scraper.

**Spec.**
1. New `apps/api/core/ssrf.py`:
   ```python
   def validate_public_url(url: str) -> str:
       """Return the URL if safe; raise ValueError otherwise.
       - scheme in {http, https} only
       - resolve ALL A/AAAA records; reject if any is loopback/private/
         link-local/reserved/multicast (ipaddress.ip_address(...).is_*)
       - explicitly block 169.254.169.254 and ::ffff:169.254.169.254
       - reject userinfo (user:pass@) and non-standard ports optionally
       """
   ```
   Use `socket.getaddrinfo` to resolve, check every resolved IP (defeats DNS
   rebinding for the initial fetch).
2. Call it in `public_scrape` before scraping (400 on failure), and **again**
   inside `UniversalScraper.scrape` (defense in depth) and on each redirect hop
   (set `follow_redirects=False`, validate `Location`, re-request). Set
   `verify=True`.
3. Apply to `person_intel` deep scrape and any other user-URL fetch
   (`enrichment` web-research `client.stream` at `leads.py:241` validates
   `provider['base_url']`).
4. Optional per-workspace allowlist/denylist later via `workspace_settings`.

**Acceptance.** `POST /api/v2/scraper/scrape {"url":"http://169.254.169.254/"}`
→ 400; `{"url":"http://localhost:8000/health"}` → 400; a real public URL still
works. Add tests for `validate_public_url` over a table of IPs.

---

## P1 — Close the Clay loop: enrich → push

### WI-5 — Output column (webhook / CRM / sequencer)

**Problem.** The `output` column type exists in the registry
(`models.py` `COLUMN_TYPES["output"]`) and schema (`schemas.py` ColumnConfig
`destination`, `destination_config`) but has **no execution**. The destination
services already exist and work: `services/crm/hubspot.py`
`push_lead_as_contact(lead, field_map)` (`:89`), `services/outreach/sequence.py`
`enroll_leads(seq_id, lead_ids)` (`:212`). Only the workbook wiring is missing.

**Spec.**
1. New `apps/api/services/workbook/output.py`:
   ```python
   async def execute_output_column(
       col_config: dict, lead_data: dict, columns_config: list,
       workbook_id: str, lead_id: int, workspace_id: str,
   ) -> dict:
       """Push the row to an external destination.
       Returns {"success": bool, "value": <summary str>, "error": str|None}."""
       dest = col_config.get("destination")            # webhook | crm | sequencer
       cfg  = col_config.get("destination_config") or {}
       if dest == "webhook":   return await _send_webhook(cfg, lead_data, columns_config, workspace_id)
       if dest == "crm":       return await _push_crm(cfg, lead_data, workspace_id)
       if dest == "sequencer": return _enroll_sequence(cfg, lead_id)
       return {"success": False, "value": "", "error": f"unknown destination {dest}"}
   ```
   - `_send_webhook`: resolve `{column}` in `cfg["url"]` / `cfg["body"]` via the
     existing `ai_column._resolve_prompt` + `_get_row_values`; **validate the URL
     with `core.ssrf.validate_public_url`** (reuse WI-4); `httpx` request with
     `cfg["method"]` (default POST), `cfg.get("headers", {})`; return status code.
   - `_push_crm`: `cfg["type"]` selects adapter (`hubspot` now, `salesforce` in
     WI-8). Build a lead object/dict from `lead_data`, call
     `push_lead_as_contact(lead, cfg.get("field_map"))`. Read the API token
     per-workspace (WI-6).
   - `_enroll_sequence`: `enroll_leads(cfg["sequence_id"], [lead_id])`; optionally
     `execute_pending_sends` if `cfg.get("send_now")`.
2. Dispatch — `services/workbook/enrichment.py` `enrich_cell`, add a branch after
   the `ai_formula` block (~`:165`), before the provider `else`:
   ```python
   elif col_type == "output":
       out = await execute_output_column(col_config, lead_data, columns_config,
                                         workbook_id, lead_id, _workspace_id)
       result_value, result_error = out["value"], out.get("error")
       result_provider = col_config.get("destination", "output")
   ```
3. Run filter — `routers/workbooks.py` run endpoint (`~:661`): add `"output"` to
   `c.get("type") in ("enrichment","waterfall","ai_formula")`. The worker
   (`worker.py:process_enrich_cell`) needs no change (dispatches by type).
4. **Idempotency.** Output columns must not double-push on re-run. Gate: skip if
   the cell's existing enrichment status is already `success`, unless the run was
   triggered with `force=True`. Document this in the column config
   (`cfg["run_once"]`, default true).
5. **Ordering.** Outputs should run after enrichment columns. Sort the run list
   so `output` columns execute last within a row.

**Acceptance.** A workbook with an `email` waterfall + an `output→webhook` column
fires the webhook once per row with the resolved body; `output→crm` creates a
HubSpot contact; `output→sequencer` enrolls the lead. Cell shows
`success`/`error` with the HTTP status or CRM id.

### WI-6 — Per-workspace integration credentials

**Problem.** `hubspot._get_token` (`:22`) and `sender.get_smtp_config` (`:38`)
read from the **global** `settings` table (`data.db`) via `_db_get`. In a
multi-tenant app, Team A's HubSpot token must not be used for Team B. The
`workspaces.db` already has a `workspace_settings(workspace_id, key, value)`
table — unused for this.

**Spec.**
1. Add `services/workspace/manager.py` helpers:
   `get_workspace_setting(workspace_id, key, default="")` and
   `set_workspace_setting(workspace_id, key, value)` over `workspace_settings`.
2. Add a resolver `core/secrets.py: get_secret(workspace_id, key)` =
   workspace_settings → global settings → env (so global stays a fallback).
3. Thread `workspace_id` into `hubspot._get_token`, `sender.get_smtp_config`, and
   the output adapters; read via `get_secret`.
4. Settings UI: a per-workspace integrations panel writing `workspace_settings`.

**Acceptance.** Setting a HubSpot token in workspace B and pushing from B uses
B's token; A is unaffected. (Do this with WI-5 so CRM/sequencer pushes are
tenant-correct from day one.)

---

## P2 — The two things Clay has that we don't

### WI-7 — Web-research ("Claygent") column

**Problem.** `ai_column.execute_ai_column` (`:74`) runs an LLM over *existing*
row data only. `llm.py` has **no tool-use / web-search** (`complete` `:153`,
`extract_json` `:185` hit plain `/chat/completions`). Clay's Claygent *browses*
to answer arbitrary per-row questions. We already have the pieces: scrapers
(`scrapers/google_search.py`, `ddgs`), `UniversalScraper.scrape` (with WI-4
guard), and the LLM client.

**Spec.** Build a bounded ReAct loop (no native tool-use needed — drive it with
`extract_json`).
1. Register a new column type `research` in `models.COLUMN_TYPES` + `schemas.py`
   config: `{ "prompt": str, "max_steps": int=4, "output_format": "text|json" }`.
2. New `services/workbook/research_column.py`:
   ```python
   async def execute_research_column(prompt, row_values, columns_config,
                                     max_steps=4, output_format="text") -> dict:
       # 1. resolve {placeholders} in prompt (reuse ai_column._resolve_prompt)
       # 2. loop up to max_steps:
       #    decision = await llm.extract_json(REACT_PROMPT + scratchpad)
       #       -> {"action": "search"|"fetch"|"answer", "query"|"url"|"final": ...}
       #    search -> ddgs/google_search top-k titles+snippets
       #    fetch  -> validate_public_url + UniversalScraper.scrape -> preview_text
       #    answer -> return value
       # 3. hard stop at max_steps -> force a final answer from scratchpad
   ```
   Tools wrap existing functions; cap fetched bytes; record `llm` usage as today.
3. Dispatch in `enrich_cell` (`elif col_type == "research"`).

**Acceptance.** A `research` column with prompt "Does {Company} use Kubernetes?
cite a source" performs 1–3 searches/fetches and returns an answer + URL, within
`max_steps`. Latency and step count bounded; failures degrade to "unknown".

**Risk.** Cost/latency — enforce `max_steps`, per-row timeout, and (with P3) a
credit cost. Reuse WI-4's SSRF guard on every fetch.

### WI-8 — Salesforce + Sheets/Airtable destinations

**Spec.**
1. `services/crm/salesforce.py` mirroring the hubspot interface:
   `push_lead_as_contact(lead, field_map)`, `_get_token` via `get_secret`
   (OAuth2 username-password or connected-app token in `workspace_settings`:
   `SALESFORCE_*`). Register as `destination_config.type == "salesforce"` in
   `output._push_crm`.
2. Google Sheets export: `services/integrations/sheets.py` using a service-account
   JSON (per-workspace setting) or OAuth; append rows. Expose as an `output`
   destination `sheets` and as a bulk `/api/workbooks/{id}/export/sheets`.
3. Airtable: `services/integrations/airtable.py` (PAT + base/table in settings);
   same two entry points.
4. Generic outbound webhook is already covered by WI-5.

**Acceptance.** From a workbook, push to Salesforce (contact created), and export
a workbook to a Google Sheet / Airtable base.

---

## P3 — Productization (only if commercial)

### WI-9 — Credits / billing enforcement + Stripe

**Problem.** Usage is *tracked* (`llm_usage` table `db.py:178`,
`record_llm_usage` `:634`) but never *enforced*: no credit balance, no cost map,
no Stripe. Free-tier strings in `settings.py` are informational.

**Spec.**
1. Schema (workspaces.db or Postgres):
   `workspace_credits(workspace_id PK, balance INTEGER, plan TEXT, updated_at)`
   and `usage_ledger(id, workspace_id, ts, kind, units, cost_credits, ref)`.
2. Cost map `services/billing/costs.py`: credits per action
   (enrichment provider call, AI column, research step, output push). Start
   simple: 1 credit / enrichment cell, N / research step.
3. `services/billing/meter.py: check_and_debit(workspace_id, cost) -> bool`
   (atomic decrement; raise `402 Payment Required` when insufficient).
4. **Enforcement hook.** The single funnel is `enrich_cell` (every cell run, all
   types pass through it). Call `check_and_debit` at the top with the cost for
   `col_type`; record to `usage_ledger`. (LLM-direct calls in `copilotkit` can be
   metered separately or exempted.)
5. Stripe: `routers/billing.py` — `POST /api/billing/checkout` (Stripe Checkout
   for a credit pack / plan), `POST /api/billing/webhook` (verify signature,
   credit the workspace on `checkout.session.completed`). Env:
   `STRIPE_SECRET_KEY`, `STRIPE_WEBHOOK_SECRET`, `STRIPE_PRICE_*` (add to
   `core/config.py`).
6. UI: balance badge + buy-credits + usage ledger.

**Acceptance.** A workspace with 0 credits gets 402 on enrichment; buying a pack
via Stripe test mode credits the balance; the ledger reconciles with runs.

### WI-10 — Postgres migration

**Problem.** Four SQLite stores: SQLAlchemy `data.db` (users, workbooks, jobs,
person_intel — `database.py:7`), raw-sqlite `workspaces.db` (workspace manager),
raw-sqlite per-workspace `leads.db` files (`LeadDB`, `db.py:54`), raw-sqlite
`signals.db`. Mixed ORM + file-per-tenant won't survive concurrent multi-tenant
load.

**Spec (phased).**
1. **Unify on SQLAlchemy.** Port `LeadDB`, workspace manager, and signals to
   SQLAlchemy models with an explicit `workspace_id` column (leads already carry
   `workspace_id`, `db.py:207`). Replace file-per-workspace with **row-level**
   partitioning by `workspace_id` in a single `leads` table.
2. `ctx.lead_db()` changes from "open a file" to "a workspace-scoped query
   helper" that always filters `workspace_id == ctx.workspace_id` (and asserts it
   on writes). This also removes the WI-1/WI-3 path-threading once landed.
3. Postgres via `DATABASE_URL=postgresql+psycopg://...`; keep SQLite for local
   dev. Add Alembic for migrations (replacing the hand-rolled
   `check_and_migrate_db`).
4. Optional hard isolation: Postgres Row-Level Security policies on
   `workspace_id` as defense in depth.
5. Data migration script: walk every `data/workspaces/*/leads.db` + `data/leads.db`,
   insert rows tagged with the owning `workspace_id` into Postgres.

**Acceptance.** App runs against Postgres; two tenants' leads coexist in one
table with zero cross-reads; Alembic head is clean; the per-file DBs are
retired.

**Why XL.** Touches every raw-sqlite call site and the tenancy core. Do it after
the product loop (P1) and ideally alongside WI-9 (both want a real DB).

---

## Dependency order (recommended)

```
WI-4 (SSRF util)  ─┐
WI-1, WI-2, WI-3   ├─ P0: safe to onboard a 2nd team
                   │
WI-6 (ws creds) ───┼─ P1: WI-5 output column (reuses WI-4 + WI-6)
WI-5 ──────────────┘
                   │
WI-7 research  ────┼─ P2 (reuses WI-4 guard)
WI-8 destinations ─┘   (reuses WI-5 framework)
                   │
WI-9 billing  ─────┼─ P3 (enforce at enrich_cell funnel)
WI-10 Postgres ────┘   (subsumes WI-1/WI-3 path-threading)
```

Smallest valuable slice to ship next: **WI-4 + WI-1 + WI-5** — safe tenancy on
the collection path plus the enrich→push loop, which is the headline "it's Clay"
capability.
