# Workbook controls and activity verification

Tested implementation: `f217b9d4`, 2026-10-29. The production frontend was
embedded in the Go API and served at `http://127.0.0.1:8002`.
[Recorded results](results.json) include the tested revision, asset fingerprints,
interaction outcomes and accessibility states. The served index matched the
built index byte for byte. Captures and requests use isolated synthetic fixtures.

## Delivered behavior

- Column settings provide Move left/right, a position picker, and the existing
  numeric width editor. Saves use the existing expected-order contract, retain
  failed choices, and report confirmed outcomes. The identity column stays
  pinned. The supported 80px minimum keeps three separate header action targets.
- Fast keyboard drops retain their logical target even before the drag preview
  has updated or new measurements are available. Focus returns to the current
  handle after the saved order is fetched. Pointer dragging and Escape
  cancellation retain their behavior. Announcements use column names.
- Activity height has a pointer-accessible picker, keyboard separator, pointer
  drag and persisted preference. Narrow/short screens constrain its size; tabs,
  named controls and close-focus return use the shared UI contracts.
- Activity statistics use the grid's execution types, report their loaded-row
  scope, and keep skipped results separate from failures. The Errors tab reads
  current saved results after reload and renders at most 50 failures per page.
  The Changes feed reports observed transitions, includes final results even
  after the workbook leaves running state, and does not invent history for
  results already present when a page loads.
- Settings selects, prompts and condition fields have connected labels; help
  text uses semantic colors with readable contrast. Mobile controls preserve
  touch targets and editable fields use 16px text.

## Verification

- **102 frontend tests passed**, plus lint, typecheck/production build and native
  Go API compilation with the fresh UI embedded.
- **15 new Chromium axe states passed with zero violations and no page errors**:
  light/dark settings, all activity tabs in both themes, narrow activity views
  at 320/390/768px, and actual 320px touch settings/activity contexts.
- Built-app interactions passed click ordering, position saves, numeric-width
  validation/reload, 503 and 409 retention/retry, 80/600px immediate keyboard
  drops, post-save focus, pointer dragging after keyboard use, Escape without
  saving, panel size persistence, keyboard tabs, current-error inspection,
  bounded error pagination, short-height limits and touch targets.
- The 75-error pagination case is an injected synthetic API response; real
  column layout changes are saved and read back through the running API. It
  does not claim a database load benchmark.
- Existing state regressions passed: stale/malformed/503 recovery, view-only
  presentation, grid arrows/selection, pinned widths, keyboard resize/reorder,
  import focus trapping/Escape, 320–1920px reflow, 200% overview text sizing,
  forced colors and reduced motion.
- Five isolated database tests passed under Go's race detector: literal `order`
  column routing, error/expected-version guards, width bounds, concurrent column
  edits and foreign/missing-workbook isolation. The temporary database was
  migrated before testing and dropped afterward.

## Keyboard regression

A minimized browser loop failed three of three immediate Arrow/Space drops.
The keyboard coordinate callback selected the correct column, while the drop
callback still saw the previous frame's collision target and sent no save.
The fix tracks logical keyboard intent synchronously and shares the existing
version-checked save path with pointer movement. It also restores focus after
the refreshed header has committed.

`scripts/qa/dashboard_layout.py` preserves that regression through the actual
rendered grid. It lets the keyboard sensor install its document listener on its
documented next task after pickup, then dispatches Arrow and drop together in
one task. Waiting for a completed drag preview would hide the original bug.
The older state harness now respects the same initialization boundary and
chooses a valid resize direction when the saved width is already at its maximum.

Run the [parent verification commands](../README.md) with the journey report from
an isolated synthetic workspace. API database tests require a migrated,
disposable database; the test pool helper does not apply migrations:

```sh
# From apps/backend-go, with TEST_DATABASE_URL set to that disposable database:
DATABASE_URL="$TEST_DATABASE_URL" go run ./cmd/migrate up
go test -race -tags integration ./internal/api -run 'TestGuardStrings|TestWidthBounds|TestConcurrentColumnEditsSerialize|TestForeignAndMissingWorkbook404|TestOrderLiteralNotCapturedByColumnID' -count=1 -v
```

The [completion audit](../completion-audit.md) remains authoritative for the full
transformation. This increment does not close the remaining grid-editing,
hidden-column, full execution-lifecycle, browser/screen-reader, performance or
operator-pilot gates. Transient notification placement around inspector actions
also remains in the shared-component audit.

## Captures

[Column settings](captures/settings-light.png) ·
[Dark settings](captures/settings-dark.png) ·
[320px touch settings](captures/settings-touch-320.png) ·
[Activity](captures/panel-light.png) ·
[Dark activity](captures/panel-dark.png) ·
[320px activity](captures/panel-320.png) ·
[390px activity](captures/panel-390.png)
