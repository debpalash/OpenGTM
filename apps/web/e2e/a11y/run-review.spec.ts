import { expect, test } from "@playwright/test"
import { workbookId } from "./fixtures/data"
import { installMocks, runAxe } from "./helpers"

// Before a paid run the review names each column, the providers that charge,
// and what that column may cost on the matching rows.
test("run review shows what each column will cost", async ({ page }) => {
  await installMocks(page, "light")
  await page.route((url) => url.pathname === `/api/workbooks/${workbookId}/run/estimate`, (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        rows: 3, best_usd: 0.03, worst_usd: 0.09, catalog_complete: false, unknown_providers: ["acme_scraper"],
        note: "Catalog estimate only.",
        breakdown: [
          { column: "domain", paid_providers: ["apollo", "hunter"], best_usd: 0.03, worst_usd: 0.09, unknown_providers: [] },
          { column: "name", paid_providers: [], best_usd: 0, worst_usd: 0, unknown_providers: ["acme_scraper"] },
        ],
      }),
    }))
  await page.goto(`/workbooks/${workbookId}?review=1`)
  const dialog = page.getByRole("dialog", { name: "Review workbook run" })
  const table = dialog.getByRole("table", { name: "What will run on 3 rows" })
  await expect(table).toBeVisible()
  await expect(table.getByRole("row", { name: /Domain apollo, hunter \$0\.03–\$0\.09/ })).toBeVisible()
  await expect(table.getByRole("row", { name: /Company None in the catalog.*Unpriced: acme_scraper Unknown/ })).toBeVisible()
  const serious = (await runAxe(page)).filter((v) => v.impact === "serious" || v.impact === "critical")
  expect(serious).toEqual([])
})
