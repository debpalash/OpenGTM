import { describe, expect, test } from "bun:test"
import { QueryClient } from "@tanstack/react-query"
import type { PluginRun } from "../src/lib/plugin-api"
import { applyPluginRunEvent, mergeLiveProgress, parsePlatformEvent, type PlatformEvent } from "../src/lib/plugin-events"
import { flattenRuns, type RunListData } from "../src/lib/plugin-run-list"
import { runPollInterval, FALLBACK_POLL_MS, LIVE_POLL_MS } from "../src/lib/plugin-hooks"
import { queryKeys } from "../src/lib/query-client"

const run = (id: string, patch: Partial<PluginRun> = {}): PluginRun => ({
  id, plugin: "acme_team_page", plugin_version: "0.1.0", status: "pending", inputs: { domain: "acme.example" },
  job_id: 7, created_by: "ada", created_at: "2026-10-05T10:00:00Z", started_at: null, completed_at: null, error: null, stats: null,
  ...patch,
})

const event = (name: string, data: Record<string, unknown>, extra: Partial<PlatformEvent> = {}): PlatformEvent =>
  ({ workspace_id: "ws1", event: name, data, at: "2026-10-05T10:00:05Z", ...extra })

/** The list as the infinite query stores it: pages of runs, here split in two. */
const pagesOf = (runs: PluginRun[]): RunListData => {
  const cut = Math.ceil(runs.length / 2)
  return { pages: [{ runs: runs.slice(0, cut), next_cursor: runs.length > cut ? "cursor-1" : null }, ...(runs.length > cut ? [{ runs: runs.slice(cut), next_cursor: null }] : [])], pageParams: [null, "cursor-1"].slice(0, runs.length > cut ? 2 : 1) }
}

function setup(runs: PluginRun[]) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  qc.setQueryData(queryKeys.plugins.runList, pagesOf(runs))
  for (const r of runs) qc.setQueryData(queryKeys.plugins.run(r.id), r)
  return qc
}
const listed = (qc: QueryClient, id: string) => flattenRuns(qc.getQueryData<RunListData>(queryKeys.plugins.runList)).find(r => r.id === id)
const detail = (qc: QueryClient, id: string) => qc.getQueryData<PluginRun>(queryKeys.plugins.run(id))
const invalid = (qc: QueryClient, key: readonly unknown[]) => qc.getQueryState(key)?.isInvalidated ?? false

describe("parsePlatformEvent", () => {
  test("accepts the Go event envelope and rejects anything else", () => {
    expect(parsePlatformEvent(JSON.stringify({ workspace_id: "ws1", event: "plugin_run_started", data: { run_id: "r1" }, at: "t" })))
      .toEqual({ workspace_id: "ws1", event: "plugin_run_started", data: { run_id: "r1" }, truncated: false, at: "t" })
    expect(parsePlatformEvent("not json")).toBeNull()
    expect(parsePlatformEvent(JSON.stringify({ type: "job_progress" }))).toBeNull()
    expect(parsePlatformEvent(JSON.stringify([1]))).toBeNull()
  })
})

