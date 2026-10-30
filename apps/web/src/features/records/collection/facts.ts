import { displayField, type SharedRecord } from "@/lib/records"

const EMPTY = new Set([
  "",
  "n/a",
  "na",
  "unknown",
  "not found",
  "none",
  "null",
  "undefined",
  "—",
  "-"
])
export function factText(value: unknown): string {
  if (typeof value === "string")
    return EMPTY.has(value.trim().toLowerCase()) ? "" : value.trim()
  if (typeof value === "number")
    return Number.isFinite(value) ? String(value) : ""
  if (Array.isArray(value))
    return value
      .map((item) =>
        typeof item === "string" || typeof item === "number"
          ? factText(item)
          : ""
      )
      .filter(Boolean)
      .join(", ")
  return ""
}
export function recordName(record: SharedRecord): string {
  return (
    displayField(record.fields.name || record.fields.full_name).trim() ||
    "Unnamed record"
  )
}
export function currentRole(record: SharedRecord): string {
  const employment = record.employments?.find((item) => item.current)
  return (
    factText(employment?.fields.title) ||
    factText(employment?.fields.contact_title)
  )
}
export function currentCompany(record: SharedRecord): {
  name: string
  id: string | null
} {
  const employment = record.employments?.find((item) => item.current)
  return {
    name:
      factText(record.company?.name) ||
      factText(employment?.company_name) ||
      factText(record.company?.domain),
    id: record.company_id || employment?.company_id || null
  }
}
export function recordLocation(record: SharedRecord): string {
  const parts = [record.fields.city, record.fields.state, record.fields.country]
    .map(factText)
    .filter(Boolean)
  return [...new Set(parts)].join(", ") || factText(record.fields.full_address)
}
export interface CoreFact {
  label: string
  present: boolean
}
export function coreFacts(record: SharedRecord): CoreFact[] {
  const fields = record.fields
  const company = currentCompany(record)
  const values: Array<[string, unknown]> =
    record.record_type === "company"
      ? [
          ["Company name", factText(fields.name) || factText(fields.full_name)],
          ["Domain", fields.domain],
          [
            "Industry",
            factText(fields.industry) || factText(fields.specialization)
          ],
          ["Location", recordLocation(record)],
          ["Description", fields.description],
          ["Employee count", fields.employee_count_exact]
        ]
      : [
          ["Full name", factText(fields.full_name) || factText(fields.name)],
          ["Current company", company.name || company.id],
          ["Current role", currentRole(record)],
          ["Work email", fields.email],
          ["Phone", fields.phone],
          ["LinkedIn", fields.linkedin_url]
        ]
  return values.map(([label, value]) => ({ label, present: !!factText(value) }))
}
export type EmailState =
  | "verified"
  | "invalid"
  | "risky"
  | "unverified"
  | "missing"
export function emailState(record: SharedRecord): EmailState {
  if (!factText(record.fields.email)) return "missing"
  const raw = record.fields.email_verify_status || record.fields.email_verify
  const status = factText(
    raw && typeof raw === "object" && !Array.isArray(raw) && "status" in raw
      ? raw.status
      : raw
  ).toLowerCase()
  if (["valid", "verified", "deliverable"].includes(status)) return "verified"
  if (["invalid", "undeliverable", "not_valid"].includes(status))
    return "invalid"
  if (
    ["risky", "catch_all", "catch-all", "accept_all", "accept-all"].includes(
      status
    )
  )
    return "risky"
  return "unverified"
}

export function safeExternalURL(value: string): string | null {
  try {
    if (!/^https?:\/\//i.test(value)) return null
    const url = new URL(value)
    return ["http:", "https:"].includes(url.protocol) &&
      !url.username &&
      !url.password
      ? url.href
      : null
  } catch {
    return null
  }
}
