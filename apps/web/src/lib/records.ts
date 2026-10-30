import { parseRecordJSON } from "./record-json"
export type RecordKind = "company" | "person"
export interface SharedRecord {
  id: string
  record_type: RecordKind
  version: number
  updated_at?: string
  company_id: string | null
  person_id: string | null
  employment_id: number | null
  fields: Record<string, unknown>
  field_owners?: Record<string, { source: string; updated_at: string }>
  company?: Record<string, unknown>
  employments?: {
    id: number
    company_id: string | null
    company_name: string
    fields: Record<string, unknown>
    current: boolean
    first_observed_at: string
    last_observed_at: string
  }[]
  external_ids?: { source: string; external_id: string }[]
}
export interface RecordPage {
  records: SharedRecord[]
  total: number
  limit: number
  offset: number
}
export interface ImportResult {
  preview: boolean
  replayed: boolean
  total: number
  imported: number
  rejected: number
  mapping: Record<string, string>
  warnings: string[]
  rows: {
    row: number
    status: string
    id?: string
    company_id?: string
    error?: string
  }[]
}
export interface RecordImport {
  record_type: RecordKind
  source: string
  idempotency_key: string
  csv?: string
  rows?: Record<string, unknown>[]
  mapping?: Record<string, string>
}
export interface RecordWorkflow {
  name: string
  columns: Record<string, unknown>[]
}
export class RecordRequestError extends Error {
  readonly status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
    this.name = "RecordRequestError"
  }
}
export async function recordAPI<T>(
  path: string,
  body?: unknown,
  method = "POST",
  signal?: AbortSignal
): Promise<T> {
  const res = await fetch(
    `/api/${path}`,
    body === undefined
      ? { signal }
      : {
          method,
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(body),
          signal
        }
  )
  if (!res.ok) {
    const error: unknown = await res.json().catch(() => null)
    const detail =
      error &&
      typeof error === "object" &&
      "detail" in error &&
      typeof error.detail === "string"
        ? error.detail
        : `Request failed (${res.status})`
    throw new RecordRequestError(res.status, detail)
  }
  return parseRecordJSON(await res.text()) as T
}
export function validateSharedRecord(
  value: unknown,
  kind: RecordKind,
  id: string
): SharedRecord {
  if (
    !id ||
    !value ||
    typeof value !== "object" ||
    !("id" in value) ||
    value.id !== id ||
    !("record_type" in value) ||
    value.record_type !== kind ||
    !("version" in value) ||
    typeof value.version !== "number" ||
    !Number.isSafeInteger(value.version) ||
    value.version < 1 ||
    !("fields" in value) ||
    !value.fields ||
    typeof value.fields !== "object" ||
    Array.isArray(value.fields)
  ) {
    throw new Error(
      "The record response could not be verified. Your draft is kept; try loading the current record again."
    )
  }
  const data = value as Record<string, unknown>
  const object = (item: unknown): item is Record<string, unknown> =>
    !!item && typeof item === "object" && !Array.isArray(item)
  const invalid =
    (data.company != null && !object(data.company)) ||
    (data.company_id != null && typeof data.company_id !== "string") ||
    (data.employments != null &&
      (!Array.isArray(data.employments) ||
        !data.employments.every(
          (item) =>
            object(item) &&
            typeof item.id === "number" &&
            Number.isSafeInteger(item.id) &&
            typeof item.current === "boolean" &&
            typeof item.company_name === "string" &&
            object(item.fields) &&
            typeof item.first_observed_at === "string" &&
            typeof item.last_observed_at === "string"
        ))) ||
    (data.field_owners != null &&
      (!object(data.field_owners) ||
        !Object.values(data.field_owners).every(
          (item) =>
            object(item) &&
            typeof item.source === "string" &&
            typeof item.updated_at === "string"
        ))) ||
    (data.external_ids != null &&
      (!Array.isArray(data.external_ids) ||
        !data.external_ids.every(
          (item) =>
            object(item) &&
            typeof item.source === "string" &&
            typeof item.external_id === "string"
        )))
  if (invalid)
    throw new Error(
      "The record relationships or source metadata could not be verified. Your draft is kept."
    )
  return value as SharedRecord
}
export async function fetchSharedRecord(
  kind: RecordKind,
  id: string,
  signal?: AbortSignal
): Promise<SharedRecord> {
  return validateSharedRecord(
    await recordAPI<unknown>(
      `records/${kind}/${encodeURIComponent(id)}`,
      undefined,
      "GET",
      signal
    ),
    kind,
    id
  )
}
export async function fetchRecordPage(
  kind: RecordKind,
  options: Record<string, string | number>,
  signal?: AbortSignal
): Promise<RecordPage> {
  const params = new URLSearchParams(
    Object.entries(options).map(([key, value]) => [key, String(value)])
  )
  const value = await recordAPI<unknown>(
    `records/${kind}?${params}`,
    undefined,
    "GET",
    signal
  )
  if (
    !value ||
    typeof value !== "object" ||
    !("records" in value) ||
    !Array.isArray(value.records) ||
    !("total" in value) ||
    typeof value.total !== "number" ||
    !Number.isSafeInteger(value.total) ||
    value.total < value.records.length ||
    !("limit" in value) ||
    typeof value.limit !== "number" ||
    !Number.isSafeInteger(value.limit) ||
    value.limit < 1 ||
    value.limit > 500 ||
    value.records.length > value.limit ||
    !("offset" in value) ||
    typeof value.offset !== "number" ||
    !Number.isSafeInteger(value.offset) ||
    value.offset < 0
  )
    throw new Error(
      "The record collection response could not be verified. Try loading it again."
    )
  for (const record of value.records) {
    if (!record || typeof record !== "object" || typeof record.id !== "string")
      throw new Error("The record collection contains an invalid identity.")
    validateSharedRecord(record, kind, record.id)
  }
  return value as RecordPage
}
export function displayField(value: unknown): string {
  return value == null
    ? ""
    : typeof value === "object"
      ? JSON.stringify(value)
      : String(value)
}
export function downloadData(
  data: BlobPart,
  name: string,
  type = "application/json"
) {
  const url = URL.createObjectURL(new Blob([data], { type }))
  const a = document.createElement("a")
  a.href = url
  a.download = name
  a.click()
  setTimeout(() => URL.revokeObjectURL(url), 1000)
}

// Keep JSON number tokens intact when importing a file. Parsing a row into
// JavaScript numbers before sending it would round large external IDs.
export { recordImportAPI } from "./record-import"
