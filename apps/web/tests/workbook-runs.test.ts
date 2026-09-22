import { afterEach, expect, test } from "bun:test"
import { fetchWorkbookRuns, workbookRunLabel, type WorkbookRun } from "../src/lib/workbook-runs"

const originalFetch = globalThis.fetch
test("output receipt counts reject malformed and inconsistent summaries", async () => {
  const valid = { total: 3, succeeded: 1, failed: 0, awaiting_receipt: 1, unknown: 1 }
  for (const output_attempts of [undefined, null, valid, { ...valid, total: 4 }, { ...valid, unknown: -1 }, { ...valid, succeeded: "1" }]) {
    const run = { job_id: 1, status: "completed", row_count: 1, column_count: 3,
      retry_count: 0, fill_missing: false, force: false, result: null, output_attempts }
    globalThis.fetch = (async () => Response.json({ runs: [run], has_more: false })) as typeof fetch
    if (output_attempts == null || output_attempts === valid) expect((await fetchWorkbookRuns("one")).runs).toHaveLength(1)
    else await expect(fetchWorkbookRuns("one")).rejects.toThrow("invalid run history")
  }
})
afterEach(() => { globalThis.fetch = originalFetch })
test("queue completion is not represented as verified data", () => {
  const run = { status: "completed", result: null } as WorkbookRun
  expect(workbookRunLabel(run)).toBe("Worker finished")
  expect(workbookRunLabel({ ...run, result: { errors: 1 } } as WorkbookRun)).toBe("Finished with cell errors")
  expect(workbookRunLabel({ ...run, result: { stopped: true } } as WorkbookRun)).toBe("Stopped")
  expect(workbookRunLabel({ ...run, status: "cancelled", result: { errors: 0 } } as WorkbookRun)).toBe("Cancelled")
  expect(workbookRunLabel({ ...run, status: "pending", retry_count: 1 })).toBe("Retry queued")
})
test("history API errors are not empty histories", async () => {
  globalThis.fetch = (async () => Response.json({}, { status: 503 })) as typeof fetch
  await expect(fetchWorkbookRuns("one")).rejects.toThrow("Could not load")
})
test("invalid pagination and receipt identities are rejected", async () => {
  for (const data of [{ runs: [], has_more: true }, { runs: [{ job_id: -1, status: "completed" }], has_more: false }]) {
    globalThis.fetch = (async () => Response.json(data)) as typeof fetch
    await expect(fetchWorkbookRuns("one")).rejects.toThrow("invalid run history")
  }
})
test("history cursor and workbook ID are encoded", async () => {
  globalThis.fetch = (async (input) => {
    expect(String(input)).toBe("/api/workbooks/one%2Ftwo/runs?limit=20&before_id=99")
    return Response.json({ runs: [], has_more: false, next_before_id: null })
  }) as typeof fetch
  await fetchWorkbookRuns("one/two", 99)
})
