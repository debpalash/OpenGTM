import { expect, test } from "bun:test"
import { analyticsFreshness, analyticsRegionState } from "../src/lib/analytics-state"

const empty = (d: { n: number }) => d.n === 0

test("pending query with no data is loading", () => {
  expect(analyticsRegionState({ data: undefined, isError: false })).toEqual({ kind: "loading", stale: false })
})
test("failed query with no data is error, never zeros", () => {
  expect(analyticsRegionState({ data: undefined, isError: true })).toEqual({ kind: "error", stale: false })
})
test("successful zero data is empty, not error", () => {
  expect(analyticsRegionState({ data: { n: 0 }, isError: false }, empty)).toEqual({ kind: "empty", stale: false })
})
test("successful data is loaded", () => {
  expect(analyticsRegionState({ data: { n: 3 }, isError: false }, empty)).toEqual({ kind: "loaded", stale: false })
})
test("refetch failure keeps old data but marks it stale", () => {
  expect(analyticsRegionState({ data: { n: 3 }, isError: true }, empty)).toEqual({ kind: "loaded", stale: true })
  expect(analyticsRegionState({ data: { n: 0 }, isError: true }, empty)).toEqual({ kind: "empty", stale: true })
})
test("freshness ignores regions without data and uses the oldest load", () => {
  expect(analyticsFreshness([{ data: undefined, dataUpdatedAt: 0 }, { data: 1, dataUpdatedAt: 500 }, { data: 1, dataUpdatedAt: 200 }])).toBe(200)
  expect(analyticsFreshness([{ data: undefined, dataUpdatedAt: 0 }])).toBeNull()
})
