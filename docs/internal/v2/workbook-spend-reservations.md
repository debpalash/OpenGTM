# Workbook provider spend reservations

Status: implementation design, not a completed spend guarantee.

## Open execution defect: ambiguous AI batch outcomes

Code-path audit found three connected replay hazards (not fixed by the provider
lookup ledger):

- `LLMClient.batch_complete_anthropic` catches result-stream errors and returns
  the accumulated subset, including an empty dictionary. Its timeout path also
  proceeds to result retrieval without establishing a terminal vendor state.
- `_run_ai_batch_prepass` treats absent results and generic exceptions as a
  reason to execute those cells synchronously. An accepted batch may already
  have performed and charged for the same cells.
- `QueueService._execute_job` retries failed jobs while retry budget remains;
  merely propagating an exception does not prevent paid batch resubmission.

The 29 current Anthropic/batch tests pass, but they test successful mapping,
cooperative cancellation and generic fallback, not post-acceptance transport
failure, partial result streams or crash recovery. Do not cite those passes as
batch replay safety.

Required connected implementation: persist a scoped batch attempt and input
contract before dispatch; store vendor batch identity after acknowledgement;
distinguish pre-dispatch rejection from ambiguous submission; resume retrieval
for known vendor identities; preserve partial committed results; fence stale
writers; and prevent both per-cell fallback and queue resubmission for uncertain
attempts. Lost acknowledgements require explicit reconciliation, not a new
submission. Fault-injection acceptance must count actual mock create/sync calls
across retry/restart boundaries and verify persisted cell outcomes and exposure.

## Implementation checkpoints

Explicit no-call outcomes (2026-09-23): a waterfall cell that calls no provider
now reports why instead of generic `no_data`: `no_providers_selected` for an
explicit empty selection, `no_default_providers` when no default exists, and
`providers_unavailable: <provider> (<reason>)` listing unregistered
(`unknown_provider`), `cooldown` and `over_budget` exclusions in user order.
The planner reports exclusions through an optional `skipped` list without
changing selection or order. Three parameterized API cases assert no provider
call and the exact error. When some providers ran and others were skipped, the
cell still reports the ran-provider outcome; skipped entries are not yet in cell
metadata or the attempt history UI. Chat contact actions: the legacy-receipt
selection check is now order-insensitive, matching the sorted durable contract;
a reorder regression fails without the fix. Full backend suite: 1,786 passed,
120 skipped.

Batch retrieval telemetry deduplication: queued recovery passes checkpointed
request IDs to the client. Re-read answers still reach result validation, but
their token usage is not added again; repeated IDs within a single stream are
also counted once. Tests verify both persisted-usage calls and in-memory call
counts while retaining all answers, and partial-stream recovery forwards the
known IDs. This is not atomic invoice accounting: usage logging and result
checkpoint commits remain separate, leaving a crash window between them. A
durable usage ledger/reconciliation is still required for exactly-once economics.

Incremental batch checkpoints: the client invokes a result callback for each
successful answer; queued prepass stores it under the accepted lease before
continuing the stream. Checkpoints merge distinct request IDs, replay identical
answers and reject contradictory overwrites. A two-request fault test retains
the first answer across a stream failure, retrieves the existing vendor batch,
and applies both results without a second create. Client-level streamed-failure
coverage verifies callback ordering. The combined regression passes 149 tests;
all 26 batch-attempt tests pass with the additional merge/conflict test. Partial
answers remain checkpointed until the complete expected set is available; they
are not yet shown as partially completed workbook cells. Usage deduplication and
bounded result storage remain open.

Complete batch-result checkpoint: after validating the full expected result set,
queued prepass durably stores results on the accepted attempt before applying
cells. Same-contract retries reuse this checkpoint without another vendor read
or create. The checkpoint is lease-fenced and refuses contradictory overwrites.
A new fault case fails the first cell write after checkpointing, then confirms
retry applies the stored result with exactly one vendor invocation. This covers
complete downloads; interrupted partial result streams still need incremental
checkpoints. Job-payload result size/retention and telemetry reconciliation remain
open operational concerns.

