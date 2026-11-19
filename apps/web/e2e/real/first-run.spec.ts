import { mkdirSync, writeFileSync } from "node:fs"
import { join } from "node:path"
import { expect, test, type Page } from "@playwright/test"

/**
 * First-run journey against a REAL stack (nginx -> api-go -> postgres), as a new user would drive it.
 * Nothing is mocked. It records every /api and /auth response, console errors/warnings and visible error
 * states per route and theme, writes screenshots and a findings.json, and fails only on hard blockers
 * (setup, sample workbook, run settling) or on any 5xx / uncaught page error.
 *
 *   OPENGTM_E2E_BASE_URL     required, the nginx URL, e.g. http://127.0.0.1:23480
 *   OPENGTM_E2E_SETUP_TOKEN  required on a fresh stack: the token api-go logs once at first start
 *   OPENGTM_E2E_OUT          output directory for screenshots and findings.json (default ./e2e-real-out)
 *
 * Needs a FRESH stack (no admin yet). The run step makes real outbound provider requests for the 25-row sample.
 */

const BASE = process.env.OPENGTM_E2E_BASE_URL
const SETUP_TOKEN = process.env.OPENGTM_E2E_SETUP_TOKEN
const OUT = process.env.OPENGTM_E2E_OUT ?? "e2e-real-out"
const PASSWORD = "e2e-Real-Backend-Pw-123"
const USER = "e2e-admin"

type Theme = "light" | "dark"
interface ApiCall { where: string; method: string; status: number; path: string }
interface Finding { where: string; kind: string; detail: string }

const calls: ApiCall[] = []
const findings: Finding[] = []
let where = "setup"

const note = (kind: string, detail: string, at = where) => {
  findings.push({ where: at, kind, detail: detail.slice(0, 600) })
}

function watch(page: Page) {
  page.on("response", async (res) => {
    const url = new URL(res.url())
    if (!url.pathname.startsWith("/api/") && !url.pathname.startsWith("/auth/")) return
    const req = res.request()
    calls.push({ where, method: req.method(), status: res.status(), path: url.pathname + url.search })
    if (res.status() >= 400) {
      let body = ""
      try { body = (await res.text()).slice(0, 300) } catch { /* body gone */ }
      note(res.status() >= 500 ? "http-5xx" : "http-4xx", `${req.method()} ${url.pathname}${url.search} -> ${res.status()} ${body}`)
    }
  })
  page.on("console", (msg) => {
    if (msg.type() === "error" || msg.type() === "warning") {
      note(`console-${msg.type()}`, `${msg.text()} @ ${msg.location().url}`)
    }
  })
  page.on("pageerror", (err) => note("pageerror", String(err)))
}

/** Visible error states and toasts, by role and by the app's own wording. */
async function visibleErrors(page: Page): Promise<string[]> {
  return page.evaluate(() => {
    const out: string[] = []
    const seen = new Set<Element>()
    const sel = "[role=alert], [data-sonner-toast], [data-state=open][role=status], .getstarted-error"
    for (const el of Array.from(document.querySelectorAll(sel))) {
      const text = (el.textContent ?? "").trim().replace(/\s+/g, " ")
      if (text && !seen.has(el) && /(error|fail|unavailable|invalid|could not|couldn.t|unable|went wrong|denied)/i.test(text)) {
        seen.add(el)
        out.push(text.slice(0, 200))
      }
    }
    const body = document.body.innerText
    for (const m of body.match(/[^\n]*(temporarily unavailable|something went wrong|Unexpected Application Error|returned an invalid response)[^\n]*/gi) ?? []) out.push(m.trim().slice(0, 200))
    return Array.from(new Set(out))
  })
}

async function settle(page: Page) {
  await page.waitForLoadState("networkidle", { timeout: 20_000 }).catch(() => note("slow", "network never went idle in 20s"))
}

async function shot(page: Page, name: string, theme: Theme | "default") {
  mkdirSync(OUT, { recursive: true })
  await page.screenshot({ path: join(OUT, `${name}-${theme}.png`), fullPage: false })
}

