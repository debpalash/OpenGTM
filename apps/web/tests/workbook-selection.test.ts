import { describe, expect, test } from "bun:test"
import { selectedWorkbookRowIds } from "../src/lib/workbook-selection"

describe("workbook selection identity", () => {
  test("keeps exact IDs independent of display order and current page", () => {
    expect(selectedWorkbookRowIds({ "row:2001": true, "row:3": false, "row:17": true })).toEqual([2001, 17])
  })
  test("empty selection has no targets", () => {
    expect(selectedWorkbookRowIds({ "row:3": false })).toEqual([])
  })
  test("rejects an entire mixed selection instead of silently dropping or globally deleting leads", () => {
    expect(() => selectedWorkbookRowIds({ "row:1": true, "lead:2": true })).toThrow("legacy lead rows")
  })
  for (const key of ["row:NaN", "row:-1", "row:1.2", "row:9007199254740992", "0", "lead:null"]) {
    test(`rejects invalid identity ${key}`, () => {
      expect(() => selectedWorkbookRowIds({ [key]: true })).toThrow()
    })
  }
})
