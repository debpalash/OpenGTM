import { useQuery } from "@tanstack/react-query"
import { fetchJobs, type Job } from "./api"
import { useAuth } from "./auth-context"
import { getActiveWorkspace } from "./auth"
import { fetchWorkbooks, type Workbook } from "./workbook-api"

export type AppNotification = {
  id: string
  severity: "critical" | "urgent"
  category: "task" | "workbook" | "signal"
  title: string
  description: string
  href: string
  createdAt: number
}

// Encode even dot segments so an API-provided id cannot change the internal route.
function routeId(id: string): string {
  return encodeURIComponent(id).replace(/\./g, "%2E")
}

function timestamp(value: unknown, source: string): number {
  if (typeof value !== "string" || !value.trim()) {
    throw new Error(`Invalid ${source} timestamp`)
  }
  // SQLite job timestamps can be naive UTC ISO strings; do not parse them as local time.
  const iso = /(?:Z|[+-]\d{2}:?\d{2})$/i.test(value) ? value : `${value}Z`
  const parsed = Date.parse(iso)
  if (!Number.isFinite(parsed)) throw new Error(`Invalid ${source} timestamp`)
  return parsed
}

function jobNotifications(payload: Job[], workspaceId: string): AppNotification[] {
  if (!Array.isArray(payload)) throw new Error("Invalid tasks response")
  return payload.flatMap((job) => {
    if (!job || typeof job !== "object" || typeof job.status !== "string") {
      throw new Error("Invalid tasks response")
    }
    if (job.workspace_id && job.workspace_id !== workspaceId) throw new Error("Invalid tasks workspace")
    if (job.status !== "failed") return []
    if (typeof job.id !== "string" || !job.id || typeof job.query !== "string") {
      throw new Error("Invalid failed task")
    }
    return [{
      id: `task:${job.id}`,
      severity: "critical" as const,
      category: "task" as const,
      title: job.query || "Task failed",
      description: "Task failed. Open it to review the failure.",
      href: `/agents/${routeId(job.id)}`,
      createdAt: timestamp(job.completed_at || job.created_at, "task"),
    }]
  })
}

function workbookNotifications(payload: { workbooks: Workbook[] }, workspaceId: string): AppNotification[] {
  if (!payload || !Array.isArray(payload.workbooks)) throw new Error("Invalid workbooks response")
  return payload.workbooks.flatMap((workbook) => {
    if (!workbook || typeof workbook !== "object" || typeof workbook.status !== "string") {
      throw new Error("Invalid workbooks response")
    }
    if ("workspace_id" in workbook && workbook.workspace_id !== workspaceId) {
      throw new Error("Invalid workbooks workspace")
    }
    if (workbook.status !== "failed") return []
    if (typeof workbook.id !== "string" || !workbook.id || typeof workbook.name !== "string") {
      throw new Error("Invalid failed workbook")
    }
    return [{
      id: `workbook:${workbook.id}`,
      severity: "critical" as const,
      category: "workbook" as const,
      title: workbook.name || "Workbook failed",
      description: "Workbook failed. Open it to review the run.",
      href: `/workbooks/${routeId(workbook.id)}`,
      createdAt: timestamp(workbook.updated_at || workbook.created_at, "workbook"),
    }]
  })
}

type RecentSignal = {
  id: string
  title: string
  description: string
  weight: number
  read: number | boolean
  created_at: number
  workspace_id?: string
}

async function signalNotifications(workspaceId: string): Promise<AppNotification[]> {
  const response = await fetch("/api/signals?limit=200")
  if (!response.ok) throw new Error(`Could not load recent signals (${response.status})`)
  const payload: unknown = await response.json()
  if (!payload || typeof payload !== "object" || !("signals" in payload) || !Array.isArray(payload.signals)) {
    throw new Error("Invalid signals response")
  }
  return payload.signals.flatMap((value: unknown) => {
    if (!value || typeof value !== "object") throw new Error("Invalid signals response")
    const signal = value as RecentSignal
    if (signal.workspace_id && signal.workspace_id !== workspaceId) throw new Error("Invalid signals workspace")
    if (typeof signal.id !== "string" || !signal.id || typeof signal.title !== "string" ||
        typeof signal.description !== "string" || !Number.isFinite(signal.weight) ||
        !Number.isFinite(signal.created_at) || (signal.read !== 0 && signal.read !== 1 &&
          signal.read !== false && signal.read !== true)) {
      throw new Error("Invalid signals response")
    }
    if (signal.read || signal.weight < 7) return []
    return [{
      id: `signal:${signal.id}`,
      severity: "urgent" as const,
      category: "signal" as const,
      title: signal.title,
      description: signal.description,
      href: "/signals",
      createdAt: signal.created_at * 1000,
    }]
  })
}

export function useNotifications() {
  const { activeWorkspaceId } = useAuth()
  return useQuery<AppNotification[]>({
    queryKey: ["notifications", activeWorkspaceId],
    enabled: !!activeWorkspaceId,
    refetchInterval: 30_000,
    queryFn: async () => {
      const workspaceId = activeWorkspaceId
      if (!workspaceId || getActiveWorkspace() !== workspaceId) throw new Error("Workspace changed while loading notifications")
      const [jobs, workbooks, signals] = await Promise.all([
        fetchJobs(), fetchWorkbooks(), signalNotifications(workspaceId),
      ])
      if (getActiveWorkspace() !== workspaceId) throw new Error("Workspace changed while loading notifications")
      return [
        ...jobNotifications(jobs, workspaceId),
        ...workbookNotifications(workbooks, workspaceId),
        ...signals,
      ].sort((a, b) => b.createdAt - a.createdAt || a.id.localeCompare(b.id))
    },
  })
}
