// Writes Go server progress events (`GET /api/v2/events`) into the TanStack
// Query cache for plugin runs.
//
// Payload: {workspace_id, event, data, at, truncated?}. Plugin run events are
// plugin_run_queued | started | progress | completed | failed | cancelled,
// each with data.run_id. Progress patches the cached run in place (it fires
// up to twice a second, so it never triggers a refetch); status changes patch
// optimistically and then invalidate so the server's row wins.

import type { QueryClient } from "@tanstack/react-query"
import type { PluginRun, PluginRunStats } from "./plugin-api"
import { queryKeys } from "./query-client"

export interface PlatformEvent {
  workspace_id?: string
  event: string
  data?: Record<string, unknown>
  /** The payload exceeded the NOTIFY limit and was dropped; refetch. */
  truncated?: boolean
  at?: string
}

const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === "object" && value !== null && !Array.isArray(value)

/** Parse one SSE `data:` payload; null for anything that is not an event. */
export function parsePlatformEvent(raw: string): PlatformEvent | null {
  let value: unknown
  try { value = JSON.parse(raw) } catch { return null }
  if (!isRecord(value) || typeof value.event !== "string") return null
  return {
    workspace_id: typeof value.workspace_id === "string" ? value.workspace_id : undefined,
    event: value.event,
    data: isRecord(value.data) ? value.data : undefined,
    truncated: value.truncated === true,
    at: typeof value.at === "string" ? value.at : undefined,
  }
}

const STATUS_BY_EVENT: Record<string, PluginRun["status"]> = {
  plugin_run_started: "running",
  plugin_run_completed: "completed",
  plugin_run_failed: "failed",
  plugin_run_cancelled: "cancelled",
}

const num = (value: unknown) => typeof value === "number" && Number.isFinite(value) ? value : undefined

function patchRun(run: PluginRun, event: PlatformEvent): PluginRun {
  const data = event.data ?? {}
  switch (event.event) {
    case "plugin_run_progress": {
      // Progress can arrive after a terminal event; never move a run backwards.
      if (run.status !== "pending" && run.status !== "running") return run
      const stats: PluginRunStats = { ...(run.stats ?? {}) }
      const records = num(data.records)
      const pages = num(data.pages)
      if (records !== undefined) stats.records = Math.max(records, stats.records ?? 0)
      if (pages !== undefined) stats.pages = Math.max(pages, stats.pages ?? 0)
      return { ...run, status: "running", stats }
    }
    case "plugin_run_started":
      if (run.status !== "pending" && run.status !== "running") return run
      return { ...run, status: "running", started_at: run.started_at ?? event.at ?? null, error: null }
    case "plugin_run_completed":
    case "plugin_run_failed":
    case "plugin_run_cancelled": {
      const next: PluginRun = { ...run, status: STATUS_BY_EVENT[event.event], completed_at: run.completed_at ?? event.at ?? null }
      if (isRecord(data.stats)) next.stats = { ...(run.stats ?? {}), ...(data.stats as PluginRunStats) }
      if (typeof data.error === "string") next.error = data.error
      if (event.event === "plugin_run_cancelled" && !next.error) next.error = "Cancelled by user"
      return next
    }
    default:
      return run
  }
}

/**
 * The server stores a run's stats only when it finishes, so a refetch of an
 * active run would wipe the record and page counts that progress events
 * delivered. Keep the larger live counts while the run is still active.
 */
export function mergeLiveProgress(fresh: PluginRun, cached: PluginRun | undefined): PluginRun {
  const active = (run: PluginRun) => run.status === "pending" || run.status === "running"
  if (!cached || cached.id !== fresh.id || !active(fresh) || !active(cached) || !cached.stats) return fresh
  const stats: PluginRunStats = { ...(fresh.stats ?? {}) }
  for (const key of ["records", "pages"] as const) {
    const live = cached.stats[key]
    if (typeof live === "number" && live > (stats[key] ?? -1)) stats[key] = live
  }
  return { ...fresh, stats }
}

/**
 * Apply one platform event to the plugin run caches. Returns true when the
 * event concerned plugin runs. Events for another workspace are ignored (the
 * stream is already scoped; this guards a workspace switch mid-stream).
 */
export function applyPluginRunEvent(qc: QueryClient, event: PlatformEvent, workspaceId?: string | null): boolean {
  if (workspaceId && event.workspace_id && event.workspace_id !== workspaceId) return false
  if (!event.event.startsWith("plugin_run_")) return false
  const runId = typeof event.data?.run_id === "string" ? event.data.run_id : undefined
  if (event.truncated || !runId) {
    void qc.invalidateQueries({ queryKey: queryKeys.plugins.runs })
    return true
  }

  const patch = (run: PluginRun) => run.id === runId ? patchRun(run, event) : run
  qc.setQueryData<PluginRun[]>(queryKeys.plugins.runList, runs => runs?.map(patch))
  qc.setQueryData<PluginRun>(queryKeys.plugins.run(runId), run => run && patch(run))

  if (event.event === "plugin_run_progress") return true
  const listed = qc.getQueryData<PluginRun[]>(queryKeys.plugins.runList)
  // A run created elsewhere (another tab or teammate) is not cached yet.
  if (event.event === "plugin_run_queued" || !listed?.some(run => run.id === runId)) {
    void qc.invalidateQueries({ queryKey: queryKeys.plugins.runList })
  }
  if (event.event in STATUS_BY_EVENT) {
    void qc.invalidateQueries({ queryKey: queryKeys.plugins.runList })
    void qc.invalidateQueries({ queryKey: queryKeys.plugins.run(runId) })
    if (event.event !== "plugin_run_started") void qc.invalidateQueries({ queryKey: queryKeys.plugins.results(runId) })
  }
  return true
}
