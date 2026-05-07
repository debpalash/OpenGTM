import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query"
import { useEffect, useRef, useCallback, useState } from "react"
import { queryKeys } from "./query-client"
import {
  fetchLeads, fetchStats, fetchFilters, fetchJobs,
  fetchSystemStats, fetchWorkspaces, submitCollect,
  updateStatus, updateLead, deleteLead, addLead, fetchLead,
  fetchConversations, fetchConversationMessages,
  fetchAnalyticsOverview, fetchAnalyticsPipeline,
  fetchAnalyticsCollection, fetchAnalyticsEnrichment, fetchAnalyticsLLM,
  importDataCollector,
  type Lead, type Job,
} from "./api"

// ── Leads ───────────────────────────────────────────────────────

export function useLeads(filters: Record<string, string> = {}) {
  return useQuery({
    queryKey: queryKeys.leads.list(filters),
    queryFn: () => fetchLeads(filters),
  })
}

export function useLead(id: number) {
  return useQuery({
    queryKey: queryKeys.leads.detail(id),
    queryFn: () => fetchLead(id),
    enabled: !!id,
  })
}

export function useStats() {
  return useQuery({
    queryKey: queryKeys.stats.all,
    queryFn: fetchStats,
    staleTime: 10 * 1000,
  })
}

export function useFilters() {
  return useQuery({
    queryKey: queryKeys.filters,
    queryFn: fetchFilters,
    staleTime: 60 * 1000,
  })
}

// ── Mutations ───────────────────────────────────────────────────

export function useUpdateStatus() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ id, status, note }: { id: number; status: string; note?: string }) =>
      updateStatus(id, status, note),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: queryKeys.leads.all })
      qc.invalidateQueries({ queryKey: queryKeys.stats.all })
    },
  })
}

export function useUpdateLead() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ id, fields }: { id: number; fields: Partial<Lead> }) =>
      updateLead(id, fields),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: queryKeys.leads.all })
    },
  })
}

export function useDeleteLead() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (id: number) => deleteLead(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: queryKeys.leads.all })
      qc.invalidateQueries({ queryKey: queryKeys.stats.all })
    },
  })
}

export function useAddLead() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (data: Partial<Lead>) => addLead(data),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: queryKeys.leads.all })
      qc.invalidateQueries({ queryKey: queryKeys.stats.all })
    },
  })
}

export function useCollect() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ query, workspace_id }: { query: string; workspace_id?: string }) =>
      submitCollect(query, workspace_id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: queryKeys.jobs.all })
    },
  })
}

export function useImportDataCollector() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ module, limit }: { module?: string; limit?: number } = {}) =>
      importDataCollector(module || "all", limit),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: queryKeys.leads.all })
      qc.invalidateQueries({ queryKey: queryKeys.stats.all })
      qc.invalidateQueries({ queryKey: queryKeys.filters })
    },
  })
}

// ── Jobs ────────────────────────────────────────────────────────

export function useJobs(status?: string) {
  return useQuery({
    queryKey: queryKeys.jobs.list(status),
    queryFn: () => fetchJobs(status),
    // SSE handles real-time updates via query invalidation.
    // Polling is just a safety net — 15s for active jobs, 60s idle.
    refetchInterval: (query) => {
      const jobs = query.state.data as Job[] | undefined
      const hasActive = jobs?.some(
        (j: Job) => j.status === "running" || j.status === "pending"
      )
      return hasActive ? 15000 : 60000
    },
  })
}

export function useSystemStats() {
  return useQuery({
    queryKey: queryKeys.system,
    queryFn: fetchSystemStats,
    staleTime: 10 * 1000,
  })
}

// ── Workspaces ──────────────────────────────────────────────────

export function useWorkspaces() {
  return useQuery({
    queryKey: queryKeys.workspaces,
    queryFn: fetchWorkspaces,
    staleTime: 60 * 1000,
  })
}

// ── SSE Live Feed ───────────────────────────────────────────────

interface SSEEvent {
  type: string
  data: Record<string, unknown>
  timestamp: number
}

