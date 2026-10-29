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
| UI-00: baseline | Navigation regression repaired; 102 frontend tests pass; durable journey, state, matrix, density, and gallery scripts | Expand fixtures as remaining states are implemented; retain the original audit evidence |
| UI-01: analytics trust | HTTP errors and malformed payloads rejected; unavailable/retry and stale data tested; no unconditional Live label | Add deliberate timeout/offline and authorization cases beyond the recorded 503/malformed probes |
| UI-02: tokens | App-owned semantic light/dark foundation, readable type, reduced motion, compact geometry, component gallery | Palette/type/import regression gate; audit all interactive gallery states and remaining route-local colors |
| UI-03: primitives | Button/Input adapters share app controls; Field and named IconButton boundary; pending confirmation preserves its label/width; Sources switches/provider reveal named; workbook settings labels, connected help and semantic text colors repaired | Migrate the remaining forms and direct primitive imports; verify popup/invalid/permission states across all primitive families and keep transient notifications clear of inspector/footer actions |
| UI-04: shell | Shared page heading, main landmark, skip link, grouped navigation, route focus, mobile drawer; 13 route families scanned in both themes | Complete all-route landmark/focus coverage, short-height and mobile-keyboard checks, manual assistive-technology navigation |
| UI-05: collections | Server filters/sorts/pagination, pinned identity, explicit-ID selection, compact/standard/comfortable preference, URL search/view/page/record | Data-completeness column and column configuration; wider selection/workspace-switch and touch state matrix |
| UI-06: workbook | Preserved identity in output-only workflows; semantic column headers; roving focus; keyboard resize/reorder; reviewed native enrichment; click-based move/position and numeric width; persistent activity sizing, truthful loaded counts, paginated saved errors, and immediate-keyboard-drop/focus regression checks | Full edit/paste/virtualization focus coverage (including refetch during editing), hidden-column pointer ordering, and complete queued/paused/partial/canceled lifecycle audit |
| UI-07: inspector | Field ownership, accepted/alternative observations, draft edits, employment history, focus return; URL-selected record survives reload | Company People tab; retained conflict draft with changed-field comparison; new custom-field type picker; dirty-draft protection for route Back/unmount |
| UI-08: migration | Upload/map/review/import/results; actual limits; precision preservation; partial rejection report; stable-key recovery after lost response/reload; CSV/JSON exports tested | Grouped/confidence-aware mapping with source samples, target/identity conflicts; imported-record links and separate optional enrichment; complete restore/reimport and mapping-preservation matrix |
| UI-09: metrics | Canonical Overview counts/coverage; explicit legacy Analytics scope, validation/freshness and table alternatives | Date range/timezone and supported comparison-period presentation; complete metric-definition/series-cue review |
| UI-10: activation | Canonical Company/Person audience member links and snapshots, honest counts, member pagination, scoped retry errors, refresh receipt | Extract destination workflow behind a focused interface; explicit external-sync/retry/token-rotation review and receipts; show rotated token independently of history-fetch success; explicit ad-consent attestation; apply same contracts to all activation routes |
| UI-11: administration | Searchable source/provider settings foundation, named controls and truthful save feedback | Authoritative role/override handling and correct global/workspace scope; provider/settings grouping; replace Agents' local-only toggles with honest actual configuration; full draft/error/permission verification |
| UI-12: verification | Durable Chromium journey/matrix/state probes; density probes in Chromium and WebKit; automated reflow/text-size/forced-colors/reduced-motion evidence | Full network/role/data/input/browser matrix; VoiceOver/Safari and NVDA/Chrome; actual browser zoom/text spacing; performance baselines and budgets |
| UI-13: architecture | App-owned patterns/primitives, typed audience presentation seam, scoped record queries; gallery shares build chunks; activity observation/statistics have a tested pure module and feature-owned styles | Complete the remaining workbook, destination and migration extraction, then remove duplicate primitive/token paths after parity |
| UI-14: rollout | Local embedded-build validation and explicit release limitations documented | Five-operator pilot, blocker triage, production metrics, staged route deployment and reviewed rollback drill |

## Hard gates

| Gate | Current evidence | Completion condition |
| --- | --- | --- |
| Tests/type/build/lint | 102 frontend tests; current lint and production build pass; native Go API with embedded UI compiles | Keep passing after the remaining changes; add meaningful coverage for newly completed workflows |
| Automated A/AA | Core route/theme/viewport matrix, journey and density states have recorded axe scans | Zero untriaged violations on the full final release matrix, including newly added states; no blanket rule exclusions |
| Keyboard and screen readers | Recorded keyboard/focus/import/grid probes | Complete VoiceOver/Safari and NVDA critical journeys, including popup boundaries and employment/conflict review |
| Reference geometry | Measured before/after row and chrome geometry, screenshots at pinned sizes | Approve final references and review key alignments within 1px; cover all required states, text spacing and fonts |
| Large data | Server-side pagination/filter/sort retained; 1,000-company fixture shows 100-row page | Exercise final 10,000-record and virtual-scroll state matrix; no download-all workaround |
| Backend contracts | Prior canonical records/tenant/version/import/native-run integration evidence retained; current changes use existing bounded IDs and APIs | Re-run relevant Go integration tests whenever later request/selection/authorization behavior changes |
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
