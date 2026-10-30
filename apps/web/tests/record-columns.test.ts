import { expect, test } from "bun:test"
import {
  columnPreferenceKey,
  defaultColumnLayout,
  encodeColumnLayout,
  moveColumn,
  parseColumnLayout,
  visibleColumns
} from "../src/features/records/collection/columns"
import {
  coreFacts,
  currentCompany,
  currentRole,
  emailState,
  factText,
  recordLocation,
  safeExternalURL
} from "../src/features/records/collection/facts"
import type { SharedRecord } from "../src/lib/records"

const company: SharedRecord = {
  id: "company-a",
  record_type: "company",
  company_id: "company-a",
  person_id: null,
  employment_id: null,
  version: 1,
  fields: {
    name: "Fixture",
    domain: "fixture.test",
    industry: "Software",
    city: "Paris",
    description: "Testing",
    employee_count_exact: 0
  }
}
const person: SharedRecord = {
  ...company,
  id: "person-a",
  record_type: "person",
  person_id: "person-a",
  company_id: null,
  fields: { full_name: "Fixture Person", email: "fixture@example.test" }
}

test("company and people default columns match their workbench contracts", () => {
  expect(
    visibleColumns("company", defaultColumnLayout("company")).map(
      (column) => column.id
    )
  ).toEqual([
    "identity",
    "domain",
    "industry",
    "fit",
    "location",
    "completeness",
    "updated"
  ])
  expect(
    visibleColumns("person", defaultColumnLayout("person")).map(
      (column) => column.id
    )
  ).toEqual(["identity", "company_role", "email", "fit", "updated"])
})

test("stored layouts cannot hide or displace identity or introduce foreign columns", () => {
  const layout = parseColumnLayout(
    "company",
    JSON.stringify({
      version: 1,
      kind: "company",
      order: ["updated", "unknown", "identity", "domain", "domain"],
      hidden: ["identity", "unknown", "domain", "domain"]
    })
  )
  expect(layout.order[0]).toBe("identity")
  expect(layout.order.filter((id) => id === "domain")).toHaveLength(1)
  expect(layout.order).not.toContain("unknown")
  expect(layout.hidden).not.toContain("identity")
  expect(visibleColumns("company", layout).map((column) => column.id)).toEqual([
    "identity",
    "updated",
    "industry",
    "fit",
    "location",
    "completeness"
  ])
})

test("malformed, oversized, future-version and wrong-entity preferences recover defaults", () => {
  for (const raw of [
    null,
    "{",
    "x".repeat(16385),
    JSON.stringify({ version: 2 }),
    JSON.stringify({ version: 1, kind: "person", order: [], hidden: [] }),
    JSON.stringify({ version: 1, kind: "company", order: [null], hidden: [] })
  ]) {
    expect(parseColumnLayout("company", raw)).toEqual(
      defaultColumnLayout("company")
    )
  }
})

test("ordering preserves hidden choices, clamps moves and never mutates a saved layout", () => {
  const original = defaultColumnLayout("company")
  const encoded = encodeColumnLayout("company", original)
  const moved = moveColumn(original, "industry", -1)
  expect(
    visibleColumns("company", moved)
      .map((column) => column.id)
      .slice(0, 3)
  ).toEqual(["identity", "industry", "domain"])
  expect(moved.hidden).toEqual(original.hidden)
  expect(encodeColumnLayout("company", original)).toBe(encoded)
  expect(moveColumn(original, "identity", 1)).toBe(original)
  expect(moveColumn(original, "domain", -1)).toBe(original)
  expect(moveColumn(original, "unknown", 1)).toBe(original)
  expect(parseColumnLayout("company", encoded)).toEqual(original)
})

test("preference keys separate users, workspaces and record types without delimiter collisions", () => {
  const keys = [
    columnPreferenceKey(1, "a:b", "company"),
    columnPreferenceKey(2, "a:b", "company"),
    columnPreferenceKey(1, "a:b", "person"),
    columnPreferenceKey(1, "a%3Ab", "company")
  ]
  expect(new Set(keys).size).toBe(4)
})

