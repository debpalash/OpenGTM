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
} as const
