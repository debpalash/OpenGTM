/**
 * Workbook API client — Clay-style self-contained tables.
 *
 * Workbooks have their own rows (WorkbookRow), not live views on leads DB.
 * Enrichment results stored inline per row.
 */

import { authQuery } from "./auth"

const API = ""

// ── Types ────────────────────────────────────────────────────────────────

// Per-fact provenance for an enriched cell (license/freshness/source).
// Present only when PROVENANCE_TRACKING_ENABLED on the backend; absent on
// legacy/un-enriched cells.
export interface Provenance {
  source: string
  license: string
  confidence?: number | null
  fetched_at?: string | null
}

export interface ResearchEvidence {
  answer: string
  citations: { url: string; title: string; quoted_text: string; fetched_at?: string | null }[]
  cost_usd?: number | null
  stopped_reason?: string | null
  synthesis_fallback: boolean
}

export interface EnrichmentOverlay {
  value: any
  status: "pending" | "running" | "complete" | "error" | "skipped"
  provider?: string | null
  error?: string | null
  verify_status?: "valid" | "invalid" | "catch_all" | "unknown" | null
  provenance?: Provenance | null
  research?: ResearchEvidence | null
}

export interface ColumnConfig {
  id: string
  name: string
  type: "lead_field" | "input" | "source" | "enrichment" | "waterfall" | "ai_formula" | "conditional" | "agent" | "research" | "output" | "http" | "formula"
  width: number
  reactive?: boolean | null
  lead_field?: string | null
  provider?: string | null
  waterfall?: string[] | null
  target_field?: string | null
  prompt?: string | null
  input_columns?: string[] | null
  max_steps?: number | null
  cell_budget_usd?: number | null
  output_format?: string | null
  max_tokens?: number | null
  verify?: boolean | null
  condition?: string | null
  destination?: string | null
  destination_config?: Record<string, any> | null
  run_once?: boolean | null
  // Pillar 0 — source column
  icp?: { description?: string; industry?: string; geo?: string[]; size?: { min?: number; max?: number } } | null
  channels?: { categories?: string[]; regions?: string[]; explicit_sources?: string[] } | null
  target_rows?: number | null
  // Pillar 4 — agent column
  goal?: string | null
  tools?: string[] | null
  policy?: { max_steps?: number; max_cost_usd?: number; prefer?: string } | null
  // HTTP action column
  http_url?: string | null
  http_method?: string | null
  http_headers?: Record<string, string> | null
  http_body?: any | null
  http_extract?: string | null
  // Formula action column
  formula?: string | null
}

export interface FilterCriteria {
  city?: string
  state?: string
  score_tier?: string
  status?: string
  source?: string
  job_ids?: string[]
  specialization?: string
  company_size?: string
  has_email?: boolean
  has_phone?: boolean
  has_website?: boolean
  min_score?: number
  max_score?: number
  search?: string
}

export interface Workbook {
  id: string
  name: string
  description: string
  status: "draft" | "running" | "paused" | "failed" | "complete"
  source_type?: "empty" | "csv" | "leads_filter" | "job_results" | "ambitionbox"
  source_config?: Record<string, any> | null
  filter_criteria: FilterCriteria | null
  columns_config: ColumnConfig[]
  total_rows: number
  completed_rows: number
  sync_to_leads?: boolean
  budget_max_usd?: number
  budget_spent_usd?: number
  refresh_policy?: RefreshPolicy | null
  created_at: string
  updated_at: string
  last_run_at: string | null
}

/** Durable execution state for a paginated workbook source connector. */
export interface ConnectorRun {
  id: string
  workspace_id: string
  workbook_id: string
  connector: string
  status: "pending" | "running" | "retrying" | "complete" | "partial" | "failed"
  query: Record<string, any>
  cursor: Record<string, any>
  requested_count: number
  fetched_count: number
  added_count: number
  updated_count: number
  skipped_count: number
  pages_fetched: number
  source_total: number | null
  target_met: boolean
  exhausted: boolean
  error: string | null
  started_at: string | null
  completed_at: string | null
  created_at: string | null
  updated_at: string | null
}

