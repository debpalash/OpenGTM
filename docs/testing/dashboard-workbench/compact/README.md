# Compact workstation verification

Tested implementation: `a56cc927`, on `feat/dashboard-workbench`, 2026-10-29.
The production frontend was embedded in the Go API and served at
`http://127.0.0.1:8002`. The served index matched the built index byte for byte.
[Results and asset fingerprints](results.json) retain the measurements and
interaction outcomes. All captures use synthetic fixtures.

At **1440×1000**, the same 1,000-company fixture shows **22 fully visible rows
instead of 15: 46.7% more**, with the same collection text sizes. This is a local
geometry measurement, not a general productivity or conversion claim.

| Element | Previous core build | Compact build |
| --- | ---: | ---: |
| Complete visible record rows | 15 | 22 |
| Record row height | 45px | 34px |
| Global bar | 56px | 44px |
| Page header | 108px | 64px |
| Resource toolbar, including border | 61px | 45px |
| Sidebar | 240px | 208px |

The global bar, heading and toolbar use 72px less vertical space. Record rows
default to Compact; Standard (40px) and Comfortable (48px) remain available from
Table density. The preference survives reloads. Touch name actions, checkbox
targets and menu items expand to at least 44px, with aligned pinned offsets.
The inspector is 400px wide on desktop. Overview, workbook rows, cards, forms,
dialogs and legacy routes inherit the tighter spacing and shared controls.

## Verification

- **98 frontend tests passed**, with zero failures; lint and type/build passed.
- Native Go API compilation with the embedded UI passed. A clean frozen Bun
  install applied the pinned focus-guard accessibility patch successfully; the
  Docker build copies patches before installation.
- **82 automated axe scans returned zero violations**, with no JavaScript page
  errors: 42 Chromium route/theme/viewport scans, 10 import/enrichment/inspector
  journey scans, 10 Chromium density states, 10 WebKit density states, 5 gallery
  states and 5 canonical audience states. No scan rules were excluded.
- Both Chromium and WebKit measured the same desktop geometry. Density checks
  cover focused light/dark menus, all three preferences and persistence,
  invalid-preference fallback, and actual touch contexts at 320/390/768px.
- The built-app journey passed imports, partial rejection, lost-response/reload
  recovery, provenance/manual editing, dirty-draft recovery/focus return, exact
  numeric identifiers, reviewed native enrichment, identity columns, exports,
  real overview counts and mobile inspectors.
- State probes passed stale/malformed/503 recovery, view-only presentation,
  keyboard grid movement/selection/resize/reorder, import focus containment and
  Escape, 320–1920px reflow, 200% overview text size, forced colors and reduced
  motion. Gallery probes passed stable pending-button width and named, connected
  invalid-field/dialog states.
- Canonical audience checks passed Company/Person member navigation, shareable
  inspector reloads, refresh receipts matching the actual API counts, mobile
  layout, and members-error retry without a false empty state.

The [parent instructions](../README.md) document repeatable commands. Full
transformation acceptance remains in the [completion audit](../completion-audit.md).
WebKit subset automation does not certify Safari/VoiceOver; Firefox's local
headless launch remains unresolved. Manual screen-reader checks, the full
remaining state matrix, operator pilot, performance targets and staged rollout
are not claimed complete by this compact increment.

## Captures

[Previous Companies](captures/companies-before.png) ·
[Compact Companies](captures/companies-compact-light.png) ·
[Dark Companies](captures/companies-compact-dark.png) ·
[320px touch](captures/companies-touch-320.png) ·
[Overview](captures/overview-compact.png) ·
[Workbook](captures/workbook-compact.png) ·
[People audience](captures/audience-people.png)