Parent queue retry integration verified: fault tests exercise actual
`QueueService._process_job` finalization and `claim_next_job` around a mocked
child-process boundary. Both accepted and unacknowledged batch JSON survive the
pending transition and new lease. The retry retrieves the accepted vendor ID or
refuses an unacknowledged resubmission; each scenario counts one create only.
Unacknowledged work ends failed after the configured retry ceiling, while the
mock retrieval path completes. This verifies queue persistence/reclaim behavior,
not real subprocess crashes, vendor retrieval or database-remote concurrency.

Between-provider ownership checks: agent and waterfall loops now perform a short
lease preflight before each selected provider, including zero-cost providers
outside the spend ledger. Fault tests replace the worker during the first failed
lookup: neither loop invokes the second provider, and the newer workbook result
survives. Three AI/waterfall/agent stale-result checks pass; the broader prior
matrix passes 138 tests before the additional agent parameter. The preflight
does not eliminate the race between releasing the check and a remote call;
paid attempts retain their separate durable dispatch authorization.

Dispatch run-identity check: the guarded reservation update now also requires
`run_id == job:<active job id>`. A valid lease for another job in the same
workbook is not enough to dispatch an older reservation. The owner test matrix
includes this cross-run case and verifies its reservation stays reserved with
unchanged exposure; the matching job still dispatches. Workbook/spend/batch
regression suites pass together (137 tests).

Paid dispatch transition fencing: when queue ownership is present, the
reserved→dispatched transaction now checks/locks the active job lease before
changing attempt status. Cancelled, replaced and foreign-workspace owners cannot
authorize a call; their reservation remains intact. Valid ownership permits the
existing single-winner dispatch transition. Four new cases exercise these
outcomes and retained exposure. This fences authorization, not a remote vendor:
cancellation after committed dispatch remains an uncertain in-flight operation,
and free/non-ledger provider paths still need per-attempt checks.

Queued cell-entry guard: `_run_one_cell` verifies lease ownership in a separate
short transaction before invoking the cell implementation. Replaced/cancelled
owners do not enter provider/output logic. A valid-owner test performs an
independent database write inside execution to prove the entry lock was released
before external work; final result writes retain their separate transaction
fence. This is a preflight check, not atomic authorization across network I/O:
ownership can still change after the check, and multi-provider cells need
per-attempt dispatch protection.

Non-batch result commit fencing: the shared enrichment result-write block now
checks active queue ownership immediately before its budget/overlay writes,
after provider awaits, holding the check through commit. A real cell test changes
ownership during an AI call and writes a replacement result; the stale call is
rejected and rollback preserves the replacement JSON with no stale overlay row.
The fixture matches production autoflush=False. This does not fence earlier
external outputs, legacy lead-store writeback, agent trace writes, or provider
dispatch itself; it protects the shared final result transaction only.

Workbook state fencing: queue-owned running/progress/empty-completion writes now
check the active lease in the same transaction as the workbook update. Finalizers
that lost ownership skip both state writes and final-status broadcasts while
still closing Redis. Fault tests change worker ID, lease timestamp or cancel
the job during row work; all preserve the replacement's status and progress
without a stale broadcast. All 18 batch/ownership tests pass. This is not a
complete stale-worker guarantee: non-batch cell writes, dispatch-time fencing,
post-commit broadcast races and direct callers still require treatment.

Batch cell-write fencing: each queued batch cell transaction now locks/checks
the active job lease and matching accepted contract before writing the result,
holding that lock through cell commit. A fault test changes worker ownership
after vendor acknowledgement but before results return; the stale path performs
zero cell writes while preserving the vendor identity for recovery. Existing
retry/resume and batch checks remain green. This fences batch cell writes only:
workbook-level progress/finalization and non-batch cells still need equivalent
ownership protection, and PostgreSQL concurrency is not proven by SQLite tests.

Batch eligibility fencing: prepass checks the durable attempt before disabled,
missing-provider and below-threshold/changed-column exits. Existing attempts now
require recovery instead of falling into synchronous execution. Empty workbook
scope cannot silently mark an unresolved batch complete. The outer generic
prepass exception handler also checks for a claim before permitting fallback.
Five new fault cases forbid both batch submission and row execution after
eligibility changes and verify the original claim survives. All 14 batch-attempt
tests pass. This preserves safety by refusing changed inputs; automatic recovery
from an immutable stored request snapshot is still not implemented.

