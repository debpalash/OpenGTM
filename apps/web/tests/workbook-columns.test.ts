import { describe, expect, test } from "bun:test"
import { normalizeWorkbookColumns } from "../src/lib/workbook-columns"

describe("workbook column identities", () => {
  test("does not turn executable or unknown types into editable inputs", () => {
    const types = ["research", "http", "formula", "agent", "source", "output", "future_type"]
    expect(normalizeWorkbookColumns(types.map(type => ({ id: type, type }))).map(column => column.type)).toEqual(types)
  })
  test("retains legacy keys and defaults only absent types", () => {
    expect(normalizeWorkbookColumns([{ key: "company" }])).toEqual([{ key: "company", id: "company", lead_field: "company", type: "lead_field" }])
  })
})
