# Dashboard workbench validation

The workbench implementation follows [the transformation specification](../../specs/dashboard-ui-ux-transformation.md). It keeps the existing routes and record/workbook APIs, adds tenant-scoped summary, access, and import-receipt endpoints, and requires no new migration beyond the shared-records schema already present on this branch.

## Repeatable browser checks

Install web development dependencies with `bun install` in `apps/web`. Axe is pinned to 4.14.0. The scripts use Python Playwright; they accept `--browser-path` or `PLAYWRIGHT_CHROMIUM_EXECUTABLE` to use an existing Chromium installation. Otherwise install Playwright's matching Chromium:

```sh
uv run --with playwright playwright install chromium
export OPENGTM_QA_USERNAME='<local QA username>'
export OPENGTM_QA_PASSWORD='<local QA password>'
uv run --with playwright python scripts/qa/dashboard_journey.py --base-url http://127.0.0.1:8002
uv run --with playwright python scripts/qa/dashboard_states.py --base-url http://127.0.0.1:8002
uv run --with playwright python scripts/qa/dashboard_matrix.py --base-url http://127.0.0.1:8002
```

The scripts restore the previous active workspace. The journey creates an isolated synthetic workspace and leaves it for inspection. The matrix accepts `--workspace-id` and `--workbook-id`; use the journey's IDs for populated People/Employment coverage, and a large synthetic workspace for pagination coverage. Run these scripts sequentially: both temporarily update the account's active-workspace pointer.

The journey checks staged CSV/JSON import, partial rejection, recovery after a committed response is lost and the page reloads, rapid search input, provenance, shared edits, dirty-draft recovery, focus return, exact large identifiers, saved enrichment configuration, reviewed native execution, pinned workbook identity, CSV/JSON exports, canonical overview counts, and mobile inspectors. Axe violations and page errors fail the process. Only the local native scorer runs; the test does not send outreach or invoke paid providers.

The state script uses the journey report to exercise stale/failed/malformed responses, retry, view-only controls, grid keyboard focus, resize/reorder, import focus trapping, 320–1920px geometry, 200% text size, forced colors, and reduced motion. It updates only the synthetic workbook’s column layout.

The matrix scans thirteen route families in light/dark at desktop size, then Overview, Companies, People, and an available workbook at 320px and 390px in both themes. It records headings, landmarks, focus targets, overflow, grid counts, token values, raw axe findings, and screenshots. It fails on axe violations or JavaScript page errors. Small-control candidates are measured separately: a visual element's dimensions alone do not establish a WCAG target-size failure when its actual hit area or spacing differs.

Additional workstation checks:

```sh
uv run --with playwright python scripts/qa/dashboard_density.py --workspace-id '<synthetic workspace with at least 100 Companies>'
uv run --with playwright python scripts/qa/dashboard_density.py --engine webkit --workspace-id '<same synthetic workspace>'
uv run --with playwright python scripts/qa/dashboard_gallery.py
uv run --with playwright python scripts/qa/dashboard_audiences.py
uv run --with playwright python scripts/qa/dashboard_layout.py --journey-report .run/dashboard-journey/journey-results.json
```

Install WebKit with `uv run --with playwright playwright install webkit` before the WebKit check. Density measures the same 1440×1000 viewport, verifies all three row preferences and reload persistence, scans focused light/dark menus, and checks actual touch-device targets and pinned-column offsets at 320/390/768px. Optional `--baseline-url` compares a prior build using the same synthetic workspace and API identity. The gallery verifies stable loading-button width, connected invalid-field state, dialog focus restoration, and 320px reflow without account data. The audience script creates an isolated synthetic workspace, verifies Company/Person member links into shareable inspectors, compares refresh receipts to the actual API response, and injects a recoverable members error. The layout script reuses the journey’s synthetic workbook to verify click-based ordering, numeric width, failure/conflict draft retention, reload persistence, activity sizing, tabs/focus, saved-error inspection, skipped-result counts and touch targets. These scripts fail on axe violations and page errors.

