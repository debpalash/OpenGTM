# OpenGTM 95% recovery plan

Status: active implementation

Created: 2026-08-28

Release posture: do not publish yet

## Progress log

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
