# Dense workstation verification

The density revision runs at `http://localhost:8002`, embedded in the local Go
API. It keeps the existing workflows and gives more space to data and tools.
The comparisons use the same synthetic 1,000-company workspace at 1440×1000.

| Measurement | Previous build | Dense workstation |
| --- | ---: | ---: |
| Complete visible company rows | 24 | 28 |
| Compact company row | 32px | 28px |
| Global bar | 40px | 36px |
| Desktop page header | 52px | 44px |
| Resource toolbar, including border | 41px | 41px |
| Expanded navigation | 192px | 192px |

This fixture fits 16.7% more complete rows. Company/People collection text stays
at 13px, with 12px metadata. The saved Standard and Comfortable row options remain
40/48px. Multiline content can grow rows; touch identity and selection targets
remain at least 44px. The navigation rail still survives reloads.

Overview metrics share one divided strip. Recent workbooks and imports stack in
the primary column beside attention and workspace tools. The workbook editor
has two deliberate tool rows, with inline title metadata on wide screens and
wrapping controls on narrow screens. Workbook lists, shared tables, inspector
facts and edit forms have tighter spacing. Table controls and multiline content
retain intrinsic height. Keyboard focus in the metric strip stays inside its
clipped edge. Navigation rows, the workspace switcher and global bar grow with
enlarged text; the 200% text-size check verifies label containment, not just
document width.

Measurements, verification outcomes, source fingerprints and built asset hashes
are retained in [results.json](results.json). The evidence identifies an
uncommitted working tree; the served index matches the built index byte for
byte. Repeatable checks live under `scripts/qa/`; use `--help` for inputs and
run workspace-switching scripts sequentially. To include workbook tool and
touch-target checks, pass `--workbook-id` to `dashboard_density.py`.

Verification passed on the final embedded build:

- 131 frontend tests and 331 assertions; lint, TypeScript/production build and
  the Go API build with embedded assets.
- 111 automated axe states, with zero recorded violations or page errors:
  23 Chromium and 23 WebKit workstation states, 10 native journey states,
  13 inspector states and 42 route/theme/viewport states. No blanket rule
  exclusions were used.
- Saved rail/density preferences, actual touch contexts at 320/390/768px,
  compact desktop geometry at 1280–1920px, metric keyboard traversal and workbook
  tool alignment. Both engines measure the workbook toolbar at 77px.
- Import/rejection/recovery, provenance, identifier precision, native enrichment
  review, grid identity, export, typed edits, real version conflicts and pending
  draft protection. Keyboard grid movement, resize/reorder, dialog containment,
  stale/error/retry and view-only states also passed.
- 200% root text-size reflow with navigation/workspace/breadcrumb containment,
  forced colors and reduced motion. WebKit link traversal uses native Option+Tab
  in this environment; its difference from plain Tab was reproduced on minimal
  HTML before adjusting the harness.

Automated checks do not close the manual assistive-technology, actual browser
zoom, performance, operator pilot or rollout requirements in the
[full completion audit](../completion-audit.md). This density pass does not
complete the full dashboard transformation or certify the Go PR for merging.

[Before](captures/companies-before.png) ·
[Companies](captures/companies-light.png) ·
[Dark](captures/companies-dark.png) ·
[Navigation rail](captures/navigation-rail.png) ·
[Laptop](captures/laptop.png) ·
[Touch](captures/companies-touch-320.png) ·
[Overview](captures/overview.png) ·
[Workbook](captures/workbook.png) ·
[Workbook touch](captures/workbook-touch-320.png) ·
[Inspector](captures/inspector.png) ·
[Enlarged text](captures/text-size-200.png)
