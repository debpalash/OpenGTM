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
  maps:             { icon: MapPin,     label: "Google Maps",       color: "text-red-500" },
  web:              { icon: Globe,      label: "Web Search",        color: "text-blue-500" },
  directories:      { icon: FileSearch, label: "Directories",       color: "text-amber-500" },
  linkedin:         { icon: Link2,      label: "LinkedIn",          color: "text-sky-600" },
  job_boards:       { icon: Briefcase,  label: "Job Boards",        color: "text-emerald-500" },
  review_sites:     { icon: Star,       label: "Review Sites",      color: "text-yellow-500" },
  validate:         { icon: Shield,     label: "Validation",        color: "text-violet-500" },
  dedup:            { icon: Layers,     label: "Deduplication",     color: "text-orange-500" },
  score:            { icon: Brain,      label: "AI Scoring",        color: "text-purple-500" },
  enrich:           { icon: Sparkles,   label: "Enrichment",        color: "text-cyan-500" },
  decision_makers:  { icon: Users,      label: "Decision Makers",   color: "text-pink-500" },
  personal_emails:  { icon: Mail,       label: "Personal Emails",   color: "text-teal-500" },
  crosslinked:      { icon: Link2,     label: "LinkedIn People",   color: "text-blue-600" },
  hiring_signals:   { icon: TrendingUp, label: "Hiring Signals",    color: "text-green-500" },
  smtp_verify:      { icon: ShieldCheck, label: "SMTP Verification", color: "text-indigo-500" },
  store:            { icon: Database,   label: "Storage",            color: "text-slate-500" },
}