/** A row as it appears in a workbook (v2: self-contained, v1 compat) */
export interface WorkbookLeadRow {
  lead_id: number | null
  row_id?: number  // WorkbookRow.id (v2)
  position?: number
  source_provider?: string | null
  source_record_id?: string | null
  source_rank?: number | null
  source_fetched_at?: string | null
  lead: Record<string, any>  // Full lead data (v1) or empty (v2)
  data?: Record<string, any>  // Self-contained row data (v2)
  company?: string  // Optional denormalized company label for display
  enrichments: Record<string, EnrichmentOverlay>  // {column_id: overlay}
  // Pillar 1 — canonical entity binding + cross-source trust signal
  canonical_entity_id?: string | null
  corroboration_count?: number | null
}

export interface WorkbookWithLeads {
  workbook: Workbook
  rows: WorkbookLeadRow[]
  total_rows: number
  query_total_rows?: number | null
  page: number
  page_size: number
  next_cursor?: string | null
  has_more?: boolean
}

export interface CsvImportOptions {
  rows: Record<string, any>[]
  mapping?: Record<string, string | null>
  create_columns?: boolean
  dedupe?: boolean
  file_name?: string
  source_system?: "auto" | "clay" | "generic"
}

export interface CsvImportResult {
  created: number
  added: number
  skipped_duplicates: number
  total_rows: number
  columns_added: string[]
  mapping: Record<string, string | null>
  analysis: {
    source_system: "clay" | "generic"
    detection_reason: string
    input_rows: number
    importable_rows: number
    custom_columns: string[]
    skipped_columns: string[]
    collisions: { target: string; headers: string[] }[]
  }
}

export interface FilterOptions {
  cities: string[]
  tiers: string[]
  sources: string[]
  statuses: string[]
  specializations: string[]
  total_leads: number
}

// ── Workbook CRUD ────────────────────────────────────────────────────────

export async function fetchWorkbooks(): Promise<{ workbooks: Workbook[]; total: number }> {
  const res = await fetch(`${API}/api/workbooks/`)
  if (!res.ok) throw new Error("Failed to fetch workbooks")
  const data = await res.json()
  if (!data || !Array.isArray(data.workbooks) || !data.workbooks.every((row: Partial<Workbook> | null) =>
    row && typeof row.id === "string" && typeof row.name === "string" && typeof row.status === "string" &&
    Array.isArray(row.columns_config) && Number.isFinite(row.total_rows) && Number.isFinite(row.completed_rows))) {
    throw new Error("The server returned an invalid workbook list. Try again.")
  }
  return data
}

export async function fetchWorkbook(
  id: string,
  page = 1,
  pageSize = 100,
  viewId?: string | null,
  search?: string,
  cursorMode = false,
  cursor?: string | null,
): Promise<WorkbookWithLeads> {
  const params = new URLSearchParams({ page: String(page), page_size: String(pageSize) })
  if (viewId) params.set("view_id", viewId)
  if (search?.trim()) params.set("search", search.trim())
  if (cursorMode) params.set("cursor_mode", "true")
  if (cursor) params.set("cursor", cursor)
  const res = await fetch(`${API}/api/workbooks/${id}?${params}`)
  if (!res.ok) {
    const err = new Error(
      res.status === 404 ? "Workbook not found or no access" : "Failed to fetch workbook",
    ) as Error & { status?: number }
    err.status = res.status
    throw err
  }
  return res.json()
}

export async function fetchConnectorRuns(workbookId: string, cursor?: string): Promise<{ runs: ConnectorRun[]; limit: number; offset: number | null; has_more: boolean; next_cursor: string | null }> {
  const query = new URLSearchParams({ limit: "50" })
  if (cursor) query.set("cursor", cursor)
  const res = await fetch(`${API}/api/workbooks/${workbookId}/connector-runs?${query}`)
  if (!res.ok) throw new Error("Failed to fetch connector runs")
  return res.json()
}

export async function createWorkbook(data: {
  name: string
  description?: string
  source?: "empty" | "csv" | "leads_filter" | "job_results"
  source_config?: Record<string, any>
  filter_criteria?: FilterCriteria
  columns_config?: Partial<ColumnConfig>[]
  max_rows?: number
}): Promise<Workbook> {
  const res = await fetch(`${API}/api/workbooks/`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(data),
  })
  if (!res.ok) throw new Error("Failed to create workbook")
  const workbook = await res.json()
  if (!workbook || typeof workbook.id !== "string" || !workbook.id.trim()) {
    throw new Error("The server did not return a workbook ID. Check your workbooks before retrying.")
  }
  return workbook
}

