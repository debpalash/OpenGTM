import { expect, test } from "@playwright/test"
import { installMocks, runAxe } from "./helpers"

// Sidebar areas: the area holding the current route is open and marked current,
// the others are collapsed disclosure buttons, and the choice is remembered.
test("sidebar areas follow the disclosure pattern and remember what was opened", async ({ page }) => {
  await installMocks(page, "light")
  await page.setViewportSize({ width: 1440, height: 1000 })
  await page.goto("/workbooks")
  const nav = page.getByRole("navigation", { name: "Primary navigation" })
  const enrich = nav.getByRole("button", { name: "Enrich" })
  const find = nav.getByRole("button", { name: "Find" })

  await expect(enrich).toHaveAttribute("aria-expanded", "true")
  await expect(enrich).toHaveAttribute("aria-current", "true")
  await expect(nav.getByRole("link", { name: "Workbooks" })).toHaveAttribute("aria-current", "page")
  await expect(find).toHaveAttribute("aria-expanded", "false")
  await expect(nav.getByRole("link", { name: "Data sources" })).toBeHidden()
  // One-page area is a plain link, not a disclosure.
  await expect(nav.getByRole("link", { name: "Measure" })).toBeVisible()
  await expect(nav.getByRole("button", { name: "Measure" })).toHaveCount(0)
  // The breadcrumb names the area first.
  await expect(page.getByRole("navigation", { name: "Breadcrumb" })).toContainText("Enrich")

  // Keyboard: Enter on the focused button opens it; the target is at least 24px tall.
  await find.focus()
  await page.keyboard.press("Enter")
  await expect(find).toHaveAttribute("aria-expanded", "true")
  await expect(nav.getByRole("link", { name: "Data sources" })).toBeVisible()
  expect((await find.boundingBox())!.height).toBeGreaterThanOrEqual(24)
  expect((await nav.getByRole("link", { name: "Signals" }).boundingBox())!.height).toBeGreaterThanOrEqual(24)
  const serious = (await runAxe(page)).filter((v) => v.impact === "serious" || v.impact === "critical")
  expect(serious).toEqual([])

  // Remembered per browser across a reload.
  await page.reload()
  await expect(nav.getByRole("button", { name: "Find" })).toHaveAttribute("aria-expanded", "true")
  await expect(nav.getByRole("button", { name: "Enrich" })).toHaveAttribute("aria-expanded", "true")

  // Navigating into a collapsed area opens it and marks it current.
  await nav.getByRole("button", { name: "Enrich" }).click()
  await expect(nav.getByRole("button", { name: "Enrich" })).toHaveAttribute("aria-expanded", "false")
  await page.keyboard.press("g")
  await page.keyboard.press("b")
  await expect(page).toHaveURL(/\/companies$/)
  await expect(nav.getByRole("button", { name: "Records" })).toHaveAttribute("aria-expanded", "true")
  await expect(nav.getByRole("link", { name: "Companies" })).toHaveAttribute("aria-current", "page")
})

test("sidebar works when storage is unavailable and on a phone", async ({ page }) => {
  await installMocks(page, "light")
  await page.addInitScript(() => {
    const original = Storage.prototype.getItem
    Storage.prototype.getItem = function (key: string) {
      if (key === "opengtm.nav.areas") throw new Error("blocked")
      return original.call(this, key)
    }
  })
  await page.setViewportSize({ width: 390, height: 844 })
  await page.goto("/companies")
  await page.getByRole("button", { name: /toggle sidebar/i }).first().click()
  const nav = page.getByRole("navigation", { name: "Primary navigation" })
  await expect(nav.getByRole("button", { name: "Records" })).toHaveAttribute("aria-expanded", "true")
  await nav.getByRole("button", { name: "Engage" }).click()
  await nav.getByRole("link", { name: "Campaigns" }).click()
  await expect(page).toHaveURL(/\/campaigns$/)
})
