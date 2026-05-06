/**
 * Workbook API client — Hybrid model.
 *
 * Workbooks are filtered views on the Leads DB.
 * Rows are leads. AI/enrichment results overlay on top.
 */

const API = ""

// ── Types ────────────────────────────────────────────────────────────────

export interface EnrichmentOverlay {
  value: any
  status: "pending" | "running" | "complete" | "error" | "skipped"
  provider?: string | null
  error?: string | null
}

export interface ColumnConfig {
  id: string
  name: string
  type: "lead_field" | "enrichment" | "waterfall" | "ai_formula" | "conditional" | "output"
  width: number
  lead_field?: string | null
  provider?: string | null
  waterfall?: string[] | null
  target_field?: string | null
  prompt?: string | null
  input_columns?: string[] | null
  condition?: string | null
  destination?: string | null
  destination_config?: Record<string, any> | null
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
  status: "draft" | "running" | "paused" | "complete"
  filter_criteria: FilterCriteria | null
  columns_config: ColumnConfig[]
  total_rows: number
  completed_rows: number
  created_at: string
  updated_at: string
  last_run_at: string | null
}

/** A lead row as it appears in a workbook */
export interface WorkbookLeadRow {
  lead_id: number
  lead: Record<string, any>  // Full lead data
  enrichments: Record<string, EnrichmentOverlay>  // {column_id: overlay}
}

export interface WorkbookWithLeads {
  workbook: Workbook
  rows: WorkbookLeadRow[]
  total_rows: number
  page: number
  page_size: number
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
  return res.json()
}

export async function fetchWorkbook(id: string, page = 1, pageSize = 100): Promise<WorkbookWithLeads> {
  const res = await fetch(`${API}/api/workbooks/${id}?page=${page}&page_size=${pageSize}`)
  if (!res.ok) throw new Error("Failed to fetch workbook")
  return res.json()
}

export async function createWorkbook(data: {
  name: string
  description?: string
  filter_criteria?: FilterCriteria
  columns_config?: Partial<ColumnConfig>[]
}): Promise<Workbook> {
  const res = await fetch(`${API}/api/workbooks/`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(data),
  })
  if (!res.ok) throw new Error("Failed to create workbook")
  return res.json()
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
): Promise<{ status: string }> {
  const res = await fetch(`${API}/api/workbooks/${workbookId}/leads/${leadId}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(fields),
  })
  if (!res.ok) throw new Error("Failed to update lead field")
  return res.json()
}

// ── CSV Import (creates leads in DB) ─────────────────────────────────────

export async function importLeads(
  workbookId: string,
  rows: Record<string, any>[],
): Promise<{ created: number; total_rows: number }> {
  const res = await fetch(`${API}/api/workbooks/${workbookId}/import`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ rows }),
  })
  if (!res.ok) throw new Error("Failed to import leads")
  return res.json()
}

// ── Column Operations ────────────────────────────────────────────────────

export async function addColumn(workbookId: string, column: Partial<ColumnConfig>): Promise<Workbook> {
  const res = await fetch(`${API}/api/workbooks/${workbookId}/columns`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ column }),
  })
  if (!res.ok) throw new Error("Failed to add column")
  return res.json()
}

export async function deleteColumn(workbookId: string, colId: string): Promise<Workbook> {
  const res = await fetch(`${API}/api/workbooks/${workbookId}/columns/${colId}`, { method: "DELETE" })
  if (!res.ok) throw new Error("Failed to delete column")
  return res.json()
}

// ── Execution ────────────────────────────────────────────────────────────

export async function runWorkbook(
  workbookId: string,
  opts?: { column_ids?: string[]; lead_ids?: number[] },
): Promise<{ status: string; total_jobs: number; message: string }> {
  const res = await fetch(`${API}/api/workbooks/${workbookId}/run`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(opts || {}),
  })
  if (!res.ok) throw new Error("Failed to run workbook")
  return res.json()
}

export async function stopWorkbook(workbookId: string): Promise<void> {
  await fetch(`${API}/api/workbooks/${workbookId}/stop`, { method: "POST" })
}

export async function deleteLeads(leadIds: number[]): Promise<{ deleted: number }> {
  const results = await Promise.all(
    leadIds.map(id => fetch(`${API}/api/lead/${id}`, { method: "DELETE" }))
  )
  return { deleted: results.filter(r => r.ok).length }
}

// ── Meta ─────────────────────────────────────────────────────────────────

export async function fetchFilterOptions(): Promise<FilterOptions> {
  const res = await fetch(`${API}/api/workbooks/meta/filter-options`)
  return res.json()
}

export async function fetchProviders(): Promise<{
  providers: Array<{ name: string; capabilities: string[]; confidence: number }>
}> {
  const res = await fetch(`${API}/api/workbooks/meta/providers`)
  return res.json()
}

export async function fetchLeadFields(): Promise<{ fields: string[] }> {
  const res = await fetch(`${API}/api/workbooks/meta/lead-fields`)
  return res.json()
}

// ── WebSocket ────────────────────────────────────────────────────────────

export function createWorkbookSocket(workbookId: string): WebSocket {
  const protocol = window.location.protocol === "https:" ? "wss:" : "ws:"
  const host = window.location.host
  return new WebSocket(`${protocol}//${host}/api/workbooks/${workbookId}/ws`)
}
