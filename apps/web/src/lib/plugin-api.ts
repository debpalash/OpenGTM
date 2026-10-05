// Client for the Go plugin platform (`/api/v2/plugins`, `/api/v2/plugin-runs`).
//
// Requests go through the typed client in ./api-v2, generated from
// packages/contracts/openapi.v2.yaml, so paths, parameters and bodies are
// checked against the contract at compile time and the wire types below come
// from it. The normalizers on top are deliberate: they tolerate an older or
// newer server (a missing array becomes empty, an unknown status passes) so one
// odd record cannot blank the page.
//
// Auth and workspace headers come from the global fetch interceptor
// (lib/auth.ts), like every other /api call. When the dashboard is served by
// the legacy nginx/FastAPI stack, /api/v2/* reaches FastAPI and answers 404
// "Not Found" (or the SPA's HTML); that becomes PluginPlatformUnavailableError
// so the page can explain how to start the Go server instead of erroring.

import { apiV2, isPlatformUnavailable, PluginPlatformUnavailableError, type components } from "./api-v2/client"

export { isPlatformUnavailable, PluginPlatformUnavailableError }

type Schemas = components["schemas"]

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

/** A catalog entry. `inputs` is typed with the richer JsonSchema the run form understands. */
export type PluginInfo = Omit<Schemas["PluginInfo"], "inputs"> & { inputs: JsonSchema | null }

export type PluginLoadError = Schemas["PluginLoadError"]

export interface PluginCatalog {
  plugins: PluginInfo[]
  /** Load failures; the server sends them to admins only. */
  errors: PluginLoadError[]
}

export type PluginRunStatus = Schemas["PluginRunStatus"]
export const ACTIVE_RUN_STATUSES: readonly string[] = ["pending", "running"]
export const isActiveRun = (run: Pick<PluginRun, "status">) => ACTIVE_RUN_STATUSES.includes(run.status)

export type PluginRunStats = Schemas["PluginRunStats"]

/** A run. Unknown statuses pass through, and `stats` is null until the server has some. */
export type PluginRun = Omit<Schemas["PluginRun"], "status" | "stats"> & {
  status: PluginRunStatus | string
  stats: PluginRunStats | null
}

/** One page of the run list, newest first. `next_cursor` is null on the last page. */
export interface PluginRunPage {
  runs: PluginRun[]
  next_cursor: string | null
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

const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === "object" && value !== null && !Array.isArray(value)

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
  const body: unknown = await apiV2("get", "/api/v2/plugins")
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

/** Runs per page of the list; the server allows 1-200. */
export const RUN_PAGE_SIZE = 50

/**
 * One page of runs, newest first. Pass the previous page's `next_cursor` to
 * continue; a null cursor on the result means this was the last page. A server
 * that predates pagination sends no cursor, which reads as "no more pages".
 */
export async function fetchPluginRunPage(cursor?: string | null, limit = RUN_PAGE_SIZE): Promise<PluginRunPage> {
  const body: unknown = await apiV2("get", "/api/v2/plugin-runs", { query: { limit, cursor: cursor ?? undefined } })
  if (!isRecord(body) || !Array.isArray(body.runs)) throw new Error("The plugin runs response is malformed.")
  return { runs: body.runs.map(normalizeRun), next_cursor: typeof body.next_cursor === "string" && body.next_cursor ? body.next_cursor : null }
}

export async function fetchPluginRun(id: string): Promise<PluginRun> {
  return normalizeRun(await apiV2("get", "/api/v2/plugin-runs/{id}", { path: { id } }))
}

export async function fetchPluginRunResults(id: string, offset = 0, limit = 100): Promise<PluginResultPage> {
  const body: unknown = await apiV2("get", "/api/v2/plugin-runs/{id}/results", { path: { id }, query: { offset, limit } })
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
  return normalizeRun(await apiV2("post", "/api/v2/plugin-runs", { body: { plugin, inputs } }))
}

export async function cancelPluginRun(id: string): Promise<PluginRun> {
  return normalizeRun(await apiV2("post", "/api/v2/plugin-runs/{id}/cancel", { path: { id } }))
}
