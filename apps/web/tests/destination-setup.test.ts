import { afterEach, expect, test } from "bun:test"
import type { AudienceDestination, DestinationType } from "../src/lib/api"
import { RecordRequestError } from "../src/lib/records"
import type { AudienceActivationContext } from "../src/features/activation/destination-client"
import {
  SETUP_TYPES,
  SetupValidationError,
  createReviewedDestination,
  creationFingerprint,
  destinationInput,
  draftValues,
  initialDestinationDraft,
  sameDestination,
  validateSetupCatalog,
  type DestinationDraft
} from "../src/features/activation/destination-setup-client"

const originalFetch = globalThis.fetch
afterEach(() => {
  globalThis.fetch = originalFetch
})
const type: DestinationType = {
  id: "webhook",
  category: "activation",
  capabilities: ["outbound"],
  maturity: "beta",
  certification: null
}
function draft(
  kind: DestinationDraft["type"],
  values: Record<string, string> = {}
): DestinationDraft {
  return {
    ...initialDestinationDraft(),
    name: " Review target ",
    type: kind,
    configs: { [kind]: values }
  }
}
function saved(
  input = destinationInput(
    "aud",
    draft("webhook", { url: "https://example.test/hook" })
  )
): AudienceDestination {
  return {
    ...input,
    id: "destination",
    health_status: "unverified",
    last_error: null,
    last_success_at: null,
    created_at: "2026-10-29T09:00:00Z",
    updated_at: "2026-10-29T09:00:00Z",
    latest_run: null
  }
}
const context = {
  audience: {
    id: "aud",
    name: "People",
    filters: { _record_type: "person", record_ids: ["p1"] },
    member_count: 1,
    refreshed_at: "2026-10-29T09:00:00Z",
    refresh_enabled: true,
    refresh_interval_minutes: 60
  },
  destinations: []
} as unknown as AudienceActivationContext

test("every supported type builds an explicit enablement choice and its own configuration", () => {
  const required: Record<DestinationDraft["type"], Record<string, string>> = {
    webhook: { url: "https://example.test/hook" },
    warehouse_http: {
      url: "https://example.test/ingest",
      header_secret_ref: "warehouse"
    },
    hubspot: {},
    salesforce: {},
    instantly: { campaign_id: "campaign1" },
    smartlead: { campaign_id: "campaign2" },
    google_sheets: { spreadsheet_id: "sheet1" },
    airtable: { base_id: "base1", table: "People" },
    slack: { webhook_secret_ref: "slack" },
    meta_ads: { custom_audience_id: "123", consent_source: "Approved policy" },
    google_ads: {
      customer_id: "123",
      user_list_id: "456",
      consent_source: "Approved policy"
    },
    linkedin_ads: { segment_id: "urn.123", consent_source: "Approved policy" }
  }
  for (const kind of Object.keys(SETUP_TYPES) as DestinationDraft["type"][]) {
    const d = draft(kind, required[kind])
    d.enabled = false
    d.configs.webhook = { url: "https://example.test/unused" }
    if (kind === "webhook") d.configs.webhook = required.webhook
    const input = destinationInput("aud", d)
    expect(input.name).toBe("Review target")
    expect(input.enabled).toBe(false)
    expect(input.field_map).toEqual({})
    if (!["webhook", "warehouse_http"].includes(kind))
      expect(input.config.url).toBeUndefined()
  }
  expect(
    destinationInput("aud", draft("instantly", required.instantly)).config
      .skip_if_in_campaign
  ).toBe(true)
  expect(
    destinationInput("aud", draft("warehouse_http", required.warehouse_http))
      .config.mode
  ).toBe("snapshot")
  expect(
    destinationInput("aud", draft("airtable", required.airtable)).config
      .typecast
  ).toBe(true)
})

