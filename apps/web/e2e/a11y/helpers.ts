import { readFileSync } from "node:fs"
import { createRequire } from "node:module"
import type { Page, Route } from "@playwright/test"
import { defaultFor, fixtureFor } from "./fixtures/routes"

const require = createRequire(import.meta.url)
export const axeSource = readFileSync(require.resolve("axe-core/axe.min.js"), "utf8")

const json = (route: Route, body: unknown, status = 200) =>
  route.fulfill({ status, contentType: "application/json", body: JSON.stringify(body) })


/**
 * Mock every /api/** and /auth/** request and block everything that is not
 * localhost. No backend is needed.
 */
export async function installMocks(page: Page, theme: "light" | "dark") {
  await page.addInitScript((t) => {
    localStorage.setItem("theme", t)
    localStorage.setItem("yupcha_token", "a11y-fixture-token")
    localStorage.setItem("yupcha_workspace_id", "ws-1")
  }, theme)

  await page.route(
    (url) => !["127.0.0.1", "localhost"].includes(url.hostname) && url.protocol.startsWith("http"),
    (route) => route.abort("blockedbyclient")
  )

  // Realtime sockets have no backend here: accept and immediately close.
  await page.routeWebSocket(/.*/, (ws) => ws.close())

  await page.route(
    (url) => url.pathname.startsWith("/api/") || url.pathname.startsWith("/auth/"),
    (route) => {
      const req = route.request()
      const path = new URL(req.url()).pathname
      const key = `${req.method()} ${path}`
      const hit = req.method() === "GET" ? fixtureFor(path) : undefined
      if (hit !== undefined) {
        return json(route, hit)
      }
      if (process.env.A11Y_LOG_UNMATCHED) console.log("UNMATCHED", key)
      return json(route, defaultFor(path))
    }
  )
}

export interface Violation { id: string; impact: string | null; nodes: number; help: string }

export async function runAxe(page: Page): Promise<Violation[]> {
  await page.addScriptTag({ content: axeSource })
  const results = await page.evaluate(async () => {
    // @ts-expect-error injected global
    const r = await window.axe.run(document, { resultTypes: ["violations"], runOnly: { type: "tag", values: ["wcag2a", "wcag2aa", "wcag21a", "wcag21aa", "wcag22aa", "best-practice"] } })
    return r.violations.map((v: { id: string; impact: string | null; nodes: unknown[]; help: string }) => ({
      id: v.id, impact: v.impact, nodes: v.nodes.length, help: v.help
    }))
  })
  return results
}
