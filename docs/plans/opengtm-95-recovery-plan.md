# OpenGTM 95% recovery plan

Status: active implementation

Created: 2026-08-28

Release posture: do not publish yet

## Progress log

### 2026-08-28: grounded outreach drafts

- Added the approval-gated `draft_grounded_outreach` Chat action for the accepted G7 follow-up. It selects one exact person from a server-stored contact result; a caller cannot substitute an unsaved email or identity.
- Verified addresses require a recorded valid verifier attempt. Risky addresses require separate explicit approval. Generic inboxes, role accounts, unrelated functions, missing public evidence, and undated evidence fail before a draft is written.
- Personalized subject and body lines carry saved claim IDs plus public source URL, observation time, and confidence. The deterministic copy uses no unsourced company fact.
- Draft writes are workspace-scoped, read-back confirmed, and idempotent under ordinary retries and concurrent races. A reused action key with a changed contact contract is rejected.
- Draft persistence is physically separated from outreach sends. The draft action does not import a sender, exposes no draft create/send HTTP route, creates no send ledger row, and always returns `send_performed: false` for a passing run.
- Added tenant-scoped read APIs and made `/outreach?draft=...` a functional deep link. Outreach now lists saved grounded drafts and lets the user inspect the recipient, message, claim IDs, observation dates, confidence, and source links. No send control appears on the draft screen.
- Extended the native trace adapter and gauntlet to evaluate G7. Hard gates catch contact drift, generic recipients, fabricated verification, unapproved risky addresses, ungrounded personalized sentences, false persistence, duplicate retries, cross-workspace readback, and any send during a draft-only request.
- All seven workflows now have executable scorer contracts. The native G7 slice scores 100/100 with no hard failure.
- Verification after the slice: 1,202 backend tests passed, 119 skipped; frontend lint and production build passed; a clean SQLite migration upgraded to the single head and `alembic check` reported no schema drift.

Next: assemble the combined G1 through G7 native validation artifact, run the ten-run local-native streak, repair any failures, and keep release blocked until the controlled live streak is independently recorded.

### 2026-08-28: exact account signal tracking

- Added the approval-gated `track_account_signals` Chat action for the accepted G6 follow-up. It binds `these accounts` to stable account IDs read from the saved workbook in the same conversation. Missing or drifted selections fail closed.
- One `account_group` watch now owns the exact account scope, cadence, and requested collectors for partnership hiring, leadership changes, funding, and pricing-page changes.
- Schedule writes are idempotent by exact scope and action key. Repeating the request returns the existing schedule; changing cadence or collectors updates that schedule and cancels its obsolete queued occurrence.
- Chat reports success only after reloading the saved schedule. The receipt includes account IDs and count, cadence, collectors, next run, state, attempts, last error class, next retry, manual retry action, schedule ID, and an exact deep link.
- The account-group poller uses the existing single-flight queue, restart bootstrap, bounded retries, and backoff. Collector health is recorded per account and signal type. Pricing-page fetches use the shared SSRF guard and persist only normalized fingerprints.
- Tracking refuses to claim an active schedule when Intent Watches or its Postgres lead-store dependency is disabled.
- The Watches page now honors `/watches?id=...` deep links and keeps list, create, detail, and browser-back state in the URL. Account-group schedules and their four signal labels render through the existing detail screen.
- Extended the native trace adapter and gauntlet to evaluate G6. Hard gates catch false schedule persistence, account-scope drift, duplicate retry schedules, cross-workspace readback, and failed collectors without recovery metadata.
- Recorded product-code coverage is now 6/7 workflows. The combined G2, G4, G5, and G6 recorded slice scores 100/100 with no hard failure. G1 and G3 remain independently green from their checkpointed slices.
- Verification after the slice: 1,187 backend tests passed, 119 skipped; frontend lint and production build passed.

Next: implement G7 evidence-grounded, persisted draft-only outreach with sentence-level sources and a hard separation from sending.

### 2026-08-28: structured account discovery