Queued batch integration: queue lease context now reaches the AI prepass. Before
submission, it commits a contract over requests/provider configuration (only the
hash is persisted). Acknowledgement stores the vendor ID. Same-contract retries
retrieve that ID without creating a new batch; missing acknowledgement raises a
reconciliation error. Strict retrieval propagates stream failures and polling
timeouts, and missing results do not fall through to synchronous cells. Both
prepass exception handlers preserve these errors. Fault-injection tests count
one create across lost-ack, read-error and partial-result retries; client tests
verify resume skips creation and strict stream errors propagate.

Remaining limitations: this is not full batch recovery acceptance. Direct calls
retain legacy behavior; changed batch eligibility/configuration can bypass the
prepass before it checks an existing attempt and must be fenced at run entry.
Partial results are re-fetched rather than durably checkpointed. Missing/errored
vendor cells require explicit resolution; repeated retrieval may double-count
usage telemetry. Vendor cancellation, stale cell-write fencing, PostgreSQL and
actual billing/exposure integration remain open. Queue retries can repeat reads,
but the tested same-contract prepass paths do not re-authorize creation.

Durable batch-claim foundation: `batch_attempts.py` records an input-contract hash
in the owning job's JSON before authorizing dispatch. Scoped active lease checks
and a short writer transaction allow only one competing caller to receive
`dispatch`; subsequent callers receive `reconcile` without a vendor identity or
`retrieve` with one. Acknowledgement is idempotent for the same vendor ID and
rejects changed IDs/contracts or stale owners. Reclaimed jobs retain the claim,
so an unacknowledged attempt is not reset to dispatch. Six file-backed SQLite
tests cover competition, readback, tenant/workbook/worker isolation and reclaim;
queue-related regressions pass together (16 tests). No raw prompts are stored.
This primitive is not yet wired into the batch client/prepass: the audited
duplicate-work defect remains open until integration and failure-path tests pass.
PostgreSQL concurrency and durable result storage remain unverified.

Batch-phase cancellation cleanup: the AI batch prepass now runs inside the same
protected finalization block as row execution. Cancelling an awaiting prepass
therefore preserves cancellation, records paused status/progress, broadcasts that
status and closes its Redis client. The cancellation regression covers both
phases and asserts database readback, broadcast and client cleanup. Related API,
batch and queue-reconciliation tests pass (102); both expanded cancellation
checks pass. This does not cancel a batch already accepted by an external vendor
or reconcile its eventual billing/results. Durable vendor batch recovery and
stale-worker fencing remain open.

Cancellation status correction: cancellation while the main row loop is awaiting
work now re-raises `CancelledError` but finalizes the workbook as paused with
actual processed-row progress, rather than complete/all rows. Fatal loop errors
use failed status; broadcasts use the same final status and respect an existing
pause. A regression cancels a live asyncio task after its row runner signals
entry, then reads back paused/zero completed rows. This is cooperative task
cancellation, not protection against SIGKILL, batch-prepass cancellation, or a
superseded worker finalizing a newer run; ownership fencing remains required.

Queued execution now fails closed before invoking a registered provider whose
price cannot be resolved, rather than letting the legacy zero fallback bypass
reservations. Agent traces return `agent_provider_price_unknown`; waterfall
cells return `provider_price_unknown`. Known zero-cost providers remain eligible;
known paid calls retain reservations. New tests forbid provider invocation for
five malformed agent prices and an unknown-price waterfall, asserting no attempt
receipt for those workbooks. Direct/nonqueued execution and broader workflow
cost coverage remain separate gaps; this is not a global hard-cap guarantee.

Malformed provider-price handling: null, booleans, empty/non-numeric strings,
negative values and NaN/infinity no longer become registered zero-cost or
non-JSON-safe estimate values. Declared-cost lookup treats them as unavailable;
known catalog entries retain their fallback and otherwise estimates report the
provider as unknown. Explicit finite zero and numeric strings remain supported.
Thirteen parameterized cases plus catalog/workbook/spend/accounting regressions
pass (133 tests). Execution still retains the legacy unknown-provider zero-cost
fallback; this checkpoint improves estimate honesty, not hard-cap coverage.

Run-review integration: unknown provider names now appear in an announced warning
with a pricing-review next step. Legacy responses without coverage explicitly
warn that zero is not evidence of a free run. Client validation rejects malformed
or contradictory coverage fields. The warning does not prevent an intentional
run or represent server-side spend enforcement. Twenty review browser fixture
covers the warning alongside responsive themes, conflict recovery, start and stop;
22 client tests, production build and lint pass. UI/UX error-recovery guidance
informed the announced message and next step.

