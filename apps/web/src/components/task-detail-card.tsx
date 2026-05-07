import React, { useState, useEffect } from "react"
import { useNavigate } from "react-router-dom"
import {
  CheckCircle2, XCircle, Clock, Loader2, ChevronDown, ChevronRight,
  Brain, ExternalLink, MapPin, Globe, Link2, Briefcase, Star,
  FileSearch, Users, Shield, Layers, Sparkles, Zap, ArrowRight,
  StopCircle, Trash2, RefreshCw, FileX2, Mail, Database,
  TrendingUp, ShieldCheck,
} from "lucide-react"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible"
import { Separator } from "@/components/ui/separator"
import { Skeleton } from "@/components/ui/skeleton"
import { ScrollArea } from "@/components/ui/scroll-area"
import { cn } from "@/lib/utils"
import { queryClient, queryKeys } from "@/lib/query-client"
import { toast } from "sonner"

// ── Types ────────────────────────────────────────────────────────

interface JobStage {
  id: number
  job_id: string
  stage: string
  status: string
  input_count: number
  output_count: number
  rejected_count: number
  details: Record<string, unknown>
  started_at: string
  completed_at: string
}

interface JobDetail {
  id: string
  query: string
  status: string
  leads_found: number
  created_at: string
  started_at: string
  completed_at: string
  error: string
  stages: JobStage[]
}

interface JobLead {
  id: number
  company: string
  email: string
  phone: string
  website: string
  score: number
  score_tier: string
  source: string
  city: string
}

// ── Constants ────────────────────────────────────────────────────

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

const STAGE_META: Record<string, { icon: typeof Globe; label: string; color: string }> = {
  maps:             { icon: MapPin,     label: "Maps",              color: "text-red-500" },
  web:              { icon: Globe,      label: "Web",               color: "text-blue-500" },
  directories:      { icon: FileSearch, label: "Directories",       color: "text-amber-500" },
  linkedin:         { icon: Link2,      label: "LinkedIn",          color: "text-sky-600" },
  job_boards:       { icon: Briefcase,  label: "Job Boards",        color: "text-emerald-500" },
  review_sites:     { icon: Star,       label: "Reviews",           color: "text-yellow-500" },
  validate:         { icon: Shield,     label: "Validate",          color: "text-violet-500" },
  dedup:            { icon: Layers,     label: "Dedup",             color: "text-orange-500" },
  score:            { icon: Brain,      label: "AI Score",          color: "text-purple-500" },
  enrich:           { icon: Sparkles,   label: "Enrich",            color: "text-cyan-500" },
  decision_makers:  { icon: Users,      label: "People",            color: "text-pink-500" },
  personal_emails:  { icon: Mail,       label: "Emails",            color: "text-teal-500" },
  crosslinked:      { icon: Link2,      label: "LinkedIn PPL",      color: "text-blue-600" },
  hiring_signals:   { icon: TrendingUp, label: "Hiring",            color: "text-green-500" },
  smtp_verify:      { icon: ShieldCheck, label: "SMTP",             color: "text-indigo-500" },
  store:            { icon: Database,   label: "Store",             color: "text-slate-500" },
}

// ── Stage Component (Compact) ────────────────────────────────────

