import type { ImportResult, RecordImport, RecordKind } from "./records"

export interface ImportColumn {
  name: string
  target: string
  match: "matched" | "explicit" | "custom" | "chosen"
  precision_sensitive: boolean
  samples: { row: number; value: string; truncated: boolean }[]
}
export interface ImportInspection {
  record_type: RecordKind
  rows: number
  columns: ImportColumn[]
}
const object = (value: unknown): value is Record<string, unknown> =>
  !!value && typeof value === "object" && !Array.isArray(value)
const count = (value: unknown): value is number =>
  typeof value === "number" &&
  Number.isSafeInteger(value) &&
  value >= 0 &&
  value <= 10000
const mapping = (value: unknown): value is Record<string, string> =>
  object(value) &&
  Object.values(value).every((item) => typeof item === "string")
export class ImportRequestError extends Error {
  readonly status: number
  readonly outcome: "refused" | "unknown"
  constructor(status: number, message: string) {
    super(message)
    this.status = status
    this.outcome = [401, 403, 404, 413, 422].includes(status)
      ? "refused"
      : "unknown"
  }
}
export function validateImportResult(
  value: unknown,
  preview: boolean
): ImportResult {
  const invalid = () =>
    new Error(
      preview
        ? "The preview could not be verified. Your file and mapping are kept."
        : "The import receipt could not be verified. Check the batch status before retrying."
    )
  if (
    !object(value) ||
    value.preview !== preview ||
    typeof value.replayed !== "boolean" ||
    !count(value.total) ||
    value.total < 1 ||
    !count(value.imported) ||
    !count(value.rejected) ||
    value.imported + value.rejected !== value.total ||
    !mapping(value.mapping) ||
    !Array.isArray(value.warnings) ||
    !value.warnings.every((item) => typeof item === "string") ||
    !Array.isArray(value.rows) ||
    value.rows.length !== value.total
  )
    throw invalid()
  const seen = new Set<number>()
  let accepted = 0
  for (const row of value.rows) {
    if (
      !object(row) ||
      !count(row.row) ||
      row.row < 1 ||
      row.row > value.total ||
      seen.has(row.row)
    )
      throw invalid()
    seen.add(row.row)
    if (row.status === "accepted") {
      if (typeof row.id !== "string" || !row.id || row.error != null)
        throw invalid()
      accepted++
    } else if (
      row.status !== "rejected" ||
      typeof row.error !== "string" ||
      !row.error
    )
      throw invalid()
    if (row.company_id != null && typeof row.company_id !== "string")
      throw invalid()
  }
  if (accepted !== value.imported) throw invalid()
  return value as unknown as ImportResult
}
export function validateImportInspection(
  value: unknown,
  kind: RecordKind
): ImportInspection {
  if (
    !object(value) ||
    value.record_type !== kind ||
    !count(value.rows) ||
    value.rows < 1 ||
    !Array.isArray(value.columns)
  )
    throw new Error(
      "The source columns could not be verified. Your file is kept."
    )
  const rowCount = value.rows
  const seen = new Set<string>()
  for (const column of value.columns) {
    if (
      !object(column) ||
      typeof column.name !== "string" ||
      seen.has(column.name) ||
      typeof column.target !== "string" ||
      !["matched", "explicit", "custom", "chosen"].includes(
        String(column.match)
      ) ||
      typeof column.precision_sensitive !== "boolean" ||
      !Array.isArray(column.samples) ||
      column.samples.length > 3 ||
      !column.samples.every(
        (sample) =>
          object(sample) &&
          count(sample.row) &&
          sample.row >= 1 &&
          sample.row <= rowCount &&
          typeof sample.value === "string" &&
          typeof sample.truncated === "boolean"
      )
    )
      throw new Error(
        "The source samples could not be verified. Your file is kept."
      )
    seen.add(column.name)
  }
  return value as unknown as ImportInspection
}
function body(request: RecordImport, rawRows: string) {
  if (!rawRows) return JSON.stringify(request)
  const metadata = { ...request }
  delete metadata.rows
  return `${JSON.stringify(metadata).slice(0, -1)},"rows":${rawRows}}`
}
async function post(
  request: RecordImport,
  path: string,
  rawRows: string,
  signal?: AbortSignal
): Promise<unknown> {
  const response = await fetch(path, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: body(request, rawRows),
    signal: signal ?? AbortSignal.timeout(300_000)
  })
  const output: unknown = await response.json().catch(() => null)
  if (!response.ok)
    throw new ImportRequestError(
      response.status,
      object(output) && typeof output.detail === "string"
        ? output.detail
        : `Request failed (${response.status})`
    )
  return output
}
export async function recordImportAPI(
  request: RecordImport,
  preview: boolean,
  rawRows = ""
): Promise<ImportResult> {
  return validateImportResult(
    await post(
      request,
      `/api/record-imports${preview ? "/preview" : ""}`,
      rawRows
    ),
    preview
  )
}
export async function inspectRecordImport(
  request: RecordImport,
  rawRows = "",
  signal?: AbortSignal
): Promise<ImportInspection> {
  return validateImportInspection(
    await post(request, "/api/record-imports/inspect", rawRows, signal),
    request.record_type
  )
}
export async function fetchImportReceipt(key: string): Promise<ImportResult> {
  const response = await fetch(
    `/api/record-imports/${encodeURIComponent(key)}`,
    { signal: AbortSignal.timeout(30_000) }
  )
  if (!response.ok)
    throw new ImportRequestError(
      response.status,
      response.status === 404
        ? "No completed receipt yet. Reselect the same file to retry with this batch key; the import may still be running."
        : `Batch status is unavailable (${response.status}). Try again.`
    )
  return {
    ...validateImportResult(await response.json(), false),
    replayed: true
  }
}