- Added a deterministic G1 sourcing-brief parser for explicit account requests. The server now retains requested count, company type, geography, technology, hiring criteria, exclusions, and evidence requirements as structured data.
- Explicit complete requests route directly to an approval-gated sourcing action without depending on an LLM. Incomplete briefs fail closed with the missing fields.
- Made account sourcing workbooks idempotent by workspace and action key. A retry reuses the workbook and source job; a conflicting brief with the same key is rejected.
- Added truthful queue recovery. If enqueueing fails, Chat reports that the workbook exists but sourcing did not start; a retry can enqueue the persisted source column.
- Added an evidence gate in the workbook source engine. Accepted rows require a canonical domain, criterion-level matches, an evidence URL, retrieval time, and confidence. Technology and hiring claims cannot be inferred from a company name or website alone.
- Source completion now records requested, delivered, shortfall, rejected reasons, exhausted source stages, and retry options. The engine stops at the requested total across reruns instead of adding the target count again.
- Added a native G1 trace adapter and scorer coverage. Hard gates catch unsupported account fit, duplicate domains, false complete runs, false persistence, wrong links, duplicate retries, and cross-workspace action state.
- Recorded product-code coverage is now 5/7 workflows across G1 through G5. The current G1 and G2 through G5 slices each score 100/100 with no hard failure. Release remains blocked on G6, G7, and the 10-run production-like streak.
- Verification after the slice: 1,176 backend tests passed, 119 skipped.

Next: implement G6 exact signal tracking with one idempotent schedule, durable job health, and read-back receipts.

### 2026-08-28: exact-person contact enrichment

- Added the approval-gated `enrich_people_contacts` Chat action. It operates on stable person IDs from a server-owned research result and rejects unknown selections before provider spend.
- Restricted exact-person discovery to Prospeo and Hunter. Generic Hunter domain-search contacts, mismatched names, and non-company email domains are rejected.
- Separated discovery from deliverability verification. Finder claims never receive verified status unless the verification cascade returns a valid mailbox result.
- Normalized contact results to verified, risky, catch-all, invalid, or unavailable. Every result records provider order, attempt outcomes, source licenses, observation time, and exhaustion state.
- Added retry-safe action IDs. Repeating the same approved action reuses the stored result without another provider call, while reusing a key for a different selection fails closed.
- People workbooks now snapshot contact status, finder, verifier, observation time, and provider attempts from the exact Chat result.
- Extended the gauntlet and native trace adapter to evaluate G3. Added hard gates for selection drift, verified contacts without a valid verifier attempt, non-exact discovery labeled verified, unexplained unavailable results, and duplicate contact retries.
- Current recorded product-code baseline: 100/100 with G2, G3, G4, and G5 passing and no hard failure. The release remains blocked at 4/7 workflows and 0/10 production-like passing runs.
- Verification after the slice: 1,163 backend tests passed, 119 skipped.

Next: implement and score G1 structured account discovery, then connect its exact company selection to G6 signal tracking and G7 grounded drafting.

### 2026-08-28: evidence-backed company identity

- Added conservative company resolution for people research. Explicit domains resolve directly; named companies require exactly one exact organization match and an official website from Wikidata.
- Ambiguous names, lookalikes, missing websites, timeouts, and resolver errors stay unresolved. They do not silently become canonical domains and do not prevent an honest partial people result.
- Canonical domain and resolution evidence now survive verification and exact workbook persistence.
- The recorded product-code baseline moves from 87/100 to 97/100. G2 now passes. G4 remains failed because contactability is correctly unavailable without a G3 enrichment trace, leaving accuracy and evidence at 88%, below the 90% category floor.
- Verification after the slice: 1,151 backend tests passed, 119 skipped.

Next: implement G3 exact-selection contact enrichment with attempt history and normalized verified, risky, catch-all, invalid, or unavailable outcomes.

### 2026-08-28: first product-code gauntlet baseline

- Added a native Chat trace adapter that converts research, verification, approval, and workbook action results into the gauntlet artifact contract.
- Workbook success is checked against the actual `workbooks` and `workbook_rows` records. The adapter does not trust the action receipt by itself.
- Added an isolated recorded runner test that uses the real approval resolver and executes the real `create_people_workbook` action twice.
- The recorded provider data is explicitly synthetic, but the Chat approval, idempotency, persistence, read-back, trace adaptation, and scoring paths are production code.
- Current product-code baseline: 87/100 with no hard failure. G5 passes. G2 fails canonical company resolution, and G4 fails because contactability has no evidence-backed claim yet.

Next: add evidence-backed canonical company resolution to the people research result, then add the G3 contact enrichment action so contactability can be scored instead of remaining unavailable.

### 2026-08-28: Phase 0 gauntlet kernel

