import { describe, expect, test } from "bun:test"
import {
  buildInputs, enumOptionValue, FREEFORM_FIELD, humanize, initialValues, schemaToForm,
} from "../src/lib/json-schema-form"
import type { JsonSchema } from "../src/lib/plugin-api"

// The Go host expands the manifest shorthand {domain: string, max_pages: "integer?"} to this.
const shorthand: JsonSchema = {
  type: "object",
  properties: { domain: { type: "string" }, max_pages: { type: "integer" }, verbose: { type: "boolean" } },
  required: ["domain"],
}

describe("schemaToForm", () => {
  test("maps scalar types and required flags, required fields first", () => {
    const form = schemaToForm({ ...shorthand, properties: { max_pages: { type: "integer" }, ...shorthand.properties } })
    expect(form.freeform).toBe(false)
    expect(form.fields.map(f => [f.name, f.kind, f.required])).toEqual([
      ["domain", "string", true], ["max_pages", "integer", false], ["verbose", "boolean", false],
    ])
    expect(form.fields[0].label).toBe("Domain")
    expect(form.fields[1].label).toBe("Max pages")
  })

  test("keeps titles, descriptions, bounds and nullable types", () => {
    const form = schemaToForm({
      type: "object",
      properties: {
        score: { type: ["number", "null"], minimum: 0, maximum: 1, title: "Minimum score", description: "0 to 1" },
        handle: { type: "string", pattern: "^@", maxLength: 16, examples: ["@acme"] },
      },
    })
    expect(form.fields[0]).toMatchObject({ kind: "number", label: "Minimum score", description: "0 to 1", minimum: 0, maximum: 1, required: false })
    expect(form.fields[1]).toMatchObject({ kind: "string", pattern: "^@", maxLength: 16, placeholder: "@acme" })
  })

  test("enums become choices; objects, arrays and untyped values fall back to JSON", () => {
    const form = schemaToForm({
      type: "object",
      properties: {
        region: { type: "string", enum: ["us", "eu"] },
        tier: { enum: [1, 2, 3] },
        filters: { type: "object", properties: { a: { type: "string" } } },
        domains: { type: "array", items: { type: "string" } },
        anything: {},
      },
    })
    expect(form.fields.map(f => [f.name, f.kind, f.jsonType])).toEqual([
      ["region", "enum", undefined], ["tier", "enum", undefined],
      ["filters", "json", "object"], ["domains", "json", "array"], ["anything", "json", "any"],
    ])
    expect(form.fields[1].options).toEqual([1, 2, 3])
  })

  test("no schema, empty properties, and open objects", () => {
    expect(schemaToForm(null)).toEqual({ fields: [], freeform: false })
    expect(schemaToForm({ type: "object", properties: {}, required: [] })).toEqual({ fields: [], freeform: false })
    expect(schemaToForm({ type: "object" }).freeform).toBe(true)
    expect(schemaToForm({ type: "object", additionalProperties: false }).freeform).toBe(false)
  })

  test("humanize", () => {
    expect(humanize("company_domain")).toBe("Company domain")
    expect(humanize("maxPages")).toBe("Max pages")
  })
})

describe("initialValues", () => {
  test("uses defaults and starts required checkboxes unchecked", () => {
    const form = schemaToForm({
      type: "object",
      properties: {
        q: { type: "string", default: "ceo" }, n: { type: "integer", default: 5 }, on: { type: "boolean" },
        mode: { enum: ["a", "b"], default: "b" }, tags: { type: "array", default: ["x"] },
      },
      required: ["on"],
    })
    expect(initialValues(form)).toEqual({ on: "false", q: "ceo", n: "5", mode: enumOptionValue("b"), tags: "[\n  \"x\"\n]" })
    expect(initialValues(schemaToForm({ type: "object" }))).toEqual({ [FREEFORM_FIELD]: "{}" })
  })
})

