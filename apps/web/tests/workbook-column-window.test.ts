import { expect, test } from "bun:test"
import { workbookColumnWindow } from "../src/lib/workbook-column-window"

test("wide windows preserve total width and absolute indexes with retained focus", () => {
  const segments = workbookColumnWindow(Array(50).fill(160), 3200, 960, [0, 1, 49], 0)
  expect(segments.reduce((sum, segment) => sum + segment.width, 0)).toBe(8000)
  expect(segments.filter(segment => segment.kind === "column").map(segment => segment.index)).toEqual([0, 1, 20, 21, 22, 23, 24, 25, 49])
  expect(segments.filter(segment => segment.kind === "spacer")).toEqual([
    { kind: "spacer", start: 2, end: 20, width: 2880 },
    { kind: "spacer", start: 26, end: 49, width: 3680 },
  ])
})

test("resized and hidden-column projections keep exact viewport boundaries", () => {
  expect(workbookColumnWindow([36, 50, 220, 400], 306, 400, [], 0)).toEqual([
    { kind: "spacer", start: 0, end: 3, width: 306 },
    { kind: "column", index: 3, width: 400 },
  ])
  expect(workbookColumnWindow([], 0, 100)).toEqual([])
  expect(() => workbookColumnWindow([NaN], 0, 100)).toThrow()
  expect(() => workbookColumnWindow([100], 0, -1)).toThrow()
})
