import { existsSync, readFileSync, writeFileSync } from "node:fs"
import { fileURLToPath } from "node:url"
import { expect, test } from "@playwright/test"
import { workbookId } from "./fixtures/data"
import { externalRequests, installMocks, runAxe } from "./helpers"
import { analyticsOverview } from "./fixtures/real-shapes"

type Theme = "light" | "dark"
type Counts = Record<string, number>
type Baseline = Record<string, Counts>

const BASELINE_PATH = fileURLToPath(new URL("./baseline.json", import.meta.url))
const UPDATE = process.env.A11Y_UPDATE_BASELINE === "1"
const GATED = new Set(["serious", "critical"])

const DESKTOP = { width: 1440, height: 1000 }
const MOBILE = { width: 390, height: 844 }

const desktopRoutes = [
  "/overview", "/chat", "/companies", "/people", "/workbooks", `/workbooks/${workbookId}`,
  "/audiences", "/signals", "/outreach", "/analytics", "/settings", "/sources", "/automations",
  "/agents", "/watches", "/campaigns", "/templates", "/search", "/notifications"
]
const mobileRoutes = ["/companies", "/chat", `/workbooks/${workbookId}`, "/settings"]

const cases = [
  ...desktopRoutes.flatMap((route) => (["light", "dark"] as Theme[]).map((theme) => ({ route, theme, size: DESKTOP }))),
  ...mobileRoutes.flatMap((route) => (["light", "dark"] as Theme[]).map((theme) => ({ route, theme, size: MOBILE })))
]

const keyOf = (c: (typeof cases)[number]) => `${c.route} | ${c.theme} | ${c.size.width}x${c.size.height}`

function loadBaseline(): Baseline {
  return existsSync(BASELINE_PATH) ? (JSON.parse(readFileSync(BASELINE_PATH, "utf8")) as Baseline) : {}
}

function sortedObject<T>(o: Record<string, T>): Record<string, T> {
  return Object.fromEntries(Object.entries(o).sort(([a], [b]) => a.localeCompare(b)))
}

for (const c of cases) {
  const key = keyOf(c)
  test(key, async ({ page }, testInfo) => {
    await page.setViewportSize(c.size)
    await installMocks(page, c.theme)
    await page.goto(c.route)
    await expect(page.locator("#main-content")).toBeVisible()
    await page.waitForLoadState("networkidle")
    await expect(page.getByText("Unexpected Application Error")).toHaveCount(0)
    await expect(page.locator("html")).toHaveClass(c.theme === "dark" ? /dark/ : /^(?!.*dark).*$/)
    // Let charts / transitions that ignore prefers-reduced-motion finish, and let a failed query use its one
    // retry (query-client.ts: retry 1, 1s delay) so an error state has appeared before it is audited.
    await page.evaluate(() => document.fonts.ready)
    await page.waitForTimeout(1600)
    // A page in its error state is not what real users see: a fixture is missing or has a stale shape
    // (unmatched endpoints answer `[]`). Capture the real shape with e2e/real and add it to fixtures/real-shapes.ts.
    await expect(page.getByText(/temporarily unavailable|returned an invalid response|could not be verified/i)).toHaveCount(0)
    // No third-party requests on page view (the mock aborts them, but they are still attempts to leave).
    expect(externalRequests(page), `${c.route} contacted another origin`).toEqual([])

    const violations = await runAxe(page)
    const gated: Counts = {}
    for (const v of violations) if (v.impact && GATED.has(v.impact)) gated[v.id] = v.nodes
    const report = violations.filter((v) => !(v.impact && GATED.has(v.impact)))
    await testInfo.attach("non-gated-violations.json", {
      body: JSON.stringify(report, null, 2), contentType: "application/json"
    })
    if (report.length) {
      console.log(`[report-only] ${key}: ${report.map((v) => `${v.id}(${v.impact}) x${v.nodes}`).join(", ")}`)
    }

    if (UPDATE) {
      // Run with a single worker (see playwright.config.ts) so this merge is safe.
      const next = loadBaseline()
      if (Object.keys(gated).length) next[key] = sortedObject(gated)
      else delete next[key]
      writeFileSync(BASELINE_PATH, JSON.stringify(sortedObject(next), null, 2) + "\n")
      return
    }

    const base = loadBaseline()[key] ?? {}
    const regressions: string[] = []
    const improvements: string[] = []
    for (const [id, n] of Object.entries(gated)) {
      const allowed = base[id]
      if (allowed === undefined) regressions.push(`${id}: new serious/critical rule (${n} nodes)`)
      else if (n > allowed) regressions.push(`${id}: ${n} nodes, baseline allows ${allowed}`)
      else if (n < allowed) improvements.push(`${id}: ${n} nodes, baseline ${allowed}`)
    }
    for (const [id, allowed] of Object.entries(base)) {
      if (!(id in gated)) improvements.push(`${id}: fixed (baseline ${allowed})`)
    }
    if (improvements.length) {
      console.log(`[ratchet] ${key} improved, tighten baseline with A11Y_UPDATE_BASELINE=1: ${improvements.join("; ")}`)
    }
    expect(regressions, `New or worse serious/critical a11y violations on ${key}`).toEqual([])
  })
}

