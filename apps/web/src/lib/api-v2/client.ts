// A thin typed fetch client for the Go server's /api/v2 surface.
//
// The types come from schema.ts, which is generated from the committed OpenAPI
// document (packages/contracts/openapi.v2.yaml) by `bun run gen:api`; this file
// adds no runtime dependency and about a kilobyte of code. Path, path
// parameters, query and request body are all checked against the document:
//
//   const page = await apiV2("get", "/api/v2/plugin-runs", { query: { limit: 50 } })
//   const run = await apiV2("get", "/api/v2/plugin-runs/{id}", { path: { id } })
//   await apiV2("post", "/api/v2/plugin-runs", { body: { plugin: "acme", inputs: {} } })
//
// Auth and workspace headers come from the global fetch interceptor
// (lib/auth.ts), like every other /api call, and fetch is looked up on every
// call so that interceptor (and test doubles) are always the one used.
//
// When the dashboard is served by the legacy nginx/FastAPI stack, /api/v2/*
// reaches FastAPI and answers 404 "Not Found" (or the SPA's HTML). That becomes
// PluginPlatformUnavailableError so a page can explain how to start the Go
// server instead of showing an error.

import { ApiError } from "../api"
import type { paths } from "./schema"

export type { components, paths } from "./schema"

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

type HttpMethod = "get" | "post" | "put" | "patch" | "delete"
// The generated path items list every HTTP method, as `never` when the path
// does not support it; only the documented ones are callable.
type MethodsOf<P extends keyof paths> = {
  [M in HttpMethod & keyof paths[P]]-?: [NonNullable<paths[P][M]>] extends [never] ? never : M
}[HttpMethod & keyof paths[P]]
type OperationOf<P extends keyof paths, M> = NonNullable<paths[P][M & keyof paths[P]]>

type PathParamsOf<O> = O extends { parameters: { path: infer P } } ? P : never
type QueryOf<O> = O extends { parameters: { query?: infer Q } } ? NonNullable<Q> : never
type BodyOf<O> = O extends { requestBody: { content: { "application/json": infer B } } } ? B : never

/** The JSON body of the operation's 2xx response. */
export type SuccessBody<O> = O extends { responses: infer R }
  ? { [S in keyof R & (200 | 201 | 202)]: R[S] extends { content: { "application/json": infer B } } ? B : never }[keyof R & (200 | 201 | 202)]
  : never

type Part<K extends string, T> = [T] extends [never] ? { [k in K]?: undefined } : { [k in K]: T }
type OptionalQuery<O> = [QueryOf<O>] extends [never] ? { query?: undefined } : { query?: QueryOf<O> }

/** Parameters and body of one operation; path params and body are required when the spec requires them. */
export type RequestInitFor<O> = Part<"path", PathParamsOf<O>> & OptionalQuery<O> & Part<"body", BodyOf<O>> & { signal?: AbortSignal }

type NeedsInit<O> = [PathParamsOf<O>] extends [never] ? ([BodyOf<O>] extends [never] ? false : true) : true
type InitArgs<O> = NeedsInit<O> extends true ? [init: RequestInitFor<O>] : [init?: RequestInitFor<O>]

const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === "object" && value !== null && !Array.isArray(value)

function expand(template: string, params: Record<string, unknown> | undefined): string {
  return template.replace(/\{(\w+)\}/g, (_, name: string) => encodeURIComponent(String(params?.[name] ?? "")))
}

function queryString(query: Record<string, unknown> | undefined): string {
  if (!query) return ""
  const search = new URLSearchParams()
  for (const [key, value] of Object.entries(query)) {
    if (value !== undefined && value !== null && value !== "") search.set(key, String(value))
  }
  const text = search.toString()
  return text ? `?${text}` : ""
}

/** Call a documented /api/v2 operation and return its parsed 2xx JSON body. */
export async function apiV2<P extends keyof paths, M extends MethodsOf<P>>(
  method: M,
  path: P,
  ...args: InitArgs<OperationOf<P, M>>
): Promise<SuccessBody<OperationOf<P, M>>> {
  const init = (args[0] ?? {}) as { path?: Record<string, unknown>; query?: Record<string, unknown>; body?: unknown; signal?: AbortSignal }
  const headers: Record<string, string> = {}
  let body: string | undefined
  if (init.body !== undefined) {
    headers["Content-Type"] = "application/json"
    body = JSON.stringify(init.body)
  }
  const url = expand(path, init.path) + queryString(init.query)
  const res = await fetch(url, { method: method.toUpperCase(), ...(body === undefined ? {} : { headers, body }), signal: init.signal })

  const isJson = (res.headers.get("content-type") ?? "").includes("json")
  const parsed: unknown = isJson ? await res.json().catch(() => undefined) : undefined
  const detail = isRecord(parsed) && typeof parsed.detail === "string" ? parsed.detail : ""
  if (res.status === 404 && !GO_NOT_FOUND.has(detail)) throw new PluginPlatformUnavailableError()
  // A 2xx that is not JSON is an SPA fallback page, not the Go API.
  if (res.ok && (!isJson || parsed === undefined)) throw new PluginPlatformUnavailableError()
  if (!res.ok) throw new ApiError(res.status, detail || `Request failed (${res.status})`, parsed)
  return parsed as SuccessBody<OperationOf<P, M>>
}