export async function updateWorkbook(id: string, data: Partial<Workbook>): Promise<Workbook> {
  const res = await fetch(`${API}/api/workbooks/${id}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(data),
  })
  if (!res.ok) throw new Error("Failed to update workbook")
  return res.json()
}

export async function createWorkbookFromJobs(data: {
  job_ids: string[]
  workbook_id?: string
  name?: string
}): Promise<Workbook> {
  const res = await fetch(`${API}/api/workbooks/from-jobs`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(data),
  })
  if (!res.ok) throw new Error("Failed to create workbook from jobs")
  return res.json()
}

export async function deleteWorkbook(id: string): Promise<void> {
  const res = await fetch(`${API}/api/workbooks/${id}`, { method: "DELETE" })
  if (!res.ok) throw new Error("Failed to delete workbook")
}

// ── Lead Field Update (from workbook context) ────────────────────────────

export async function updateLeadField(
  workbookId: string,
  leadId: number,
  fields: Record<string, any>,
): Promise<{ status: string; reactive_columns: string[]; recompute: { status: string; total_jobs?: number; message?: string } | null }> {
  const res = await fetch(`${API}/api/workbooks/${workbookId}/leads/${leadId}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(fields),
  })
  if (!res.ok) throw new Error("Failed to update lead field")
  return res.json()
}

/** Update a self-contained WorkbookRow snapshot (not the leads database). */
export async function updateWorkbookRow(
  workbookId: string,
  rowId: number,
  fields: Record<string, any>,
): Promise<{ status: string }> {
  const res = await fetch(`${API}/api/workbooks/${workbookId}/rows/${rowId}`, {
    method: "PATCH",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(fields),
  })
  if (!res.ok) throw new Error("Failed to update workbook row")
  return res.json()
}

export async function exportWorkbookCsv(
  id: string,
  viewId?: string | null,
  search?: string,
  rowIds?: number[],
): Promise<Blob> {
  const params = new URLSearchParams()
  if (viewId) params.set("view_id", viewId)
  if (search?.trim()) params.set("search", search.trim())
  rowIds?.forEach(rowId => params.append("row_ids", String(rowId)))
  const suffix = params.size ? `?${params}` : ""
  const res = await fetch(`${API}/api/workbooks/${id}/export.csv${suffix}`)
  if (!res.ok) {
    const payload = await res.json().catch(() => null)
    throw new Error(payload?.detail || "Failed to export workbook")
  }
  return res.blob()
}

/** Atomically update multiple snapshot rows (used by spreadsheet paste). */
export async function bulkUpdateWorkbookRows(
  workbookId: string,
  updates: Array<{ row_id: number; fields: Record<string, any> }>,
): Promise<{ status: string; updated_rows: number; reactive_columns: string[] }> {
  const res = await fetch(`${API}/api/workbooks/${workbookId}/rows`, {
    method: "PATCH",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ updates }),
  })
  if (!res.ok) {
    const payload = await res.json().catch(() => null)
    throw new Error(payload?.detail || "Failed to update workbook rows")
  }
  return res.json()
}

// ── CSV Import (creates leads in DB) ─────────────────────────────────────

export async function importLeads(
  workbookId: string,
  options: CsvImportOptions,
): Promise<CsvImportResult> {
  const res = await fetch(`${API}/api/workbooks/${workbookId}/import`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(options),
  })
  if (!res.ok) {
    const payload = await res.json().catch(() => null)
    throw new Error(payload?.detail || "Failed to import CSV")
  }
  return res.json()
}

// ── Column Operations ────────────────────────────────────────────────────

export async function addColumn(workbookId: string, column: Partial<ColumnConfig>): Promise<Workbook> {
  const res = await fetch(`${API}/api/workbooks/${encodeURIComponent(workbookId)}/columns`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ column }),
  })
  if (!res.ok) throw new Error(res.status === 409 ? "A column with this ID already exists. Refresh and review it before adding another." : "Failed to add column")
  const result = await res.json()
  const matches = Array.isArray(result?.columns_config) ? result.columns_config.filter((item: ColumnConfig) => item.id === column.id) : []
  if (result?.id !== workbookId || matches.length !== 1 || matches[0].name !== column.name || matches[0].type !== column.type) {
    throw new Error("Column creation was not confirmed. Refresh before trying again.")
  }
  return result
}