`dashboard_matrix.py --engine webkit` can run the complete route matrix with that engine; the recorded compact pass uses Chromium for the full matrix and WebKit for the density/popup/touch subset. Do not describe subset coverage as a complete cross-browser certification.

Output defaults to the ignored `.run/dashboard-journey` and `.run/dashboard-matrix` directories. Do not commit production account screenshots or raw contact data. Committed example images use synthetic fixtures.

## Code checks

```sh
cd apps/web
bun test tests
bun run lint
bun run build
```

From `apps/backend-go`, run the changed packages and the database-backed regressions:

```sh
go test ./internal/records ./internal/api ./internal/providerrun ./internal/vendorcatalog
go vet ./internal/records ./internal/api
TEST_DATABASE_URL='<owner DSN for a local test Postgres>' go test -race -tags integration ./internal/records -run 'TestWorkbenchSummaryFiltersAndRecovery|TestWorkbookExplicitDeltaAndEmploymentContinuity|TestPersonRoleWithoutCompanyAndRoleOnlyPatch|TestSharedRecordsImportEmploymentAndWorkbook' -count=1
```

The integration fixture creates and drops its own databases. It verifies scoped counts, filtering/sorting, isolation, committed import receipts, shared fact ownership, and employment continuity. The frontend tests reject HTTP errors and malformed analytics payloads instead of displaying synthetic zeroes, validate summary consistency, and preserve numeric identifier digits and PostgreSQL observation timestamps.

## Release scope and remaining acceptance

Implemented: semantic light/dark tokens, Inter/Manrope type scale, larger controls and readable metadata, reduced motion, shell/navigation/skip link/route focus, shared states, real record overview, server-backed record views, provenance inspector, staged import/recovery, workbook header semantics and pinned identity, reviewed enrichment, searchable source settings, API-key labels, honest analytics failures/freshness, and chart tables. Existing activation and legacy routes inherit the shared foundation; this change does not replace their domain workflows.

Full product release still requires the specification's VoiceOver/NVDA and Safari/Firefox checks, the operator pilot, production telemetry, and deployment/rollback review. Local Chromium/axe results are automated evidence, not a screen-reader or production Core Web Vitals certification. Other route-specific P2 workflow redesigns remain scoped follow-up work under UI-10/UI-11; advanced forms and extraction of the large legacy workbook module are not represented as completed here.

The [completion audit](completion-audit.md) retains outstanding acceptance across every work package and hard gate. The [compact workstation evidence](compact/README.md) records the denser layout revision separately from the original build below.

The [workbook controls evidence](workbook-controls/README.md) records click-based ordering, keyboard-drop/focus fixes, persistent activity sizing and truthful result inspection, including its built-app and database checks.

## Recorded local result — 2026-10-29

Core implementation: `3f98524b`. The production web build was embedded in the Go API and served on port 8002 for this pass. [Machine-readable results](results.json) retain the tested revision, environment, route list, counts, and interaction outcomes.

- 92 frontend tests passed; lint and type/build checks passed.
- Changed Go packages and vet passed; four focused database integration tests passed under the race detector.
- 42 route/theme/viewport scans and 10 journey-state scans returned **zero axe violations**, with no JavaScript page errors. Thirteen route families were scanned at desktop size in both themes; core collections, Overview, and workbook were also scanned at 320/390px in both themes.
- The built-app journey verified a real native enrichment after explicit run review, reload recovery of a committed import, rejected rows, exact identifiers, edits/provenance, exports, and mobile inspectors.
- Fault injection verified 503/stale/malformed response handling and retry. View-only controls, grid keyboard focus, column resize/reorder, import focus trapping/Escape restoration, 320–1920px page reflow, 200% overview text sizing, forced-colors rendering, and reduced-motion behavior passed the recorded automated probes.
- No production Core Web Vitals, screen-reader, operator-pilot, or cross-browser certification is claimed.

Synthetic fixture captures: [Overview](captures/overview-light.png), [dark Overview](captures/overview-dark.png), [Companies](captures/companies-light.png), [320px Companies](captures/companies-dark-320.png), [provenance](captures/record-provenance.png), [320px inspector](captures/inspector-320.png), and [workbook identity](captures/workbook-identity.png).
