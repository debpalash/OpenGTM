// Maps a plugin's `inputs` JSON Schema (type: object) onto form fields, and
// form values back onto run inputs.
//
// Supported: string, number, integer and boolean properties, `enum`, the
// usual string/number bounds, `default`, and `required`. Objects, arrays and
// properties without a usable type fall back to a JSON text field. A schema
// without properties that still accepts extra keys gets one free-form JSON
// field for the whole inputs object. The server validates the inputs again
// against the full schema before queueing the run; this mapping only catches
// mistakes early and keeps types right (numbers as numbers, not strings).

import type { JsonSchema } from "./plugin-api"

export type FieldKind = "string" | "number" | "integer" | "boolean" | "enum" | "json"
export type JsonFieldType = "object" | "array" | "any"

export interface FormField {
  name: string
  label: string
  kind: FieldKind
  required: boolean
  description?: string
  /** Enum options in schema order. */
  options?: unknown[]
  /** Expected JSON value for `json` fields. */
  jsonType?: JsonFieldType
  defaultValue?: unknown
  placeholder?: string
  format?: string
  pattern?: string
  minimum?: number
  maximum?: number
  minLength?: number
  maxLength?: number
}

export interface SchemaForm {
  fields: FormField[]
  /** No declared properties but extra keys allowed: edit inputs as one JSON object. */
  freeform: boolean
}

/** Text for every field; booleans are "true" | "false" | "" (unset). */
export type FormValues = Record<string, string>

export const FREEFORM_FIELD = "__inputs__"

const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === "object" && value !== null && !Array.isArray(value)

/** First non-null type of a property (`["string", "null"]` → "string"). */
function primaryType(schema: JsonSchema): string | undefined {
  if (Array.isArray(schema.type)) return schema.type.find(t => t !== "null")
  if (typeof schema.type === "string") return schema.type
  if (schema.enum?.length) {
    const kinds = new Set(schema.enum.map(v => typeof v))
    return kinds.size === 1 ? ({ string: "string", number: "number", boolean: "boolean" } as Record<string, string>)[[...kinds][0]] : undefined
  }
  if (schema.properties) return "object"
  if (schema.items) return "array"
  return undefined
}

/** "company_domain" → "Company domain". */
export function humanize(name: string): string {
  const words = name.replace(/([a-z0-9])([A-Z])/g, "$1 $2").replace(/[_-]+/g, " ").trim().toLowerCase()
  return words ? words[0].toUpperCase() + words.slice(1) : name
}

function fieldFor(name: string, schema: JsonSchema, required: boolean): FormField {
  const base = {
    name,
    label: schema.title?.trim() || humanize(name),
    required,
    description: schema.description,
    defaultValue: schema.default,
    placeholder: Array.isArray(schema.examples) && schema.examples.length ? String(schema.examples[0]) : undefined,
  }
  const enumValues = Array.isArray(schema.enum) ? schema.enum : schema.const !== undefined ? [schema.const] : undefined
  if (enumValues?.length && enumValues.every(v => ["string", "number", "boolean"].includes(typeof v))) {
    return { ...base, kind: "enum", options: enumValues }
  }
  const type = primaryType(schema)
  switch (type) {
    case "string":
      return { ...base, kind: "string", format: schema.format, pattern: schema.pattern, minLength: schema.minLength, maxLength: schema.maxLength }
    case "number":
    case "integer":
      return { ...base, kind: type, minimum: schema.minimum, maximum: schema.maximum }
    case "boolean":
      return { ...base, kind: "boolean" }
    case "object":
    case "array":
      return { ...base, kind: "json", jsonType: type }
    default:
      return { ...base, kind: "json", jsonType: "any" }
  }
}

export function schemaToForm(schema: JsonSchema | null | undefined): SchemaForm {
  if (!schema || !isRecord(schema)) return { fields: [], freeform: false }
  const properties = (isRecord(schema.properties) ? schema.properties : {}) as Record<string, JsonSchema>
  const required = new Set(Array.isArray(schema.required) ? schema.required : [])
  const fields = Object.entries(properties)
    .filter(([, property]) => isRecord(property))
    .map(([name, property]) => fieldFor(name, property, required.has(name)))
  // Required fields first; schema order otherwise (Array.sort is stable).
  fields.sort((a, b) => Number(b.required) - Number(a.required))
  // `properties: {}` declares "no inputs"; only a schema that names no
  // properties at all (and allows extra keys) is edited as raw JSON.
  const freeform = schema.properties === undefined && schema.additionalProperties !== false && primaryType(schema) === "object"
  return { fields, freeform }
}

