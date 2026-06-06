import { useCallback, useEffect, useRef, useState } from "react"
import { Badge } from "@/components/ui/badge"
import { ScrollArea } from "@/components/ui/scroll-area"
import { fetchJobs, fetchSystemStats, type Job, type SystemStats } from "@/lib/api"
import { authQuery } from "@/lib/auth"

interface SSEEvent {
  type: string
  message?: string
  job_id?: string
  query?: string
  leads_found?: number
  rejected?: number
  step?: number
  total?: number
  stage?: string
  error?: string
  ts?: number
  lead?: { company?: string; email?: string; phone?: string; city?: string; score?: number; score_tier?: string }
}

interface Props {
  onRefresh?: () => void
  workspaceId?: string
}

export function LiveFeed({ onRefresh, workspaceId: _workspaceId }: Props) {
  const [events, setEvents] = useState<SSEEvent[]>([])
  const [jobs, setJobs] = useState<Job[]>([])
  const [sysStats, setSysStats] = useState<SystemStats | null>(null)
  const [connected, setConnected] = useState(false)
  const scrollRef = useRef<HTMLDivElement>(null)

  // SSE connection
  useEffect(() => {
    const es = new EventSource(`/api/events${authQuery()}`)
    es.onopen = () => setConnected(true)
    es.onerror = () => setConnected(false)
    es.onmessage = (e) => {
      try {
        const data = JSON.parse(e.data) as SSEEvent
        if (data.type === "heartbeat") return
        setEvents((prev) => [...prev.slice(-149), data])
        if (data.type === "job_completed") {
          onRefresh?.()
          loadJobs()
        }
      } catch { /* ignore */ }
    }
    return () => es.close()
  }, [onRefresh])

  useEffect(() => {
    if (scrollRef.current) {
      scrollRef.current.scrollTop = scrollRef.current.scrollHeight
    }
  }, [events])

  const loadJobs = useCallback(async () => {
    setJobs(await fetchJobs())
    setSysStats(await fetchSystemStats())
  }, [])

  useEffect(() => {
    loadJobs()
    const interval = setInterval(loadJobs, 10000)
    return () => clearInterval(interval)
  }, [loadJobs])

  const pp = sysStats?.proxy_pool
  const jq = sysStats?.jobs

  // Categorize events
  const leadEvents = events.filter(e => e.type === "lead_discovered" || e.type === "lead_stored")
  const progressEvents = events.filter(e => e.type !== "lead_discovered" && e.type !== "lead_stored")

  return (
    <div className="flex flex-col h-full">
      {/* Header */}
      <div className="flex items-center justify-between px-4 h-10 border-b border-border shrink-0">
        <div className="flex items-center gap-2">
          <div className={`w-1.5 h-1.5 rounded-full ${connected ? "bg-emerald-400 live-dot" : "bg-zinc-600"}`} />
          <span className="text-xs font-semibold">Pipeline</span>
        </div>
        <div className="flex items-center gap-1">
          {pp && (
            <Badge variant="outline" className="text-[9px] h-4 px-1.5 border-border text-muted-foreground">
              {pp.total} proxies
            </Badge>
          )}
          {jq && jq.running > 0 && (
            <Badge variant="outline" className="text-[9px] h-4 px-1.5 border-blue-500/30 bg-blue-500/10 text-blue-400 live-dot">
              {jq.running} active
            </Badge>
          )}
        </div>
      </div>

      {/* Job Cards */}
      {jobs.length > 0 && (
        <div className="px-3 py-2 border-b border-border/50 space-y-1 max-h-36 overflow-y-auto">
          {jobs.slice(0, 6).map((job) => (
            <div key={job.id} className="flex items-center gap-2 text-[11px]">
              <StatusDot status={job.status} />
              <span className="font-mono text-muted-foreground text-[9px] w-12 shrink-0">{job.id}</span>
              <span className="flex-1 truncate text-foreground/80">{job.query}</span>
              {job.leads_found > 0 && (
                <Badge variant="outline" className="text-[9px] h-3.5 px-1 border-emerald-500/30 bg-emerald-500/10 text-emerald-400">
                  {job.leads_found}
                </Badge>
              )}
            </div>
          ))}
        </div>
      )}

      {/* Live Leads Discovered */}
      {leadEvents.length > 0 && (
        <div className="px-3 py-2 border-b border-border/50 max-h-40 overflow-y-auto">
          <div className="text-[9px] font-semibold uppercase tracking-wider text-muted-foreground/50 mb-1">Discovered Leads</div>
          <div className="space-y-0.5">
            {leadEvents.slice(-8).map((ev, i) => (
              <div key={i} className="feed-entry flex items-center gap-2 text-[10px] py-0.5 px-1.5 rounded bg-emerald-500/5">
                <span className="text-emerald-400">●</span>
                <span className="font-medium text-foreground/80 truncate flex-1">
                  {ev.lead?.company || "Unknown"}
                </span>
                {ev.lead?.email && (
                  <span className="text-muted-foreground/50 truncate max-w-24">📧</span>
                )}
                {ev.lead?.phone && (
                  <span className="text-muted-foreground/50">📞</span>
                )}
                <Badge variant="outline" className="text-[8px] h-3 px-1 border-border text-muted-foreground/40">
                  {ev.stage}
                </Badge>
              </div>
            ))}
          </div>
        </div>
      )}

      {/* Event Stream */}
      <ScrollArea className="flex-1">
        <div ref={scrollRef} className="p-3 space-y-0.5">
          {progressEvents.length === 0 && (
            <div className="text-center text-muted-foreground/30 text-xs py-8">
              <div className="text-2xl mb-2">📡</div>
              <div>Waiting for pipeline events…</div>
              <div className="text-[10px] mt-1">Submit a collection query to start</div>
            </div>
          )}
          {progressEvents.map((ev, i) => (
            <div key={i} className="feed-entry text-[11px] leading-relaxed py-0.5">
              <span className="text-muted-foreground/30 font-mono text-[9px] mr-2">
                {ev.ts ? new Date(ev.ts * 1000).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" }) : ""}
              </span>
              <span className={
                ev.type === "job_completed" ? "text-emerald-400 font-medium" :
                ev.type === "job_failed" ? "text-red-400" :
                "text-foreground/60"
              }>
                {ev.message || ev.type}
              </span>
            </div>
          ))}
        </div>
      </ScrollArea>
    </div>
  )
}

function StatusDot({ status }: { status: string }) {
  const colors: Record<string, string> = {
    pending: "bg-yellow-400",
    running: "bg-blue-400 live-dot",
    done: "bg-emerald-400",
    failed: "bg-red-400",
  }
  return <div className={`w-1.5 h-1.5 rounded-full shrink-0 ${colors[status] || "bg-zinc-500"}`} />
}
