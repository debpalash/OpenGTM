import { expect, test } from "bun:test"
import {
  createRecordDraft,
  addDraftField,
  isRecordDraftDirty,
  recordDraftWrite,
  recordConflictRows,
  resolveRecordConflict
} from "../src/features/records/record-draft"
import type { SharedRecord } from "../src/lib/records"

const company = (
  fields: Record<string, unknown>,
  version = 1
): SharedRecord => ({
  id: "company-a",
  record_type: "company",
  version,
  fields,
  company_id: "company-a",
  person_id: null,
  employment_id: null
})
const person = (
  companyId: string | null,
  title: string,
  version = 1
): SharedRecord => ({
  id: "person-a",
  record_type: "person",
  version,
  fields: { full_name: "Ana" },
  person_id: "person-a",
  company_id: companyId,
  employment_id: title ? 1 : null,
  company: companyId ? { name: companyId } : undefined,
  employments: title
    ? [
        {
          id: 1,
          company_id: companyId,
          company_name: companyId ?? "",
          fields: { title },
          current: true,
          first_observed_at: "2026-01-01",
          last_observed_at: "2026-01-01"
        }
      ]
    : []
})

test("typed additions and falsy values survive the write; untouched large IDs are omitted", () => {
  const base = company({ name: "Atlas", large_id: "9007199254740993" })
  let draft = createRecordDraft("company", base)
  expect(isRecordDraftDirty(draft, base)).toBe(false)
  draft = addDraftField(draft, "seats", "number")
  draft = addDraftField(draft, "enabled", "boolean")
  draft = addDraftField(draft, "details", "json")
  draft = addDraftField(draft, "empty_note", "text")
  Object.assign(draft.fields, {
    seats: "0",
    enabled: "false",
    details: '{"labels":["a"],"ready":false}'
  })
  expect(recordDraftWrite("company", draft, base)).toEqual({
    company: {
      seats: 0,
      enabled: false,
      details: { labels: ["a"], ready: false },
      empty_note: ""
    }
  })
  expect(isRecordDraftDirty(draft, base)).toBe(true)
})

test("bad numeric/JSON edits are rejected without rounding or inventing null", () => {
  const base = company({ name: "Atlas", seats: 2, details: { id: "a" } })
  for (const value of ["Infinity", "NaN", "9007199254740993"]) {
    const draft = createRecordDraft("company", base)
    draft.fields.seats = value
    expect(() => recordDraftWrite("company", draft, base)).toThrow("safe range")
  }
  for (const value of ['{"broken"', '{"id":9007199254740993}', '{"x":1e500}']) {
    const draft = createRecordDraft("company", base)
    draft.fields.details = value
    expect(() => recordDraftWrite("company", draft, base)).toThrow("valid JSON")
  }
  const draft = createRecordDraft("company", base)
  draft.fields.seats = ""
  draft.fields.details = ""
  expect(recordDraftWrite("company", draft, base)).toEqual({
    company: { seats: null, details: null }
  })
})

test("duplicate and unsafe custom names are rejected and a new blank field is dirty", () => {
  const base = company({ name: "Atlas" })
  const draft = createRecordDraft("company", base)
  expect(() => addDraftField(draft, "name", "text")).toThrow("already exists")
  expect(() => addDraftField(draft, "__proto__", "json")).toThrow("field name")
  expect(isRecordDraftDirty(addDraftField(draft, "note", "text"), base)).toBe(
    true
  )
})

test("review retains unrelated remote updates and requires an explicit choice for overlaps", () => {
  const base = company({
    name: "Atlas",
    city: "Pune",
    industry: "Software",
    flags: { a: 1, b: 2 }
  })
  const draft = createRecordDraft("company", base)
  draft.fields.city = "Mumbai"
  draft.fields.industry = "AI"
  const latest = company(
    {
      name: "Atlas Research",
      city: "Delhi",
      industry: "Software",
      flags: { b: 2, a: 1 },
      remote_note: "Retain me"
    },
    2
  )
  const rows = recordConflictRows("company", base, draft, latest)
  expect(rows.find((row) => row.key === "city")?.needsChoice).toBe(true)
  expect(rows.some((row) => row.key === "flags")).toBe(false)
  expect(() =>
    resolveRecordConflict("company", base, draft, latest, {})
  ).toThrow("Choose a value")
  const retained = resolveRecordConflict("company", base, draft, latest, {
    city: "draft"
  })
  expect(recordDraftWrite("company", retained, latest)).toEqual({
    company: { city: "Mumbai", industry: "AI" }
  })
  expect(retained.fields.name).toBe("Atlas Research")
  expect(retained.fields.remote_note).toBe("Retain me")
  const current = resolveRecordConflict("company", base, draft, latest, {
    city: "current"
  })
  expect(recordDraftWrite("company", current, latest)).toEqual({
    company: { industry: "AI" }
  })
})

test("concurrent identical values need no overwrite decision", () => {
  const base = company({ name: "Atlas", city: "Pune" })
  const draft = createRecordDraft("company", base)
  draft.fields.city = "Mumbai"
  const latest = company({ name: "Atlas", city: "Mumbai" }, 2)
  expect(
    recordConflictRows("company", base, draft, latest)[0].needsChoice
  ).toBe(false)
  expect(
    isRecordDraftDirty(
      resolveRecordConflict("company", base, draft, latest, {}),
      latest
    )
  ).toBe(false)
})

test("employment ending is explicit; a role can be created while the employer is unknown", () => {
  const unknown = person(null, "")
  const draft = createRecordDraft("person", unknown)
  draft.title = "Founder"
  expect(recordDraftWrite("person", draft, unknown)).toEqual({
    person: {},
    employment: { title: "Founder" }
  })
  const known = person("company-a", "Engineer")
  const edit = createRecordDraft("person", known)
  edit.companyId = ""
  expect(() => recordDraftWrite("person", edit, known)).toThrow(
    "End current employment"
  )
  edit.endEmployment = true
  edit.title = "Unsaved alternate role"
  expect(recordDraftWrite("person", edit, known)).toEqual({
    person: {},
    end_employment: true
  })
})

test("role and employer conflict together so a role is not silently moved to the wrong company", () => {
  const base = person("company-a", "Engineer")
  const draft = createRecordDraft("person", base)
  draft.title = "Director"
  const latest = person("company-b", "Founder", 2)
  expect(
    recordConflictRows("person", base, draft, latest).find(
      (row) => row.key === "$employment"
    )?.needsChoice
  ).toBe(true)
  const resolved = resolveRecordConflict("person", base, draft, latest, {
    $employment: "current"
  })
  expect(recordDraftWrite("person", resolved, latest)).toEqual({ person: {} })
})

test("a staged custom field remains part of the protected draft until added or cleared", () => {
  const base = company({ name: "Atlas" })
  const draft = createRecordDraft("company", base)
  draft.pendingField = { name: "Unfinished field", type: "json" }
  expect(isRecordDraftDirty(draft, base)).toBe(true)
  expect(() => recordDraftWrite("company", draft, base)).toThrow("Add or clear")
  const added = addDraftField(
    draft,
    draft.pendingField.name,
    draft.pendingField.type
  )
  expect(added.pendingField.name).toBe("")
  expect(recordDraftWrite("company", added, base)).toEqual({
    company: { "Unfinished field": {} }
  })
})
