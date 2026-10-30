import { afterEach, expect, test } from "bun:test"
import {
  validateSharedRecord,
  recordAPI,
  RecordRequestError,
  fetchRecordPage
} from "../src/lib/records"

const originalFetch = globalThis.fetch
afterEach(() => {
  globalThis.fetch = originalFetch
})
const record = {
  id: "a",
  record_type: "company",
  version: 1,
  fields: { name: "Atlas" }
}
test("record identity, versions and nested relationship/source metadata must be usable", () => {
  expect(validateSharedRecord(record, "company", "a")).toEqual(record)
  for (const patch of [
    { id: "other" },
    { version: 0 },
    { fields: [] },
    { employments: "unexpected" },
    { field_owners: { name: { source: 23 } } },
    { external_ids: [{}] }
  ]) {
    expect(() =>
      validateSharedRecord({ ...record, ...patch }, "company", "a")
    ).toThrow("verified")
  }
})
test("HTTP conflicts retain their status even with missing or malformed detail", async () => {
  globalThis.fetch = (async () =>
    new Response("null", { status: 409 })) as typeof fetch
  try {
    await recordAPI("records/company/a", {}, "PATCH")
    throw new Error("Expected conflict")
  } catch (error) {
    expect(error).toBeInstanceOf(RecordRequestError)
    expect((error as RecordRequestError).status).toBe(409)
  }
})
test("invalid collection payloads cannot masquerade as empty collections", async () => {
  globalThis.fetch = (async () =>
    Response.json({
      records: [],
      total: 2,
      limit: 0,
      offset: 0
    })) as typeof fetch
  await expect(fetchRecordPage("company", { limit: 20 })).rejects.toThrow(
    "could not be verified"
  )
  globalThis.fetch = (async () =>
    Response.json({
      records: [],
      total: 0,
      limit: 20,
      offset: 0
    })) as typeof fetch
  expect((await fetchRecordPage("company", { limit: 20 })).total).toBe(0)
})
