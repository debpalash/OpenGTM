import { QueryClient } from "@tanstack/react-query"

export const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 30 * 1000,        // 30s before refetch
      gcTime: 5 * 60 * 1000,       // 5min garbage collection
      refetchOnWindowFocus: true,
      retry: 1,
    },
  },
})

// Query key factories for consistent cache management
export const queryKeys = {
  leads: {
    all: ["leads"] as const,
    list: (filters: Record<string, string>) => ["leads", "list", filters] as const,
    detail: (id: number) => ["leads", "detail", id] as const,
  },
  stats: {
    all: ["stats"] as const,
    enrichment: ["stats", "enrichment"] as const,
  },
  filters: ["filters"] as const,
  jobs: {
    all: ["jobs"] as const,
    list: (status?: string) => ["jobs", "list", status] as const,
    detail: (id: string) => ["jobs", "detail", id] as const,
    stages: (id: string) => ["jobs", "stages", id] as const,
  },
  workspaces: ["workspaces"] as const,
  providers: ["providers"] as const,
  system: ["system-stats"] as const,
  conversations: {
    all: ["conversations"] as const,
    list: () => ["conversations", "list"] as const,
    detail: (id: string) => ["conversations", "detail", id] as const,
  },
  // ── GTM Automation UI ──────────────────────────────────────────
  meta: {
    context: ["meta", "context"] as const,
    flags: ["meta", "flags"] as const,
  },
  outreach: {
    all: ["outreach"] as const,
    sequences: ["outreach", "sequences"] as const,
    sequence: (id: string) => ["outreach", "sequence", id] as const,
    sends: (id: string) => ["outreach", "sends", id] as const,
    smtp: ["outreach", "smtp"] as const,
    suppressions: ["outreach", "suppressions"] as const,
  },
  automations: {
    all: ["automations"] as const,
    triggers: (f?: object) => ["automations", "triggers", f ?? {}] as const,
    trigger: (id: string) => ["automations", "trigger", id] as const,
    runs: (id: string) => ["automations", "runs", id] as const,
    run: (rid: string) => ["automations", "run", rid] as const,
  },
  watches: {
    all: ["watches"] as const,
    list: (p?: object) => ["watches", "list", p ?? {}] as const,
    detail: (id: string) => ["watches", "detail", id] as const,
    signals: (id: string) => ["watches", "signals", id] as const,
  },
} as const
