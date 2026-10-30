import { displayField, type RecordKind, type SharedRecord } from "@/lib/records"

export type FieldType = "text" | "number" | "boolean" | "json"
export interface RecordDraft {
  fields: Record<string, string>
  types: Record<string, FieldType>
  added: string[]
  companyId: string
  companyName: string
  title: string
  endEmployment: boolean
  pendingField: { name: string; type: FieldType }
}
export const currentTitle = (record?: SharedRecord) =>
  displayField(record?.employments?.find((item) => item.current)?.fields.title)
export const fieldType = (value: unknown): FieldType =>
  typeof value === "number"
    ? "number"
    : typeof value === "boolean"
      ? "boolean"
      : value !== null && typeof value === "object"
        ? "json"
        : "text"
export class DraftFieldError extends Error {
  readonly field: string
  constructor(field: string, message: string) {
    super(message)
    this.field = field
    this.name = "DraftFieldError"
  }
}
export function createRecordDraft(
  kind: RecordKind,
  record?: SharedRecord
): RecordDraft {
  const fields = {
    ...(kind === "company"
      ? { name: "", domain: "", industry: "", city: "", description: "" }
      : { full_name: "", email: "", linkedin_url: "", phone: "" }),
    ...record?.fields
  }
  return {
    fields: Object.fromEntries(
      Object.entries(fields).map(([key, value]) => [key, displayField(value)])
    ),
    types: Object.fromEntries(
      Object.entries(fields).map(([key, value]) => [key, fieldType(value)])
    ),
    added: [],
    companyId: record?.company_id ?? "",
    companyName: displayField(record?.company?.name),
    title: currentTitle(record),
    endEmployment: false,
    pendingField: { name: "", type: "text" }
  }
}
export function isRecordDraftDirty(
  draft: RecordDraft,
  baseline?: SharedRecord
) {
  return (
    !!draft.pendingField.name.trim() ||
    draft.added.length > 0 ||
    draft.endEmployment ||
    Object.entries(draft.fields).some(
      ([key, value]) => value !== displayField(baseline?.fields[key])
    ) ||
    draft.companyId !== (baseline?.company_id ?? "") ||
    draft.title !== currentTitle(baseline)
  )
}
export function addDraftField(
  draft: RecordDraft,
  name: string,
  type: FieldType
): RecordDraft {
  const key = name.trim()
  if (!key || ["__proto__", "constructor", "prototype"].includes(key))
    throw new DraftFieldError(
      "$custom",
      "Choose a nonempty field name other than __proto__, constructor or prototype."
    )
  if (Object.hasOwn(draft.fields, key))
    throw new DraftFieldError(
      "$custom",
      "A field with this name already exists."
    )
  return {
    ...draft,
    fields: {
      ...draft.fields,
      [key]: type === "boolean" ? "false" : type === "json" ? "{}" : ""
    },
    types: { ...draft.types, [key]: type },
    added: [...draft.added, key],
    pendingField: { name: "", type }
  }
}
function parseValue(key: string, value: string, type: FieldType): unknown {
  if (type === "text") return value
  if (!value.trim()) return null
  if (type === "boolean") {
    if (value !== "true" && value !== "false")
      throw new DraftFieldError(key, "Choose Yes, No or Not provided.")
    return value === "true"
  }
  if (type === "number") {
    const number = Number(value)
    if (
      !Number.isFinite(number) ||
      (Number.isInteger(number) && !Number.isSafeInteger(number))
    )
      throw new DraftFieldError(
        key,
        "Enter a finite number within the browser’s safe range. Store long identifiers as text."
      )
    return number
  }
  try {
    return JSON.parse(value, (_, item: unknown) => {
      if (
        typeof item === "number" &&
        (!Number.isFinite(item) ||
          (Number.isInteger(item) && !Number.isSafeInteger(item)))
      )
        throw new Error("unsafe number")
      return item
    })
  } catch {
    throw new DraftFieldError(
      key,
      "Enter valid JSON with browser-safe numbers. Store long identifiers as text; your draft is kept."
    )
  }
}
export function draftFieldChanges(draft: RecordDraft, baseline?: SharedRecord) {
  const fields: Record<string, unknown> = {}
  for (const [key, value] of Object.entries(draft.fields)) {
    if (
      baseline &&
      !draft.added.includes(key) &&
      value === displayField(baseline.fields[key])
    )
      continue
    fields[key] = parseValue(key, value, draft.types[key] ?? "text")
  }
  return fields
}
export function recordDraftWrite(
  kind: RecordKind,
  draft: RecordDraft,
  baseline?: SharedRecord
) {
  if (draft.pendingField.name.trim())
    throw new DraftFieldError(
      "$custom",
      "Add or clear the custom field name before saving. Your draft is kept."
    )
  const fields = draftFieldChanges(draft, baseline)
  if (kind === "company") return { company: fields }
  if (draft.endEmployment) return { person: fields, end_employment: true }
  if (!draft.companyId && baseline?.company_id)
    throw new DraftFieldError(
      "$employment",
      "Choose End current employment to remove the employer. You can add an employer-unknown role in a separate edit afterward."
    )
  return {
    person: fields,
    ...(draft.companyId !== (baseline?.company_id ?? "") && draft.companyId
      ? { company_id: draft.companyId }
      : {}),
    ...(draft.title !== currentTitle(baseline)
      ? { employment: { title: draft.title } }
      : {})
  }
}

