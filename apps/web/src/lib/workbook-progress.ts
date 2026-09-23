const executable = new Set(["waterfall", "enrichment", "ai_formula", "research", "agent", "http", "formula", "output"])
export const isExecutableColumn = (type: string) => executable.has(type)

export function loadedCellProgress(rows: Array<{ enrichments?: Record<string, { status?: string }> }>, columns: Array<{ id: string; type: string }>) {
  const runnable = columns.filter(column => isExecutableColumn(column.type))
  const result = { total: rows.length * runnable.length, complete: 0, errors: 0, skipped: 0, running: 0, pending: 0 }
  for (const row of rows) for (const column of runnable) {
    const status = row.enrichments?.[column.id]?.status
    if (status === "complete") result.complete++
    else if (status === "error") result.errors++
    else if (status === "skipped") result.skipped++
    else if (status === "running") result.running++
    else result.pending++
  }
  return result
}
