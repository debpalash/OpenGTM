import { afterEach, describe, expect, test } from "bun:test"
import { ApiError } from "../src/lib/api"
import {
  cancelPluginRun, createPluginRun, fetchPluginCatalog, fetchPluginRun, fetchPluginRunResults, PluginPlatformUnavailableError,
} from "../src/lib/plugin-api"
import { broadNetworkPattern, filterPlugins } from "../src/lib/plugin-catalog"
import { evidenceSummary, normalizeEvidence, safeHttpUrl } from "../src/lib/plugin-evidence"
import { formatDuration, resultColumns, runDurationMs } from "../src/lib/plugin-format"

const originalFetch = globalThis.fetch
afterEach(() => { globalThis.fetch = originalFetch })
const respond = (body: unknown, status = 200) => {
  const calls: { url: string; init?: RequestInit }[] = []
  globalThis.fetch = (async (url: string, init?: RequestInit) => {
    calls.push({ url, init })
    return typeof body === "string"
      ? new Response(body, { status, headers: { "Content-Type": "text/html" } })
      : Response.json(body, { status })
  }) as typeof fetch
  return calls
}

describe("plugin platform detection", () => {
  test("FastAPI's 404 for /api/v2 means the Go server is not serving this origin", async () => {
    respond({ detail: "Not Found" }, 404)
    await expect(fetchPluginCatalog()).rejects.toBeInstanceOf(PluginPlatformUnavailableError)
  })
  test("an HTML fallback page is not the Go API either", async () => {
    respond("<!doctype html><div id=root></div>")
    await expect(fetchPluginCatalog()).rejects.toBeInstanceOf(PluginPlatformUnavailableError)
  })
  test("Go's own not-found details stay ordinary API errors", async () => {
    respond({ detail: "Plugin run not found" }, 404)
    const error = await fetchPluginRun("x").catch(e => e)
    expect(error).toBeInstanceOf(ApiError)
    expect(error.status).toBe(404)
  })
  test("422, 403 and 409 surface the server detail", async () => {
    respond({ detail: "Invalid inputs: missing properties: 'domain'" }, 422)
    await expect(createPluginRun("acme", {})).rejects.toThrow("Invalid inputs: missing properties: 'domain'")
    respond({ detail: "Insufficient workspace role" }, 403)
    expect((await createPluginRun("acme", {}).catch(e => e)).status).toBe(403)
    respond({ detail: "Only pending or running plugin runs can be cancelled" }, 409)
    expect((await cancelPluginRun("r1").catch(e => e)).status).toBe(409)
  })
})

describe("plugin API requests", () => {
  test("catalog entries are normalized", async () => {
    respond({ plugins: [{ name: "acme", kind: "scraper", runtime: "declarative", inputs: null, runnable: false, unrunnable_reason: "no" }, { bad: 1 }], errors: [{ path: "/p", error: "e" }] })
    const catalog = await fetchPluginCatalog()
    expect(catalog.plugins).toHaveLength(1)
    expect(catalog.plugins[0]).toMatchObject({ name: "acme", network: [], secrets: [], tags: [], browser: false, runnable: false, unrunnable_reason: "no", signature: "unknown" })
    expect(catalog.errors).toEqual([{ path: "/p", error: "e" }])
  })
  test("create posts {plugin, inputs}; results pass offset and limit", async () => {
    const calls = respond({ id: "r1", status: "pending", stats: {} }, 202)
    expect((await createPluginRun("acme", { domain: "acme.example" })).id).toBe("r1")
    expect(calls[0].url).toBe("/api/v2/plugin-runs")
    expect(JSON.parse(String(calls[0].init?.body))).toEqual({ plugin: "acme", inputs: { domain: "acme.example" } })
    const resultCalls = respond({ results: [{ index: 100, data: { a: 1 }, evidence: null }], total: 101, offset: 100, limit: 100 })
    const page = await fetchPluginRunResults("r 1", 100, 100)
    expect(resultCalls[0].url).toBe("/api/v2/plugin-runs/r%201/results?offset=100&limit=100")
    expect(page).toEqual({ results: [{ index: 100, data: { a: 1 }, evidence: null }], total: 101, offset: 100, limit: 100 })
  })
})