describe("buildInputs", () => {
  test("types values and omits empty optional fields", () => {
    const form = schemaToForm(shorthand)
    expect(buildInputs(form, { domain: "acme.example", max_pages: " 3 ", verbose: "" })).toEqual({
      inputs: { domain: "acme.example", max_pages: 3 }, errors: {},
    })
    expect(buildInputs(form, { domain: "acme.example", max_pages: "", verbose: "false" }).inputs).toEqual({ domain: "acme.example", verbose: false })
  })

  test("reports required, number, integer and bound errors", () => {
    const form = schemaToForm({
      type: "object",
      properties: { domain: { type: "string" }, n: { type: "integer", minimum: 1, maximum: 10 }, ratio: { type: "number" } },
      required: ["domain", "n"],
    })
    expect(buildInputs(form, { domain: "   ", n: "", ratio: "" }).errors).toEqual({ domain: "Required", n: "Required" })
    expect(buildInputs(form, { domain: "a", n: "2.5", ratio: "abc" }).errors).toEqual({ n: "Enter a whole number", ratio: "Enter a number" })
    expect(buildInputs(form, { domain: "a", n: "0" }).errors).toEqual({ n: "Must be at least 1" })
    expect(buildInputs(form, { domain: "a", n: "11" }).errors).toEqual({ n: "Must be at most 10" })
  })

  test("string constraints", () => {
    const form = schemaToForm({ type: "object", properties: { h: { type: "string", pattern: "^@\\w+$", minLength: 3 } } })
    expect(buildInputs(form, { h: "@a" }).errors.h).toBe("Use at least 3 characters")
    expect(buildInputs(form, { h: "acme" }).errors.h).toBe("Does not match the expected format")
    expect(buildInputs(form, { h: "@acme" }).inputs).toEqual({ h: "@acme" })
  })

  test("enum values keep their JSON type", () => {
    const form = schemaToForm({ type: "object", properties: { tier: { enum: [1, "1"] } }, required: ["tier"] })
    expect(buildInputs(form, { tier: enumOptionValue(1) }).inputs).toEqual({ tier: 1 })
    expect(buildInputs(form, { tier: enumOptionValue("1") }).inputs).toEqual({ tier: "1" })
    expect(buildInputs(form, { tier: "" }).errors).toEqual({ tier: "Required" })
    expect(buildInputs(form, { tier: "\"2\"" }).errors).toEqual({ tier: "Choose one of the options" })
  })

  test("JSON fields parse and check object vs array", () => {
    const form = schemaToForm({ type: "object", properties: { f: { type: "object" }, l: { type: "array" } } })
    expect(buildInputs(form, { f: "{\"a\": 1}", l: "[1]" }).inputs).toEqual({ f: { a: 1 }, l: [1] })
    expect(buildInputs(form, { f: "[1]", l: "{}" }).errors).toEqual({
      f: "Enter a JSON object, like {\"key\": \"value\"}", l: "Enter a JSON array, like [\"a\", \"b\"]",
    })
    expect(buildInputs(form, { f: "{nope" }).errors).toEqual({ f: "Enter valid JSON" })
  })

  test("required booleans always send a value", () => {
    const form = schemaToForm({ type: "object", properties: { dry_run: { type: "boolean" } }, required: ["dry_run"] })
    expect(buildInputs(form, initialValues(form)).inputs).toEqual({ dry_run: false })
    expect(buildInputs(form, { dry_run: "true" }).inputs).toEqual({ dry_run: true })
  })

  test("free-form inputs must be a JSON object", () => {
    const form = schemaToForm({ type: "object" })
    expect(buildInputs(form, { [FREEFORM_FIELD]: "{\"q\": \"x\"}" }).inputs).toEqual({ q: "x" })
    expect(buildInputs(form, { [FREEFORM_FIELD]: "[]" }).errors).toEqual({ [FREEFORM_FIELD]: "Enter the inputs as a JSON object" })
  })
})
