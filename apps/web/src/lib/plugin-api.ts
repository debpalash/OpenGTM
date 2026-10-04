// Client for the Go plugin platform (`/api/v2/plugins`, `/api/v2/plugin-runs`).
//
// Auth and workspace headers come from the global fetch interceptor
// (lib/auth.ts), like every other /api call. When the dashboard is served by
// the legacy nginx/FastAPI stack, /api/v2/* reaches FastAPI and answers 404
// "Not Found" (or the SPA's HTML); that becomes PluginPlatformUnavailableError
// so the page can explain how to start the Go server instead of erroring.

import { ApiError } from "./api"

export type PluginKind = "provider" | "scraper" | "signal" | "destination" | "function" | "tool"
export type PluginRuntime = "declarative" | "wasm" | "process"

/** The subset of JSON Schema the run form understands. */
export interface JsonSchema {
  type?: string | string[]
  title?: string
  description?: string
  properties?: Record<string, JsonSchema>
  required?: string[]
  additionalProperties?: boolean | JsonSchema
  enum?: unknown[]
  const?: unknown
  default?: unknown
  format?: string
  pattern?: string
  minimum?: number
  maximum?: number
  minLength?: number
  maxLength?: number
  items?: JsonSchema
  examples?: unknown[]
}

export interface PluginInfo {
  name: string
  display_name: string
  kind: PluginKind | string
  runtime: PluginRuntime | string
  version: string
  author: string
  license: string
  description: string
  tags: string[]
  /** "plugin" for manifest v2, "connector" for a v1 connector loaded as a provider. */
  source: "plugin" | "connector" | string
  /** "trusted", "unsigned", or another verification outcome. */
  signature: string
  network: string[]
  secrets: string[]
  browser: boolean
  inputs: JsonSchema | null
  outputs: unknown
  runnable: boolean
  unrunnable_reason?: string
}

export interface PluginLoadError { path: string; error: string }

export interface PluginCatalog {
  plugins: PluginInfo[]
  /** Load failures; the server sends them to admins only. */
  errors: PluginLoadError[]
}

export type PluginRunStatus = "pending" | "running" | "completed" | "failed" | "cancelled"
export const ACTIVE_RUN_STATUSES: readonly string[] = ["pending", "running"]
export const isActiveRun = (run: Pick<PluginRun, "status">) => ACTIVE_RUN_STATUSES.includes(run.status)

export interface PluginRunStats {
  records?: number
  pages?: number
  cost_usd?: number
  duration_ms?: number
  stopped?: string
  provider_error?: string
}

export interface PluginRun {
  id: string
  plugin: string
  plugin_version: string | null
  status: PluginRunStatus | string
  inputs: Record<string, unknown> | null
  job_id: number | null
  created_by: string | null
  created_at: string
  started_at: string | null
  completed_at: string | null
  error: string | null
  stats: PluginRunStats | null
}

export interface PluginResult {
  index: number
  data: Record<string, unknown>
  evidence: unknown
}

export interface PluginResultPage {
  results: PluginResult[]
  total: number
  offset: number
  limit: number
}

/** The Go plugin platform is not behind this origin (legacy stack). */
export class PluginPlatformUnavailableError extends Error {
  readonly platformUnavailable = true
  constructor() {
    super("The plugin platform needs the OpenGTM Go server.")
    this.name = "PluginPlatformUnavailableError"
  }
}

export const isPlatformUnavailable = (error: unknown): error is PluginPlatformUnavailableError =>
  error instanceof PluginPlatformUnavailableError

// 404 details the Go server itself sends; any other 404 under /api/v2 came
// from a server that does not implement the route.
const GO_NOT_FOUND = new Set(["Plugin run not found", "Plugin not installed"])

const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === "object" && value !== null && !Array.isArray(value)

async function v2<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(`/api/v2${path}`, init)
  const isJson = (res.headers.get("content-type") ?? "").includes("json")
  const body: unknown = isJson ? await res.json().catch(() => undefined) : undefined
  const detail = isRecord(body) && typeof body.detail === "string" ? body.detail : ""
  if (res.status === 404 && !GO_NOT_FOUND.has(detail)) throw new PluginPlatformUnavailableError()
  // A 2xx that is not JSON is an SPA fallback page, not the Go API.
  if (res.ok && (!isJson || body === undefined)) throw new PluginPlatformUnavailableError()
  if (!res.ok) throw new ApiError(res.status, detail || `Request failed (${res.status})`, body)
  return body as T
}

