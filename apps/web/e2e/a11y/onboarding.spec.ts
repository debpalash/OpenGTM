import { expect, test } from "@playwright/test"
import { installMocks, runAxe } from "./helpers"

// The first-run checklist on /overview: a labelled region with a list of four steps, progress as text,
// keyboard-operable controls, a polite live region, and a dismissal that survives a reload.
for (const theme of ["light", "dark"] as const) {
  test(`get started checklist is accessible and dismissible (${theme})`, async ({ page }) => {
    await installMocks(page, theme)
    await page.goto("/overview")
    const card = page.getByRole("region", { name: "Get started" })
    await expect(card).toBeVisible()
    await expect(card.getByText("2 of 4 done")).toBeVisible()
    await expect(card.getByRole("listitem")).toHaveCount(4)
    await expect(card.getByText("Done", { exact: true })).toHaveCount(2)
    await expect(card.getByText("To do", { exact: true })).toHaveCount(2)
    await expect(card.getByRole("link", { name: "Import a CSV" })).toHaveAttribute("href", "/companies?import=1")
    await expect(card.getByRole("button", { name: "Load sample workbook" })).toBeVisible()
    await expect(page.locator('[role="status"][aria-live="polite"]').first()).toBeAttached()

    const serious = (await runAxe(page)).filter((v) => v.impact === "serious" || v.impact === "critical")
    expect(serious).toEqual([])

    // every target is at least 24 CSS px
    for (const box of await card.locator("a, button").evaluateAll((els) => els.map((e) => e.getBoundingClientRect()))) {
      expect(box.height).toBeGreaterThanOrEqual(24)
      expect(box.width).toBeGreaterThanOrEqual(24)
    }

    await card.getByRole("button", { name: "Dismiss" }).focus()
    await page.keyboard.press("Enter")
    await expect(card).toHaveCount(0)
    await page.reload()
    await expect(page.locator("#main-content")).toBeVisible()
    await page.waitForLoadState("networkidle")
    await expect(page.getByRole("region", { name: "Get started" })).toHaveCount(0)
  })
}
