# OpenGTM V2 delivery plan

Created: 2026-09-06. Status: implementation started; live competitive validation pending.

This is an internal engineering plan, not a release announcement. The goal is to
compete with Clay on complete GTM outcomes, reliability, usability, and economics.
Parity is a benchmark goal, not a proven percentage. Do not publish or deploy as
part of this plan without the release decision.

## Product outcome

A user can describe a target market, find accounts and the right people, verify
claims and contacts, save the exact results, monitor changes, and prepare or
deliver approved outputs. Each step preserves identity, evidence, execution
history, and cost. A worker restart or provider failure must produce a recoverable
state rather than a frozen interface or a false success message.

The competitive baseline is [dated separately](competitive-baseline-2026-09-06.md).
The existing [95 recovery plan](../opengtm-95-recovery-plan.md) remains the source
for G1–G7 contracts and its controlled-live release gate.

## Evidence and current status

PostgreSQL checkpoint (2026-09-23): with a local PostgreSQL 18.6 cluster
(`TEST_DATABASE_URL`), the full suite passes with 1,919 passed and 8 skipped;
all 8 skips are opt-in live-network tests. This is the first run of the 112
PG-gated tests (RLS isolation, worker/tenancy, MCP, outreach, seed) in this
environment. New PG tests under the non-superuser FORCE-RLS app role verify:
20 concurrent spend reservations never exceed the workbook cap (exactly 5 of
20 at a 5-slot cap), concurrent replays create one reservation, exactly one of
10 dispatchers is authorized, spend receipts are tenant-isolated, and 12
concurrent imports of one domain converge on one entity with all 12
observations (fails 3/3 without the row lock). Single-host test cluster only;
not a production load or multi-host result.

First live smoke runs (2026-09-23), LLM-only (no paid contact lookup), API +
standalone worker on PostgreSQL 18 as the non-superuser runtime role with
FORCE RLS, dedicated `gtm-release-eval` workspace, isolated worktree. No run
has passed yet; each failure was real and is recorded, not retried away:

1. Find → verify → workbook proposal → approved save all succeeded (7 people,
   workbook persisted), then the follow-up turn failed: Google rejected the
   configured key as leaked (403). Fixed: chat now fails over on 401/403.
2. `ok: true` with 0 people: every public search had errored and the errors
   were swallowed. Fixed: `people_search_unavailable` / `search_failures`.
3. Client ReadTimeout: the stream was silent for up to 45s during research.
   Fixed: SSE keepalive comments during tool calls.
4. `people_research_timeout` (truthful): no SearXNG backend is configured, so
   search falls back to the `ddgs` library, which is slow/failing from this
   host. Open: needs a reliable search backend before live runs can pass.

Also found by starting the real worker on PostgreSQL: every job claim failed
("could not determine data type of parameter"), leaving all jobs pending on
PostgreSQL deployments since 1f9c73c, including `main`. Fixed in 66fac17.

Run locally (throwaway cluster; PostgreSQL via mise):
`initdb -D <dir> -U postgres --auth=trust`, start with
`pg_ctl -D <dir> -o "-p 55432 -c unix_socket_directories='' -c listen_addresses=127.0.0.1" start`,
`createdb -h 127.0.0.1 -p 55432 -U postgres gtm_test`, then
`TEST_DATABASE_URL=postgresql+psycopg://postgres@127.0.0.1:55432/gtm_test env -u DATABASE_URL APP_ENV=test uv run pytest tests -q`.

Latest local regression checkpoint (2026-09-23): the full backend suite passes
with 1,786 passed, 120 skipped and two deprecation warnings (50.27 seconds), via
`env -u DATABASE_URL APP_ENV=test uv run pytest tests -q`. All 60 frontend and
route-payload reporter tests pass via
`bun test apps/web/tests scripts/vendor/twenty-ui/route-payload.test.mjs`.
These runs use disposable test databases and fixtures,
not live vendor outcomes or PostgreSQL concurrency acceptance. The recent
execution-safety implementation and its remaining limits are tracked in
[workbook spend and batch recovery](workbook-spend-reservations.md); the
[Twenty migration plan](twenty-ui-migration-plan.md) still has open accessibility,
payload and end-to-end release gates. Neither V2 nor the UI migration is complete.

