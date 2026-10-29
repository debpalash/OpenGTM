import { afterEach, expect, test } from "bun:test"
import {
  fetchAnalyticsOverview,
  fetchAnalyticsPipeline,
  fetchAnalyticsCollection,
  fetchAnalyticsEnrichment,
  fetchAnalyticsLLM
} from "../src/lib/api"
const original = globalThis.fetch
afterEach(() => {
  globalThis.fetch = original
})
for (const [name, load] of Object.entries({
  overview: fetchAnalyticsOverview,
  pipeline: fetchAnalyticsPipeline,
  collection: fetchAnalyticsCollection,
  enrichment: fetchAnalyticsEnrichment,
  llm: fetchAnalyticsLLM
})) {
  test(`${name}: HTTP failure cannot become zero analytics`, async () => {
    globalThis.fetch = (async () =>
      new Response('{"detail":"Temporarily unavailable"}', {
        status: 503
      })) as typeof fetch
    await expect(load()).rejects.toThrow("Temporarily unavailable")
  })
  test(`${name}: malformed success cannot become zero analytics`, async () => {
    globalThis.fetch = (async () =>
      new Response("{}", { status: 200 })) as typeof fetch
    await expect(load()).rejects.toThrow("invalid")
  })
}
test("a genuine empty collection remains valid", async () => {
  globalThis.fetch = (async () =>
    new Response(
      '{"jobs_by_day":[],"leads_by_day":[],"avg_leads_per_job":0}'
    )) as typeof fetch
  expect((await fetchAnalyticsCollection()).avg_leads_per_job).toBe(0)
})
