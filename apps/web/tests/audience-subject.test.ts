import { expect, test } from "bun:test"
import {
  audienceSubject,
  audienceScope
} from "../src/features/activation/audience-subject"
test("company-only audience members open Companies without a synthetic Lead", () =>
  expect(
    audienceSubject({
      lead_id: null,
      company_id: "acme",
      snapshot: { company: "Acme", website: "https://acme.test" }
    })
  ).toEqual({
    kind: "Company",
    name: "Acme",
    detail: "https://acme.test",
    href: "/companies?record=acme"
  }))
test("people retain their identity independently of the company and legacy Lead", () =>
  expect(
    audienceSubject({
      lead_id: 13,
      company_id: "acme",
      person_id: "ana",
      snapshot: {
        contact_person: "Ana",
        company: "Acme",
        contact_title: "Engineer"
      }
    })
  ).toEqual({
    kind: "Person",
    name: "Ana",
    detail: "Acme · Engineer",
    href: "/people?record=ana"
  }))
test("membership activity can resolve canonical snapshot identity", () =>
  expect(
    audienceSubject({
      lead_id: null,
      snapshot: { record_type: "person", id: "ana", full_name: "Ana" }
    }).href
  ).toBe("/people?record=ana"))
test("legacy Lead links remain available", () =>
  expect(
    audienceSubject({ lead_id: 7, snapshot: { company: "Legacy" } }).href
  ).toBe("/leads/7"))
test("missing references do not fabricate a Lead link", () =>
  expect(
    audienceSubject({ lead_id: null, snapshot: { company: "Unknown" } }).href
  ).toBeNull())
test("audience entity scope is explicit", () => {
  expect(audienceScope({ _record_type: "company" })).toBe("Companies")
  expect(audienceScope({ _record_type: "person" })).toBe("People")
  expect(audienceScope({ city: "Paris" })).toBe("Legacy leads")
})