| Item | Status | Evidence or remaining work |
|---|---|---|
| Existing workbook, sourcing, research, output, and queue capabilities | Implemented baseline, not a V2 parity claim | Repository README describes capabilities and remaining boundaries |
| G1–G7 recorded-provider execution | Completed historical validation | Recovery plan records ten independent local-native passes; this does not establish current live quality |
| Controlled-live G1–G7 release streak | Blocked in last recorded validation, 0/10 | Recheck PostgreSQL-backed Intent Watches and exact finder/independent verifier configuration before spending |
| Exact workbook provider selection and ordering | In progress | Exact order, no default expansion, and explicit empty/unknown/unavailable outcomes are regression-tested offline; skipped providers persist in cell metadata/row JSON and show in the cell tooltip (not yet in live WebSocket updates) |
| Bounded retries, fenced worker ownership, cancellation | In progress | First V2 tranche; requires stale-worker/restart/cancellation checks |
| Durable claims and replay for exact contact actions | In progress | SQLite tests cover concurrent claim, restart replay, contract drift, reorder, expiry-to-uncertain and timeout; not yet raced on PostgreSQL |
| Authenticated HTTP live smoke runner | In progress | Dedicated workspace; real Chat find → verify → save → retry → readback, optional paid exact-contact stage, bounded streams/timeouts, and evidence artifacts |
| Live gauntlet execution runner | Planned | Existing scorer accepts artifacts; it does not itself drive the whole live product |
| Expanded V2 capabilities below | Planned | No completion claims until implementation and appropriate evidence exist |

Update this table after implementation evidence is reviewed. Never replace a
blocked live result with a recorded fixture or mark a workstream complete because
its API exists.

The HTTP smoke runner is a useful subset and must not count toward the full
controlled-live G1–G7 release streak. No local API or worker listeners were found
in the current inspection; real execution requires starting the configured stack
and checking the dedicated workspace and provider dependencies first.

Runtime prerequisite recheck (2026-09-22): `ss -ltnp` finds the static UI preview
on 127.0.0.1:4399, but no listeners on 4099, 8000, 5432 or 6379. `psql` and
Docker clients exist; `postgres` and `initdb` are not on PATH and no server tools
were found under /usr/lib/postgresql or /opt. `docker ps` is denied access to
/var/run/docker.sock. This is a current local-runtime limitation, not evidence
about production or proof that no remote services exist. No permissions were
changed and no live smoke was attempted. Before controlled-live validation,
restore an authorized API/worker/Redis/PostgreSQL test stack and verify the
dedicated evaluation workspace. Continue local work without counting fixtures
toward the live release streak.

## Delivery sequence and acceptance criteria

### Phase 0: reliable execution and truthful baseline (P0)

1. Preserve exact provider selection and user ordering across configuration,
   execution, and receipts. An explicitly selected provider cannot silently fall
   back to a different provider. Empty, unknown, and unavailable selections have
   explicit outcomes. Attempt history identifies every actual provider call.
2. Bound retry counts and backoff, distinguish transient and permanent failures,
   fence stale worker writes, and persist cancellation. Killing a worker before
   or after a checkpoint must not lose committed work or duplicate an output.
   A worker whose lease expired cannot overwrite the new owner's result.
3. Claim exact contact actions durably before provider work. Repeated and
   concurrent requests with the same contract reuse persisted state. Reusing an
   action key with different person selection or inputs fails explicitly.
4. Build the live gauntlet runner through authenticated product paths. Record
   input, build SHA, workspace, provider health, persisted IDs, evidence links,
   action traces, timings, costs, and readback. Never accept self-reported success
   without state assertions. Require an explicit controlled test environment and
   enforce blocked external sends for the release scenarios.
5. Provision or verify a dedicated `gtm-release-eval` workspace, PostgreSQL,
   worker/scheduler health, Intent Watches, exact finder credentials, and the
   independent email verifier. Missing dependencies produce a blocked report.

Gate: relevant regression and integration checks pass; one controlled-live smoke
run completes with truthful failures/partials; then ten unique G1–G7 live passes
meet the existing >=95 score, category floors, and all hard gates. No unresolved
P0/P1 or duplicate writes. This gate validates those workflows, not all of V2.

### Phase 1: shared account/person intelligence (P0)

Progress (2026-09-23), offline evidence only:

- Company identity is deterministic: `company_identifiers` makes
  `(workspace, kind, value)` unique, exact domains resolve before fuzzy
  matching, and concurrent creates converge on one entity (savepoint +
  re-read). Merge moves identifiers to the kept entity; split restores them.
  The migration backfills existing domains (oldest owner wins; legacy
  duplicates are left for review, never auto-merged).
