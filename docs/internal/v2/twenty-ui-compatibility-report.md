# Twenty UI compatibility gate

Date: 2026-09-22
OpenGTM source: `ce16de9`, on `v2-reliable-gtm-execution`.
Status: pinned-source artifact reproduced byte-for-byte; production token/theme foundation implemented. Preview and fixture-backed app theme checks pass. Full workflow, accessibility, and performance gates remain open.

## What was actually tested

### Source Engine deferred loading

The workbook no longer statically imports Source Engine. Both the toolbar and
cost chip open the same guarded loader; pending downloads show a busy/disabled
toolbar button, failures report reload recovery, and the loaded panel remains
mounted so closing/reopening retains its prior behavior and local state.

The budget browser fixture verifies no initial Source Engine chunk, an aborted
download with a usable grid and explicit error, reload recovery, a held download
with loading feedback, and a rapid double click resulting in one chunk request.
Existing budget readback, failure/retry, validation and save checks pass too.
Production build, ESLint and diff checks pass. APIs are intercepted, not live.

Same-method static workbook JS falls from 1,073,426 raw / 334,721 gzip bytes to
1,053,075 raw / 328,617 gzip bytes (20,351 raw / 6,104 gzip saved). CSS is unchanged.
Chunk redistribution increases the shared entry chunk; this is a workbook-route
total improvement, not proof all routes improved. Full cross-route and runtime
performance acceptance remains open.

### Shared CSS investigation (current local checkpoint)

