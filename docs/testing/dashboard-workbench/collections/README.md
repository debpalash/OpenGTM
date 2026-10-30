# Configurable collections and concise overview

Companies now defaults to identity, domain, industry, fit, location, data
completeness and update time. People defaults to identity, current company/role,
work email with verification state, fit and update time. **Columns** previews
visibility/order changes, keeps identity pinned, and supports Cancel, Apply and
Reset. Ordering has labeled pointer/keyboard buttons and retains focus after a
move. Selection, search, filters, sort and inspector state remain independent.

Preferences contain only column identifiers, scoped to user/workspace/record type.
Malformed, unknown-version and foreign-kind preferences recover safely. Other
tabs observe preference changes. If browser storage fails, changes still apply to
the current view and the UI discloses that persistence failed.

Completeness is six accepted core facts, with a per-record breakdown. It does not
claim accuracy, freshness or fit. Companies checks name, domain, industry,
location, description and employee count; People checks name, current company,
current role, work email, phone and LinkedIn. Empty/placeholder values do not
count; numeric zero remains a real value. Former roles and company contact facts
are not borrowed as current person facts. Structured email-verification status is
supported, and missing/invalid/risky/unverified states do not appear verified.
Known companies link directly to their inspector; unknown employers stay text.

The overview removes repeated captions and the import banner, moves Import to
the header, shortens panel/shortcut labels, and uses concise attention counts.
Failed-workbook counts now open the supported failed-status filter. Counts,
verification denominators, workspace/freshness context, stale warnings and empty
actions remain. In the screenshot-equivalent fixture, visible overview wording
fell from 110 to 49 words (55.5%). The before/after comparison is retained
separately from final embedded-build verification.

The collection table and its display rules/layout preferences now live under
`apps/web/src/features/records/collection/`. A WebKit regression revealed that
native pointer clicks focused the scroll region rather than the record button.
The inspector now stores the actual clicked button instead of inferring it from
`document.activeElement`. A one-record native reproduction failed before the
change and passed afterward; the full collection suite includes this check.

The route matrix also exposed two identically named recent-conversation landmarks
when chat history existed. Sidebar navigation now uses “Recent chats navigation”
while the chat-home section retains “Recent conversations.” The final matrix is
recorded on the later build containing that label-only correction; preceding
functional checks identify their own embedded-build snapshot.

## Evidence

[results.json](results.json) identifies the uncommitted source/build snapshot and
the completed checks; raw reports and screenshots sit beside it. The native large
fixture contains 10,000 Companies and 10,000 People, imported through the real API
in synthetic workspaces. Collection requests stay capped at 100 rows, including
dataset-wide filtering and page-to-page explicit-ID selection. Five reload-to-table
samples per engine are lab observations, not production Web Vitals or a full
performance budget. No enrichment or external delivery runs in the collection
suite. Workspace-changing scripts run sequentially and restore prior context.

Repeatable checks are `scripts/qa/dashboard_columns.py` and
`scripts/qa/dashboard_overview_copy.py`. The former supports `--large` and reuse of
a prior synthetic fixture via `--fixture-report`. Small/short-height and touch
checks verify popup scrolling, visible actions, 44px controls and pinned identity.
Broader regression checks are recorded with their own outputs rather than
inferred from a screenshot or a narrow unit test.

This increment does not close workbook virtualization/lifecycle, all remaining
route/permission states, manual assistive technology, production performance or
rollout gates in the [completion audit](../completion-audit.md).

WebKit has a separately retained navigation edge case: forcing a full document
navigation immediately after starting a list read emitted an access-control
exception in 12/15 probe cycles. The same probe following the actual sidebar SPA
navigation completed 15/15 without an exception. The collection journey uses that
real navigation control and still checks explicit reloads after settled reads.
The mid-request full-navigation case remains recorded for the wider network and
browser audit; it is not silently filtered from the error log.

Final verification: 161 frontend tests / 523 assertions, lint, TypeScript/production build and embedded API build pass. There are 128 zero-violation axe states: 82 on the functional build (collections, overview, native journey and density), then 42 route states and four entry/reload states after the sidebar landmark-name correction. Both build hashes are retained. The state/failure suite also passes. The separate pending-read/full-navigation WebKit exception above remains an open browser-audit case; it is not counted as a passing probe.