Catalog unknown-price diagnostics: run estimates now include per-column and
aggregate `unknown_providers`, plus `catalog_complete` for the supplied provider
lists only. Unregistered providers absent from the catalog are no longer
indistinguishable in the response from registered zero-cost providers. Numeric
totals still cover known costs only, and the note explicitly disclaims a spend
ceiling. These additive diagnostics are not yet consumed by the run-review UI;
complete workflow-cost coverage and user-facing approval treatment remain open.
Catalog completeness does not mean research, AI, output or other omitted column
types have been priced.

Estimate-selection consistency: the run-estimate endpoint no longer expands an
explicit empty waterfall into default providers (or a stale single-provider
field). Ordered explicit lists remain ordered; missing/null configuration keeps
legacy defaults. Six parameterized API checks capture the exact selection passed
to the catalog estimator; all 86 workbook API tests pass. This fixes selection
parity, not completeness of estimates across research/agent/output paths.
The broader chat/contact/outreach/signal/tenancy regression selection also passes
(87 passed, one skipped); these are offline tests, not live GTM quality evidence.

Budget save edge cases verified in browser: two synchronous Save clicks produce
one request; a committed save followed by failed balance readback reports that
specific partial outcome, retains the draft and disables Save until recovery.
Refresh recovers without another write. Fixed a stale-warning bug by clearing
inline errors only after successful refresh; the expanded fixture asserts this.
The complete budget fixture, build, lint and diff checks pass. Intercepted APIs
are still distinct from live deployment acceptance.

Responsive/accessibility budget checkpoint: all five surrounding tabs now have
accessible names and decorative icons are hidden. Drawer width is bounded to
the viewport and content has consistent padding; screenshot review caught and
corrected the inherited narrow mobile width/edge-to-edge content. Browser tests
pass at 375/768/1440px in light/dark, checking drawer bounds, no inner horizontal
overflow, input focus and visible Save alongside the earlier recovery flows.
Production build passes. UI/UX skill informed contextual icon naming and layout;
these checks do not constitute a full accessibility or contrast audit.

Budget browser checkpoint: production build and isolated Chromium fixture pass
initial cost HTTP failure, disabled save until recovery, refresh, server ceiling
initialization, reserved/uncertain amounts, empty-input validation without writes,
failed save with retained draft, retry and refreshed remaining allowance. No
browser page errors occurred. The test is
`check-workbook-selection.py --check-budget`; requests are intercepted. Responsive
themes, duplicate pending submissions, readback-after-save failure and accessible
names for the surrounding icon-only tabs still need acceptance work.

Budget client guards implemented: malformed/missing/negative exposure fields,
inconsistent unlimited flags and unrecognized accounting bases reject as load
errors before rendering. Negative remaining headroom is allowed (over-cap), not
clamped or misreported as a server error. Saves reject invalid amounts before
network I/O and require an exact cap receipt plus valid accounted spend. IDs are
URL encoded. All 52 frontend unit tests, lint and TypeScript checks pass;
browser acceptance of the updated panel remains pending.

Budget panel implementation: Twenty input/button adapters now accompany explicit
accounted/reserved/uncertain amounts, refresh and load-error states, a labeled
ceiling input, synchronous duplicate-save guard, and inline validation/save
errors. Successful budget saves refetch balances; failed readback is distinguished
from a failed save. Drafts initialize from the server ceiling and remount on
workbook changes. Copy warns that amounts include estimates and coverage is
incomplete. UI/UX skill guidance informed associated labels and announced errors.
TypeScript passes; browser acceptance, malformed-response guards and full panel
migration remain pending. No reconciliation action is exposed yet.

Cost API now separates active reserved exposure and uncertain exposure and
subtracts both from remaining workbook allowance. Settled/released receipts are
not counted again. Responses explicitly label the mixed catalog/recorded-charge
accounting basis. Negative and nonfinite budget updates reject with 422 instead
of being silently coerced. Client types reflect these fields and the narrower
budget-update response. All 80 workbook API tests pass, including headroom,
invalid-budget and foreign-workspace checks. Dedicated UI presentation of
reserved/uncertain amounts and reconciliation actions is still pending.

