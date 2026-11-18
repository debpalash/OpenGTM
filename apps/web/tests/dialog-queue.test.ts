import { expect, test } from "bun:test"
import { DialogQueue } from "../src/lib/dialog-queue"

test("confirm resolves true on accept and false on cancel", async () => {
  const q = new DialogQueue()
  const yes = q.askConfirm({ title: "Delete workbook “Q4 targets”?", confirmLabel: "Delete workbook", destructive: true })
  expect(q.getSnapshot()?.title).toBe("Delete workbook “Q4 targets”?")
  q.settle(true)
  expect(await yes).toBe(true)
  expect(q.getSnapshot()).toBeNull()
  const no = q.askConfirm({ title: "Remove?", confirmLabel: "Remove" })
  q.settle(false)
  expect(await no).toBe(false)
})

test("prompt resolves the text, or null on cancel", async () => {
  const q = new DialogQueue()
  const named = q.askPrompt({ title: "Name this audience", label: "Audience name", confirmLabel: "Save audience" })
  q.settle("EMEA SaaS")
  expect(await named).toBe("EMEA SaaS")
  const cancelled = q.askPrompt({ title: "Name this audience", label: "Audience name", confirmLabel: "Save audience" })
  q.settle(null)
  expect(await cancelled).toBeNull()
})

test("a prompt cannot be confirmed as a boolean and requests queue in order", async () => {
  const q = new DialogQueue()
  const p = q.askPrompt({ title: "P", label: "L", confirmLabel: "Save" })
  const c = q.askConfirm({ title: "C", confirmLabel: "Go" })
  expect(q.getSnapshot()?.title).toBe("P")
  q.settle(true)
  expect(await p).toBeNull()
  expect(q.getSnapshot()?.title).toBe("C")
  q.settle(true)
  expect(await c).toBe(true)
})

test("subscribers are notified and settle with nothing pending is a no-op", () => {
  const q = new DialogQueue()
  let calls = 0
  q.subscribe(() => { calls++ })
  q.settle(true)
  expect(calls).toBe(0)
  void q.askConfirm({ title: "T", confirmLabel: "Go" })
  expect(calls).toBe(1)
  q.settle(false)
  expect(calls).toBe(2)
})