describe("applyPluginRunEvent", () => {
  test("started moves a pending run to running in list and detail, then refetches", () => {
    const qc = setup([run("r1"), run("r2")])
    expect(applyPluginRunEvent(qc, event("plugin_run_started", { run_id: "r1", attempt: 1 }), "ws1")).toBe(true)
    expect(listed(qc, "r1")).toMatchObject({ status: "running", started_at: "2026-10-05T10:00:05Z" })
    expect(detail(qc, "r1")?.status).toBe("running")
    expect(listed(qc, "r2")?.status).toBe("pending")
    expect(invalid(qc, queryKeys.plugins.runList)).toBe(true)
    expect(invalid(qc, queryKeys.plugins.run("r1"))).toBe(true)
  })

  test("progress patches counts in place without refetching and never decreases them", () => {
    const qc = setup([run("r1", { status: "running", stats: { records: 5, pages: 1 } })])
    applyPluginRunEvent(qc, event("plugin_run_progress", { run_id: "r1", pages: 2, records: 12 }))
    expect(listed(qc, "r1")?.stats).toEqual({ records: 12, pages: 2 })
    expect(detail(qc, "r1")?.stats).toEqual({ records: 12, pages: 2 })
    applyPluginRunEvent(qc, event("plugin_run_progress", { run_id: "r1", pages: 1, records: 3 }))
    expect(listed(qc, "r1")?.stats).toEqual({ records: 12, pages: 2 })
    expect(invalid(qc, queryKeys.plugins.runList)).toBe(false)
    expect(invalid(qc, queryKeys.plugins.run("r1"))).toBe(false)
  })

  test("late progress does not resurrect a finished run", () => {
    const qc = setup([run("r1", { status: "completed", stats: { records: 4 } })])
    applyPluginRunEvent(qc, event("plugin_run_progress", { run_id: "r1", records: 9 }))
    expect(listed(qc, "r1")).toMatchObject({ status: "completed", stats: { records: 4 } })
  })

  test("completed applies final stats and refreshes the run, the list and its results", () => {
    const qc = setup([run("r1", { status: "running", stats: { records: 3 } })])
    qc.setQueryData(queryKeys.plugins.resultPage("r1", 0, 100), { results: [], total: 0, offset: 0, limit: 100 })
    const stats = { records: 10, pages: 2, cost_usd: 0, duration_ms: 1200 }
    applyPluginRunEvent(qc, event("plugin_run_completed", { run_id: "r1", plugin: "acme_team_page", stats }))
    expect(detail(qc, "r1")).toMatchObject({ status: "completed", completed_at: "2026-10-05T10:00:05Z", stats })
    expect(invalid(qc, queryKeys.plugins.run("r1"))).toBe(true)
    expect(invalid(qc, queryKeys.plugins.resultPage("r1", 0, 100))).toBe(true)
  })

  test("failed and cancelled carry the reason", () => {
    const qc = setup([run("r1", { status: "running" }), run("r2")])
    applyPluginRunEvent(qc, event("plugin_run_failed", { run_id: "r1", error: "http_500" }))
    applyPluginRunEvent(qc, event("plugin_run_cancelled", { run_id: "r2" }))
    expect(listed(qc, "r1")).toMatchObject({ status: "failed", error: "http_500" })
    expect(listed(qc, "r2")).toMatchObject({ status: "cancelled", error: "Cancelled by user" })
  })

  test("queued, unknown runs and truncated payloads refetch the list", () => {
    const qc = setup([run("r1")])
    applyPluginRunEvent(qc, event("plugin_run_queued", { run_id: "r9", plugin: "x" }))
    expect(invalid(qc, queryKeys.plugins.runList)).toBe(true)

    const qc2 = setup([run("r1")])
    applyPluginRunEvent(qc2, event("plugin_run_completed", {}, { truncated: true, data: undefined }))
    expect(invalid(qc2, queryKeys.plugins.runList)).toBe(true)
    expect(invalid(qc2, queryKeys.plugins.run("r1"))).toBe(true)
  })

  test("ignores other workspaces and other event families", () => {
    const qc = setup([run("r1")])
    expect(applyPluginRunEvent(qc, event("plugin_run_started", { run_id: "r1" }, { workspace_id: "ws2" }), "ws1")).toBe(false)
    expect(applyPluginRunEvent(qc, event("job_progress", { run_id: "r1" }), "ws1")).toBe(false)
    expect(listed(qc, "r1")?.status).toBe("pending")
    expect(invalid(qc, queryKeys.plugins.runList)).toBe(false)
  })
})

describe("mergeLiveProgress", () => {
  test("a refetch of an active run keeps the live counts the server has not stored yet", () => {
    const cached = run("r1", { status: "running", stats: { records: 12, pages: 2 } })
    expect(mergeLiveProgress(run("r1", { status: "running", stats: null }), cached).stats).toEqual({ records: 12, pages: 2 })
    expect(mergeLiveProgress(run("r1", { status: "running", stats: { records: 20 } }), cached).stats).toEqual({ records: 20, pages: 2 })
  })
  test("finished runs and other runs take the server's stats as they are", () => {
    const cached = run("r1", { status: "running", stats: { records: 12 } })
    expect(mergeLiveProgress(run("r1", { status: "completed", stats: { records: 10 } }), cached).stats).toEqual({ records: 10 })
    expect(mergeLiveProgress(run("r2", { status: "running", stats: null }), cached).stats).toBeNull()
    expect(mergeLiveProgress(run("r1", { status: "running", stats: null }), undefined).stats).toBeNull()
  })
})

describe("runPollInterval", () => {
  test("polls only while a run is active, slower when the stream is live", () => {
    expect(runPollInterval(undefined, false)).toBe(false)
    expect(runPollInterval([{ status: "completed" }, { status: "failed" }], false)).toBe(false)
    expect(runPollInterval([{ status: "completed" }, { status: "running" }], false)).toBe(FALLBACK_POLL_MS)
    expect(runPollInterval([{ status: "pending" }], true)).toBe(LIVE_POLL_MS)
  })
})