// ── Stage Component ──────────────────────────────────────────────

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
        <div className="flex items-center gap-3 py-2 px-3 rounded-lg hover:bg-muted/50 transition-colors text-left group">
          {/* Stage Icon */}
          <div className={cn("size-7 rounded-md flex items-center justify-center bg-muted/80 shrink-0", meta.color)}>
            <StageIcon className="size-3.5" />
          </div>

          {/* Label + counts */}
          <div className="flex-1 min-w-0">
            <div className="flex items-center gap-2">
              <span className="text-sm font-medium">{meta.label}</span>
              {details.ai && (
                <Badge variant="secondary" className="text-[10px] px-1.5 py-0 gap-0.5">
                  <Brain className="size-2.5" /> AI
                </Badge>
              )}
            </div>
            <div className="text-xs text-muted-foreground mt-0.5">
              {stage.input_count > 0 && <span>{stage.input_count} in → </span>}
              <span className="font-medium text-foreground">{stage.output_count}</span> out
              {stage.rejected_count > 0 && <span className="text-destructive/70"> · {stage.rejected_count} rejected</span>}
            </div>
          </div>

          {/* Status */}
          <StatusIcon className={cn(
            "size-4 shrink-0",
            STATUS_COLORS[stage.status],
            isRunning && "animate-spin"
          )} />

          {/* Expand indicator */}
          {hasDetails && (
            <ChevronDown className="size-3.5 text-muted-foreground opacity-0 group-hover:opacity-100 transition-opacity" />
          )}
        </div>
      </CollapsibleTrigger>

      {hasDetails && (
        <CollapsibleContent>
          <div className="ml-10 mr-3 mb-2 p-2.5 rounded-md bg-muted/30 space-y-1.5 text-xs">
            {tiers && (
              <div className="flex flex-wrap gap-1.5">
                {Object.entries(tiers).map(([t, count]) => (
                  <Badge key={t} variant="outline" className="text-[10px]">
                    {t === "hot" ? "🔥" : t === "warm" ? "🟡" : "🔵"} {t}: {count}
                  </Badge>
                ))}
              </div>
            )}
            {crossRemoved !== undefined && crossRemoved > 0 && (
              <div className="text-muted-foreground">{crossRemoved} duplicates removed (already in DB)</div>
            )}
            {reasons && (
              <div className="text-muted-foreground space-y-0.5">
                {Object.entries(reasons).slice(0, 5).map(([reason, count]) => (
                  <div key={reason}>{reason.replace(/_/g, " ")}: {count as number}</div>
                ))}
              </div>
            )}
            {rejectedNames && rejectedNames.length > 0 && (
              <div className="space-y-1">
                <div className="text-muted-foreground font-medium flex items-center gap-1">
                  <FileX2 className="size-3" /> Rejected companies:
                </div>
                <div className="flex flex-wrap gap-1">
                  {rejectedNames.map((name, i) => (
                    <Badge key={i} variant="outline" className="text-[10px] border-destructive/20 text-destructive/70 bg-destructive/5">
                      {name}
                    </Badge>
                  ))}
                </div>
              </div>
            )}
            {tokens && (
              <div className="text-muted-foreground">
                Tokens: {(tokens.total_tokens as number) ?? 0} ({(tokens.provider as string) ?? "—"}) · {(tokens.calls as number) ?? 0} calls
              </div>
            )}
            {samples && samples.length > 0 && (
              <div className="text-muted-foreground">
                Found: {samples.slice(0, 6).join(", ")}
                {samples.length > 6 && ` +${samples.length - 6} more`}
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

  // Auto-refresh while running
  useEffect(() => {
    if (!job || job.status !== "running") return
    const interval = setInterval(fetchData, 3000)
    return () => clearInterval(interval)
  }, [job?.status])

  if (loading) {
    return compact ? (
      <Skeleton className="h-24 w-full rounded-lg" />
    ) : (
      <div className="space-y-4 p-6">
        <Skeleton className="h-8 w-64" />
        <Skeleton className="h-4 w-48" />
        {Array.from({ length: 5 }).map((_, i) => <Skeleton key={i} className="h-12 w-full" />)}
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
            <ExternalLink className="size-3.5 text-muted-foreground shrink-0 mt-1" />
          </div>
          {job.stages?.length > 0 && <CompactStageProgress stages={job.stages} />}
        </CardContent>
      </Card>
    )
  }

  // ── Full mode (detail page) ────────────────────────────────────

  // Compute metrics
  const sourceStages = (job.stages || []).filter(s =>
    ["maps", "web", "directories", "linkedin", "job_boards", "review_sites"].includes(s.stage)
  )
  const totalDiscovered = sourceStages.reduce((sum, s) => sum + s.output_count, 0)
  const validateStage = job.stages?.find(s => s.stage === "validate")
  const dedupStage = job.stages?.find(s => s.stage === "dedup")
  const scoreStage = job.stages?.find(s => s.stage === "score")

  return (
    <div className="space-y-6">
      {/* Header */}
      <div>
        <div className="flex items-center gap-3">
          <StatusIcon className={cn("size-5", STATUS_COLORS[job.status], isRunning && "animate-spin")} />
          <h2 className="text-lg font-semibold flex-1">{job.query}</h2>
          {/* Action buttons */}
          <div className="flex items-center gap-1.5">
            {(job.status === "running" || job.status === "pending") && (
              <Button variant="outline" size="sm" className="h-7 text-xs gap-1 text-destructive hover:text-destructive" onClick={async () => {
                await fetch(`/api/jobs/${job.id}/cancel`, { method: "POST" })
                queryClient.invalidateQueries({ queryKey: queryKeys.jobs.all })
                toast.success("Task cancelled")
                fetchData()
              }}>
                <StopCircle className="size-3.5" /> Stop
              </Button>
            )}
            {(job.status === "failed" || job.status === "cancelled") && (
              <Button variant="outline" size="sm" className="h-7 text-xs gap-1" onClick={async () => {
                await fetch(`/api/jobs/${job.id}/retry`, { method: "POST" })
                queryClient.invalidateQueries({ queryKey: queryKeys.jobs.all })
                toast.success("Queued for retry")
                fetchData()
              }}>
                <RefreshCw className="size-3.5" /> Retry
              </Button>
            )}
            <Button variant="ghost" size="sm" className="h-7 text-xs gap-1" onClick={async () => {
              await fetch(`/api/jobs/${job.id}?keep_leads=true`, { method: "DELETE" })
              queryClient.invalidateQueries({ queryKey: queryKeys.jobs.all })
              toast.success("Task removed (leads kept)")
              navigate("/agents")
            }}>
              <FileX2 className="size-3.5" /> Remove
            </Button>
            <Button variant="ghost" size="sm" className="h-7 text-xs gap-1 text-destructive hover:text-destructive" onClick={async () => {
              if (!confirm(`Delete "${job.query}" and all its leads?`)) return
              await fetch(`/api/jobs/${job.id}?keep_leads=false`, { method: "DELETE" })
              queryClient.invalidateQueries({ queryKey: queryKeys.jobs.all })
              queryClient.invalidateQueries({ queryKey: queryKeys.leads.all })
              toast.success("Task and leads deleted")
              navigate("/agents")
            }}>
              <Trash2 className="size-3.5" /> Delete All
            </Button>
          </div>
        </div>
        <div className="flex items-center gap-3 mt-1.5 ml-8">
          <Badge variant={job.status === "done" ? "default" : job.status === "failed" ? "destructive" : "secondary"}>
            {job.status}
          </Badge>
          {job.leads_found > 0 && (
            <span className="text-sm text-muted-foreground">{job.leads_found} leads stored</span>
          )}
          {duration > 0 && (
            <span className="text-sm text-muted-foreground">{formatDuration(duration)}</span>
          )}
          <span className="text-xs text-muted-foreground">
            {new Date(job.created_at).toLocaleString()}
          </span>
        </div>
      </div>

      {/* Metrics Row */}
      <div className="grid grid-cols-4 gap-3">
        <MetricCard label="Discovered" value={totalDiscovered} icon={Globe} />
        <MetricCard label="Validated" value={validateStage?.output_count ?? 0} icon={Shield} sub={validateStage ? `${validateStage.rejected_count} rejected` : undefined} />
        <MetricCard label="Unique" value={dedupStage?.output_count ?? 0} icon={Layers} sub={dedupStage?.details?.cross_job_removed ? `${dedupStage.details.cross_job_removed} cross-job` : undefined} />
        <MetricCard label="Scored" value={scoreStage?.output_count ?? 0} icon={Brain} sub={scoreStage?.details?.tiers ? Object.entries(scoreStage.details.tiers as Record<string, number>).map(([t, c]) => `${t}:${c}`).join(" ") : undefined} />
      </div>

      <Separator />

      {/* Pipeline Stages */}
      <div>
        <h3 className="text-sm font-medium mb-2 flex items-center gap-2">
          <Zap className="size-4" />
          Pipeline Stages
        </h3>
        <div className="space-y-0.5">
          {(job.stages || []).map(stage => (
            <StageRow key={stage.id} stage={stage} />
          ))}
          {(!job.stages || job.stages.length === 0) && (
            <div className="text-sm text-muted-foreground py-4 text-center">No stages recorded</div>
          )}
        </div>
      </div>

      {job.error && (
        <>
          <Separator />
          <div className="text-sm text-destructive">
            <strong>Error:</strong> {job.error}
          </div>
        </>
      )}

      {/* Leads Table */}
      {leads.length > 0 && (
        <>
          <Separator />
          <div>
            <h3 className="text-sm font-medium mb-2 flex items-center gap-2">
              <Users className="size-4" />
              Leads Found ({leads.length})
            </h3>
            <div className="rounded-lg border overflow-hidden">
              <table className="w-full text-sm">
                <thead className="bg-muted/50">
                  <tr>
                    <th className="text-left px-3 py-2 text-xs font-medium text-muted-foreground">Company</th>
                    <th className="text-left px-3 py-2 text-xs font-medium text-muted-foreground">Email</th>
                    <th className="text-left px-3 py-2 text-xs font-medium text-muted-foreground">City</th>
                    <th className="text-left px-3 py-2 text-xs font-medium text-muted-foreground">Score</th>
                    <th className="text-left px-3 py-2 text-xs font-medium text-muted-foreground">Source</th>
                  </tr>
                </thead>
                <tbody>
                  {leads.slice(0, 25).map(lead => (
                    <tr
                      key={lead.id}
                      className="border-t border-border/30 hover:bg-muted/30 cursor-pointer transition-colors"
                      onClick={() => navigate(`/leads/${lead.id}`)}
                    >
                      <td className="px-3 py-2 font-medium">{lead.company}</td>
                      <td className="px-3 py-2 text-muted-foreground">{lead.email || "—"}</td>
                      <td className="px-3 py-2 text-muted-foreground">{lead.city || "—"}</td>
                      <td className="px-3 py-2">
                        {lead.score_tier && (
                          <Badge variant="outline" className={cn("text-[10px]",
                            lead.score_tier === "hot" && "border-red-500/30 text-red-500",
                            lead.score_tier === "warm" && "border-yellow-500/30 text-yellow-500",
                            lead.score_tier === "cold" && "border-blue-500/30 text-blue-500",
                          )}>
                            {lead.score_tier === "hot" ? "🔥" : lead.score_tier === "warm" ? "🟡" : "🔵"} {lead.score}
                          </Badge>
                        )}
                      </td>
                      <td className="px-3 py-2 text-muted-foreground text-xs">{lead.source}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
              {leads.length > 25 && (
                <div className="text-center py-2 text-xs text-muted-foreground border-t">
                  +{leads.length - 25} more leads
                </div>
              )}
            </div>
          </div>
        </>
      )}
    </div>
  )
}

// ── Metric Card ──────────────────────────────────────────────────

function MetricCard({ label, value, icon: Icon, sub }: {
  label: string
  value: number
  icon: typeof Globe
  sub?: string
}) {
  return (
    <div className="rounded-lg border bg-card p-3">
      <div className="flex items-center gap-2 text-xs text-muted-foreground mb-1">
        <Icon className="size-3.5" />
        {label}
      </div>
      <div className="text-2xl font-bold tracking-tight">{value}</div>
      {sub && <div className="text-[10px] text-muted-foreground mt-0.5">{sub}</div>}
    </div>
  )
}

export default TaskDetailCard