export interface GeneratedColumn {
  column: Partial<ColumnConfig> & { id: string; name: string; type: ColumnConfig["type"] }
  kind: "formula" | "ai_formula" | "http"
  explanation: string
}

/** NL → column generator: instruction → validated, ready-to-add column config. */
export async function generateColumn(workbookId: string, instruction: string): Promise<GeneratedColumn> {
  const res = await fetch(`${API}/api/v2/workbooks/${workbookId}/generate-column`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ instruction }),
  })
  if (!res.ok) throw new Error((await res.json().catch(() => null))?.detail || "Failed to generate column")
  return res.json()
}

export async function deleteColumn(workbookId: string, colId: string, expectedColumn: ColumnConfig): Promise<Workbook> {
  if (expectedColumn.id !== colId) throw new Error("Column confirmation identity does not match")
  const res = await fetch(`${API}/api/workbooks/${encodeURIComponent(workbookId)}/columns/${encodeURIComponent(colId)}`, {
    method: "DELETE", headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ expected_column: expectedColumn }),
  })
  if (!res.ok) {
    const detail = res.status === 409 ? (await res.json().catch(() => null))?.detail : null
    throw new Error(res.status === 409
      ? (typeof detail === "string" && detail.trim() ? detail : "Column changed since confirmation. Refresh and review before deleting.")
      : "Column deletion was not confirmed. Refresh before trying again.")
  }
  const result = await res.json()
  if (result?.id !== workbookId || !Array.isArray(result.columns_config) || result.columns_config.some((column: ColumnConfig) => column.id === colId)) {
    throw new Error("Column deletion was not confirmed. Refresh before trying again.")
  }
  return result
}

// ── Execution ────────────────────────────────────────────────────────────

export type WorkbookRunOptions = { column_ids?: string[]; row_ids?: number[]; lead_ids?: number[]; view_id?: string; search?: string; expected_rows?: number; fill_missing?: boolean; force?: boolean }

export async function runWorkbook(
  workbookId: string,
  opts?: WorkbookRunOptions,
): Promise<{ status: string; total_jobs: number; message: string; job_id?: number | null; run_id?: string | null }> {
  const res = await fetch(`${API}/api/workbooks/${workbookId}/run`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(opts || {}),
  }).catch(() => { throw new Error("Could not confirm whether the run started. Check activity before starting another run.") })
  if (!res.ok) {
    const detail = (await res.json().catch(() => null))?.detail
    throw new Error(typeof detail === "string" ? detail : detail?.message || "Failed to run workbook")
  }
  const result = await res.json()
  if (typeof result?.status !== "string" || typeof result?.message !== "string" || !Number.isInteger(result?.total_jobs) || result.total_jobs < 0) {
    throw new Error("The server returned an invalid run receipt. Check activity before starting another run.")
  }
  return result
}

/** (Re-)run a SINGLE cell synchronously. force=true bypasses success-skip gates
 * (for output columns it overrides run-once and re-pushes — confirm in the UI). */
export async function runWorkbookCell(
  workbookId: string,
  rowId: number,
  colId: string,
  force = false,
): Promise<{ status: "complete" | "error" | "skipped"; value: any; provider?: string | null; error?: string | null; skipped: boolean; forced: boolean }> {
  const res = await fetch(`${API}/api/workbooks/${workbookId}/rows/${rowId}/cells/${encodeURIComponent(colId)}/run`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ force }),
  })
  if (!res.ok) throw new Error("Failed to run cell")
  return res.json()
}

export async function stopWorkbook(workbookId: string): Promise<void> {
  const response = await fetch(`${API}/api/workbooks/${workbookId}/stop`, { method: "POST" })
  if (!response.ok) throw new Error("Could not stop the workbook run. Try again.")
}

export async function deleteLeads(leadIds: number[]): Promise<{ deleted: number }> {
  const results = await Promise.all(
    leadIds.map(id => fetch(`${API}/api/lead/${id}`, { method: "DELETE" }))
  )
  return { deleted: results.filter(r => r.ok).length }
}

export async function deleteWorkbookRows(
  workbookId: string,
  rowIds: number[],
): Promise<{ deleted: number }> {
  const res = await fetch(`${API}/api/workbooks/${workbookId}/rows`, {
    method: "DELETE",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ row_ids: rowIds }),
  })
  if (!res.ok) throw new Error("Failed to delete workbook rows")
  return res.json()
}

