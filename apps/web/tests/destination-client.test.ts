import { afterEach, expect, test } from "bun:test"
import {
  actionUnavailable,
  destinationTarget,
  fetchActivationAccess,
  fetchDeliveryPage,
  fetchRunPage,
  performDestinationAction,
  reviewFingerprint,
  validateDestinationList,
  validateRun,
  audienceReviewFingerprint,
  fetchAudienceActivationContext,
  performAudienceChange,
  type DestinationContext
} from "../src/features/activation/destination-client"

const originalFetch = globalThis.fetch
afterEach(() => {
  globalThis.fetch = originalFetch
})
const run = {
  id: "run-one",
  destination_id: "dest",
  status: "pending",
  attempted: 0,
  succeeded: 0,
  failed: 0,
  skipped: 0,
  error: null,
  started_at: null,
  finished_at: null,
  created_at: "2026-10-29T09:00:00"
}
const context = {
  destination: {
    id: "dest",
    audience_id: "aud",
    name: "CRM",
    destination_type: "webhook",
    enabled: true,
    config: {
      url: "https://user:secret@crm.test/private-token?api_key=secret"
    },
    field_map: { email: "email" },
    health_status: "unverified",
    updated_at: "2026-10-29T09:00:00",
    latest_run: null
  },
  audience: {
    id: "aud",
    name: "Audience",
    description: "",
    created_at: "2026-10-29T08:00:00",
    refresh_health: "healthy",
    last_refresh_error: null,
    consecutive_refresh_failures: 0,
    next_refresh_at: null,
    filters: { _record_type: "person" },
    member_count: 50,
    refreshed_at: "2026-10-29T08:00:00",
    updated_at: "2026-10-29T09:00:00",
    refresh_enabled: true,
    refresh_interval_minutes: 60
  }
} as DestinationContext

test("run receipts require matching identities, known states and usable counters/timestamps", () => {
  expect(validateRun(run, "dest")).toEqual(run)
  for (const patch of [
    { id: "" },
    { destination_id: "other" },
    { status: "delivered" },
    { succeeded: -1 },
    { failed: 9007199254740992 },
    { created_at: "invalid" }
  ])
    expect(() => validateRun({ ...run, ...patch }, "dest")).toThrow("verified")
  expect(() => validateRun(run, "dest", "other-run")).toThrow("verified")
})
test("review fingerprints detect changed audience/configuration while tolerating cancellation progress", () => {
  const base = reviewFingerprint(context, "sync")
  expect(
    reviewFingerprint(
      { ...context, audience: { ...context.audience, member_count: 51 } },
      "sync"
    )
  ).not.toBe(base)
  expect(
    reviewFingerprint(
      {
        ...context,
        destination: {
          ...context.destination,
          config: { url: "https://other.test" }
        }
      },
      "sync"
    )
  ).not.toBe(base)
  const running = {
    ...context,
    destination: {
      ...context.destination,
      latest_run: { ...run, status: "running" }
    }
  }
  expect(
    reviewFingerprint(
      {
        ...running,
        destination: {
          ...running.destination,
          latest_run: {
            ...running.destination.latest_run,
            succeeded: 1,
            attempted: 1
          }
        }
      },
      "cancel"
    )
  ).toBe(reviewFingerprint(running, "cancel"))
  expect(
    reviewFingerprint(
      {
        ...running,
        destination: {
          ...running.destination,
          latest_run: { ...run, status: "completed" }
        }
      },
      "cancel"
    )
  ).not.toBe(reviewFingerprint(running, "cancel"))
})
test("disabled/active/changed runs cannot silently acquire a different action", () => {
  expect(actionUnavailable(context, "sync")).toBeNull()
  expect(
    actionUnavailable(
      { ...context, destination: { ...context.destination, enabled: false } },
      "sync"
    )
  ).toContain("disabled")
  const active = {
    ...context,
    destination: { ...context.destination, latest_run: run }
  }
  expect(actionUnavailable(active, "sync")).toContain("already active")
  expect(actionUnavailable(active, "retry", run.id)).toContain(
    "no longer eligible"
  )
  expect(actionUnavailable(active, "cancel", "other")).toContain("changed")
  expect(actionUnavailable(active, "cancel", run.id)).toBeNull()
})
test("target hints never expose embedded URL credentials, query values or secret paths", () => {
  expect(destinationTarget(context.destination)).toBe("https://crm.test")
})
test("effective access and destination collections reject malformed or mixed-workspace data", async () => {
  globalThis.fetch = (async () =>
    Response.json({ can_write: "true" })) as typeof fetch
  await expect(fetchActivationAccess()).rejects.toThrow("verified")
  expect(() =>
    validateDestinationList(
      [{ ...context.destination, audience_id: "other" }],
      "aud"
    )
  ).toThrow("verified")
  expect(() =>
    validateDestinationList([context.destination, context.destination], "aud")
  ).toThrow("verified")
})
test("history pages cannot invent empty success or loop on a reused cursor", async () => {
  globalThis.fetch = (async () =>
    new Response("null", { status: 503 })) as typeof fetch
  await expect(fetchRunPage("dest", null)).rejects.toThrow()
  globalThis.fetch = (async () =>
    Response.json({
      runs: [run],
      limit: 20,
      has_more: true,
      next_cursor: "same"
    })) as typeof fetch
  await expect(fetchRunPage("dest", "same")).rejects.toThrow("verified")
  globalThis.fetch = (async () =>
    Response.json({
      runs: [],
      limit: 20,
      has_more: false,
      next_cursor: null
    })) as typeof fetch
  expect((await fetchRunPage("dest", null)).runs).toEqual([])
})
test("mutations validate the response scope and encode opaque run IDs", async () => {
  let url = "",
    method = ""
  globalThis.fetch = (async (input, init) => {
    url = String(input)
    method = init?.method || ""
    return Response.json({ ...run, id: "run/one" })
  }) as typeof fetch
  expect((await performDestinationAction("retry", "dest", "run/one")).id).toBe(
    "run/one"
  )
  expect(url).toContain("runs/run%2Fone/retry")
  expect(method).toBe("POST")
  await expect(
    performDestinationAction("retry", "dest", "other")
  ).rejects.toThrow("verified")
})
test("delivery pages preserve canonical identity and reject scope/order/legacy-zero errors", async () => {
  const receipt = {
    id: 1,
    run_id: run.id,
    destination_id: "dest",
    lead_id: null,
    company_id: "company",
    operation: "upsert",
    status: "success",
    attempts: 1,
    external_id: null,
    summary: null,
    error: null,
    delivered_at: null,
    created_at: run.created_at
  }
  globalThis.fetch = (async () => Response.json([receipt])) as typeof fetch
  expect(
    (await fetchDeliveryPage("dest", run.id, null, ""))[0].company_id
  ).toBe("company")
  for (const patch of [
    { lead_id: 0 },
    { run_id: "other" },
    { destination_id: "other" },
    { id: 0 }
  ]) {
    globalThis.fetch = (async () =>
      Response.json([{ ...receipt, ...patch }])) as typeof fetch
    await expect(fetchDeliveryPage("dest", run.id, null, "")).rejects.toThrow(
      "verified"
    )
  }
})

