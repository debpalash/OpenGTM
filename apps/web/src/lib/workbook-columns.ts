/** Preserve executable/unknown types; only missing legacy types become inputs. */
export function normalizeWorkbookColumns<T extends { id?: string; key?: string; lead_field?: string | null; type?: string }>(columns: T[]) {
  return columns.map((column, index) => ({
    ...column,
    id: column.id || column.key || `col_${index}`,
    lead_field: column.lead_field || column.key || column.id || "",
    type: column.type || "lead_field",
  }))
}
