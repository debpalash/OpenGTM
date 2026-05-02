import { useState, useEffect } from "react"
import { useParams, useNavigate } from "react-router-dom"
import { ArrowLeft, Loader2, CheckCircle2, XCircle, SkipForward, Clock, ArrowDown, Users, ChevronDown, ChevronUp } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Card } from "@/components/ui/card"
import { Badge } from "@/components/ui/badge"

const API = ""

interface Stage {
  id: number; stage: string; status: string
  input_count: number; output_count: number; rejected_count: number
  details: Record<string, any>
  started_at: string; completed_at: string
}
interface Job {
  id: string; query: string; status: string; leads_found: number
  error: string; created_at: string; started_at: string; completed_at: string
  workspace_id: string; stages: Stage[]
}
interface Lead {
  id: number; company: string; email: string; phone: string
  city: string; score: number; score_tier: string; website: string
}

const STAGE_META: Record<string, { icon: string; label: string; color: string }> = {
  maps:        { icon: "🗺️", label: "Google Maps",    color: "text-blue-400" },
  web:         { icon: "🌐", label: "Web Search",     color: "text-cyan-400" },
  directories: { icon: "📂", label: "Directories",    color: "text-purple-400" },
  validate:    { icon: "🔍", label: "Validation",     color: "text-amber-400" },
  dedup:       { icon: "🔄", label: "Deduplication",  color: "text-orange-400" },
  score:       { icon: "⭐", label: "Scoring",        color: "text-yellow-400" },
  store:       { icon: "💾", label: "Storage",        color: "text-emerald-400" },
}

