// Compile-time checks for the typed client. Nothing imports this file and it
// is never run: `tsc -b` (part of `bun run build`) fails when the client stops
// rejecting calls the OpenAPI document does not allow, or stops inferring the
// documented response types.

import { apiV2, type SuccessBody } from "./client"
import type { components } from "./schema"

type Page = components["schemas"]["PluginRunPage"]
type Same<A, B> = (<T>() => T extends A ? 1 : 2) extends (<T>() => T extends B ? 1 : 2) ? true : false
const assertType = <_T extends true>() => undefined

export async function typeChecks(signal: AbortSignal) {
  // Accepted: documented calls, with the documented response types.
  const page = await apiV2("get", "/api/v2/plugin-runs", { query: { limit: 50, cursor: "c" } })
  assertType<Same<typeof page, Page>>()
  const run = await apiV2("post", "/api/v2/plugin-runs", { body: { plugin: "acme", inputs: { domain: "acme.example" } } })
  assertType<Same<typeof run, components["schemas"]["PluginRun"]>>()
  const one = await apiV2("get", "/api/v2/plugin-runs/{id}", { path: { id: "r1" }, signal })
  assertType<Same<typeof one.status, components["schemas"]["PluginRunStatus"]>>()
  await apiV2("get", "/api/v2/plugins")
  await apiV2("get", "/api/v2/version")
  await apiV2("get", "/api/v2/plugin-runs/{id}/results", { path: { id: "r1" }, query: { offset: 0, limit: 100 } })
  assertType<Same<SuccessBody<components["schemas"]["Error"]>, never>>()

  // Rejected: everything the document does not allow.
  // @ts-expect-error unknown path
  await apiV2("get", "/api/v2/nope")
  // @ts-expect-error method not documented for this path
  await apiV2("delete", "/api/v2/plugin-runs")
  // @ts-expect-error path parameters are required
  await apiV2("get", "/api/v2/plugin-runs/{id}")
  // @ts-expect-error the request body is required
  await apiV2("post", "/api/v2/plugin-runs")
  // @ts-expect-error the body must have `plugin`
  await apiV2("post", "/api/v2/plugin-runs", { body: { inputs: {} } })
  // @ts-expect-error unknown body field
  await apiV2("post", "/api/v2/plugin-runs", { body: { plugin: "acme", extra: true } })
  // @ts-expect-error limit is a number
  await apiV2("get", "/api/v2/plugin-runs", { query: { limit: "50" } })
  // @ts-expect-error unknown query parameter
  await apiV2("get", "/api/v2/plugin-runs", { query: { offset: 3 } })
}
