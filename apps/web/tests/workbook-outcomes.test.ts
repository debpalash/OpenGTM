import { expect, test } from "bun:test"
import table from "../../backend-go/internal/cellstate/testdata/outcomes.json"
import { cellOutcome, runCountsLabel, runNeedsAttention } from "../src/lib/workbook-outcomes"
import { workbookRunLabel, type WorkbookRun } from "../src/lib/workbook-runs"

test("the web classifier agrees with the Go classifier on the shared table", () => {
  for (const row of table) expect(cellOutcome(row.status, row.error)).toBe(row.outcome as never)
})

test("run counts read as a neutral summary", () => {
  expect(runCountsLabel({ completed: 86, no_data: 89, needs_setup: 25, errors: 0 })).toBe("86 filled · 89 no data · 25 need setup")
  expect(runCountsLabel({ completed: 3, no_data: 0, needs_setup: 0, errors: 2 })).toBe("3 filled · 2 errors")
  expect(runCountsLabel({ completed: 0, no_data: 0, needs_setup: 0, errors: 0 })).toBe("")
  // A result recorded before the outcome counts existed has no such keys.
  expect(runCountsLabel({ completed: 4, errors: 1 } as never)).toBe("4 filled · 1 error")
})

test("only real errors need attention", () => {
  expect(runNeedsAttention("complete")).toBe(false)
  expect(runNeedsAttention("completed_with_errors")).toBe(true)
  expect(runNeedsAttention("failed")).toBe(true)
  expect(runNeedsAttention("running")).toBe(false)
})

test("a finished run with no-data cells is not shown as an error", () => {
  const run = { status: "completed", result: { completed: 1, no_data: 5, needs_setup: 0, errors: 0, stopped: false } } as WorkbookRun
  expect(workbookRunLabel(run)).toBe("Finished: 1 filled · 5 no data")
  const bad = { ...run, result: { ...run.result!, errors: 2 } } as WorkbookRun
  expect(workbookRunLabel(bad)).toBe("Finished with cell errors")
})
