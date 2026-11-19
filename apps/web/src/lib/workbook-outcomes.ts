// What a cell or a run ended as. Mirrors apps/backend-go/internal/cellstate; both are tested against
// the shared table in apps/backend-go/internal/cellstate/testdata/outcomes.json.
export type CellOutcome = "filled" | "no_data" | "needs_setup" | "error" | "skipped"

export interface RunCounts { completed?: number; no_data?: number; needs_setup?: number; errors?: number }

const needsSetupMarkers = [
  "no llm provider is configured", "browser_service_unavailable", "no_providers_selected", "no_default_providers",
  "smtp_port_blocked", "provider_key_missing",
]
const noDataTexts = ["no_data", "no_answer", "no_email_to_verify", "upstream_dependency_failed", "upstream_dependency_unavailable"]

export function cellOutcome(status: string | undefined, error?: string | null): CellOutcome {
  if (status === "complete") return "filled"
  if (status === "skipped") return "skipped"
  const e = (error ?? "").trim().toLowerCase()
  if (!e) return "no_data"
  if (needsSetupMarkers.some(m => e.includes(m))) return "needs_setup"
  if (noDataTexts.some(p => e === p || e.startsWith(`${p}:`) || e.startsWith(`${p} `))) return "no_data"
  return "error"
}

/** "86 filled · 89 no data · 25 need setup"; empty when nothing was counted. */
export function runCountsLabel(c: RunCounts): string {
  const parts: string[] = []
  if (c.completed) parts.push(`${c.completed} filled`)
  if (c.no_data) parts.push(`${c.no_data} no data`)
  if (c.needs_setup) parts.push(`${c.needs_setup} need setup`)
  if (c.errors) parts.push(`${c.errors} ${c.errors === 1 ? "error" : "errors"}`)
  return parts.join(" · ")
}

/** Red treatment only for a run that really failed or has errored cells. */
export const runNeedsAttention = (status: string) => status === "failed" || status === "completed_with_errors"
