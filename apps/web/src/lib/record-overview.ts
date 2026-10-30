export interface OverviewWorkbook {
  id: string
  name: string
  status: string
  total_rows: number
  completed_rows: number
  updated_at: string | null
}
export interface OverviewImport {
  source: string
  imported: number
  rejected: number
  created_at: string
}
export interface RecordOverview {
  companies: number
  people: number
  people_with_email: number
  verified_emails: number
  companies_needing_domain: number
  running_workbooks: number
  failed_workbooks: number
  workbooks: OverviewWorkbook[]
  imports: OverviewImport[]
  sampled_at: string
}
const countKeys = [
  "companies",
  "people",
  "people_with_email",
  "verified_emails",
  "companies_needing_domain",
  "running_workbooks",
  "failed_workbooks"
] as const
function object(value: unknown): value is Record<string, unknown> {
  return !!value && typeof value === "object" && !Array.isArray(value)
}
function count(value: unknown): value is number {
  return typeof value === "number" && Number.isSafeInteger(value) && value >= 0
}
export function parseRecordOverview(value: unknown): RecordOverview {
  if (
    !object(value) ||
    !countKeys.every((key) => count(value[key])) ||
    typeof value.sampled_at !== "string" ||
    !Number.isFinite(Date.parse(value.sampled_at)) ||
    !Array.isArray(value.workbooks) ||
    !Array.isArray(value.imports)
  )
    throw new Error(
      "The workspace summary returned an invalid response. Try again."
    )
  for (const item of value.workbooks)
    if (
      !object(item) ||
      typeof item.id !== "string" ||
      typeof item.name !== "string" ||
      typeof item.status !== "string" ||
      !count(item.total_rows) ||
      !count(item.completed_rows) ||
      (item.updated_at !== null && typeof item.updated_at !== "string")
    )
      throw new Error("The workbook summary returned an invalid response.")
  for (const item of value.imports)
    if (
      !object(item) ||
      typeof item.source !== "string" ||
      !count(item.imported) ||
      !count(item.rejected) ||
      typeof item.created_at !== "string"
    )
      throw new Error("The import summary returned an invalid response.")
  if (
    (value.verified_emails as number) > (value.people_with_email as number) ||
    (value.people_with_email as number) > (value.people as number)
  )
    throw new Error(
      "The email coverage totals are inconsistent. Try refreshing the summary."
    )
  return value as unknown as RecordOverview
}
export function recordTimestamp(value: string): number {
  let normalized = value.trim().replace(" ", "T")
  if (/^\d{4}-\d{2}-\d{2}$/.test(normalized)) normalized += "T00:00:00"
  normalized = normalized
    .replace(/([+-]\d{2})$/, "$1:00")
    .replace(/([+-]\d{2})(\d{2})$/, "$1:$2")
    .replace(/(\.\d{3})\d+/, "$1")
  if (!/(?:Z|[+-]\d{2}:\d{2})$/i.test(normalized)) normalized += "Z"
  return Date.parse(normalized)
}
export function relativeTime(value?: string | null): string {
  if (!value) return "Not yet updated"
  const timestamp = recordTimestamp(value)
  if (!Number.isFinite(timestamp)) return "Update time unavailable"
  const minutes = Math.max(0, Math.floor((Date.now() - timestamp) / 60000))
  if (minutes < 1) return "Just now"
  if (minutes < 60) return `${minutes}m ago`
  if (minutes < 1440) return `${Math.floor(minutes / 60)}h ago`
  return `${Math.floor(minutes / 1440)}d ago`
}