// UI-01 / audit A01: analytics requests failing with 503 must show an honest
// error state (alert + retry), never zeros or a "Live" claim.
test("/analytics | 503 unavailable | light | 1440x1000", async ({ page }) => {
  await page.setViewportSize(DESKTOP)
  await installMocks(page, "light")
  await page.route(
    (url) => url.pathname.startsWith("/api/analytics/"),
    (route) => route.fulfill({ status: 503, contentType: "application/json", body: JSON.stringify({ detail: "Temporarily unavailable" }) })
  )
  await page.goto("/analytics")
  await expect(page.locator("#main-content")).toBeVisible()
  await expect(page.getByRole("alert").filter({ hasText: "temporarily unavailable" })).toBeVisible({ timeout: 15_000 })
  await expect(page.getByRole("button", { name: /try again|retry/i })).toBeVisible()
  await expect(page.getByText(/\blive\b/i)).toHaveCount(0)
  await page.waitForLoadState("networkidle")
  await page.evaluate(() => document.fonts.ready)

  const gated: Counts = {}
  for (const v of await runAxe(page)) if (v.impact && GATED.has(v.impact)) gated[v.id] = v.nodes
  // Only debt shared with the normal /analytics case is allowed.
  const base = loadBaseline()["/analytics | light | 1440x1000"] ?? {}
  const regressions = Object.entries(gated).filter(([id, n]) => (base[id] ?? 0) < n).map(([id, n]) => `${id}: ${n}`)
  expect(regressions, "New serious/critical a11y violations on /analytics 503").toEqual([])
})

// The default analytics view is the current model; the legacy scope is a secondary filter shown only when legacy
// rows exist. Both views are audited, in both themes.
for (const theme of ["light", "dark"] as Theme[]) {
  test(`/analytics | legacy scope | ${theme} | 1440x1000`, async ({ page }) => {
    await page.setViewportSize(DESKTOP)
    await installMocks(page, theme)
    await page.goto("/analytics")
    await expect(page.getByText("Verified emails", { exact: true })).toBeVisible()
    await expect(page.getByRole("button", { name: /legacy leads/i })).toHaveAttribute("aria-pressed", "false")
    await page.getByRole("button", { name: /legacy leads/i }).click()
    await expect(page.getByRole("button", { name: /legacy leads/i })).toHaveAttribute("aria-pressed", "true")
    await expect(page.getByText("Total Leads")).toBeVisible()
    await page.waitForLoadState("networkidle")
    await page.evaluate(() => document.fonts.ready)
    await page.waitForTimeout(1600)
    await expect(page.getByText(/temporarily unavailable|returned an invalid response|could not be verified/i)).toHaveCount(0)
    const gated: Counts = {}
    for (const v of await runAxe(page)) if (v.impact && GATED.has(v.impact)) gated[v.id] = v.nodes
    const base = loadBaseline()[`/analytics | ${theme} | 1440x1000`] ?? {}
    const regressions = Object.entries(gated).filter(([id, n]) => (base[id] ?? 0) < n).map(([id, n]) => `${id}: ${n}`)
    expect(regressions, "New serious/critical a11y violations on /analytics legacy scope").toEqual([])
  })
}

test("/analytics | no legacy rows | the legacy scope is not offered", async ({ page }) => {
  await page.setViewportSize(DESKTOP)
  await installMocks(page, "light")
  await page.route("**/api/analytics/overview", async (route) => {
    await route.fulfill({ json: { ...analyticsOverview, legacy: { present: false, leads: 0 } } })
  })
  await page.goto("/analytics")
  await expect(page.getByText("Verified emails", { exact: true })).toBeVisible()
  await expect(page.getByRole("button", { name: /legacy leads/i })).toHaveCount(0)
})

test("the login page contacts no other origin", async ({ page }) => {
  await installMocks(page, "light")
  await page.addInitScript(() => localStorage.removeItem("yupcha_token"))
  await page.goto("/login")
  await expect(page.getByLabel("Username")).toBeVisible()
  await page.waitForLoadState("networkidle")
  await page.waitForTimeout(500)
  expect(externalRequests(page), "/login contacted another origin").toEqual([])
})
