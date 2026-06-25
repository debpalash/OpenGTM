<!-- Auto-generated design spec (2026-06-26). Review verdict: needs-revisions. -->

## ✅ LOCKED SCOPE DECISIONS (owner-approved)

1. **Add two tiny read-only backend endpoints for PROACTIVE gating:** `GET /api/me/context` → the caller's per-workspace role (admin|editor|member), and `GET /api/flags` → {automations_enabled, intent_poller_enabled, pg_lead_store, allow_legacy_outreach}. UI disables/hides write controls + flagged features up front (tooltip explaining why), with the reactive 403/404/409 handling kept as a backstop.
2. **Live updates = POLLING** (React Query `refetchInterval` on run-history + signal-feed views, plus invalidate-on-mutation). No SSE in v1.
3. Defaults accepted: webhook `header_secret_ref` and `push_crm` `field_map` use free-text inputs + helper copy in v1 (no secret-list / CRM-field discovery endpoints yet); confirm in code that the worker persists the run `fire_key` onto TriggerRun for the Save&Run→RunDetail match (else fall back to navigating to the rule's run-history list).

BUILD APPROACH: scaffold first (the 2 backend endpoints + frontend foundation: typed API client for all surfaces + me/context + flags, React Query hooks/keys, routing+nav, shared gating/empty/error components), merged; THEN parallel page slices (Sequences rewire · Automations · Watches · run/signal inspection) on top.


# Production Spec — GTM Automation UI (Sequences · Automations/Rules · Intent Watches · Run/Signal Inspection)

Target: `apps/web` (Vite 8 + React 19 + Tailwind v4 + shadcn/ui). Backend surfaces: `apps/api/routers/{outreach,automations,watches}.py`. This spec rewires the existing `outreach.tsx` + the SMTP tab in `settings.tsx`, and adds two net-new flagged pages (Automations, Watches) plus shared run/signal inspection. All new code uses **React Query style #1** (fetch fns in `api.ts` → hooks in `hooks.ts` → keys in `query-client.ts`), per the web brief's stated rewire goal.

> **Changes after review** are summarized at the very end. The most consequential corrections: (a) full **403 role-gating UX** added across every mutation, with a documented constraint that per-workspace role is **not** exposed client-side; (b) the bogus **402 spend-cap HTTP path** removed — cap enforcement is now a **client-side** `projected_total_usd` vs `max_spend_usd_per_day` comparison; (c) **workbook scope + re_enrich column pickers** are concretely specified by reusing the existing `workbook-api.ts`/`useWorkbooks()` (they exist); (d) the **Save & Run → RunDetail** flow is redesigned around `fire_key` (run returns no `run_id`); (e) corrected exact `detail` strings and the two distinct 429 reasons; (f) per-action config sub-forms, condition-field help, legacy-gate decision, SignalFeed contract, and a11y grouping are all nailed down.

---

## 1. Goals & UX — user journeys

### 1.1 Sequences (Outreach) — rewire of `apps/web/src/pages/outreach.tsx`
**Journey:** Marketer opens **Outreach**, sees SMTP status chip + sequence list. Creates a sequence (name, description, consent_basis, steps with subject/body/delay, send window + daily limit). Opens detail → sees per-status stats from `GET /sequences/{id}` `stats` (pending/scheduled/sent/opened/replied/bounced/failed/skipped/suppressed/completed). Enrolls leads (lead picker → `POST /enroll` with `consent_source`), Activates (`/start`), pages through the **Sends log** (`GET /sends`), and manages **Suppressions** (`GET/POST/DELETE /suppressions`). SMTP config stays in Settings (`settings.tsx` SMTPConfigTab, rewired to React Query).

Key fixes vs current code: `outreach.tsx:301` POSTs `{name, steps}` only — must send full `CreateSequenceRequest` (`consent_basis`, `daily_limit`, send-window fields). `outreach.tsx:447` enroll omits `consent_source` (required by `EnrollLeadsRequest`). `outreach.tsx:463` reads `data.results.sent` but `/execute` returns `{enqueued:n}` — fix shape. Start/pause errors are toasted (good, `outreach.tsx:419`); extend to surface 400 SMTP/consent/footer `detail` and **403 (admin-only)** — see §6.

**Role reality (verified):** `create/update/delete/start/pause/execute/smtp/suppression` all require `require_workspace_role("admin")`; only `list/get/stats/sends/smtp-status/list-suppressions` and **`enroll`** use `current_workspace` (any member). So a member **can enroll but cannot start/execute** — this asymmetry must be surfaced (see §6, member banner).

```
┌ Outreach ───────────────────────────── [SMTP: ok ✓] [+ New Sequence] ┐
│ 4 sequences · 220 sent · 51 opened · 12 replied                       │
│ (member, non-admin) ⓘ You can view & enroll; activating needs admin.  │
├──────────────────────────────────────────────────────────────────────┤
│ ● Cold — SaaS founders   active   3 steps · 80 leads   80▸ 30▣ 8↩  ⋮ >│
│ ○ Re-engage Q2           draft    2 steps · 0 leads     0▸  0▣ 0↩  ⋮ >│
└──────────────────────────────────────────────────────────────────────┘
Detail:  ← Back  Cold — SaaS founders  ●active   [Pause] [Send now]
 ⚠ Auto-paused after 6 bounces — resolve then [Resume] (admin)   (if auto_paused)
 ┌Enrolled 80┐┌Sent 80┐┌Opened 30┐┌Replied 8┐┌Bounced 2┐┌Suppr 1┐
 [Pick leads…] [consent: existing_customer ▾] [Enroll]   Tabs: Steps | Sends log | Suppressions
 Step 1 ● Subject…  │ Step 2 +72h ● Subject…
```

### 1.2 Automations (rules) — new `apps/web/src/pages/automations.tsx`
**Journey:** Admin opens **Automations** (404 when `AUTOMATIONS_ENABLED` off → "feature off" screen, never blank). Lists triggers (`GET /triggers`). Builds a rule: name → **trigger** (`on_signal|on_row_changed|on_row_added|on_schedule`; schedule needs `trigger_config.interval`) → optional **condition** string → **ordered actions** (`re_enrich|push_crm|webhook`; `sequencer|send_email` legacy, gated) → scope workbooks, caps (`max_spend_usd_per_day`, `max_actions_per_day`), `stop_on_error`. **Dry-run preview** (`POST /preview`) shows matched rows + `projected_total_usd` + per-action `would_charge_usd`; the panel compares `projected_total_usd` to the rule's `max_spend_usd_per_day` **client-side** and colors it destructive if over (there is **no** server 402 on preview/run — verified). Pause/Resume. Opens detail → `recent_runs`; opens a run → `GET /runs/{run_id}` with `action_results`.

```
┌ Automations ─────────────────────────────────── [+ New Rule] ┐
│ ● Re-enrich new rows   on_row_added   enabled   last fired 2h │
│ ○ Push hot to CRM      on_signal      paused    —           ⋮ │
└───────────────────────────────────────────────────────────────┘
Rule builder:                                  (admin-only — see §6 gating)
  Name [__________]   Trigger ( on_signal ▾ )  [interval: daily ▾]*
   ⓘ on_signal needs the Postgres lead store; if not enabled, Save returns
     409 and we keep your form so you can switch trigger type.
  Scope workbooks [▾ multi from useWorkbooks()]   Condition [score >= 80 ▾fields]
   ⓘ insert field ▾ (column names from scoped workbooks)   op help: == != < <= > >= and/or
  Actions (ordered):
   1. re_enrich  {column_ids:[col_email]}      [↑][↓][✕]   (cols from scoped workbooks)
   2. push_crm   {type:hubspot, field_map:{…}} [↑][↓][✕]
   [+ add action ▾]   (send_email/sequencer hidden unless legacy flag on — see §6)
  Caps: max $/day [10] · max actions/day [500] · ☑ stop_on_error
  [Dry-run preview]  → matched 42 · projected $1.30  (red if > max $/day)
  [Save]   [Save & Run…]   (Run opens confirm; dry_run defaults ON)
Run detail:  status succeeded · 42 matched · $1.28 charged
  # action_type   status    charge  summary
  1 re_enrich     success   $0.03   email found
  2 push_crm      skipped   $0.00   no crm configured
```

### 1.3 Intent Watches — new `apps/web/src/pages/watches.tsx`
**Journey:** User opens **Watches** (404 when `INTENT_POLLER_ENABLED` off → feature-off screen; 409 `intent_poller_requires_pg_lead_store` → "requires Postgres lead store" screen — distinct copy). Lists watches (`GET ""`). Creates a watch: `kind` (`funding|hiring|feed|company`) → `target` → `interval` → `signal_types` (per-kind allowed set, verified `watches.py:41`: funding→{company_funded,executive_hired}, hiring→{hiring_surge,new_tech_adopted}, **feed→{news}**, company→all four), optional `lead_id`, optional `create_webhook_rule` (+`webhook_url`). **Poll now** (`POST /{id}/poll`; handle 409 `watch disabled`, two 429 reasons, and the 202 `already_queued`). Views the **signal feed** (`GET /{id}/signals`) — reuse the `signals.tsx` feed layout. Pause/Enable/Delete. Mutations require `editor`/`admin`; reads are any member.

```
┌ Intent Watches ───────────────────────────── [+ New Watch] ┐
│ ● Acme funding   funding   daily   next 4h   2 fails  ⓘ ⋮  │
│ ○ Stripe hiring  hiring    hourly  disabled         ⋮      │
└────────────────────────────────────────────────────────────┘
Watch detail:  Acme · funding · daily   [Poll now] [Pause] [Delete]
  signal_types: company_funded, executive_hired
  Signals feed (reuses signals.tsx row layout — full Signal shape):
   ⊕ company_funded  Acme raised Series B   crunchbase   2h
   ⊕ executive_hired New VP Eng            news         1d
```

---

## 2. Information architecture & routing — `apps/web/src/App.tsx`

- **Nav (`NAV_ITEMS`, App.tsx:41):** keep `Outreach` (`Send`). Add `{to:"/automations", icon: Zap, label:"Automations"}` and `{to:"/watches", icon: Radar, label:"Watches"}` (import `Radar` from lucide; `Zap` already imported App.tsx:14). Place both after `/outreach`, before `/signals`. **Nav items stay visible for all roles** — the page itself surfaces flag-off / dependency / role states; we never hide nav based on role because per-workspace role isn't reliably known client-side (see §6).
- **Routes (AppContent `<Routes>`, App.tsx:365):** add `<Route path="/automations/*" element={<AutomationsPage/>} />` and `<Route path="/watches/*" element={<WatchesPage/>} />`. Import both at top (App.tsx:38 region). Keep wildcard redirect to `/chat`.
- **`getTitle()` (App.tsx:345):** add `if (startsWith("/automations")) return "Automations"` and `"/watches" → "Watches"`.
- **No router nesting needed** — both pages own internal view state (`list|create|detail|run`) like `outreach.tsx:70`. Optional URL params for `/automations/:id` / `/automations/:id/runs/:runId` are a stretch (mirrors `/leads/:id` at App.tsx:367). Spec assumes internal `useState` view machine to match `outreach.tsx`.
- **Flag-off pages still mount** — `RequireAuth` (App.tsx:413) already guarantees an active workspace; the flag-off / feature-disabled / 403 state is rendered *inside* the page from the query/mutation result, so nav links never lead to a blank screen.

---

## 3. API client layer — add to `apps/web/src/lib/api.ts`

Follow existing thin-fetch pattern (no auth headers — interceptor in `auth.ts:66` wraps `window.fetch` and attaches `Authorization`+`X-Workspace-Id` for `/api/*`; always `Content-Type: application/json` on mutations). Add a shared helper to parse error bodies so 404/409/422/403/429 `detail` surfaces instead of `catch {}` swallowing. (Note: workbook fetch fns live in the existing `workbook-api.ts`; **reuse** them rather than duplicating — see §4.)

```ts
// api.ts — shared error handling for the new surfaces
export class ApiError extends Error {
  constructor(public status: number, public detail: string, public body?: unknown) {
    super(detail); this.name = "ApiError"
  }
}
async function jsonOrThrow<T>(res: Response): Promise<T> {
  const data = await res.json().catch(() => ({}))
  if (!res.ok) throw new ApiError(res.status, (data as any)?.detail ?? `HTTP ${res.status}`, data)
  return data as T
}
```

### 3.1 Types (api.ts)
```ts
// Outreach
export interface SeqStep { step_number:number; subject:string; body_html:string; delay_hours:number }
export interface Sequence { id:string; workspace_id:string; name:string; description:string;
  steps:SeqStep[]; status:string; daily_limit:number; send_window_start:number; send_window_end:number;
  send_window_tz:string; consent_basis:string; bounce_count:number; complaint_count:number;
  auto_paused:boolean; created_at:string; updated_at:string }
// Known per-status keys are explicit (the loose index sig only covers extras), so typos are caught:
export interface SeqStats {
  total:number; emails_sent:number; bounce_count:number; complaint_count:number; auto_paused:boolean;
  pending?:number; scheduled?:number; sent?:number; opened?:number; replied?:number; bounced?:number;
  failed?:number; skipped?:number; suppressed?:number; completed?:number;
  [status:string]:number|boolean|undefined }
export interface SeqSend { id:string; sequence_id:string; enrollment_id:string; lead_id:number;
  step_number:number; to_email:string; subject:string; status:string; skip_reason:string|null;
  message_id:string|null; charged_usd:number; migrated:boolean; error:string|null;
  sent_at:string|null; created_at:string }
export interface Suppression { id:string; email:string; reason:string; source:string; locked:boolean; created_at:string }
export interface SmtpStatus { configured:boolean; host:string|null; email:string|null; from_name:string; max_per_hour:number }
export interface CreateSequenceRequest { name:string; description?:string; steps:SeqStep[];
  daily_limit?:number; send_window_start?:number; send_window_end?:number; send_window_tz?:string; consent_basis:string }
export interface EnrollLeadsRequest { lead_ids:number[]; consent_source:string }

// Automations
export type TriggerType = "on_signal"|"on_row_changed"|"on_row_added"|"on_schedule"
export type ActionType = "re_enrich"|"push_crm"|"webhook"|"sequencer"|"send_email"
export interface RuleAction { type:ActionType; config:Record<string,unknown> }
export interface Trigger { id:string; workspace_id:string; name:string; enabled:boolean;
  trigger_type:TriggerType; trigger_config:Record<string,unknown>; condition:string; actions:RuleAction[];
  scope_workbook_ids:string[]; stop_on_error:boolean; max_spend_usd_per_day:number|null;
  max_actions_per_day:number|null; next_run_at:string|null; schedule_anchor:string|null;
  last_fired_at:string|null; created_by:string; created_at:string; updated_at:string }
export interface TriggerRun { id:string; trigger_id:string; trigger_name_snapshot:string; job_id:string|null;
  status:string; fire_source:string; fire_key:string; matched_rows:number; actions_attempted:number;
  actions_succeeded:number; actions_skipped:number; actions_failed:number; total_charged_usd:number;
  error:string|null; started_at:string|null; finished_at:string|null }
export interface ActionResult { id:string; run_id:string; trigger_id:string; workbook_id:string; row_id:string;
  lead_id:number|null; action_index:number; action_type:ActionType; status:string; skip_reason:string|null;
  charged_usd:number; result_summary:string; error:string|null; created_at:string }
export interface PreviewResult { matched:number; projected_total_usd:number;
  sample:{ row_id:string; workbook_id:string; condition_pass:boolean;
    actions:{ type:ActionType; would_charge_usd:number; resolved_payload_preview:unknown }[] }[] }
export interface RunHandle { job_id:string; fire_key:string; dry_run:boolean } // POST /run returns NO run_id

// Watches
export type WatchKind = "funding"|"hiring"|"feed"|"company"
export type WatchInterval = "hourly"|"daily"|"weekly"
export interface Watch { id:string; workspace_id:string; kind:WatchKind; target:string;
  resolved_cik:string|null; lead_id:number|null; signal_types:string[]; interval:WatchInterval;
  enabled:boolean; next_poll_at:string|null; last_polled_at:string|null; last_error:string|null;
  consecutive_failures:number; cursor:Record<string,unknown>; created_at:string }
// /signals returns the FULL signal dict (verified _signal_to_dict): description/source_url/weight/read present;
// created_at is a NUMBER (epoch), matching signals.tsx — not a string.
export interface WatchSignal { id:string; workspace_id:string; lead_id:number|null; company:string;
  signal_type:string; title:string; description:string; source:string; source_url:string|null;
  weight:number; created_at:number; read:number /* 0|1 */ }
export interface WatchCreate { kind:WatchKind; target:string; lead_id?:number; signal_types?:string[];
  interval?:WatchInterval; create_webhook_rule?:boolean; webhook_url?:string; webhook_secret_ref?:string }
```

### 3.2 Fetch methods (path · method · req · res)
**Outreach** (`/api/outreach`, no flag):
- `listSequences()` → GET `/sequences` → `{sequences:Sequence[]}`
- `createSequence(b:CreateSequenceRequest)` → POST `/sequences` → `{id,name,status}` (403 admin)
- `getSequence(id)` → GET `/sequences/{id}` → `Sequence & {stats:SeqStats}` (404)
- `updateSequence(id, b:Partial<CreateSequenceRequest>&{status?})` → PUT `/sequences/{id}` → `{status:"ok",id}` (403/404)
- `deleteSequence(id)` → DELETE → `{status:"ok"}` (403/404)
- `startSequence(id)` → POST `/sequences/{id}/start` → `{status:"active"}` (403; 400 SMTP/consent/footer → `detail`)
- `pauseSequence(id)` → POST `/sequences/{id}/pause` → `{status:"paused"}` (403)
- `enrollLeads(id, b:EnrollLeadsRequest)` → POST `/sequences/{id}/enroll` → `{enrolled,skipped:[{lead_id,reason}],total_lead_ids}` (any member)
- `executeSequence(id)` → POST `/sequences/{id}/execute` → `{enqueued:number}` (403)
- `getSequenceStats(id)` → GET `/sequences/{id}/stats`; `listSends(id)` → GET `/sequences/{id}/sends` → `{sends:SeqSend[]}`
- `getSmtpStatus()` → GET `/smtp/status` → `SmtpStatus`; `updateSmtp(b)` → PUT `/smtp/config` → `{status:"ok"}` (403); `testSmtp(to_email)` → POST `/smtp/test` → `{status:"ok",message}` (403/400)
- `listSuppressions()` → GET `/suppressions` → `{suppressions:Suppression[]}`; `addSuppression(email)` → POST → `{status:"ok",added}` (403); `removeSuppression(email)` → DELETE `/suppressions/{email}` → `{status:"ok"}` (403 admin, 404, **403 `Cannot remove an unsubscribe/complaint suppression`** for locked)

**Automations** (`/api/automations`, flag `AUTOMATIONS_ENABLED` → all 404 when off):
- `listTriggers(p?:{enabled?:boolean;trigger_type?:TriggerType})` → GET `/triggers` → `Trigger[]`
- `createTrigger(b)` → POST `/triggers` → `Trigger` (403 admin; 422 invalid trigger_type/interval/action/condition/`re_enrich column not in scope`/`too many actions`/`max rules per workspace reached`; 409 `on_signal_requires_pg_lead_store` / `legacy_outreach_disabled`; 404/400 sequencer/send_email sub-errors)
- `getTrigger(id)` → GET `/triggers/{id}` → `Trigger & {recent_runs:TriggerRun[]}` (404)
- `patchTrigger(id, b)` → PATCH → `Trigger` (403); `deleteTrigger(id)` → DELETE → `{deleted:true}` (403)
- `pauseTrigger(id)` → POST `/triggers/{id}/pause` → `{enabled:false}` (403); `resumeTrigger(id)` → POST `.../resume` → `{enabled:true,next_run_at}` (403)
- `previewTrigger(id, b:{row_ids?:string[];limit?:number})` → POST `/triggers/{id}/preview` → `PreviewResult` (any member; **no 402** — returns matched/projected only)
- `runTrigger(id, b:{row_ids?:string[];dry_run:boolean})` → POST `/triggers/{id}/run` → `RunHandle{job_id,fire_key,dry_run}` (403 admin; **async — no run_id, no 402**)
- `listRuns(id, limit?)` → GET `/triggers/{id}/runs` → `TriggerRun[]`
- `getRun(runId)` → GET `/runs/{runId}` → `TriggerRun & {action_results:ActionResult[]}` (404)

**Watches** (`/api/watches`, flag `INTENT_POLLER_ENABLED` → 404; PG off → 409 on the *first* call):
- `listWatches(p?:{limit?:number;offset?:number})` → GET `""` → `{watches:Watch[],limit,offset}`
- `createWatch(b:WatchCreate)` → POST `""` → `Watch & {webhook_rule_id?:string}` (403 editor/admin; 422 invalid kind/interval/`signal_types … not valid for kind`/`max watches per workspace reached`/`webhook_url required`/`only http/https allowed`/`credentials not allowed`/`webhook_secret_ref not found`; 409 `automations_disabled_cannot_create_webhook_rule`)
- `getWatch(id)` → GET `/{id}` → `Watch`; `patchWatch(id, b)` → PATCH → `Watch` (403); `deleteWatch(id)` → DELETE → `{deleted:true,id}` (403)
- `pollWatch(id)` → POST `/{id}/poll` → `{status:"queued"|"already_queued",fire_key,job_id?}` (403; 409 `watch disabled`; **429 `poll-now rate limited`** or **429 `poll-now daily quota exhausted`** — distinct)
- `listWatchSignals(id, p?:{signal_type?;limit?;offset?})` → GET `/{id}/signals` → `{signals:WatchSignal[]}`

All read fns: `return jsonOrThrow(await fetch(url))`. Mutations: `fetch(url,{method,headers:{"Content-Type":"application/json"},body:JSON.stringify(b)})` then `jsonOrThrow`. **No auth/workspace headers** (interceptor handles it; web brief rule #2).

---

## 4. Component breakdown

### Shared / reusable
- **`StatusDot`** — promote the existing `outreach.tsx:580` helper into `apps/web/src/components/status-dot.tsx`. The current map covers `draft/active/paused/completed` only. **Extend with the full verified set**: sequence (`draft/active/paused/completed`), run (`queued/running/succeeded/failed/skipped`), send (`pending/scheduled/sent/opened/replied/bounced/failed/skipped/suppressed/completed`), watch (`enabled`→emerald, `disabled`→zinc). Unmapped statuses fall through to a neutral default. A single source-of-truth `STATUS_COLORS` constant is exported so the test in §11 can assert coverage against the literal status union.
- **`FeatureDisabled`** — new `apps/web/src/components/feature-disabled.tsx`: centered empty-state block (icon + title + copy + optional doc link), modeled on the empty states in `signals.tsx:164` and `outreach.tsx:194`. Variants: `flag-off` (404) and `dependency` (409). Used by Automations + Watches pages. Detects which to show by inspecting the thrown `ApiError.status` (404 vs 409) + `detail`.
- **`SignalFeed`** — extract the row-rendering loop from `signals.tsx:176-232` into `apps/web/src/components/signal-feed.tsx`. **Contract:** `signals: WatchSignal[]` where the type carries the full shape (`description`, `source_url`, `weight`, `read`, numeric `created_at`) — verified that the watches `/signals` endpoint returns exactly this via `_signal_to_dict`. The component still guards optional rendering (`description &&`, `source_url &&`) exactly as signals.tsx does, and treats `read` as `0|1` truthy. Keeps `SIGNAL_ICONS`/`SIGNAL_COLORS` maps and `formatTime`. signals.tsx is refactored to consume the same component (no behavior change).
- **`LeadPicker`** — dialog (`@/components/ui/dialog`) wrapping `useLeads` (hooks.ts:18) with tier/search filter + checkbox selection, returning `number[]`. Replaces the brittle `outreach.tsx:432` "fetch by tier then map ids" flow. The `consent_source` is chosen via a **separate Select next to the Enroll button** (a fixed set: `existing_customer`/`opt_in`/`event`/`other`), not inside the picker — keeps it explicit per the `EnrollLeadsRequest.consent_source` requirement.
- **`ConfirmDialog`** — thin wrapper over `@/components/ui/dialog` for delete/pause/real-run destructive confirms (preferred over `window.confirm` for the Run flow because it shows projected cost).
- **`useWorkspaceRole()` (new tiny hook in `hooks.ts`)** — returns `{ role: "owner"|"member"|"unknown", isOwner: boolean }` derived from `useAuth()`: compares the active workspace's `owner_id` to `user.id` to detect **owner** (the one role we *can* know client-side). It **cannot** distinguish admin/editor/member because `/api/workspaces` does not expose per-workspace role (verified). It is used only for *soft* hints (e.g., the member banner), never as the sole gate — the authoritative gate is the reactive 403 handler (§6). See open questions.

### Sequences (rewire `outreach.tsx`)
- `OutreachPage` (view machine `list|create|detail`) — keep, swap raw fetch for hooks.
- `SequenceList` (`outreach.tsx:177`) — keep; add `useDeleteSequence`.
- `SequenceCreator` (`outreach.tsx:263`) — extend form with `description`, `consent_basis` (`@/components/ui/select`), `daily_limit`, `send_window_start/end/tz` (Inputs). Send full `CreateSequenceRequest`.
- `SequenceDetailView` (`outreach.tsx:404`) — stats grid from `SeqStats`; add **Tabs** (`@/components/ui/tabs`): Steps | Sends log (`DataTable` over `SeqSend[]`) | Suppressions. Replace tier-only enroll with `LeadPicker` + consent Select. Render an **auto-paused banner** when `auto_paused` (or `bounce_count`/`complaint_count` high) with a `Resume` affordance that calls `updateSequence(id,{status:"active"})`/pause toggle (admin; 403 handled). Surface 400 detail on start with an inline "Configure SMTP in Settings" CTA → `/settings`.
- SMTP stays in `settings.tsx` `SMTPConfigTab` — rewire to `useSmtpStatus`/`useUpdateSmtp`/`useTestSmtp`.

### Automations (new)
- `AutomationsPage` — query `listTriggers`; on `ApiError.status===404` render `<FeatureDisabled variant="flag-off" feature="Automations"/>`.
- `TriggerList` — rows like `SequenceList`; status dot, `trigger_type` badge, `last_fired_at`, pause/resume/delete dropdown.
- `RuleBuilder` — sections: name; **trigger selector** (`Select` of 4 types; `on_schedule` reveals interval `Select`; `on_signal` shows an inline note that it needs PG and Save may 409 — we can't know PG state client-side, so this is informational, not a hard block); **scope** multi-select of workbooks **from `useWorkbooks()`** (existing `workbook-api.ts`); **condition** — see ConditionInput below; **ActionEditor** ordered list; caps (`max_spend_usd_per_day`, `max_actions_per_day`, `stop_on_error` `@/components/ui/switch`). Footer: **Dry-run preview** → `PreviewPanel`; **Save**; **Save & Run** (opens `RunConfirmDialog`).
- `ConditionInput` — `Input` plus an **"insert field ▾" dropdown** populated with column names from the selected scope workbooks' `columns_config` (via `useWorkbooks()`), and a static operator legend (`== != < <= > >= and or`). Live syntax check is left to backend; on 422 `invalid condition: …` the `detail` is shown **inline beneath the field** (not just toast). This removes the blind round-trip-422 problem.
- `ActionEditor` / `ActionConfigForm` — ordered list with up/down/remove. **Add-action dropdown lists only `re_enrich`, `push_crm`, `webhook` by default** (legacy decision below). Per-type config sub-forms, now concretely specified to match the backend config keys (verified):
  - `re_enrich` → `{ column_ids: string[] }` — multiselect of columns from the scoped workbooks (`useWorkbooks()` → `columns_config`). Client validates each chosen column belongs to a scoped workbook (mirrors backend 422 `re_enrich column not in scope`).
  - `push_crm` → `{ type: "hubspot"|… , field_map: Record<string,string> }` — a CRM `Select` (default `hubspot`) and a key→value mapping editor (add-row UI: CRM field name ↔ source column/token). No secrets shown.
  - `webhook` → `{ url: string, method?: "POST"|"GET"|… , body?: object, header_secret_ref?: string }` — URL Input (client-validate http/https, reject embedded credentials, mirroring backend 422s), method Select, optional JSON body textarea, and a `header_secret_ref` **Select of references** (never a raw secret; the value is a reference name). If no secret-ref source exists in the app, render the field as a plain ref-name Input with helper copy.
- **Legacy actions (`sequencer`/`send_email`) — single coherent decision:** because there is **no flags endpoint** to learn `AUTOMATIONS_ALLOW_LEGACY_OUTREACH` client-side, we **omit these two from the add-action dropdown by default** and place a single muted footnote: "Email/Sequence actions are part of legacy outreach automation; enable `AUTOMATIONS_ALLOW_LEGACY_OUTREACH` to use them." There is **no "try anyway" control** (the prior contradiction is removed). If a trigger fetched from the server already contains a legacy action (created elsewhere), the editor renders it **read-only with that explanation** rather than dropping it. Any 409 `legacy_outreach_disabled` from an edit still surfaces via toast.
- `PreviewPanel` — renders `PreviewResult`: `matched`, `projected_total_usd` (formatted USD), sample table of rows with per-action `would_charge_usd` + `resolved_payload_preview`. **Cap check is client-side:** if `projected_total_usd > (trigger.max_spend_usd_per_day ?? rule form value)` and a cap is set, color the total `text-destructive` and show "over daily cap" — there is no server 402. No writes.
- `RunConfirmDialog` — opened by **Save & Run**. `dry_run` defaults **ON** (checkbox). Shows the latest `projected_total_usd` from the most recent preview (prompts "Run a dry-run preview first" if none). Unchecking `dry_run` for a real run requires explicit confirmation; if projected is over cap, the confirm button is gated behind a second "I understand" check.
- `TriggerDetail` — header + `recent_runs` list (`RunList`).
- `RunList` / `RunDetail` — `RunDetail` uses `getRun(run_id)` → counts header (matched/attempted/succeeded/skipped/failed, `total_charged_usd`) + `DataTable` over `ActionResult[]`. In-flight runs (`status` queued/running) auto-refetch (§5).
- **Save & Run → RunDetail transition (redesigned — run returns NO run_id):** `runTrigger` resolves to `RunHandle{job_id, fire_key, dry_run}`. We do **not** navigate straight to a `RunDetail` by id (the `TriggerRun` row may not exist yet → 404 race). Instead, after a successful run we (1) toast "Run queued", (2) switch to `TriggerDetail` and **invalidate + poll `listRuns(triggerId)`**, (3) locate the new run by matching `fire_key === handle.fire_key` (the run's `fire_key` is the same `manual:{trigger_id}:{iso}` value). Once a matching `TriggerRun` appears, the row is auto-selectable and `RunDetail` opens against its real `id`. Until then the runs list shows a "starting…" pending row keyed by `fire_key`. This eliminates the open-run-404 race.

### Watches (new)
- `WatchesPage` — query `listWatches`; 404 → `FeatureDisabled variant="flag-off"`; 409 `intent_poller_requires_pg_lead_store` → `FeatureDisabled variant="dependency"`.
- `WatchList` — rows: `kind` badge, `target`, `interval`, `next_poll_at`, `consecutive_failures`/`last_error` warning chip, enabled dot; dropdown pause/enable/delete (editor/admin; 403 handled).
- `WatchBuilder` — `kind` Select → drives allowed `signal_types` checkbox set (per-kind map verified `watches.py:41`; **feed allows only `news`**); `target` Input; `interval` Select; optional `lead_id`; `create_webhook_rule` switch revealing `webhook_url` (note: 409 `automations_disabled_cannot_create_webhook_rule` if automations flag off — surface exact string). **signal_types default behavior:** the UI initializes the checkbox group to **all allowed types checked** for the kind (matching backend's `sorted(allowed)` default). If the user unchecks everything, the **Create button is disabled** with helper text "Select at least one signal type" — we never send an empty `signal_types` array (avoids ambiguous backend behavior).
- `WatchDetail` — header with **Poll now**, pause/enable/delete; renders `SignalFeed` from `listWatchSignals`. Poll-now handles the full result set: `queued` (toast success), `already_queued` (toast info "Already queued"), 409 `watch disabled` (toast + suggest Enable), **429 distinguished**: `poll-now rate limited` → "Rate limited — try again shortly"; `poll-now daily quota exhausted` → "Daily poll quota reached — resets tomorrow" (so the user doesn't retry forever).

---

## 5. Data fetching & state

Confirmed stack: **`@tanstack/react-query`** (provider in `main.tsx`, `queryClient` in `query-client.ts`; defaults staleTime 30s, retry 1, refetchOnWindowFocus). Use style #1. Note `useWorkbooks()`/`workbook-hooks.ts` already exist and are reused for scope/columns — no new workbook hooks needed.

**Query keys** — add to `queryKeys` factory (`query-client.ts:15`):
```ts
outreach: { all:["outreach"], sequences:["outreach","sequences"],
  sequence:(id:string)=>["outreach","sequence",id],
  sends:(id:string)=>["outreach","sends",id], smtp:["outreach","smtp"],
  suppressions:["outreach","suppressions"] },
automations: { all:["automations"], triggers:(f?:object)=>["automations","triggers",f??{}],
  trigger:(id:string)=>["automations","trigger",id],
  runs:(id:string)=>["automations","runs",id], run:(rid:string)=>["automations","run",rid] },
watches: { all:["watches"], list:(p?:object)=>["watches","list",p??{}],
  detail:(id:string)=>["watches","detail",id], signals:(id:string)=>["watches","signals",id] },
```

**Hooks** — add to `hooks.ts` mirroring `useLeads`/`useUpdateStatus`/`useJobs`:
- Reads: `useSequences`, `useSequence(id)`, `useSequenceSends(id)`, `useSmtpStatus`, `useSuppressions`; `useTriggers(filters)`, `useTrigger(id)`, `useRuns(id)`, `useRun(runId)`; `useWatches`, `useWatch(id)`, `useWatchSignals(id)`; plus `useWorkspaceRole()` (§4).
- Mutations with `onSuccess` invalidation: create/update/delete/start/pause/enroll/execute (invalidate `outreach.sequences` + `sequence(id)`); SMTP (invalidate `outreach.smtp`); suppression add/remove (invalidate `suppressions`); trigger create/patch/delete/pause/resume/run (invalidate `triggers` + `trigger(id)` + `runs(id)`); watch create/patch/delete/poll (invalidate `list` + `detail(id)` + `signals(id)`).
- **Every mutation hook attaches a shared `onError`** that maps `ApiError` per §7 (especially 403 → role message). This guarantees no silent failure even where the calling component forgets a handler.
- **Preview is not cached** — `useMutation` returning `PreviewResult`; re-running re-computes; not stored in query cache.
- **In-flight polling:** `useRun`/`useRuns` set `refetchInterval` based on status (mirror `useJobs` hooks.ts:122-134): poll ~4–5s while any run `status ∈ {queued,running}`, else `false`. After a successful `runTrigger`, poll `useRuns(triggerId)` until the `fire_key` match appears (§4 transition). After `pollWatch` queues, briefly poll `useWatch`/`useWatchSignals`.
- **Refetch over optimistic** for all mutations (matches existing leads mutations) — these aren't high-frequency. Exception: pause/resume/enable toggles may optimistically flip `enabled`/`status` then invalidate.
- **Workspace switching:** keys are workspace-agnostic — rely on page remount; no manual cross-workspace invalidation.
- **SSE:** optionally extend `useSSE` (hooks.ts:185) to invalidate `automations.runs`/`watches.signals` on `run_completed`/`signal_detected` events **if** the backend emits them; otherwise interval polling covers liveness. (Open question.)

---

## 6. Gating / Flag / Role UX (critical)

| Condition | HTTP / detail | Where | UX |
|---|---|---|---|
| `AUTOMATIONS_ENABLED` off | 404 `automations disabled` | every `/api/automations/*` | `AutomationsPage` → `<FeatureDisabled variant="flag-off" feature="Automations">`. Nav item stays visible; never blank. |
| `INTENT_POLLER_ENABLED` off | 404 `intent poller disabled` | every `/api/watches/*` | `WatchesPage` → `<FeatureDisabled variant="flag-off" feature="Intent Watches">`. |
| `PG_LEAD_STORE` off (watches) | 409 `intent_poller_requires_pg_lead_store` | watches first call | `<FeatureDisabled variant="dependency">` "Intent Watches require the Postgres lead store (`PG_LEAD_STORE`)." — **distinct copy** from 404. |
| **Non-admin on admin mutation** | **403** | outreach create/update/delete/start/pause/execute/smtp/suppression; **all** automations mutations | Surface a clear inline + toast: "This action requires the **admin** role for this workspace." Keep form data. Because per-workspace role isn't known client-side (verified: `/api/workspaces` lacks `role`), we **render the write controls but handle 403 reactively**. Soft hint: if `useWorkspaceRole()` says the user is **not the owner**, show a small "Some actions may require admin" note on the page header. |
| **Non-editor on watch mutation** | **403** | watches create/patch/delete/poll | Same pattern, message: "This action requires the **editor or admin** role." |
| **Member enroll vs admin start asymmetry** | — | outreach detail | Show a persistent member-facing banner (when not owner): "You can view and enroll leads; **activating/sending requires admin**." Prevents the enroll-then-blocked dead-end the review flagged. |
| `on_signal` trigger w/o PG | 409 `on_signal_requires_pg_lead_store` | `POST /triggers` | RuleBuilder shows an **informational** note for `on_signal` (we cannot proactively know PG state). On 409, `toast.error(detail)` + keep form so user can switch trigger type. |
| legacy `sequencer`/`send_email` action | 409 `legacy_outreach_disabled` (or 422/404/400 sub-errors) | `POST /triggers` | Legacy actions **omitted from the add dropdown** by default (single coherent decision, §4) with a footnote. Existing legacy actions render read-only. Any 409/422/404/400 sub-error still surfaces `detail` via toast. |
| send_email/sequencer sub-errors | 422 `sequence_id required`, 404 `sequence not found`, 400 `SMTP not configured`, 400 `OUTREACH_FOOTER required` | `POST /triggers` | Mapped to inline/toast `detail` (only relevant when legacy enabled & a legacy action is present). |
| sequence start SMTP/consent/footer missing | 400 `SMTP not configured` / `consent_basis required before activating` / `OUTREACH_FOOTER (physical address) required` | `/sequences/{id}/start` | toast `detail`; SMTP case adds inline "Configure SMTP in Settings" CTA → `/settings`. |
| watch create webhook-rule, automations off | 409 `automations_disabled_cannot_create_webhook_rule` | `POST /watches` | Match this **exact** string; inline message under the `create_webhook_rule` switch: "Webhook rules need Automations enabled." (Spec previously used wrong strings — corrected.) |
| max rules / max watches per workspace | 422 `max rules per workspace reached (N)` / `max watches per workspace reached (N)` | create | toast `detail` + inline "Workspace limit reached." |
| poll-now disabled | 409 `watch disabled` | `POST /{id}/poll` | toast + suggest Enable. |
| poll-now rate-limited | **429 `poll-now rate limited`** | poll | "Rate limited — try again shortly." |
| poll-now quota exhausted | **429 `poll-now daily quota exhausted`** | poll | "Daily poll quota reached — resets tomorrow." (distinct; user won't retry forever) |
| **Spend cap (triggers)** | **none — no HTTP 402** | preview/run | **Client-side only:** PreviewPanel compares `projected_total_usd` to the rule's `max_spend_usd_per_day` and colors it destructive when over; RunConfirmDialog requires explicit confirm for over-cap real runs. Actual overruns surface later in `TriggerRun`/`ActionResult` results, not as an HTTP error. (The previous 402 row was dead code and is removed.) |
| locked suppression delete | 403 `Cannot remove an unsubscribe/complaint suppression` | DELETE suppression | toast "Locked (unsubscribe/complaint) and cannot be removed." Distinguished from the role-403 by `detail` string. |

`FeatureDisabled` chooses its state from the page query's thrown `ApiError` (`status` 404 vs 409, plus `detail`).

---

## 7. Validation & error handling

**Form validation (client, `zod` in stack):**
- Sequence: name required; ≥1 step; each step subject+body non-empty; `consent_basis` required; `daily_limit>0`; `0≤send_window_start<send_window_end≤24`.
- Rule: name required; valid `trigger_type`; `on_schedule` requires `interval`; `re_enrich` `column_ids` must each be within scope workbooks' columns (from `useWorkbooks()`; mirrors backend 422 `re_enrich column not in scope`); ≥1 action; `webhook.url` http/https & no embedded credentials; condition syntax deferred to backend with `detail` shown **inline** under the field.
- Watch: `kind` valid; `target` non-empty; `signal_types ⊆` per-kind allowed set and **non-empty** (Create disabled if empty); valid `interval`; if `create_webhook_rule` then `webhook_url` required (http/https, no credentials).

**Server error surfacing (replace `catch {}`):** all mutations route through `ApiError`; the shared `onError` (§5) maps status→UX:
- **403** → distinguish by `detail`: locked-suppression string → "Locked…" message; otherwise role message ("requires admin" / "requires editor or admin"). Keep dialog/form open.
- **409** → distinguish by exact `detail`: `on_signal_requires_pg_lead_store`, `legacy_outreach_disabled`, `automations_disabled_cannot_create_webhook_rule`, watch `watch disabled`, `intent_poller_requires_pg_lead_store` → targeted inline message + toast.
- **404** at page level → `FeatureDisabled`; item-level (deleted sequence/trigger/watch, or run-not-found) → `toast.error` + return to list (the run-fire_key transition in §4 prevents the open-run race).
- **422** → toast `detail`; field-identifiable cases (condition, signal_types, webhook url, column scope, max-rules/watches) annotate the field.
- **429** (poll) → branch on the two distinct details (rate-limited vs quota-exhausted).
- **400** → toast `detail`; sequence-start SMTP case adds Settings CTA.

**Per-view states (every view handles all three):** loading → skeleton (`@/components/ui/skeleton`) or "Loading…" matching `outreach.tsx:191`; empty → empty-state block (icon+copy+CTA, like `signals.tsx:164`); error → flag/dependency 404/409 → `FeatureDisabled`, else inline error row + retry (and toast). React Query `isLoading/isError/data?.length===0` drive these.

---

## 8. Design system, responsiveness, a11y

- **Components:** only `@/components/ui/*` (card, button, input, label, textarea, select, switch, checkbox, dialog, dropdown-menu, tabs, separator, badge, skeleton, tooltip, scroll-area, pagination) + app `@/components/data-table.tsx` for Sends log & Action Results. Icons: lucide with `size-N`. Toasts: `import { toast } from "sonner"`.
- **Density / style:** match existing — `p-4` page container, header `text-base font-semibold` + `text-xs text-muted-foreground` subtitle, `<Separator/>`, stat row, `text-xs/text-[11px]` rows. Reuse `StatusDot`.
- **Responsiveness:**
  - Stat grids `grid-cols-2 md:grid-cols-5`; builder forms `max-w-2xl`/`max-w-3xl`; tables wrap in `overflow-x-auto`.
  - **Caps row** uses `grid grid-cols-1 sm:grid-cols-3 gap-3` so the three caps stack on narrow screens (the prior overflow-x note covered only tables).
  - **Action rows** in `ActionEditor` use `flex flex-col gap-2 md:flex-row md:items-start`: on narrow screens the inline config and the up/down/remove buttons wrap below the action label instead of overflowing.
  - Page root sits inside App's scroll container (App.tsx:364); use internal scroll for long lists.
- **a11y:**
  - Every input has a `<Label htmlFor>`; icon-only buttons have `aria-label`/`title`.
  - **Grouping for screen readers:** the ordered action list, the scope multiselect, and each per-kind `signal_types` checkbox group are wrapped in `<fieldset>` + `<legend>` (the legend visually styled as the section label) so AT announces them as groups, not loose inputs.
  - **Action reorder a11y:** up/down arrow buttons each have explicit `aria-label` ("Move action {n} up/down"), are disabled at list ends (`aria-disabled` + `disabled`), and **focus follows the moved item** (after reorder, focus returns to the same logical control on the moved row) so keyboard users keep context. `@dnd-kit` (in deps) is **not** used here — arrow buttons are the canonical, fully keyboard-accessible pattern.
  - Dialog/DropdownMenu/Tabs (Radix shadcn) provide focus trap + ARIA + keyboard nav.
  - Destructive items use `text-destructive`; the legacy-action footnote and the role-403 messages are real text (not tooltip-only) so they're always reachable by AT.

---

## 9. Edge cases

- **Empty lists** — dedicated empty states with primary CTA (create sequence/rule/watch).
- **Long lists / pagination** — Sequences/Triggers small (list fine); Sends log + Action Results + watch Signals can be large → `@/components/ui/pagination` + server `limit/offset` (watches signals + watches list support `limit/offset`; runs support `limit`).
- **In-flight / paused runs** — `RunDetail` polls while queued/running; spinner + live counts; paused triggers show `enabled:false` dot and disabled Run button.
- **Dry-run cost preview** — always offer Preview before Run; show `projected_total_usd`; over `max_spend_usd_per_day` → destructive color + explicit confirm in `RunConfirmDialog`; `dry_run` defaults ON. (No server 402.)
- **Save & Run → RunDetail** — resolved via `fire_key` matching, not a returned id (§4); shows a pending "starting…" row until the `TriggerRun` materializes.
- **Destructive confirms** — delete sequence/trigger/watch and pause active sequence via `ConfirmDialog` (or `confirm()` for simple cases). Locked suppression delete → blocked with explanation (403 string).
- **Consent** — enroll requires `consent_source` (explicit Select); block enroll if empty. Block/guide start if sequence `consent_basis` empty (would 400). Auto-paused sequences show warning banner + Resume.
- **Member/role dead-ends** — member banner on outreach detail (enroll-yes / start-no); reactive 403 everywhere keeps forms intact.
- **Webhook secret** — never displayed; `webhook_secret_ref`/`header_secret_ref` are reference names only, masked.
- **Watch failures** — `consecutive_failures>0` / `last_error` shown as warning chip on row + detail.
- **WatchSignal completeness** — feed reuses full signals.tsx layout safely because `/signals` returns the complete dict; optional fields still guarded (`description &&`, `source_url &&`).

---

## 10. File-by-file change list (exact `apps/web` paths)

**Modify**
- `src/App.tsx` — import `AutomationsPage`, `WatchesPage`, `Radar`; add 2 `NAV_ITEMS`; add 2 `<Route>`s; add 2 `getTitle()` cases.
- `src/lib/api.ts` — add `ApiError`, `jsonOrThrow`; all types (§3.1) and fetch methods (§3.2) for outreach/automations/watches.
- `src/lib/hooks.ts` — add query + mutation hooks (§5) with shared `onError` mapper; add `useWorkspaceRole()`; optional SSE event types.
- `src/lib/query-client.ts` — extend `queryKeys` with `outreach`/`automations`/`watches` factories.
- `src/pages/outreach.tsx` — rewire raw fetch → hooks; fix `CreateSequenceRequest`, `EnrollLeadsRequest` (`consent_source`), `/execute` `{enqueued}` shape; add Sends log + Suppressions tabs; use `LeadPicker` + consent Select; auto-paused banner + Resume; member banner; surface 400/403 detail.
- `src/pages/settings.tsx` — `SMTPConfigTab` rewire to `useSmtpStatus`/`useUpdateSmtp`/`useTestSmtp` (raw fetch at `:624,639,664`); handle 403.
- `src/pages/signals.tsx` — refactor row loop to consume the shared `SignalFeed` (no behavior change).

**Add**
- `src/pages/automations.tsx` — `AutomationsPage` + `TriggerList`, `RuleBuilder`, `ConditionInput`, `ActionEditor`, `ActionConfigForm`, `PreviewPanel`, `RunConfirmDialog`, `TriggerDetail`, `RunList`, `RunDetail`.
- `src/pages/watches.tsx` — `WatchesPage` + `WatchList`, `WatchBuilder`, `WatchDetail`.
- `src/components/status-dot.tsx` — promoted from `outreach.tsx:580`; exported `STATUS_COLORS` covering all verified status strings.
- `src/components/feature-disabled.tsx` — flag-off / dependency states.
- `src/components/signal-feed.tsx` — extracted from `signals.tsx:176`; `WatchSignal[]` contract.
- `src/components/lead-picker.tsx` — dialog over `useLeads` for enroll.
- `src/components/confirm-dialog.tsx`.

*(No new workbook API/hooks file — reuse existing `src/lib/workbook-api.ts` + `src/lib/workbook-hooks.ts` `useWorkbooks()`.)*

---

## 11. Test / verification plan

- **Typecheck + build:** `npm run build` (= `tsc -b && vite build`) must pass — strict TS, no `any` in public fns. The `SeqStats` type now lists explicit known keys (the index sig only covers extras), so a typo'd known stat key fails typecheck.
- **Lint:** `npm run lint` clean.
- **Status coverage check:** `STATUS_COLORS` is exported as a single object keyed by the literal status union; a tiny compile-time assertion (`satisfies Record<StatusKey,string>`) guarantees every known status (run: queued/running/succeeded/failed/skipped; send: pending/scheduled/sent/opened/replied/bounced/failed/skipped/suppressed/completed; sequence: draft/active/paused/completed; watch: enabled/disabled) is mapped — caught by `tsc`, not just runtime.
- **Component tests:** **no test runner is configured** (no vitest/jest in `package.json`, no `*.test.*`). Adding a harness is a noted follow-up (Vitest + RTL, Vite-native). **If/when added**, prioritize the riskiest, hardest-to-eyeball logic: (a) `FeatureDisabled` 404-vs-409 branch; (b) the `ApiError`→UX mapper for 403 (role vs locked-suppression by `detail`), the two 429 details, and the 409 detail strings; (c) `RuleBuilder` validation incl. re_enrich column-in-scope; (d) the `fire_key` run-matching transition; (e) `WatchBuilder` empty-signal_types guard.
- **Manual QA checklist:**
  1. Flags ON, **admin**: create/edit/delete Sequence; full `CreateSequenceRequest` persists; enroll via LeadPicker sends `consent_source`; start with SMTP unconfigured → toast 400 + Settings CTA; configure SMTP → start succeeds; Sends log + Suppressions render & paginate; add/remove suppression; **locked suppression delete → 403 "Locked…" (not the role message)**.
  2. **Non-admin member (outreach):** member banner shows; can enroll; start/execute/create/delete → **403 "requires admin"** inline+toast, form intact. **Non-editor member (watches):** create/poll/delete → **403 "requires editor or admin"**.
  3. Automations ON, admin: build each trigger type; `on_schedule` requires interval; scope picker lists real workbooks; re_enrich column picker restricted to scoped columns; condition insert-field works and a bad condition shows inline 422; dry-run preview shows matched + `projected_total_usd`; set a low `max_spend_usd_per_day` so preview total goes destructive; **Save & Run (dry) → runs list shows pending row by fire_key, then resolves to a real RunDetail that polls to terminal**; pause/resume; delete confirm; trigger `max rules` 422 message.
  4. **Flags OFF:** Automations → flag-off screen (not blank); Watches w/ poller off → flag-off; Watches w/ PG off → **dependency** screen (distinct copy).
  5. Legacy actions absent from add-dropdown; a server trigger containing a legacy action renders read-only with explanation; any 409 `legacy_outreach_disabled` surfaces `detail`.
  6. Watches ON + PG ON: create per kind with correct signal_types (**feed only `news`**); uncheck all signal_types → Create disabled; poll now → queued; second immediate poll → `already_queued` info; disabled watch poll → 409 `watch disabled`; rate-limited → "try again shortly"; **quota-exhausted → "Daily poll quota reached" (distinct)**; webhook-rule with automations off → 409 `automations_disabled_cannot_create_webhook_rule` inline; signal feed renders full layout.
  7. Workspace switch → page remounts with correct tenant data; no header code anywhere (interceptor only).
  8. a11y: keyboard-navigate builder; fieldset/legend groups announced; arrow reorder keeps focus on moved row; dialogs trap focus; icon buttons have labels; caps row + action rows reflow on a narrow viewport.

---

## 12. Acceptance criteria (numbered, testable)

1. Sidebar shows **Automations** and **Watches** nav items; routes `/automations`, `/watches` render their pages; `getTitle()` returns correct titles.
2. All new data access goes through `api.ts` fetch fns → `hooks.ts` hooks → `query-client.ts` keys (reusing existing `useWorkbooks()` for scope/columns); **no** component sets `Authorization`/`X-Workspace-Id` headers.
3. Creating a sequence sends a complete `CreateSequenceRequest` (incl. `consent_basis`, `daily_limit`, send-window fields); enroll sends `EnrollLeadsRequest{lead_ids,consent_source}`; `/execute` result is read as `{enqueued}`.
4. Sequence detail shows per-status stats from `SeqStats`, a paginated **Sends log** (`/sends`), and **Suppressions** management with locked-suppression 403 handled distinctly from role 403.
5. Rule builder produces a valid trigger (trigger + condition + ordered actions with real per-type config keys + workbook scope + caps + stop_on_error); `on_schedule` enforces an interval; `re_enrich` columns are validated against scoped workbooks.
6. Dry-run **Preview** calls `POST /preview`, displays `matched`, `projected_total_usd`, and per-action `would_charge_usd` with **no writes/charges**, and colors the total destructive when it exceeds the rule's `max_spend_usd_per_day` **client-side** (no reliance on a nonexistent HTTP 402).
7. Run inspection: after **Save & Run** the UI resolves the new `TriggerRun` by `fire_key` (run returns no `run_id`), then `RunDetail` shows counts + `total_charged_usd` + `action_results` and auto-refreshes until terminal — with no open-run 404 race.
8. Watch builder restricts `signal_types` to the per-kind allowed set (feed→only `news`), defaults all-checked, blocks empty submission, and produces a valid `WatchCreate`; **Poll now** handles `queued`/`already_queued`, 409 `watch disabled`, and the **two distinct 429** reasons with separate copy.
9. When `AUTOMATIONS_ENABLED`/`INTENT_POLLER_ENABLED` are off (404), the pages render a feature-disabled screen — never blank.
10. When `PG_LEAD_STORE` is off, Watches renders a **dependency** screen distinct from flag-off (driven by 409 `intent_poller_requires_pg_lead_store`).
11. Legacy `sequencer`/`send_email` actions are omitted from the add-action menu with an explanatory footnote (single coherent decision, no "try anyway"); pre-existing legacy actions render read-only; any 409 `legacy_outreach_disabled` surfaces the backend `detail`.
12. **403 role errors** on every outreach/automations/watches mutation surface a targeted "requires admin"/"requires editor or admin" message (distinguished from the locked-suppression 403) with the form/dialog preserved; the outreach member banner pre-warns about the enroll-vs-activate asymmetry.
13. Error responses 403/409/404/422/429/400 surface their exact `detail` (correct strings: `on_signal_requires_pg_lead_store`, `legacy_outreach_disabled`, `automations_disabled_cannot_create_webhook_rule`, `watch disabled`, `poll-now rate limited`, `poll-now daily quota exhausted`, `max rules/watches per workspace reached`) via toast/inline — never silently swallowed (`catch {}` removed from all new/rewired flows).
14. Every view renders explicit loading, empty, and error states.
15. `StatusDot` maps all verified status strings (compile-time asserted) with a neutral default fallback.
16. `npm run build` (tsc + vite) and `npm run lint` pass with no errors.

### Out of scope
- Public unsubscribe pages / bounce webhooks — server/token-auth, not SPA.
- Adding a test runner / CI (none exists) — noted follow-up; manual QA (incl. role paths) is load-bearing in the interim.
- New backend endpoints — including a **feature-flags discovery endpoint** and a **per-workspace-role field on `/api/workspaces`** (both would materially improve proactive gating; see open questions).
- Realtime SSE for runs/signals beyond optional invalidation (depends on backend emitting those events).
- Rich HTML email editor (steps stay subject + body textarea).

---

## Changes after review (summary)

1. **403 role gating (blocking):** Verified every outreach admin mutation, all automations mutations, and watch editor mutations enforce roles. Added full reactive 403 UX across all mutations (distinct "requires admin" vs "requires editor/admin" vs locked-suppression messages), a shared `onError` mapper so nothing fails silently, an outreach **member banner** for the enroll-yes/activate-no asymmetry, and a soft owner-based hint. Documented the hard constraint that per-workspace role is **not** exposed client-side (only `user.is_admin`/`user.role` global + workspace `owner_id`), so gating is reactive, not proactive.
2. **402 spend-cap (blocking):** Removed entirely — verified preview/run never raise 402 and run is async. Cap enforcement redesigned as a **client-side** `projected_total_usd` vs `max_spend_usd_per_day` comparison in PreviewPanel + RunConfirmDialog.
3. **Workbooks API (blocking):** Corrected — `workbook-api.ts`/`useWorkbooks()` already exist with `columns_config`. Scope multiselect, re_enrich `column_ids` picker, and column-in-scope validation are concretely specified by reusing them (no new backend, no ugly free-text IDs).
4. **Run→RunDetail race (blocking):** Redesigned around `fire_key` matching since `POST /run` returns `{job_id,fire_key,dry_run}` and no `run_id`; pending row shown until the `TriggerRun` materializes.
5. **Corrected exact strings:** `automations_disabled_cannot_create_webhook_rule`, `on_signal_requires_pg_lead_store`, `watch disabled`, and the **two** 429 reasons (`poll-now rate limited` / `poll-now daily quota exhausted`) now distinguished; added `max rules/watches per workspace` and the sequencer/send_email sub-errors to the matrix.
6. **Builder substance:** Specified `ConditionInput` (field-insert + operator help + inline 422), per-type `ActionConfigForm` keys (`re_enrich.column_ids`, `push_crm.{type,field_map}`, `webhook.{url,method,body,header_secret_ref}`), and a single coherent legacy-action decision (omit + footnote, no "try anyway").
7. **WatchBuilder:** feed→only `news` corrected; signal_types default all-checked + empty-submission guard.
8. **SignalFeed contract:** Verified `/signals` returns the full signal dict; `WatchSignal` type expanded (description/source_url/weight/read, numeric created_at) so the reused layout is safe.
9. **a11y/responsive:** fieldset/legend grouping, arrow-reorder aria + focus management, and explicit mobile reflow for the caps row and action rows.
10. **Tests/coverage:** `SeqStats` explicit known keys; compile-time `STATUS_COLORS` coverage assertion; manual QA now exercises non-admin/non-editor paths, the fire_key transition, the webhook-rule 409 string, and both 429 reasons.

---

## Open questions for the owner

1. Per-workspace role is NOT exposed client-side: `/api/workspaces` (workspace_manager.py `_serialize`) returns only `owner_id`, and `AuthUser` carries a single global `role`/`is_admin` — neither reflects the caller's role *in the active workspace* (admin vs editor vs member). This forces all role gating to be reactive (handle 403 after the fact) rather than proactively disabling write controls. Should the backend add the caller's per-workspace role to the workspaces list response so we can disable/hide write affordances up front? (Currently out of scope: no backend changes.)
2. There is no feature-flags discovery endpoint, so the UI cannot proactively know AUTOMATIONS_ENABLED, INTENT_POLLER_ENABLED, PG_LEAD_STORE, or AUTOMATIONS_ALLOW_LEGACY_OUTREACH state — all are inferred reactively from 404/409 (and legacy actions are simply omitted). Is exposing a lightweight read-only flags endpoint acceptable to make on_signal/PG and legacy-action affordances proactive instead of trial-and-error?
3. Does the backend emit SSE event types for run completion (`run_completed`) and signal detection (`signal_detected`)? If yes we can invalidate `automations.runs`/`watches.signals` via `useSSE` instead of relying solely on interval polling; if not, polling stands. Need confirmation of the exact event names.
4. For `webhook` actions, `header_secret_ref` (and watch `webhook_secret_ref`) are reference names validated against a backend secret store (422 `… not found`). Is there an endpoint to *list* available secret refs so we can render a Select instead of a free-text ref Input? Without it we fall back to a plain ref-name Input with helper copy.
5. The `fire_key`-based Save&Run→RunDetail transition assumes the `TriggerRun` row eventually carries the same `fire_key` the run endpoint returned (`manual:{trigger_id}:{iso}`). Please confirm the worker persists that exact `fire_key` onto the created `TriggerRun` so the client can reliably match it; if the worker rewrites/derives a different key, we need the mapping.
6. `push_crm` config shape: the preview path reads `config.type` (default 'hubspot') and `config.field_map`. Is the full set of supported CRM `type` values and the expected `field_map` key space documented anywhere so the mapping editor can offer the right CRM-side field names rather than free text?
