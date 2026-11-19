import { expect, test } from "@playwright/test"
import { workbookId } from "./fixtures/data"
import { installMocks } from "./helpers"

// The shell once used CSS `zoom` to shrink everything to 85%. That made text
// about 11px and forced compensation maths in every target size. Keep it out.
for (const route of ["/companies", "/chat", "/settings"]) {
  test(`${route} | real pixel scale, no CSS zoom`, async ({ page }) => {
    await page.setViewportSize({ width: 1440, height: 1000 })
    await installMocks(page, "light")
    await page.goto(route)
    await expect(page.locator("#main-content")).toBeVisible()
    await page.waitForLoadState("networkidle")

    const body = await page.evaluate(() => getComputedStyle(document.body).fontSize)
    expect(body).toBe("14px")

    const zoomed = await page.evaluate(() =>
      [document.documentElement, ...document.querySelectorAll("body *")]
        .filter((el) => getComputedStyle(el).zoom !== "1")
        .slice(0, 5)
        .map((el) => `${el.tagName}.${(el as HTMLElement).className}`)
    )
    expect(zoomed, "no element may use CSS zoom").toEqual([])
  })
}

for (const route of ["/companies", "/chat", `/workbooks/${workbookId}`, "/settings"]) {
  test(`${route} | no horizontal page scroll at 390px`, async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 })
    await installMocks(page, "light")
    await page.goto(route)
    await expect(page.locator("#main-content")).toBeVisible()
    await page.waitForLoadState("networkidle")
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth)
    expect(overflow).toBeLessThanOrEqual(0)
  })
}