- Added a deterministic JSON artifact scorer for the G2 to G4 to G5 partnership workflow.
- Encoded the 30/25/15/15/10/5 release weights and the 90% per-category floor as executable checks.
- Added hard gates for wrong-company people, unsupported or contradicted verified claims, former or unrelated people labeled verified, false persistence, duplicate retries, frozen jobs without recovery metadata, unapproved external writes, wrong result links, and cross-workspace state.
- Added an anti-cheat boundary: only G2, G4, and G5 can be marked evaluated until the other workflow scorers exist.
- Added a synthetic recorded contract fixture and 15 scorer tests covering correct, partial, fabricated, duplicate, missing-write, frozen-job, approval, link, and tenant-isolation outcomes.
- The contract fixture scores 100/100 for the implemented slice, while the release gate correctly remains closed at 3/7 workflows and 0/10 production-like runs. This validates the harness contract, not current product quality.
- Verification after the kernel: 1,144 backend tests passed, 119 skipped.

Run the slice locally:

```bash
uv run python scripts/run_gtm_gauntlet.py \
  --input tests/fixtures/gtm_gauntlet/partnership_people_pass.json
```

Use `--require-release` in the eventual release job. It intentionally exits nonzero until the full workflow and streak gates pass.

Next: capture the same artifact shape from the real Chat action trace and persisted workbook state, then record the first reproducible product baseline.

### 2026-08-28: first G2 to G5 trust slice

- Created restorable pre-implementation checkpoint `da04851`, tagged `checkpoint/opengtm-pre-95-20260828`.
- Added deterministic person IDs and result-set IDs to partnership-team research and verification.
- Added exact person-ID subset selection for Chat-created people workbooks.
- Added workspace-scoped action idempotency, backed by a database uniqueness constraint.
- Added persisted action receipts that distinguish a newly created workbook from a reused workbook.
- Added rejection of unknown person IDs before any write occurs.
- Verification after the slice: 1,129 backend tests passed, 119 skipped; frontend lint and production build passed; Alembic has one head.

The Phase 0 gauntlet kernel above now scores this contract. Connecting it to real action traces remains the next product-evidence step.

## Objective

Make OpenGTM complete real GTM work from Chat with trustworthy evidence, durable actions, and recoverable background jobs. Release only after the production workflow gauntlet scores at least 95/100 and clears every hard gate in this document.

This plan accepts the current product recommendations:

1. Build a machine-scored workflow gauntlet before adding more surface area.
2. Make Chat use stable internal actions instead of narrating around missing capabilities.
3. Treat every person, company, contact method, and signal as a set of evidence-backed claims.
4. Let users create exact workbooks from the people or companies already found in Chat.
5. Expose the existing enrichment and verification waterfall through those actions.
6. Make queued work idempotent, observable, retryable, and honest about partial failure.
7. Polish the interface only after the underlying workflow passes its contract.

## Baseline

The latest manual production-style review exposed a gap between available features and completed user outcomes. The product could find candidate people, but follow-up requests such as `verify them` and `make a workbook with them` were refused because Chat lost entity identity and lacked the required write actions. A collection could remain queued without an actionable progress explanation. Chat also presented low-value or incorrect rows as if they had passed verification.

Planning baseline:

| Measure | Current evidence |
| --- | --- |
| Workflow score | 37/100 in the latest production-style grilling pass |
| Focused backend checks | 105 passed, 1 skipped |
| Full backend suite | 1,125 passed, 119 skipped |
| Database migrations | One Alembic head |
| Publishing | Blocked by this release contract |

The passing test suite proves regression coverage, not user outcome quality. The new gauntlet must test the complete path from a natural-language request to persisted, inspectable work.

## Release contract

### Weighted score

| Category | Weight | What earns full credit |
| --- | ---: | --- |
| Outcome completion | 30 | The requested artifact exists and is usable without manual repair |
| Accuracy and evidence | 25 | Entities and claims match the target, cite evidence, and expose uncertainty |
| Actionability | 15 | Results can be selected, enriched, saved, exported, monitored, or drafted against |
| Reliability | 15 | Retries, timeouts, idempotency, partial results, and recovery behave correctly |
| Speed | 10 | Immediate acknowledgement, visible progress, and bounded completion time |
| UX clarity | 5 | Status, next actions, errors, receipts, and links are understandable |