export default function PipelinePage() {
  const { jobId } = useParams<{ jobId: string }>()
  const navigate = useNavigate()
  const [job, setJob] = useState<Job | null>(null)
  const [leads, setLeads] = useState<Lead[]>([])
  const [loading, setLoading] = useState(true)
  const [expanded, setExpanded] = useState<Record<number, boolean>>({})

  useEffect(() => {
    if (!jobId) return
    Promise.all([
      fetch(`${API}/api/jobs/${jobId}`).then(r => r.json()),
      fetch(`${API}/api/jobs/${jobId}/leads`).then(r => r.json()),
    ]).then(([j, l]) => {
      setJob(j); setLeads(l)
    }).finally(() => setLoading(false))
  }, [jobId])

  const toggle = (id: number) => setExpanded(p => ({ ...p, [id]: !p[id] }))

  if (loading) return (
    <div className="flex items-center justify-center h-full">
      <Loader2 className="animate-spin text-primary" size={28} />
    </div>
  )
  if (!job) return (
    <div className="flex flex-col items-center justify-center h-full gap-3">
      <XCircle size={32} className="text-muted-foreground/30" />
      <p className="text-sm text-muted-foreground">Job not found</p>
      <Button variant="ghost" size="sm" onClick={() => navigate("/leads")}>
        <ArrowLeft size={14} className="mr-1" /> Back to Leads
      </Button>
    </div>
  )

  const statusBadge = (s: string) => {
    const map: Record<string, string> = {
      done: "bg-emerald-500/10 text-emerald-400 border-emerald-500/20",
      running: "bg-blue-500/10 text-blue-400 border-blue-500/20",
      failed: "bg-red-500/10 text-red-400 border-red-500/20",
      pending: "bg-amber-500/10 text-amber-400 border-amber-500/20",
      skipped: "bg-zinc-500/10 text-zinc-400 border-zinc-500/20",
    }
    return map[s] || map.pending
  }

  const stageIcon = (status: string) => {
    if (status === "done") return <CheckCircle2 size={14} className="text-emerald-400" />
    if (status === "failed") return <XCircle size={14} className="text-red-400" />
    if (status === "skipped") return <SkipForward size={14} className="text-zinc-400" />
    if (status === "running") return <Loader2 size={14} className="animate-spin text-blue-400" />
    return <Clock size={14} className="text-muted-foreground" />
  }

  const duration = (start: string, end: string) => {
    if (!start || !end) return ""
    const ms = new Date(end).getTime() - new Date(start).getTime()
    return ms < 1000 ? `${ms}ms` : `${(ms / 1000).toFixed(1)}s`
  }

  return (
    <div className="flex flex-col h-full overflow-hidden">
      {/* Header */}
      <div className="p-4 border-b border-border shrink-0">
        <div className="flex items-center gap-3">
          <Button variant="ghost" size="icon" className="h-7 w-7" onClick={() => navigate("/leads")}>
            <ArrowLeft size={14} />
          </Button>
          <div className="flex-1 min-w-0">
            <div className="flex items-center gap-2">
              <h2 className="text-sm font-semibold truncate">Pipeline: {job.query}</h2>
              <Badge variant="secondary" className={`text-[9px] h-4 ${statusBadge(job.status)}`}>
                {job.status}
              </Badge>
            </div>
            <div className="flex items-center gap-3 text-[10px] text-muted-foreground mt-0.5">
              <span>ID: <code className="text-[9px]">{job.id}</code></span>
              <span>{job.leads_found} leads</span>
              {job.created_at && <span>{new Date(job.created_at).toLocaleString()}</span>}
              {job.started_at && job.completed_at && <span>{duration(job.started_at, job.completed_at)} total</span>}
            </div>
          </div>
        </div>
      </div>

      {/* Content */}
      <div className="flex-1 overflow-y-auto min-h-0">
        <div className="p-4 max-w-3xl mx-auto">

          {/* Pipeline Stages */}
          <div className="space-y-0">
            {job.stages.map((s, i) => {
              const meta = STAGE_META[s.stage] || { icon: "⚙️", label: s.stage, color: "text-muted-foreground" }
              const isExpanded = expanded[s.id]
              const hasDetails = s.details && Object.keys(s.details).length > 0

              return (
                <div key={s.id}>
                  {/* Connector line */}
                  {i > 0 && (
                    <div className="flex justify-center py-0.5">
                      <ArrowDown size={12} className="text-border" />
                    </div>
                  )}

                  {/* Stage card */}
                  <Card
                    className={`p-3 transition-all cursor-pointer hover:bg-card/80 ${
                      s.status === "failed" ? "border-red-500/30" : s.status === "skipped" ? "opacity-50" : ""
                    }`}
                    onClick={() => hasDetails && toggle(s.id)}
                  >
                    <div className="flex items-center justify-between gap-3">
                      <div className="flex items-center gap-2.5 min-w-0">
                        {stageIcon(s.status)}
                        <span className="text-base">{meta.icon}</span>
                        <div>
                          <span className={`text-[11px] font-semibold ${meta.color}`}>{meta.label}</span>
                          {s.output_count > 0 && (
                            <span className="text-[10px] text-muted-foreground ml-2">
                              {s.input_count > 0 ? `${s.input_count} → ${s.output_count}` : `${s.output_count} found`}
                              {s.rejected_count > 0 && <span className="text-red-400/70 ml-1">(-{s.rejected_count})</span>}
                            </span>
                          )}
                        </div>
                      </div>
                      <div className="flex items-center gap-2 shrink-0">
                        {s.started_at && s.completed_at && (
                          <span className="text-[9px] text-muted-foreground font-mono">{duration(s.started_at, s.completed_at)}</span>
                        )}
                        <Badge variant="secondary" className={`text-[8px] h-3.5 ${statusBadge(s.status)}`}>
                          {s.status}
                        </Badge>
                        {hasDetails && (
                          isExpanded ? <ChevronUp size={12} className="text-muted-foreground" /> : <ChevronDown size={12} className="text-muted-foreground" />
                        )}
                      </div>
                    </div>

                    {/* Expanded details */}
                    {isExpanded && hasDetails && (
                      <div className="mt-3 pt-3 border-t border-border space-y-2">
                        {s.details.samples && (
                          <div>
                            <span className="text-[9px] text-muted-foreground font-medium">Sample leads:</span>
                            <div className="flex flex-wrap gap-1 mt-1">
                              {(s.details.samples as string[]).map((name, j) => (
                                <Badge key={j} variant="outline" className="text-[8px] h-4 font-normal">{name}</Badge>
                              ))}
                            </div>
                          </div>
                        )}
                        {s.details.reasons && Object.keys(s.details.reasons).length > 0 && (
                          <div>
                            <span className="text-[9px] text-muted-foreground font-medium">Rejection reasons:</span>
                            <div className="grid grid-cols-2 gap-1 mt-1">
                              {Object.entries(s.details.reasons as Record<string, number>).map(([r, c]) => (
                                <div key={r} className="flex items-center justify-between text-[9px] px-2 py-0.5 rounded bg-red-500/5 border border-red-500/10">
                                  <span className="text-red-300/80 truncate">{r}</span>
                                  <span className="text-red-400 font-mono ml-1">{c}</span>
                                </div>
                              ))}
                            </div>
                          </div>
                        )}
                        {s.details.rejected_names && (
                          <div>
                            <span className="text-[9px] text-muted-foreground font-medium">Rejected entries:</span>
                            <div className="flex flex-wrap gap-1 mt-1">
                              {(s.details.rejected_names as string[]).map((name, j) => (
                                <Badge key={j} variant="outline" className="text-[8px] h-4 font-normal text-red-400/70 border-red-500/20">{name}</Badge>
                              ))}
                            </div>
                          </div>
                        )}
                        {s.details.tiers && (
                          <div>
                            <span className="text-[9px] text-muted-foreground font-medium">Score distribution:</span>
                            <div className="flex gap-2 mt-1">
                              {Object.entries(s.details.tiers as Record<string, number>).map(([tier, count]) => (
                                <Badge key={tier} variant="secondary" className={`text-[8px] h-4 ${
                                  tier === "hot" ? "bg-emerald-500/10 text-emerald-400" :
                                  tier === "warm" ? "bg-amber-500/10 text-amber-400" :
                                  "bg-zinc-500/10 text-zinc-400"
                                }`}>{tier}: {count}</Badge>
                              ))}
                            </div>
                          </div>
                        )}
                        {s.details.error && (
                          <p className="text-[9px] text-red-400 bg-red-500/5 px-2 py-1 rounded">{s.details.error}</p>
                        )}
                      </div>
                    )}
                  </Card>
                </div>
              )
            })}
          </div>

          {/* Leads Table */}
          {leads.length > 0 && (
            <div className="mt-6">
              <h3 className="text-xs font-semibold mb-2 flex items-center gap-1.5">
                <Users size={13} className="text-primary" />
                Leads Produced ({leads.length})
              </h3>
              <Card className="overflow-hidden">
                <table className="w-full text-[10px]">
                  <thead>
                    <tr className="border-b border-border bg-card/50">
                      <th className="text-left px-3 py-1.5 font-medium text-muted-foreground">Score</th>
                      <th className="text-left px-3 py-1.5 font-medium text-muted-foreground">Company</th>
                      <th className="text-left px-3 py-1.5 font-medium text-muted-foreground">City</th>
                      <th className="text-left px-3 py-1.5 font-medium text-muted-foreground">Email</th>
                      <th className="text-left px-3 py-1.5 font-medium text-muted-foreground">Phone</th>
                    </tr>
                  </thead>
                  <tbody>
                    {leads.map(l => (
                      <tr key={l.id} className="border-b border-border/40 hover:bg-card/40">
                        <td className="px-3 py-1.5">
                          <Badge variant="secondary" className={`text-[8px] h-3.5 ${
                            l.score >= 60 ? "bg-emerald-500/10 text-emerald-400" :
                            l.score >= 30 ? "bg-amber-500/10 text-amber-400" :
                            "bg-zinc-500/10 text-zinc-400"
                          }`}>{l.score}</Badge>
                        </td>
                        <td className="px-3 py-1.5 font-medium">{l.company}</td>
                        <td className="px-3 py-1.5 text-muted-foreground">{l.city || "—"}</td>
                        <td className="px-3 py-1.5 text-muted-foreground font-mono">{l.email || "—"}</td>
                        <td className="px-3 py-1.5 text-muted-foreground font-mono">{l.phone || "—"}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </Card>
            </div>
          )}

          {leads.length === 0 && job.status === "done" && (
            <Card className="mt-6 p-6 text-center">
              <p className="text-xs text-muted-foreground">No leads produced — all entries were rejected by validation</p>
            </Card>
          )}
        </div>
      </div>
    </div>
  )
}
