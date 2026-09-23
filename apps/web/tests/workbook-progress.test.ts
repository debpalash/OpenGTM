import { expect, test } from "bun:test"
import { loadedCellProgress } from "../src/lib/workbook-progress"

test("loaded progress includes all execution types and separates skipped from failed", () => {
  const columns = ["research", "agent", "http", "formula", "output", "input"].map(type => ({ id: type, type }))
  const rows = [{ enrichments: {
    research: { status: "error", value: "stale" }, agent: { status: "skipped" },
    http: { status: "running" }, formula: { status: "complete", value: 0 },
    output: { status: "complete", value: false },
  } }]
  expect(loadedCellProgress(rows, columns)).toEqual({ total: 5, complete: 2, errors: 1, skipped: 1, running: 1, pending: 0 })
  expect(loadedCellProgress([], columns).total).toBe(0)
})
