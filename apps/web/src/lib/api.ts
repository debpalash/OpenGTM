const API_BASE = ""

export interface Lead {
  id: number
  company: string
  website: string
  email: string
  email_confidence: string
  email_provider: string
  phone: string
  phone_provider: string
  contact_person: string
  contact_title: string
  city: string
  state: string
  address: string
  specialization: string
  company_size: string
  employee_count_exact: number
  description: string
  revenue_range: string
  founded_year: string
  industry_tags: string
  technologies: string
  funding_stage: string
  linkedin_url: string
  twitter_url: string
  facebook_url: string
  secondary_emails: string
  secondary_phones: string
  decision_makers: string
  glassdoor_rating: string
  hiring_signals: string
  enrichment_attempts: number
  enrichment_waterfall: string
  source: string
  workspace_id: string
  score: number
  score_tier: string
  status: string
  yupcha_value_prop: string
  company_need: string
  notes: string
  created_at: string
  updated_at: string
  last_enriched_at: string
}

export interface Stats {
  total: number
  by_status: Record<string, number>
  by_tier: Record<string, number>
  by_city: Record<string, number>
  by_source: Record<string, number>
  enrichment: {
    total: number
    with_email: number
    with_phone: number
    with_website: number
    with_linkedin: number
    with_contact: number
    avg_score: number
  }
}

export interface Filters {
  cities: string[]
  sources: string[]
  statuses: string[]
  tiers: string[]
}

export interface Workspace {
  id: string
  name: string
  description: string
  lead_count: number
  job_count: number
  active_lead_count: number
  created_at: string
  updated_at: string
}

export interface Job {
  id: string
  query: string
  status: string
  tier: number
  attempts: number
  max_attempts: number
  leads_found: number
  workspace_id: string
  proxy_used: string
  error: string
  created_at: string
  started_at: string
  completed_at: string
}

export interface SystemStats {
  proxy_pool: {
    total: number
    http: number
    socks5: number
    socks4: number
    blocked: number
    domain_assignments: number
  }
  rate_limiter: {
    tracked_domains: number
    tripped_domains: string[]
  }
  jobs: {
    total: number
    pending: number
    running: number
    done: number
    failed: number
  }
}

// ── Lead CRUD ───────────────────────────────────────────────────

export async function fetchLeads(params: Record<string, string>): Promise<Lead[]> {
  const qs = new URLSearchParams(params).toString()
  const res = await fetch(`${API_BASE}/api/leads?${qs}`)
  return res.json()
}

export async function fetchStats(): Promise<Stats> {
  const res = await fetch(`${API_BASE}/api/stats`)
  return res.json()
}

export async function fetchFilters(): Promise<Filters> {
  const res = await fetch(`${API_BASE}/api/filters`)
  return res.json()
}

export async function fetchLead(id: number): Promise<Lead> {
  const res = await fetch(`${API_BASE}/api/lead/${id}`)
  if (!res.ok) throw new Error(`Lead ${id} not found (${res.status})`)
  return res.json()
}

export interface SimilarLeads {
  reference: string
  count: number
  similar_leads: Lead[]
}

export async function fetchSimilarLeads(id: number, limit = 10): Promise<SimilarLeads> {
  const res = await fetch(`${API_BASE}/api/lead/${id}/similar?limit=${limit}`)
  if (!res.ok) throw new Error(`Similar lookup failed (${res.status})`)
  return res.json()
}

export async function updateStatus(id: number, status: string, note = ""): Promise<void> {
  await fetch(`${API_BASE}/api/lead/${id}/status`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ status, note }),
  })
}

