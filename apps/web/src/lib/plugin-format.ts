// Display helpers for plugin runs and results.

import type { PluginResult, PluginRun } from "./plugin-api"

const valid = (iso: string | null | undefined) => !!iso && Number.isFinite(Date.parse(iso))

export function relativeTime(iso: string | null | undefined, now = Date.now()): string {
  if (!valid(iso)) return "—"
  const seconds = Math.round((now - Date.parse(iso!)) / 1000)
  if (seconds < 45) return "just now"
  const minutes = Math.round(seconds / 60)
  if (minutes < 60) return `${minutes}m ago`
  const hours = Math.round(minutes / 60)
  if (hours < 24) return `${hours}h ago`
  const days = Math.round(hours / 24)
  if (days < 7) return `${days}d ago`
  return new Date(iso!).toLocaleDateString()
}

export const absoluteTime = (iso: string | null | undefined) => valid(iso) ? new Date(iso!).toLocaleString() : "—"

export function formatDuration(ms: number | null | undefined): string {
  if (ms == null || !Number.isFinite(ms) || ms < 0) return "—"
  if (ms < 1000) return `${Math.round(ms)} ms`
  const seconds = ms / 1000
  if (seconds < 60) return `${seconds < 10 ? seconds.toFixed(1) : Math.round(seconds)} s`
  const minutes = Math.floor(seconds / 60)
  const rest = Math.round(seconds % 60)
  if (minutes < 60) return rest ? `${minutes}m ${rest}s` : `${minutes}m`
  const hours = Math.floor(minutes / 60)
  return `${hours}h ${minutes % 60}m`
}

/**
 * Run duration: the worker's measured `duration_ms` when finished, otherwise
 * wall time from start to completion (or to now while running).
 */
export function runDurationMs(run: PluginRun, now = Date.now()): number | null {
  if (typeof run.stats?.duration_ms === "number") return run.stats.duration_ms
  if (!valid(run.started_at)) return null
  const end = valid(run.completed_at) ? Date.parse(run.completed_at!) : run.status === "running" ? now : null
  return end === null ? null : Math.max(0, end - Date.parse(run.started_at!))
}

export function formatCost(usd: number | null | undefined): string {
  if (usd == null || !Number.isFinite(usd)) return "—"
  if (usd === 0) return "$0"
  return usd < 0.01 ? `$${usd.toFixed(4)}` : `$${usd.toFixed(2)}`
}

export function formatBytes(bytes: number | null | undefined): string {
  if (bytes == null || !Number.isFinite(bytes)) return "—"
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`
  return `${(bytes / 1024 / 1024).toFixed(1)} MB`
}

/** Compact text for any JSON value in a results cell. */
export function cellText(value: unknown): string {
  if (value === null || value === undefined) return ""
  if (typeof value === "string") return value
  if (typeof value === "number" || typeof value === "boolean") return String(value)
  return JSON.stringify(value)
}

/** Union of result `data` keys in first-seen order (the results table columns). */
export function resultColumns(results: Pick<PluginResult, "data">[]): string[] {
  const keys = new Set<string>()
  for (const result of results) for (const key of Object.keys(result.data)) keys.add(key)
  return [...keys]
}
