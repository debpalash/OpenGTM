import { describe, expect, test } from "bun:test"
import {
  parseRecordOverview,
  relativeTime,
  recordTimestamp
} from "../src/lib/record-overview"
const snapshot = {
  companies: 12,
  people: 30,
  people_with_email: 20,
  verified_emails: 10,
  companies_needing_domain: 2,
  running_workbooks: 1,
  failed_workbooks: 0,
  workbooks: [],
  imports: [],
  sampled_at: "2026-10-29T00:00:00Z"
}
describe("workspace overview integrity", () => {
  test("accepts an empty but valid workspace", () =>
    expect(
      parseRecordOverview({
        ...snapshot,
        companies: 0,
        people: 0,
        people_with_email: 0,
        verified_emails: 0
      })
    ).toBeDefined())
  test("rejects missing counters instead of displaying invented zeroes", () =>
    expect(() => parseRecordOverview({ workbooks: [], imports: [] })).toThrow())
  test("rejects inconsistent email coverage", () =>
    expect(() =>
      parseRecordOverview({ ...snapshot, verified_emails: 21 })
    ).toThrow("inconsistent"))
  test("rejects malformed workbook data", () =>
    expect(() =>
      parseRecordOverview({
        ...snapshot,
        workbooks: [{ id: "abc", total_rows: -1 }]
      })
    ).toThrow())
  test("rejects negative and fractional counts", () => {
    expect(() => parseRecordOverview({ ...snapshot, companies: -1 })).toThrow()
    expect(() => parseRecordOverview({ ...snapshot, companies: 1.5 })).toThrow()
  })
  test("treats naive SQL timestamps as UTC", () =>
    expect(relativeTime("2026-10-20 10:00:00")).toBe(
      relativeTime("2026-10-20T10:00:00Z")
    ))
  test("does not invent an update time", () =>
    expect(relativeTime("broken")).toBe("Update time unavailable"))
})

test("PostgreSQL timezone offsets and microseconds produce real observation times", () =>
  expect(recordTimestamp("2026-10-20 10:00:00.123456+00")).toBe(
    Date.parse("2026-10-20T10:00:00.123Z")
  ))
test("date-only values do not mistake the day for a timezone", () =>
  expect(recordTimestamp("2026-10-20")).toBe(
    Date.parse("2026-10-20T00:00:00Z")
  ))
