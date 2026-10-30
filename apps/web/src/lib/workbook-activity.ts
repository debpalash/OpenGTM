import type { ColumnConfig, WorkbookLeadRow } from "./workbook-api"
import { isExecutableColumn, loadedCellProgress } from "./workbook-progress"

export interface ActivityEvent {
  id: string
  ts: number
  type: "enrich_start" | "enrich_complete" | "enrich_error" | "enrich_skip"
  column: string
  company: string
  provider?: string
  value?: string
  error?: string
}
export type ActivitySnapshot = Record<string, string>

function identity(row: WorkbookLeadRow) {
  return row.row_id ?? row.lead_id ?? row.position
}
function rowName(row: WorkbookLeadRow) {
  const data = row.data ?? row.lead
  for (const name of [
    data?.full_name,
    data?.name,
    row.company,
    data?.company
  ]) {
    if (typeof name === "string" && name.trim()) return name
  }
  return `Row ${identity(row) ?? "unknown"}`
}

/** Totals describe loaded rows, using the grid's complete execution-type set. */
export function workbookActivitySummary(
  rows: WorkbookLeadRow[],
  columns: ColumnConfig[]
) {
  return columns
    .filter((column) => isExecutableColumn(column.type))
    .map((column) => ({
      id: column.id,
      name: column.name,
      provider:
        column.provider ||
        column.waterfall?.join(" → ") ||
        "Provider not specified",
      ...loadedCellProgress(rows, [column])
    }))
}

/** Current failures can be inspected after reload, independently of the live feed. */
export function workbookActivityErrors(
  rows: WorkbookLeadRow[],
  columns: ColumnConfig[]
) {
  return rows.flatMap((row) =>
    columns
      .filter((column) => isExecutableColumn(column.type))
      .flatMap((column) => {
        const overlay = row.enrichments?.[column.id]
        return overlay?.status === "error"
          ? [
              {
                id: `${identity(row)}:${column.id}`,
                column: column.name,
                company: rowName(row),
                provider: overlay.provider || undefined,
                error:
                  overlay.error ||
                  "The provider did not return an error description."
              }
            ]
          : []
      })
  )
}

/** Observe transitions, including a final response that also changes run status.
 * Existing results/newly paged-in rows establish a baseline, not invented events.
 */
export function workbookActivityChanges(
  rows: WorkbookLeadRow[],
  columns: ColumnConfig[],
  previous: ActivitySnapshot,
  now: number
) {
  const snapshot: ActivitySnapshot = {}
  const events: ActivityEvent[] = []
  const runnable = columns.filter((column) => isExecutableColumn(column.type))
  for (const row of rows)
    for (const column of runnable) {
      if (identity(row) == null) continue
      const key = `${identity(row)}:${column.id}`
      const overlay = row.enrichments?.[column.id]
      const status = overlay?.status ?? "pending"
      snapshot[key] = status
      if (!overlay || previous[key] === undefined || previous[key] === status)
        continue
      const type =
        status === "complete"
          ? "enrich_complete"
          : status === "error"
            ? "enrich_error"
            : status === "running"
              ? "enrich_start"
              : status === "skipped"
                ? "enrich_skip"
                : null
      if (!type) continue
      events.push({
        id: `${key}:${now}:${status}`,
        ts: now,
        type,
        column: column.name,
        company: rowName(row),
        provider: overlay.provider || undefined,
        value:
          type === "enrich_complete"
            ? (typeof overlay.value === "string"
                ? overlay.value
                : JSON.stringify(overlay.value)
              )?.slice(0, 160)
            : undefined,
        error: overlay.error || undefined
      })
    }
  return { snapshot, events }
}
