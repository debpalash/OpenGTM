# Dashboard transformation completion audit

The full [UI/UX specification](../../specs/dashboard-ui-ux-transformation.md)
remains the acceptance contract. A package is not complete merely because the
shared styling has reached its routes. This ledger preserves the unfinished
requirements alongside the tested increments. The compact workstation direction
supersedes the specification's original spacious dimensions; it does not waive
workflow or accessibility requirements.

## Package review — 2026-10-29

| Package | Evidence in the implementation | Outstanding acceptance |
| --- | --- | --- |
| UI-00: baseline | Navigation regression repaired; 161 frontend tests pass; durable journey, state, matrix, density, and gallery scripts | Expand fixtures as remaining states are implemented; retain the original audit evidence |
| UI-01: analytics trust | HTTP errors and malformed payloads rejected; unavailable/retry and stale data tested; no unconditional Live label | Add deliberate timeout/offline and authorization cases beyond the recorded 503/malformed probes |
| UI-02: tokens | App-owned semantic light/dark foundation, readable type, reduced motion, compact geometry, component gallery | Palette/type/import regression gate; audit all interactive gallery states and remaining route-local colors |
| UI-03: primitives | Button/Input adapters share app controls; Field and named IconButton boundary; pending confirmation preserves its label/width; Sources switches/provider reveal named; workbook settings labels, connected help and semantic text colors repaired | Migrate the remaining forms and direct primitive imports; verify popup/invalid/permission states across all primitive families and keep transient notifications clear of inspector/footer actions |
| UI-04: shell | Shared compact page heading, main landmark, skip link, grouped navigation, route focus, mobile drawer and persistent collapsed rail; 13 route families scanned in both themes | Complete all-route landmark/focus coverage, remaining short-height popup and mobile-keyboard checks, manual assistive-technology navigation |
| UI-05: collections | Server filters/sorts/pagination, pinned identity, explicit-ID selection, 28/40/48px row preference, URL search/view/page/record; scoped visibility/order preferences with pinned identity, reset/cancel/apply, cross-tab updates and honest storage failures; defined core-fact completeness and explicit email-verification states | Broader permission/network and selection/touch state matrix across remaining collection workflows |
| UI-06: workbook | Preserved identity in output-only workflows; semantic column headers; roving focus; keyboard resize/reorder; reviewed native enrichment; click-based move/position and numeric width; persistent activity sizing, truthful loaded counts, paginated saved errors, and immediate-keyboard-drop/focus regression checks | Full edit/paste/virtualization focus coverage (including refetch during editing), hidden-column pointer ordering, and complete queued/paused/partial/canceled lifecycle audit |
| UI-07: inspector | Field ownership, source alternatives, paged/searchable Company People, employment history, typed custom fields, version-conflict comparison/rebase with explicit choices, retained drafts across refetch/retry/tabs, shared route/reload/context/sign-out guards, protected pending saves, tab-bound workspace request scope, focus return; URL-selected record survives reload | Broader permission-change/source/employment state matrix; workspace membership revocation beyond the tested session revocation path |
| UI-08: migration | Upload/map/review/import/results; actual limits; grouped ownership, source samples and actual match provenance; editable conflicting suggestions; exact numeric JSON and CSV IDs; partial rejection reports; locked stable-key recovery after lost responses/reload, honest blocked-storage handling and validated receipts; accepted-record links and separate selection for reviewed enrichment; CSV/JSON exports tested | Complete restore/reimport and wider mapping/permission/network matrix; investigate the 10,000-row preview/commit timeout and establish import performance bounds |
| UI-09: metrics | Canonical Overview counts/coverage; explicit legacy Analytics scope, validation/freshness and table alternatives | Date range/timezone and supported comparison-period presentation; complete metric-definition/series-cue review |
| UI-10: activation | Canonical Company/Person member links/snapshots, honest counts, pagination, refresh receipts; extracted inbound setup with reviewed rotation, independent token/history lifetime, truthful clipboard/manual storage acknowledgement, guarded unsaved token and paged history errors; explicit ad-consent checkbox scoped to audience/type; reviewed sync/retry/cancel with scope/permission rechecks, paged run/delivery receipts and canonical links; reviewed membership refresh and schedule enable/interval changes disclose automatic activation; all 12 destination forms have reviewed creation, visible labels, guarded drafts, explicit automatic/ad acknowledgements, permission/scope preflight and unknown-response recovery without reposting | Complete Signals/Watches/Outreach/Campaigns contracts and wider activation state matrix |
| UI-11: administration | Searchable source/provider foundation; instance-provider admin/read-only scope; truthful save/test/default failures, retained drafts and confirmed-save baseline, stale/read retry states; protected provider tabs/context/sign-out; scoped receipts and read-only recovery of unknown saves, atomic provider field writes, persistent filters, explicit draft discard and readable touch forms; Agents links to actual configuration; workspace manager switches through shared auth context | Role/override and global/workspace audit across remaining administration forms; provider/source grouping; other forms' draft/error/permission and multi-setting partial-write verification; workspace create/delete and membership lifecycle audit |
| UI-12: verification | Durable Chromium journey/matrix/state probes; density probes in Chromium and WebKit; automated reflow/text-size/forced-colors/reduced-motion evidence | Full network/role/data/input/browser matrix; VoiceOver/Safari and NVDA/Chrome; actual browser zoom/text spacing; performance baselines and budgets |
| UI-13: architecture | App-owned patterns/primitives, typed audience presentation seam, scoped record queries; shared leave-decision module; extracted inbound, destination-run, audience-control and provider settings workflows; gallery shares build chunks; activity observation/statistics have a tested pure module and feature-owned styles; extracted collection rendering, core-fact display and versioned layout preferences | Complete the remaining workbook, destination and migration extraction, then remove duplicate primitive/token paths after parity |
| UI-14: rollout | Local embedded-build validation and explicit release limitations documented | Five-operator pilot, blocker triage, production metrics, staged route deployment and reviewed rollback drill |

