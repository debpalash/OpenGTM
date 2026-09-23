# OpenGTM interface migration to Twenty's design system

## Current decision and rollout status

Use Twenty for the presentation layer, not as a replacement CRM or execution
engine. Preserve OpenGTM branding, agents, evidence, workbooks, enrichment,
approvals, workspace isolation, and all existing data contracts.

The delivery sequence below remains the rollout plan. Local implementation has
already progressed through the foundation and shell into the workbook reference
slice; it is not a completed migration or an approved production release.
Finish and accept that slice before broadening the replacement to other screens.

Current release blockers include unresolved route-payload regression,
remaining wide-grid/accessibility coverage,
and complete execution/recovery acceptance. No migration deployment has occurred.

The checkpoint entries below are chronological work notes in newest-first order;
older statements about what is implemented or tested are superseded by newer
entries. Passing intercepted browser fixtures does not establish live provider
or end-to-end GTM success.

## Implementation checkpoints

Unknown output placeholders: output rendering now uses strict reference lookup,
rejecting nonexistent fields instead of transmitting a literal not-found marker.
Existing optional null fields still render empty. Webhook template ValueErrors
become explicit failed output receipts. A mocked HTTP-client sentinel proves an
unknown nested-body reference never opens a client; the dispatcher returns a
failed result. 206 rendering/workbook tests passed before dispatcher handling;
all 14 rendering tests pass afterward. Other destination error presentation and
structured-versus-string JSON template behavior remain to audit.

Sequencer regression fix (2026-09-23): strict lookup made Instantly/Smartlead
default field maps raise on workbooks without `first_name`/`last_name`. Field
maps again drop unknown columns (the existing documented contract), and never
send a literal marker. Ambiguous references still fail. The dispatcher now turns
template ValueErrors from every destination into failed receipts before any
vendor call.

JSON string webhook bodies: a string template that already parses as JSON now
resolves structurally like a dict body, so cell values containing quotes or
newlines stay escaped. Before, they broke the JSON and were silently sent as
text/plain. Non-JSON string templates keep the previous behavior. A mock
transport test verifies the escaped payload. Full backend suite: 1,783 passed,
120 skipped.

Normalized reference mutation guards: deletion and breaking-rename checks now
recognize underscore/space aliases as rendering does. An API regression verifies
`{company_name}` protects a Company Name input from deletion/rename and leaves
the configuration untouched on rejection. All 207 workbook/graph tests pass.
These mutation guards intentionally remain conservative around alias collisions;
they do not claim identical precedence behavior to execution or automatic
reference rewriting.

Reactive reference parity: downstream selection now resolves column identities
with the shared matcher, propagates normalized references transitively, and
maps edited lead_field keys to their input columns. Unmodelled raw fields retain
normalized matching, while an exact ID no longer gets tainted by an unrelated
column with a colliding name. Tests cover these distinctions; all 206 graph/
workbook API tests pass. Eligibility and output retry rules remain unchanged;
edit-plus-enqueue atomicity and deletion/rename reference parity remain open.

Cycle/availability reference parity: both guards now use the same exact-ID,
alias and normalized reference matching as template validation. Cycle matching
retains input columns during resolution so a colliding executable display name
cannot turn a valid input reference into a false self-cycle. Tests cover a
normalized two-column cycle, missing/false-valued dependencies and input-ID
precedence. All 298 graph/workbook/template/batch tests pass. Reactive propagation
and dependency deletion/rename checks remain to align with normalized matching.

Normalized scheduling references: topological ordering and independent batch
selection now share reference matching with template validation, including exact
ID precedence and underscore/space fallback. A reversed-order fixture verifies
`{Company Name}` waits for company_name and neither dependent column enters the
independent batch set. All 39 graph/batch/rendering tests pass. Cycle detection,
reactive propagation and availability checking still require matching parity.

Shared cell ambiguity guard: declared template/input-column references are now
validated before synchronous execution, storing ambiguous_column_reference under
the current queue lease. Direct research, HTTP and formula API cases prove
duplicate display references never reach their executors. All 217 workbook/
rendering/batch tests pass. This extends workbook execution coverage, not every
standalone helper caller; graph normalization and alias-precedence parity remain
open and no live vendor execution was performed.

Duplicate display-name references: AI prompt construction and output template
resolution now validate references against column identities before flattening
values. Referenced duplicate names fail explicitly (including case variants);
exact column IDs remain usable, so unrelated duplicate display labels do not
block ID-based templates. Both ordering cases exercise AI/output rendering and
all 214 rendering/workbook/batch tests pass. Research/HTTP/formula resolver parity
and graph normalization remain to audit; this is not a platform-wide ambiguity
completion claim.

Ambiguous template fallback: case-insensitive and underscore/space-normalized
matching now rejects multiple differing values instead of choosing insertion
order. Exact-key matching retains precedence and synonymous keys with equal
rendered values remain usable. Four order/normalization cases verify explicit
failure without leaking candidate values; all 40 rendering/batch/native-research
tests pass. Identical display aliases already collapsed during projection and
graph/resolver normalization parity remain separate unresolved cases.

Post-dependency full regression: the complete backend suite passes with 1,765
passed, 120 skipped and two deprecation warnings in 49.90 seconds. This covers
the newly added graph admission, template integrity, falsy-value rendering and
exact-ID alias-precedence changes alongside existing research/output tests.
Command: `env -u DATABASE_URL APP_ENV=test uv run pytest tests -q`.
PostgreSQL and controlled-live outcome gates remain unproven; this checkpoint
does not advance the live release streak or establish full V2 parity.

Template reference consistency: output/lead-value projection now includes current
display-name aliases, and both AI/output projections give exact stable ID keys
precedence over colliding display-name keys independent of insertion order.
Stale raw display-name copies no longer override current computed aliases.
Two ordering cases verify matching prompt/output resolution; all 208 rendering/
workbook/batch tests pass. Case-folded duplicate aliases and graph/resolver
normalization parity still need explicit ambiguity handling.

Falsy-value rendering: shared prompt/output text conversion now treats only
None as absent instead of erasing zero and False through `value or ""`.
AI row extraction, exact/case-insensitive/normalized prompt matching, raw lead
field mapping and nested output-body rendering are covered by five value cases.
Non-string JSON body values remain typed. All 206 rendering/workbook/batch tests
pass. No provider or destination was contacted; this validates deterministic
rendering, not external integration delivery.

Legacy template identity integrity: enrichment column IDs must now be explicit
and unique; creation no longer invents IDs or suffix-renames collisions while
leaving dependent references untouched. Three regression cases cover collision,
blank and missing identities, asserting no workbook persistence or shared
template mutation. All 80 gallery tests pass, including shipped legacy template
creation. This does not yet resolve ambiguous display-name references across
the platform.

Template graph acceptance: legacy template creation now rejects cyclic graphs
before persistence. Shipped legacy templates instantiate successfully; every
gallery recipe is checked for cycles. This audit caught and fixed a regression
in the new graph guard: self-check conditions such as `{email} == ""` are not
computation cycles. Self-referencing prompts/formulas remain blocked, and
cross-column condition dependencies remain in the graph. 277 template/graph/
workbook tests pass; all 77 gallery tests pass after strengthening the catalog
assertion. Template ID auto-renaming/reference integrity still needs review.

Workbook creation graph validation: supplied column definitions now reject
duplicate IDs, computed self cycles and multi-column cycles before creating a
workbook or snapshotting source rows. Three API cases use CSV-backed creation
and assert no workbook, row or queue job survives rejection. All 196 workbook/
CSV tests pass. Existing CSV append builds input columns rather than accepting
arbitrary execution definitions; specialized service/template constructors still
need a complete audit before claiming every creation path is covered.

Column-save cycle boundary: settings PATCH, full workbook column replacement,
and add-column reject changes that newly put columns in or downstream of a
computed cycle. Validation occurs before mutation under the existing parent
lock. Existing broken workbooks can still be repaired without first replacing
the whole graph. The API regression covers PATCH/PUT/add rejection, unchanged
metadata/configuration and successful repair of a seeded legacy cycle; all 185
workbook API tests pass. This is a no-newly-blocked-columns rule, not proof that
all legacy cycles or alias ambiguities have been eliminated; run-time guards
remain necessary. Creation/import/generated configuration paths need audit.

Run admission cycle validation: selected cyclic/dependent columns now receive a
422 naming affected column IDs and directing reference repair before billing,
workbook status changes or queue creation. Exact per-row scope is respected:
an unrelated safe column can still run even when other workbook columns cycle.
The API test verifies no billing/queue mutation on rejection and exact safe-scope
enqueue. All 195 workbook/dependency tests pass. Estimate/configuration editors
still need early graph diagnostics; the worker guard remains defense in depth.

Execution cycle guard: computed cycles, self references and their downstream
dependants now produce dependency_cycle in synchronous execution, even when old
complete values exist. Independent batch eligibility excludes self-cyclic columns
as well. A graph test isolates safe independent work; an output API test proves
saved values cannot authorize a cycle-dependent send. 206 dependency/workbook/
batch tests pass, plus the added focused output case. The ordering utility retains
its legacy fallback for non-execution consumers; configuration-time validation
and user-facing cycle repair remain open.

Computed dependency availability: synchronous cell execution now rejects missing
values from referenced executable columns before provider/output work, storing
upstream_dependency_unavailable under a fenced queue lease. Zero and False remain
valid inputs. Four direct output API cases verify failed snapshots (including
stale copied values) make no destination call, while zero/false dependencies do.
The preceding scope/workbook regression passed 212 tests; all four new focused
cases pass. Input-field validation, evidence freshness, alias ambiguity, cyclic
references and cross-path/batch acceptance still require further work.

