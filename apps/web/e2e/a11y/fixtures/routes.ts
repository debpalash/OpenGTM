import * as fx from "./data"

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
  "/api/leads": fx.emailLeads
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
