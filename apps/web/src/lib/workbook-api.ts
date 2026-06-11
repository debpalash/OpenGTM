/**
 * Workbook API client — Clay-style self-contained tables.
 *
 * Workbooks have their own rows (WorkbookRow), not live views on leads DB.
 * Enrichment results stored inline per row.
 */

import { authQuery } from "./auth"

const API = ""

// ── Types ────────────────────────────────────────────────────────────────

export interface EnrichmentOverlay {
  value: any
  status: "pending" | "running" | "complete" | "error" | "skipped"
  provider?: string | null
  error?: string | null
  verify_status?: "valid" | "invalid" | "catch_all" | "unknown" | null
}

export interface ColumnConfig {
  id: string
  name: string
  type: "lead_field" | "source" | "enrichment" | "waterfall" | "ai_formula" | "conditional" | "agent" | "output" | "http" | "formula"
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
  status: "draft" | "running" | "paused" | "complete"
  source_type?: "empty" | "csv" | "leads_filter" | "job_results"
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

/** A row as it appears in a workbook (v2: self-contained, v1 compat) */
export interface WorkbookLeadRow {
  lead_id: number
  row_id?: number  // WorkbookRow.id (v2)
  position?: number
  lead: Record<string, any>  // Full lead data (v1) or empty (v2)
  data?: Record<string, any>  // Self-contained row data (v2)
  enrichments: Record<string, EnrichmentOverlay>  // {column_id: overlay}
  // Pillar 1 — canonical entity binding + cross-source trust signal
  canonical_entity_id?: string | null
  corroboration_count?: number | null
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
  if (!res.ok) {
    const err = new Error(
      res.status === 404 ? "Workbook not found or no access" : "Failed to fetch workbook",
    ) as Error & { status?: number }
    err.status = res.status
    throw err
  }
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
  opts?: { column_ids?: string[]; lead_ids?: number[]; fill_missing?: boolean },
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
  rows: number
  worst_usd: number
  best_usd: number
  breakdown: Array<{ column: string; paid_providers: string[]; worst_usd: number; best_usd: number }>
  note: string
}

export async function fetchRunEstimate(workbookId: string): Promise<RunCostEstimate | null> {
  const res = await fetch(`${API}/api/workbooks/${workbookId}/run/estimate`)
  if (!res.ok) return null
  return res.json()
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
  remaining_usd: number | null
  unlimited: boolean
}

export async function fetchWorkbookCost(workbookId: string): Promise<CostInfo> {
  const res = await fetch(`${API}/api/workbooks/${workbookId}/cost`)
  if (!res.ok) throw new Error("Failed to fetch cost")
  return res.json()
}

export async function setWorkbookBudget(workbookId: string, maxUsd: number): Promise<CostInfo> {
  const res = await fetch(`${API}/api/workbooks/${workbookId}/budget`, {
    method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ max_usd: maxUsd }),
  })
  if (!res.ok) throw new Error("Failed to set budget")
  return res.json()
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