export async function deleteMatchingWorkbookRows(
  workbookId: string,
  options: { expected_count: number; confirmation: string; view_id?: string; search?: string },
): Promise<{ deleted: number; matched: number }> {
  const res = await fetch(`${API}/api/workbooks/${workbookId}/rows/delete-query`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(options),
  })
  if (!res.ok) {
    const payload = await res.json().catch(() => null)
    const detail = payload?.detail
    throw new Error(typeof detail === "string" ? detail : detail?.message || "Failed to delete matching rows")
  }
  return res.json()
}

// ── Saved Views (v2) ─────────────────────────────────────────────────────
// Named filter/sort/hidden-column presets per workbook, applied client-side.

export type ViewFilterOp = "equals" | "not_equals" | "contains" | "not_contains" | "empty" | "not_empty"

export interface ViewFilter {
  column: string
  op: ViewFilterOp
  value?: any
}

export interface ViewSort {
  column: string
  dir: "asc" | "desc"
}

export interface ViewConfig {
  filters: ViewFilter[]
  sort: ViewSort[]
  hidden_columns: string[]
}

export interface WorkbookView {
  id: string
  workbook_id: string
  name: string
  config: ViewConfig
  created_at?: string | null
  updated_at?: string | null
}

const V2 = `${API}/api/v2/workbooks`

export async function saveWorkbookColumnOrder(workbookId: string, columnIds: string[], expectedColumnIds: string[]): Promise<void> {
  const res = await fetch(`${API}/api/workbooks/${encodeURIComponent(workbookId)}/columns/order`, {
    method: "PATCH", headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ column_ids: columnIds, expected_column_ids: expectedColumnIds }),
  })
  if (res.status === 409) throw new Error("Column order changed. Refresh before reordering.")
  if (!res.ok) throw new Error("Column order was not saved")
  const result = await res.json()
  if (!Array.isArray(result?.column_ids) || result.column_ids.length !== columnIds.length ||
      !result.column_ids.every((id: unknown, index: number) => id === columnIds[index])) {
    throw new Error("Column order save was not confirmed")
  }
}

export async function saveWorkbookColumnWidth(workbookId: string, columnId: string, width: number): Promise<{ column_id: string; width: number }> {
  const res = await fetch(`${API}/api/workbooks/${encodeURIComponent(workbookId)}/columns/${encodeURIComponent(columnId)}/width`, {
    method: "PATCH", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ width }),
  })
  if (!res.ok) throw new Error("Column width was not saved")
  const result = await res.json()
  if (result?.column_id !== columnId || result.width !== width) throw new Error("Column width save was not confirmed")
  return result
}

export async function saveWorkbookColumnSettings(workbookId: string, columnId: string,
  changes: Record<string, unknown>, expected: Record<string, unknown>): Promise<{ column_id: string; changes: Record<string, unknown> }> {
  const res = await fetch(`${API}/api/workbooks/${encodeURIComponent(workbookId)}/columns/${encodeURIComponent(columnId)}/settings`, {
    method: "PATCH", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ changes, expected }),
  })
  if (!res.ok) {
    const error = await res.json().catch(() => null)
    throw new Error(res.status === 409
      ? (typeof error?.detail === "string" && error.detail.trim()
        ? error.detail : "Column settings changed elsewhere. Your edit was not saved.")
      : "Column settings could not be saved.")
  }
  const result = await res.json()
  const same = (a: unknown, b: unknown): boolean => {
    if (a === b) return true
    if (!a || !b || typeof a !== "object" || typeof b !== "object" || Array.isArray(a) !== Array.isArray(b)) return false
    const left = a as Record<string, unknown>, right = b as Record<string, unknown>
    return Object.keys(left).length === Object.keys(right).length && Object.keys(left).every(key =>
      Object.hasOwn(right, key) && same(left[key], right[key]))
  }
  if (result?.column_id !== columnId || !same(result.changes, changes)) throw new Error("Column settings save was not confirmed.")
  return result
}

