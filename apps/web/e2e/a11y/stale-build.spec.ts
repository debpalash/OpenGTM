import { expect, test } from "@playwright/test"
import { installMocks } from "./helpers"

// A tab left open across an upgrade asks for a page file that no longer exists.
test("a missing page file reloads the tab once instead of showing an error", async ({ page }) => {
  await installMocks(page, "dark")
  let failed = 0
  await page.route(/\/assets\/audiences-[^/]+\.js$/, (route) => {
    if (failed++ === 0) return route.abort()
    return route.fallback()
  })
  await page.goto("/overview")
  await expect(page.locator("#main-content")).toBeVisible()
  await page.evaluate(() => { history.pushState({}, "", "/audiences"); dispatchEvent(new PopStateEvent("popstate")) })
  await expect(page.getByRole("heading", { name: /audiences/i }).first()).toBeVisible({ timeout: 15_000 })
  await expect(page.getByText("This page could not be displayed")).toHaveCount(0)
  expect(failed).toBeGreaterThan(1)
})
