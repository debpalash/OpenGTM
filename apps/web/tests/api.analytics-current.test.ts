import { afterEach, expect, test } from "bun:test"
import { fetchAnalyticsOverview } from "../src/lib/api"

const original = globalThis.fetch
afterEach(() => {
  globalThis.fetch = original
})

const legacyOverview = {
  total_leads: 0, leads_this_week: 0, leads_this_month: 0, avg_score: 0, tiers: {},
  enrichment: { total: 0, with_email: 0, with_phone: 0, with_website: 0, with_contact: 0, with_linkedin: 0, email_pct: 0, phone_pct: 0, website_pct: 0, contact_pct: 0 },
  email_confidence: {}, jobs: { total: 0, completed: 0, failed: 0, success_rate: 0 }
}
const currentOverview = {
  ...legacyOverview,
  records: { companies: 25, people: 40, people_with_email: 12, verified_emails: 5, companies_needing_domain: 1, email_pct: 30.0, verified_pct: 12.5 },
  activity: { window_days: 30, workbooks: 1, workbooks_run: 1, running_workbooks: 0, failed_workbooks: 0, cells_filled: 3, cells_no_data: 1, fill_rate_pct: 75.0, by_day: [{ date: "2026-11-19", filled: 3, no_data: 1 }] },
  providers: [{ provider: "apollo", cells_filled: 3, share_pct: 100.0, paid_attempts: 2, spend_usd: 0.04 }],
  spend: { window_days: 30, window_usd: 0.04, total_usd: 0.04, by_day: [{ date: "2026-11-19", usd: 0.04 }] },
  legacy: { present: false, leads: 0 }
}

test("overview accepts the current-model fields and a backend that lacks them", async () => {
  globalThis.fetch = (async () => new Response(JSON.stringify(currentOverview))) as typeof fetch
  expect((await fetchAnalyticsOverview()).records?.companies).toBe(25)
  globalThis.fetch = (async () => new Response(JSON.stringify(legacyOverview))) as typeof fetch
  expect((await fetchAnalyticsOverview()).records).toBeUndefined()
})

test("overview rejects a malformed current-model block instead of showing zeros", async () => {
  for (const bad of [
    { ...currentOverview, records: { companies: "many" } },
    { ...currentOverview, activity: { ...currentOverview.activity, by_day: [{ date: 1 }] } },
    { ...currentOverview, providers: [{ provider: "x" }] },
    { ...currentOverview, legacy: { present: "no", leads: 0 } }
  ]) {
    globalThis.fetch = (async () => new Response(JSON.stringify(bad))) as typeof fetch
    await expect(fetchAnalyticsOverview()).rejects.toThrow("invalid")
  }
})
