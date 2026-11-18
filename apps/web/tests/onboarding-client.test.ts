import { afterEach, expect, test } from "bun:test"
import {
  OnboardingError,
  fetchOnboarding,
  loadSampleWorkbook,
  validateOnboardingState,
  validateSampleResult
} from "../src/features/onboarding/onboarding-client"

const originalFetch = globalThis.fetch
afterEach(() => {
  globalThis.fetch = originalFetch
})

const state = {
  steps: [
    { id: "llm_provider", done: true },
    { id: "data_source", done: true },
    { id: "data", done: false },
    { id: "enrichment", done: false }
  ],
  done_count: 2,
  total: 4,
  complete: false,
  sample_workbook_id: null
}

const reply = (status: number, body: unknown) =>
  (globalThis.fetch = (async () =>
    new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } })) as typeof fetch)

test("a valid checklist state passes validation unchanged", () => {
  expect(validateOnboardingState(state)).toEqual(state)
})

test("malformed checklist states are rejected", () => {
  for (const bad of [
    null,
    {},
    { ...state, steps: "x" },
    { ...state, done_count: 3 },
    { ...state, total: 5 },
    { ...state, complete: "no" },
    { ...state, sample_workbook_id: 7 },
    { ...state, steps: [...state.steps.slice(0, 3), { id: "unknown", done: false }] },
    { ...state, steps: [state.steps[0], state.steps[0], state.steps[2], state.steps[3]] },
    { ...state, steps: [{ id: "llm_provider", done: "yes" }, ...state.steps.slice(1)] }
  ]) {
    expect(() => validateOnboardingState(bad)).toThrow("could not be verified")
  }
})

test("fetchOnboarding reads GET /api/onboarding", async () => {
  let seen = ""
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    seen = String(input)
    return new Response(JSON.stringify(state), { status: 200 })
  }) as typeof fetch
  expect(await fetchOnboarding()).toEqual(state)
  expect(seen).toBe("/api/onboarding")
})

test("fetchOnboarding reports the status of a failed read", async () => {
  reply(500, { detail: "boom" })
  const error = await fetchOnboarding().catch((e: unknown) => e)
  expect(error).toBeInstanceOf(OnboardingError)
  expect((error as OnboardingError).status).toBe(500)
  expect((error as OnboardingError).message).toContain("500")
})

test("sample result validation", () => {
  expect(validateSampleResult({ workbook_id: "sample-1", name: "Sample", created: true }).workbook_id).toBe("sample-1")
  for (const bad of [null, {}, { workbook_id: "", name: "x", created: true }, { workbook_id: "a", name: "x", created: 1 }]) {
    expect(() => validateSampleResult(bad)).toThrow("could not be verified")
  }
})

test("loadSampleWorkbook POSTs and returns the workbook (created or existing)", async () => {
  let method = ""
  let url = ""
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    url = String(input)
    method = init?.method ?? "GET"
    return new Response(JSON.stringify({ workbook_id: "sample-1", name: "Sample", created: false }), { status: 200 })
  }) as typeof fetch
  expect(await loadSampleWorkbook()).toEqual({ workbook_id: "sample-1", name: "Sample", created: false })
  expect([method, url]).toEqual(["POST", "/api/onboarding/sample"])
})

test("loadSampleWorkbook explains a viewer's refusal and a rate limit", async () => {
  reply(403, { detail: "Insufficient permissions" })
  const refused = (await loadSampleWorkbook().catch((e: unknown) => e)) as OnboardingError
  expect(refused.status).toBe(403)
  expect(refused.message).toContain("editors and admins")
  reply(429, { detail: "rate limited" })
  const limited = (await loadSampleWorkbook().catch((e: unknown) => e)) as OnboardingError
  expect(limited.status).toBe(429)
  reply(500, {})
  const failed = (await loadSampleWorkbook().catch((e: unknown) => e)) as OnboardingError
  expect(failed.message).toContain("500")
})
