import { expect, test } from "@playwright/test"
import { installMocks, runAxe } from "./helpers"

// Native window.confirm/prompt are replaced by the in-app dialog: focus moves in,
// Escape cancels and returns focus to the trigger, destructive actions are named.
test("delete task opens an accessible destructive confirm dialog", async ({ page }) => {
  let nativeDialogs = 0
  page.on("dialog", (dialog) => { nativeDialogs++; void dialog.dismiss() })
  let deletes = 0
  await installMocks(page, "light")
  await page.route((url) => url.pathname.startsWith("/api/jobs/job-1") && !url.pathname.endsWith("/cancel"), (route) => {
    if (route.request().method() === "DELETE") deletes++
    return route.fulfill({ status: 200, contentType: "application/json", body: "{}" })
  })
  await page.goto("/agents")
  const trigger = page.getByRole("button", { name: "Actions for task Logistics companies in Rotterdam" })
  await trigger.focus()
  await page.keyboard.press("Enter")
  await page.getByRole("menuitem", { name: /Delete Task & Leads/ }).click()

  const dialog = page.getByRole("dialog", { name: "Delete task “Logistics companies in Rotterdam”?" })
  await expect(dialog).toBeVisible()
  await expect(dialog.getByRole("button", { name: "Delete task and leads" })).toBeVisible()
  await expect(dialog.getByRole("button", { name: /^(OK|Confirm)$/ })).toHaveCount(0)
  await expect(dialog.locator(":focus")).toHaveCount(1) // focus moved inside
  const serious = (await runAxe(page)).filter((v) => v.impact === "serious" || v.impact === "critical")
  expect(serious).toEqual([])

  await page.keyboard.press("Escape")
  await expect(dialog).toHaveCount(0)
  await expect(trigger).toBeFocused()
  expect(deletes).toBe(0)

  await trigger.press("Enter")
  await page.getByRole("menuitem", { name: /Delete Task & Leads/ }).click()
  await page.getByRole("dialog").getByRole("button", { name: "Delete task and leads" }).click()
  await expect.poll(() => deletes).toBe(1)
  expect(nativeDialogs).toBe(0)
})
