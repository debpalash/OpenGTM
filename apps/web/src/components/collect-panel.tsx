import { useCallback, useEffect, useState } from "react"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Ban, ChartNoAxesColumn, Clock, Globe, LoaderCircle, Lock, Search } from "lucide-react"
import { StatusIcon, type RunStatus } from "@/components/semantic-icons"
import {
  COLLECTION_CLARIFICATION_EVENT,
  submitCollect,
  fetchJobs,
  fetchSystemStats,
  type Job,
  type SystemStats,
} from "@/lib/api"

const JOB_STATUSES = new Set<RunStatus>(["pending", "running", "done", "failed", "skipped"])
const jobStatus = (status: string): RunStatus => (JOB_STATUSES.has(status as RunStatus) ? status as RunStatus : "pending")

export function CollectPanel({ onCollected }: { onCollected?: () => void }) {
  const [query, setQuery] = useState("")
  const [loading, setLoading] = useState(false)
  const [jobs, setJobs] = useState<Job[]>([])
  const [sysStats, setSysStats] = useState<SystemStats | null>(null)

  const loadJobs = useCallback(async () => {
    const data = await fetchJobs()
    setJobs(data)
  }, [])

  const loadSystem = useCallback(async () => {
    const data = await fetchSystemStats()
    setSysStats(data)
  }, [])

  useEffect(() => {
    loadJobs()
    loadSystem()
    const interval = setInterval(() => {
      loadJobs()
      loadSystem()
    }, 15000)
    return () => clearInterval(interval)
  }, [loadJobs, loadSystem])

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!query.trim()) return
    setLoading(true)
    try {
      const result = await submitCollect(query.trim())
      if (!result.ok) {
        window.dispatchEvent(new CustomEvent(COLLECTION_CLARIFICATION_EVENT, {
          detail: result,
        }))
        return
      }
      setQuery("")
      await loadJobs()
      onCollected?.()
    } finally {
      setLoading(false)
    }
  }

  const pp = sysStats?.proxy_pool
  const jq = sysStats?.jobs

  return (
    <div className="flex flex-col gap-2 p-3 bg-[var(--t-background-secondary)] border-b border-border">
      {/* ── Submit Row ── */}
      <form onSubmit={handleSubmit} className="flex gap-2">
        <Input
          placeholder="Collection query, e.g. 'IT staffing agency Mumbai'"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          className="h-7 text-sm flex-1"
        />
        <Button type="submit" size="sm" disabled={loading || !query.trim()}>
          {loading ? <LoaderCircle aria-hidden="true" className="animate-spin" /> : <Search aria-hidden="true" />}
          Collect
        </Button>
      </form>

      {/* ── System Badges ── */}
      <div className="flex items-center gap-1.5 flex-wrap">
        {pp && (
          <>
            <Badge variant="outline" className="text-muted-foreground">
              <Globe aria-hidden="true" />{pp.total} proxies
            </Badge>
            <Badge variant="outline" className="text-muted-foreground">
              <Lock aria-hidden="true" />{pp.socks5} SOCKS5
            </Badge>
            {pp.blocked > 0 && (
              <Badge variant="destructive">
                <Ban aria-hidden="true" />{pp.blocked} blocked
              </Badge>
            )}
          </>
        )}
        {jq && (
          <>
            {jq.running > 0 && (
              <Badge variant="outline" className="text-[var(--t-color-blue9)]">
                <LoaderCircle aria-hidden="true" className="animate-spin" />{jq.running} running
              </Badge>
            )}
            {jq.pending > 0 && (
              <Badge variant="outline" className="text-[var(--t-color-orange9)]">
                <Clock aria-hidden="true" />{jq.pending} pending
              </Badge>
            )}
            <Badge variant="outline" className="text-muted-foreground">
              <ChartNoAxesColumn aria-hidden="true" />{jq.done}/{jq.total} jobs done
            </Badge>
          </>
        )}
      </div>

      {/* ── Recent Jobs ── */}
      {jobs.length > 0 && (
        <div className="flex flex-col gap-0.5 max-h-24 overflow-y-auto">
          {jobs.slice(0, 5).map((job) => (
            <div key={job.id} className="flex items-center gap-2 text-xs text-muted-foreground px-1">
              <StatusIcon status={jobStatus(job.status)} className="size-3" />
              <span className="font-mono text-muted-foreground w-16">{job.id}</span>
              <span className="flex-1 truncate">{job.query}</span>
              {job.leads_found > 0 && (
                <Badge variant="secondary" className="text-[var(--t-color-green9)]">
                  {job.leads_found} leads
                </Badge>
              )}
              {job.error && job.status === "failed" && (
                <span className="text-destructive truncate max-w-32">{job.error}</span>
              )}
            </div>
          ))}
        </div>
      )}
    </div>
  )
}
