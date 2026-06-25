import React, { useState, useEffect } from "react"
import { useNavigate } from "react-router-dom"
import { toast } from "sonner"
import {
  CheckCircle2, XCircle, Clock, Loader2, Play,
  LayoutList, LayoutGrid, Zap, MoreHorizontal,
  StopCircle, Trash2, RefreshCw, FileX2,
} from "lucide-react"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Skeleton } from "@/components/ui/skeleton"
import { ScrollArea } from "@/components/ui/scroll-area"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"
import {
  DropdownMenu, DropdownMenuContent, DropdownMenuItem,
  DropdownMenuSeparator, DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { useJobs, useCollect } from "@/lib/hooks"
import { TaskDetailCard } from "@/components/task-detail-card"
import type { Job } from "@/lib/api"
import { cn } from "@/lib/utils"
import { queryClient, queryKeys } from "@/lib/query-client"

// ── Task Actions ─────────────────────────────────────────────────

async function cancelJob(jobId: string) {
  await fetch(`/api/jobs/${jobId}/cancel`, { method: "POST" })
  queryClient.invalidateQueries({ queryKey: queryKeys.jobs.all })
  toast.success("Task cancelled")
}

async function deleteJob(jobId: string, keepLeads: boolean) {
  await fetch(`/api/jobs/${jobId}?keep_leads=${keepLeads}`, { method: "DELETE" })
  queryClient.invalidateQueries({ queryKey: queryKeys.jobs.all })
  if (!keepLeads) {
    queryClient.invalidateQueries({ queryKey: queryKeys.leads.all })
    queryClient.invalidateQueries({ queryKey: queryKeys.stats.all })
  }
  toast.success(keepLeads ? "Task removed (leads kept)" : "Task and leads deleted")
}

async function retryJob(jobId: string) {
  await fetch(`/api/jobs/${jobId}/retry`, { method: "POST" })
  queryClient.invalidateQueries({ queryKey: queryKeys.jobs.all })
  toast.success("Task queued for retry")
}

// ── Status Helpers ───────────────────────────────────────────────

const STATUS_ICON: Record<string, typeof CheckCircle2> = {
  done: CheckCircle2,
  running: Loader2,
  pending: Clock,
  failed: XCircle,
  cancelled: StopCircle,
}

const STATUS_COLORS: Record<string, string> = {
  done: "text-green-500",
  running: "text-blue-500",
  pending: "text-muted-foreground",
  failed: "text-destructive",
  cancelled: "text-orange-500",
}

const STATUS_BG: Record<string, string> = {
  running: "border-l-blue-500",
  done: "border-l-green-500",
  failed: "border-l-destructive",
  pending: "border-l-muted-foreground",
  cancelled: "border-l-orange-500",
}

type FilterStatus = "all" | "running" | "done" | "failed"

// ── List Row ─────────────────────────────────────────────────────

function TaskListRow({ job, onClick }: { job: Job; onClick: () => void }) {
  const Icon = STATUS_ICON[job.status] || Clock
  const isRunning = job.status === "running"

  // Server stores UTC timestamps without 'Z' suffix
  const utc = (ts: string) => ts && !ts.endsWith("Z") ? ts + "Z" : ts

  const duration = job.completed_at && job.started_at
    ? Math.round((new Date(utc(job.completed_at)).getTime() - new Date(utc(job.started_at)).getTime()) / 1000)
    : job.started_at
      ? Math.round((Date.now() - new Date(utc(job.started_at)).getTime()) / 1000)
      : 0

  const formatDuration = (s: number) => {
    if (s < 60) return `${s}s`
    return `${Math.floor(s / 60)}m ${s % 60}s`
  }

  return (
    <div
      className={cn(
        "w-full flex items-center gap-2 px-3 py-2 rounded-lg",
        "border-l-2 hover:bg-muted/50 transition-all duration-200 group",
        STATUS_BG[job.status] || "border-l-transparent",
        isRunning && "bg-blue-500/[0.03]",
      )}
    >
      <button onClick={onClick} className="flex items-center gap-2.5 flex-1 min-w-0 text-left">
        <Icon className={cn(
          "size-4 shrink-0",
          STATUS_COLORS[job.status],
          isRunning && "animate-spin",
        )} />

        <span className="text-sm font-medium truncate flex-1 min-w-0">{job.query}</span>

        <Badge variant="outline" className="text-[10px] py-0 shrink-0 tabular-nums w-16 justify-center">
          {job.leads_found} leads
        </Badge>

        <span className="text-xs text-muted-foreground shrink-0 tabular-nums w-14 text-right">
          {duration > 0 ? formatDuration(duration) : "—"}
        </span>

        <span className="text-xs text-muted-foreground shrink-0 tabular-nums w-12 text-right">
          {new Date(job.created_at).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}
        </span>
      </button>

      {/* Actions dropdown */}
      <TaskActionsMenu job={job} />
    </div>
  )
}

// ── Card View ────────────────────────────────────────────────────

function TaskCard({ job }: { job: Job; onClick: () => void }) {
  return <TaskDetailCard jobId={job.id} compact />
}

// ── Main Page ────────────────────────────────────────────────────

export default function AgentsPage() {
  const navigate = useNavigate()
  const [view, setView] = useState<"list" | "cards">(() => {
    return (localStorage.getItem("task-view") as "list" | "cards") || "list"
  })
  const [filter, setFilter] = useState<FilterStatus>("all")
  const [collectQuery, setCollectQuery] = useState("")
  const { data: jobs, isLoading } = useJobs()
  const collect = useCollect()

  useEffect(() => {
    localStorage.setItem("task-view", view)
  }, [view])

  const handleCollect = () => {
    if (!collectQuery.trim()) return
    collect.mutate({ query: collectQuery })
    toast.success(`Pipeline started: "${collectQuery}"`)
    setCollectQuery("")
  }

  const filtered = React.useMemo(() => {
    if (!jobs) return []
    if (filter === "all") return jobs
    return jobs.filter(j => j.status === filter)
  }, [jobs, filter])

  const runningCount = jobs?.filter(j => j.status === "running").length ?? 0
  const doneCount = jobs?.filter(j => j.status === "done").length ?? 0
  const failedCount = jobs?.filter(j => j.status === "failed").length ?? 0

  return (
    <div className="flex flex-col h-full">
      {/* Header Bar */}
      <div className="shrink-0 border-b px-4 py-3 space-y-3">
        {/* Top row: title + controls */}
        <div className="flex items-center gap-3">
          <div className="flex items-center gap-2 flex-1 min-w-0">
            <Zap className="size-4 text-primary shrink-0" />
            <h2 className="text-sm font-semibold">Task Queue</h2>
            {runningCount > 0 && (
              <Badge variant="secondary" className="text-[10px] gap-1">
                <Loader2 className="size-2.5 animate-spin" />
                {runningCount} running
              </Badge>
            )}
          </div>

          {/* View Toggle */}
          <ToggleGroup value={[view]} onValueChange={(v) => { const next = v[0]; if (next) setView(next as "list" | "cards") }} size="sm">
            <ToggleGroupItem value="list" aria-label="List view">
              <LayoutList className="size-4" />
            </ToggleGroupItem>
            <ToggleGroupItem value="cards" aria-label="Card view">
              <LayoutGrid className="size-4" />
            </ToggleGroupItem>
          </ToggleGroup>
        </div>

        {/* Filter chips + new collection */}
        <div className="flex items-center gap-2">
          <div className="flex items-center gap-1">
            <FilterChip label="All" count={jobs?.length ?? 0} active={filter === "all"} onClick={() => setFilter("all")} />
            <FilterChip label="Running" count={runningCount} active={filter === "running"} onClick={() => setFilter("running")} variant="running" />
            <FilterChip label="Done" count={doneCount} active={filter === "done"} onClick={() => setFilter("done")} variant="done" />
            <FilterChip label="Failed" count={failedCount} active={filter === "failed"} onClick={() => setFilter("failed")} variant="failed" />
          </div>

          <div className="flex-1" />

          {/* Bulk cleanup */}
          {(jobs?.length ?? 0) > 0 && (
            <DropdownMenu>
              <DropdownMenuTrigger
                className="inline-flex items-center justify-center gap-1 h-8 px-2 text-xs rounded-md text-muted-foreground hover:bg-accent hover:text-accent-foreground cursor-pointer transition-colors"
              >
                <Trash2 className="size-3.5" /> Clean up
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end" className="w-56">
                <DropdownMenuItem onClick={async () => {
                  const empty = jobs?.filter(j => j.status === "done" && j.leads_found === 0) || []
                  if (!empty.length) { toast.info("No empty tasks to clear"); return }
                  if (!confirm(`Delete ${empty.length} completed tasks with 0 leads?`)) return
                  await Promise.all(empty.map(j => fetch(`/api/jobs/${j.id}?keep_leads=false`, { method: "DELETE" })))
                  queryClient.invalidateQueries({ queryKey: queryKeys.jobs.all })
                  toast.success(`Cleared ${empty.length} empty tasks`)
                }}>
                  <FileX2 className="size-4 mr-2" />
                  Clear empty tasks (0 leads)
                </DropdownMenuItem>
                <DropdownMenuItem onClick={async () => {
                  const done = jobs?.filter(j => j.status === "done") || []
                  if (!done.length) { toast.info("No completed tasks"); return }
                  if (!confirm(`Remove ${done.length} completed tasks? Leads will be kept.`)) return
                  await Promise.all(done.map(j => fetch(`/api/jobs/${j.id}?keep_leads=true`, { method: "DELETE" })))
                  queryClient.invalidateQueries({ queryKey: queryKeys.jobs.all })
                  toast.success(`Cleared ${done.length} completed tasks (leads kept)`)
                }}>
                  <CheckCircle2 className="size-4 mr-2" />
                  Clear all completed (keep leads)
                </DropdownMenuItem>
                <DropdownMenuSeparator />
                <DropdownMenuItem
                  className="text-destructive focus:text-destructive"
                  onClick={async () => {
                    if (!confirm(`DELETE ALL ${jobs?.length} tasks and their leads? This cannot be undone.`)) return
                    await Promise.all((jobs || []).map(j => fetch(`/api/jobs/${j.id}?keep_leads=false`, { method: "DELETE" })))
                    queryClient.invalidateQueries({ queryKey: queryKeys.jobs.all })
                    queryClient.invalidateQueries({ queryKey: queryKeys.leads.all })
                    queryClient.invalidateQueries({ queryKey: queryKeys.stats.all })
                    toast.success("All tasks deleted")
                  }}
                >
                  <Trash2 className="size-4 mr-2" />
                  Delete everything
                </DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>
          )}

          <div className="flex items-center gap-1">
            <Input
              placeholder="New collection..."
              value={collectQuery}
              onChange={(e) => setCollectQuery(e.target.value)}
              onKeyDown={(e) => e.key === "Enter" && handleCollect()}
              className="h-8 w-52 text-xs"
            />
            <Button size="sm" className="h-8 px-2.5" onClick={handleCollect} disabled={collect.isPending}>
              {collect.isPending ? <Loader2 className="size-3.5 animate-spin" /> : <Play className="size-3.5" />}
            </Button>
          </div>
        </div>
      </div>

      {/* Task List/Grid */}
      <ScrollArea className="flex-1">
        <div className="p-3">
          {isLoading ? (
            <div className={cn(
              view === "cards" ? "grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-3" : "space-y-0.5"
            )}>
              {Array.from({ length: 8 }).map((_, i) => (
                <Skeleton key={i} className={view === "cards" ? "h-28" : "h-11"} />
              ))}
            </div>
          ) : filtered.length === 0 ? (
            <div className="text-center py-16 space-y-3">
              <Zap className="size-12 mx-auto text-muted-foreground/20" />
              <p className="text-sm text-muted-foreground">
                {filter !== "all" ? "No tasks match this filter" : "No tasks yet. Start a collection above."}
              </p>
            </div>
          ) : view === "list" ? (
            <div className="space-y-0.5">
              {filtered.map(job => (
                <TaskListRow
                  key={job.id}
                  job={job}
                  onClick={() => navigate(`/agents/${job.id}`)}
                />
              ))}
            </div>
          ) : (
            <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-3">
              {filtered.map(job => (
                <TaskCard key={job.id} job={job} onClick={() => navigate(`/agents/${job.id}`)} />
              ))}
            </div>
          )}
        </div>
      </ScrollArea>
    </div>
  )
}

// ── Filter Chip ──────────────────────────────────────────────────

function FilterChip({ label, count, active, onClick }: {
  label: string
  count: number
  active: boolean
  onClick: () => void
  variant?: "running" | "done" | "failed"
}) {
  return (
    <button
      onClick={onClick}
      className={cn(
        "text-[11px] px-2 py-1 rounded-md font-medium transition-colors",
        active
          ? "bg-primary text-primary-foreground"
          : "bg-muted/50 text-muted-foreground hover:bg-muted hover:text-foreground"
      )}
    >
      {label}
      {count > 0 && (
        <span className={cn("ml-1", active ? "opacity-80" : "opacity-60")}>{count}</span>
      )}
    </button>
  )
}

// ── Task Actions Menu ────────────────────────────────────────────

function TaskActionsMenu({ job }: { job: Job }) {
  const isActive = job.status === "running" || job.status === "pending"
  const isFailed = job.status === "failed" || job.status === "cancelled"

  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        className="size-7 p-0 opacity-0 group-hover:opacity-100 transition-opacity inline-flex items-center justify-center rounded-md hover:bg-accent hover:text-accent-foreground cursor-pointer"
        onClick={(e: React.MouseEvent) => e.stopPropagation()}
      >
        <MoreHorizontal className="size-4" />
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" className="w-48">
        {isActive && (
          <DropdownMenuItem
            onClick={(e) => { e.stopPropagation(); cancelJob(job.id) }}
            className="text-destructive focus:text-destructive"
          >
            <StopCircle className="size-4 mr-2" />
            Stop Task
          </DropdownMenuItem>
        )}
        {isFailed && (
          <DropdownMenuItem
            onClick={(e) => { e.stopPropagation(); retryJob(job.id) }}
          >
            <RefreshCw className="size-4 mr-2" />
            Retry Task
          </DropdownMenuItem>
        )}
        {(isActive || isFailed) && <DropdownMenuSeparator />}
        <DropdownMenuItem
          onClick={(e) => { e.stopPropagation(); deleteJob(job.id, true) }}
        >
          <FileX2 className="size-4 mr-2" />
          Remove Task (Keep Leads)
        </DropdownMenuItem>
        <DropdownMenuItem
          onClick={(e) => {
            e.stopPropagation()
            if (confirm(`Delete task "${job.query}" and all its leads?`)) {
              deleteJob(job.id, false)
            }
          }}
          className="text-destructive focus:text-destructive"
        >
          <Trash2 className="size-4 mr-2" />
          Delete Task & Leads
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  )
}
