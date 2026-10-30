# Power workstation verification

The compact workstation runs locally at `http://localhost:8002`, with the
production UI embedded in the Go API. Tested on 2026-10-29 using synthetic data.
The served index matches the built index byte for byte. Source and build hashes,
measurements and interaction outcomes are retained in [results.json](results.json).
This evidence identifies a working tree, including the preserved inspector work.

At **1440×1000**, the same 1,000-company fixture shows **24 complete rows**:
9.1% more than the previous compact build's 22, and 60% more than the original
build's 15. Collection text sizes remain unchanged. This measures visible content
for this fixture; it is not a general productivity claim.

| Element | Previous compact build | Current build |
| --- | ---: | ---: |
| Complete visible rows | 22 | 24 |
| Compact row | 34px | 32px |
| Global bar | 44px | 40px |
| Desktop page header | 64px | 52px |
| Resource toolbar, including border | 45px | 41px |
| Expanded sidebar | 208px | 192px |

Wide page headers place their title and description on one line when space
allows; long or enlarged text can wrap. Overview panels, workbook lists and
audience navigation use tighter spacing. Overview and workbook lists use the
available width. The 52px navigation rail now survives reloads; `[` toggles it.
Compact, Standard and Comfortable row preferences remain available. Touch
controls retain at least 44px targets and the global bar grows to 48px.

Verification passed:

- 113 frontend tests, lint, TypeScript/production build, Go vet and the embedded
  Go API build. Four relevant Go record integration tests passed with the race
  detector for the inspector's company-people API filter.
- 93 automated axe scans of the updated application returned zero violations:
  42 Chromium route/theme/viewport states, 10 native journey states, 13 inspector
  states, 14 Chromium density states and 14 WebKit density states. One additional
  scan covered the previous compact build for comparison. No page errors or
  blanket rule exclusions were recorded.
- Both engines measured the same desktop geometry. Checks cover short laptop
  viewports, 1920px desktop, saved rail and row preferences, focused density menus
  in light/dark, and actual touch contexts at 320/390/768px.
- The production journey passed import/rejection/recovery, source review, typed
  edits and identifier precision, reviewed native enrichment, grid identity,
  exports, overview counts and mobile inspectors.
- Keyboard grid movement, selection, resize/reorder, import focus containment,
  stale/error/retry and view-only states, text-size doubling, forced colors and
  reduced motion passed. Inspector checks cover real version conflicts and retry,
  retained drafts across refetch/tabs, Back/reload protection, pending saves,
  paged company people, typed custom fields and employment changes.

Repeatable scripts live under `scripts/qa/`; use `--help` for inputs. The scripts
read credentials from environment variables and restore the previous active
workspace. Run workspace-switching scripts sequentially.

Manual assistive-technology checks, full browser certification, actual browser
zoom, performance and operator-release gates remain in the
[completion audit](../completion-audit.md). Workspace/context changes during an
inspector draft also remain open. This pass does not complete the full dashboard
transformation.

[Before](captures/companies-before.png) ·
[Companies](captures/companies-light.png) ·
[Dark](captures/companies-dark.png) ·
[Saved rail](captures/navigation-rail.png) ·
[Laptop](captures/laptop.png) ·
[Touch](captures/companies-touch-320.png) ·
[Overview](captures/overview.png) ·
[Workbook](captures/workbook.png)