/** Option value for an enum entry; JSON keeps "1" and 1 distinct. */
export const enumOptionValue = (value: unknown) => JSON.stringify(value)

function initialValue(field: FormField): string {
  const value = field.defaultValue
  if (value === undefined || value === null) return ""
  switch (field.kind) {
    case "enum": return enumOptionValue(value)
    case "boolean": return typeof value === "boolean" ? String(value) : ""
    case "json": return JSON.stringify(value, null, 2)
    default: return typeof value === "object" ? JSON.stringify(value) : String(value)
  }
}

export function initialValues(form: SchemaForm): FormValues {
  const values: FormValues = {}
  for (const field of form.fields) {
    values[field.name] = initialValue(field)
    // A required checkbox always has a value; start it unchecked.
    if (field.kind === "boolean" && field.required && !values[field.name]) values[field.name] = "false"
  }
  if (form.freeform) values[FREEFORM_FIELD] = "{}"
  return values
}

export interface BuildResult {
  inputs: Record<string, unknown>
  /** Field name → message; empty when the inputs are ready to send. */
  errors: Record<string, string>
}

function parseField(field: FormField, raw: string): { value?: unknown; error?: string } {
  const text = field.kind === "string" ? raw : raw.trim()
  if (raw.trim() === "") {
    return field.required ? { error: field.kind === "boolean" ? "Choose a value" : "Required" } : {}
  }
  switch (field.kind) {
    case "string": {
      if (field.minLength !== undefined && text.length < field.minLength) return { error: `Use at least ${field.minLength} characters` }
      if (field.maxLength !== undefined && text.length > field.maxLength) return { error: `Use at most ${field.maxLength} characters` }
      if (field.pattern) {
        try {
          if (!new RegExp(field.pattern, "u").test(text)) return { error: "Does not match the expected format" }
        } catch { /* an invalid pattern is the server's to report */ }
      }
      return { value: text }
    }
    case "number":
    case "integer": {
      const value = Number(text)
      if (!Number.isFinite(value)) return { error: "Enter a number" }
      if (field.kind === "integer" && !Number.isInteger(value)) return { error: "Enter a whole number" }
      if (field.minimum !== undefined && value < field.minimum) return { error: `Must be at least ${field.minimum}` }
      if (field.maximum !== undefined && value > field.maximum) return { error: `Must be at most ${field.maximum}` }
      return { value }
    }
    case "boolean":
      if (text === "true") return { value: true }
      if (text === "false") return { value: false }
      return { error: "Choose yes or no" }
    case "enum": {
      const match = field.options?.find(option => enumOptionValue(option) === text)
      return match === undefined ? { error: "Choose one of the options" } : { value: match }
    }
    case "json": {
      let value: unknown
      try { value = JSON.parse(text) } catch { return { error: "Enter valid JSON" } }
      if (field.jsonType === "object" && !isRecord(value)) return { error: "Enter a JSON object, like {\"key\": \"value\"}" }
      if (field.jsonType === "array" && !Array.isArray(value)) return { error: "Enter a JSON array, like [\"a\", \"b\"]" }
      return { value }
    }
  }
}

/** Convert form values to typed run inputs, collecting per-field errors. */
export function buildInputs(form: SchemaForm, values: FormValues): BuildResult {
  const inputs: Record<string, unknown> = {}
  const errors: Record<string, string> = {}
  if (form.freeform) {
    const text = (values[FREEFORM_FIELD] ?? "").trim()
    if (text) {
      let parsed: unknown
      try { parsed = JSON.parse(text) } catch { parsed = undefined }
      if (isRecord(parsed)) Object.assign(inputs, parsed)
      else errors[FREEFORM_FIELD] = "Enter the inputs as a JSON object"
    }
  }
  for (const field of form.fields) {
    const { value, error } = parseField(field, values[field.name] ?? "")
    if (error) errors[field.name] = error
    else if (value !== undefined) inputs[field.name] = value
  }
  return { inputs, errors }
}