async function setTheme(page: Page, theme: Theme) {
  await page.evaluate((t) => localStorage.setItem("theme", t), theme)
}

async function checkRoute(page: Page, route: string, theme: Theme) {
  where = `${route} [${theme}]`
  await setTheme(page, theme)
  const res = await page.goto(route)
  if (!res || res.status() >= 400) note("http-page", `document ${route} -> ${res?.status()}`)
  await expect(page.locator("#main-content")).toBeVisible({ timeout: 20_000 })
  await settle(page)
  await page.waitForTimeout(400) // charts/transitions that ignore reduced motion
  const dark = await page.evaluate(() => document.documentElement.classList.contains("dark"))
  if (dark !== (theme === "dark")) note("theme", `expected ${theme}, html.dark=${dark}`)
  for (const text of await visibleErrors(page)) note("error-state", text)
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth + 1)
  if (overflow) note("layout", "horizontal page overflow at 1440px")
  const slug = route === "/" ? "root" : route.replace(/^\//, "").replace(/[/?=&]/g, "-")
  await shot(page, slug, theme)
  flush()
}

/** Run one named user action; a failure is recorded as a finding and the journey continues. */
async function action(name: string, fn: () => Promise<void>) {
  where = `action: ${name}`
  try {
    await fn()
  } catch (e) {
    note("action-failed", `${name}: ${String(e).split("\n")[0]}`)
  }
  flush()
}

/** findings.json is rewritten as the journey goes, so an interrupted run still leaves its evidence. */
function flush() {
  mkdirSync(OUT, { recursive: true })
  writeFileSync(join(OUT, "findings.json"), JSON.stringify({ findings, calls }, null, 2))
}

// The sidebar and admin area, from components/app-shell/navigation.ts.
const ROUTES = [
  "/overview", "/chat", "/search", "/sources", "/signals", "/watches", "/companies", "/people", "/audiences", "/leads",
  "/workbooks", "/templates", "/agents", "/outreach", "/campaigns", "/automations", "/analytics", "/settings", "/agency",
  "/notifications"
]

test.describe.configure({ mode: "serial" })

test("first-run journey on the real backend", async ({ page }) => {
  test.skip(!BASE, "set OPENGTM_E2E_BASE_URL to a running stack")
  expect(SETUP_TOKEN, "OPENGTM_E2E_SETUP_TOKEN is required on a fresh stack").toBeTruthy()
  watch(page)
  try {
    await journey(page)
  } finally {
    flush()
  }
  const fatal = findings.filter((f) => f.kind === "http-5xx" || f.kind === "pageerror")
  expect(fatal, "5xx responses or uncaught page errors").toEqual([])
})

