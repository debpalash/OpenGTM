import { afterEach, expect, test } from "bun:test"

import { addLead } from "../src/lib/api"

const originalFetch = globalThis.fetch

afterEach(() => {
  globalThis.fetch = originalFetch
})

test("lead creation refuses the API collision response with its actionable message", async () => {
  globalThis.fetch = (async () => new Response(JSON.stringify({
    detail: { message: "Update the existing lead instead.", lead_id: 7 },
  }), { status: 409, headers: { "Content-Type": "application/json" } })) as typeof fetch

  await expect(addLead({ company: "Fixture" })).rejects.toThrow("Update the existing lead instead.")
})

test("a successful lead creation still returns the created identifier", async () => {
  globalThis.fetch = (async () => new Response(JSON.stringify({ ok: true, id: 7 }), {
    status: 200, headers: { "Content-Type": "application/json" },
  })) as typeof fetch

  await expect(addLead({ company: "Fixture" })).resolves.toEqual({ ok: true, id: 7 })
})
