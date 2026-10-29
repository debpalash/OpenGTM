import { expect, test } from "bun:test"
import {
  workbookActivityChanges,
  workbookActivityErrors,
  workbookActivitySummary
} from "../src/lib/workbook-activity"
import type { ColumnConfig, WorkbookLeadRow } from "../src/lib/workbook-api"

const columns: ColumnConfig[] = [
  "research",
  "agent",
  "http",
  "formula",
  "output",
  "lead_field"
].map((type) => ({
  id: type,
  name: type,
  type: type as ColumnConfig["type"],
  width: 180
}))
const row = (
  id: number,
  enrichments: WorkbookLeadRow["enrichments"]
): WorkbookLeadRow => ({
  row_id: id,
  lead_id: null,
  lead: {},
  data: { full_name: `Person ${id}` },
  enrichments
})

test("activity includes research, agents, HTTP, formulas and output while keeping skips out of failures", () => {
  const rows = [
    row(1, {
      research: { status: "error", value: null, error: "Lookup failed" },
      agent: { status: "skipped", value: null },
      http: { status: "running", value: null },
      formula: { status: "complete", value: 0 },
      output: { status: "complete", value: false }
    })
  ]
  const summary = workbookActivitySummary(rows, columns)
  expect(summary.map((column) => column.id)).toEqual([
    "research",
    "agent",
    "http",
    "formula",
    "output"
  ])
  expect(summary.reduce((total, column) => total + column.errors, 0)).toBe(1)
  expect(summary.reduce((total, column) => total + column.skipped, 0)).toBe(1)
  expect(summary.every((column) => column.total === 1)).toBe(true)
  expect(workbookActivityErrors(rows, columns)).toEqual([
    {
      id: "1:research",
      column: "research",
      company: "Person 1",
      provider: undefined,
      error: "Lookup failed"
    }
  ])
  expect(
    workbookActivitySummary([], columns).every((column) => column.total === 0)
  ).toBe(true)
})

test("initial and paged-in results are baselines, not invented completion events", () => {
  const first = workbookActivityChanges(
    [row(1, { formula: { status: "complete", value: 0 } })],
    columns,
    {},
    1
  )
  expect(first.events).toEqual([])
  const second = workbookActivityChanges(
    [row(2, { formula: { status: "error", value: null } })],
    columns,
    first.snapshot,
    2
  )
  expect(second.events).toEqual([])
})

test("final result transitions do not depend on workbook still being marked running", () => {
  const initial = workbookActivityChanges([row(1, {})], columns, {}, 1)
  const started = workbookActivityChanges(
    [row(1, { formula: { status: "running", value: null } })],
    columns,
    initial.snapshot,
    2
  )
  expect(started.events[0].type).toBe("enrich_start")
  const finalRows = [
    row(1, {
      formula: { status: "complete", value: 0 },
      output: { status: "complete", value: false },
      agent: { status: "skipped", value: null, error: "Suppressed" }
    })
  ]
  const final = workbookActivityChanges(finalRows, columns, started.snapshot, 3)
  expect(final.events.map((event) => event.type)).toEqual([
    "enrich_skip",
    "enrich_complete",
    "enrich_complete"
  ])
  expect(final.events.find((event) => event.column === "formula")?.value).toBe(
    "0"
  )
  expect(final.events.find((event) => event.column === "output")?.value).toBe(
    "false"
  )
  expect(
    workbookActivityChanges(finalRows, columns, final.snapshot, 4).events
  ).toEqual([])
})

test("reloaded failures remain inspectable even without an observed event", () => {
  const rows = [
    row(1, {
      http: {
        status: "error",
        value: null,
        error: "Rate limited",
        provider: "test-provider"
      }
    })
  ]
  expect(workbookActivityChanges(rows, columns, {}, 1).events).toEqual([])
  expect(workbookActivityErrors(rows, columns)[0]).toMatchObject({
    company: "Person 1",
    error: "Rate limited",
    provider: "test-provider"
  })
})