test("completeness measures exactly six accepted core facts and counts numeric zero", () => {
  expect(coreFacts(company)).toHaveLength(6)
  expect(coreFacts(company).every((fact) => fact.present)).toBe(true)
  const sparse = {
    ...company,
    fields: {
      name: "Sparse",
      domain: "  n/A ",
      industry: [],
      city: " ",
      description: {},
      employee_count_exact: null,
      ignored_custom_data: "not a core fact"
    },
    field_owners: {
      domain: { source: "source-alternative", updated_at: "2026-10-29" }
    }
  }
  expect(
    coreFacts(sparse)
      .filter((fact) => fact.present)
      .map((fact) => fact.label)
  ).toEqual(["Company name"])
  expect(factText(Number.NaN)).toBe("")
  expect(factText(false)).toBe("")
  expect(factText(["Software", "", "n/a", "Services"])).toBe(
    "Software, Services"
  )
})

test("person coverage uses current employment without borrowing former employers or company contact facts", () => {
  const former = {
    id: 1,
    company_id: "former",
    company_name: "Old employer",
    fields: { title: "Former role" },
    current: false,
    first_observed_at: "2025-01-01",
    last_observed_at: "2025-02-01"
  }
  const current = {
    ...former,
    id: 2,
    company_id: null,
    company_name: "",
    fields: { title: "Research lead" },
    current: true
  }
  const value = {
    ...person,
    company: { phone: "company-owned-phone" },
    employments: [former, current]
  }
  expect(currentRole(value)).toBe("Research lead")
  expect(currentCompany(value)).toEqual({ id: null, name: "" })
  expect(
    coreFacts(value)
      .filter((fact) => fact.present)
      .map((fact) => fact.label)
  ).toEqual(["Full name", "Current role", "Work email"])
  expect(coreFacts(person).filter((fact) => fact.present)).toHaveLength(2)
})

test("email verification reads accepted scalar or structured status and requires an email", () => {
  expect(
    emailState({
      ...person,
      fields: { email: "a@example.test", email_verify: { status: "verified" } }
    })
  ).toBe("verified")
  expect(
    emailState({
      ...person,
      fields: { email: "a@example.test", email_verify_status: "DELIVERABLE" }
    })
  ).toBe("verified")
  expect(
    emailState({
      ...person,
      fields: { email: "n/a", email_verify_status: "valid" }
    })
  ).toBe("missing")
  expect(
    emailState({
      ...person,
      fields: { email: "a@example.test", email_verify: { status: "invalid" } }
    })
  ).toBe("invalid")
  expect(
    emailState({
      ...person,
      fields: { email: "a@example.test", email_verify_status: "catch_all" }
    })
  ).toBe("risky")
  expect(emailState(person)).toBe("unverified")
})

test("location has useful accepted regional context and an address fallback", () => {
  expect(
    recordLocation({
      ...company,
      fields: { city: "Paris", state: "Île-de-France", country: "France" }
    })
  ).toBe("Paris, Île-de-France, France")
  expect(
    recordLocation({ ...company, fields: { full_address: "1 Fixture Street" } })
  ).toBe("1 Fixture Street")
})

test("external profile links reject script, data, credential and relative URLs", () => {
  for (const value of [
    "javascript:alert(1)",
    "data:text/html,test",
    "/relative",
    "https://user:password@example.test/profile",
    "https:example.test"
  ])
    expect(safeExternalURL(value)).toBeNull()
  expect(safeExternalURL("https://www.linkedin.com/in/fixture")).toBe(
    "https://www.linkedin.com/in/fixture"
  )
})

test("a former employment or an old person title is never presented as the current role", () => {
  const record = {
    ...person,
    fields: { ...person.fields, contact_title: "Former title" },
    employments: [
      {
        id: 1,
        company_id: "former",
        company_name: "Old employer",
        current: false,
        fields: { title: "Former title" },
        first_observed_at: "2025-01-01",
        last_observed_at: "2025-02-01"
      }
    ]
  }
  expect(currentRole(record)).toBe("")
  expect(
    coreFacts(record).find((fact) => fact.label === "Current role")?.present
  ).toBe(false)
})
