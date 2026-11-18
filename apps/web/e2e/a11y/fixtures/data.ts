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

export const runEstimate = { unknown_providers: [], catalog_complete: true, rows: 3, worst_usd: 0, best_usd: 0, breakdown: [] }

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