## Hard gates

| Gate | Current evidence | Completion condition |
| --- | --- | --- |
| Tests/type/build/lint | 169 frontend tests; current lint and production build pass; native Go API with embedded UI compiles | Keep passing after the remaining changes; add meaningful coverage for newly completed workflows |
| Automated A/AA | Core route/theme/viewport matrix, journey and density states have recorded axe scans | Zero untriaged violations on the full final release matrix, including newly added states; no blanket rule exclusions |
| Keyboard and screen readers | Recorded keyboard/focus/import/grid probes | Complete VoiceOver/Safari and NVDA critical journeys, including popup boundaries and employment/conflict review |
| Reference geometry | Measured before/after row and chrome geometry, screenshots at pinned sizes | Approve final references and review key alignments within 1px; cover all required states, text spacing and fonts |
| Large data | Server-side pagination/filter/sort retained; native 10,000-Company and 10,000-Person fixture retains 100-row pages, server filters and explicit page selection | Complete workbook virtual-scroll/lifecycle cases and performance budgets; no download-all workaround |
| Backend contracts | Prior canonical records/tenant/version/import/native-run integration evidence retained; current work adds effective activation access and canonical delivery identity without a schema migration; existing bounded IDs/pagination remain | Re-run relevant Go integration tests whenever later request/selection/authorization behavior changes |
| Route state coverage | Core stale/error/empty/view-only and selected popup states tested | Every release route covers applicable loading/empty/error/partial/stale/permission states |

## Required coverage still to close

- Themes: system first paint, cross-tab preferences, and all portaled states.
- Viewports: recorded 320–1920px reflow is a foundation; short-height laptop,
  mobile keyboard, real browser zoom and text-spacing overrides remain.
- Data: final empty/one/100/1,000/10,000 set, Unicode/long/multiline values,
  precision-sensitive identifiers, and unknown employer.
- Network: add slow/timeout/offline, 401/403/409/422/429 and partial mutation
  cases to the recorded 503/malformed/stale/retry coverage.
- Roles: actual admin/editor/viewer overrides, feature-disabled workspace,
  authorization scope, and workspace switching.