const strings = (value: unknown): string[] => Array.isArray(value) ? value.filter((v): v is string => typeof v === "string") : []

/** Coerce one catalog entry; missing arrays become empty, unknown fields pass. */
export function normalizePlugin(raw: unknown): PluginInfo | null {
  if (!isRecord(raw) || typeof raw.name !== "string" || !raw.name) return null
  const text = (key: string) => typeof raw[key] === "string" ? raw[key] as string : ""
  return {
    name: raw.name,
    display_name: text("display_name"),
    kind: text("kind"),
    runtime: text("runtime"),
    version: text("version"),
    author: text("author"),
    license: text("license"),
    description: text("description"),
    tags: strings(raw.tags),
    source: text("source") || "plugin",
    signature: text("signature") || "unknown",
    network: strings(raw.network),
    secrets: strings(raw.secrets),
    browser: raw.browser === true,
    inputs: isRecord(raw.inputs) ? raw.inputs as JsonSchema : null,
    outputs: raw.outputs,
    runnable: raw.runnable !== false,
    unrunnable_reason: typeof raw.unrunnable_reason === "string" ? raw.unrunnable_reason : undefined,
  }
}

export async function fetchPluginCatalog(): Promise<PluginCatalog> {
  const body = await v2<unknown>("/plugins")
  if (!isRecord(body) || !Array.isArray(body.plugins)) throw new Error("The plugin catalog response is malformed.")
  const errors = Array.isArray(body.errors)
    ? body.errors.filter(isRecord).map(e => ({ path: String(e.path ?? ""), error: String(e.error ?? "") }))
    : []
  return { plugins: body.plugins.map(normalizePlugin).filter((p): p is PluginInfo => p !== null), errors }
}

function normalizeRun(raw: unknown): PluginRun {
  if (!isRecord(raw) || typeof raw.id !== "string") throw new Error("A plugin run in the response is malformed.")
  return { ...(raw as unknown as PluginRun), stats: isRecord(raw.stats) ? raw.stats as PluginRunStats : null }
}

export async function fetchPluginRuns(limit = 50): Promise<PluginRun[]> {
  const body = await v2<unknown>(`/plugin-runs?limit=${limit}`)
  if (!isRecord(body) || !Array.isArray(body.runs)) throw new Error("The plugin runs response is malformed.")
  return body.runs.map(normalizeRun)
}

export async function fetchPluginRun(id: string): Promise<PluginRun> {
  return normalizeRun(await v2<unknown>(`/plugin-runs/${encodeURIComponent(id)}`))
}

export async function fetchPluginRunResults(id: string, offset = 0, limit = 100): Promise<PluginResultPage> {
  const body = await v2<unknown>(`/plugin-runs/${encodeURIComponent(id)}/results?offset=${offset}&limit=${limit}`)
  if (!isRecord(body) || !Array.isArray(body.results)) throw new Error("The plugin results response is malformed.")
  const results = body.results.filter(isRecord).map((r, i) => ({
    index: typeof r.index === "number" ? r.index : offset + i,
    data: isRecord(r.data) ? r.data : { value: r.data },
    evidence: r.evidence ?? null,
  }))
  const total = typeof body.total === "number" ? body.total : results.length
  return { results, total, offset: typeof body.offset === "number" ? body.offset : offset, limit: typeof body.limit === "number" ? body.limit : limit }
}

export async function createPluginRun(plugin: string, inputs: Record<string, unknown>): Promise<PluginRun> {
  return normalizeRun(await v2<unknown>("/plugin-runs", {
    method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ plugin, inputs }),
  }))
}

export async function cancelPluginRun(id: string): Promise<PluginRun> {
  return normalizeRun(await v2<unknown>(`/plugin-runs/${encodeURIComponent(id)}/cancel`, { method: "POST" }))
}