function checkedView(value: unknown, workbookId: string): WorkbookView {
  const view = value as WorkbookView | null
  const config = view?.config
  if (!view || typeof view.id !== "string" || !view.id.trim() || view.workbook_id !== workbookId ||
      typeof view.name !== "string" || !config || !Array.isArray(config.filters) ||
      !Array.isArray(config.sort) || !Array.isArray(config.hidden_columns) ||
      !config.hidden_columns.every(column => typeof column === "string") ||
      !config.filters.every(filter => filter && typeof filter.column === "string" &&
        ["equals", "not_equals", "contains", "not_contains", "empty", "not_empty"].includes(filter.op)) ||
      !config.sort.every(sort => sort && typeof sort.column === "string" && ["asc", "desc"].includes(sort.dir))) {
    throw new Error("Server returned an invalid saved view")
  }
  return view
}

export async function fetchWorkbookViews(workbookId: string): Promise<{ views: WorkbookView[]; total: number }> {
  const res = await fetch(`${V2}/${workbookId}/views`)
  if (!res.ok) throw new Error("Failed to fetch views")
  const data = await res.json()
  if (!Array.isArray(data?.views)) throw new Error("Server returned an invalid view list")
  return { ...data, views: data.views.map((view: unknown) => checkedView(view, workbookId)) }
}

export async function createWorkbookView(
  workbookId: string,
  body: { name: string; config?: Partial<ViewConfig> },
): Promise<WorkbookView> {
  const res = await fetch(`${V2}/${workbookId}/views`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  })
  if (!res.ok) throw new Error("Failed to create view")
  return checkedView(await res.json(), workbookId)
}

export async function updateWorkbookView(
  workbookId: string,
  viewId: string,
  body: { name?: string; config?: ViewConfig },
): Promise<WorkbookView> {
  const res = await fetch(`${V2}/${workbookId}/views/${viewId}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  })
  if (!res.ok) throw new Error("Failed to update view")
  const view = checkedView(await res.json(), workbookId)
  if (view.id !== viewId) throw new Error("Server returned a different saved view")
  return view
}

export async function deleteWorkbookView(workbookId: string, viewId: string): Promise<void> {
  const res = await fetch(`${V2}/${workbookId}/views/${viewId}`, { method: "DELETE" })
  if (!res.ok) throw new Error("Failed to delete view")
}

// ── Meta ─────────────────────────────────────────────────────────────────

export async function fetchFilterOptions(): Promise<FilterOptions> {
  const res = await fetch(`${API}/api/workbooks/meta/filter-options`)
  return res.json()
}

export async function fetchProviders(): Promise<{
  providers: Array<{ name: string; capabilities: string[]; confidence: number; maturity: "beta" | "supported" }>
}> {
  const res = await fetch(`${API}/api/workbooks/meta/providers`)
  return res.json()
}

export async function fetchLeadFields(): Promise<{ fields: string[] }> {
  const res = await fetch(`${API}/api/workbooks/meta/lead-fields`)
  return res.json()
}

export interface AiColumnPreset {
  id: string
  name: string
  category: string
  column_type: "ai_formula" | "research"
  output_format: string
  description: string
  prompt: string
}

export async function fetchAiColumnPresets(): Promise<{ presets: AiColumnPreset[]; categories: string[] }> {
  const res = await fetch(`${API}/api/workbooks/meta/ai-column-presets`)
  if (!res.ok) return { presets: [], categories: [] }
  return res.json()
}

export interface RunCostEstimate {
  unknown_providers?: string[]
  catalog_complete?: boolean
  rows: number
  worst_usd: number
  best_usd: number
  breakdown: Array<{ column: string; paid_providers: string[]; worst_usd: number; best_usd: number }>
  note: string
}

export async function fetchRunEstimate(workbookId: string, viewId?: string | null, search?: string, signal?: AbortSignal): Promise<RunCostEstimate> {
  const params = new URLSearchParams()
  if (viewId) params.set("view_id", viewId)
  if (search?.trim()) params.set("search", search.trim())
  const res = await fetch(`${API}/api/workbooks/${workbookId}/run/estimate${params.size ? `?${params}` : ""}`, { signal })
  if (!res.ok) throw new Error("Could not load the run estimate. Try again before starting.")
  const estimate = await res.json()
  const hasCoverage = estimate?.unknown_providers !== undefined || estimate?.catalog_complete !== undefined
  if (hasCoverage && (!Array.isArray(estimate.unknown_providers) ||
      !estimate.unknown_providers.every((name: unknown) => typeof name === "string" && name.trim().length > 0) ||
      estimate.catalog_complete !== (estimate.unknown_providers.length === 0))) {
    throw new Error("The server returned invalid estimate coverage. Refresh before starting.")
  }
  if (!Number.isSafeInteger(estimate?.rows) || estimate.rows < 0 ||
      !Number.isFinite(estimate?.best_usd) || estimate.best_usd < 0 ||
      !Number.isFinite(estimate?.worst_usd) || estimate.worst_usd < estimate.best_usd ||
      !Array.isArray(estimate?.breakdown) || typeof estimate?.note !== "string") {
    throw new Error("The server returned an invalid run estimate. Refresh before starting.")
  }
  return estimate
}

// ── WebSocket ────────────────────────────────────────────────────────────

export function createWorkbookSocket(workbookId: string): WebSocket {
  const protocol = window.location.protocol === "https:" ? "wss:" : "ws:"
  const host = window.location.host
  return new WebSocket(`${protocol}//${host}/api/workbooks/${workbookId}/ws${authQuery()}`)
}

// ── Pillar 0: Source columns (live sourcing) ─────────────────────────────

export interface SourcePreview {
  query: string
  source_count: number
  sources: Array<{ name: string; label: string; category?: string; region?: string[] }>
}

export async function addSourceColumn(
  workbookId: string,
  body: { name?: string; icp: Record<string, any>; channels?: Record<string, any>; target_rows?: number },
): Promise<{ column: ColumnConfig }> {
  const res = await fetch(`${API}/api/workbooks/${workbookId}/sources`, {
    method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body),
  })
  if (!res.ok) throw new Error("Failed to add source column")
  return res.json()
}