test("refresh review includes enabled destinations and field-map order", async () => {
  const scope = {
    audience: context.audience,
    destinations: [context.destination]
  }
  const original = audienceReviewFingerprint(scope)
  expect(
    audienceReviewFingerprint({
      ...scope,
      destinations: [context.destination, { ...context.destination, id: "new" }]
    })
  ).not.toBe(original)
  const first = {
    ...context,
    destination: {
      ...context.destination,
      field_map: { email: "target", full_name: "target" }
    }
  }
  const reordered = {
    ...context,
    destination: {
      ...context.destination,
      field_map: { full_name: "target", email: "target" }
    }
  }
  expect(reviewFingerprint(first, "sync")).not.toBe(
    reviewFingerprint(reordered, "sync")
  )
  globalThis.fetch = (async (input) =>
    Response.json(
      String(input).includes("audience-destinations?")
        ? [context.destination]
        : [context.audience]
    )) as typeof fetch
  expect((await fetchAudienceActivationContext("aud")).destinations[0].id).toBe(
    "dest"
  )
})

test("membership receipts reject malformed counts and another audience", async () => {
  for (const payload of [
    null,
    {
      audience: context.audience,
      entered: -1,
      exited: 0,
      changed: 0,
      unchanged: 50
    },
    {
      audience: { ...context.audience, id: "other" },
      entered: 0,
      exited: 0,
      changed: 0,
      unchanged: 50
    }
  ]) {
    globalThis.fetch = (async () => Response.json(payload)) as typeof fetch
    await expect(
      performAudienceChange("aud", { kind: "refresh" })
    ).rejects.toThrow("verified")
  }
  globalThis.fetch = (async () =>
    Response.json({
      audience: context.audience,
      entered: 0,
      exited: 0,
      changed: 0,
      unchanged: 50
    })) as typeof fetch
  const result = await performAudienceChange("aud", { kind: "refresh" })
  expect(result.kind).toBe("refresh")
  if (result.kind === "refresh") expect(result.receipt.unchanged).toBe(50)
})

test("a schedule write must confirm the requested values", async () => {
  globalThis.fetch = (async () =>
    Response.json(context.audience)) as typeof fetch
  await expect(
    performAudienceChange("aud", {
      kind: "schedule",
      refresh_interval_minutes: 15
    })
  ).rejects.toThrow("verified")
  globalThis.fetch = (async () =>
    Response.json({
      ...context.audience,
      refresh_interval_minutes: 15
    })) as typeof fetch
  const result = await performAudienceChange("aud", {
    kind: "schedule",
    refresh_interval_minutes: 15
  })
  expect(result.kind).toBe("schedule")
})