describe("evidence", () => {
  test("declarative scraper evidence", () => {
    const view = normalizeEvidence({
      url: "https://acme.example/team", fetched_at: "2026-10-05T10:00:00Z", status: 200, page: 1,
      items: "css:.team-member", fields: { title: "css:.role::text", full_name: "css:.name::text" }, body_sha256: "ab12",
    })
    expect(view.sources).toEqual([{ url: "https://acme.example/team", fetchedAt: "2026-10-05T10:00:00Z", status: 200, page: 1, sha256: "ab12",
      method: undefined, contentType: undefined, bytes: undefined }])
    expect(view.itemsSelector).toBe("css:.team-member")
    expect(view.selectorKind).toBe("selector")
    expect(view.selectors.map(s => s.field)).toEqual(["full_name", "title"])
    expect(evidenceSummary(view)).toBe("acme.example")
  })
  test("declarative provider evidence", () => {
    const view = normalizeEvidence({
      source: { source_url: "https://api.acme.example/v1?key=REDACTED", method: "GET", status: 200, fetched_at: "t", bytes: 10, sha256: "cd", mappings: { company_size: "$.company.employees" } },
      confidence: 0.75, cost_usd: 0.01,
    })
    expect(view.sources[0]).toMatchObject({ url: "https://api.acme.example/v1?key=REDACTED", method: "GET", sha256: "cd", bytes: 10 })
    expect(view.selectorKind).toBe("mapping")
    expect(view).toMatchObject({ confidence: 0.75, costUsd: 0.01 })
  })
  test("wasm evidence lists every host fetch and keeps self-reported evidence apart", () => {
    const view = normalizeEvidence({ fetches: [{ url: "https://a.example/x", status: 200, sha256: "1" }, { url: "https://b.example/y", status: 404 }], plugin: { note: "hi" } })
    expect(view.sources.map(s => s.url)).toEqual(["https://a.example/x", "https://b.example/y"])
    expect(view.pluginReported).toEqual({ note: "hi" })
    expect(evidenceSummary(view)).toBe("a.example +1")
  })
  test("only http(s) URLs become links", () => {
    expect(safeHttpUrl("https://acme.example/a")).toBe("https://acme.example/a")
    expect(safeHttpUrl("javascript:alert(1)")).toBeNull()
    expect(safeHttpUrl("data:text/html,x")).toBeNull()
    expect(safeHttpUrl("acme.example")).toBeNull()
  })
})

describe("catalog and format helpers", () => {
  const plugin = (name: string, kind: string, runtime: string, extra = {}) => ({
    name, display_name: "", kind, runtime, version: "1", author: "", license: "", description: "", tags: [], source: "plugin",
    signature: "trusted", network: [], secrets: [], browser: false, inputs: null, outputs: null, runnable: true, ...extra,
  })
  test("filterPlugins matches all terms and the kind/runtime filters", () => {
    const plugins = [plugin("acme_team_page", "scraper", "declarative", { tags: ["people"] }), plugin("echo", "provider", "wasm", { description: "Echo inputs" })]
    expect(filterPlugins(plugins, "acme people", "all", "all").map(p => p.name)).toEqual(["acme_team_page"])
    expect(filterPlugins(plugins, "", "provider", "all").map(p => p.name)).toEqual(["echo"])
    expect(filterPlugins(plugins, "echo", "all", "declarative")).toEqual([])
  })
  test("broad network patterns are flagged", () => {
    expect(broadNetworkPattern("https://*")).not.toBeNull()
    expect(broadNetworkPattern("http://127.0.0.1:*")).toBe("Plain HTTP")
    expect(broadNetworkPattern("https://*.acme.example")).toBeNull()
  })
  test("result columns are the union of data keys in first-seen order", () => {
    expect(resultColumns([{ data: { name: 1, title: 2 } }, { data: { email: 3, name: 4 } }])).toEqual(["name", "title", "email"])
  })
  test("durations", () => {
    expect(formatDuration(450)).toBe("450 ms")
    expect(formatDuration(1500)).toBe("1.5 s")
    expect(formatDuration(125_000)).toBe("2m 5s")
    const base = { id: "r", plugin: "p", plugin_version: null, inputs: null, job_id: null, created_by: null, created_at: "", error: null }
    expect(runDurationMs({ ...base, status: "completed", started_at: "2026-01-01T00:00:00Z", completed_at: "2026-01-01T00:00:02Z", stats: { duration_ms: 1800 } })).toBe(1800)
    expect(runDurationMs({ ...base, status: "running", started_at: "2026-01-01T00:00:00Z", completed_at: null, stats: null }, Date.parse("2026-01-01T00:00:05Z"))).toBe(5000)
    expect(runDurationMs({ ...base, status: "pending", started_at: null, completed_at: null, stats: null })).toBeNull()
  })
})
