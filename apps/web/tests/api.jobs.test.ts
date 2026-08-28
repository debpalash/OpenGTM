import { afterEach, describe, expect, test } from "bun:test"

import { fetchJobs } from "../src/lib/api"

const originalFetch = globalThis.fetch

afterEach(() => {
  globalThis.fetch = originalFetch
})

describe("fetchJobs", () => {
  test("rejects an API error payload instead of returning it as a job list", async () => {
    globalThis.fetch = (async () =>
      new Response(JSON.stringify({ detail: "Not authenticated" }), {
        status: 401,
        headers: { "Content-Type": "application/json" },
      })) as typeof fetch

    await expect(fetchJobs()).rejects.toThrow("Jobs request failed (401): Not authenticated")
  })

  test("rejects a successful response whose payload is not an array", async () => {
    globalThis.fetch = (async () =>
      new Response(JSON.stringify({ jobs: [] }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      })) as typeof fetch

    await expect(fetchJobs()).rejects.toThrow("Jobs response must be an array")
  })
})