Release requires all of the following:

- Weighted score of at least 95/100.
- At least 9/10 equivalent performance in every category.
- Seven of seven accepted workflows passing.
- Ten consecutive production-like gauntlet runs without a hard failure.
- No unresolved P0 or P1 issue in the accepted workflow path.

### Hard failures

Any one of these fails the run regardless of weighted score:

- A wrong-company person is presented as a valid match.
- A former employee or unrelated function is labeled current and verified.
- A claim marked verified lacks supporting evidence or contradicts its source.
- Chat says an action succeeded when the persisted state does not exist.
- A retry creates duplicate workbooks, schedules, rows, drafts, or sends.
- A user-visible job remains queued or running without a heartbeat, timeout, or recovery path.
- An external write or outreach send occurs without explicit user approval.
- A result link opens the wrong workspace, record, workbook, or job.
- Cross-workspace data is exposed.

## Accepted workflow gauntlet

Every scenario runs against recorded fixtures in CI and against live providers in a controlled staging run. Live provider degradation may produce an honest partial result, but it may not produce false certainty.

### G1. Account discovery

Prompt: `Find 20 B2B SaaS companies in India that use Stripe and are hiring partnership roles.`

Acceptance:

- The request becomes a structured sourcing brief with count, geography, company type, technology, and hiring criteria.
- The workbook contains 20 unique exact-fit companies when evidence exists.
- A partial result states the requested count, delivered count, exhausted sources, and retry options.
- Every row has normalized company name, domain, fit reasons, evidence URLs, retrieval time, and field-level confidence.
- Duplicate domains and companies outside the target geography or segment are rejected.

### G2. Partnership team mapping

Prompt: `Find Stripe partnership teams.`

Acceptance:

- Chat resolves `Stripe` to the intended company before sourcing people.
- Results require current-employment evidence and partnership-function evidence as separate claims.
- Each person retains a stable entity ID across later turns.
- Rows contain name, current title, location when available, public profile, evidence, retrieval time, and confidence.
- Former employees, wrong companies, legal-only roles, and unrelated uses of `partnership` are rejected or explicitly labeled uncertain.

### G3. Contact enrichment

Prompt: `Find work emails for the selected people and verify them.`

Acceptance:

- The selected entity IDs, not the previous message text, define the target set.
- The configured waterfall runs in policy order and records each provider attempt.
- A work email is labeled verified, risky, catch-all, invalid, or unavailable.
- Inferred addresses are never described as verified.
- Unavailable results show exhaustion evidence without leaking provider secrets.

### G4. Claim verification

Prompt: `Verify that they still work there and own partnerships.`

Acceptance:

- Employment, title, function, company identity, and contactability are independent claims.
- Each claim has status, source, observed time, confidence, and contradiction handling.
- Fresh authoritative evidence can supersede stale snippets without deleting the audit trail.
- Chat summarizes passed, failed, uncertain, and changed claims.

### G5. Exact workbook creation

Prompt: `Make a workbook with them.`

Acceptance:

- Chat creates a workbook from the exact selected person IDs.
- Row count and identity match the selection exactly.
- The action is idempotent when retried with the same action key.
- The success receipt includes workbook ID, row count, skipped count, and a correct link.
- Chat can continue enriching the created rows without rediscovering them.

### G6. Signal tracking

Prompt: `Track these accounts weekly for partnership hiring, leadership changes, funding, and pricing-page changes.`

Acceptance:

- One schedule is created for the selected accounts and four requested signal types.
- A repeated request updates or returns the existing schedule instead of duplicating it.
- Chat reads the saved schedule back before claiming success.
- The receipt states cadence, scope, signal types, next run, current state, and link.
- Failed collectors expose attempt count, last error class, next retry, and a manual retry action.

### G7. Grounded outreach draft

Prompt: `Draft a short partnership email to the best verified contact. Do not send it.`

Acceptance:

- The chosen contact has a verified or explicitly approved risky address.
- Personalization cites saved claims and does not invent company facts.
- Generic inboxes and role accounts are flagged before drafting.
- The draft is persisted and linked, but no message is sent.
- The user can inspect the evidence used in each personalized sentence.

## Workstreams and implementation order

### Phase 0: executable evaluation harness

Goal: turn the release contract into repeatable evidence.

Build:

- Add a gauntlet runner with scenario fixtures, deterministic IDs, seeded provider responses, persisted-state assertions, latency capture, and a JSON score report.
- Add adversarial fixtures for stale employment, namesake companies, former roles, conflicting titles, generic emails, zero-result providers, duplicate requests, worker restarts, and partial source failure.
- Capture action traces separately from model prose so scoring is based on state and evidence.
- Add a controlled live mode that uses a dedicated workspace, blocks external sends, and records provider health.
- Store run artifacts outside the source tree or in ignored test-output paths.

Primary areas:

- `tests/`
- `scripts/`
- `apps/api/routers/copilotkit.py`
- `apps/api/services/agent/autopilot.py`

Exit criteria:

- All seven scenarios execute end to end in recorded mode.
- The scorer can distinguish a correct result, honest partial result, fabricated claim, missing action, duplicate write, and frozen job.
- The current product receives a reproducible baseline score.

### Phase 1: Chat trust kernel

Goal: preserve identity and make Chat claims match persisted state.

Build:

- Introduce typed action envelopes with `action_id`, `workspace_id`, `conversation_id`, `actor_id`, `intent`, typed inputs, selected entity IDs, approval state, and idempotency key.
- Persist result-set handles and selections so `them`, `these accounts`, and `the best contact` resolve to stable IDs.
- Add action receipts containing state, object IDs, counts, links, warnings, and next actions.
- Require read-after-write confirmation before Chat says `created`, `tracking`, `verified`, or `sent`.
- Separate proposed, queued, running, partial, succeeded, failed, cancelled, and timed-out states.
- Centralize authorization, approval, and workspace checks for every write action.

Primary areas:

- `apps/api/routers/copilotkit.py`
- `apps/api/services/agent/autopilot.py`
- Chat persistence models and action registry
- `apps/web` Chat state and receipts

Exit criteria:

- The G2 result can flow through G3 and G5 without entity loss.
- Replaying a successful action does not create a duplicate.
- No success wording is emitted before persisted-state confirmation.

### Phase 2: structured sourcing briefs

Goal: convert ambiguous requests into inspectable search contracts.

Build:

- Define typed company and people briefs with required filters, optional filters, result count, exclusions, evidence policy, freshness, and stopping conditions.
- Show the interpreted brief in Chat and allow field-level correction without restarting.
- Plan source queries from the brief and record why each source was selected.
- Apply deterministic normalization, domain identity, deduplication, exclusion, and fit scoring before rows become accepted results.
- Return honest partial results when the evidence-qualified pool is smaller than the requested count.

Primary areas:

- `apps/api/services/workbook/source_engine.py`
- Lead sourcing services
- Workbook creation and row provenance
- Chat brief and progress components

Exit criteria:

- G1 passes exact-fit, deduplication, provenance, count, and partial-result assertions.
- Re-running the same normalized brief reuses prior evidence when still fresh.

### Phase 3: claim-based people research and exact subsets

Goal: stop treating a search snippet as a verified person.

Build:

- Model people results as entities plus independent employment, role, function, location, and profile claims.
- Add exact target-company matching using canonical domain and company aliases.
- Add current-employment and partnership-remit verification policies with contradiction rules.
- Keep rejected candidates and reasons in the trace while excluding them from the accepted set.
- Add `create_workbook_from_selection` for exact company or person IDs, with deterministic row identity and idempotency.
- Add `verify_selection` so follow-up verification operates on existing entities.

Primary areas:

- `apps/api/services/leadgen/targeted_people.py`
- Entity and evidence models
- Workbook row materialization
- Chat action registry

Exit criteria:

- G2, G4, and G5 pass.
- The PayPal or Stripe workflow cannot include a wrong-company or unrelated-function row as verified.

### Phase 4: enrichment and actionability

Goal: expose the product's existing enrichment depth as a coherent Chat operation.

Build:

- Route contact discovery and verification through the existing provider waterfall.
- Record provider attempts, license class, source, timestamps, validation result, and cost metadata.
- Keep discovery confidence separate from mailbox verification status.
- Add explicit exhaustion and policy-blocked outcomes.
- Let Chat add enrichment columns or run a predefined contactability recipe on an exact workbook selection.
- Produce a ranked best-contact decision with inspectable reasons.

Primary areas:

- `apps/api/services/workbook/enrichment.py`
- `apps/api/services/leadgen/enrichment/`
- Workbook cells, traces, and provenance
- Chat enrichment actions

