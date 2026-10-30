import { expect, test } from "bun:test"
import { LeaveGuard } from "../src/features/navigation/leave-guard"

test("context changes are immediate when no draft or save is protected", async () => {
  const guard = new LeaveGuard()
  expect(await guard.request("Switch workspaces")).toBe(true)
  expect(guard.getSnapshot()).toBeNull()
})
test("cancelling retains all editors and never replaces the reviewed action", async () => {
  const guard = new LeaveGuard()
  guard.set("record", { dirty: true, busy: false })
  guard.set("provider", { dirty: true, busy: false })
  const decision = guard.request("Switch workspaces")
  expect(await guard.request("Sign out")).toBe(false)
  expect(guard.getSnapshot()).toEqual({
    action: "Switch workspaces",
    dirtyCount: 2,
    busy: false,
    messages: [],
    busyLabel: null
  })
  guard.answer(false)
  expect(await decision).toBe(false)
  expect(guard.isProtected()).toBe(true)
})
test("discard cannot abandon an in-flight save; finishing it still requires a decision", async () => {
  const guard = new LeaveGuard()
  guard.set("record", { dirty: true, busy: true })
  let result: boolean | undefined
  const decision = guard.request("Sign out").then((value) => {
    result = value
  })
  guard.answer(true)
  await Promise.resolve()
  expect(result).toBeUndefined()
  guard.set("record", { dirty: true, busy: false })
  expect(guard.getSnapshot()?.busy).toBe(false)
  guard.answer(true)
  await decision
  expect(result).toBe(true)
})
test("revocation cancels a pending action, and unmounted editors stop blocking", async () => {
  const guard = new LeaveGuard()
  guard.set("record", { dirty: true, busy: false })
  const decision = guard.request("Switch workspaces")
  guard.cancel()
  expect(await decision).toBe(false)
  guard.set("record", null)
  expect(await guard.request("Switch workspaces")).toBe(true)
})