The current shared stylesheet contains approximately 159 KB of generated
utilities, 63 KB of light/dark token declarations and 56 KB of generated color
support blocks (raw bytes). There are no embedded base64 resources in this asset.
Explicitly restricting Tailwind detection to `apps/web/src` plus `index.html`
produces identical production asset hashes and route totals. Unrelated source
scanning therefore does not explain this build's payload. The explicit path is
retained to make source detection independent of the command's working directory,
following [Tailwind's source-path documentation](https://tailwindcss.com/docs/detecting-classes-in-source-files).

TypeScript/production build and fixture-backed app theme browser checks pass;
the browser reports no page errors. No CSS-size or runtime-speed improvement is
claimed. Theme tokens/support blocks were preserved. Further payload work should
inspect dependency reachability and optional panels rather than blindly deleting
tokens or color fallbacks. Full performance acceptance remains open.

Installed `twenty-ui@2.41.0`, React/React DOM 19.2.5, React Router DOM 7.14.2, and Vite 8.2.2 in an isolated temporary project. OpenGTM's dependencies and application code were not modified. Install scripts were disabled.

Registry tarball identity:

- SHA-1: `3a56302ff7a39999639da5c10f5e13f5df267833`
- Integrity: `sha512-hBTzxiFU3mMWbdd2PhtxkBDxgO9/4rW80ai2rTWUNce8DspusWQVrZSQG9DXUxeFI59U9QI0NSmwrjmCDPzsXw==`
- Packed bytes: 485,219; unpacked bytes: 2,605,603.

Temporary harness: `/tmp/opengtm-twenty-compat-kZqQOR`. This path is an inspection artifact, not a durable dependency or product source location.

## Findings

### 1. Published navigation components do not share the host router

`npm ls` confirms two installed routers: application Router 7.14.2 and Twenty's nested Router 6.30.6. React itself deduplicates correctly.

Rendering the host `Link` inside the host `MemoryRouter` succeeds. Rendering Twenty's `UndecoratedLink` inside that same router throws:

```text
Cannot destructure property 'basename' of 'React__namespace.useContext(...)' as it is null.
```

This is a reproduced context mismatch, not just a version-range concern. Do not downgrade the OpenGTM router to accommodate it. Use application-owned routing adapters and evaluate the pinned Twenty source that already declares Router 7. Broad dependency overrides are not proven compatible by this test.

Minimal reproduction after installing the exact dependencies above:

```js
import React from 'react';
import { renderToString } from 'react-dom/server';
import { MemoryRouter, Link } from 'react-router-dom';
import { UndecoratedLink } from 'twenty-ui/primitives/navigation';

for (const Component of [Link, UndecoratedLink]) {
  console.log(renderToString(React.createElement(MemoryRouter, {},
    React.createElement(Component, { to: '/workbooks' }, 'Workbooks'))));
}
```

The first render succeeds; the second throws.

### 2. Ordinary control imports emit editor assets

The preview imports ThemeProvider, Button, Input, AlertDialog, both theme stylesheets, and the base stylesheet. It does not use a code editor.

The published input entry point nevertheless re-exports CodeEditor from its shared implementation chunk; that chunk imports `@monaco-editor/react`. Production output includes:

| Artifact | Approximate uncompressed kB, reported by Vite |
| --- | ---: |
| TypeScript worker | 6,926.00 |
| CSS worker | 1,075.63 |
| HTML worker | 740.68 |
| JSON worker | 430.43 |
| Codicon font | 140.95 |
| Preview entry JS, including React and dependencies | 476.67 |
| Preview CSS, including both themes | 284.90 |

Worker assets total roughly 9.17 MB. Emitted files are not proof of initial network transfer; the test does not claim they all download on page load. They are still unwanted output and make this release a poor default for our migration. Newer source splits the editor entry point; verify the resulting build rather than assuming the split fixes every import path.

### 3. Core control behavior works in the browser

Headless system Chromium exercised the production preview at 1280×800 and 375×812:

- Controlled Input updates the corresponding output value.
- ThemeProvider switches the root to dark mode.
- AlertDialog opens, closes, and returns focus to its trigger.
- The dark dialog resolves dark surface and light text tokens.
- Application Router 7 Link navigates to `/workbooks` successfully.
- No page JavaScript errors occurred; the narrow fixture had no horizontal document overflow.

Screenshots were captured as `light.png`, `dark.png`, and `mobile.png` in the temporary harness; the dark screenshot was visually inspected. This fixture is intentionally plain and is not a proposed screen design or a visual acceptance baseline for OpenGTM.

The fixture's ordinary heading/body retained browser typography and margins. Theme tokens and component CSS alone do not establish the intended app typography/layout. Explicitly integrate font assets, page styles, reset ordering, and token adapters. Validate the exact artifact, not only current online documentation.

Not yet verified: all menu/select/ref APIs, nested portal scopes, screen-reader behavior, whole-app CSS reset interaction, live workspace flows, full route screenshots, and runtime performance under large datasets. This gate is not complete.

## Existing OpenGTM baseline

From `apps/web`, `bun run build` and `bun run lint` both pass. The installed build uses Vite 8.2.2.

Measured emitted file sizes, with gzip recomputed using Node's default `gzipSync` (use the same method for later comparisons):

| File family | Bytes | Gzip bytes |
| --- | ---: | ---: |
| Entry JavaScript | 406,269 | 124,905 |
| Shared CSS | 194,836 | 29,715 |
| Chat route JavaScript | 182,505 | 53,248 |
| Workbook editor JavaScript | 164,306 | 46,780 |

These are individual assets, not total route transfer sizes or loaded runtime memory. Shared dependencies and font requests must be included in later route network measurements.

Route inventory also found `/campaigns/*` and `/agency/*` in addition to the primary sidebar entries. Include Campaigns and workspace management in the migration, along with login, detail routes, wildcard routes, and query-parameter deep links.

## Decision and next work

Do not add unmodified `twenty-ui@2.41.0` to OpenGTM. Continue with the pinned MIT UI source identified in the migration plan, build it reproducibly, and repeat routing and asset checks. Keep Router 7 and OpenGTM's data/execution engines.

If that source passes, create the theme/primitive adapter preview in the repository and expand browser coverage before any shared UI replacement. Keep the theme migration and workbook reference implementation on the delivery path; this result does not reduce the requested redesign to recoloring existing controls.

## Pinned-source follow-up

Built `packages/twenty-ui` from `e99fd1a7683f37a957852c3f7ef8f6a29732bb38` in `/tmp/opengtm-twenty-source-e99fd1`. Used upstream Vite configuration, TypeScript checking, declaration generation, and existing token CSS. No library implementation files were patched. Package-local installation used `--workspaces=false --ignore-scripts --no-audit --no-fund`; no Twenty backend or CRM application was run.

The first build failed because the standalone installation lacked the declared Node type library. Added these build-only dependencies to the temporary package manifest: `@types/node` (resolved 22.20.4), `tslib` (2.8.1), and `vite` (8.2.2, aligned with the OpenGTM test fixture). The resulting `npm run build` passed, as did upstream `tsx scripts/checkOptionalDependencies.ts`. This upstream check verifies the root JS and declaration entry points do not require optional editor peers.

Temporary artifact identities:

- Package lock SHA-256: `5c41602b19dd80b06dbcfe2f008c6a5fff6ef67b1b896f1097eda3df5860986c`.
- Built `twenty-ui-2.42.0.tgz` SHA-256: `5f21ab5f3122251b9a2334df86371d1507888db682e7a7411b2b895e23b19875`.
- These are local experimental artifacts, **not** an official published 2.42.0 release. Their lockfile and build tooling still need durable repository packaging before adoption; a hash alone does not make the build reproducible for another developer.

### Router requirement confirmed

| Fixture | Installed router instances | Result |
| --- | --- | --- |
| Host 7.14.2 + source-built Twenty | Host 7.14.2; nested 7.18.4 | Twenty link throws the null router-context error |
| Host 7.18.4 + source-built Twenty | Single 7.18.4, deduplicated | Both host and Twenty links pass SSR and browser navigation |

Matching the router's major version is insufficient. The candidate integration requires one compatible runtime router instance. A host update to 7.18.4 worked in the fixture without overrides or source patches; OpenGTM's router is still unchanged. Test authentication redirects, nested routes, query parameters, browser history, and deep links before accepting this update in the application.

### Production fixture results

The revised fixture renders both host `Link` and Twenty `UndecoratedLink`. Production build succeeds and emits only the following two assets (default Node `gzipSync`):

| Asset | Bytes | Gzip bytes |
| --- | ---: | ---: |
| JavaScript | 456,128 | 137,938 |
| CSS | 265,049 | 78,736 |

No Monaco worker or codicon asset is emitted. This resolves the observed unwanted-editor-output problem for these imports. It is not a claim that the complete OpenGTM bundle is smaller: its current shared CSS is materially smaller, so the migration must measure combined route payload and remove replaced CSS.

Chromium checks pass: controlled input, light-to-dark switch, dialog visibility and focus return, host navigation to `/workbooks`, and Twenty navigation to `/leads`. There are zero page errors and no horizontal document overflow at 375px. The test now asserts both conditions rather than merely reporting them. Preview remains a minimal compatibility fixture, not visual design acceptance or proof of all application workflows.

Next: preserve the candidate build recipe/lock and license, add an isolated repository theme/component preview, then verify reset interaction, menus, forms, portal scopes, and application routing before changing shared controls. PR 0 is still open; no application dependency or source file has changed.

## Repository integration follow-up

The preceding sections record the historical isolated spike. Subsequent implementation now includes:

- `scripts/vendor/twenty-ui/build.mjs`, a pinned build manifest/lock, and MIT notice. A fresh checkout and `npm ci` reproduced the original tarball SHA-256 exactly. The script refuses to copy an unexpected artifact.
- `apps/web/vendor/twenty-ui-2.42.0-opengtm.tgz`, installed through the Bun lockfile, plus self-hosted Inter font assets.
- An exact Router 7.18.4 pin. **Baseline correction:** the old manifest range began at 7.14.2, which was used for the isolated fixture; the committed OpenGTM lockfile actually resolved 7.18.2. The repository update is 7.18.2 → 7.18.4, not a downgrade or a major upgrade.
- A separate `ui-preview.html` entry, `build:ui-preview` command, and application-owned primitive import boundary. Twenty's CSS is imported only by this preview, not the production application entry. Normal production builds do not emit the preview page.
- Docker copies the vendored package before its frozen dependency installation. A full container build has not yet been tested.

`scripts/vendor/twenty-ui/check-preview.py` passes on the built preview in Chromium: stable-ID selection through filtering, exact selected names in the confirmation dialog, cancel/focus return, local confirmation receipt, menu density change, clear selection, empty results, and Twenty navigation through the host router. Both themes have no horizontal document overflow at 375/768/1280/1440px. The preview makes no API/auth requests and loads no editor workers. The test uses reduced-motion mode. Desktop light and mobile dark screenshots were inspected; an inherited dark link-color problem was fixed with explicit semantic tokens.

All records and receipts on this page are explicitly fictional/local. It is a component and presentation test—not the real workbook implementation, a live-data result, or evidence of completed GTM execution. The preview is intentionally not linked from public application navigation.

The expanded preview includes menu controls, fonts, and a small data table. Its build emits approximately 507kB JavaScript and 269kB CSS before gzip and reports the 500kB chunk warning. Do not suppress that warning as a performance fix. Production route payload and actual runtime behavior must be measured during migration.

Remaining: whole-app router regression, reset/legacy CSS coexistence, theme persistence/system changes and boot timing, scoped nested portals, screen-reader and contrast audit, 200% zoom, real workbook editing/virtualization, and full workflow regression. PR 0 and the larger V2 goal are not complete.

Repository checks after integration: frozen Bun install, normal application build, preview build, ESLint, and the two existing API job-client tests pass. Normal production output contains no preview HTML. The normal build reports ~407kB entry JS, ~195kB shared CSS, ~200kB Chat JS, and ~164kB workbook JS. The Chat artifact differs from the earlier installed-tree measurement despite unchanged route source; do not attribute that delta to Twenty (which is not imported by the production entry). Establish a clean lockfile-installed baseline before claiming migration payload improvements.

## Production theme foundation follow-up

The real application now imports Twenty's generated light/dark token CSS and Inter.
`design-system/theme/tokens.css` maps existing semantic variables to those tokens;
the old warm/dim palette blocks were removed. Existing controls still use their
current behavior and markup. This is the migration foundation, not the complete
Twenty component or workbook replacement. Twenty's general component stylesheet
is still confined to the preview, avoiding an untested global reset cutover.

`public/theme.js` is the single preference store and runs synchronously in the HTML
head. It preserves saved `theme=light|dark`, retains the existing dark default,
supports `system`, handles blocked preference storage, and synchronizes other tabs.
React consumes it through `useSyncExternalStore`; Twenty's scoped provider does
not write the root class. Header, command palette, and toast all use this store.
Removed unused next-themes and Geist dependencies. Existing Base UI 1.7.0 and Zod
4.4.3 are explicitly pinned to avoid incidental updates; Twenty retains its own
Base UI 1.8 dependency until primitive migration can validate consolidation.

The command-palette browser test exposed an existing missing cmdk context:
`CommandDialog` rendered input/items without a `Command` root. Added the root and
moved its accessible title/description inside the dialog popup. Ctrl+K → theme
action now passes without the previous `subscribe` exception.

Verification:

- Eight theme-store unit cases plus the two existing API client tests pass.
- Preview browser checks pass for menu/dialog focus, exact filtered selection,
  two themes/four widths, system changes, and persisted reload.
- `check-app-theme.py` runs the real application with all business API requests
  intercepted: header preference selection, light/dark at 375/1280px, system
  changes, reload, cross-tab synchronization, and command-palette selection pass.
  It also blocks the React bundle and verifies the saved class is set by the boot
  script with an empty React root. This establishes pre-React initialization,
  not a measured first-paint timing guarantee under every network condition.
- Desktop light/dark and mobile dark screenshots were inspected. App build,
  ESLint, frozen install, and whitespace checks pass. No live provider or workspace
  action was executed by these tests.

Performance remains an open gate. A narrow PostCSS transform replaces only
`--t-background-noisy` with `none` because OpenGTM does not use Twenty's embedded
decorative PNG. The vendored package stays untouched. This reduced combined CSS
from ~361kB/94kB gzip to ~313kB/55kB gzip, still above the pre-migration ~195kB/30kB
baseline. Entry JS is ~450kB/135kB gzip. Remove superseded styles/components and
measure route loading before claiming acceptance of the performance budget.
The Chat chunk returned to ~182kB after restoring dependency pins.

Next: shared primitive adapters and grouped shell/navigation, then the complete
real Workbooks slice. Still unverified: all authentication/deep-link routes on
the deployed serving stack, full contrast/screen-reader audit, 200% zoom, large
workbook interaction latency, and live GTM execution. Login and other hardcoded
screen styles still need their scheduled migration.

## Grouped shell navigation follow-up

Extracted the sidebar into `components/app-shell/app-sidebar.tsx` and created a
single navigation registry used by sidebar groups, command-palette destinations,
and page titles. Workspace, Execution, and Resources groups retain all original
paths; Campaigns and Manage workspaces are now directly discoverable. Active
matching respects slash boundaries rather than arbitrary string prefixes.

The workspace picker is at the top with a visible command-search entry. Recent
chats are collapsible, with labeled search/new/delete controls; delete actions
are siblings of navigation links rather than nested buttons inside links. Removed
sidebar noise overlays, press scaling, and raw violet active styles. Navigation
uses semantic hover/selection tokens and larger touch targets on mobile.

Workspace switching exposed a pre-existing client-state gap: query keys were not
workspace-scoped and successful switching left their cache intact. The switch
now cancels queries and clears the shared cache before updating the active ID;
the authenticated shell remounts by workspace ID, and successful switching returns
to Chat rather than retaining a previous workspace's entity URL. Logout and
unauthorized transitions also clear the cache. This is a client display/state
boundary, not a replacement for backend tenancy checks or a complete audit of
in-flight mutation behavior.

Verification: 13 frontend unit tests pass, including registry completeness and
boundary matching. App build/lint pass. Expanded Chromium smoke verifies command
search opens and returns focus, newly discoverable routes appear in the palette,
history expands, mobile navigation closes after selection, rejected workspace
switches retain the original view, and successful switching requests workbooks
with the new workspace header and shows no old workbook. API calls are intercepted
fixtures, including simulated switch success/denial; no real workspace changed.
Light/dark desktop screenshots were inspected.

Not complete: header information simplification, full route-by-route functional
testing, reusable Twenty primitive replacement, and the actual workbook editor
vertical slice. Existing workbook cards still contain hardcoded status styling
and hover-only actions; those belong in the upcoming workbook migration.

### Workbook list follow-up

The actual Workbooks list now uses Twenty controls and a compact semantic table,
with URL-backed search/status/sort, bounded 50-row pagination, explicit deletion
confirmation, and distinguishable loading/error/empty states. Creation retains
the existing column and source contracts. Template creation now checks HTTP
errors and returned IDs before navigating. Pending guards prevent duplicate
submissions; failed requests preserve input and expose retryable errors.

Browser repetition caught a rapid-filter URL update race, now corrected. Visual
inspection caught Twenty's unlayered button reset overriding application
utilities; the imported styles now occupy the components CSS layer. Both fixes
are part of this local migration, not backend redesigns.

Verification: 21 frontend unit tests pass. The workbook browser suite covers
light/dark at 375/768/1440px, search/filter/sort, 105-record pagination, failed and
successful create/delete/template requests, duplicate submit suppression, and
navigation to returned IDs. All business APIs are intercepted fixtures: this is
not proof of live collection or enrichment. The workbook editor is explicitly
outside this suite. Lint and production build pass; route payload/performance,
200% zoom, full accessibility review, and the remaining PR 3 journey remain open.

### Editor selection and deletion follow-up

The editor now uses Twenty's button and AlertDialog components for bulk selection
actions and confirmation. Selection labels distinguish all selected IDs from the
subset on the current page; all-matching selection explicitly says across all
pages. Confirmation captures the row IDs or count/search/view scope when opened.
Pending submissions are guarded, errors remain inside the dialog, Cancel returns
focus to the trigger, and successful deletion returns focus into the grid.

The audit found and removed a dangerous legacy fallback in
`useDeleteWorkbookRows`: workbook deletion previously called the global lead
delete API for `lead:` identities. Selected legacy rows now produce migration
guidance without deleting original leads; mixed selections are rejected rather
than partially applied. The dedicated Leads deletion workflow is unchanged.

Real browser clicks exposed unstable cell component identities: normal focus
updates rebuilt column definitions and detached checkbox inputs during a click.
Normalized columns and mutation-method dependencies are now stable. Decorative
row-entry animations were removed from the virtualized editor. Input cells also
read self-contained row data consistently with their table accessor, instead of
displaying only the legacy lead object.

Verification: 30 frontend tests, frontend lint/build, and 30 backend workbook
router tests pass. A new backend test checks selected-ID deletion against
unselected rows, another workbook, and another workspace. Chromium exercises
cell edit/escape, cross-page IDs 1 and 1001, exact delete payloads, cancellation,
duplicate clicks, failed deletion/retry, query-count conflict, focus return, and
legacy selection rejection. The browser uses intercepted APIs/sockets; backend
tests use disposable SQLite and overridden auth. Neither proves live GTM runs.
Light/dark confirmation dialogs were rendered at 375/1440px and screenshots
inspected. The old editor toolbar still needs responsive restructuring, and
estimate/approval/run/progress/evidence/recovery remains the next PR 3 work.

### Workbook run review follow-up

Main-toolbar run and fill-missing actions now open a Twenty Dialog with the saved
view, search, full-query row count, catalog provider estimate, explicit estimate
limitations, and external-output warning. Checkbox selection is explicitly not
the run scope. Missing/invalid estimates disable Start; failed submissions retain
errors and require a refreshed review. Rapid duplicate clicks are guarded locally.
Stop now checks HTTP status instead of claiming success on a failed response.

The optional `expected_rows` run request field rejects count drift with HTTP 409
before billing or enqueueing. It is backwards-compatible and needs no database
migration. It is NOT a frozen record/configuration snapshot: same-count identity
changes remain possible. This UI gate is also not a durable approval record or
server-side idempotency guarantee. Network/invalid-receipt errors tell users to
check activity before another run. Column/cell force-run routes still have their
existing confirmation behavior and are not migrated by this change.

The new browser flow exposed another editor mismatch: TanStack was filtering an
already server-filtered page using only legacy lead fields, hiding matching
self-contained rows. Filtering is now explicitly server-owned. The cost chip
also clears the previous scope's estimate while a replacement loads.

Verification: 38 frontend tests, 31 offline SQLite workbook-router tests, frontend
build/lint, and run-review browser checks pass. The backend tests prove count
drift does not reach billing or create a Job, and matching counts queue the exact
resolved rows. Chromium checks failed estimates, view/search payloads, selection
scope disclosure, cancel/focus, normal/fill-missing modes, failed run/review
refresh, duplicate clicks, and stop errors/retry. Previous list and editor
selection browser suites also pass. All browser APIs/sockets are fixtures.

UI/UX accessibility checks found clipped actions on short mobile viewports; the
dialog now scrolls its body independently, retains visible actions and an
external-send warning, and focuses submission errors. Light/dark screenshots at
375×667 and 1440×1000 were checked. Remaining PR 3 work includes the full editor
toolbar/saved-view migration, complete run receipts/progress/evidence/recovery,
stronger approval/idempotency semantics, and actual persisted execution checks.

### Durable workbook run history and executable-column follow-up

Run responses now return the real queue job ID and run ID. A workspace-owned,
cursor-paginated `/api/workbooks/{id}/runs` endpoint exposes allow-listed queue
metadata and recorded outcomes without returning job payloads or row-ID lists.
These IDs belong to the durable queue, not the separate collection Tasks store;
the UI deliberately does not link them to `/agents/{id}`.

The queue passes its claim identity to the worker. Workbook result recording
compares worker ID, lease timestamp, workspace, workbook, and job state before
writing, repeats the lease predicate for SQLite, and preserves cancellation.
Previous-attempt outcomes are not displayed as the current retry's result. A
receipt-write failure is logged rather than automatically replaying external
actions that already executed. No database migration was needed: outcome
summaries use the existing job JSON payload.

Twenty's Run history dialog distinguishes queued/running/cancelled/failed,
worker-finished, and finished-with-cell-errors. It polls only while open, supports
older pages, exposes dates/heartbeat/errors, and states that queue completion is
not evidence of verified data. Missing outcomes remain explicitly unavailable.
UI/UX guidance informed contextual live status labels, error/retry states, focus
return, and a scrolling body with visible mobile footer actions.

Column normalization now preserves research, HTTP, formula, agent, source, and
unknown types rather than turning them into editable inputs. Missing legacy
types still default to input fields. Both renderer and backend result handling
preserve zero/false; scalar computed values are serialized for the legacy Text
overlay while the row JSON retains its value. Toolbar groups now wrap, although
the complete toolbar/saved-view redesign is still pending.

Evidence: 44 frontend tests, build/lint, and 115 backend tests covering workbook,
enrichment, queue claims/leases/recovery/concurrency/load, and subprocess handling
pass. Two pre-existing concurrency mocks omitted the production lease timestamp;
the mocks now model and assert the actual claim contract. Browser list,
selection, run-review, and history suites pass with intercepted APIs/sockets;
history screenshots at 375×667/1440×900 were inspected in both themes.

A new router-to-worker test uses real formula evaluation and database writes:
one row yields two completed cells (including zero/false) and one intentionally
invalid formula. The durable job completes, while its persisted receipt records
two complete / one error / three planned cells. The test replaces the subprocess
boundary, Redis, and provider-pool setup; it does not exercise live providers or
production infrastructure. Separate tests cover tenant isolation, pagination,
payload exclusion, expired-claim rejection, retries, and cancellation.

Remaining: per-cell evidence inspection/recovery UI, accurate per-run live
progress, immutable approvals/server idempotency, large-workbook performance,
full accessibility checks, and live GTM workflow acceptance. Workbook-global
status/progress still needs a concurrency audit against superseded runs; the new
history reports durable queue state rather than treating that global status as
proof of an individual run's success.
# Workbook route payload checkpoint

CSV backend protection: import now performs a workspace-scoped, refreshed
`FOR UPDATE` workbook read before calculating schema additions, duplicate
identities and row positions. On PostgreSQL this serializes imports with other
workbook writers that use the same row-lock protocol. It does not make legacy
unlocked full-configuration writers safe. New database tests prove foreign
workbooks return 404 without mutation and imports retain agent execution fields,
unknown future settings, refresh policy, existing evidence and ordered row
positions. The CSV and workbook API suites pass together (88 tests). SQLite
ignores `FOR UPDATE`; concurrent PostgreSQL acceptance remains unproven.

CSV duplicate-click regression reproduced and fixed: two synchronous button
clicks previously issued two identical POST requests. The import dialog now
holds a synchronous ref guard until its asynchronous parent callback finishes;
the callback contract explicitly returns a promise. Existing pending feedback
remains, and failed requests unlock for an explicit retry. The expanded save
fixture failed before the change and passes afterward with exactly one failed
request followed by one requested retry. Parser/recovery fixture, build, lint
and diff checks pass. UI/UX loading-button guidance informed the interaction
check. This is a single-dialog guard, not backend idempotency across tabs or
retries after an ambiguous committed response.

CSV submission browser acceptance: `check-workbook-selection.py --check-csv-save`
passes exact mapped-row payload (including an explicitly skipped column), enabled
deduplication, injected 503 with retained mapping/draft, explicit retry with the
same payload, and a successful receipt reporting one added/one duplicate skipped.
It asserts exactly two requests and no unexpected API requests or page errors.
The fixture does not claim database persistence: save responses are intercepted.
Ambiguous network outcomes after commit, simultaneous imports and double-click
submission are not established by this test and remain separate acceptance work.

CSV safety follow-up: the deferred-import fixture now aborts the parser download,
asserts the exact recovery message and usable grid, then reloads and successfully
opens mapping. All scenarios assert zero business writes. Inspection also found
ignored parser errors: the editor now rejects malformed quotes and mismatched
field counts before mapping, while permitting valid single-column CSV files
whose delimiter cannot be detected. Browser checks cover those cases with exact
error messages, plus quoted values and repeated selection. Build, lint, diff
checks and all six backend CSV import tests pass. These are isolated frontend
and backend checks, not a live end-to-end import claim.

CSV parser deferral: workbook file selection now dynamically imports Papa Parse
instead of loading it on every workbook visit. The input resets before awaiting
the chunk, and module-load failure gives a reload/retry message. Production build
and lint pass. The new `check-workbook-selection.py --check-csv-loading` fixture
verifies no parser resource on initial load, loading upon file selection, quoted
CSV values, automatic Company/Email mapping, and selecting the same file twice.
It does not submit an import to a live backend. Measured initial route JavaScript
is now 1,072,442 raw / 333,888 gzip bytes across 61 files: 16,655 raw / 5,530 gzip
bytes below the preceding build. CSS is unchanged. This is a modest measured
reduction, not completion of the payload acceptance gate.

Latest regression recheck (2026-09-22): the 50-column / 1,000-row fixture passes
wide editing, selection, keyboard/pointer reorder, width-save failure recovery,
and persistence assertions. This local run mounted 534 cells, with scroll-render
median 50.1 ms and p95 52.2 ms. A payload measurement ran concurrently, so this is
a functional regression check with observational timings, not a controlled
performance comparison. Current workbook-route payload is 1,089,097 raw /
339,418 gzip JavaScript bytes (60 files), and 408,937 raw / 68,901 gzip CSS bytes
(3 files). The payload release gate remains open. No styles were purged based
only on this fixture; hidden states and other routes still require their styles.

Backend regression command covering workbook views, spend models, execution
identity, provider accounting, batch prepass, CSV import, load, workspace scoping,
queue concurrency, native research, declarative providers, and waterfall
validation passes: 183 tests. Executed with `DATABASE_URL` unset and `APP_ENV=test`
against disposable databases. This does not establish PostgreSQL concurrency,
live vendor correctness, or production end-to-end acceptance.

Wide-table finding: benchmark now accepts `--benchmark-columns` (6–200). With
1,000 rows / 50 input columns, a current-build local run measured median 218.3 ms,
p95 537.3 ms and 2,174 mounted grid cells at the start position. This contradicts
any general claim that the six-column result establishes spreadsheet performance.
Inspection confirms row virtualization only (`overscan: 20`), with every visible
column rendered for each mounted row. Next implementation target is bounded
horizontal rendering, preserving column widths, resize controls, cell editing,
selection/copy/paste, keyboard focus and offscreen navigation. Do not reduce test
column counts or weaken assertions to claim acceptance. Repeated old/new wide
benchmarks are still needed for migration attribution.

Scroll timing experiment: `check-workbook-selection.py --benchmark-only` uses
1,000 rows and six identical supported input columns in both revisions, four
warm-up traversals and twenty alternating end/start samples. Measures requested
scroll to target-row DOM presence plus an animation frame in headless Chromium.
One sequential local pair: clean checkpoint median 40.1 ms / p95 47.5 ms; current
median 34.6 ms / p95 45.8 ms. This pair shows no regression in this narrow case,
not a statistically established improvement or field INP. Multiple runs, wide
tables, editing and network-backed datasets remain required for acceptance.

Virtualization checkpoint: the real editor selection browser fixture now scrolls
a loaded 1,000-row page to its midpoint, end, and back to the start. It verifies
the expected middle/end/start records become available while fewer than 100 DOM
rows exist. Current counts are 64, 43, and 43 (including table scaffolding), then
the original edit/cross-page selection/delete assertions pass. This proves bounded
row rendering for that fixture; it is not a latency benchmark or evidence for
large column counts, slow networks, or real provider data.

Base UI consolidation result: application now pins 1.8.0, matching Twenty; the
installed tree contains one Base UI version. Production build, lint and 46 frontend
tests pass. Workbook list, evidence, views, history, selection, and run-review
browser fixtures pass. Selection required an isolated rerun after a parallel-run
failure; run-review's old fixture omitted the now-required `workbook_id`, corrected
to the real API shape before rerunning. No production validation was relaxed.
Current workbook route JS: 1,082,152 raw / 337,267 gzip bytes in 60 files, a 20,304
gzip-byte reduction from the dual-version build. CSS is unchanged. JS remains
~16.5% above the clean pre-migration baseline, so the performance gate is still
open; these checks do not cover every unmigrated application screen.

Payload attribution follow-up: production imports Twenty light/dark tokens once
in `index.css` and component CSS once through the primitive boundary. The installed
component sheet is ~96 KiB; upstream light/dark token sheets are ~64/~85 KiB before
the existing noisy-background removal. Both themes define 999 tokens; 148 have
identical values, accounting for only ~5,401 raw bytes of duplicate declarations.
Do not assume wholesale CSS duplication or remove unseen-state styles to meet a
size target. The dependency tree does confirm two Base UI versions: application
1.7.0 and Twenty's nested 1.8.0. Next controlled experiment: consolidate versions
and compare build, route payload and existing dialog/menu/browser workflows;
retain the change only if compatibility checks pass. Remaining legacy styles can
be removed as their actual consumers migrate, not by unsafe blanket purging.

Clean baseline comparison: archived commit `ce16de9752f6d0ce4aa40af0da9fa552045bfce7`
into `/tmp/opengtm-baseline-bPDmwn`, installed its frozen Bun lockfile with lifecycle
scripts disabled, and built successfully. The same empty-workbook browser fixture
against its preview on port 4400 requests 911,252 JS bytes / 289,491 gzip bytes
(53 files), and 194,836 CSS bytes / 29,124 gzip bytes (1 file). Current gzip totals
are approximately +23.5% JS and +136.6% CSS. This exceeds the plan's 10%
investigation threshold; payload acceptance is not met. The baseline also lacks
new evidence/run-history functionality, so attribution requires module/style
analysis rather than calling the entire delta a Twenty defect.

Reproduce with `check-workbook-views.py --measure-only --url <preview-url>
--dist <matching-build-dist>`. Keep both builds on the same hardware/toolchain.

The saved-view browser fixture now records resource URLs actually requested by
the production workbook route and sums their matching local build assets. Gzip
sizes use deterministic local compression, not claims about deployment transfer
headers. At the current build: 53 JavaScript files, 1,158,667 raw bytes / 357,571
gzip bytes; 3 CSS files, 408,916 raw bytes / 68,903 gzip bytes. Fonts are listed but
not included in those totals. This measures the empty workbook fixture route,
not large-data rendering, provider latency, or cold network performance.

The complete saved-view fixture still passes. The performance gate remains open:
compare the same route against a pre-migration build and measure large-workbook
interaction latency before accepting the migration. Individual chunk movements
are not a valid proxy for total route cost.
# Reproducible route-payload checkpoint

Shared CSS structural inspection (2026-09-22, current build): parsing the
308,952-byte index stylesheet with Vite's installed PostCSS dependency shows
159,016 bytes in the utilities layer; 31,689 / 31,663 bytes in the light/dark
base rules; and 28,331 / 27,730 bytes in their display-p3 support rules. The
theme layer is 4,750 bytes and base layer 3,955 bytes. These are uncompressed
serialized AST-node sizes, not additive gzip costs or browser transfer timings.
The four primary theme-token occurrences represent two themes and their color
fallbacks, not evidence of redundant imports. The noise-image override is already
effective: this built index CSS contains no data URLs. Do not strip theme fallback
rules or arbitrary selectors to meet the payload gate. Remaining investigation
should measure safely shared theme declarations and the primitive stylesheet's
component boundaries against light/dark and interaction fixtures.

Shared-token experiment: the upstream source has 148 identical light/dark
declarations (5,400 serialized raw bytes). On the current compiled index asset,
an in-memory PostCSS experiment moved 153 identical `--t-*` declarations from
the two base theme rules to `.light,.dark`. Index CSS decreased from 308,952 raw /
53,950 gzip to 304,854 raw / 52,956 gzip bytes: only 994 gzip bytes saved. This
experiment did not establish cascade equivalence and was not written to source
or distribution. Do not implement a cascade-sensitive transform for this small
gain while component-boundary costs remain open. Both theme fallbacks and the
production build are unchanged by this experiment.

The production build now emits a Vite manifest. The dependency-graph reporter
`scripts/vendor/twenty-ui/route-payload.mjs` measures the shell plus a selected
route without double-counting shared chunks or counting unopened dynamic imports.
Three Node tests cover shared/cyclic dependencies, lazy exclusions, missing-entry
failure and byte measurement. Production TypeScript/Vite build passes.

Current workbook static payload: JS 1,073,426 raw / 334,721 gzip bytes across
61 files; CSS 408,937 raw / 70,053 gzip bytes across three files. These are local
compression estimates, excluding fonts/images/API data, not browser latency.
The largest CSS asset is 309,143 raw / 53,983 gzip bytes; Twenty primitive CSS is
97,494 raw / 15,327 gzip bytes. Inspect the large shared stylesheet before
assuming workbook code alone explains the regression. This checkpoint adds
measurement, not a performance improvement; the performance gate remains open.
