# Adaptive scale and quieter overview

The dashboard defaults to Auto. On the local Mac's 1280×832 logical display
(2560×1664 Retina pixels), it renders at 85%. Appearance offers Auto, Compact
(85%) and Comfortable (100%), persisted across reloads and tabs. Auto uses
logical browser window dimensions and updates when the window changes size.
Fine-pointer viewports of at least 1024px compact when the window is at most
1600px wide or 900px high. Larger desktop windows use 100%. Touch and narrower
viewports use 100%, including when a saved compact preference exists.

Document zoom keeps dashboard chrome, content and portals proportional.
Compensated viewport heights fill the screen. A positioned body lets floating
UI calculate menu anchors in the same coordinate system. Pointer deltas for
workbook column sizing/reordering, context menus and the activity panel account
for UI scale. Browser page zoom remains available for enlargement.

Overview cards use background separation instead of outlines. Redundant
workbook, shortcut and attention dividers are removed. Dark primary and secondary
text and decorative borders are softer; input boundaries and focus rings keep
their dedicated contrast tokens. The design contract is version 5 in
`docs/specs/dashboard-design-tokens.json`.

Verified against the native embedded build on localhost:8002, 2026-10-29:

- 169 frontend tests, 565 assertions; lint, production frontend build and Go API
  build with embedded assets passed.
- Chromium and WebKit each passed 20 recorded checks: six viewport/input
  configurations, twelve light/dark axe audits, an import-dialog audit, and a
  workbook pointer-resize check. All recorded axe violations and page errors
  were empty.
- Laptop Retina 1280×832, desktop 1920×1080, narrow 900×700, touch laptop,
  touch phone 390×844, and 320px reflow retained full height and avoided
  document overflow.
- Appearance radio state, 100% preference across reload, cross-tab preference
  changes, window resizing, menu anchoring and dialog containment passed.
- A 51px visual column drag changed its layout width by 60px at 85%. Width saves
  were intercepted; the native workbook was unchanged.
- Served HTML and scale bootstrap matched the new frontend build byte for byte;
  native health returned 200.

Run `scripts/qa/dashboard_scale.py --help`. Credentials are supplied through
`OPENGTM_QA_USERNAME` and `OPENGTM_QA_PASSWORD`. Reports and screenshots are in
[scaling/](scaling/). The complete dashboard transformation remains in progress;
these checks certify this scaling and visual refinement increment.