export async function runSourceColumn(workbookId: string, colId: string): Promise<{ status: string }> {
  const res = await fetch(`${API}/api/workbooks/${workbookId}/sources/${colId}/run`, { method: "POST" })
  if (!res.ok) throw new Error("Failed to run source column")
  return res.json()
}

export async function previewSourceColumn(workbookId: string, colId: string): Promise<SourcePreview> {
  const res = await fetch(`${API}/api/workbooks/${workbookId}/sources/${colId}/preview`)
  if (!res.ok) throw new Error("Failed to preview source")
  return res.json()
}

// ── Pillar 2: Cost & provider stats ──────────────────────────────────────

export interface CostInfo {
  budget_max_usd: number
  budget_spent_usd: number
  reserved_usd: number
  uncertain_usd: number
  accounting_basis: "catalog_estimates_and_recorded_charges"
  remaining_usd: number | null
  unlimited: boolean
}

export async function fetchWorkbookCost(workbookId: string): Promise<CostInfo> {
  const res = await fetch(`${API}/api/workbooks/${encodeURIComponent(workbookId)}/cost`)
  if (!res.ok) throw new Error("Failed to fetch cost")
  const cost = await res.json()
  const nonnegative = (value: unknown) => typeof value === "number" && Number.isFinite(value) && value >= 0
  if (!cost || ![cost.budget_max_usd, cost.budget_spent_usd, cost.reserved_usd, cost.uncertain_usd].every(nonnegative) ||
      typeof cost.unlimited !== "boolean" || cost.unlimited !== (cost.budget_max_usd === 0) ||
      cost.accounting_basis !== "catalog_estimates_and_recorded_charges" ||
      (cost.unlimited ? cost.remaining_usd !== null : typeof cost.remaining_usd !== "number" || !Number.isFinite(cost.remaining_usd))) {
    throw new Error("Server returned invalid budget balances")
  }
  return cost
}

export async function setWorkbookBudget(workbookId: string, maxUsd: number): Promise<Pick<CostInfo, "budget_max_usd" | "budget_spent_usd">> {
  if (!Number.isFinite(maxUsd) || maxUsd < 0) throw new Error("Budget must be a nonnegative finite amount")
  const res = await fetch(`${API}/api/workbooks/${encodeURIComponent(workbookId)}/budget`, {
    method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ max_usd: maxUsd }),
  })
  if (!res.ok) throw new Error("Failed to set budget")
  const receipt = await res.json()
  if (receipt?.budget_max_usd !== maxUsd || typeof receipt.budget_spent_usd !== "number" ||
      !Number.isFinite(receipt.budget_spent_usd) || receipt.budget_spent_usd < 0) {
    throw new Error("Budget save was not confirmed. Refresh before retrying.")
  }
  return receipt
}

export interface ProviderStat {
  provider: string; field: string; attempts: number; hits: number
  hit_rate: number; avg_confidence: number; avg_latency_ms: number
  total_cost_usd: number; cooldown_until: string | null
}

