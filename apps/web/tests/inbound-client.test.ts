import { afterEach, expect, test } from "bun:test"
import {
  fetchInboundPage,
  rotateInboundToken
} from "../src/features/activation/inbound-client"

const originalFetch = globalThis.fetch
afterEach(() => {
  globalThis.fetch = originalFetch
})
test("history errors and malformed successes cannot become empty callback histories", async () => {
  globalThis.fetch = (async () =>
    new Response("null", { status: 503 })) as typeof fetch
  await expect(fetchInboundPage("crm", null)).rejects.toThrow()
  for (const payload of [
    null,
    { receipts: null },
    { receipts: [{ id: "a" }], next_cursor: null },
    { receipts: [], next_cursor: 5 }
  ]) {
    globalThis.fetch = (async () => Response.json(payload)) as typeof fetch
    await expect(fetchInboundPage("crm", null)).rejects.toThrow("verified")
  }
})
test("valid history preserves callback identity, scope and encoded pagination", async () => {
  const page = {
    receipts: [
      {
        id: "one",
        external_event_id: "sync-1",
        lead_id: null,
        status: "unmatched",
        applied_fields: []
      }
    ],
    next_cursor: "next"
  }
  let url = ""
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    url = String(input)
    return Response.json(page)
  }) as typeof fetch
  expect(await fetchInboundPage("crm/one", "cursor+a&b")).toEqual(page)
  expect(url).toContain("crm%2Fone/inbound-receipts?cursor=cursor%2Ba%26b")
})
test("token rotation returns only a verified nonempty token", async () => {
  for (const payload of [null, {}, { token: 12 }, { token: "" }]) {
    globalThis.fetch = (async () => Response.json(payload)) as typeof fetch
    await expect(rotateInboundToken("crm")).rejects.toThrow("verified")
  }
  globalThis.fetch = (async () =>
    Response.json({ token: "synthetic-token" })) as typeof fetch
  expect(await rotateInboundToken("crm")).toBe("synthetic-token")
})
