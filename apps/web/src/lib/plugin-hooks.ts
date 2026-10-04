// TanStack Query hooks and the live event stream for the Go plugin platform.

import { useEffect, useState } from "react"
import { keepPreviousData, useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { ApiError } from "./api"
import { authQuery } from "./auth"
import { useAuth } from "./auth-context"
import {
  cancelPluginRun, createPluginRun, fetchPluginCatalog, fetchPluginRun, fetchPluginRunResults, fetchPluginRuns,
  isActiveRun, isPlatformUnavailable, type PluginRun,
} from "./plugin-api"
import { applyPluginRunEvent, mergeLiveProgress, parsePlatformEvent } from "./plugin-events"
import { queryKeys } from "./query-client"

export const RUN_LIST_LIMIT = 50
/** Poll interval while a run is active and the event stream is down. */
export const FALLBACK_POLL_MS = 3_000
/** Safety-net poll while a run is active and the stream is up. */
export const LIVE_POLL_MS = 20_000

// A missing Go server is a state, not a transient error: do not retry it.
const retry = (count: number, error: unknown) => !isPlatformUnavailable(error) && count < 1

/** Poll interval for a set of runs: fast without live events, slow with them. */
export function runPollInterval(runs: Pick<PluginRun, "status">[] | undefined, live: boolean): number | false {
  if (!runs?.some(isActiveRun)) return false
  return live ? LIVE_POLL_MS : FALLBACK_POLL_MS
}

export function usePluginCatalog() {
  return useQuery({ queryKey: queryKeys.plugins.catalog, queryFn: fetchPluginCatalog, staleTime: 60_000, retry })
}

export function usePluginRuns(live: boolean, enabled = true) {
  const qc = useQueryClient()
  return useQuery({
    queryKey: queryKeys.plugins.runList,
    queryFn: async () => {
      const runs = await fetchPluginRuns(RUN_LIST_LIMIT)
      const cached = new Map((qc.getQueryData<PluginRun[]>(queryKeys.plugins.runList) ?? []).map(run => [run.id, run]))
      return runs.map(run => mergeLiveProgress(run, cached.get(run.id)))
    },
    retry,
    enabled,
    refetchInterval: query => runPollInterval(query.state.data, live),
  })
}

export function usePluginRun(id: string, live: boolean, enabled = true) {
  const qc = useQueryClient()
  return useQuery({
    queryKey: queryKeys.plugins.run(id),
    queryFn: async () => mergeLiveProgress(await fetchPluginRun(id), qc.getQueryData<PluginRun>(queryKeys.plugins.run(id))),
    retry: (count, error) => !(error instanceof ApiError && error.status === 404) && retry(count, error),
    enabled,
    refetchInterval: query => runPollInterval(query.state.data && [query.state.data], live),
  })
}

export function usePluginRunResults(id: string, offset: number, limit: number, enabled = true) {
  return useQuery({
    queryKey: queryKeys.plugins.resultPage(id, offset, limit),
    queryFn: () => fetchPluginRunResults(id, offset, limit),
    retry,
    enabled,
    placeholderData: keepPreviousData,
  })
}

/** Write a run into the list and detail caches (newest first). */
function storeRun(qc: ReturnType<typeof useQueryClient>, run: PluginRun) {
  qc.setQueryData(queryKeys.plugins.run(run.id), run)
  qc.setQueryData<PluginRun[]>(queryKeys.plugins.runList, runs =>
    runs && (runs.some(r => r.id === run.id) ? runs.map(r => r.id === run.id ? run : r) : [run, ...runs]))
}

export function useCreatePluginRun() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ plugin, inputs }: { plugin: string; inputs: Record<string, unknown> }) => createPluginRun(plugin, inputs),
    onSuccess: run => {
      storeRun(qc, run)
      void qc.invalidateQueries({ queryKey: queryKeys.plugins.runList })
    },
  })
}

export function useCancelPluginRun() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => cancelPluginRun(id),
    onSuccess: run => storeRun(qc, run),
    // 409: it finished first; show the real state.
    onSettled: (_run, _error, id) => { void qc.invalidateQueries({ queryKey: queryKeys.plugins.run(id) }) },
  })
}

export type StreamState = "connecting" | "live" | "offline"

/**
 * Subscribe to `/api/v2/events` while `enabled`, writing plugin run events
 * into the query cache. EventSource reconnects by itself after a dropped
 * connection; a refused one (non-200) closes it, and we retry with backoff.
 * Callers poll while the state is not "live".
 */
export function usePlatformEvents(enabled: boolean): StreamState {
  const qc = useQueryClient()
  // The stream is scoped to a workspace (?workspace_id=); reconnect on switch.
  const { activeWorkspaceId } = useAuth()
  const [state, setState] = useState<StreamState>("connecting")

  useEffect(() => {
    if (!enabled || typeof EventSource === "undefined") return
    let source: EventSource | null = null
    let timer: number | undefined
    let attempt = 0
    let disposed = false

    const connect = () => {
      if (disposed) return
      source = new EventSource(`/api/v2/events${authQuery()}`)
      source.onopen = () => { attempt = 0; setState("live") }
      source.onmessage = message => {
        const event = parsePlatformEvent(message.data)
        if (event) applyPluginRunEvent(qc, event, activeWorkspaceId)
      }
      source.onerror = () => {
        if (source?.readyState === EventSource.CLOSED) {
          source.close()
          setState("offline")
          const delay = Math.min(60_000, 2_000 * 2 ** attempt++)
          timer = window.setTimeout(() => { setState("connecting"); connect() }, delay)
        } else {
          setState("connecting")
        }
      }
    }
    connect()
    return () => {
      disposed = true
      window.clearTimeout(timer)
      source?.close()
    }
  }, [enabled, qc, activeWorkspaceId])

  return enabled ? state : "offline"
}
