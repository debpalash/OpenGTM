const API_BASE = ""

export interface Lead {
  id: number
  company: string
  website: string
  email: string
  phone: string
  contact_person: string
  contact_title: string
  city: string
  state: string
  specialization: string
  company_size: string
  description: string
  linkedin_url: string
  twitter_url: string
  source: string
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

export function exportCSVUrl(params: Record<string, string> = {}): string {
  const qs = new URLSearchParams(params).toString()
  return `${API_BASE}/api/export/csv?${qs}`
}

// ── Collection & Jobs ──────────────────────────────────────────

export interface Job {
  id: string
  query: string
  status: string
  tier: number
  attempts: number
  max_attempts: number
  leads_found: number
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

export async function submitCollect(query: string): Promise<{ ok: boolean; job_id: string; query: string }> {
  const res = await fetch(`${API_BASE}/api/collect`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ query }),
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

