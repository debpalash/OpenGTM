import { expect, test } from "@playwright/test"
import { installMocks } from "./helpers"
test("settings opens the tab named in the link", async ({ page }) => {
  await installMocks(page, "dark")
  await page.goto("/settings?tab=email")
  await expect(page.getByRole("tab", { name: "Email" })).toHaveAttribute("aria-selected", "true")
  await page.goto("/settings?tab=nope")
  await expect(page.getByRole("tab", { name: "AI Providers" })).toHaveAttribute("aria-selected", "true")
})