function StageRow({ stage }: { stage: JobStage }) {
  const meta = STAGE_META[stage.stage] || { icon: Zap, label: stage.stage, color: "text-muted-foreground" }
  const StageIcon = meta.icon
  const StatusIcon = STATUS_ICON[stage.status] || Clock
  const isRunning = stage.status === "running"

  const details = stage.details || {}
  const tiers = details.tiers as Record<string, number> | undefined
  const reasons = details.reasons as Record<string, number> | undefined
  const samples = details.samples as string[] | undefined
  const tokens = details.tokens as Record<string, unknown> | undefined
  const error = details.error as string | undefined
  const crossRemoved = details.cross_job_removed as number | undefined
  const rejectedNames = details.rejected_names as string[] | undefined

  const hasDetails = !!(tiers || reasons || samples || tokens || error || crossRemoved || rejectedNames)

  return (
    <Collapsible>
      <CollapsibleTrigger className="w-full" disabled={!hasDetails}>
        <div className="flex items-center gap-2 py-1.5 px-2 rounded-md hover:bg-muted/50 transition-colors text-left group">
          <div className={cn("size-5 rounded flex items-center justify-center bg-muted/80 shrink-0", meta.color)}>
            <StageIcon className="size-3" />
          </div>
          <span className="text-xs font-medium flex-1 truncate">{meta.label}</span>
          <span className="text-[10px] text-muted-foreground tabular-nums">
            {stage.output_count > 0 && <span className="text-foreground font-medium">{stage.output_count}</span>}
            {stage.rejected_count > 0 && <span className="text-destructive/70 ml-1">-{stage.rejected_count}</span>}
          </span>
          <StatusIcon className={cn("size-3 shrink-0", STATUS_COLORS[stage.status], isRunning && "animate-spin")} />
          {hasDetails && <ChevronDown className="size-2.5 text-muted-foreground opacity-0 group-hover:opacity-100" />}
        </div>
      </CollapsibleTrigger>

      {hasDetails && (
        <CollapsibleContent>
          <div className="ml-7 mr-2 mb-1.5 p-2 rounded bg-muted/30 space-y-1 text-[10px]">
            {tiers && (
              <div className="flex flex-wrap gap-1">
                {Object.entries(tiers).map(([t, count]) => (
                  <Badge key={t} variant="outline" className="text-[9px] px-1 py-0">
                    {t === "hot" ? "🔥" : t === "warm" ? "🟡" : "🔵"} {t}: {count}
                  </Badge>
                ))}
              </div>
            )}
            {crossRemoved !== undefined && crossRemoved > 0 && (
              <div className="text-muted-foreground">{crossRemoved} cross-job dupes removed</div>
            )}
            {reasons && (
              <div className="text-muted-foreground">
                {Object.entries(reasons).slice(0, 4).map(([r, c]) => `${r.replace(/_/g, " ")}: ${c}`).join(" · ")}
              </div>
            )}
            {rejectedNames && rejectedNames.length > 0 && (
              <div className="flex flex-wrap gap-0.5">
                {rejectedNames.slice(0, 8).map((name, i) => (
                  <Badge key={i} variant="outline" className="text-[9px] px-1 py-0 border-destructive/20 text-destructive/70">{name}</Badge>
                ))}
                {rejectedNames.length > 8 && <span className="text-muted-foreground">+{rejectedNames.length - 8}</span>}
              </div>
            )}
            {tokens && (
              <div className="text-muted-foreground">
                {(tokens.total_tokens as number) ?? 0} tokens · {(tokens.calls as number) ?? 0} calls
              </div>
            )}
            {samples && samples.length > 0 && (
              <div className="text-muted-foreground truncate">
                {samples.slice(0, 4).join(", ")}{samples.length > 4 && ` +${samples.length - 4}`}
              </div>
            )}
            {error && <div className="text-destructive">{error}</div>}
          </div>
        </CollapsibleContent>
      )}
    </Collapsible>
  )
}

// ── Compact Card (for chat + list cards) ─────────────────────────

function CompactStageProgress({ stages }: { stages: JobStage[] }) {
  const total = stages.length || 1
  const completed = stages.filter(s => s.status === "done").length
  const failed = stages.filter(s => s.status === "failed").length
  const pct = Math.round(((completed + failed) / total) * 100)

  return (
    <div className="space-y-1">
      <div className="flex items-center justify-between text-xs text-muted-foreground">
        <span>{completed}/{total} stages</span>
        <span>{pct}%</span>
      </div>
      <div className="h-1.5 rounded-full bg-muted overflow-hidden">
        <div
          className={cn(
            "h-full rounded-full transition-all duration-500",
            failed > 0 ? "bg-destructive" : "bg-primary"
          )}
          style={{ width: `${pct}%` }}
        />
      </div>
    </div>
  )
}

