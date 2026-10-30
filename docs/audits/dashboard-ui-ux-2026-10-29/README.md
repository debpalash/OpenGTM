# OpenGTM dashboard UI, UX, and accessibility audit

**Baseline:** local Go-backed app on port 8002, commit `4375bbdc`, 29 October 2026. **Outcome:** functional GTM workflows with an inconsistent presentation layer and confirmed accessibility/trust defects. The next investment should establish a single visual and interaction system, then rebuild the core work surfaces around it.

The [implementation work spec](../../specs/dashboard-ui-ux-transformation.md) defines the proposed direction, tokens, layouts, work packages, dependencies, and release criteria. This audit records the current evidence. No production UI was changed by this audit.

## Method and limits

- Inspected 162 frontend source files, the application token bridge, both primitive APIs, route/navigation code, and existing frontend tests.
- Captured 12 authenticated routes in light and dark at 1440 × 1000: Chat, Companies, People, Workbooks, workbook editor, Audiences, Signals, Outreach, Analytics, Settings, Sources, and Automations.
- Captured Chat, Companies, workbook editor, and Settings at 390 × 844. Tested a synthetic workspace containing 1,000 companies and two people with different qualification results.
- Ran axe-core **4.14.0** in Chromium **156.0.8078.4** across those 28 route/theme/viewport combinations. Checked keyboard focus in the import dialog and injected read-request failures in the browser to inspect error states. No provider, campaign, source-toggle, or outreach mutation was performed.
- The automated scans reported **14 rule types and 483 affected-node occurrences**. Occurrences repeat shared controls across pages/themes; they are not 483 unique bugs. Findings are triaged below. An automated scan does not establish WCAG conformance.
- Screen-reader sessions, Safari/Firefox rendering, 320px reflow, 200–400% zoom, non-admin permissions, running/failed production jobs, and real user task studies remain release-test requirements. Source inspection informs findings where those states were not exercised.
- Wigolo and ego-browser were unavailable. Official standards were fetched through the web fallback; visual checks used Playwright. Workspace selection was restored after the populated-workspace capture.

## Findings

P0 blocks release of the redesign. P1 belongs in the core dashboard transformation. P2 completes consistency across secondary workflows.

