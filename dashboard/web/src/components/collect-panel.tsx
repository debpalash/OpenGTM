import { useCallback, useEffect, useState } from "react"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { submitCollect, fetchJobs, fetchSystemStats, type Job, type SystemStats } from "@/lib/api"

const STATUS_ICON: Record<string, string> = {
  pending: "⏳",
  running: "🔄",
  done: "✅",
  failed: "❌",
}

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
    }, 5000)
    return () => clearInterval(interval)
  }, [loadJobs, loadSystem])

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!query.trim()) return
    setLoading(true)
    try {
      await submitCollect(query.trim())
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
    <div className="flex flex-col gap-2 p-3 bg-zinc-900/50 border-b border-zinc-800">
      {/* ── Submit Row ── */}
      <form onSubmit={handleSubmit} className="flex gap-2">
        <Input
          placeholder="Collection query, e.g. 'IT staffing agency Mumbai'"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          className="h-7 text-xs bg-zinc-900 border-zinc-700 flex-1"
        />
        <Button type="submit" size="sm" className="h-7 text-xs px-3" disabled={loading || !query.trim()}>
          {loading ? "⏳" : "🔍"} Collect
        </Button>
      </form>

      {/* ── System Badges ── */}
      <div className="flex items-center gap-1.5 flex-wrap">
        {pp && (
          <>
            <Badge variant="outline" className="text-[10px] h-4 border-zinc-700 bg-zinc-900 text-zinc-400">
              🌐 {pp.total} proxies
            </Badge>
            <Badge variant="outline" className="text-[10px] h-4 border-zinc-700 bg-zinc-900 text-zinc-400">
              🔒 {pp.socks5} SOCKS5
            </Badge>
            {pp.blocked > 0 && (
              <Badge variant="outline" className="text-[10px] h-4 border-red-800 bg-red-950/50 text-red-400">
                ⛔ {pp.blocked} blocked
              </Badge>
            )}
          </>
        )}
        {jq && (
          <>
            {jq.running > 0 && (
              <Badge variant="outline" className="text-[10px] h-4 border-blue-800 bg-blue-950/50 text-blue-400">
                🔄 {jq.running} running
              </Badge>
            )}
            {jq.pending > 0 && (
              <Badge variant="outline" className="text-[10px] h-4 border-yellow-800 bg-yellow-950/50 text-yellow-400">
                ⏳ {jq.pending} pending
              </Badge>
            )}
            <Badge variant="outline" className="text-[10px] h-4 border-zinc-700 bg-zinc-900 text-zinc-400">
              📊 {jq.done}/{jq.total} jobs done
            </Badge>
          </>
        )}
      </div>

      {/* ── Recent Jobs ── */}
      {jobs.length > 0 && (
        <div className="flex flex-col gap-0.5 max-h-24 overflow-y-auto">
          {jobs.slice(0, 5).map((job) => (
            <div key={job.id} className="flex items-center gap-2 text-[10px] text-zinc-400 px-1">
              <span className="w-3">{STATUS_ICON[job.status] || "?"}</span>
              <span className="font-mono text-zinc-500 w-16">{job.id}</span>
              <span className="flex-1 truncate">{job.query}</span>
              {job.leads_found > 0 && (
                <Badge variant="secondary" className="text-[9px] h-3.5 bg-emerald-950/50 text-emerald-400 border-emerald-800">
                  {job.leads_found} leads
                </Badge>
              )}
              {job.error && job.status === "failed" && (
                <span className="text-red-400 truncate max-w-32">{job.error}</span>
              )}
            </div>
          ))}
        </div>
      )}
    </div>
  )
}