Exit criteria:

- G3 passes with verified, risky, invalid, and unavailable fixture cases.
- The chosen G7 contact is traceable to a saved enrichment result.

### Phase 5: truthful jobs and signals

Goal: eliminate frozen collections and contradictory status.

Build:

- Standardize job states and transitions across collection, enrichment, refresh, and signal work.
- Add heartbeats, leases, bounded attempts, exponential backoff, timeout classification, cancellation, and dead-letter handling.
- Make enqueue operations transactional and idempotent.
- Reconcile abandoned running jobs after worker restart.
- Surface stage, completed units, total units when known, last heartbeat, last error class, next retry, and recovery action.
- Make schedule creation use a canonical scope and idempotency key, followed by read-after-write confirmation.

Primary areas:

- `apps/api/services/job_process_runner.py`
- `apps/api/services/workbook/refresh.py`
- Collection and signal workers
- Job and schedule APIs
- Chat and workbook status components

Exit criteria:

- G6 passes normal, retry, duplicate-request, provider-timeout, and worker-restart cases.
- No fixture can remain silently queued or running past its declared timeout.

### Phase 6: grounded drafts and functional UX polish

Goal: make the passing system easy to operate without hiding uncertainty.

Build:

- Add draft-only outreach action with explicit send separation and approval checks.
- Bind draft claims to workbook evidence and expose sentence-level sources.
- Replace indefinite spinners with stage, progress, elapsed time, and recovery actions.
- Put the primary next action beside each result: verify, enrich, save, track, draft, retry, or inspect evidence.
- Improve empty, offline, partial, error, and completed states across Chat, workbooks, jobs, and signals.
- Add keyboard, focus, responsive, contrast, and screen-reader checks to accepted workflow screens.
- Remove UI controls that do not perform a real action or clearly label them unavailable.

Primary areas:

- `apps/web`
- Outreach draft APIs and persistence
- Shared status and evidence components

Exit criteria:

- G7 passes without any external send.
- All seven workflows pass at supported desktop and mobile widths.
- The ten-run release streak reaches at least 95/100 with no hard failure.

## Delivery rules

- Work in vertical slices. Each change set should make one gauntlet assertion pass from Chat through persisted state and back to the UI.
- Add the failing scenario before changing implementation.
- Put provider-dependent behavior behind controlled adapters and recorded fixtures.
- Use feature flags for action routing or schema transitions that need a safe rollback.
- Keep migrations additive until the replacement path is proven and backfilled.
- Preserve an audit record when claims change; never rewrite stale evidence into apparent current truth.
- Never use model prose as the source of truth for action success.
- Do not spend release time on billing, marketplace breadth, visual novelty, or additional providers unless a gauntlet failure requires them.

## Scorecard artifact

Each gauntlet run should emit a human-readable summary and machine-readable JSON with:

- build SHA and environment;
- scenario and step IDs;
- requested and resolved entities;
- action IDs and idempotency keys;
- state transitions and timestamps;
- evidence and contradiction counts;
- persisted object IDs and validated links;
- provider attempts and classified failures;
- latency to acknowledgement, first useful result, and completion;
- category scores, hard failures, and final release decision.

## Verification commands

The exact gauntlet command will be added in Phase 0. The expected verification sequence is:

```bash
uv run --group dev python -m pytest -q
bun run --cwd apps/web lint
bun run --cwd apps/web build
uv run --group dev python scripts/run_gtm_gauntlet.py --mode recorded
uv run --group dev python scripts/run_gtm_gauntlet.py --mode staging --workspace gtm-release-eval
```

The staging runner must default to blocking outreach sends and any other external write not explicitly included in the scenario.

## First implementation slice

Start with G2 through G5 because they reproduce the clearest current failure:

1. Add recorded fixtures for the original PayPal partnership-team result, including wrong-function and stale-employment candidates.
2. Persist the accepted result set with stable person IDs.
3. Implement `verify_selection` with independent employment and function claims.
4. Implement `create_workbook_from_selection` with an idempotency key and exact row assertions.
5. Return action receipts to Chat and render working workbook links.
6. Add the same flow for Stripe and run it twice to prove deduplication.

This slice is complete only when the user can ask `find partnership teams`, `verify them`, and `make a workbook with them` in consecutive turns and receive the exact verified rows in a real workbook.