function equal(a: unknown, b: unknown): boolean {
  if (Object.is(a, b)) return true
  if (
    !a ||
    !b ||
    typeof a !== "object" ||
    typeof b !== "object" ||
    Array.isArray(a) !== Array.isArray(b)
  )
    return false
  const left = Object.keys(a),
    right = Object.keys(b)
  return (
    left.length === right.length &&
    left.every(
      (key) =>
        Object.hasOwn(b, key) &&
        equal(
          (a as Record<string, unknown>)[key],
          (b as Record<string, unknown>)[key]
        )
    )
  )
}
export interface ConflictRow {
  key: string
  before: unknown
  current: unknown
  draft: unknown
  mineChanged: boolean
  currentChanged: boolean
  needsChoice: boolean
}
const relationship = (record: SharedRecord) => ({
  companyId: record.company_id ?? "",
  title: currentTitle(record),
  ended: !record.employments?.some((item) => item.current) && !record.company_id
})
const draftRelationship = (draft: RecordDraft, baseline: SharedRecord) =>
  draft.endEmployment
    ? { companyId: "", title: "", ended: true }
    : {
        companyId: draft.companyId,
        title: draft.title,
        ended: !draft.companyId && !draft.title && relationship(baseline).ended
      }
export function recordConflictRows(
  kind: RecordKind,
  baseline: SharedRecord,
  draft: RecordDraft,
  latest: SharedRecord
): ConflictRow[] {
  const changed = draftFieldChanges(draft, baseline)
  const rows: ConflictRow[] = []
  for (const key of new Set([
    ...Object.keys(baseline.fields),
    ...Object.keys(latest.fields),
    ...Object.keys(changed)
  ])) {
    const before = baseline.fields[key],
      current = latest.fields[key]
    const mineChanged = Object.hasOwn(changed, key)
    const proposed = mineChanged ? changed[key] : before
    const currentChanged = !equal(before, current)
    if (mineChanged || currentChanged)
      rows.push({
        key,
        before,
        current,
        draft: proposed,
        mineChanged,
        currentChanged,
        needsChoice: mineChanged && currentChanged && !equal(current, proposed)
      })
  }
  if (kind === "person") {
    const before = relationship(baseline),
      current = relationship(latest),
      proposed = draftRelationship(draft, baseline)
    const mineChanged = !equal(before, proposed),
      currentChanged = !equal(before, current)
    if (mineChanged || currentChanged)
      rows.push({
        key: "$employment",
        before,
        current,
        draft: proposed,
        mineChanged,
        currentChanged,
        needsChoice: mineChanged && currentChanged && !equal(current, proposed)
      })
  }
  return rows
}
/** Start from the latest record, then retain only the explicitly reviewed edits. */
export function resolveRecordConflict(
  kind: RecordKind,
  baseline: SharedRecord,
  draft: RecordDraft,
  latest: SharedRecord,
  choices: Record<string, "draft" | "current">
): RecordDraft {
  const result = createRecordDraft(kind, latest)
  result.pendingField = { ...draft.pendingField }
  for (const row of recordConflictRows(kind, baseline, draft, latest)) {
    if (row.needsChoice && !choices[row.key])
      throw new DraftFieldError(
        row.key,
        "Choose a value for every field changed in both versions."
      )
    if (!row.mineChanged || choices[row.key] === "current") continue
    if (row.key === "$employment") {
      result.companyId = draft.companyId
      result.companyName = draft.companyName
      result.title = draft.title
      result.endEmployment = draft.endEmployment
    } else {
      result.fields[row.key] = draft.fields[row.key]
      result.types[row.key] = draft.types[row.key]
      if (!Object.hasOwn(latest.fields, row.key)) result.added.push(row.key)
    }
  }
  return result
}
