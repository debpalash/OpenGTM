import { Bot, ArrowRight } from "lucide-react"

/**
 * Renders an Autopilot plan (the argument of an `execute_plan` confirmation)
 * as an ordered, readable step list. Used inside the confirmation gate so the
 * user reviews the full plan before approving execution.
 */
export interface AutopilotPlan {
  goal?: string
  estimated_rows?: number | null
  steps?: Array<{ kind: string; description?: string; params?: Record<string, unknown> }>
}

const KIND_LABEL: Record<string, string> = {
  create_source_workbook: "Source companies",
  add_agent_column: "Enrich (agent)",
  set_workbook_refresh: "Keep fresh",
  report: "Report",
}

export function AutopilotPlanCard({ plan }: { plan: AutopilotPlan }) {
  const steps = plan.steps ?? []
  return (
    <div className="rounded-xl border border-rose-500/30 bg-rose-500/[0.04] overflow-hidden">
      <div className="flex items-center gap-2 px-3.5 py-2.5 border-b border-rose-500/15">
        <Bot className="size-4 text-rose-400 shrink-0" />
        <span className="text-sm font-medium text-rose-300">Autopilot plan</span>
        {plan.estimated_rows ? (
          <span className="ml-auto text-[11px] text-muted-foreground">~{plan.estimated_rows} companies</span>
        ) : null}
      </div>
      {plan.goal && (
        <div className="px-3.5 pt-2.5 text-xs text-muted-foreground italic">{plan.goal}</div>
      )}
      <ol className="px-3.5 py-2.5 space-y-1.5">
        {steps.map((s, i) => (
          <li key={i} className="flex items-start gap-2.5 text-sm">
            <span className="flex size-5 shrink-0 items-center justify-center rounded-full bg-rose-500/15 text-rose-300 text-[11px] font-medium mt-0.5">
              {i + 1}
            </span>
            <div className="flex-1 min-w-0">
              <div className="flex items-center gap-1.5">
                <span className="text-[10px] uppercase tracking-wider text-rose-400/70">
                  {KIND_LABEL[s.kind] ?? s.kind}
                </span>
              </div>
              <div className="text-muted-foreground">{s.description ?? s.kind}</div>
            </div>
          </li>
        ))}
      </ol>
      <div className="px-3.5 pb-2.5 flex items-center gap-1.5 text-[11px] text-muted-foreground/70">
        <ArrowRight className="size-3" />
        Approve to build the workbook and start sourcing + enrichment.
      </div>
    </div>
  )
}