export function useSSE(url = "/api/events") {
  const qc = useQueryClient()
  const [events, setEvents] = useState<SSEEvent[]>([])
  const [connected, setConnected] = useState(false)
  const esRef = useRef<EventSource | null>(null)

  const connect = useCallback(() => {
    if (esRef.current) {
      esRef.current.close()
    }

    const es = new EventSource(url)
    esRef.current = es

    es.onopen = () => setConnected(true)
    es.onerror = () => {
      setConnected(false)
      // Reconnect after 3s
      setTimeout(() => connect(), 3000)
    }

    es.onmessage = (event) => {
      try {
        const parsed = JSON.parse(event.data) as SSEEvent
        setEvents(prev => [parsed, ...prev].slice(0, 100))

        // Invalidate relevant queries based on event type
        const type = parsed.type
        if (type === "lead_stored" || type === "lead_discovered") {
          qc.invalidateQueries({ queryKey: queryKeys.leads.all })
          qc.invalidateQueries({ queryKey: queryKeys.stats.all })
        }
        if (type === "job_completed" || type === "job_failed" || type === "job_started") {
          qc.invalidateQueries({ queryKey: queryKeys.jobs.all })
          qc.invalidateQueries({ queryKey: queryKeys.leads.all })
          qc.invalidateQueries({ queryKey: queryKeys.stats.all })
        }
        if (type === "job_progress") {
          qc.invalidateQueries({ queryKey: queryKeys.jobs.all })
        }
      } catch {
        // Ignore parse errors
      }
    }
  }, [url, qc])

  useEffect(() => {
    connect()
    return () => {
      esRef.current?.close()
    }
  }, [connect])

  return { events, connected }
}

// ── Providers ───────────────────────────────────────────────────

export interface Provider {
  id: string
  name: string
  icon: string
  configured: boolean
  api_key_masked: string
  base_url: string
  model: string
  default_model: string
  docs_url: string
  free_tier: string
  is_default: boolean
  openai_compatible: boolean
}

export function useProviders() {
  return useQuery({
    queryKey: queryKeys.providers,
    queryFn: async (): Promise<{ providers: Provider[]; default_provider: string }> => {
      const res = await fetch("/api/settings/providers")
      return res.json()
    },
    staleTime: 60 * 1000,
  })
}

export interface LLMUsageProvider {
  provider: string
  model: string
  calls: number
  tokens: number
  daily_limit: number
  limit_label: string
  remaining: number
  pct: number
}

export interface LLMUsageData {
  providers: LLMUsageProvider[]
  today_calls: number
  today_tokens: number
  all_time_calls: number
  all_time_tokens: number
}

export function useLLMUsage() {
  return useQuery({
    queryKey: ["llm-usage"],
    queryFn: async (): Promise<LLMUsageData> => {
      const res = await fetch("/api/settings/llm-usage")
      return res.json()
    },
    refetchInterval: 30 * 1000, // refresh every 30s
  })
}

// ── Conversations ───────────────────────────────────────────────

export function useConversations() {
  return useQuery({
    queryKey: queryKeys.conversations.list(),
    queryFn: fetchConversations,
  })
}

export function useConversationMessages(id: string | null) {
  return useQuery({
    queryKey: id ? queryKeys.conversations.detail(id) : [],
    queryFn: () => (id ? fetchConversationMessages(id) : null),
    enabled: !!id,
  })
}

// ── Analytics ───────────────────────────────────────────────────

export function useAnalyticsOverview() {
  return useQuery({ queryKey: ["analytics", "overview"], queryFn: fetchAnalyticsOverview, staleTime: 30_000 })
}
export function useAnalyticsPipeline() {
  return useQuery({ queryKey: ["analytics", "pipeline"], queryFn: fetchAnalyticsPipeline, staleTime: 30_000 })
}
export function useAnalyticsCollection() {
  return useQuery({ queryKey: ["analytics", "collection"], queryFn: fetchAnalyticsCollection, staleTime: 30_000 })
}
export function useAnalyticsEnrichment() {
  return useQuery({ queryKey: ["analytics", "enrichment"], queryFn: fetchAnalyticsEnrichment, staleTime: 30_000 })
}
export function useAnalyticsLLM() {
  return useQuery({ queryKey: ["analytics", "llm"], queryFn: fetchAnalyticsLLM, staleTime: 30_000 })
}
