import { parseRecordJSON } from "./record-json"
export type RecordKind = "company" | "person"
export interface SharedRecord {
  id: string; record_type: RecordKind; version: number; updated_at?: string
  company_id: string | null; person_id: string | null; employment_id: number | null
  fields: Record<string, unknown>; field_owners?: Record<string,{source:string;updated_at:string}>; company?: Record<string, unknown>
  employments?: { id: number; company_id: string | null; company_name: string; fields: Record<string, unknown>; current: boolean; first_observed_at: string; last_observed_at: string }[]
  external_ids?: { source: string; external_id: string }[]
}
export interface RecordPage { records: SharedRecord[]; total: number; limit: number; offset: number }
export interface ImportResult {
  preview: boolean; replayed: boolean; total: number; imported: number; rejected: number
  mapping: Record<string, string>; warnings: string[]
  rows: { row: number; status: string; id?: string; company_id?: string; error?: string }[]
}
export interface RecordImport {
  record_type: RecordKind; source: string; idempotency_key: string; csv?: string
  rows?: Record<string, unknown>[]; mapping?: Record<string, string>
}
export interface RecordWorkflow { name: string; columns: Record<string, unknown>[] }
export async function recordAPI<T>(path: string, body?: unknown, method = "POST", signal?: AbortSignal): Promise<T> {
  const res = await fetch(`/api/${path}`, body === undefined ? { signal } : { method, headers: { "Content-Type": "application/json" }, body: JSON.stringify(body), signal })
  if (!res.ok) { const error = await res.json().catch(() => ({})); throw new Error(typeof error.detail === "string" ? error.detail : `Request failed (${res.status})`) }
  return parseRecordJSON(await res.text()) as T
}
export function displayField(value: unknown): string {
  return value == null ? "" : typeof value === "object" ? JSON.stringify(value) : String(value)
}
export function downloadData(data: BlobPart, name: string, type = "application/json") {
  const url = URL.createObjectURL(new Blob([data], { type })); const a = document.createElement("a"); a.href = url; a.download = name; a.click(); setTimeout(() => URL.revokeObjectURL(url), 1000)
}

// Keep JSON number tokens intact when importing a file. Parsing a row into
// JavaScript numbers before sending it would round large external IDs.
export async function recordImportAPI(request: RecordImport, preview: boolean, rawRows = ""): Promise<ImportResult> {
  const metadata = { ...request }; delete metadata.rows
  const body = rawRows ? `${JSON.stringify(metadata).slice(0, -1)},"rows":${rawRows}}` : JSON.stringify(request)
  const res = await fetch(`/api/record-imports${preview ? "/preview" : ""}`, { method: "POST", headers: { "Content-Type": "application/json" }, body })
  const output = await res.json()
  if (!res.ok) throw new Error(typeof output.detail === "string" ? output.detail : `Request failed (${res.status})`)
  return output
}
