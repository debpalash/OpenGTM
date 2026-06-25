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

export interface DedupSuggestion {
  master_id: number
  master_company: string
  duplicate_ids: number[]
  duplicate_companies: string[]
  confidence: number
}
export interface DedupResult {
  stats: { total_leads: number; duplicates_found: number; clusters: number }
  merge_suggestions: DedupSuggestion[]
  scanned?: number
}

/** Run dedup over a bounded, filtered subset (city/source/tier/search). */
export async function runDedup(params: Record<string, string>): Promise<DedupResult> {
  const qs = new URLSearchParams(params).toString()
  const res = await fetch(`${API_BASE}/api/leads/dedup?${qs}`, { method: "POST" })
  if (!res.ok) throw new Error(`Dedup failed (${res.status})`)
  return res.json()
}

export async function bulkEnrich(
  lead_ids: number[],
  action: "find_emails" | "scrape_website",
): Promise<{ ok: boolean; job_id: string; count: number; action: string }> {
  const res = await fetch(`${API_BASE}/api/leads/bulk-enrich`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ lead_ids, action }),
  })
  if (!res.ok) throw new Error(`Bulk enrich failed (${res.status})`)
  return res.json()
}

export async function mergeDuplicates(master_id: number, duplicate_ids: number[]): Promise<void> {
  const res = await fetch(`${API_BASE}/api/leads/dedup/merge`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ master_id, duplicate_ids }),
  })
  if (!res.ok) throw new Error(`Merge failed (${res.status})`)
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

// ════════════════════════════════════════════════════════════════════════════
// GTM Automation UI — Outreach · Automations · Watches + meta (role/flags)
//
// All fetch fns below route errors through `ApiError`/`jsonOrThrow` so 4xx/5xx
// `detail` strings surface to the UI instead of being swallowed. No auth or
// workspace headers are set here — the fetch interceptor in `auth.ts` attaches
// Authorization + X-Workspace-Id to every `/api/*` request.
// ════════════════════════════════════════════════════════════════════════════

/** Typed error carrying the backend `detail` + HTTP status for reactive gating. */
export class ApiError extends Error {
  status: number
  detail: string
  body?: unknown
  constructor(status: number, detail: string, body?: unknown) {
    super(detail)
    this.name = "ApiError"
    this.status = status
    this.detail = detail
    this.body = body
  }
}

async function jsonOrThrow<T>(res: Response): Promise<T> {
  const data = await res.json().catch(() => ({}))
  if (!res.ok) {
    const detail =
      (data as { detail?: unknown })?.detail
    throw new ApiError(
      res.status,
      typeof detail === "string" ? detail : `HTTP ${res.status}`,
      data,
    )
  }
  return data as T
}

const JSON_HEADERS = { "Content-Type": "application/json" }

/** GET helper. */
function apiGet<T>(path: string): Promise<T> {
  return fetch(`${API_BASE}${path}`).then(jsonOrThrow<T>)
}

/** Mutation helper (POST/PUT/PATCH/DELETE) with JSON body + error surfacing. */
function apiSend<T>(
  path: string,
  method: "POST" | "PUT" | "PATCH" | "DELETE",
  body?: unknown,
): Promise<T> {
  return fetch(`${API_BASE}${path}`, {
    method,
    headers: JSON_HEADERS,
    ...(body !== undefined ? { body: JSON.stringify(body) } : {}),
  }).then(jsonOrThrow<T>)
}

// ── Meta: per-workspace role + feature flags ────────────────────────────────

export type WorkspaceRole = "owner" | "admin" | "editor" | "member"

export interface MeContext {
  user_id: string
  workspace_id: string
  role: WorkspaceRole
  is_owner: boolean
}

export interface FeatureFlags {
  automations_enabled: boolean
  intent_poller_enabled: boolean
  pg_lead_store: boolean
  allow_legacy_outreach: boolean
}

export const fetchMeContext = (): Promise<MeContext> =>
  apiGet<MeContext>("/api/me/context")

export const fetchFlags = (): Promise<FeatureFlags> =>
  apiGet<FeatureFlags>("/api/flags")

// ════════════════════════════ Outreach types ═══════════════════════════════

export interface SeqStep {
  step_number: number
  subject: string
  body_html: string
  delay_hours: number
}

export interface Sequence {
  id: string
  workspace_id: string
  name: string
  description: string
  steps: SeqStep[]
  status: string
  daily_limit: number
  send_window_start: number
  send_window_end: number
  send_window_tz: string
  consent_basis: string
  bounce_count: number
  complaint_count: number
  auto_paused: boolean
  created_at: string
  updated_at: string
}