async function journey(page: Page) {
  // 1. First run: the app sends a new visitor to /setup.
  where = "first-run"
  await page.goto("/")
  await expect(page).toHaveURL(/\/setup$/, { timeout: 20_000 })
  await shot(page, "setup", "default")
  await page.getByLabel("Username").fill(USER)
  await page.getByLabel("Password", { exact: true }).fill(PASSWORD)
  await page.getByLabel("Confirm password").fill(PASSWORD)
  await page.getByLabel("Workspace name (optional)").fill("E2E Workspace")
  await page.getByLabel("Setup token").fill(SETUP_TOKEN!)
  await page.locator("form button[type=submit]").first().click()
  await page.waitForURL((u) => !/\/(setup|login)$/.test(u.pathname), { timeout: 30_000 })
  note("info", `after setup the product landed on ${new URL(page.url()).pathname}`)
  await settle(page)
  await expect(page.locator("#main-content")).toBeVisible()

  // 2. Get started checklist: load the sample workbook, open it, start a run.
  where = "get-started"
  await page.goto("/overview")
  await settle(page)
  await shot(page, "overview-first-visit", "default")
  await expect(page.getByRole("heading", { name: "Get started" })).toBeVisible()
  await page.getByRole("button", { name: "Load sample workbook" }).click()
  // Loading the sample completes step 3, and done steps carry no actions, so the way in is step 4's link.
  const open = page.getByRole("link", { name: "Run the sample workbook" })
  await expect(open).toBeVisible({ timeout: 30_000 })
  await open.click()
  await expect(page).toHaveURL(/\/workbooks\/[^/]+$/)
  const workbookUrl = page.url()
  await settle(page)
  await shot(page, "sample-workbook-before-run", "light")
  for (const text of await visibleErrors(page)) note("error-state", text)

  where = "run"
  const id = workbookUrl.split("/").pop()!
  const state = async () => {
    const r = await page.request.get(`/api/workbooks/${id}`, { headers: await authHeaders(page) })
    const w = ((await r.json()) as { workbook?: { status?: string; total_rows?: number; completed_rows?: number } }).workbook ?? {}
    return `${w.status} ${w.completed_rows ?? 0}/${w.total_rows ?? 0}`
  }
  // The run goes through a review dialog (estimate), then "Start run".
  // OPENGTM_E2E_SKIP_RUN=1 skips it (no outbound provider traffic) when you only want the route sweep.
  if (!process.env.OPENGTM_E2E_SKIP_RUN) {
    await page.getByRole("button", { name: "Review run" }).click()
    await page.getByRole("dialog").getByRole("button", { name: "Start run" }).click()
    // Poll state, never sleep. First the run must visibly start (status leaves draft), then it must settle.
    await expect.poll(state, { timeout: 60_000, intervals: [1000, 2000] }).not.toMatch(/^draft 0\//)
    await expect
      .poll(async () => /^(running|queued|pending|processing)\b/.test(await state()), { timeout: 8 * 60_000, intervals: [3000] })
      .toBe(false)
  }
  const last = await state()
  note("info", `sample run settled at: ${last}`)
  await page.reload()
  await settle(page)
  await shot(page, "sample-workbook-after-run", "light")
  for (const text of await visibleErrors(page)) note("error-state", text)

  // 3. Every route, both themes.
  for (const route of ROUTES) for (const theme of ["light", "dark"] as Theme[]) await checkRoute(page, route, theme)
  await checkRoute(page, new URL(workbookUrl).pathname, "light")
  await checkRoute(page, new URL(workbookUrl).pathname, "dark")

  // 4. One safe, local core action per area. A failing action is a finding, the journey goes on.
  await page.goto("/overview")
  await action("create a company record", async () => {
    await page.goto("/companies")
    await page.getByRole("button", { name: "New company" }).first().click()
    await page.locator("#record-field-name").fill("E2E Test Company")
    await page.locator("#record-field-domain").fill("e2e-test-company.example")
    await page.getByRole("button", { name: "Save shared record" }).click()
    await expect(page.getByText("E2E Test Company").first()).toBeVisible({ timeout: 15_000 })
    await shot(page, "action-company-created", "light")
  })

  await action("create an audience from a filter", async () => {
    await page.goto("/companies")
    await page.getByRole("searchbox", { name: "Search companies" }).fill("stripe")
    // The filter must actually narrow the list before it is turned into an audience.
    await expect(page.getByText("1 record", { exact: true })).toBeVisible()
    await page.getByRole("checkbox", { name: "Select all records on this page" }).check()
    await page.getByRole("button", { name: "More actions" }).click()
    await page.getByRole("menuitem", { name: "Create audience" }).click()
    await expect(page).toHaveURL(/\/audiences/, { timeout: 20_000 })
    await expect(page.getByRole("region", { name: "Audience details" }).getByRole("heading", { level: 2 })).toBeVisible()
    await settle(page)
    await shot(page, "action-audience-created", "light")
    for (const text of await visibleErrors(page)) note("error-state", text)
  })

  await action("open a webhook destination dialog and cancel", async () => {
    await page.goto("/audiences")
    await settle(page)
    await page.getByRole("button", { name: "Add destination" }).click()
    const dialog = page.getByRole("dialog")
    await expect(dialog).toBeVisible()
    const type = dialog.getByLabel(/destination type/i).first()
    if (await type.count()) await type.selectOption({ label: "Webhook" }).catch(() => type.selectOption("webhook"))
    await shot(page, "action-destination-dialog", "light")
    await dialog.getByRole("button", { name: "Cancel" }).click()
    await expect(dialog).toBeHidden()
  })

  await action("create a workbook from a template", async () => {
    await page.goto("/templates")
    await settle(page)
    await page.getByRole("button", { name: "Use template" }).first().click()
    await page.waitForURL(/\/workbooks\/[^/]+/, { timeout: 30_000 }).catch(async () => {
      // Some templates open a naming dialog first.
      const go = page.getByRole("dialog").getByRole("button", { name: /^(create|use|continue|start)/i }).first()
      await go.click()
      await page.waitForURL(/\/workbooks\/[^/]+/, { timeout: 30_000 })
    })
    await settle(page)
    await shot(page, "action-template-workbook", "light")
    for (const text of await visibleErrors(page)) note("error-state", text)
  })

  await action("save a harmless setting", async () => {
    await page.goto("/settings")
    await page.getByRole("tab", { name: "Pipeline" }).click()
    const field = page.locator("#config-retry_passes")
    await field.fill("2")
    await page.locator("#main-content").getByRole("button", { name: "Save" }).first().click()
    await expect(page.getByText(/saved|updated/i).first()).toBeVisible({ timeout: 10_000 })
    await page.reload()
    await page.getByRole("tab", { name: "Pipeline" }).click()
    await expect(page.locator("#config-retry_passes")).toHaveValue("2")
    await shot(page, "action-settings-saved", "light")
  })

  await action("command palette navigation", async () => {
    await page.goto("/overview")
    await settle(page)
    await page.keyboard.press("Control+k")
    const palette = page.getByRole("dialog").first()
    await expect(palette).toBeVisible()
    await page.keyboard.type("Workbooks")
    await shot(page, "action-command-palette", "light")
    await page.keyboard.press("Enter")
    await expect(page).toHaveURL(/\/workbooks/, { timeout: 10_000 })
  })

  await action("import a 5-row CSV", async () => {
    await page.goto("/companies?import=1")
    const dialog = page.getByRole("dialog")
    await expect(dialog).toBeVisible()
    const csv = ["name,domain,industry"]
      .concat(Array.from({ length: 5 }, (_, i) => `E2E Import ${i + 1},e2e-import-${i + 1}.example,Software`))
      .join("\n")
    await dialog.getByLabel("Import file").setInputFiles({ name: "e2e-companies.csv", mimeType: "text/csv", buffer: Buffer.from(csv) })
    await shot(page, "action-import-mapping", "light")
    // Walk the wizard (choose file, map columns, review, results) by its forward button.
    for (let step = 1; step <= 5; step++) {
      const labels = await dialog.getByRole("button").allInnerTexts()
      note("info", `import step ${step} buttons: ${labels.join(" | ")}`)
      await shot(page, `action-import-step${step}`, "light")
      const forward = dialog.getByRole("button", { name: /^(map columns|validate|review|import|start import|confirm)/i }).last()
      if (!(await forward.isVisible().catch(() => false))) break
      await forward.click()
      await settle(page)
    }
    await expect(dialog.getByText("Import complete")).toBeVisible({ timeout: 20_000 })
    await shot(page, "action-import-done", "light")
  })
}

async function authHeaders(page: Page): Promise<Record<string, string>> {
  const token = await page.evaluate(() => localStorage.getItem("yupcha_token"))
  const ws = await page.evaluate(() => localStorage.getItem("yupcha_workspace_id"))
  return { ...(token ? { Authorization: `Bearer ${token}` } : {}), ...(ws ? { "X-Workspace-ID": ws } : {}) }
}