Cross-path cap check: real agent and waterfall cell paths compete in both start
orders with one lookup allowance. File-backed sessions match production's
`autoflush=False`; tests explicitly require the losing path's budget error, not
merely one result (auto-flushing test sessions initially produced lock failures).
Exactly one fake-provider call, settled receipt and debit occur. All 16 spend
tests pass. Verification audit found only SMTP and optional Reacher registered
in current source, despite comments mentioning HTTP vendors. Their cost semantics
must be established rather than inventing paid-provider charges; verification
coverage and PostgreSQL acceptance remain open.

Queued ordinary-enrichment integration started: paid waterfall/single-provider
calls now reserve and settle through the shared service, preserve result replay
before fresh-spend planning, and avoid the legacy duplicate debit. Paid timeout
or uncertain accounting stops the chain with an explicit error. Tests exercise
the actual cell runner, persisted workbook rows and receipt readback: successful
forced retries replay at an exhausted cap with one call/debit; paid misses remain
uncertain and do not call again. All 79 workbook tests pass. Verifier calls,
non-queue/direct execution, cross-path concurrency, PostgreSQL, credential-version
identity and user-facing reconciliation remain open. Catalog exposure is an
estimate, not a guaranteed bound on vendor invoices.

Ordinary-enrichment pre-integration audit fixed an authorization issue: explicit
`waterfall: []` no longer expands to default providers (or a leftover `provider`
setting). Missing/null waterfall retains existing default/single-provider
behavior. Router-to-cell tests verify zero calls for both empty-list cases and
retain existing exact-order and cost/cooldown checks. Workbook/spend suites pass
(91 tests). Ordinary enrichment reservation integration is still pending.

Concurrent-agent integration check: two async agent cells with independent
database sessions now compete for a workbook cap equal to one paid lookup. The
fake provider yields before settlement, exposing the in-flight reservation
window. With the real planner and reservation service, exactly one provider call
occurs; the other cell reports `agent_workbook_budget`, and readback shows one
settled receipt and one debit. All 14 spend tests pass. This is file-backed SQLite
with an injected provider, not PostgreSQL or live vendor billing acceptance.

Exhausted-headroom replay fixed for queued agents: persisted provider attempts
for the exact workspace/workbook/job/typed-row/column are visited before newly
planned spend. Their wrapper still enforces contract identity and dispatch state;
this is not a budget bypass for new attempts. The integration test now uses the
real planner and a workbook cap equal to one lookup, proving a successful retry
replays despite zero remaining headroom and does not debit twice. Paid-miss
uncertain retries remain blocked. Spend/workbook suites pass together (88 tests).

Queued agent integration started: paid calls with a durable execution identity
now reserve/dispatch through the wrapper and accounting adapter, suppress the
legacy success debit, and stop on uncertain accounting. File-backed tests run
the real agent loop and reservation service with a fake provider: successful
retries replay one result/debit, paid misses retain exposure and block retries.
Typed row identities distinguish workbook rows from leads. Contracts hash the
original row input, not Lead serialization's generated timestamps. All 13 spend
tests pass; preceding workbook/spend regression run passed 86 tests before the
two new integration cases. Direct agent calls, ordinary enrichment/verifiers,
credential-version identity, exhausted-headroom replay and PostgreSQL remain
open. Do not claim a complete shared cap while any execution path bypasses it.

Accounting adapter added: provider results can carry optional billing evidence
(integer micro-USD plus provider reference), preserved through subprocess output.
`accounting_envelope` labels success-based catalog amounts as estimates, accepts
explicit referenced charges/nonbillable outcomes, and rejects missing/mismatched
results or paid misses without evidence as uncertain. It preserves overruns and
copies the result snapshot. No vendor adapter currently supplies new evidence;
do not mistake the optional field for verified pricing integration. Reservation
execution is still not connected to workbook provider loops. Accounting,
workbook and reservation tests pass together (98 tests).

Provider isolation correction implemented: agent columns now use the existing
killable subprocess runner rather than awaiting `provider.enrich` directly.
Workbook configured provider timeout is forwarded; timeout attempts receive
explicit trace/telemetry markers. Regression tests forbid direct invocation and
verify the runner receives the configured timeout exactly once. This does not
classify a timeout as non-billable, nor integrate the new reservation service.
Provider result dictionaries currently lack billing evidence; an accounting
adapter must distinguish catalog estimates from confirmed vendor outcomes.