// ── Main Export ──────────────────────────────────────────────────

export interface TaskDetailCardProps {
  jobId: string
  compact?: boolean
}

export function TaskDetailCard({ jobId, compact = false }: TaskDetailCardProps) {
  const navigate = useNavigate()
  const [job, setJob] = useState<JobDetail | null>(null)
  const [leads, setLeads] = useState<JobLead[]>([])
  const [loading, setLoading] = useState(true)

  const fetchData = async () => {
    try {
      const res = await fetch(`/api/jobs/${jobId}`)
      if (!res.ok) return
      const data = await res.json()
      setJob(data)

      if (!compact) {
        const leadsRes = await fetch(`/api/jobs/${jobId}/leads`)
        if (leadsRes.ok) {
          const leadsData = await leadsRes.json()
          setLeads(Array.isArray(leadsData) ? leadsData : leadsData.leads || [])
        }
      }
    } catch {
      // ignore
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    fetchData()
  }, [jobId])

  // Auto-refresh while running — polling fallback
  useEffect(() => {
    if (!job || job.status !== "running") return
    const interval = setInterval(fetchData, 2000)
    return () => clearInterval(interval)
  }, [job?.status])

  // Real-time SSE updates for running jobs
  useEffect(() => {
    if (!job || job.status !== "running") return
    let es: EventSource | null = null
    try {
      es = new EventSource("/api/events")
      es.onmessage = (e) => {
        try {
          const data = JSON.parse(e.data)
          if (data.type === "heartbeat") return
          const payload = data.payload || data
          if (payload.job_id === jobId) {
            fetchData()
          }
        } catch { /* ignore */ }
      }
      es.onerror = () => {
        es?.close()
      }
    } catch { /* SSE not supported */ }
    return () => es?.close()
  }, [job?.status, jobId])

  if (loading) {
    return compact ? (
      <Skeleton className="h-24 w-full rounded-lg" />
    ) : (
      <div className="space-y-3 p-4">
        <Skeleton className="h-6 w-64" />
        <Skeleton className="h-4 w-48" />
        {Array.from({ length: 4 }).map((_, i) => <Skeleton key={i} className="h-8 w-full" />)}
      </div>
    )
  }

  if (!job) {
    return (
      <div className="text-sm text-muted-foreground p-4 text-center">
        Task not found
      </div>
    )
  }

  const StatusIcon = STATUS_ICON[job.status] || Clock
  const isRunning = job.status === "running"

  // Server stores UTC timestamps without 'Z' suffix — force UTC parse
  const utc = (ts: string) => ts && !ts.endsWith("Z") ? ts + "Z" : ts

  const duration = job.completed_at && job.started_at
    ? Math.round((new Date(utc(job.completed_at)).getTime() - new Date(utc(job.started_at)).getTime()) / 1000)
    : job.started_at
      ? Math.round((Date.now() - new Date(utc(job.started_at)).getTime()) / 1000)
      : 0

  const formatDuration = (s: number) => {
    if (s < 60) return `${s}s`
    const m = Math.floor(s / 60)
    return `${m}m ${s % 60}s`
  }

  // ── Compact mode (chat / list cards) ───────────────────────────
  if (compact) {
    const handleOpenInWorkbook = async (e: React.MouseEvent) => {
      e.stopPropagation()
      try {
        const { createWorkbookFromJobs } = await import("@/lib/workbook-api")
        const wb = await createWorkbookFromJobs({ job_ids: [job.id] })
        toast.success(`Workbook "${wb.name}" created with ${wb.total_rows} leads`)
        navigate(`/workbooks/${wb.id}`)
      } catch (err) {
        toast.error("Failed to create workbook")
      }
    }

    return (
      <Card
        className="cursor-pointer hover:bg-accent/50 transition-colors border-l-2"
        style={{ borderLeftColor: isRunning ? "hsl(var(--primary))" : job.status === "done" ? "hsl(142, 76%, 36%)" : job.status === "failed" ? "hsl(var(--destructive))" : "transparent" }}
        onClick={() => navigate(`/agents/${job.id}`)}
      >
        <CardContent className="p-3 space-y-2">
          <div className="flex items-start justify-between gap-2">
            <div className="flex-1 min-w-0">
              <div className="text-sm font-medium truncate">{job.query}</div>
              <div className="flex items-center gap-2 mt-0.5">
                <StatusIcon className={cn("size-3.5", STATUS_COLORS[job.status], isRunning && "animate-spin")} />
                <span className="text-xs text-muted-foreground capitalize">{job.status}</span>
                {job.leads_found > 0 && (
                  <Badge variant="outline" className="text-[10px] py-0">{job.leads_found} leads</Badge>
                )}
                {duration > 0 && (
                  <span className="text-xs text-muted-foreground">{formatDuration(duration)}</span>
                )}
              </div>
            </div>
            <div className="flex items-center gap-1 shrink-0 mt-1">
              {job.status === "done" && job.leads_found > 0 && (
                <button
                  onClick={handleOpenInWorkbook}
                  className="p-1 rounded-md hover:bg-primary/10 text-muted-foreground hover:text-primary transition-colors"
                  title="Open in Workbook"
                >
                  <Database className="size-3.5" />
                </button>
              )}
              <ExternalLink className="size-3.5 text-muted-foreground" />
            </div>
          </div>
          {job.stages?.length > 0 && <CompactStageProgress stages={job.stages} />}
        </CardContent>
      </Card>
    )
  }

  // ── Full mode (detail page) — compact single-screen layout ────

  const sourceStages = (job.stages || []).filter(s =>
    ["maps", "web", "directories", "linkedin", "job_boards", "review_sites"].includes(s.stage)
  )
  const totalDiscovered = sourceStages.reduce((sum, s) => sum + s.output_count, 0)
  const validateStage = job.stages?.find(s => s.stage === "validate")
  const dedupStage = job.stages?.find(s => s.stage === "dedup")
  const scoreStage = job.stages?.find(s => s.stage === "score")
  const storeStage = job.stages?.find(s => s.stage === "store")

  const totalStages = (job.stages || []).length || 1
  const completedStages = (job.stages || []).filter(s => s.status === "done").length
  const pipelinePct = Math.round((completedStages / totalStages) * 100)

  return (
    <div className="space-y-3">
      {/* ── Header: title + meta + actions all in one block ── */}
      <div className="flex items-start gap-3">
        <StatusIcon className={cn("size-5 mt-0.5 shrink-0", STATUS_COLORS[job.status], isRunning && "animate-spin")} />
        <div className="flex-1 min-w-0">
          <h2 className="text-base font-semibold leading-tight truncate">{job.query}</h2>
          <div className="flex items-center gap-2 mt-1 flex-wrap">
            <Badge variant={job.status === "done" ? "default" : job.status === "failed" ? "destructive" : "secondary"} className="text-[10px]">
              {job.status}
            </Badge>
            {job.leads_found > 0 && (
              <span className="text-xs text-muted-foreground">{job.leads_found} leads</span>
            )}
            {duration > 0 && (
              <span className="text-xs text-muted-foreground">{formatDuration(duration)}</span>
            )}
            <span className="text-[10px] text-muted-foreground">
              {new Date(job.created_at).toLocaleString()}
            </span>
          </div>
        </div>
        {/* Actions */}
        <div className="flex items-center gap-1 shrink-0">
          {(job.status === "running" || job.status === "pending") && (
            <Button variant="outline" size="sm" className="h-6 text-[10px] gap-0.5 px-2 text-destructive hover:text-destructive" onClick={async () => {
              await fetch(`/api/jobs/${job.id}/cancel`, { method: "POST" })
              queryClient.invalidateQueries({ queryKey: queryKeys.jobs.all })
              toast.success("Task cancelled")
              fetchData()
            }}>
              <StopCircle className="size-3" /> Stop
            </Button>
          )}
          {(job.status === "failed" || job.status === "cancelled") && (
            <Button variant="outline" size="sm" className="h-6 text-[10px] gap-0.5 px-2" onClick={async () => {
              await fetch(`/api/jobs/${job.id}/retry`, { method: "POST" })
              queryClient.invalidateQueries({ queryKey: queryKeys.jobs.all })
              toast.success("Queued for retry")
              fetchData()
            }}>
              <RefreshCw className="size-3" /> Retry
            </Button>
          )}
          {job.status === "done" && job.leads_found > 0 && (
            <Button variant="outline" size="sm" className="h-6 text-[10px] gap-0.5 px-2" onClick={async () => {
              try {
                const { createWorkbookFromJobs } = await import("@/lib/workbook-api")
                const wb = await createWorkbookFromJobs({ job_ids: [job.id] })
                toast.success(`Workbook "${wb.name}" created`)
                navigate(`/workbooks/${wb.id}`)
              } catch {
                toast.error("Failed to create workbook")
              }
            }}>
              <Database className="size-3" /> Workbook
            </Button>
          )}
          <Button variant="ghost" size="sm" className="h-6 text-[10px] gap-0.5 px-1.5" onClick={async () => {
            await fetch(`/api/jobs/${job.id}?keep_leads=true`, { method: "DELETE" })
            queryClient.invalidateQueries({ queryKey: queryKeys.jobs.all })
            toast.success("Task removed (leads kept)")
            navigate("/agents")
          }}>
            <FileX2 className="size-3" />
          </Button>
          <Button variant="ghost" size="sm" className="h-6 text-[10px] gap-0.5 px-1.5 text-destructive hover:text-destructive" onClick={async () => {
            if (!confirm(`Delete "${job.query}" and all its leads?`)) return
            await fetch(`/api/jobs/${job.id}?keep_leads=false`, { method: "DELETE" })
            queryClient.invalidateQueries({ queryKey: queryKeys.jobs.all })
            queryClient.invalidateQueries({ queryKey: queryKeys.leads.all })
            toast.success("Task and leads deleted")
            navigate("/agents")
          }}>
            <Trash2 className="size-3" />
          </Button>
        </div>
      </div>

      {/* ── Metrics: inline row ── */}
      <div className="flex items-center gap-2 flex-wrap">
        <MetricPill icon={Globe} label="Discovered" value={totalDiscovered} />
        <MetricPill icon={Shield} label="Valid" value={validateStage?.output_count ?? 0} sub={validateStage?.rejected_count ? `-${validateStage.rejected_count}` : undefined} />
        <MetricPill icon={Layers} label="Unique" value={dedupStage?.output_count ?? 0} />
        <MetricPill icon={Brain} label="Scored" value={scoreStage?.output_count ?? 0} />
        {storeStage && <MetricPill icon={Database} label="Stored" value={storeStage.output_count} />}
        {/* Pipeline progress */}
        <div className="ml-auto flex items-center gap-1.5 text-xs text-muted-foreground">
          <span className="tabular-nums">{completedStages}/{totalStages}</span>
          <div className="w-16 h-1.5 rounded-full bg-muted overflow-hidden">
            <div className="h-full rounded-full bg-primary transition-all duration-300" style={{ width: `${pipelinePct}%` }} />
          </div>
          <span className="tabular-nums">{pipelinePct}%</span>
        </div>
      </div>

      <Separator />

      {/* ── Pipeline Stages: 2-column grid ── */}
      <div>
        <h3 className="text-xs font-medium mb-1.5 flex items-center gap-1.5 text-muted-foreground">
          <Zap className="size-3" />
          Pipeline ({(job.stages || []).length} stages)
        </h3>
        <div className="grid grid-cols-2 gap-x-2 gap-y-0">
          {(job.stages || []).map(stage => (
            <StageRow key={stage.id} stage={stage} />
          ))}
        </div>
        {(!job.stages || job.stages.length === 0) && (
          <div className="text-xs text-muted-foreground py-3 text-center">No stages recorded</div>
        )}
      </div>

      {job.error && (
        <>
          <Separator />
          <div className="text-xs text-destructive">
            <strong>Error:</strong> {job.error}
          </div>
        </>
      )}

      {/* ── Leads Table: compact ── */}
      {leads.length > 0 && (
        <>
          <Separator />
          <div>
            <h3 className="text-xs font-medium mb-1.5 flex items-center gap-1.5 text-muted-foreground">
              <Users className="size-3" />
              Leads ({leads.length})
            </h3>
            <div className="rounded-md border overflow-hidden">
              <table className="w-full text-xs">
                <thead className="bg-muted/50">
                  <tr>
                    <th className="text-left px-2 py-1.5 font-medium text-muted-foreground">Company</th>
                    <th className="text-left px-2 py-1.5 font-medium text-muted-foreground">Email</th>
                    <th className="text-left px-2 py-1.5 font-medium text-muted-foreground">City</th>
                    <th className="text-left px-2 py-1.5 font-medium text-muted-foreground">Score</th>
                    <th className="text-left px-2 py-1.5 font-medium text-muted-foreground">Source</th>
                  </tr>
                </thead>
                <tbody>
                  {leads.slice(0, 20).map(lead => (
                    <tr
                      key={lead.id}
                      className="border-t border-border/30 hover:bg-muted/30 cursor-pointer transition-colors"
                      onClick={() => navigate(`/leads/${lead.id}`)}
                    >
                      <td className="px-2 py-1 font-medium truncate max-w-[160px]">{lead.company}</td>
                      <td className="px-2 py-1 text-muted-foreground truncate max-w-[180px]">{lead.email || "—"}</td>
                      <td className="px-2 py-1 text-muted-foreground">{lead.city || "—"}</td>
                      <td className="px-2 py-1">
                        {lead.score_tier && (
                          <Badge variant="outline" className={cn("text-[9px] px-1 py-0",
                            lead.score_tier === "hot" && "border-red-500/30 text-red-500",
                            lead.score_tier === "warm" && "border-yellow-500/30 text-yellow-500",
                            lead.score_tier === "cold" && "border-blue-500/30 text-blue-500",
                          )}>
                            {lead.score_tier === "hot" ? "🔥" : lead.score_tier === "warm" ? "🟡" : "🔵"} {lead.score}
                          </Badge>
                        )}
                      </td>
                      <td className="px-2 py-1 text-muted-foreground">{lead.source}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
              {leads.length > 20 && (
                <div className="text-center py-1.5 text-[10px] text-muted-foreground border-t">
                  +{leads.length - 20} more leads
                </div>
              )}
            </div>
          </div>
        </>
      )}
    </div>
  )
}

// ── Metric Pill (inline) ─────────────────────────────────────────

function MetricPill({ label, value, icon: Icon, sub }: {
  label: string; value: number; icon: typeof Globe; sub?: string
}) {
  return (
    <div className="inline-flex items-center gap-1.5 px-2.5 py-1 rounded-md border bg-card text-xs">
      <Icon className="size-3 text-muted-foreground" />
      <span className="text-muted-foreground">{label}</span>
      <span className="font-semibold tabular-nums">{value}</span>
      {sub && <span className="text-destructive/70 text-[10px]">{sub}</span>}
    </div>
  )
}

export default TaskDetailCard
