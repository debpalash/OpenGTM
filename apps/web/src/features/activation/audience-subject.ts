export interface AudienceSubject {
  lead_id: number | null
  company_id?: string | null
  person_id?: string | null
  snapshot: Record<string, unknown>
}
const text = (value: unknown) => (typeof value === "string" ? value : "")

/** Canonical identity wins over a legacy Lead reference when both exist. */
export function audienceSubject(subject: AudienceSubject) {
  const data = subject.snapshot
  const personID =
    subject.person_id ||
    text(data.person_id) ||
    (data.record_type === "person" ? text(data.id) : "")
  const companyID =
    subject.company_id ||
    text(data.company_id) ||
    (data.record_type === "company" ? text(data.id) : "")
  if (personID)
    return {
      kind: "Person",
      name:
        text(data.full_name) ||
        text(data.contact_person) ||
        text(data.email) ||
        "Unnamed person",
      detail: [
        text(data.company) || text(data.name),
        text(data.contact_title) || text(data.title)
      ]
        .filter(Boolean)
        .join(" · "),
      href: `/people?record=${encodeURIComponent(personID)}`
    }
  if (companyID)
    return {
      kind: "Company",
      name:
        text(data.company) ||
        text(data.name) ||
        text(data.domain) ||
        "Unnamed company",
      detail: text(data.domain) || text(data.website),
      href: `/companies?record=${encodeURIComponent(companyID)}`
    }
  if (subject.lead_id != null)
    return {
      kind: "Legacy lead",
      name:
        text(data.company) ||
        text(data.contact_person) ||
        `Lead ${subject.lead_id}`,
      detail: text(data.contact_title) || text(data.email),
      href: `/leads/${subject.lead_id}`
    }
  return {
    kind: "Unresolved record",
    name:
      text(data.company) ||
      text(data.full_name) ||
      text(data.contact_person) ||
      "Record unavailable",
    detail: "No record reference is available.",
    href: null
  }
}
export function audienceScope(
  filters: Record<string, unknown>
): "Companies" | "People" | "Legacy leads" {
  return filters._record_type === "company"
    ? "Companies"
    : filters._record_type === "person"
      ? "People"
      : "Legacy leads"
}
