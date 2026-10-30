import type { ImportColumn } from "@/lib/record-import"
import type { RecordKind } from "@/lib/records"

export const COMPANY_FIELDS = [
  "name",
  "domain",
  "website",
  "email",
  "phone",
  "linkedin_url",
  "industry",
  "description",
  "city",
  "state",
  "country",
  "address",
  "employee_count_exact",
  "company_size",
  "technologies",
  "funding_stage",
  "revenue_range"
]
export const PERSON_FIELDS = [
  "full_name",
  "first_name",
  "last_name",
  "email",
  "personal_email",
  "phone",
  "mobile_phone",
  "linkedin_url"
]
export const EMPLOYMENT_FIELDS = ["title", "department", "seniority"]
const relationships = new Set([
  "external_id",
  "company_id",
  "person_id",
  "company_external_id",
  "company_source"
])
export const MAPPING_GROUPS = [
  "Company",
  "Person",
  "Employment",
  "Custom",
  "Ignored"
] as const
export type MappingGroup = (typeof MAPPING_GROUPS)[number]

export function mappingGroup(target: string, kind: RecordKind): MappingGroup {
  if (!target) return "Ignored"
  if (target === "person_id" || (target === "external_id" && kind === "person"))
    return "Person"
  if (relationships.has(target)) return "Company"
  const [owner, ...parts] = target.split(".")
  const field = parts.join(".")
  if (owner === "company" && COMPANY_FIELDS.includes(field)) return "Company"
  if (owner === "person" && PERSON_FIELDS.includes(field)) return "Person"
  if (owner === "employment" && EMPLOYMENT_FIELDS.includes(field))
    return "Employment"
  return "Custom"
}

// Match the server's mapping rules; row/identity validation remains the preview's job.
export function mappingIssues(
  columns: ImportColumn[],
  mapping: Record<string, string>,
  kind: RecordKind
): Map<string, string> {
  const issues = new Map<string, string>(),
    targets = new Map<string, string[]>()
  for (const column of columns) {
    const target = mapping[column.name] ?? column.target
    if (!target) continue
    const dot = target.indexOf(".")
    const owner = target.slice(0, dot)
    if (
      !relationships.has(target) &&
      (dot < 1 ||
        dot === target.length - 1 ||
        !["company", "person", "employment"].includes(owner))
    )
      issues.set(
        column.name,
        "Use company.field, person.field or employment.field, or clear to ignore."
      )
    else if (
      kind === "company" &&
      (owner === "person" || owner === "employment")
    )
      issues.set(
        column.name,
        "Company imports cannot write Person or Employment fields."
      )
    const names = targets.get(target) ?? []
    names.push(column.name)
    targets.set(target, names)
  }
  for (const [target, names] of targets)
    if (names.length > 1)
      for (const name of names)
        issues.set(
          name,
          `Also mapped by ${names.filter((other) => other !== name).join(", ")}. Keep one column for ${target}.`
        )
  return issues
}

export function mappingWarnings(
  columns: ImportColumn[],
  mapping: Record<string, string>,
  kind: RecordKind
): string[] {
  const targets = new Set(
    columns.map((column) => mapping[column.name] ?? column.target)
  )
  const identifiers =
    kind === "company"
      ? [
          "company_id",
          "external_id",
          "company.domain",
          "company.website",
          "company.linkedin_url"
        ]
      : ["person_id", "external_id", "person.email", "person.linkedin_url"]
  const warnings: string[] = []
  if (!identifiers.some((target) => targets.has(target)))
    warnings.push(
      "No stable record identifier is mapped. Names alone may not reconnect to existing records; review row validation before importing."
    )
  if (
    kind === "person" &&
    [...targets].some((target) => target.startsWith("employment.")) &&
    ![
      "company_id",
      "company_external_id",
      "company.domain",
      "company.website",
      "company.name"
    ].some((target) => targets.has(target))
  )
    warnings.push(
      "Employment fields have no mapped company. Review row validation for missing employer links."
    )
  return warnings
}

export function importTargets(kind: RecordKind) {
  return [
    "external_id",
    "company_id",
    "company_external_id",
    "company_source",
    ...(kind === "person" ? ["person_id"] : []),
    ...COMPANY_FIELDS.map((field) => `company.${field}`),
    ...(kind === "person"
      ? [
          ...PERSON_FIELDS.map((field) => `person.${field}`),
          ...EMPLOYMENT_FIELDS.map((field) => `employment.${field}`)
        ]
      : [])
  ]
}
