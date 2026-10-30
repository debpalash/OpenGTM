import type { RecordKind } from "@/lib/records"

export interface RecordColumn {
  id: string
  label: string
  shown: boolean
}
const COMPANY: RecordColumn[] = [
  { id: "identity", label: "Company", shown: true },
  { id: "domain", label: "Domain", shown: true },
  { id: "industry", label: "Industry", shown: true },
  { id: "fit", label: "Fit", shown: true },
  { id: "location", label: "Location", shown: true },
  { id: "completeness", label: "Data completeness", shown: true },
  { id: "updated", label: "Updated", shown: true },
  { id: "description", label: "Description", shown: false },
  { id: "employees", label: "Employee count", shown: false },
  { id: "phone", label: "Phone", shown: false },
  { id: "linkedin", label: "LinkedIn", shown: false }
]
const PERSON: RecordColumn[] = [
  { id: "identity", label: "Person", shown: true },
  { id: "company_role", label: "Company & role", shown: true },
  { id: "email", label: "Work email", shown: true },
  { id: "fit", label: "Fit", shown: true },
  { id: "updated", label: "Updated", shown: true },
  { id: "phone", label: "Phone", shown: false },
  { id: "linkedin", label: "LinkedIn", shown: false },
  { id: "location", label: "Location", shown: false },
  { id: "completeness", label: "Data completeness", shown: false }
]
export interface ColumnLayout {
  order: string[]
  hidden: string[]
}
export function recordColumns(kind: RecordKind): readonly RecordColumn[] {
  return kind === "company" ? COMPANY : PERSON
}
export function defaultColumnLayout(kind: RecordKind): ColumnLayout {
  const columns = recordColumns(kind)
  return {
    order: columns.map((column) => column.id),
    hidden: columns.filter((column) => !column.shown).map((column) => column.id)
  }
}
// Preferences contain identifiers only. They cannot introduce unknown fields,
// remove/reorder identity, or accidentally transfer a People layout to Companies.
export function parseColumnLayout(
  kind: RecordKind,
  raw: string | null
): ColumnLayout {
  try {
    if (raw && raw.length > 16384) return defaultColumnLayout(kind)
    const value = JSON.parse(raw ?? "null")
    if (
      value?.version !== 1 ||
      value.kind !== kind ||
      !Array.isArray(value.order) ||
      !Array.isArray(value.hidden) ||
      ![...value.order, ...value.hidden].every((id) => typeof id === "string")
    )
      return defaultColumnLayout(kind)
    const columns = recordColumns(kind)
    const known = new Set(columns.map((column) => column.id))
    const order = [
      "identity",
      ...new Set<string>(
        value.order.filter((id: string) => id !== "identity" && known.has(id))
      )
    ]
    const hidden = new Set<string>(
      value.hidden.filter((id: string) => id !== "identity" && known.has(id))
    )
    for (const column of columns)
      if (!order.includes(column.id)) {
        order.push(column.id)
        if (!column.shown) hidden.add(column.id)
      }
    return { order, hidden: order.filter((id) => hidden.has(id)) }
  } catch {
    return defaultColumnLayout(kind)
  }
}
export function encodeColumnLayout(
  kind: RecordKind,
  layout: ColumnLayout
): string {
  return JSON.stringify({ version: 1, kind, ...layout })
}
export function visibleColumns(
  kind: RecordKind,
  layout: ColumnLayout
): RecordColumn[] {
  const byId = new Map(recordColumns(kind).map((column) => [column.id, column]))
  return [...new Set(["identity", ...layout.order])]
    .filter((id) => id === "identity" || !layout.hidden.includes(id))
    .flatMap((id) => (byId.has(id) ? [byId.get(id)!] : []))
}
export function moveColumn(
  layout: ColumnLayout,
  id: string,
  direction: -1 | 1
): ColumnLayout {
  const index = layout.order.indexOf(id)
  const next = index + direction
  if (index < 1 || next < 1 || next >= layout.order.length) return layout
  const order = [...layout.order]
  ;[order[index], order[next]] = [order[next], order[index]]
  return { ...layout, order }
}
export function columnPreferenceKey(
  user: number,
  workspace: string,
  kind: RecordKind
): string {
  return `opengtm:record-columns:v1:${user}:${encodeURIComponent(workspace)}:${kind}`
}