export async function fetchProviderStats(): Promise<{ stats: ProviderStat[] }> {
  const res = await fetch(`${API}/api/workbooks/meta/provider-stats`)
  if (!res.ok) throw new Error("Failed to fetch provider stats")
  return res.json()
}

// ── Pillar 3: Living workbooks ───────────────────────────────────────────

export interface RefreshPolicy {
  enabled?: boolean
  interval?: "hourly" | "daily" | "weekly" | null
  on_signal?: string[]
  staleness_ttl_days?: Record<string, number>
}

export async function setRefreshPolicy(workbookId: string, policy: RefreshPolicy): Promise<any> {
  const res = await fetch(`${API}/api/workbooks/${workbookId}/refresh-policy`, {
    method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify(policy),
  })
  if (!res.ok) throw new Error("Failed to set refresh policy")
  return res.json()
}

export async function refreshWorkbook(workbookId: string): Promise<{ status: string }> {
  const res = await fetch(`${API}/api/workbooks/${workbookId}/refresh`, { method: "POST" })
  if (!res.ok) throw new Error("Failed to refresh workbook")
  return res.json()
}

export interface ActivityItem { id: number; kind: string; message: string; created_at: string | null }

export async function fetchActivity(workbookId: string, limit = 50): Promise<{ activity: ActivityItem[] }> {
  const res = await fetch(`${API}/api/workbooks/${workbookId}/activity?limit=${limit}`)
  if (!res.ok) throw new Error("Failed to fetch activity")
  return res.json()
}

// ── Pillar 4: Agent column reasoning trace ───────────────────────────────

export interface CellTrace {
  workbook_id: string; lead_id: number; column_id: string; goal: string
  steps: Array<{ step: number; provider: string; success: boolean; value?: string; cost?: number; reason: string }>
  outcome: string; total_cost_usd: Record<string, number>
}

export async function fetchCellTrace(workbookId: string, leadId: number, colId: string): Promise<CellTrace> {
  const res = await fetch(`${API}/api/workbooks/${workbookId}/rows/${leadId}/cells/${colId}/trace`)
  if (!res.ok) throw new Error("No trace for this cell")
  return res.json()
}

// ── Pillar 1: Entity graph ───────────────────────────────────────────────

export interface CompanyEntity {
  id: string; workspace_id: string; canonical_name: string
  primary_domain: string; corroboration_count: number; observation_count: number
  sources: string[]; fields: Record<string, Array<{ value: string; source: string; observed_at: string }>>
  source_agreement: Record<string, number>
}

export async function fetchEntity(entityId: string): Promise<CompanyEntity> {
  const res = await fetch(`${API}/api/entities/company/${entityId}`)
  if (!res.ok) throw new Error("Entity not found")
  return res.json()
}

export async function fetchEntities(opts?: { min_corroboration?: number; workspace_id?: string }): Promise<{ entities: CompanyEntity[] }> {
  const p = new URLSearchParams()
  if (opts?.min_corroboration) p.set("min_corroboration", String(opts.min_corroboration))
  if (opts?.workspace_id != null) p.set("workspace_id", opts.workspace_id)
  const res = await fetch(`${API}/api/entities/company?${p.toString()}`)
  if (!res.ok) throw new Error("Failed to fetch entities")
  return res.json()
}

export async function mergeEntities(keptId: string, mergedId: string): Promise<any> {
  const res = await fetch(`${API}/api/entities/merge`, {
    method: "POST", headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ kept_id: keptId, merged_id: mergedId }),
  })
  if (!res.ok) throw new Error("Failed to merge entities")
  return res.json()
}

export interface ReviewPair { id: number; entity_id: string; candidate: Record<string, any>; score: number; reason: string; status: string }

export async function fetchReviewQueue(): Promise<{ pairs: ReviewPair[] }> {
  const res = await fetch(`${API}/api/entities/review-queue`)
  if (!res.ok) throw new Error("Failed to fetch review queue")
  return res.json()
}

export async function decideReview(pairId: number, decision: "merge" | "reject"): Promise<any> {
  const res = await fetch(`${API}/api/entities/review-queue/decide`, {
    method: "POST", headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ pair_id: pairId, decision }),
  })
  if (!res.ok) throw new Error("Failed to decide review")
  return res.json()
}