// Known per-status keys are explicit (the loose index sig only covers extras),
// so a typo'd known stat key fails typecheck.
export interface SeqStats {
  total: number
  emails_sent: number
  bounce_count: number
  complaint_count: number
  auto_paused: boolean
  pending?: number
  scheduled?: number
  sent?: number
  opened?: number
  replied?: number
  bounced?: number
  failed?: number
  skipped?: number
  suppressed?: number
  completed?: number
  [status: string]: number | boolean | undefined
}

export interface SeqSend {
  id: string
  sequence_id: string
  enrollment_id: string
  lead_id: number
  step_number: number
  to_email: string
  subject: string
  status: string
  skip_reason: string | null
  message_id: string | null
  charged_usd: number
  migrated: boolean
  error: string | null
  sent_at: string | null
  created_at: string
}

export interface Suppression {
  id: string
  email: string
  reason: string
  source: string
  locked: boolean
  created_at: string
}

export interface SmtpStatus {
  configured: boolean
  host: string | null
  email: string | null
  from_name: string
  max_per_hour: number
}

export interface CreateSequenceRequest {
  name: string
  description?: string
  steps: SeqStep[]
  daily_limit?: number
  send_window_start?: number
  send_window_end?: number
  send_window_tz?: string
  consent_basis: string
}

export interface EnrollLeadsRequest {
  lead_ids: number[]
  consent_source: string
}

export interface EnrollResult {
  enrolled: number
  skipped: { lead_id: number; reason: string }[]
  total_lead_ids: number
}

// ═══════════════════════════ Automations types ═════════════════════════════

export type TriggerType =
  | "on_signal"
  | "on_row_changed"
  | "on_row_added"
  | "on_schedule"

export type ActionType =
  | "re_enrich"
  | "push_crm"
  | "webhook"
  | "sequencer"
  | "send_email"

export interface RuleAction {
  type: ActionType
  config: Record<string, unknown>
}

export interface Trigger {
  id: string
  workspace_id: string
  name: string
  enabled: boolean
  trigger_type: TriggerType
  trigger_config: Record<string, unknown>
  condition: string
  actions: RuleAction[]
  scope_workbook_ids: string[]
  stop_on_error: boolean
  max_spend_usd_per_day: number | null
  max_actions_per_day: number | null
  next_run_at: string | null
  schedule_anchor: string | null
  last_fired_at: string | null
  created_by: string
  created_at: string
  updated_at: string
}

export interface TriggerRun {
  id: string
  trigger_id: string
  trigger_name_snapshot: string
  job_id: string | null
  status: string
  fire_source: string
  fire_key: string
  matched_rows: number
  actions_attempted: number
  actions_succeeded: number
  actions_skipped: number
  actions_failed: number
  total_charged_usd: number
  error: string | null
  started_at: string | null
  finished_at: string | null
}

export interface ActionResult {
  id: string
  run_id: string
  trigger_id: string
  workbook_id: string
  row_id: string
  lead_id: number | null
  action_index: number
  action_type: ActionType
  status: string
  skip_reason: string | null
  charged_usd: number
  result_summary: string
  error: string | null
  created_at: string
}

export interface PreviewResult {
  matched: number
  projected_total_usd: number
  sample: {
    row_id: string
    workbook_id: string
    condition_pass: boolean
    actions: {
      type: ActionType
      would_charge_usd: number
      resolved_payload_preview: unknown
    }[]
  }[]
}

/** `POST /run` is async and returns NO run_id — resolve the run via fire_key. */
export interface RunHandle {
  job_id: string
  fire_key: string
  dry_run: boolean
}

export interface CreateTriggerRequest {
  name: string
  trigger_type: TriggerType
  trigger_config?: Record<string, unknown>
  condition?: string
  actions: RuleAction[]
  scope_workbook_ids?: string[]
  stop_on_error?: boolean
  max_spend_usd_per_day?: number | null
  max_actions_per_day?: number | null
}

// ═══════════════════════════════ Watches types ═════════════════════════════

export type WatchKind = "funding" | "hiring" | "feed" | "company"
export type WatchInterval = "hourly" | "daily" | "weekly"

