import { afterEach, describe, expect, test } from "bun:test"
import { InfiniteQueryObserver, QueryClient } from "@tanstack/react-query"
import { fetchPluginRunPage, RUN_PAGE_SIZE, type PluginRun } from "../src/lib/plugin-api"
import { pluginRunsOptions } from "../src/lib/plugin-hooks"
import { applyPluginRunEvent } from "../src/lib/plugin-events"
import { flattenRuns, type RunListData } from "../src/lib/plugin-run-list"
import { queryKeys } from "../src/lib/query-client"

const originalFetch = globalThis.fetch
afterEach(() => { globalThis.fetch = originalFetch })

const run = (n: number, patch: Partial<PluginRun> = {}): PluginRun => ({
  id: `run-${String(n).padStart(4, "0")}`, plugin: "acme", plugin_version: "1.0.0", status: "completed", inputs: {}, job_id: n,
  created_by: "ada", created_at: new Date(Date.UTC(2026, 9, 1) + n * 60_000).toISOString(), started_at: null, completed_at: null,
  error: null, stats: null, ...patch,
})

/**
 * A stand-in for GET /api/v2/plugin-runs with the same contract as the Go
 * handler: newest first, `limit` rows, `next_cursor` naming the last row.
 */
function fakeServer(count: number) {
  const store = Array.from({ length: count }, (_, i) => run(i + 1)).reverse() // newest first
  const requests: URL[] = []
  globalThis.fetch = (async (input: string) => {
    const url = new URL(input, "http://localhost")
    requests.push(url)
    const limit = Number(url.searchParams.get("limit") ?? 50)
    const cursor = url.searchParams.get("cursor")
    const after = cursor ? store.findIndex(r => r.id === atob(cursor)) + 1 : 0
    const rows = store.slice(after, after + limit)
    const more = after + limit < store.length
    return Response.json({ runs: rows, next_cursor: more ? btoa(rows[rows.length - 1].id) : null })
  }) as unknown as typeof fetch
  return { store, requests }
}

type Observer = InfiniteQueryObserver<unknown, Error, PluginRun[], readonly unknown[], string | null>
const observe = (qc: QueryClient, live = false) =>
  new InfiniteQueryObserver(qc, pluginRunsOptions(qc, live) as never) as unknown as Observer

describe("fetchPluginRunPage", () => {
  test("sends limit and cursor, and reads next_cursor", async () => {
    const { requests } = fakeServer(120)
    const first = await fetchPluginRunPage()
    expect(requests[0].pathname + requests[0].search).toBe("/api/v2/plugin-runs?limit=50")
    expect(first.runs).toHaveLength(RUN_PAGE_SIZE)
    expect(first.next_cursor).toBeTruthy()
    const second = await fetchPluginRunPage(first.next_cursor, 20)
    expect(requests[1].searchParams.get("cursor")).toBe(first.next_cursor!)
    expect(requests[1].searchParams.get("limit")).toBe("20")
    expect(second.runs[0].id).not.toBe(first.runs[0].id)
  })
  test("a server without pagination (no next_cursor) is a single last page", async () => {
    globalThis.fetch = (async () => Response.json({ runs: [run(1)] })) as unknown as typeof fetch
    expect(await fetchPluginRunPage()).toMatchObject({ next_cursor: null, runs: [{ id: "run-0001" }] })
  })
})

describe("the infinite run list", () => {
  test("loads page after page until the server has no more, in order and without repeats", async () => {
    const { store, requests } = fakeServer(120)
    const qc = new QueryClient()
    const observer = observe(qc)
    const unsubscribe = observer.subscribe(() => {})
    await observer.refetch()

    let result = observer.getCurrentResult()
    expect(result.data).toHaveLength(50)
    expect(result.hasNextPage).toBe(true)

    await observer.fetchNextPage()
    result = observer.getCurrentResult()
    expect(result.data).toHaveLength(100)
    expect(result.hasNextPage).toBe(true)

    await observer.fetchNextPage()
    result = observer.getCurrentResult()
    expect(result.data).toHaveLength(120)
    expect(result.hasNextPage).toBe(false)
    expect(result.data!.map(r => r.id)).toEqual(store.map(r => r.id))
    expect(new Set(result.data!.map(r => r.id)).size).toBe(120)
    expect(requests.map(u => u.searchParams.get("cursor") === null)).toEqual([true, false, false])
    unsubscribe()
  })

  test("a refetch reloads the loaded pages and picks up a run created meanwhile without shifting or repeating rows", async () => {
    const { store } = fakeServer(120)
    const qc = new QueryClient()
    const observer = observe(qc)
    const unsubscribe = observer.subscribe(() => {})
    await observer.refetch()
    await observer.fetchNextPage()
    expect(observer.getCurrentResult().data).toHaveLength(100)

    // A new run lands on the server, then the list refetches (poll or live event).
    store.unshift(run(121))
    await observer.refetch()
    const data = observer.getCurrentResult().data!
    expect(data[0].id).toBe("run-0121")
    expect(data).toHaveLength(100) // two pages' worth; the oldest row moved to page 3
    expect(data.map(r => r.id)).toEqual(store.slice(0, 100).map(r => r.id))
    expect(new Set(data.map(r => r.id)).size).toBe(100)
    expect(observer.getCurrentResult().hasNextPage).toBe(true)
    unsubscribe()
  })

  test("a refetch of an active run keeps the live progress counts the server has not stored", async () => {
    const { store } = fakeServer(3)
    store[0] = run(3, { status: "running", stats: null })
    const qc = new QueryClient()
    const observer = observe(qc)
    const unsubscribe = observer.subscribe(() => {})
    await observer.refetch()
    applyPluginRunEvent(qc, { event: "plugin_run_progress", data: { run_id: "run-0003", records: 12, pages: 2 } })
    expect(flattenRuns(qc.getQueryData<RunListData>(queryKeys.plugins.runList))[0].stats).toEqual({ records: 12, pages: 2 })
    await observer.refetch()
    expect(observer.getCurrentResult().data![0].stats).toEqual({ records: 12, pages: 2 })
    unsubscribe()
  })

  test("live events patch a run that is on a later page", async () => {
    fakeServer(120)
    const qc = new QueryClient()
    const observer = observe(qc)
    const unsubscribe = observer.subscribe(() => {})
    await observer.refetch()
    await observer.fetchNextPage()
    const onPageTwo = observer.getCurrentResult().data![75].id
    applyPluginRunEvent(qc, { event: "plugin_run_failed", data: { run_id: onPageTwo, error: "http_500" }, at: "2026-10-05T10:00:05Z" })
    expect(observer.getCurrentResult().data![75]).toMatchObject({ id: onPageTwo, status: "failed", error: "http_500" })
    unsubscribe()
  })

  test("a failed page leaves what is loaded in place and can be retried", async () => {
    const { store } = fakeServer(120)
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    const observer = observe(qc)
    const unsubscribe = observer.subscribe(() => {})
    await observer.refetch()
    const ok = globalThis.fetch
    globalThis.fetch = (async () => Response.json({ detail: "Could not list plugin runs" }, { status: 500 })) as unknown as typeof fetch
    await observer.fetchNextPage()
    let result = observer.getCurrentResult()
    expect(result.isFetchNextPageError).toBe(true)
    expect(result.data).toHaveLength(50)
    globalThis.fetch = ok
    await observer.fetchNextPage()
    result = observer.getCurrentResult()
    expect(result.isFetchNextPageError).toBe(false)
    expect(result.data!.map(r => r.id)).toEqual(store.slice(0, 100).map(r => r.id))
    unsubscribe()
  })
})
