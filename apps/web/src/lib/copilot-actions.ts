/**
 * CopilotKit Actions — Frontend tool definitions
 * 
 * These hooks provide the AI copilot with context about
 * the current app state and register frontend-side actions.
 */
import { useCopilotReadable, useCopilotAction } from "@copilotkit/react-core"
import { useStats } from "./hooks"
import type { Lead } from "./api"

/**
 * Provides the current lead context to the copilot.
 * Called from the Leads page to feed visible data to the AI.
 */
export function useLeadContext(leads: Lead[] | undefined, selectedIds: number[]) {
  useCopilotReadable({
    description: "Current leads visible in the data table",
    value: leads
      ? `${leads.length} leads loaded. Top 5: ${leads
          .slice(0, 5)
          .map(l => `${l.company} (${l.city}, score:${l.score}, ${l.status})`)
          .join("; ")}`
      : "No leads loaded",
  })

  useCopilotReadable({
    description: "Currently selected lead IDs for bulk operations",
    value: selectedIds.length > 0
      ? `${selectedIds.length} leads selected: IDs ${selectedIds.join(", ")}`
      : "No leads selected",
  })
}

/**
 * Provides dashboard stats context.
 */
export function useStatsContext() {
  const { data: stats } = useStats()
  
  useCopilotReadable({
    description: "Lead pipeline statistics",
    value: stats
      ? `Total: ${stats.total} leads. Hot: ${stats.by_tier?.hot || 0}, Warm: ${stats.by_tier?.warm || 0}, Cold: ${stats.by_tier?.cold || 0}. With email: ${stats.enrichment?.with_email || 0}, with phone: ${stats.enrichment?.with_phone || 0}.`
      : "Stats not loaded",
  })
}

/**
 * Registers a frontend action to navigate to a specific page.
 */
export function useNavigationAction(navigate: (path: string) => void) {
  useCopilotAction({
    name: "navigateTo",
    description: "Navigate to a specific page in the application",
    parameters: [
      {
        name: "page",
        type: "string",
        description: "Page to navigate to: chat, leads, search, agents, campaigns, sources, settings",
        required: true,
      },
    ],
    handler: async ({ page }: { page: string }) => {
      const validPages = ["chat", "leads", "search", "agents", "campaigns", "sources", "settings"]
      if (validPages.includes(page)) {
        navigate(`/${page}`)
        return `Navigated to ${page}`
      }
      return `Invalid page: ${page}. Valid pages: ${validPages.join(", ")}`
    },
  })
}
