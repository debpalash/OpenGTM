// Small deterministic fixtures for the a11y gate. No real data.
export const workspace = { id: "ws-1", name: "Acme GTM", slug: "acme-gtm", icon: "A", is_active: true }

export const me = { id: 1, username: "admin", is_admin: true, role: "global_admin", email: "admin@example.com" }

export const workspaces = { workspaces: [workspace], active_id: workspace.id }

const stamp = "2026-10-20T09:00:00Z"

const companyRows = [
  ["c1", "Northwind Logistics", "northwind.example", "Logistics", 450, "Rotterdam, NL"],
  ["c2", "Globex Software", "globex.example", "Software", 120, "Austin, US"],
  ["c3", "Initech Health", "initech.example", "Healthcare", 900, "Leeds, UK"]
] as const

const personRows = [
  ["p1", "Ada Quinn", "VP Sales", "Northwind Logistics", "ada@northwind.example"],
  ["p2", "Ben Ortiz", "Head of Growth", "Globex Software", "ben@globex.example"],
  ["p3", "Chen Wu", "CTO", "Initech Health", "chen@initech.example"]
] as const

export const companyRecords = {
  records: companyRows.map(([id, name, domain, industry, employees, location]) => ({
    id, record_type: "company", version: 1, updated_at: stamp, company_id: id, person_id: null, employment_id: null,
    fields: { name, domain, industry, employee_count: employees, location }
  })),
  total: companyRows.length, limit: 50, offset: 0
}

export const personRecords = {
  records: personRows.map(([id, name, title, company, email]) => ({
    id, record_type: "person", version: 1, updated_at: stamp, company_id: null, person_id: id, employment_id: null,
    fields: { name, title, company, email }
  })),
  total: personRows.length, limit: 50, offset: 0
}

export const workbookId = "wb-1"

const workbook = (id: string, name: string, rows: number) => ({
  id, name, description: "", status: "complete", source_type: "empty", source_config: null, filter_criteria: null,
  columns_config: [
    { id: "name", name: "Company", type: "lead_field", width: 220, lead_field: "company" },
    { id: "domain", name: "Domain", type: "lead_field", width: 180, lead_field: "website" }
  ],
  total_rows: rows, completed_rows: rows, created_at: stamp, updated_at: stamp, last_run_at: null
})

export const workbooks = { workbooks: [workbook(workbookId, "Q4 Logistics Prospects", 3), workbook("wb-2", "Healthcare Champions", 0)], total: 2 }

export const workbookDetail = {
  workbook: workbook(workbookId, "Q4 Logistics Prospects", 3),
  rows: companyRows.map(([, name, domain], i) => ({
    lead_id: i + 1, row_id: i + 1, position: i, lead: { company: name, website: domain }, data: {}, enrichments: {}
  })),
  total_rows: 3, page: 1, page_size: 1000, has_more: false, next_cursor: null
}

export const workbookCost = {
  budget_max_usd: 0, budget_spent_usd: 0, reserved_usd: 0, uncertain_usd: 0, unlimited: true,
  remaining_usd: null, accounting_basis: "catalog_estimates_and_recorded_charges"
}

export const runEstimate = { unknown_providers: [], catalog_complete: true, rows: 3, worst_usd: 0, best_usd: 0, breakdown: [], note: "Catalog estimate only." }

export const providers = {
  providers: [
    { name: "apollo", capabilities: ["email", "company"], confidence: 0.8, maturity: "supported" },
    { name: "hunter", capabilities: ["email"], confidence: 0.7, maturity: "beta" }
  ]
}

export const signalAnalytics = {
  summary: { total: 12, weighted_score: 48, active_accounts: 3, momentum_pct: 20 },
  trend: [
    { date: "2026-10-18", weight: 10 },
    { date: "2026-10-19", weight: 18 },
    { date: "2026-10-20", weight: 20 }
  ],
  by_type: [
    { signal_type: "hiring", weight: 24 },
    { signal_type: "funding", weight: 24 }
  ],
  top_accounts: [{ lead_id: 1, company: "Northwind Logistics", count: 5, weight: 20 }]
}

// /agents, /notifications (failed task), /campaigns (leads with emails).
const jobRow = (id: string, query: string, status: string, leads: number) => ({
  id, query, intent: "companies", status, tier: 1, attempts: 1, max_attempts: 3, leads_found: leads,
  workspace_id: "ws-1", proxy_used: "", error: status === "failed" ? "Provider timeout" : "",
  created_at: stamp, started_at: stamp, completed_at: status === "running" ? "" : stamp
})
export const jobs = [
  jobRow("job-1", "Logistics companies in Rotterdam", "done", 12),
  jobRow("job-2", "Healthcare SaaS in Leeds", "running", 3),
  jobRow("job-3", "Fintech founders in Austin", "failed", 0)
]

export const researchPlaybooks = [{
  id: "pb-1", name: "Funding research", description: "Find the latest funding round",
  prompt_template: "Find the latest funding round for {company}.", steps: [], output_format: "text", max_steps: 3,
  cell_budget_usd: 0.1, version: 1, enabled: true, schedule_audience_id: null, schedule_interval_minutes: null,
  next_run_at: null, created_at: stamp, updated_at: stamp
}]

export const featureFlags = { automations_enabled: true, intent_poller_enabled: true, pg_lead_store: true, allow_legacy_outreach: false }

export const watches = {
  watches: [{
    id: "w-1", workspace_id: "ws-1", kind: "sec_filings", target: "Northwind Logistics", resolved_cik: null, lead_id: 1,
    signal_types: ["filing"], interval: "daily", enabled: true, next_poll_at: stamp, last_polled_at: stamp,
    last_error: null, consecutive_failures: 0, cursor: {}, created_at: stamp, state: "active"
  }],
  limit: 50, offset: 0, has_more: false, next_cursor: null
}

export const signals = {
  signals: [{
    id: "s-1", workspace_id: "ws-1", lead_id: 1, company: "Northwind Logistics", signal_type: "funding",
    title: "Northwind raised a Series B", description: "Announced a new funding round.", source: "news",
    source_url: null, weight: 9, created_at: 1760950800, read: 0
  }],
  total: 1
}

export const galleryRecipes = {
  recipes: [
    { slug: "lead-enrichment", name: "Lead enrichment", description: "Enrich a lead list with email and company data.", category: "enrichment", icon: "rocket", columns_count: 4, column_types: { lead_field: 2, enrichment: 2 } },
    { slug: "intent-scoring", name: "Intent scoring", description: "Score accounts by recent buying signals.", category: "scoring", icon: "star", columns_count: 3, column_types: { ai: 3 } }
  ],
  categories: {
    enrichment: { label: "Enrichment", icon: "rocket", color: "blue" },
    scoring: { label: "Scoring", icon: "star", color: "amber" }
  },
  total: 2
}

export const emailLeads = [
  { id: 1, company: "Northwind Logistics", email: "ada@northwind.example", score: 82 },
  { id: 2, company: "Globex Software", email: "ben@globex.example", score: 74 }
]

// First-run checklist on /overview: two steps done, the rest open, no sample yet, so the card renders both states.
export const onboarding = {
  steps: [
    { id: "llm_provider", done: true },
    { id: "data_source", done: true },
    { id: "data", done: false },
    { id: "enrichment", done: false }
  ],
  done_count: 2,
  total: 4,
  complete: false,
  sample_workbook_id: null
}