export async function updateLead(id: number, fields: Partial<Lead>): Promise<void> {
  await fetch(`${API_BASE}/api/lead/${id}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(fields),
  })
}

export async function deleteLead(id: number): Promise<void> {
  await fetch(`${API_BASE}/api/lead/${id}`, { method: "DELETE" })
}

export async function addLead(data: Partial<Lead>): Promise<{ ok: boolean; id: number }> {
  const res = await fetch(`${API_BASE}/api/lead`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(data),
  })
  return res.json()
}

export type EnrichAction = "web_research" | "find_emails" | "scrape_website" | "find_phone" | "find_address"

export async function enrichLead(
  id: number,
  action: EnrichAction,
  onEvent: (event: Record<string, unknown>) => void,
): Promise<void> {
  const res = await fetch(`${API_BASE}/api/lead/${id}/enrich?action=${action}`, {
    method: "POST",
  })
  if (!res.ok || !res.body) throw new Error(`Enrich failed: ${res.status}`)

  const reader = res.body.getReader()
  const decoder = new TextDecoder()
  let buffer = ""

  while (true) {
    const { done, value } = await reader.read()
    if (done) break
    buffer += decoder.decode(value, { stream: true })
    const lines = buffer.split("\n")
    buffer = lines.pop() || ""
    for (const line of lines) {
      if (line.startsWith("data: ")) {
        try {
          const data = JSON.parse(line.slice(6))
          onEvent(data)
        } catch { /* skip */ }
      }
    }
  }
}

export function exportCSVUrl(params: Record<string, string> = {}): string {
  const qs = new URLSearchParams(params).toString()
  return `${API_BASE}/api/export/csv?${qs}`
}

// ── Data Collector Import ───────────────────────────────────

export async function importDataCollector(
  module: string = "all",
  limit?: number,
): Promise<{ ok: boolean; job_id: string; module: string; limit: number | null; message: string }> {
  const params = new URLSearchParams({ module })
  if (limit) params.set("limit", String(limit))
  const res = await fetch(`${API_BASE}/api/leads/import/data-collector?${params}`, {
    method: "POST",
  })
  return res.json()
}

// ── Collection & Jobs ───────────────────────────────────────────

export async function submitCollect(query: string, workspace_id?: string): Promise<{ ok: boolean; job_id: string; query: string }> {
  const res = await fetch(`${API_BASE}/api/collect`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ query, workspace_id: workspace_id || "" }),
  })
  return res.json()
}

export async function fetchJobs(status?: string): Promise<Job[]> {
  const qs = status ? `?status=${status}` : ""
  const res = await fetch(`${API_BASE}/api/jobs${qs}`)
  return res.json()
}

export async function fetchSystemStats(): Promise<SystemStats> {
  const res = await fetch(`${API_BASE}/api/system-stats`)
  return res.json()
}

// ── Workspaces ──────────────────────────────────────────────────

export async function fetchWorkspaces(): Promise<Workspace[]> {
  const res = await fetch(`${API_BASE}/api/workspaces`)
  return res.json()
}

export async function createWorkspace(name: string, description?: string): Promise<{ ok: boolean; id: string }> {
  const res = await fetch(`${API_BASE}/api/workspaces`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ name, description: description || "" }),
  })
  return res.json()
}

export async function deleteWorkspace(id: string): Promise<void> {
  await fetch(`${API_BASE}/api/workspaces/${id}`, { method: "DELETE" })
}

// ── Chat / Conversations ────────────────────────────────────────

export interface Conversation {
  id: string
  title: string
  created_at: string
  updated_at: string
}

export interface ChatMessage {
  id: string
  conversation_id: string
  role: "user" | "assistant" | "system" | "tool"
  content: string
  tool_data: string | null
  created_at: string
}

export async function fetchConversations(): Promise<Conversation[]> {
  const res = await fetch(`${API_BASE}/api/copilotkit/conversations`)
  const data = await res.json()
  return data.conversations || []
}

export async function fetchConversationMessages(id: string): Promise<{ conversation: Conversation; messages: ChatMessage[] }> {
  const res = await fetch(`${API_BASE}/api/copilotkit/conversations/${id}`)
  return res.json()
}

export async function deleteConversation(id: string): Promise<void> {
  await fetch(`${API_BASE}/api/copilotkit/conversations/${id}`, { method: "DELETE" })
}

// Verbatim OpenAI-style tool call echoed by the server in a confirmation event.
export interface ToolCall {
  id: string
  type: string
  function: { name: string; arguments: string }
}

export interface ApprovedToolCall {
  tool_call: ToolCall
  decision: "approve" | "deny"
}

export interface ChatStreamEvent {
  conversation_id?: string
  content?: string
  tool_call?: { name: string; args: Record<string, unknown> }
  tool_result?: { name: string; result: Record<string, unknown> }
  tool_denied?: { name: string }
  confirmation_required?: {
    confirmation_id: string
    tool_call: ToolCall
    name: string
    args: Record<string, unknown>
    description: string
    level: "high" | "medium" | "low"
    label: string
  }
  awaiting_confirmation?: boolean
  warning?: string
  error?: string
}

export interface StreamChatOpts {
  signal?: AbortSignal
  approvedToolCalls?: ApprovedToolCall[]
}

export async function streamChat(
  messages: Array<{ role: string; content: string }>,
  conversationId: string | null,
  onEvent: (event: ChatStreamEvent) => void,
  opts: StreamChatOpts = {},
): Promise<void> {
  const res = await fetch(`${API_BASE}/api/copilotkit`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    signal: opts.signal,
    body: JSON.stringify({
      messages,
      conversation_id: conversationId,
      ...(opts.approvedToolCalls?.length ? { approved_tool_calls: opts.approvedToolCalls } : {}),
    }),
  })
  if (!res.ok || !res.body) throw new Error(`Chat failed: ${res.status}`)

  const reader = res.body.getReader()
  const decoder = new TextDecoder()
  let buffer = ""

  while (true) {
    const { done, value } = await reader.read()
    if (done) break
    buffer += decoder.decode(value, { stream: true })
    const lines = buffer.split("\n")
    buffer = lines.pop() || ""
    for (const line of lines) {
      if (line.startsWith("data: ")) {
        const raw = line.slice(6)
        if (raw === "[DONE]") return
        try {
          onEvent(JSON.parse(raw))
        } catch { /* skip */ }
      }
    }
  }
}

// ── Analytics ────────────────────────────────────────────────────

export interface AnalyticsOverview {
  total_leads: number
  leads_this_week: number
  leads_this_month: number
  avg_score: number
  tiers: Record<string, number>
  enrichment: {
    total: number
    with_email: number
    with_phone: number
    with_website: number
    with_contact: number
    with_linkedin: number
    email_pct: number
    phone_pct: number
    website_pct: number
    contact_pct: number
  }
  email_confidence: Record<string, number>
  jobs: {
    total: number
    completed: number
    failed: number
    success_rate: number
  }
}

export interface AnalyticsPipeline {
  statuses: Record<string, number>
  status_tiers: Record<string, Record<string, number>>
}

export interface AnalyticsCollection {
  jobs_by_day: Array<{ day: string; jobs: number; leads: number; completed: number }>
  leads_by_day: Array<{ day: string; count: number }>
  avg_leads_per_job: number
}

export interface AnalyticsEnrichment {
  source_quality: Array<{ source: string; count: number; avg_score: number; with_email: number; with_phone: number; with_contact: number }>
  by_city: Array<{ city: string; count: number; avg_score: number }>
  score_distribution: Array<{ range: string; count: number }>
}

export interface AnalyticsLLM {
  by_day: Array<{ day: string; tokens: number; calls: number; providers: string }>
  by_provider: Array<{ provider: string; tokens: number; calls: number }>
  total_tokens: number
  total_calls: number
}

export const fetchAnalyticsOverview = (): Promise<AnalyticsOverview> =>
  fetch(`${API_BASE}/api/analytics/overview`).then(r => r.json())

export const fetchAnalyticsPipeline = (): Promise<AnalyticsPipeline> =>
  fetch(`${API_BASE}/api/analytics/pipeline`).then(r => r.json())

export const fetchAnalyticsCollection = (): Promise<AnalyticsCollection> =>
  fetch(`${API_BASE}/api/analytics/collection`).then(r => r.json())

export const fetchAnalyticsEnrichment = (): Promise<AnalyticsEnrichment> =>
  fetch(`${API_BASE}/api/analytics/enrichment`).then(r => r.json())

export const fetchAnalyticsLLM = (): Promise<AnalyticsLLM> =>
  fetch(`${API_BASE}/api/analytics/llm`).then(r => r.json())