export interface Watch {
  id: string
  workspace_id: string
  kind: WatchKind
  target: string
  resolved_cik: string | null
  lead_id: number | null
  signal_types: string[]
  interval: WatchInterval
  enabled: boolean
  next_poll_at: string | null
  last_polled_at: string | null
  last_error: string | null
  consecutive_failures: number
  cursor: Record<string, unknown>
  created_at: string
}

// `/signals` returns the FULL signal dict; created_at is a NUMBER (epoch),
// read is 0|1 — matching signals.tsx.
export interface WatchSignal {
  id: string
  workspace_id: string
  lead_id: number | null
  company: string
  signal_type: string
  title: string
  description: string
  source: string
  source_url: string | null
  weight: number
  created_at: number
  read: number
}

export interface WatchCreate {
  kind: WatchKind
  target: string
  lead_id?: number
  signal_types?: string[]
  interval?: WatchInterval
  create_webhook_rule?: boolean
  webhook_url?: string
  webhook_secret_ref?: string
}

export interface PollResult {
  status: "queued" | "already_queued"
  fire_key: string
  job_id?: string
}

// ═══════════════════════════ Outreach fetch fns ════════════════════════════

const OUT = "/api/outreach"

export const listSequences = (): Promise<{ sequences: Sequence[] }> =>
  apiGet(`${OUT}/sequences`)

export const createSequence = (
  b: CreateSequenceRequest,
): Promise<{ id: string; name: string; status: string }> =>
  apiSend(`${OUT}/sequences`, "POST", b)

export const getSequence = (
  id: string,
): Promise<Sequence & { stats: SeqStats }> =>
  apiGet(`${OUT}/sequences/${id}`)

export const updateSequence = (
  id: string,
  b: Partial<CreateSequenceRequest> & { status?: string },
): Promise<{ status: string; id: string }> =>
  apiSend(`${OUT}/sequences/${id}`, "PUT", b)

export const deleteSequence = (id: string): Promise<{ status: string }> =>
  apiSend(`${OUT}/sequences/${id}`, "DELETE")

export const startSequence = (id: string): Promise<{ status: string }> =>
  apiSend(`${OUT}/sequences/${id}/start`, "POST")

export const pauseSequence = (id: string): Promise<{ status: string }> =>
  apiSend(`${OUT}/sequences/${id}/pause`, "POST")

export const enrollLeads = (
  id: string,
  b: EnrollLeadsRequest,
): Promise<EnrollResult> =>
  apiSend(`${OUT}/sequences/${id}/enroll`, "POST", b)

export const executeSequence = (
  id: string,
): Promise<{ enqueued: number }> =>
  apiSend(`${OUT}/sequences/${id}/execute`, "POST")

export const getSequenceStats = (id: string): Promise<SeqStats> =>
  apiGet(`${OUT}/sequences/${id}/stats`)

export const listSends = (id: string): Promise<{ sends: SeqSend[] }> =>
  apiGet(`${OUT}/sequences/${id}/sends`)

export const getSmtpStatus = (): Promise<SmtpStatus> =>
  apiGet(`${OUT}/smtp/status`)

export const updateSmtp = (
  b: Record<string, unknown>,
): Promise<{ status: string }> =>
  apiSend(`${OUT}/smtp/config`, "PUT", b)

export const testSmtp = (
  to_email: string,
): Promise<{ status: string; message: string }> =>
  apiSend(`${OUT}/smtp/test`, "POST", { to_email })

export const listSuppressions = (): Promise<{ suppressions: Suppression[] }> =>
  apiGet(`${OUT}/suppressions`)

export const addSuppression = (
  email: string,
): Promise<{ status: string; added: boolean }> =>
  apiSend(`${OUT}/suppressions`, "POST", { email })

export const removeSuppression = (
  email: string,
): Promise<{ status: string }> =>
  apiSend(`${OUT}/suppressions/${encodeURIComponent(email)}`, "DELETE")

// ══════════════════════════ Automations fetch fns ══════════════════════════

const AUTO = "/api/automations"

export const listTriggers = (p?: {
  enabled?: boolean
  trigger_type?: TriggerType
}): Promise<Trigger[]> => {
  const qs = new URLSearchParams()
  if (p?.enabled !== undefined) qs.set("enabled", String(p.enabled))
  if (p?.trigger_type) qs.set("trigger_type", p.trigger_type)
  const suffix = qs.toString() ? `?${qs}` : ""
  return apiGet(`${AUTO}/triggers${suffix}`)
}

export const createTrigger = (b: CreateTriggerRequest): Promise<Trigger> =>
  apiSend(`${AUTO}/triggers`, "POST", b)