Stale materialized values: execution hydration now removes a column's ID/name
copies from row.data when its saved enrichment snapshot is failed, running,
pending, skipped, or complete without a value. Previously skipping the overlay
still left an old materialized value available to downstream templates. Five
projection tests cover these states and assert stored row data is not mutated;
all 212 scope/workbook tests pass. This removes stale inputs but does not yet
enforce dependency availability across every partial run, direct cell call and
batch path; that broader dependency gate remains open.

Shared job-payload concurrency: run-result persistence now acquires a fenced
writer lock before reading/merging the job JSON. SELECT FOR UPDATE alone was
ineffective on SQLite and allowed a stale result writer to erase an intervening
output claim. A SQL-order assertion verifies lock acquisition precedes reading;
four concurrent writers exercise twenty interleaved output claims/result writes
and preserve all ten claims, the result and unrelated metadata. All 211 workbook
and journal tests pass, including stale-lease/cancelled-run receipt rules. This
does not substitute for the still-open PostgreSQL concurrency gate.

Full regression checkpoint: 1,735 backend tests passed, 120 skipped, two
deprecation warnings in 49.53 seconds using the disposable SQLite test setup.
All 60 frontend/payload-reporter tests pass. This includes the recently integrated
output journal and receipt-summary contracts alongside the rest of the suite.
PostgreSQL-gated RLS coverage remains skipped without TEST_DATABASE_URL; fixture
success is not live-provider, cross-job output, or full migration acceptance.

Output receipts in run history: the Twenty-based history dialog now shows
succeeded/failed/awaiting/unknown counts and warns users to check the destination
before re-sending when delivery is unresolved. Older runs explicitly say tracking
was not recorded. Client validation rejects negative, malformed or inconsistent
count summaries. Five client tests, production build, lint and the history
browser fixture pass; browser assertions cover a pending receipt, warning and
legacy fallback alongside pagination/polling/reload. UI/UX recovery guidance
informed the explicit next step. This is visibility, not a reconciliation action
or proof of delivery, and no automatic resend control was introduced.

Output-history API foundation: run receipts now include an allowlisted count
summary of output attempts (awaiting receipt, succeeded, failed, unknown). Missing
journals return null, not a fabricated zero. In-flight claims are not labelled
confirmed failures; a recorded failure does not establish absence of an external
side effect. The scoped/paginated history test verifies counts and exclusion of
cell identifiers, hashes, response values and error text from this summary.
The focused API test and diff check pass. UI presentation and reconciliation
actions remain to implement; this endpoint does not authorize resending.

Queue-level output recovery verification: tests now exercise QueueService's
failure/requeue/claim/process sequence, mocking only the child process and external
destination. Both lost acknowledgement and a crash after receipt persistence
retain output_attempts across the new lease and make exactly one destination
call. A separate lease-loss-during-send case rejects stale receipt writes and
requires review under the replacement owner. All 32 journal/ownership tests pass.
These are SQLite tests with mocked sends, not real provider or PostgreSQL proof;
they do not close cross-job/direct-call idempotency or reconciliation UX gaps.

Queued output replay boundary: the engine now uses the output-attempt journal
before external output dispatch. A lease-scoped, row/column-specific claim is
committed before vendor I/O; recorded receipts are reused, while claims without
receipts return output_delivery_requires_review without resending. Changed input
contracts fail closed, including force/run_once=false replays within the same
job. Independent-writer tests establish no claim transaction spans vendor I/O.
205 workbook/journal tests passed, then two actual enrich_cell integration cases
passed for successful receipt reuse and lost acknowledgement, asserting one
mocked destination call and correct stored row status. No real send occurred.
This protects retries of the same queued job, not separately queued jobs or
direct cell API calls; destination-side idempotency, cross-job approval and a
reconciliation UI remain required. PostgreSQL concurrency remains unverified.

Command-palette failure acceptance: an injected aborted module download exposed
that Chromium caches failed dynamic imports, making the original Retry action a
dead end even after networking recovers. The dialog now offers explicit Reload
page with guidance to close/save pending edits first; it never reloads on its
own. The fixture proves the failure is announced, Escape closes it and returns
focus, and a user-requested reload followed by opening commands recovers. Normal
theme/workspace/shortcut checks also pass with no page errors. Build/lint and
diff checks pass. UI/UX error-recovery guidance informed the message/action.
This closes the previous checkpoint's injected-failure check, not broader
workbook performance or end-to-end execution acceptance.

Command-palette loading boundary: global shortcuts and the sidebar event listener
remain in the shell, while command UI and its lead query load on first opening.
Closed palettes disable their lead query. Loading/error feedback uses a labelled
dialog; UI/UX focus guidance informed the browser assertions for autofocus,
Escape and return to the sidebar trigger. The app-theme browser fixture confirms
no command chunk or lead request before opening, then successful commands/theme
switching and reopening. Build/lint, 59 frontend/reporter tests and diff checks
pass. Static workbook JS falls from 1,063,100 raw / 331,494 gzip to 1,008,243 raw /
313,089 gzip bytes; CSS is unchanged at 409,248 raw / 70,153 gzip. These are local
compression estimates, not live latency. Performance acceptance remains open;
chunk-load failure recovery still needs an injected-failure browser check.

Output result truthfulness: nonempty diagnostic strings from failed deliveries
(for example POST 500) no longer become complete enrichment receipts. Output
handling respects explicit false/malformed success flags and error-bearing
responses, persisting error/null value in both overlay and row snapshot. Five
failure cases include missing, contradictory and malformed success fields;
the force-success regression still passes. 203 workbook/scope tests passed before
adding two malformed-response cases; the six focused cases pass afterward.
Webhook/HTTP helpers inspected here do not internally loop retries; other output
integrations and durable replay still need auditing. Destinations were mocked.

Automatic retry boundary for external writes: cell retry passes now exclude
output columns and mutating/unknown HTTP methods; GET/HEAD/OPTIONS and ordinary
computed columns retain their retry path. Explicit run_once=false does not grant
automatic resend authority. Engine-loop cases verify an output cell is attempted
once while a formula retries, for returned errors and thrown failures; method
policy cases cover POST/PUT/DELETE and read methods. 55 scope/batch recovery tests
pass. This only governs in-run cell retry passes: durable job replay, output
provider internal retries and agent tool side effects still require idempotency
and uncertainty handling. No actual webhook/CRM call was made.

Run-history scope clarity: receipts now expose selected_cell_count separately
from worker outcome totals. Exact row/column maps count only selected cells;
malformed or unrecorded scope returns unknown rather than a fabricated rectangle.
The UI displays selected cells independently from completed/error/planned outcomes
and remains compatible with older responses missing this field. Unit/API tests
cover sparse selections and readback; the browser fixture verifies two selected
cells across two rows/two columns. 191 backend tests passed before the added API
readback assertion; that focused case also passes. Build/lint pass. Selected
scope is not proof that all cells ran, especially in fill-missing mode.

Responsive progress acceptance: the run browser fixture now supplies a complete
zero-valued cell, an error with stale data and a skipped cell, asserting 1/3
complete, one error, one skipped and 100% processed in light/dark at 375/1440px.
Full bounding-box assertions (not merely partial viewport intersection) caught
the processed label overflowing at 375px. The footer and progress groups now wrap;
the stricter browser rerun, build, lint and diff checks pass. UI/UX explicit-state
and responsive guidance informed the change. This covers fixture presentation,
not live execution or every long-label/zoom combination.

Progress display semantics: column bars count complete status only, not truthy
stale values on failed cells. The running footer covers research/agent/HTTP/formula/
output columns as well as enrichment, distinguishes skipped cells from errors,
and explicitly labels loaded-cell counts and processed percentage. Activity is
available for every executable type. UI/UX explicit-state guidance informed these
labels; this remains loaded-data progress, not an authoritative run-wide receipt.
A pure progress test covers stale errors, zero/false results, skipped/running and
empty data. Build/lint pass, and the existing run-review browser regression passes
with fixture APIs. Dedicated responsive progress assertions remain to add.

Failure-state UI: workbook typing and list filtering now include failed. The
editor keeps a semantic alert visible for failed runs, directing users to history
and cell errors and warning against resending completed outputs. UI/UX Pro Max
error-recovery guidance informed the announced, persistent message and explicit
next step; no automatic retry action was added. Production build and lint pass.
The history browser fixture now starts with a failed workbook and checks the
recovery message before exercising history errors, retry, pagination and reload.

Explicit row-storage marker: new/migrated workbooks persist row_storage_version=2
in source_config. Both row-deletion endpoints mark existing snapshots in the same
transaction, preserving source metadata. Shared legacy detection in detail/count/
run/single-cell/worker paths honors this marker. Tests delete the final leads_filter
snapshot by IDs and query, then verify empty readback, skipped run, rejected cell
execution and empty worker loading. 179 workbook/CSV tests pass. Already-empty,
unmarked historical workbooks still require an explicit migration audit; their
identity cannot safely be inferred. No production data was migrated.