- Journeys: finish reviewed audience activation and restore/reimport chains,
  beyond the recorded import/enrich/edit/export and canonical member-link checks.
- Workbook: final hidden/reordered/narrow/wide/virtualized selection and focus,
  plus paused/failed/retried jobs.
- Release: collect lab p50/p95 and main-thread measurements; field p75 LCP/INP/CLS
  and the five-operator pilot require the corresponding deployed environment and
  human participation. Local browser results cannot certify them.

Firefox is installed locally but its headless launch currently fails with
“Could not find profile folder.” WebKit browser automation works; it is not a
manual Safari/VoiceOver certification. These limits do not prevent independent
implementation or Chromium/WebKit verification of the remaining requirements.

Do not close the goal until these outstanding acceptance checks are completed
with authoritative evidence, or explicitly resolved with the user.

The latest density pass retains its measurements, built-app regressions and
release limitations in [Dense workstation verification](workstation/README.md).
The previous density reference remains in [Power workstation verification](power/README.md).

Workspace/session and activation trust checks are retained in
[Trust workflow verification](trust/README.md). The original release gates remain open.

Reviewed audience activation and native API contract evidence are retained in
[Destination workbench verification](activation/README.md). This does not complete
the remaining activation routes or full release gates.

## Destination setup and entry checkpoint — 2026-10-29

[Setup evidence](setup/README.md) records 143 tests/441 assertions, 120 automated axe states on the destination build and four entry states on the later home-route repair. The root URL now redirects through authentication instead of rendering an empty outlet. These are distinct build snapshots. A reproduced destination UTC insert regression is fixed without a migration; historical timestamps were not rewritten. [Go PR readiness](go-pr-readiness.md) remains a separate release decision. The full transformation and its remaining gates above stay open.

## Provider settings checkpoint — 2026-10-29

[Provider evidence](providers/README.md) records transactional multi-field saves, honest unknown outcomes and scoped receipts, read-only reconciliation, retained filtered drafts, explicit discard, compact heading/search and touch controls. Frontend tests now pass 150 tests/476 assertions. These results close this provider defect and preserve the remaining administration and release gates above.

## Collections and overview checkpoint — 2026-10-29

[Collection evidence](collections/README.md) records configurable columns, honest core-fact coverage, scoped browser preferences and native large-data checks. The overview removes repeated prose while preserving counts, freshness, errors and actions. A reproduced WebKit inspector-focus return defect is corrected using the actual trigger. The remaining requirements and full release gates above stay open.

A separate WebKit probe reproduces an access-control exception when a full document navigation starts immediately after a collection read (12/15 cycles); sidebar SPA navigation passed 15/15. This pending-read/full-navigation case remains open in the wider network/browser audit. Raw evidence is retained with the collection increment rather than excluded from page-error assertions.

This increment passed 161 frontend tests/523 assertions, native journey/state checks, both-engine collection/density checks and 128 axe states across the two documented build phases. The final route matrix and signed-in/out home-entry checks pass on the live embedded build. The separately recorded WebKit mid-request full-navigation exception remains open.

## Import mapping and recovery checkpoint — 2026-10-29

[Import evidence](imports/README.md) records grouped mapping, exact source samples,
editable target conflicts, source ownership, preserved mapping/focus, validated
read-only receipt recovery, bounded metadata, blocked-storage disclosure and
accepted-record links. The result can select imported records for a separately
reviewed enrichment workflow; import starts no run. Feature-owned modules replace
the growing dialog implementation.

This build passed 169 frontend tests/565 assertions, lint and production build,
records/API race unit checks, and the records integration suite with an explicit
large-case exclusion. Chromium and WebKit passed 26 full-document axe scans total,
and the native import/edit/reviewed-enrichment/export journey passed ten more
states. The source fingerprints and live embedded-index hash are recorded.

The broad records integration invocation timed out after six minutes in
`TestTenThousandRowImportPreviewCommitAndReplay`; its log is retained and its
disposable database was cleaned up after the process ended. This leaves a scale
finding open. The passing follow-up explicitly excludes that case and does not
certify 10,000-row import performance. No remaining release gate is waived.