Worker identity propagation implemented: `run_workbook_enrichment` scopes a
frozen workspace/workbook/job identity through ContextVar inheritance to child
tasks. Attempt keys include stable run, typed row, column and provider identities,
not retry passes, leases or mutable inputs (which belong in the conflict-checked
operation contract). Direct runs explicitly clear inherited queue identity;
they still need durable approved identities before reservation integration.
Concurrent-run/retry/nesting tests and workbook/service regressions pass (87).
This propagates identity but does not yet wrap provider calls or change billing.

Retry identity hardened: reservation now requires an explicit nonempty operation
contract and includes it in the deterministic hash. Same row/provider/cost with
changed inputs or instructions no longer replays an old attempt/result. Only the
hash is persisted, not the raw operation contract. Eleven tests pass, including
changed-input reservation rejection and changed-instruction rejection after both
successful and uncertain execution, without another provider call. Integration
must build this contract from the exact immutable inputs/config/credential
identity used by the operation; do not include raw credentials or rely on IDs
alone. This remains an unconnected execution foundation.

Execution wrapper foundation: `execute_reserved_attempt` admits and commits a
reservation, claims dispatch once, invokes an injected operation outside the
transaction, and settles an explicit accounting envelope. Completed retries
return the persisted receipt. Exceptions, task cancellation and missing
accounting mark the attempt uncertain and block repeated calls; failed uncertain
marking leaves the dispatched guard in place. Tests also perform a separate
database write inside the provider callback to check no writer lock spans I/O.
Eleven SQLite storage/service/wrapper tests pass. Workbook providers do not yet
use this wrapper; integration must supply stable approved execution identities
and validated provider-specific accounting, not invent invoice semantics.

PostgreSQL verification prepared, not executed: existing workbook RLS tests now
seed/check spend attempts for tenant SELECT isolation, missing-context denial,
forced RLS, grants and non-null workspace identity. A two-session application-role
test competes for the last reservation allowance with per-transaction workspace
context. Local discovery found no PostgreSQL binaries and Docker socket access
was denied. The combined run reports 7 passed / 22 skipped without
`TEST_DATABASE_URL`; skips are not PostgreSQL acceptance. Use an explicitly
disposable owner database with the existing RLS suite setup to run this gate.

Settlement foundation implemented locally: settlement locks workbook then
attempt, records the accounted micro-USD amount/result/basis, and updates legacy
workbook spend in one transaction. Identical receipts replay without charging;
changed receipts conflict. Zero/partial/over-reservation amounts are preserved
rather than clipped, with the original reservation retained for audit. Cell
headroom uses settled cost after completion. Uncertain attempts cannot use this
ordinary settlement path. Two-session SQLite settlement tests produce exactly
one debit, including overruns; seven storage/service tests pass. The unreleased
migration now includes nullable settled micro-USD. PostgreSQL validation,
reconciliation and live execution integration remain unfinished.

Admission implemented locally: `reserve_attempt` serializes admissions with a
scoped workbook UPDATE before reading headroom, sums outstanding receipt exposure,
checks cumulative per-run/cell exposure, and persists a hashed contract and cost
basis before returning. Amounts use integer micro-USD; legacy floating workbook
caps/spend are converted conservatively. Matching attempts replay; changed
contracts reject. File-backed SQLite two-thread tests admit exactly one of two
requests for the last allowance and prove uncertain exposure remains counted.
All four storage/admission/lifecycle tests pass. PostgreSQL, cross-workbook
attempt-key contention, settlement and execution integration remain unverified.

Lifecycle guards implemented locally in `spend_service.py`: conditional scoped
updates commit reserved→dispatched, reserved→released, or dispatched→uncertain.
Only the winning dispatcher receives authorization; duplicates and mismatched
workspace/workbook/contract identities fail closed. Cancellation cannot release
a dispatched or uncertain attempt. File-backed SQLite tests use two independent
sessions/threads and verify one dispatch winner, retained uncertain exposure,
and pre-dispatch cancellation. All three storage/lifecycle tests pass. Admission,
settlement, PostgreSQL concurrency/RLS and provider integration remain open.

