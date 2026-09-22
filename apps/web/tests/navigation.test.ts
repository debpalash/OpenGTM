import { describe, expect, test } from "bun:test"
import { ALL_NAVIGATION, NAVIGATION_GROUPS, getPageTitle, isNavigationActive } from "../src/components/app-shell/navigation"

describe("shared application navigation", () => {
  test("retains every top-level product route once", () => {
    const routes = ALL_NAVIGATION.map(item => item.to)
    expect(new Set(routes).size).toBe(routes.length)
    expect([...routes].sort()).toEqual([
      "/chat", "/leads", "/audiences", "/workbooks", "/templates", "/search",
      "/agents", "/outreach", "/automations", "/watches", "/signals", "/sources",
      "/analytics", "/agency", "/campaigns", "/settings",
    ].sort())
  })
  test("groups routes without renaming backend concepts", () => {
    expect(NAVIGATION_GROUPS.map(group => group.label)).toEqual(["Workspace", "Execution", "Resources"])
    expect(getPageTitle("/agents/job-123")).toBe("Tasks")
    expect(getPageTitle("/agency/team-123")).toBe("Manage workspaces")
  })
  test("matches route boundaries rather than similarly prefixed paths", () => {
    expect(isNavigationActive("/leads/contact-123", "/leads")).toBe(true)
    expect(isNavigationActive("/leads-other", "/leads")).toBe(false)
    expect(getPageTitle("/unknown")).toBe("OpenGTM")
  })
})