Empty v2 readback parity: workbook detail now stays on the WorkbookRow query path
for empty non-legacy workbooks, and list/metadata counts do not fall back to legacy
leads for those source types. Three tests cover empty/CSV/agent workbooks, assert
zero displayed rows/counts and explicitly record that no legacy query occurred
(including inside the metadata helper's exception handler). 169 workbook tests
pass; the three focused cases pass after strengthening that no-query assertion.
This aligns display scope with the previously fixed execution scope. Legacy
leads_filter snapshots emptied after migration still require a durable identity
version marker rather than inference from row count.

Legacy single-cell membership: the endpoint now resolves the requested lead
through the workbook's filter plus an exact lead-ID constraint, not an unfiltered
get_lead lookup. Both SQLite and PostgreSQL lead-store adapters accept this ID
intersection; an empty ID list matches nothing. SQLite tests verify matching and
nonmatching specialization/ID intersections, and API tests prove only matching
leads reach execution. PostgreSQL adapter changes still need service-backed
acceptance; no RLS result is inferred from the SQLite tests.

Empty-workbook execution isolation: full-run API and worker loaders now permit
legacy lead-store fallback only for leads_filter workbooks. Empty, CSV, agent and
unknown source types with no stored rows return no work; an empty table cannot
silently become a workspace-wide legacy lead run. Four cases prohibit lead-store
access and assert skipped/no jobs in API and loader paths. A positive legacy case
verifies explicit leads_filter execution still queues its filter-resolved lead IDs.
The workbook/scoping/RLS command passed 174 tests with 22 RLS skips before adding
that positive case. PostgreSQL remains unverified; legacy single-cell membership
and an explicit persisted v1/v2 identity marker still need review.

Single-cell identity fallback removed for v2: a missing workbook row ID now
returns 404 instead of matching another row's lead_id or opening the legacy lead
store. Only a leads_filter workbook without stored rows retains the legacy store
path. Two tests cover empty v2 workbooks and a numerically matching linked lead,
with execution and lead-store access set to fail if reached. Both assert no
enrichment writes or queued jobs. Full-run empty-workbook legacy detection and
legacy filter-membership validation remain separate audit items.

Exact-scope pricing precision: billing projection accepts per-column cell counts,
sums catalog charges with Decimal arithmetic and rounds once at the established
four-decimal output boundary. Exact-cell runs aggregate selected column counts
and call projection once instead of summing separately rounded one-row estimates.
A regression verifies ten 0.00004 catalog charges project as 0.0004 rather than
zero and unselected columns contribute nothing. The mixed bulk-edit test asserts
the same exact column counts reach billing and the queue. This retains existing
ledger precision; it is not actual-provider invoice reconciliation.

Billing provider-selection consistency: the run billing projection now preserves
explicit empty waterfalls (including when a single provider is also configured),
retains configured provider order, and applies defaults only to absent/null
waterfalls without a selected provider. The existing six estimate-selection cases
now execute the run billing path with projection/debit stubs and assert identical
provider inputs. This fixes a mismatch where a disabled chain could project spend
for default providers. No real balance was debited. Complete AI/research pricing,
catalog freshness and invoice reconciliation remain outside this guarantee.

Batch dependency graph scope: batch eligibility now uses all executable columns
in the workbook configuration, then intersects each row's selected columns.
Selecting only one side of an upstream/downstream pair no longer makes that
column appear independent. Input columns remain outside this executable graph.
Two regressions cover either side of a partially selected pair and assert no
batch submission. Older isolated pre-pass fixtures now provide their actual
column configuration instead of an impossible empty workbook definition.
58 batch/recovery/cell-scope tests pass; diff checks pass. These tests use mocked
batch transport, not a paid batch submission.

Failed-upstream propagation: within a row run, dependent cells no longer execute
against hydrated old values after an upstream attempt fails. They persist an
upstream_dependency_failed error, propagating through selected dependent chains.
A real formula rerun test first failed because a division-by-zero exception left
the previous complete upstream result in storage; the cell exception boundary now
records a generic cell_execution_failed state in a fresh ownership-fenced
transaction. This prevents retries from treating that prior result as current.
The test now proves both cells remain errors across a retry rather than producing
a stale successful label. 56 scope/batch tests pass. Failure persistence can still
fail if ownership/storage changes, and external-send idempotency is not provided
by this change. Cross-run input versioning remains open.

Partial-run dependency hydration: row execution payloads now include complete
stored computed values for current columns, under stable IDs and display names,
before reasserting database-owned execution identities. Worker runs and direct
cell runs share the builder. Failed and deleted enrichment entries are excluded;
zero and false remain typed values. The real formula integration test reruns only
the downstream label and proves it consumes the saved upstream domain and persists
the correct result. 175 workbook/scope tests pass; all 19 scope tests pass after
adding typed-value/stale-entry checks. No paid provider calls. Staleness/versioning
of otherwise complete upstream evidence remains a separate unresolved concern.

Whole-suite regression checkpoint: after the execution-identity, exact-cell scope,
retry and saved-view changes, `env -u DATABASE_URL APP_ENV=test uv run pytest tests
-q --maxfail=3` completed with 1,694 passed, 120 skipped and two deprecation
warnings in 48.80 seconds. Tests used the suite's disposable SQLite database.
`bun test tests` in apps/web passed all 55 tests (143 assertions). Skipped tests
are not acceptance evidence; this checkpoint does not establish PostgreSQL
concurrency/RLS, live-provider outcomes, full Twenty route migration, or release
readiness. The remaining workbook identity/storage and recovery gates still apply.

Execution identity ownership: API run loading, direct cell loading and worker
loading now build row payloads with database-owned id/__row_id/__lead_id values
after user-controlled cell data. Computed column IDs/display names cannot replace
those identities while threading results to downstream cells. A regression seeds
forged reserved data keys and checks loader, direct cell execution and exact-scope
enqueue all retain the real row identity; another checks a computed reserved alias
cannot redirect the next cell. 174 workbook/scope tests passed before adding the
alias case; all 18 cell-scope tests pass including it. Diff checks pass. Stored
user data is preserved; only its interpretation as execution identity is blocked.

Truthful workbook terminal status: a normally finished loop with unresolved cell
errors now persists failed rather than complete; stopped and ownership-fenced
behavior remain unchanged. Row exceptions count only that row's attempted columns,
not every column in the workbook-wide union. Retries also intersect the initial
work-item cell set, excluding historical errors outside the actual attempt even
without an explicit row_columns map. Engine tests assert successful real formula
runs persist complete, failing/retried runs persist failed, and thrown row failures
retain exact error totals. 209 regression tests passed before adding the thrown-
row case; all 17 cell-scope tests pass including that case. This does not imply
external delivery idempotency or durable per-cell recovery messages are complete.

Output run-once identity: v2 output skips now require the selected row's own
complete receipt, returning its value rather than a lead-keyed sibling receipt.
If only an ambiguous legacy complete receipt exists, execution returns
output_identity_requires_review instead of silently claiming this row was sent
or automatically sending again. Explicit force retains the existing override.
Tests cover own complete/error/absent receipts with two rows sharing one lead,
plus the force behavior; destinations are mocked and no external sends occur.
The ambiguous state still needs a clearer UI recovery presentation, and this
guard is not atomic external-send idempotency or a legacy storage migration.

V2 batch identities: custom IDs and handled-cell keys now use an explicit row_
namespace for workbook rows, keeping legacy lead keys unchanged. Distinct rows
linked to one lead no longer collapse into the same request/result index, and
handling one row does not remove another row's synchronous work. A three-row
collision regression checks unique requests, exact row-specific result writes
and selective pruning; 53 batch/scope regression tests pass. Old persisted v2
batch contracts use different IDs and must reconcile through the existing hash
guard rather than being silently resubmitted. Legacy overlay storage identities
still need migration; this change does not claim complete identity isolation.

V2 completion/retry identity: fill-missing and retry lookups now use each selected
WorkbookRow.enrichments snapshot, keyed by a tagged row identity. Legacy rows
continue using lead-keyed overlay records through a separate path. Retry grouping
no longer collapses two v2 rows sharing a lead ID. The real formula collision test
now seeds a misleading complete legacy record and verifies the missing v2 cell
still executes and persists correctly. Historical-error tests seed actual v2
error snapshots rather than relying on legacy state. This does not migrate the
legacy overlay unique key or batch custom IDs, and output run-once checks still
need the same identity audit. No live provider calls or production changes.

Exact result-row mirroring: synchronous cell writes and batch result writes now
pass the explicit workbook row ID to enrichment persistence. When present, the
mirror queries that row only; it no longer chooses a different row whose linked
lead ID happens to equal the selected row's numeric ID. The real-formula test now
includes that collision and verifies both selected results persist while the
unselected linked row remains unchanged. Legacy callers retain their old fallback.
This is not a complete identity migration: the legacy WorkbookEnrichment unique
key still uses lead_id, so mixed identity collisions in legacy records, skip/retry
queries and batch custom IDs need further work before full acceptance.

Real formula execution evidence: a disposable SQLite test now runs the actual
workbook engine, row/cell execution, formula evaluator and enrichment persistence
without mocking those boundaries. Two rows have different cell selections; a
deliberately reversed dependency chain produces the expected domain and uppercase
label, exactly three selected records persist, and an unselected existing label
remains unchanged after readback. Redis/automation delivery and AI batching are
disabled in this test; no network provider or real user data is involved. All 15
cell-scope tests pass. This proves the local formula path, not paid-provider or
PostgreSQL acceptance. Linked-lead versus row-ID collision behavior and persisted
run receipt UI still need broader coverage.

Retry scope leak fixed: the engine's retry query can find historical errors in
cells outside a row-specific selection. Retry targets now pass through the same
row_columns intersection as initial work before reaching execution. A disposable
SQLite engine-loop regression seeds errors in all four cells of a two-row/two-
column workbook, selects only the diagonal, and verifies batch input, synchronous
work and retry work never include the other two cells. The actual orchestration
and database reads run; batch and per-row execution boundaries are test doubles,
not live provider calls. All 14 cell-scope tests pass. Full mixed-scope execution
with persisted computed values and UI receipts remains to verify.

Exact bulk-edit scope connected: bulk recompute now derives dependent column IDs
per changed row instead of running a cross-product of every changed field and row.
The run API validates canonical row identities, matching explicit row selections
and available runnable columns; missing selected rows reject before billing.
Queue payloads carry the row_columns allowlist, total_jobs counts selected cells,
and platform cost projection receives each row's selected provider columns rather
than the cross-product. Empty entries do not run. Audit metadata records the scope.
A mixed two-row/two-dependency test verifies two jobs/cells rather than four,
exact queued identities and pricing inputs; malformed scope tests assert no jobs.
187 workbook/scope/batch-prepass tests pass and diff checks pass. Billing tests
stub catalog projection/debit, so this is scope correctness evidence, not proof
of complete provider pricing or a live invoice. Real engine execution of mixed
scope, run receipt presentation and atomic edit/enqueue recovery remain to verify.

Per-row execution contract, worker half: queued workbook execution now accepts an
optional row_columns allowlist and intersects work items before batch submission
or synchronous execution. It preserves dependency order, uses workbook row IDs
instead of lead IDs, treats missing/empty entries as no work, and rejects malformed
scope payloads. Existing absent-scope behavior remains unchanged. Thirteen helper
tests plus handler forwarding and existing scoping/batch tests pass (60 total).
This is preparatory, not a completed bulk edit fix: the API must still construct
and validate the allowlist, price its actual cells, and report exact scope before
bulk edits use it. End-to-end worker/provider proof remains outstanding.

Explicit run scope safety: API and worker loaders distinguish an absent row/lead
selection from an explicitly empty selection. Empty selections now skip rather
than expand to all workbook rows. When selected v2 rows disappear before worker
execution, the loader returns no rows instead of falling back to the legacy lead
store. Six API/loader cases exercise empty row IDs, empty lead IDs and nonexistent
row IDs with and without stored workbook rows, asserting no jobs are queued.
170 workbook/scoping/batch-prepass tests pass; six focused cases pass after the
additional empty legacy-selection short circuit. Diff checks pass. This closes a
scope-expansion bug found while tracing bulk recompute; exact per-cell bulk scope
and transactional edit/enqueue still need implementation.

Reactive edit cost discipline: single-row and bulk saves compare actual stored
JSON values before scheduling dependants. Unchanged saves remain successful but
enqueue no recompute; mixed bulk edits exclude wholly unchanged rows from the
queued row IDs. Comparisons distinguish boolean/numeric values and ignore object
key order. The existing bulk updated_rows receipt remains the acknowledged input
count for client compatibility. Three new API cases verify no-op and mixed scopes;
143 workbook tests pass. A separate engine/accounting regression run passed 118
tests covering formulas, dependency ordering, scoping, batch recovery, ownership
and spend. These are offline tests, not live vendor outcomes. Different changed
fields across bulk rows still use a union of dependent columns; exact cell-level
bulk recompute and transactional edit/enqueue recovery remain open.

Browser acceptance refresh: rebuilt the current production UI and passed lint.
Saved-view, run-review, evidence, column-add and column-delete fixture browser
flows pass. The 50-column wide-grid flow initially failed because its conflict
assertion expected the old generic client error; the fixture now uses the real
server conflict text and additionally checks rename dependency rejection, retained
draft, unchanged stored name, and success after simulated external repair. The
rerun passes keyboard/width/settings/rename recovery, with 1,000 loaded rows and
534 rendered cells. Observed scroll sample median 50.1ms, p95 51.4ms is a local
headless fixture measurement, not field INP or a production performance gate.
All browser requests were intercepted fixtures; these checks do not verify live
provider outcomes, PostgreSQL contention, or complete workbook acceptance.

Rename reference integrity: targeted settings PATCH and full-list PUT reject
renames that remove a display-name alias still used by retained column prompts,
formulas or other supported reference fields. Stable-ID references remain valid;
the server does not silently rewrite prompts or redirect to a duplicate name.
The settings client preserves the 409 recovery message identifying dependants
and instructing callers to switch references to stable IDs. Six API cases cover
both writers, display-name casing/whitespace and permitted ID-based references.
150 workbook/dependency tests pass; the targeted client suite passes. This does
not yet detect newly introduced alias collisions, arbitrary code references or
coordinate already-running agents. No production changes were made.

Full-list column update guard: workbook PUT now locks the workspace-owned parent,
rejects duplicate column IDs, and checks removed columns against retained column
references and saved filter/sort/hidden-column references before changing metadata.
Source-column append also shares the parent lock to avoid overwriting concurrent
layout changes from these participating endpoints. Five new tests cover the four
reference paths and duplicate identities, including unchanged metadata/config and
no queued jobs after rejection. 144 workbook/dependency tests pass; 22 RLS tests
were skipped, not verified. Diff checks pass. Full-list PUT remains a replacement
operation without an expected-version guard; stale callers can still overwrite
retained column settings. Name-alias changes and active-run coordination remain
open. No production mutations or provider calls were made.

Saved-view write integrity: creation and configuration updates now reject missing
filter, sort and hidden-column IDs before mutation. View create/update/delete
acquire the workspace-scoped workbook row lock used by column deletion, keeping
the parent-before-view ordering consistent. Rename-only updates remain possible
for legacy stale views; replacing their configuration requires valid references.
Tests seed legacy stale views directly to retain fail-closed query coverage and
verify invalid create/update requests preserve existing view names/configuration,
while valid replacements succeed without queuing jobs. 139 workbook/dependency
tests pass and diff checks pass. SQLite tests establish sequential behavior, not
PostgreSQL contention; other full-list column writers and active-run coordination
remain outside this guarantee. No production changes or paid provider calls.

Saved-view safety: column deletion now rejects saved filter/sort/hidden-column
references with the view names and requires an explicit view update/removal.
More importantly, the shared row-query builder no longer silently skips a missing
filter or sort column. It rejects the stale view with 409, preventing a removed
filter from widening read/export/run/delete-query scope. Backend tests cover both
missing filter and sort references across detail, CSV export, estimate, run and
query-based row deletion, asserting rows remain and no jobs are queued. Three
view-reference deletion cases verify rejection then success after view removal.

136 workbook/dependency tests pass; the five focused view-safety cases also pass
after adding the destructive-query check. This is isolated sequential evidence.
View writers do not yet share the deletion lock or validate all referenced IDs;
a concurrent writer can still create a stale view, but using its filter/sort now
fails closed. Full-list replacement and in-flight execution gaps remain open.

Deletion dependency guard: under the same workbook lock, DELETE now rejects
columns referenced by other current columns through ID or display-name aliases.
It reuses the engine's reference extraction for prompt/formula/goal/condition,
explicit input columns, HTTP URL/headers/nested body and nested destination config.
The response names affected columns and the client preserves that recovery
message. Checks run before removing definitions or legacy evidence and also cover
legacy DELETE callers. Ambiguous name aliases are conservatively treated as
references rather than silently resolving to another column.

131 workbook/dependency tests and 55 frontend tests pass, with build/lint/diff
checks. Nine API cases assert unchanged config/evidence and no queued jobs on
rejection. This does not cover arbitrary code references, saved-view filters or
other external dependencies, nor does it coordinate an already-running worker.
Full-list replacement APIs elsewhere can still bypass this deletion guard.

Editor deletion cutover: both column delete entry points capture a cloned raw
stored column (not normalized display defaults) and open a Twenty AlertDialog.
The copy distinguishes removed definition/legacy enrichments from retained row
data/evidence and original leads, and warns about references and external actions.
DELETE uses that frozen snapshot through the serialized layout mutation. Failure
keeps the dialog open, disables repeat deletion and asks for cancel/refresh/review;
success applies only the confirmed column configuration. The editor no longer
imports useUpdateWorkbook or sends full-list configuration replacements for its
column actions. Other application writers and legacy unguarded DELETE callers
are not covered by this statement.

The new browser delete mode passes cancel-without-write, 503, double-submit guard,
409 after a changed width, re-review with the latest raw snapshot, unrelated-column
retention and reload. Build, lint, diff checks and 55 frontend tests pass; backend
delete behavior was separately covered in the 112-test workbook suite. All browser
API calls are fixtures: no live data was deleted. Dependency/active-run coordination,
PostgreSQL contention and complete migration acceptance remain open.

Stale column-delete confirmation guard: DELETE optionally accepts an exact
`expected_column` snapshot and compares it under the workbook lock before any
cleanup. A changed prompt/config rejects with 409 and leaves both definition and
legacy evidence intact. Legacy callers without a body retain prior behavior;
therefore not every deletion path is guarded yet. The web API client now requires
the snapshot, encodes both IDs, rejects identity mismatch before fetch and checks
the response contains the right workbook with the column absent. It is not yet
connected to the editor's delete action. That integration must capture raw
`workbook.columns_config`, not display-normalized columns with synthesized fields.
112 backend workbook tests and 55 frontend tests pass, plus production build,
lint and diff checks. Tests are isolated; no live deletion occurred.

Column-delete endpoint audit: deletion now locks and rereads the workspace-owned
workbook, rejects missing (404) and duplicate/ambiguous (409) identities, and scopes
legacy enrichment cleanup by workspace as well as workbook and column. The
existing semantics are explicit: remove the definition and legacy enrichment
records, retaining self-contained row data/evidence and shared leads. This is not
complete erasure. 111 workbook tests pass, including unrelated/future-setting
preservation, foreign-workspace isolation, retained row evidence, scoped legacy
cleanup, repeated-delete behavior and no queued jobs. No live data was deleted.

UI deletion still uses full-list PUT. Its confirmation must describe the endpoint's
cleanup before switching to DELETE. Stale confirmation, dependency handling and
in-flight worker coordination remain open; row-lock code is not yet verified
against concurrent PostgreSQL transactions.

Column creation client cutover: custom columns and both preset groups now POST a
single column through the existing append endpoint, sharing the layout mutation
queue. A synchronous submission guard and disabled pending form prevent repeated
clicks/draft changes during save; failed requests retain the form, and only a
confirmed workbook/column identity closes it and resets drafts. Duplicate IDs
produce explicit review/refresh feedback. The old popup's footer-clipped save
button failed the real-click test, so it now uses a scrollable Twenty dialog.

Production build, lint, diff checks and 54 frontend tests pass. The new add-column
browser mode tests 503/draft/retry, double-click protection, duplicate rejection,
preservation of a remotely changed width, reload persistence, and waterfall/AI
presets against intercepted APIs. Backend sequential behavior was separately
covered by 109 workbook tests. Column deletion remains the editor's full-list
configuration writer; PostgreSQL concurrency and full accessibility/performance
acceptance remain open. Nothing deployed.

Column-add endpoint hardening: the existing POST columns endpoint now reads the
latest workspace-owned workbook under a row lock, appends without replacing
stored column dictionaries, rejects duplicate IDs with 409 and rejects blank
identity/name or out-of-range width. Duplicate IDs previously made targeting
ambiguous. 109 workbook tests pass, including newer-width/future-field retention,
unchanged row evidence, explicit empty tools/false/zero policy, foreign workspace
rejection, duplicate retry rejection and no queued jobs. SQLite tests establish
sequential behavior only, not PostgreSQL lock contention. The editor's add flows
still use full-list PUT and must now be connected to the hardened endpoint with
retained drafts and honest failure/retry handling. No live data was changed.

Standalone Rename now uses Twenty Dialog/Input/Button and the targeted settings
API, capturing the expected name when opened rather than rewriting all columns.
Failed saves retain the draft; an immediate ref guard prevents duplicate submits,
and dismissal is blocked during a pending save. Focus restoration waits for the
confirmed name to render before locating the current header button, fixing a
receipt/rerender race found by the browser test. The wide fixture passes injected
503, repeated-click protection, explicit retry, exact name-only payload, focus
return and reload persistence with the prior width intact. Build/lint/diff checks
and 54 frontend tests pass. Add/delete column still send full configurations;
their concurrency migration remains open. Browser acceptance uses fixture APIs.

Column-settings client integration: configuration-panel edits now send only the
changed fields and their expected prior values to the targeted PATCH, sharing the
workbook layout mutation queue. Undefined clears become explicit null; successful
receipts must match the requested identity and values before cache merge. Failures
retain submitted arguments, announce the error, suspend further settings edits
and run/delete actions, and offer retry or explicit discard-and-readback. A failed
readback does not discard the retained request. Other full-list writers elsewhere
in the editor remain outside this change.

54 frontend tests, production build, lint and the extended 50-column browser
fixture pass. The browser injects 503 then 409 with a remote rename, checks retained
draft/request, reloads the newer name, and saves against that name while preserving
the 240px width. Client tests cover encoded IDs, exact request scope, nested
zero/false/empty values and mismatched/error receipts. This is intercepted browser
evidence plus the separately tested backend endpoint, not PostgreSQL concurrent
transaction or live GTM acceptance. Per-control execution-setting coverage and
broader config-writer concurrency remain open.

Targeted column-settings API foundation: editor-only workspace-scoped PATCH
`/api/workbooks/{workbook_id}/columns/{column_id}/settings` accepts changed fields
plus matching expected old values. Under the workbook row lock it compares only
those fields, rejects stale edits (409), validates against ColumnConfig, and merges
only supplied fields into current JSON. Identity/type/width and unknown patch keys
are excluded; unknown stored fields, order and unrelated settings are preserved.
Missing/null expected optional values are equivalent. Error responses do not echo
prompts or configuration. The endpoint does not queue execution.

104 workbook tests pass, including stale rejection, preservation of a newer width,
cross-workspace/missing targets, invalid keys/values without mutation and explicit
zero/false/empty values. Tests use disposable SQLite; PostgreSQL concurrency is
not established. The settings panel is not connected yet, so its legacy full-list
save remains a release risk. Next is client integration with explicit failure and
conflict recovery; this API alone does not resolve the user-facing race.

Column settings now uses Twenty's modal dialog primitive in a right-side layout,
with a named title, contained keyboard navigation, Escape dismissal and full-width
375px presentation. Focus restoration resolves the current header control by
column ID because saving replaces the originally focused DOM node. The browser
fixture verifies forward/reverse Tab containment, mobile bounds, Escape return,
and close-button return after saving/re-rendering. Existing wide interaction and
width failure/retry checks pass, as do build, lint and diff checks. Removed the
duplicate legacy width input that still saved full column configuration; width
editing now has one targeted form path. Other configuration fields still use the
legacy full-config save and need concurrency/accessibility work. These fixture
checks do not establish complete screen-reader or touch-device acceptance.

Single-pointer width alternative: a named settings button in each column header
opens the existing configuration panel without sorting or dragging. Its new
Twenty Input/Button width form accepts only integers 80–600, writes only the
target column width through the existing serialized mutation, prevents duplicate
submissions and updates grid geometry only after a successful receipt. Failed
saves retain the draft with an inline announced error and explicit retry; success
is announced. The panel close control now has an accessible name.

The 50-column fixture passes mouse/keyboard/reorder checks plus empty/out-of-range/
fractional validation without requests, a failed form save, double-click guard,
retained draft and unchanged grid on failure, retry and reload persistence after
reordering. Build, lint, 53 frontend tests and diff checks pass. This is fixture
evidence, not live backend or complete accessibility acceptance. The existing
configuration panel's focus management and actual touch-device testing remain
open. The benchmark now ends with a 240px column; do not compare its timing to
earlier differently sized layouts as a performance improvement.

Keyboard width controls: column resize handles are focusable named separators
with current/min/max width semantics and visible focus. Left/Right adjusts 10px,
Shift adjusts 50px, and Home/End selects the 80/600px bounds. Events do not activate
the parent drag sensor. Width writes use the existing identity-scoped mutation
queue and retain explicit save-failure feedback. The 50-column browser fixture
verifies focus, rendered/announced width, bounds without redundant writes, no
accidental reorder and reload persistence after prior column reordering. This
exposed and corrected a fixture that updated the first column rather than the
requested ID. Build, lint, 53 frontend tests and the wide failure/recovery fixture
pass. Its final timing sample now includes a 600px column and must not be compared
directly with previous 320px samples. A click/tap alternative to resizing and full
assistive-technology acceptance remain open; keyboard support alone does not
close the dragging-movements gate.

Current regression sweep: 53 frontend tests and 151 selected backend tests pass
(Anthropic client, durable batch attempts/prepass and workbook router/views).
Browser fixtures pass selection/deletion, saved views, evidence, run review,
history, CSV loading and CSV save recovery. The 50-column/1,000-row fixture also
passes editing, range clipboard, resize, keyboard/pointer reorder and injected
width-save failure recovery. Its latest observational scroll sample was median
49.8 ms / p95 52.2 ms with 534 mounted cells; this is not field INP or a controlled
before/after comparison.

The selection verifier previously silently ignored wide-interaction flags unless
benchmark mode was also selected. It now rejects incompatible/ignored modes,
checks unexpected fixture API requests in benchmark mode, and reports which
interaction/recovery checks actually ran. An initial ordinary selection pass was
not counted as wide acceptance; the correct 50-column commands were run separately.
These checks do not close live-provider, PostgreSQL concurrency, full accessibility
or route-payload release gates. The complete Workbooks slice remains unaccepted.

Agent budget check: tests now run the real planner against fixture provider costs
and a fake provider, asserting no call with zero/insufficient cell headroom or
insufficient/exhausted workbook headroom, and one persisted catalog-cost debit
for an affordable success. Positive-but-insufficient headroom previously reported
`exhausted`; it now reports `budget`, excluding already tried providers. All 74
workbook tests pass. This is not a spend guarantee: the current agent only debits
paid successes, does not model potentially billable failed attempts, and does not
atomically reserve shared workbook budget across concurrent cells. Resolve those
execution gaps before claiming hard spend enforcement or production acceptance.

Creation and agent-selection checkpoint: execution-setting preservation tests
now exercise both POST creation and PUT save with database/GET readback. Agent
execution previously treated explicit `tools: []` as missing and expanded it to
default providers; it now copies the explicit list (including empty), falling
back only for absent/null tools. Tests capture planner candidates for empty,
restricted and default selections and forbid provider calls for an exhausted
plan. Workbook/research suites pass (85 tests). Planner/provider execution is
mocked in these selection checks; actual provider outcomes and budget charging
are not established by them.

Browser reorder recovery checkpoint: the wide fixture now injects an HTTP 503,
then a 409 after a simulated second client changes another column pair. Both
show the exact failure message, trigger detail readback, leave the requested
move uncommitted and make exactly one attempt. After conflict, the grid reflects
the changed server order; the subsequent leftward retry and pointer reorder
preserve that other change in their identity-only payloads and survive reload.
The complete 50-column fixture passes. This verifies browser recovery against
intercepted requests, not simultaneous transactions in production PostgreSQL.

Targeted reorder client integration: the editor now PATCHes only desired and
expected prior column IDs, never full execution configuration. Width and reorder
mutations share a workbook-local queue. Reordering is ignored while its save is
pending; settled requests invalidate workbook detail, and errors display explicit
failure/conflict feedback. Receipt validation requires exact ordered IDs. The
50-column browser fixture passes keyboard right/left, cancellation, pointer drag,
reload and width retention using the new endpoint, asserting identity-only
payloads. 50 frontend tests, lint, build and diff checks pass. Browser-level
conflict/error recovery and simultaneous cross-client writes remain unverified;
other full-configuration writers are not made concurrency-safe by this change.

Targeted reorder API foundation: editor-only, workspace-scoped PATCH
`/api/workbooks/{id}/columns/order` accepts only desired IDs and expected prior
IDs. It locks the workbook row, rejects stale order with 409 and invalid
permutations with 422, and rearranges the latest stored dictionaries without
reserializing execution settings. Database tests prove a newer width and unknown
future settings survive, stale/invalid requests do not mutate, foreign workspace
access is rejected, and no jobs are queued. All 61 workbook router tests pass.
The UI still uses full configuration PUT; client integration is the next step.
SQLite tests do not prove simultaneous PostgreSQL lock behavior, and other
full-configuration writers still require concurrency treatment.

Execution-settings schema audit: the existing agent engine reads `goal`, `tools`
and `policy`, the output runner reads `run_once`, and batch AI execution reads
`max_tokens`; those fields were absent from the API column schema and silently
discarded on full configuration saves. They are now explicit schema fields,
with typed nonnegative finite agent limits and positive integer token limits.
Omitted policy values retain engine defaults; explicit zero/false/empty values
survive serialization. Client column types now include these execution settings
and the existing research/verification settings. Added database tests verify
save/readback, no execution jobs, and invalid-limit rejection without mutation.
76 workbook/research tests and 30 additional workbook batch/import/load/scoping
tests pass, alongside 49 frontend tests, lint, build, and diff checks. This
establishes configuration preservation, not agent effectiveness or actual spend
enforcement. Concurrent full-configuration saves remain an open risk.

Pointer reorder and persistence checkpoint: the wide browser fixture now drags
the 320px Company header past its 100px neighbor, verifies the exact third order
update, no extra width write, and order/width retention after reload. It passes.
A new disposable-database reorder test exposed loss of `cell_budget_usd` through
the column schema. The schema now accepts nonnegative finite research budgets;
tests verify budget/prompt/width preservation, unchanged row data/evidence, no
queued jobs, invalid-budget rejection without mutation, and foreign-workspace
rejection. Workbook and native-research suites pass together (65 tests).
The latest browser timing sample had p95 171.7ms despite median 58.8ms; performance
acceptance remains open, not waived by the functional passes. Other execution
configuration fields and concurrent configuration updates still need auditing.

Keyboard reorder fix: keyboard targeting now centers the dragged header on the
adjacent visible column, matching center-based collision detection. The default
sortable edge alignment left a 320px column over itself when moving toward a
100px neighbor. The 50-column/1,000-loaded-row browser fixture passes rightward
reorder, exact configuration payload, reload order/width retention, Escape
cancellation without another write, and committed leftward reorder. The same
fixture covers editing, range clipboard, resizing and failed-width-save retry.
Production build, lint and 49 frontend unit tests pass. These are intercepted
API browser checks, not proof of live backend reorder persistence or full app
acceptance; pointer reorder and other remaining gates still need coverage.

Reorder acceptance uncovered an interaction bug: resize-handle pointer events
also activated header dragging, sometimes reordering columns during resize.
The handle now stops pointer propagation. Sortable keyboard coordinates are
configured, but the expanded wide interaction fixture still FAILS its explicit
keyboard reorder assertion: Space/Right/Space drops over the original column and
does not send the expected configuration update. This must be diagnosed before
marking wide interaction acceptance complete. Build passes; do not cite the earlier
green wide fixture as proof of reorder correctness.

Width failure browser checkpoint: `--fail-first-width-save` injects an HTTP 503
for the first resize, verifies explicit unsaved feedback and unchanged stored
fixture width, then resizes again and waits for a successful receipt. Reload
shows the retried width with exactly two attempted requests. The complete wide
keyboard/edit/clipboard/resize fixture passes. Overlapping rapid width requests
and cross-client configuration races remain separate checks.

Resize persistence integration: mouse release sends the targeted integer width,
using a workbook-scoped mutation queue so width saves execute in order. Confirmed
receipts update cached column configuration; failures notify that the width was
not saved. The 50-column browser fixture now verifies one PATCH and width retention
after reload. Client tests reject HTTP errors and mismatched receipts. Build/lint
pass. Cross-client config races and rapid-save/failure browser scenarios remain
unverified; mutation serialization here applies to width requests in this client.

Width persistence API foundation: editor-authorized, workspace-scoped PATCH
`/api/workbooks/{id}/columns/{column_id}/width` accepts only integer widths 80–600.
It changes only the named width and preserves research prompts/budgets and other
columns; it queues no execution. Tests verify database/GET readback, missing and
foreign targets, and malformed bounds/types. UI resize is not yet connected;
serialized saves, failure feedback, and reload browser checks are next.

Wide-table resize checkpoint: browser interaction drags Company 80px wider,
checks exact matching header/body widths, navigates to the final column and back,
then confirms the remounted cell retains the new width. The 50-column interaction
fixture passes alongside keyboard/edit cancellation/copy/paste checks. This tests
in-session geometry only: the existing resize handler stores widths in a local
ref, so reload persistence is not established. Reorder acceptance remains open.

Wide-paste persistence checkpoint: disposable-database API tests patch 50 input
fields, then verify exact database and GET readback including Unicode, zero,
false, empty strings and preservation of unrelated fields. Mixed requests with
a row from another workbook (same or different workspace) reject atomically:
neither row changes and no recomputation job is queued. All 47 workbook router
tests pass. This complements, but is separate from, intercepted browser tests.

Wide-table clipboard checkpoint: the browser fixture selects from Company through
the last of 50 columns with Shift+End and verifies the actual clipboard contains
all 50 logical values, including unmounted cells. Pasting a 50-field TSV emits one
exact row-ID/field-map update and returns focus to the final target column. All
requests are intercepted; this proves frontend payload/focus behavior, not real
backend persistence. Navigation uses Home/Arrow keys to remount offscreen targets;
tests do not assume those cells permanently exist in the DOM.

Wide-table keyboard coverage now exercises Home/End across the horizontal window,
F2 editing at the last column, keeping an unsaved focused draft mounted while
scrolling away, and Escape cancellation without writes. Escape now restores focus
to its grid cell explicitly. Build/lint pass. Remaining wide-table gates include
resizing, reorder, saving/paste and cross-window range copying.

Horizontal body rendering is now integrated using exact-width colspan spacers,
retained active-column cells, scroll/resize viewport tracking and horizontal
keyboard scroll targeting. Headers remain mounted for resizing/reordering. Table
width is explicit so scroll geometry matches the column calculation. A 50-column
fixture now reaches the rightmost column and back; a local post-fix timing run
reported median 56.3 ms / p95 67.9 ms with 534 body cells versus the earlier 2,174.
Build passes. Wide-table edit/resize/reorder/copy/paste and focus retention need
further acceptance coverage; do not treat one timing run as the full gate.

Horizontal rendering work in progress: `workbook-column-window.ts` computes
viewport columns and exact-width spacer segments while retaining absolute indexes
and explicitly retained focused cells. Unit tests cover a 50-column viewport,
offscreen focus retention, resized width boundaries and invalid inputs. It is not
yet wired into the editor: integration must update headers/body together, preserve
resizing and offscreen keyboard focus, and pass browser editing/selection tests
before any performance improvement can be claimed.

Date: 2026-09-22
Status: compatibility and theme foundation are implemented in the worktree; grouped shell navigation is implemented and browser-tested with fixtures. Shared primitive replacement and the real workbook vertical slice remain next. These are worktree milestones, not merged or fully accepted PRs.

Compatibility follow-up: direct use of published 2.41.0 is rejected. The pinned-source artifact is now reproducibly packaged and installed, with a shared Router 7.18.4. The isolated repository preview passes browser interaction checks. Full-app CSS, routing, and workflow integration gates remain open. See the [compatibility report](twenty-ui-compatibility-report.md).

## Product contract

Adopt Twenty's visual system, theme architecture, reusable controls, and data presentation patterns while preserving OpenGTM's product model. OpenGTM remains a GTM workspace organized around agents, research, evidence, workbooks, enrichment, audiences, signals, and activation.

Keep the OpenGTM name, logo, and restrained brand accent. Preserve exact entity selection, provider order, cost previews, independent verification, action approvals, draft/send separation, workspace isolation, persisted receipts, retry/cancellation, and existing deep links. A visual migration must not alter any of these behaviors.

Scope: authenticated web application, including login and settings. The documentation/marketing site is a separate design surface and is not part of this migration.

## Evidence and compatibility findings

Reference source revision: `twentyhq/twenty@e99fd1a7683f37a957852c3f7ef8f6a29732bb38`, inspected on the date above. Links below are pinned where source code is cited.

- [Twenty UI](https://github.com/twentyhq/twenty/blob/e99fd1a7683f37a957852c3f7ef8f6a29732bb38/packages/twenty-ui/README.md) is a standalone React 19 library with prebuilt CSS and public component subpaths. Its documentation labels it alpha.
- The [package manifest](https://github.com/twentyhq/twenty/blob/e99fd1a7683f37a957852c3f7ef8f6a29732bb38/packages/twenty-ui/package.json) at that revision is version 2.42.0. The npm registry currently reports 2.41.0 as latest. These are distinct artifacts; do not treat current source documentation as proof of published API compatibility.
- Published 2.41.0 declares React Router 6.30.6, Base UI 1.8, and editor dependencies. Inspected main declares Router 7.18.4 and separates optional editor peers. OpenGTM currently uses React 19.2, Router 7.14, Base UI 1.4, Tailwind 4, TanStack Table/Virtual, and Geist.
- [Getting started](https://docs.twenty.com/ui/getting-started) documents a global CSS reset. It can affect existing Tailwind controls, typography, and tables; loading it is a migration step, not an innocuous import.
- [Dark mode](https://docs.twenty.com/ui/dark-mode) exposes explicit light/dark schemes. Application code owns persistence and system preference handling. Scoped themes need explicit treatment of portalled menus and dialogs.
- [Spacing](https://github.com/twentyhq/twenty/blob/e99fd1a7683f37a957852c3f7ef8f6a29732bb38/packages/twenty-ui/design-tokens/spacing.ts) follows 4px increments, with 2px and 6px half steps. [Border tokens](https://github.com/twentyhq/twenty/blob/e99fd1a7683f37a957852c3f7ef8f6a29732bb38/packages/twenty-ui/design-tokens/border.ts) include 4px/8px small/medium radii. [Typography](https://github.com/twentyhq/twenty/blob/e99fd1a7683f37a957852c3f7ef8f6a29732bb38/packages/twenty-ui/design-tokens/font.ts) specifies Inter and semantic text tiers. Values expressed in rem must be evaluated with the shipped reset; do not assume the effective size.
- Twenty's [record table](https://github.com/twentyhq/twenty/blob/e99fd1a7683f37a957852c3f7ef8f6a29732bb38/packages/twenty-front/src/modules/object-record/record-table/components/RecordTable.tsx) depends on object metadata, permissions, and application state. It is not a drop-in standalone grid. Its [32px row height](https://github.com/twentyhq/twenty/blob/e99fd1a7683f37a957852c3f7ef8f6a29732bb38/packages/twenty-front/src/modules/object-record/record-table/constants/RecordTableRowHeight.ts) is a desktop reference, not a mandatory touch target.
- The [UI package license](https://github.com/twentyhq/twenty/blob/e99fd1a7683f37a957852c3f7ef8f6a29732bb38/packages/twenty-ui/LICENSE) is MIT. Preserve its notice for reused code. The [repository license](https://github.com/twentyhq/twenty/blob/e99fd1a7683f37a957852c3f7ef8f6a29732bb38/LICENSE) distinguishes the rest of the application and enterprise-marked files. Prefer the public UI package; do not import enterprise code or Twenty branding.

## Current OpenGTM migration surface

| Surface | Current implementation | Work required |
| --- | --- | --- |
| Tokens | `apps/web/src/index.css`: warm light palette and very dark palette | Replace palette and dimensions with semantic Twenty mappings, including a documented OpenGTM accent overlay |
| Theme state | `index.html`, `src/main.tsx`, `src/App.tsx` each manipulate theme | One preference controller; early boot script consumes the same preference contract |
| Shared controls | `src/components/ui/*`: local Shadcn/Base UI wrappers | Preserve application-facing contracts while replacing internals or composing Twenty primitives |
| Shell | `src/App.tsx`, 13 flat primary navigation entries plus recent chats | Extract shell; grouped navigation; workspace switcher; compact context header; consistent panels |
| Generic tables | `components/data-table.tsx`, `leads-table.tsx` | Shared table chrome, field presentation, selection semantics, saved view controls |
| Workbook | `pages/workbook-editor.tsx`, 2,526 lines with existing keyboard grid and virtualization | Extract rendering/presentation before restyling; keep run logic, APIs, editing, and data hooks intact |
| Agent interaction | `pages/chat.tsx`, plan cards, task cards, receipts | Compact structured results with actionable selections and evidence drawers |
| Quality tooling | Web package has build/lint scripts, but no declared browser/visual test command | Add reproducible browser interaction, accessibility, and screenshot coverage before broad replacement |

## Architecture decision

Use a local OpenGTM design-system boundary around Twenty UI. Pages import OpenGTM wrappers; wrappers consume Twenty's public components and semantic tokens. This keeps upstream alpha API changes confined to one layer and makes product-specific evidence, spend, and execution controls consistent.

Keep React Router, TanStack Query, Table/Virtual, authentication, workspace context, and existing API contracts. Tailwind can remain for layout; it must resolve colors, typography, radii, and spacing through the new system. Remove obsolete visual definitions and unused Shadcn infrastructure after migration, not the behavioral code still serving unmigrated pages.

Proposed organization:

```text
apps/web/src/design-system/
  theme/       preference controller, token mappings, brand overrides
  primitives/  button, field, menu, dialog, tabs, toast adapters
  patterns/    page header, view toolbar, record panel, data states
  data/        typed cell renderers, selection toolbar, evidence display
apps/web/src/components/app-shell/
  navigation, workspace menu, command menu, context header
```

During transition, existing `components/ui/*` imports delegate to adapters. Compatibility wrappers have a tracked removal list. Do not leave two active theme controllers, two toast systems, or competing reset styles after cutover.

### Package selection gate

First test the published package in a small isolated preview against our actual React/Vite/Router stack. Verify routing context, controlled inputs, focus management, refs, portal placement, class overrides, reset interaction, tree shaking, and production bundle output.

Prefer an exact published version if those checks pass. If they fail due to the known Router/editor mismatch, evaluate a reproducible build of the pinned MIT UI source with notices and a recorded patch list. Do not downgrade OpenGTM's router, install a floating Git dependency, or import Twenty's complete CRM to make the UI compile. Record the chosen artifact and lockfile before implementation continues.

## Visual and interaction direction

Use neutral light surfaces and layered dark surfaces, fine separators, compact typography, quiet toolbars, and one primary action per context. Inter is the proposed UI face to align with Twenty. Self-host font assets. Validate effective font sizes and contrast rather than inheriting tiny labels blindly.

Twenty supplies the visual grammar; OpenGTM supplies domain semantics:

| System layer | Proposed use in OpenGTM |
| --- | --- |
| Surface tokens | Canvas, navigation, record panel, menu, selected row, hovered cell |
| Text tokens | Primary content, secondary metadata, placeholders, disabled controls; readable evidence metadata |
| Accent | OpenGTM accent for focus, selected controls, and primary action emphasis; avoid coloring every surface |
| Status | Distinct labeled states for queued/running/partial/failed, verified/risky/unknown, and draft/sent |
| Density | Compact desktop table mode (32–36px candidate range) and comfortable mode (40–44px); touch actions at least 44px |
| Motion | Short functional transitions; reduced-motion support; no animated background or decorative dashboard movement |
| Numbers | Tabular numerals and aligned units for cost, counts, confidence, and timing |

Semantic mapping examples: current `--background` maps to the chosen Twenty canvas surface; `--foreground` to primary text; `--muted-foreground` to readable secondary text; `--popover` to floating surface. Border and ring tokens remain separate. Store exact verified mappings after the package spike instead of inventing raw colors in this plan.

### Navigation proposal

- Workspace menu at the top; global search/command entry immediately available.
- Workspace: Chat, Workbooks, Leads, Audiences.
- Execution: Tasks, Automations, Watches, Signals, Outreach.
- Resources: Sources, Templates, Analytics; Settings in the utility area.
- Preserve the Search route through a clear navigation or command entry. Keep existing URLs and browser history behavior.
- Recent chats belong in a collapsible section and in Chat's own history surface; they must not crowd out product navigation.
- Show detailed provider usage and connection health on demand; retain a visible actionable warning when connectivity or budget prevents work.

These are navigation groupings, not renamed backend concepts or permissions.

### Data presentation contract

Workbooks, Leads, and Audiences share a view toolbar: saved view, search, filters, sort, columns, density, and context-appropriate action. Bulk selection reveals an action bar with exact selection scope. Distinguish current page, loaded records, and all matching records before any bulk action.

Retain workbook spreadsheet editing and virtualization. Add consistent typed renderers for company/person identity, URL, email, money, timestamp, score, verification, and job status. Long values truncate with an accessible way to inspect/copy the full value. Missing, loading, unavailable, and failed values are visually distinct.

Record details open in a side panel while keeping table position, filters, and selection. Deep links must reopen the correct record in the correct workspace. Evidence, provider attempts, retrieval dates, costs, and errors are available in the panel. On narrow screens it becomes a full-width detail view with a predictable Back action.

Chat renders researched people and companies as structured selectable results using the same field presentation. Follow-ups such as verify, save, enrich, track, and draft operate on the existing selection IDs. Keep honest partial results and persisted receipts visible. Retain explicit approval before actions that currently require it.

## Delivery sequence and exit gates

| PR | Deliverable | Required exit evidence |
| --- | --- | --- |
| 0 | Inventory and compatibility preview | Exact artifact selected; existing route/state inventory; dependency/reset/bundle findings; screenshots of representative controls in both themes |
| 1 | Tokens, theme provider, primitive adapters | Light/dark/system persistence; old preferences honored; no initial theme flash; dialogs/menus themed correctly; disabled/loading/error/focus states checked |
| 2 | App shell and navigation | Every existing route reachable; workspace switching retains isolation; deep links, browser Back, command palette, mobile navigation, and keyboard focus work |
| 3 | Complete Workbooks vertical slice | List → saved view → edit/select → estimate → approve/run → progress → inspect evidence → retry/cancel works using existing contracts |
| 4 | Leads, Audiences, record detail, Search | Shared field rendering and view controls; stable record-ID selection across sorting/refetch/pagination; role restrictions and exact record links preserved |
| 5 | Chat, Tasks, Signals/Watches, Automations, Outreach | End-to-end GTM journey works; structured receipts readable; no lost drafts, changed recipient scope, duplicate actions, or hidden recovery controls |
| 6 | Sources, Settings, Templates, Analytics, Campaigns, workspace manager, login; remove old visual system | Route inventory complete; no orphan controls or old palette; lint/build/browser/a11y/visual gates pass; rollback checkpoint and migration notes recorded |

Do PR 3 as a fully functioning reference screen before repeating the new patterns across the app. Do not make the workbook migration only a screenshot or a static mock dataset.

## Verification and rollback

No redesign can promise zero mistakes. Minimize regression risk with explicit, repeatable gates:

1. Capture the current screen/state inventory and current functional failures before edits. Record baseline bundle size and interaction timing.
2. Browser coverage includes both themes, system theme changes, 375/768/1280/1440px widths, 200% zoom, long names, empty datasets, pending requests, permission denial, provider errors, and network interruption.
3. Keyboard flows cover command search, menu/dialog focus return, grid navigation and editing, range copy/paste, selection, resize alternatives, and record-panel Back behavior. Do not trigger shortcuts while typing in inputs.
4. Accessibility checks cover accessible names, meaningful contrast (4.5:1 normal text), visible focus, non-color status labels, reduced motion, and manual keyboard review. Automated checks supplement manual inspection.
5. Data regression tests use stable row IDs and include sorting/filtering/refetch while selected. Preserve query parameters, saved views, column configuration, virtualization, and exact action inputs.
6. Measure large workbook behavior using existing load-test datasets and the same hardware/browser baseline. Investigate any >10% regression in measured scroll/edit latency or route payload before merging; prevent unbounded DOM row rendering.
7. Run `bun run lint` and `bun run build` from `apps/web`, plus the new browser suite. Run relevant backend workflow tests where UI request construction changes. UI polish is not evidence of successful live GTM outcomes.
8. Keep each PR revertible without a database rollback. Preserve old preference values and saved workbook/view data. Cut over after acceptance; remove temporary migration flags and deprecated styles in PR 6.

## Planning result / next step

The decision is to adopt Twenty's actual design system through an OpenGTM adapter layer and adapt its data UX to OpenGTM's existing engines. The first implementation is the compatibility preview and theme/component specimen, followed by the shell and the complete workbook slice.

This document was written before implementation as requested. Subsequent work has added the candidate library, a reproducible build recipe, a Router dependency pin, a separate preview entry, and the production theme/token foundation. Screen structure, workbook behavior, and backend behavior have not been replaced. UI/UX Pro Max informed the keyboard, responsive-table, focus, and accessibility gates; its generic marketing-page recommendation was not used as the product design direction.

Progress update: the grouped application shell and actual workbook list/create/template/delete interfaces are now implemented locally. The list includes search, status filtering, sorting, and pagination. Unit and intercepted-API browser tests cover these flows; they do not establish live GTM outcomes. PR 3 is still incomplete: the editor, selection, estimate/approval/run, evidence, and recovery journey is the next reference slice. No migration deployment has occurred. See the compatibility report for detailed evidence and remaining gates.

Editor update: Twenty bulk-action controls and scoped deletion confirmation are
implemented, with browser coverage of cross-page selection, editing, cancellation,
retry, duplicate submission, and focus. A legacy global-lead deletion fallback was
removed. PR 3 remains open: the main toolbar, saved views, estimate/approval/run,
progress, evidence, and recovery must still be migrated and verified together.

Run-review update: the main toolbar's normal/fill-missing actions now show a
Twenty review dialog with query scope, catalog estimate limitations, and external
output warnings. An optional backend reviewed-count check rejects drift before
billing/enqueue. Browser and offline router tests cover this gate; immutable
approval snapshots, server idempotency, live execution receipts/progress, and
evidence/recovery remain open. Also audit column normalization in the editor:
the legacy allow-list currently treats research/HTTP/formula columns as input
fields even though the backend supports them. Preserve those types during the
remaining renderer/editor migration rather than silently changing their meaning.

Receipt update: that column-type normalization issue is fixed and covered by
unit/browser checks, including zero/false values. Workbook-specific durable run
history now has a tenant-scoped API, claim-guarded result persistence, and a
Twenty dialog. A disposable-database integration test executes real formulas
through the router, queue, worker, and receipt readback. PR 3 remains open for
evidence/recovery, per-run live progress and concurrency semantics, saved-view
and toolbar migration, performance/accessibility gates, and live GTM acceptance.

Next evidence gap confirmed in code: research results store citations and stop
reasons under `WorkbookEnrichment.cell_metadata.research`, but the row overlay
schema/read path currently exposes only verification and basic provenance.
Trace that metadata through persistence and the API before adding the evidence
panel, including no-answer cells and safe external citation links.

Evidence persistence update: research metadata now mirrors into workbook row
overlays and has an explicit API schema for citations, answer, cost, stop reason,
and synthesis fallback. The legacy enrichment read path also exposes research.
Disposable-database tests verify saved row/API readback for answered and no-answer
cells. Live socket delivery, historical row backfill, and the evidence inspection
UI remain open; this is the persistence foundation, not the completed evidence
journey.

Live evidence transport update: terminal worker cell events now carry research,
verification, and provenance metadata; the browser cache retains those fields.
Cell-event batching keys on the workbook row ID when available rather than only
the linked lead ID. Single-cell execution responses expose research too. Offline
tests execute mocked research through the real cell runner and check response,
saved API readback, and captured worker events for answered/no-answer outcomes.
The evidence inspection UI and browser-level socket acceptance remain pending.

Evidence inspector implementation: research cells now expose a visible sources
button, including zero-source/no-answer cells, opening a Twenty dialog with the
full answer, quotes, source links, recorded cost, and stop reason. Missing retrieval
dates are explicitly labeled, not invented. Citation links allow only HTTP(S)
without credentials; source text is rendered as text, never injected HTML.
Keyboard-visible controls and a scrollable body follow the UI/UX skill's focus
guidance. Unit checks cover unsafe URL rejection. Browser focus-return, narrow
viewport, theme, and live-socket acceptance remain open before sign-off.

Evidence browser follow-up: `check-workbook-evidence.py` now passes keyboard
Enter/Escape/focus return, light/dark at 375/1440px, visible Close control,
safe links and literal untrusted quote text, no-answer inspection, and live
socket evidence delivery into the second unlinked row. It exposed and fixed
grid shortcuts intercepting nested button activation and dialog width overflow.
This uses intercepted API/socket fixtures, not a live provider acceptance run.

Research evidence integrity follow-up: the native fetch harness previously added
URLs to its citation allowlist even after scraper exceptions or empty/error pages.
Only successful, nonempty fetches now enter that set. Offline native-research
tests cover exceptions, HTTP failures, scraper error states, and blank content;
29 research tests pass. Fetch timestamps remain unrecorded and must not be inferred
from workbook update time. Citation eligibility proves a page was read, not that
every generated claim or quotation is independently verified.

Freshness follow-up: successful native fetches now record a runner-generated UTC
timestamp per URL. Validated citations carry that timestamp through persistence,
API readback, and the inspector. Older sources remain explicitly undated; model
output cannot supply the timestamp. This is fetch time, not publication time or
proof of current employment. The 67 offline research/workbook tests pass, with
production build and the evidence browser fixture covering dated/undated sources.

Historical evidence readback: older v2 row overlays that omitted research now
recover metadata from matching stored enrichment receipts without rerunning paid
research or mutating rows. Lookup is limited to the visible page, research columns,
owned workbook and current workspace; value/status must match before attaching
evidence. Existing explicit research overlays win. Tests cover matching historical
evidence, stale values, foreign-workspace receipts, and read-only behavior.

Retry evidence lifecycle: metadata-free writes now clear the previous cell
receipt metadata instead of retaining old citations/verification. Regression tests
cover running, failed, and same-text successful replacement results, including
API readback through the historical fallback. This prevents stale evidence from
being resurrected after a retry. The 62 workbook/batch/cache tests pass.

Quotation integrity: the native runner retains the bounded text it actually read
and only persists a citation quote if that excerpt occurs in the text (whitespace
layout differences allowed). Unmatched/model-invented quotes are removed while
the fetched source link remains available. This does not establish semantic claim
support and does not retrospectively validate old stored quotes. The 34 native
research tests pass, including missing-source, altered-case, invented-text, and
whitespace-normalized quote cases.

Regression checkpoint: 174 backend tests across workbook execution/scoping,
queue recovery/concurrency, research and injection/evaluation gates pass. Saved
view API responses now validate identity, owning workbook, filter/sort operators,
and config arrays before entering UI state; update responses must match the
requested view. Frontend tests now total 46 passing; lint passes. Next: migrate
saved-view controls, retain drafts on errors, and cover their real browser flows.

Saved-view naming implementation: create/rename use Twenty dialogs and labeled
inputs in place of custom fixed overlays. Failed requests retain draft names and
show inline alerts; synchronous pending guards prevent repeated Enter submissions.
Dialogs cannot dismiss during pending mutations. TypeScript and lint pass;
browser focus return, menu-to-dialog transitions, failure/retry and duplicate
submission checks remain required before acceptance. View selection/filtering and
deletion controls still await migration.

Saved-view browser checkpoint: `check-workbook-views.py` passes menu-to-dialog
create/rename, failed-save draft retention, retry, duplicate Enter events producing
one create attempt, and visible mobile actions in light/dark. Production build
passes. APIs are intercepted fixtures; focus-return and full filter/delete flows
remain separate acceptance gates.

Filter recovery: Apply & save now closes only after success. Failure keeps the
draft open with an inline alert; pending submissions are guarded and background
view refreshes do not overwrite an open draft. Controls have accessible labels,
disable while saving, and wrap within narrow viewports. The saved-view browser
fixture passes failure/retry with exact saved filter payload; build/lint pass.
The legacy filter popover and view menu still need Twenty primitive migration.

Filter surface migration: the popover is replaced by a Twenty dialog with a
scrolling body and persistent Add/Cancel/Apply footer. Browser checks pass saved
filter payload, failure/retry, Escape/cancel without writes, draft reset on explicit
cancel, and focus return to the filter trigger after saving/closing. Build/lint
pass. The saved-view menu and delete confirmation remain unmigrated.

Saved-view deletion now uses a Twenty confirmation naming a captured view target,
explicitly preserving workbook rows. Pending guards prevent repeat submission;
failure remains visible with retry and warns that network interruption can leave
the outcome unconfirmed. Browser fixtures pass cancel-without-write and failed
delete/retry, returning to All rows after success. Build/lint pass. Menu styling,
view-list load errors, and naming/deletion focus return still require follow-up.

Focus-return acceptance: naming and delete dialogs explicitly return focus to the
persistent saved-view switcher. Browser assertions pass after creation, rename,
delete cancellation and successful deletion (including the changed All rows
label). Build and lint pass. The saved-view menu and list-loading/error states
remain the next migration surface.

Saved-view load states now distinguish pending and failed requests, offer retry,
and disable new-view creation until the list is available. An unresolved selected
view no longer appears as All rows or marks that menu choice selected. Browser
coverage includes a failed list response and successful retry before the existing
naming/filter/delete journey. Build/lint and the expanded fixture pass.

Saved-view menu migration: the switcher now uses Twenty Menu through the shared
primitive boundary. The complete fixture journey (list failure/retry, naming,
filtering, deletion, and focus return) passes with the new menu; build/lint pass.
Bundle gate remains open: this build changes shared chunk allocation (entry
451.14 kB / 136.73 kB gzip, primitives 165.17 kB / 53.22 kB gzip). Investigate
route-loaded totals and compare measured interaction performance before acceptance;
successful behavior checks do not establish payload/performance parity.