Storage foundation implemented locally: `WorkbookSpendAttempt` and migration
`1d2e3f405162` persist scoped attempt identities, immutable-cost-basis snapshots
(immutability must be enforced by the forthcoming service), integer micro-USD
exposure, lifecycle status, timestamps and results. Unique attempt keys,
nonnegative exposure and allowed statuses have database constraints. PostgreSQL
DDL includes forced workspace RLS using the existing configured application role.
SQLite migration upgrade/downgrade and constraint tests pass alongside workbook
tests (75 total). PostgreSQL RLS is not yet exercised. No execution path uses the
table yet, and reservation/settlement transitions remain to implement. Outstanding
exposure can initially be summed from indexed receipts under the workbook lock;
avoid an additional denormalized counter until its consistency is justified.

## Evidence from current execution

- `workbook/vendor_catalog.py`: `Vendor.base_cost` describes approximate USD per
  successful lookup. It does not identify vendors that charge for unsuccessful
  attempts or provide invoice reconciliation.
- `workbook/agent_column.py`: reads workbook headroom before calls and debits
  only paid successes afterward. Parallel cells can observe the same headroom.
- `workbook/enrichment.py`: ordinary enrichment has the same read-before-call,
  charge-after-success pattern. Fixing agents alone leaves the shared cap unsafe.
- `leadgen/contact_execution.py`: durable, tenant-scoped action claims prevent
  automatic re-execution after uncertain outcomes. This is a useful lifecycle
  precedent, not a workbook budget reservation ledger.
- `workbook/provider_runner.py`: ordinary enrichment uses killable provider
  processes. Agents currently await providers directly; timeout/isolation parity
  needs separate correction without confusing timeout with non-billable failure.

## Contract

Keep provider cost estimates separate from platform credits and verified vendor
charges. A reservation limits estimated exposure; it cannot promise an invoice
cap when prices or billing semantics are unknown. UI receipts must say which
amount is estimated, reserved, confirmed, or uncertain.

Before external I/O, reserve the catalog exposure in a short, committed database
transaction. Atomically require current spent plus reserved plus requested
exposure to fit the workbook cap. No network call while holding the lock.
Use the smaller remaining cell allowance as an additional constraint. Apply this
to agent and regular enrichment, including verification calls where chargeable.

Persist each attempt with workspace/workbook, durable run identity, row identity,
column, provider, attempt key, cost basis, reserved amount and lifecycle status.
A unique scoped attempt key prevents duplicate dispatch. Persist the cost basis
at reservation time; later catalog changes must not alter that receipt.

On known completion, settle once and release only proven unused exposure.
A timeout, lost worker or transport failure after dispatch is uncertain, not
automatically free. Keep its exposure until reconciliation; never blindly retry
with the same approval. Explicit operator recovery must leave an audit record.
Do not retroactively reinterpret existing success-based spend as attempt costs.

## Implementation sequence

1. Add a migration for tenant-scoped reservation/attempt records and workbook
   reserved exposure, with SQLite and PostgreSQL parity and relevant RLS policy.
2. Implement reserve/dispatch/settle/uncertain transitions with conditional SQL,
   idempotent receipts, and injected session factories for isolated tests.
3. Integrate both enrichment paths. Remove their old success debit only once
   settlement owns that accounting, avoiding duplicate charges.
4. Expose reserved/uncertain exposure in run history and budget previews without
   calling estimates actual spend. Define explicit reconciliation actions.
5. Validate restart, cancellation, deadline and repeated-delivery behavior.

## Required acceptance

- Two independent PostgreSQL sessions competing for the final lookup allowance:
  one dispatch only; no oversubscription. SQLite coverage alone is insufficient.
- No provider invocation when reservation fails or tenant ownership mismatches.
- Duplicate attempt keys cannot dispatch or settle twice.
- Crashes before dispatch, after dispatch and after response have distinct,
  durable outcomes; uncertain work is not silently refunded or auto-retried.
- Failed billable, failed non-billable and unknown-billing outcomes do not share
  an invented accounting rule. Preserve catalog estimates and evidence.
- Agent, waterfall and verifier calls share the same workbook headroom.
- Run cancellation/restart cannot erase outstanding exposure.
- Database locks are released before provider I/O; login and heartbeats remain
  responsive during slow execution.

No live provider traffic or billing changes were made during this audit.
