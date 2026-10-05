import { describe, expect, test } from "bun:test"
import type { PluginRun, PluginRunPage } from "../src/lib/plugin-api"
import {
  flattenRuns, hasRun, LOAD_MORE_THRESHOLD, mapRuns, nextRunCursor, prependRun, shouldLoadMore, upsertRun, type RunListData,
} from "../src/lib/plugin-run-list"

const run = (id: string, patch: Partial<PluginRun> = {}): PluginRun => ({
  id, plugin: "acme", plugin_version: "1.0.0", status: "completed", inputs: {}, job_id: 1, created_by: "ada",
  created_at: "2026-10-05T10:00:00Z", started_at: null, completed_at: null, error: null, stats: null, ...patch,
})
const page = (ids: string[], next: string | null): PluginRunPage => ({ runs: ids.map(id => run(id)), next_cursor: next })
const data = (...pages: PluginRunPage[]): RunListData => ({ pages, pageParams: pages.map((_, i) => i === 0 ? null : `c${i}`) })

describe("flattenRuns", () => {
  test("concatenates pages in order, and an empty cache is an empty list", () => {
    expect(flattenRuns(undefined)).toEqual([])
    expect(flattenRuns(data(page(["a", "b"], "c1"), page(["c"], null))).map(r => r.id)).toEqual(["a", "b", "c"])
  })
  test("a run that shows up on two pages is listed once, the newest page's copy", () => {
    const d = data({ runs: [run("a", { status: "running" })], next_cursor: "c1" }, { runs: [run("a", { status: "pending" }), run("b")], next_cursor: null })
    expect(flattenRuns(d).map(r => [r.id, r.status])).toEqual([["a", "running"], ["b", "completed"]])
  })
})

describe("patching the paged cache", () => {
  const base = data(page(["a", "b"], "c1"), page(["c", "d"], null))
  test("mapRuns reaches every page and keeps cursors and page params", () => {
    const next = mapRuns(base, r => r.id === "d" ? { ...r, status: "failed" } : r)!
    expect(flattenRuns(next).find(r => r.id === "d")?.status).toBe("failed")
    expect(next.pages.map(p => p.next_cursor)).toEqual(["c1", null])
    expect(next.pageParams).toEqual(base.pageParams)
    expect(mapRuns(undefined, r => r)).toBeUndefined()
  })
  test("prependRun adds to the top of the first page only, and only once", () => {
    const next = prependRun(base, run("new"))!
    expect(flattenRuns(next).map(r => r.id)).toEqual(["new", "a", "b", "c", "d"])
    expect(next.pages[0].next_cursor).toBe("c1") // page 2 is still reachable from the old last row
    expect(prependRun(next, run("new"))).toBe(next)
    expect(prependRun(undefined, run("x"))).toBeUndefined()
  })
  test("upsertRun replaces a loaded run and prepends an unknown one", () => {
    expect(flattenRuns(upsertRun(base, run("c", { status: "failed" })))[2]).toMatchObject({ id: "c", status: "failed" })
    expect(flattenRuns(upsertRun(base, run("z"))).map(r => r.id)[0]).toBe("z")
    expect(hasRun(base, "d")).toBe(true)
    expect(hasRun(base, "nope")).toBe(false)
  })
})

describe("paging helpers", () => {
  test("a missing cursor means there is no next page", () => {
    expect(nextRunCursor(page(["a"], "c1"))).toBe("c1")
    expect(nextRunCursor(page(["a"], null))).toBeUndefined()
  })
  test("shouldLoadMore fires near the end only, and never while busy or at the end", () => {
    const base = { count: 100, hasNextPage: true, busy: false }
    expect(shouldLoadMore({ ...base, lastVisibleIndex: 99 - LOAD_MORE_THRESHOLD })).toBe(true)
    expect(shouldLoadMore({ ...base, lastVisibleIndex: 99 })).toBe(true)
    expect(shouldLoadMore({ ...base, lastVisibleIndex: 99 - LOAD_MORE_THRESHOLD - 1 })).toBe(false)
    expect(shouldLoadMore({ ...base, lastVisibleIndex: 99, busy: true })).toBe(false)
    expect(shouldLoadMore({ ...base, lastVisibleIndex: 99, hasNextPage: false })).toBe(false)
    expect(shouldLoadMore({ ...base, count: 0, lastVisibleIndex: -1 })).toBe(false)
    // A list shorter than the threshold loads the next page straight away.
    expect(shouldLoadMore({ ...base, count: 3, lastVisibleIndex: 0 })).toBe(true)
  })
})