test("per-type drafts preserve their own values and defaults without leaking hidden fields", () => {
  const d = draft("slack", { webhook_secret_ref: "SLACK_REF" })
  d.configs.google_sheets = { spreadsheet_id: "sheet" }
  expect(draftValues(d).message_template).toContain("{company}")
  expect(destinationInput("aud", d).config).not.toHaveProperty("spreadsheet_id")
  d.type = "google_sheets"
  expect(draftValues(d).range).toBe("Sheet1!A:ZZ")
  expect(draftValues(d).spreadsheet_id).toBe("sheet")
  d.type = "slack"
  expect(draftValues(d).webhook_secret_ref).toBe("SLACK_REF")
})

test("validation reports field ownership and preserves Unicode names", () => {
  const d = draft("webhook")
  d.name = " "
  try {
    destinationInput("aud", d)
    throw new Error("expected validation")
  } catch (error) {
    expect(error).toBeInstanceOf(SetupValidationError)
    expect((error as SetupValidationError).fields).toHaveProperty("name")
    expect((error as SetupValidationError).fields).toHaveProperty("url")
  }
  d.configs.webhook = { url: "https://example.test" }
  d.name = "👩".repeat(200)
  expect(destinationInput("aud", d).name).toBe(d.name)
  d.name += "x"
  expect(() => destinationInput("aud", d)).toThrow(SetupValidationError)
})

test("endpoint validation rejects credential-bearing URLs and applies HTTPS to warehouse targets", () => {
  for (const url of [
    "javascript:alert(1)",
    "http:example.test",
    "file:///tmp/data",
    "https://user:password@example.test",
    "not a URL"
  ])
    expect(() => destinationInput("aud", draft("webhook", { url }))).toThrow(
      SetupValidationError
    )
  expect(
    destinationInput(
      "aud",
      draft("webhook", { url: "http://example.test/path?a=1" })
    ).config.method
  ).toBe("POST")
  expect(() =>
    destinationInput(
      "aud",
      draft("warehouse_http", {
        url: "http://example.test",
        header_secret_ref: "KEY"
      })
    )
  ).toThrow(SetupValidationError)
})

test("sheet output preserves field order, ignores empty comma segments and enforces actual bounds", () => {
  const d = draft("google_sheets", {
    spreadsheet_id: "sheet",
    columns: " email,company,,phone "
  })
  expect(destinationInput("aud", d).config.columns).toEqual([
    "email",
    "company",
    "phone"
  ])
  d.configs.google_sheets!.columns = Array.from(
    { length: 101 },
    (_, i) => "field" + i
  ).join(",")
  expect(() => destinationInput("aud", d)).toThrow(SetupValidationError)
  d.configs.google_sheets!.columns = ", ,"
  expect(() => destinationInput("aud", d)).toThrow(SetupValidationError)
})

test("advertising IDs reject path syntax and consent is never manufactured from a policy string", async () => {
  const d = draft("meta_ads", {
    custom_audience_id: "123",
    consent_source: "Policy"
  })
  const input = destinationInput("aud", d)
  expect(input.config.consent_attested).toBe(false)
  let calls = 0
  globalThis.fetch = (() => {
    calls++
    return Promise.resolve(Response.json({}))
  }) as typeof fetch
  await expect(createReviewedDestination(input)).rejects.toThrow(
    "Confirm advertising permission"
  )
  expect(calls).toBe(0)
  for (const id of ["../123", "..", "with/slash", "x".repeat(65)]) {
    d.configs.meta_ads!.custom_audience_id = id
    expect(() => destinationInput("aud", d)).toThrow(SetupValidationError)
  }
})

test("Slack templates retain literal text but reject unsupported expressions and oversized input", () => {
  const d = draft("slack", {
    webhook_secret_ref: "SLACK",
    message_template: "{{Member}}: {company} {email}"
  })
  expect(destinationInput("aud", d).config.message_template).toBe(
    "{{Member}}: {company} {email}"
  )
  for (const template of [
    "{company.__class__}",
    "{email!r}",
    "{company[0]}",
    "unterminated {",
    "x".repeat(2001)
  ]) {
    d.configs.slack!.message_template = template
    expect(() => destinationInput("aud", d)).toThrow(SetupValidationError)
  }
})

