import * as fx from "./data"
import * as real from "./real-shapes"

/** Exact-path GET fixtures (trailing slashes are normalised away). */
const exact: Record<string, unknown> = {
  "/auth/me": fx.me,
  "/auth/setup/status": { setup_required: false, setup_token_required: false },
  "/api/workspaces": fx.workspaces,
  "/api/workspaces/switch": { ok: true },
  "/api/records/company": fx.companyRecords,
  "/api/records/person": fx.personRecords,
  "/api/records/access": { can_write: true },
  "/api/workbooks": fx.workbooks,
  [`/api/workbooks/${fx.workbookId}`]: fx.workbookDetail,
  [`/api/v2/workbooks/${fx.workbookId}/views`]: { views: [], total: 0 },
  [`/api/workbooks/${fx.workbookId}/cost`]: fx.workbookCost,
  [`/api/workbooks/${fx.workbookId}/run/estimate`]: fx.runEstimate,
  [`/api/workbooks/${fx.workbookId}/connector-runs`]: { runs: [], limit: 20, offset: 0, has_more: false, next_cursor: null },
  "/api/signals/analytics": fx.signalAnalytics,
  "/api/signals/sources": { sources: [] },
  "/api/workbooks/meta/providers": fx.providers,
  "/api/workbooks/meta/ai-column-presets": { presets: [], categories: [] },
  "/api/workbooks/meta/lead-fields": { fields: ["company", "website", "email", "title"] },
  "/api/jobs": fx.jobs,
  "/api/research-playbooks": fx.researchPlaybooks,
  "/api/research-playbooks/pb-1/runs": { runs: [], limit: 20, offset: 0, has_more: false, next_cursor: null },
  "/api/flags": fx.featureFlags,
  "/api/watches": fx.watches,
  "/api/signals": fx.signals,
  "/api/templates/gallery": fx.galleryRecipes,
  "/api/leads": fx.emailLeads,
  "/api/onboarding": fx.onboarding,
  // Shapes captured from the real API (fixtures/real-shapes.ts).
  "/api/records/summary": real.recordSummary,
  "/api/records/company/workflow": real.recordWorkflow,
  "/api/records/person/workflow": real.recordWorkflow,
  "/api/analytics/overview": real.analyticsOverview,
  "/api/analytics/collection": real.analyticsCollection,
  "/api/analytics/enrichment": real.analyticsEnrichment,
  "/api/analytics/llm": real.analyticsLlm,
  "/api/analytics/pipeline": real.analyticsPipeline,
  "/api/stats": real.leadStats,
  "/api/filters": real.leadFilters,
  "/api/settings/providers": real.llmProviders,
  "/api/settings/llm-usage": real.llmUsage,
  "/api/audience-destinations/types": real.destinationTypes,
  "/api/audience-destinations/access": real.destinationAccess,
  "/api/outreach/sequences": real.outreachSequences,
  "/api/outreach/drafts": real.outreachDrafts,
  "/api/outreach/smtp/status": real.smtpStatus,
  "/api/me/context": real.meContext,
  "/api/workspaces/dashboard": real.workspaceDashboard,
  "/api/copilotkit/conversations": real.conversations
}

export const fixturePaths = Object.keys(exact)

export function normalise(path: string): string {
  return path.length > 1 ? path.replace(/\/+$/, "") : path
}

export function fixtureFor(path: string): unknown {
  return exact[normalise(path)]
}

/** Safe default for every unmatched endpoint: 200 with an empty list. */
export function defaultFor(_path: string): unknown {
  return []
}
