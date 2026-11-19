// Response shapes captured from the real Go API (nginx -> api-go -> postgres) by the first-run journey in
// e2e/real/. Values are small and deterministic. Where the real API returned an empty list on a fresh stack,
// the list is empty here too rather than guessing the element shape.
import { workbookId, workspace } from "./data"

const stamp = "2026-10-20T09:00:00Z"

// GET /api/records/summary. The overview page validates this strictly (lib/record-overview.ts). With no
// fixture the mock fell through to `[]`, which renders "The workspace summary returned an invalid response".
export const recordSummary = {
  companies: 3, people: 3, people_with_email: 3, verified_emails: 2, companies_needing_domain: 0,
  running_workbooks: 0, failed_workbooks: 0,
  workbooks: [
    { id: workbookId, name: "Q4 Logistics Prospects", status: "complete", total_rows: 3, completed_rows: 3, updated_at: stamp },
    { id: "wb-2", name: "Healthcare Champions", status: "draft", total_rows: 0, completed_rows: 0, updated_at: stamp }
  ],
  imports: [{ source: "clay:prospects", imported: 5, rejected: 0, created_at: stamp }],
  sampled_at: "2026-10-20T09:00:00+00:00"
}

// GET /api/analytics/{overview,collection,enrichment,llm,pipeline}. The analytics page counts a request with
// the wrong body shape as failed, so `[]` made the whole page "temporarily unavailable".
export const analyticsOverview = {
  total_leads: 3, leads_this_week: 3, leads_this_month: 3, avg_score: 78.0, tiers: {},
  enrichment: {
    total: 3, with_email: 3, with_phone: 1, with_website: 3, with_contact: 2, with_linkedin: 1,
    email_pct: 100, phone_pct: 33, website_pct: 100, contact_pct: 67
  },
  email_confidence: {},
  jobs: { total: 3, completed: 2, failed: 1, success_rate: 67 },
  records: { companies: 25, people: 40, people_with_email: 12, verified_emails: 5, companies_needing_domain: 1, email_pct: 30.0, verified_pct: 12.5 },
  activity: {
    window_days: 30, workbooks: 1, workbooks_run: 1, running_workbooks: 0, failed_workbooks: 0, cells_filled: 75, cells_no_data: 25, fill_rate_pct: 75.0,
    by_day: [{ date: "2026-11-18", filled: 0, no_data: 0 }, { date: "2026-11-19", filled: 75, no_data: 25 }]
  },
  providers: [
    { provider: "apollo", cells_filled: 50, share_pct: 66.7, paid_attempts: 50, spend_usd: 1.25 },
    { provider: "hunter", cells_filled: 25, share_pct: 33.3, paid_attempts: 0, spend_usd: 0 }
  ],
  spend: { window_days: 30, window_usd: 1.25, total_usd: 1.25, by_day: [{ date: "2026-11-19", usd: 1.25 }] },
  legacy: { present: true, leads: 3 }
}
export const analyticsCollection = { jobs_by_day: [], leads_by_day: [], avg_leads_per_job: 0 }
export const analyticsEnrichment = { source_quality: [], by_city: [], score_distribution: [] }
export const analyticsLlm = { by_day: [], by_provider: [], total_tokens: 0, total_calls: 0 }
export const analyticsPipeline = { statuses: {}, status_tiers: {} }
export const leadStats = {
  total: 3, by_status: {}, by_tier: {}, by_city: {}, by_source: {},
  enrichment: { total: 3, with_email: 3, with_phone: 1, with_website: 3, with_linkedin: 1, with_contact: 2, avg_score: 78 }
}
export const leadFilters = {
  cities: [], sources: [], statuses: ["new", "contacted", "qualified", "negotiating", "converted", "dead"],
  tiers: ["hot", "warm", "cold", "unqualified"]
}

// GET /api/settings/providers. Settings shows "Provider settings could not be verified" for any other shape.
const llmProvider = (id: string, name: string, model: string, isDefault: boolean) => ({
  id, name, icon: "", configured: false, api_key_masked: "", base_url: `https://${id}.example/v1`, model, default_model: model,
  docs_url: `https://${id}.example/keys`, free_tier: "Free tier", is_default: isDefault, openai_compatible: true
})
export const llmProviders = {
  default_provider: "openrouter",
  providers: [
    llmProvider("openrouter", "OpenRouter", "meta-llama/llama-3.3-70b-instruct:free", true),
    llmProvider("anthropic", "Anthropic (Claude)", "claude-opus-4-8", false)
  ]
}
export const llmUsage = { providers: [], today_calls: 0, today_tokens: 0, all_time_calls: 0, all_time_tokens: 0 }

const destinationType = (id: string, category: string, capabilities: string[]) => ({
  id, category, capabilities,
  required_checks: ["authentication", "external_write", "idempotency", "retry_recovery", "tenant_isolation"],
  maturity: "beta", certification: null
})
export const destinationTypes = {
  types: [destinationType("webhook", "activation", ["outbound"]), destinationType("hubspot", "crm", ["outbound", "inbound"])]
}
export const destinationAccess = { can_write: true }

export const outreachSequences = { sequences: [] }
export const outreachDrafts = { drafts: [], limit: 50, offset: 0 }
export const smtpStatus = { configured: false, host: null, email: null, from_name: "OpenGTM", max_per_hour: 50 }
export const recordWorkflow = { name: "Default enrichment", columns: [] }
export const meContext = { user_id: "1", workspace_id: workspace.id, role: "owner", is_owner: true }
export const workspaceDashboard = {
  total_leads: 3, total_workspaces: 1,
  workspaces: [{ id: workspace.id, name: workspace.name, slug: workspace.slug, icon: workspace.icon, leads_count: 3, created_at: 1760950800 }]
}
export const conversations = { conversations: [] }