test("catalog validation never invents availability or supported certification", () => {
  expect(validateSetupCatalog({ types: [type] })).toEqual([type])
  for (const value of [
    {},
    { types: {} },
    { types: [type, type] },
    { types: [{ ...type, id: "unknown" }] },
    { types: [{ ...type, maturity: "supported" }] },
    { types: [{ ...type, capabilities: "outbound" }] }
  ])
    expect(() => validateSetupCatalog(value)).toThrow()
  const certificate = {
    validated_at: "2026-10-29T09:00:00Z",
    expires_at: "2026-11-29T09:00:00Z",
    build_sha: "sha",
    validation_run_id: "run",
    evidence_url: "https://example.test/evidence"
  }
  expect(
    validateSetupCatalog({
      types: [{ ...type, maturity: "supported", certification: certificate }]
    })[0]!.maturity
  ).toBe("supported")
})

test("creation review detects scope, target, schedule, membership and certification changes", () => {
  const input = destinationInput(
      "aud",
      draft("webhook", { url: "https://example.test/target" })
    ),
    fingerprint = creationFingerprint(context, type, input)
  for (const change of [
    { member_count: 2 },
    { refresh_enabled: false },
    { refresh_interval_minutes: 15 },
    { filters: { _record_type: "company" } },
    { name: "Renamed audience" }
  ])
    expect(
      creationFingerprint(
        { ...context, audience: { ...context.audience, ...change } },
        type,
        input
      )
    ).not.toBe(fingerprint)
  expect(
    creationFingerprint(
      context,
      { ...type, capabilities: ["outbound", "inbound"] },
      input
    )
  ).not.toBe(fingerprint)
  expect(
    creationFingerprint(context, type, {
      ...input,
      config: { url: "https://example.test/other" }
    })
  ).not.toBe(fingerprint)
})

test("saved-state recovery verifies exact configuration and scope, including array order", () => {
  const input = destinationInput(
      "aud",
      draft("google_sheets", { spreadsheet_id: "sheet" })
    ),
    value = saved(input)
  expect(sameDestination(value, input)).toBe(true)
  expect(sameDestination({ ...value, audience_id: "other" }, input)).toBe(false)
  expect(sameDestination({ ...value, enabled: !input.enabled }, input)).toBe(
    false
  )
  expect(
    sameDestination(
      {
        ...value,
        config: { ...value.config, columns: ["website", "company"] }
      },
      input
    )
  ).toBe(false)
})

test("creation sends one explicit request and validates its receipt before confirming success", async () => {
  const input = destinationInput(
      "aud",
      draft("webhook", { url: "https://example.test/hook" })
    ),
    calls: unknown[] = []
  globalThis.fetch = ((_url, init) => {
    calls.push(JSON.parse(String(init?.body)))
    return Promise.resolve(Response.json(saved(input)))
  }) as typeof fetch
  expect((await createReviewedDestination(input)).id).toBe("destination")
  expect(calls).toEqual([input])
  for (const change of [
    { audience_id: "other" },
    { name: "Wrong name" },
    { enabled: false },
    { config: { url: "https://example.test/other" } },
    { id: "" }
  ]) {
    globalThis.fetch = (() =>
      Promise.resolve(
        Response.json({ ...saved(input), ...change })
      )) as typeof fetch
    await expect(createReviewedDestination(input)).rejects.toThrow()
  }
})

test("uncertain server failures propagate without a hidden second POST", async () => {
  let calls = 0
  globalThis.fetch = (() => {
    calls++
    return Promise.resolve(
      Response.json({ detail: "Synthetic failure" }, { status: 503 })
    )
  }) as typeof fetch
  await expect(
    createReviewedDestination(
      destinationInput("aud", draft("webhook", { url: "https://example.test" }))
    )
  ).rejects.toBeInstanceOf(RecordRequestError)
  expect(calls).toBe(1)
})