| ID | Priority | Evidence and consequence | Implementation destination |
| --- | --- | --- | --- |
| A01 | P0 | Analytics requests forced to HTTP 503 still produce zero-valued metrics and **“Live”**, with no alert or retry. `analytics.tsx:116,140,173`. Records correctly show an error/retry for the same failure class. | UI-01: explicit unavailable/stale/empty states and data freshness. |
| A02 | P0 | White normal text on the primary action color measures **4.017:1 light / 3.647:1 dark**, below 4.5:1. Axe confirms failures on actions, badges, and selected tabs. Tertiary/faint tokens also fail normal-text contrast on their base surfaces. | UI-02: semantic palette and a complete state contrast matrix. |
| A03 | P0 | Sources has **96 unnamed switches per desktop theme**. `sources.tsx:182` supplies an ID, but the adjacent text is not connected as its accessible name. | UI-03: labeled source switches and shared field primitives. |
| A04 | P0 | Settings exposes **13 unnamed buttons** per sampled settings page, including API-key reveal controls. Labels are visually present but several lack programmatic associations. `settings.tsx:140–156`. | UI-03: accessible forms and reveal controls. |
| A05 | P0 | Workbook sortable headers become buttons containing focusable settings/resize controls. Axe reports nested interactive elements. The virtualized grid has no `aria-rowcount`/`aria-colcount`; logical row/column positions need a complete contract. `workbook-editor.tsx:1667` onward. | UI-06: grid semantics and keyboard behavior. |
| A06 | P1 | Axe confirms **33 target-size node occurrences**; examples include the 20 × 20 new-chat action, column settings, and row checkboxes. Raw undersized-target counts are candidates only: spacing and inline exceptions matter. | UI-02/UI-03: 32–44px product hit-area policy. |
| A07 | P1 | **72 label/name mismatch occurrences** repeat shell controls whose ARIA labels omit visible text, including workspace and account menus. This affects speech navigation. `app-sidebar.tsx:93,203`. | UI-04: visible text and accessible-name agreement. |
| A08 | P1 | With reduced motion enabled, `.row-stagger`, `.cell-updated`, `.col-running`, and `.skeleton-cell` still animate for 0.25s, 1.4s, 2s, and 1.8s. The existing reduction selector only covers selected component slots. | UI-02: comprehensive motion policy. |
| A09 | P1 | 141 literal-color matches outside the main token file; 385 direct Tailwind palette-color uses in 26 files; two primitive import paths with different engines/contracts. These are inventory signals, not individually proven bugs. | UI-02/UI-03: owned semantic tokens and one public primitive API. |
| A10 | P1 | 335 explicit pixel text classes in 41 files, including 145 uses of 10px and 31 of 9px. Analytics renders 8–11px annotations in the sampled viewport. Controls vary among 22/24/28/32px. | UI-02/UI-05: readable type and deliberate density. |
| A11 | P1 | Companies, People, Chat, Audiences, and Automations can repeat page-level H1s. Audiences nests another main inside the shell main. Shared sidebar content falls outside a useful landmark; no skip link was found. Multiple H1s are a hierarchy concern, not by themselves a WCAG failure. | UI-04: page-header/landmark ownership and route focus. |
| A12 | P1 | Company/people tables have five top-level file/admin actions, duplicated resource tabs, long record-edit dialogs, and dense per-row tab stops. At 390px the header/toolbar consumes a large portion of the viewport; horizontally hidden columns have little orientation support. | UI-05/UI-07/UI-08: resource workbench, inspector, and staged imports. |
| A13 | P1 | A saved enrichment-only workflow can show only row numbers and outputs, with no person/company identity. The populated workbook screenshot shows scores 55/35 but no record names. | UI-06: mandatory pinned identity context. |
| A14 | P1 | Analytics still presents legacy Lead metrics while the same workspace has 1,000 canonical Companies. The Lead total may be correct for its old definition, but it does not summarize the new warehouse. | UI-09: explicit entity scopes and canonical aggregates. |
| A15 | P1 | `bun test tests` reports **69 pass / 1 fail**. The navigation contract omits the newly added Companies and People routes. Existing tests cover useful behaviors, but no committed visual/axe route matrix exists. | UI-00/UI-12: repair the baseline and add durable quality gates. |
| A16 | P2 | Several destructive or high-consequence actions use browser `confirm`/`prompt`, while run-review surfaces use application dialogs. Terminology and feedback patterns vary by module. | UI-10/UI-11: shared confirmation, feedback, and operational states. |
| A17 | P2 | Workbook editor is 2,709 lines; Automations 1,776; Outreach 1,218; Settings 1,008. Behavior, requests, and presentation are tightly coupled in several route modules. Size is a maintenance signal rather than a rule violation. | UI-13: extract cohesive feature modules around the new patterns. |

## What is worth retaining

The token bridge and shared theme boot store already coordinate first paint and portal themes. The shell has a main landmark and a named navigation region. Workbook code already includes row/column virtualization, roving cell focus, keyboard resize, clipboard handling, run review/history, and useful error/draft recovery. These should be preserved and tested during consolidation.

The settled import-dialog keyboard test kept focus inside for 18 samples, closed with Escape, and returned focus to its trigger. Initial immediate samples included transient focus guards; the delayed confirmation is the reliable result. There were **no JavaScript page errors** during the 28 captures.

## Evidence

| Area | Capture/data |
| --- | --- |
| Record table | [Light](evidence/companies-light.png), [dark](evidence/companies-dark.png), [390px](evidence/companies-mobile.png) |
| Workbook | [Desktop](evidence/workbook-dark.png), [390px](evidence/workbook-mobile.png) |
| Home and analytics | [Chat entry](evidence/chat-light.png), [analytics](evidence/analytics-light.png), [analytics with 503s](evidence/analytics-503-light.png) |
| Secondary surfaces | [Settings](evidence/settings-light.png), [sources](evidence/sources-dark.png), [import dialog](evidence/import-dialog-dark.png) |
| Machine evidence | [Scan matrix](evidence/baseline.json), [axe summary](evidence/axe-summary.json), [token inventory](evidence/token-inventory.json), [contrast calculations](evidence/contrast.json), [state checks](evidence/state-results.json), [focus confirmation](evidence/focus-confirmation.json), [test output](evidence/frontend-test-results.txt) |

The inventory is a source-text scan, including comments/string literals where matched. Contrast calculations use flat sRGB foreground/background pairs; actual layered, hover, focus, and disabled combinations still require component-state tests. The proposed control-border colors are non-text colors and should not be used for normal text.
