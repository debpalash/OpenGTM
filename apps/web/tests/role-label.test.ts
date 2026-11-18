import { describe, expect, test } from "bun:test"
import { roleLabel } from "../src/lib/role-label"

describe("roleLabel", () => {
  test("maps known roles to human labels", () => {
    expect(roleLabel("global_admin")).toBe("Instance admin")
    expect(roleLabel("Global_admin")).toBe("Instance admin")
  })
  test("sentence-cases unknown roles and tolerates missing ones", () => {
    expect(roleLabel("billing_viewer")).toBe("Billing viewer")
    expect(roleLabel("member")).toBe("Member")
    expect(roleLabel(undefined)).toBe("")
  })
})