export const getTrigger = (
  id: string,
): Promise<Trigger & { recent_runs: TriggerRun[] }> =>
  apiGet(`${AUTO}/triggers/${id}`)

export const patchTrigger = (
  id: string,
  b: Partial<CreateTriggerRequest>,
): Promise<Trigger> =>
  apiSend(`${AUTO}/triggers/${id}`, "PATCH", b)

export const deleteTrigger = (id: string): Promise<{ deleted: boolean }> =>
  apiSend(`${AUTO}/triggers/${id}`, "DELETE")

export const pauseTrigger = (id: string): Promise<{ enabled: boolean }> =>
  apiSend(`${AUTO}/triggers/${id}/pause`, "POST")

export const resumeTrigger = (
  id: string,
): Promise<{ enabled: boolean; next_run_at: string | null }> =>
  apiSend(`${AUTO}/triggers/${id}/resume`, "POST")

export const previewTrigger = (
  id: string,
  b: { row_ids?: string[]; limit?: number },
): Promise<PreviewResult> =>
  apiSend(`${AUTO}/triggers/${id}/preview`, "POST", b)

export const runTrigger = (
  id: string,
  b: { row_ids?: string[]; dry_run: boolean },
): Promise<RunHandle> =>
  apiSend(`${AUTO}/triggers/${id}/run`, "POST", b)

export const listRuns = (
  id: string,
  limit?: number,
): Promise<TriggerRun[]> => {
  const suffix = limit ? `?limit=${limit}` : ""
  return apiGet(`${AUTO}/triggers/${id}/runs${suffix}`)
}

export const getRun = (
  runId: string,
): Promise<TriggerRun & { action_results: ActionResult[] }> =>
  apiGet(`${AUTO}/runs/${runId}`)

// ════════════════════════════ Watches fetch fns ════════════════════════════

const WATCH = "/api/watches"

export const listWatches = (p?: {
  limit?: number
  offset?: number
}): Promise<{ watches: Watch[]; limit: number; offset: number }> => {
  const qs = new URLSearchParams()
  if (p?.limit !== undefined) qs.set("limit", String(p.limit))
  if (p?.offset !== undefined) qs.set("offset", String(p.offset))
  const suffix = qs.toString() ? `?${qs}` : ""
  return apiGet(`${WATCH}${suffix}`)
}

export const createWatch = (
  b: WatchCreate,
): Promise<Watch & { webhook_rule_id?: string }> =>
  apiSend(`${WATCH}`, "POST", b)

export const getWatch = (id: string): Promise<Watch> =>
  apiGet(`${WATCH}/${id}`)

export const patchWatch = (
  id: string,
  b: Partial<WatchCreate> & { enabled?: boolean },
): Promise<Watch> =>
  apiSend(`${WATCH}/${id}`, "PATCH", b)

export const deleteWatch = (
  id: string,
): Promise<{ deleted: boolean; id: string }> =>
  apiSend(`${WATCH}/${id}`, "DELETE")

export const pollWatch = (id: string): Promise<PollResult> =>
  apiSend(`${WATCH}/${id}/poll`, "POST")

export const listWatchSignals = (
  id: string,
  p?: { signal_type?: string; limit?: number; offset?: number },
): Promise<{ signals: WatchSignal[] }> => {
  const qs = new URLSearchParams()
  if (p?.signal_type) qs.set("signal_type", p.signal_type)
  if (p?.limit !== undefined) qs.set("limit", String(p.limit))
  if (p?.offset !== undefined) qs.set("offset", String(p.offset))
  const suffix = qs.toString() ? `?${qs}` : ""
  return apiGet(`${WATCH}/${id}/signals${suffix}`)
}

/** signal_types each watch kind may emit (mirrors backend watches.py:41). */
export const WATCH_KIND_SIGNAL_TYPES: Record<WatchKind, string[]> = {
  funding: ["company_funded", "executive_hired"],
  hiring: ["hiring_surge", "new_tech_adopted"],
  feed: ["news"],
  company: [
    "company_funded",
    "executive_hired",
    "hiring_surge",
    "new_tech_adopted",
  ],
}

/** Action types offered in the builder by default (legacy types omitted). */
export const DEFAULT_ACTION_TYPES: ActionType[] = [
  "re_enrich",
  "push_crm",
  "webhook",
]
export const LEGACY_ACTION_TYPES: ActionType[] = ["sequencer", "send_email"]