- Fixed two pre-existing defects found by the new tests: profile/platform
  hosts (facebook.com pages, Linktree, Google Maps, etc.) counted as an exact
  domain match, auto-merging unrelated same-city businesses (score 0.90 >= 0.85);
  and concurrent observations lost provenance through an unlocked JSON
  read-modify-write (now row-locked before append).
- Source materialization resolves entities in the run's workspace, matching
  the rows that reference them.
- Threaded SQLite test: 8 concurrent imports of one domain yield one entity
  with all 8 sources. On PostgreSQL under FORCE RLS: 12 concurrent imports
  converge with all observations, and identifiers are tenant-isolated.
- Merge now repoints every stored account reference: row `data.account_id`
  (read first by signal tracking) and account-group watches (accounts,
  per-account polling cursors, collector health, scope key; the kept
  account's cursor wins when both were tracked). Split restores them.
  Verified on SQLite and PostgreSQL under FORCE RLS. Audiences and
  destinations key on lead IDs, not entity IDs, so merges do not affect them.
- Persisted people: `person_identifiers` (unique LinkedIn profile, verified
  email, or legacy id per workspace) and `person_employments` (dated history
  per person and company; most recent observation is current). A job change
  keeps the person. Chat `person_…` and workbook `person:…` ids are legacy
  aliases, so earlier actions and idempotency keys are unchanged. Chat
  "create people workbook" and the People Search source persist people and
  stamp rows with `canonical_person_id`. Verified on SQLite and PostgreSQL
  (10 concurrent saves converge under FORCE RLS).
- Not started: job-change detection over employment history, claim-level
  evidence, entity-backed segments, durable chat selections, and chat
  selections keyed by canonical person ids.

- Introduce or consolidate canonical account and person identities shared by
  chat, workbooks, contact enrichment, signals, and destinations. Preserve aliases,
  source identifiers, employment history, and merge lineage.
- Store claim-level evidence with source, observation/retrieval timestamps,
  confidence basis, expiry, and contradictions. Reverification appends history;
  it does not silently replace an old claim with an unsupported conclusion.
- Build dynamic segments over canonical fields and relationships. Changes update
  memberships and schedule only the affected dependent work.
- Preserve exact selections through follow-up turns, including corrections,
  exclusions, partial results, and references to older saved results.

Gate: entity continuity across every G1–G7 step; no duplicate canonical account
for the same normalized identity in concurrent imports; merge tests preserve all
provenance; stale/conflicting claims are visible; held-out paraphrases and
correction turns do not select the wrong people.

Dependencies: Phase 0 execution contracts and tenant-scoped persistence.

### Phase 2: reusable workflows and measurable economics (P0/P1)

- Version workflows with typed node inputs/outputs, explicit dependency bindings,
  draft/publish/restore, reusable functions, and per-step run inspection. Pin runs
  to the version they started with. Expose the same contracts through UI, chat,
  REST, and MCP rather than separate execution implementations.
- Add deterministic conditional routing, bounded agent steps, durable waits,
  event triggers, pause/resume, and retry from failure with output idempotency.
- Tenantize the reusable-function catalog and scope connections to authorized
  users/actions. Review dependencies before publishing function changes.
- Reserve budgets atomically before concurrent paid work; reconcile actual
  provider/LLM usage after completion. Show estimated versus actual spend, cache
  savings, yield, and cost per accepted result. Label unknown prices explicitly.
- Maintain a capability catalog reporting supported action, required credentials,
  health, provider constraints, normalized output, and live validation status.

Gate: version edits do not change in-flight runs; stale references fail before
spend; replay cannot duplicate activation; concurrent jobs cannot exceed the
configured reservation policy; every paid attempt has an attributable ledger
entry. Provider counts include only tested actions, with their validation tier.

Dependencies: Phase 1 canonical IDs; Phase 0 fencing and replay.

### Phase 3: recurring intelligence and activation (P1)

- Connect signal events to canonical accounts, deduplicate event identities, and
  preserve source time versus detection time. Show cadence, last success, next
  attempt, and recovery controls.
- Maintain account research memory with bounded refresh, evidence expiry, and
  changed-conclusion inspection. Support selected BYOK models with recorded cost.
- Deliver reliable HubSpot/Salesforce and webhook synchronization first, followed
  by Sheets/Airtable, warehouses, and sequencer destinations according to usage.
  Include field mapping previews, upsert keys, conflict rules, delivery receipts,
  and reconciliation of ambiguous remote responses.
- Build reusable plays: inbound qualification/routing, CRM refresh, job-change
  reactivation, account expansion research, and signal-triggered qualification.
- Keep drafting, approvals, suppression, and sending distinct. Sending requires
  appropriate recipient eligibility, authorization, and a durable send ledger.

Gate: duplicate or delayed events trigger no duplicate activation; disconnected
destinations recover without data loss; each play passes an approved live
scenario with documented cost and outcome. Test external writes only against
designated test destinations, never ordinary customer records by default.

Dependencies: Phase 2 versioned execution and economic controls.

### Phase 4: scale, operations, and product completion (P1)

- Move remaining workspace/control-plane SQLite state to PostgreSQL before
  claiming multi-host availability. Finish tenant scoping of legacy utilities.
- Add connection access controls, audit export, secret redaction, backup/restore
  drills, and deployment health diagnostics. Hosted exposure additionally needs
  the documented egress, key-management, and security-review work.
- Validate workbook editing, dependency recomputation, imports, filters, keyboard
  operation, and recovery UI on realistic large datasets. Establish measured
  latency/memory targets on a declared hardware profile before claiming scale.
- Ship starter plays and connector setup checks that bring a fresh workspace to
  a first useful result. Measure onboarding completion and manual interventions.
- Run held-out competitive benchmarks and external pilot workflows; use observed
  gaps to prioritize integration breadth and collaboration polish.

Gate: successful restore drill, cross-tenant negative tests, documented load-test
envelope, accessible critical workflows, and no unresolved critical production
issues. Publish only supported claims with a reproducible evidence artifact.

## Twenty-person operating model

This is a staffing/workstream model for the requested team-level effort, not a
claim that twenty agents or employees are running. Actual parallel execution is
limited by available agents, shared-file ownership, and dependency readiness.

| Workstream | People equivalent | Ownership and deliverable |
|---|---:|---|
| Product and GTM operators | 2 | ICPs, representative plays, held-out task briefs, outcome review |
| Core runtime and reliability | 3 | Queue, leases, retries, cancellation, workflow versioning |
| Data platform | 3 | Entities, claims, provenance, segments, migrations |
| Research and agent behavior | 3 | Planning, tools, conversational continuity, bounded research |
| Providers and activation | 3 | Connector health, waterfalls, exact verification, CRM/output delivery |
| Product design and frontend | 2 | Tables, workflow/run inspection, setup, accessibility |
| Quality and evaluation | 2 | Live runner, fault injection, competitive benchmark, release evidence |
| Platform and security | 2 | Tenancy, secrets, deployment, restore, observability |

Each implementation slice has one owner, a separate reviewer, acceptance
evidence, migration/rollback notes where applicable, and an explicit status.
Prefer narrow completed vertical slices over simultaneous partially connected
surfaces. Do not assign multiple writers to the same files without coordination.

## Live benchmark methodology

1. Freeze a dated build and scenario set. Include G1–G7 and the five additional
   plays above, common paraphrases, explicit exclusions, and realistic failure
   cases. Keep development fixtures separate from held-out evaluation examples.
2. Use the same starting records, selection constraints, observation window, and
   budget for Clay and OpenGTM. Run both native best-configuration and matched
   provider comparisons where possible; disclose inaccessible provider features.
3. Have reviewers independently label account fit, current employment/function,
   claim support, and contact status against accessible evidence. Resolve
   disagreements and preserve the rubric. Unavailable evidence is not a pass.
4. Report precision, useful coverage, evidence completeness/freshness, exact
   selection retention, task completion, human interventions, p50/p95 latency,
   retry recovery, duplicate actions, and all-in cost per accepted result.
   Separate infrastructure, vendor, model, and platform costs.
5. Report sample counts and uncertainty; repeat across geographies and company
   sizes. A failed or partial run remains in the denominator. Do not hide missing
   credentials or inflate success by excluding difficult scenarios after running.
6. Preserve machine-readable raw results and a reviewable report with build,
   workspace, provider configuration fingerprints (never secrets), run IDs, and
   evidence references. The ten-run release streak is necessary for the initial
   contracts but is not statistical proof of market-wide parity.

## Scope and non-goals

V2 includes the research-to-activation loop, reusable execution, transparent
economics, and the operational foundation needed to trust those features.
Full incumbent feature parity is an evolving backlog governed by measured user
outcomes. Native ad buying, every marketplace provider, a billing business,
multi-region hosting, and unsupported massive-scale claims are not prerequisites
for the first useful V2 release. They require their own scoped acceptance plans.

Do not rewrite the functioning stack wholesale, expose public multi-tenant SaaS
before its prerequisites, claim benchmark superiority without comparative runs,
or publish internal reports in user-facing documentation. External outreach and
production deployment remain separate from implementation and test authorization.
